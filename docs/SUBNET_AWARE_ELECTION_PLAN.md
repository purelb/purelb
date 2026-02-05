# Plan: Subnet-Aware Election via K8s Leases

## Summary

Replace HashiCorp memberlist with Kubernetes Leases that include subnet annotations. This enables **subnet-aware election**: only nodes with a matching local subnet participate in the election for a given IP. No more kubelb0 fallback when any node has the subnet locally.

## Problem Being Solved

**Current behavior:**
- All nodes participate in election regardless of subnet
- If winner doesn't have matching subnet, IP goes to kubelb0 (dummy interface)
- Other nodes that DO have the subnet don't announce it

**Desired behavior:**
- Only nodes with matching local subnet are election candidates
- If IP `192.168.1.100` matches Node-A and Node-C's subnet, only they compete
- Node-B (different subnet) does NOT add to kubelb0
- Result: IP announced on exactly one node, always on a real interface

## How It Works

### 1. Lease with Subnet Annotations

Each lbnodeagent creates a lease with its local subnets:

```yaml
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: purelb-node-worker-1
  namespace: purelb-system
  annotations:
    purelb.io/subnets: "192.168.1.0/24,10.0.0.0/24"
spec:
  holderIdentity: worker-1
  leaseDurationSeconds: 10
  renewTime: "2024-01-05T12:00:00Z"
```

### 2. Election Builds Subnet→Nodes Mapping

```go
type Election struct {
    liveNodes     []string                    // nodes with valid leases
    subnetToNodes map[string][]string         // "192.168.1.0/24" → ["worker-1", "worker-3"]
    nodeToSubnets map[string][]string         // "worker-1" → ["192.168.1.0/24", "10.0.0.0/24"]
}
```

### 3. Winner() Filters by Subnet

```go
func (e *Election) Winner(ipStr string) string {
    ip := net.ParseIP(ipStr)

    // Find candidates: nodes that have a subnet containing this IP
    var candidates []string
    for subnet, nodes := range e.subnetToNodes {
        _, ipnet, _ := net.ParseCIDR(subnet)
        if ipnet.Contains(ip) {
            candidates = append(candidates, nodes...)
        }
    }

    if len(candidates) == 0 {
        return ""  // No node has this subnet
    }

    // Deduplicate and run hash election
    candidates = unique(candidates)
    return election(ipStr, candidates)[0]
}
```

### 4. Announcer Changes

The announcer now uses an address lifetime system with `AddressOptions` to prevent CNI conflicts
(e.g., Flannel incorrectly selecting VIPs as node addresses). All address additions must use
`addNetworkWithOptions()` and schedule renewals.

```go
func (a *announcer) announceLocal(svc *v1.Service, announceInt netlink.Link, lbIP net.IP, lbIPNet net.IPNet) error {
    l := log.With(a.logger, "service", svc.Name)
    nsName := svc.Namespace + "/" + svc.Name

    // ... ExternalTrafficPolicy handling unchanged ...

    // See if we won the announcement election (UPDATED for subnet-aware)
    winner := a.election.Winner(lbIP.String())

    if winner == "" {
        // No node has this subnet - don't announce anywhere
        // (This replaces the kubelb0 fallback for local addresses)
        l.Log("msg", "noEligibleNodes", "ip", lbIP)
        return nil
    }

    if winner != a.myNode {
        // We lost the election so withdraw any announcement
        l.Log("msg", "notWinner", "node", a.myNode, "winner", winner, "service", nsName)
        return a.deleteAddress(nsName, "lostElection", lbIP)
    }

    // We won - announce on local interface using AddressOptions
    l.Log("msg", "Winner", "node", a.myNode, "service", nsName)
    a.client.Infof(svc, "AnnouncingLocal", "Node %s announcing %s on interface %s", a.myNode, lbIP, announceInt.Attrs().Name)

    // IMPORTANT: Use AddressOptions for lifetime management
    opts := a.getLocalAddressOptions()
    if err := addNetworkWithOptions(lbIPNet, announceInt, opts); err != nil {
        return err
    }
    // Schedule renewal to refresh address before lifetime expires
    a.scheduleRenewal(nsName, lbIPNet, announceInt, opts)

    // Update annotations and metrics...
    if svc.Annotations == nil {
        svc.Annotations = map[string]string{}
    }
    svc.Annotations[purelbv1.AnnounceAnnotation+addrFamilyName(lbIP)] = a.myNode + "," + announceInt.Attrs().Name
    // ...
}
```

**Key changes from current code:**
- Line 252: Change `if winner := a.election.Winner(...); winner != a.myNode` to separate checks
- Add handling for `winner == ""` (no eligible nodes with matching subnet)
- Preserve existing `addNetworkWithOptions()` + `scheduleRenewal()` pattern

### 5. Pool Type: Local vs Remote (Mutually Exclusive)

ServiceGroup now has mutually exclusive `local` and `remote` pool types:

**Local Pool** (Subnet-Aware Election):
```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: local-pool
  namespace: purelb-system
spec:
  local:
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
      aggregation: default
    - subnet: 192.168.2.0/24
      pool: 192.168.2.240-192.168.2.250
      aggregation: default
    v6pools:
    - subnet: fd53:9ef0:8683::/120
      pool: fd53:9ef0:8683::-fd53:9ef0:8683::3
      aggregation: default
```

**Remote Pool** (All Nodes Announce to kubelb0):
```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: bgp-pool
  namespace: purelb-system
spec:
  remote:
    v4pools:
    - subnet: 10.100.0.0/24
      pool: 10.100.0.0/24
      aggregation: default
    v6pools:
    - subnet: 2001:db8:100::/64
      pool: 2001:db8:100::-2001:db8:100::ff
      aggregation: default
```

**Behavior:**
- `spec.local` → Subnet-filtered election, only nodes with matching subnet participate, IP on real interface
- `spec.remote` → All nodes announce to kubelb0 dummy interface (for BGP/ECMP routing)
- `spec.netbox` → Existing Netbox IPAM integration (unchanged)
- **Validation**: Exactly one of `local`, `remote`, or `netbox` must be specified

### Multi-Pool Allocation (Optional)

By default, a Service gets one IP per address family (current behavior). Services can opt-in to multi-pool allocation via annotation:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  annotations:
    purelb.io/multi-pool: "true"   # NEW: Allocate from all matching pools
spec:
  type: LoadBalancer
  # ...
```

**With multi-pool enabled:**
- Allocator finds all pools where at least one node has matching subnet
- Allocates one IP from each matching pool
- Service gets multiple IPs in `status.loadBalancer.ingress`

**Example:**
```yaml
# ServiceGroup with two pools
spec:
  local:
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
    - subnet: 192.168.2.0/24
      pool: 192.168.2.240-192.168.2.250

# Service with multi-pool annotation
status:
  loadBalancer:
    ingress:
    - ip: 192.168.1.241    # From pool 1, announced on nodes with 192.168.1.0/24
    - ip: 192.168.2.241    # From pool 2, announced on nodes with 192.168.2.0/24
```

Each IP is announced by exactly one node (the election winner for that IP's subnet).

**Implementation note:** For multi-pool to work, the Allocator needs visibility into which subnets have nodes. Options:
- Allocator watches lbnodeagent leases to see subnet annotations
- Or: Allocator allocates from all pools, lbnodeagent only announces if it has matching subnet (simpler but wastes IPs)

### 6. LBNodeAgent Interface Configuration

New `interfaces` field allows explicit interface list for election participation.
Note: The existing `addressConfig` field controls address lifetime and flags.

```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    # localInterface controls default route auto-detection:
    # - "default": auto-detect interface with default route (current behavior)
    # - "none": disable auto-detection
    localInterface: default

    # NEW: interfaces[] adds extra interfaces for election participation
    # Subnets from these interfaces are included in lease annotation
    # +optional
    interfaces:
    - eth1
    - bond0.100

    dummyInterface: kube-lb0

    # garpConfig replaces the old sendgarp boolean
    garpConfig:
      enabled: true
      count: 3
      interval: "500ms"

    # EXISTING: addressConfig controls address lifetime and flags
    # This prevents CNI plugins (e.g., Flannel) from selecting VIPs as node addresses
    addressConfig:
      localInterface:
        validLifetime: 300      # 5 minutes (default), 0 = permanent
        preferredLifetime: 300
        noPrefixRoute: true     # Prevents automatic prefix route creation
      dummyInterface:
        validLifetime: 0        # Permanent (default for dummy)
        noPrefixRoute: false
```

**Behavior:**
- `localInterface: default` + no `interfaces`: Current behavior, auto-detect only
- `localInterface: default` + `interfaces: [eth1]`: Auto-detect + eth1 subnets
- `localInterface: none` + `interfaces: [eth0, eth1]`: Only listed interfaces, no auto-detect

**Lease annotation includes all configured interface subnets:**
```yaml
annotations:
  purelb.io/subnets: "192.168.1.0/24,192.168.2.0/24,10.0.0.0/24"
```

### 7. Subnet Change Detection

Watch netlink for real-time interface changes:
- Subscribe to `RTM_NEWADDR` and `RTM_DELADDR` events via `netlink.AddrSubscribe()`
- Filter events to configured interfaces only
- On change, update lease annotation with new subnets
- Trigger `ForceSync()` to re-evaluate services

**Leverage existing code** in `internal/local/network.go`:
- `checkLocal(intf, lbIP)` - determines if IP belongs to same network as interface
- `findLocal(regex, lbIP)` - finds local interface matching regex with IP in subnet
- These can be adapted for the `GetLocalSubnets()` callback

### 8. Lease Garbage Collection

When a Kubernetes node is permanently removed (`kubectl delete node`), its PureLB lease becomes orphaned. While orphaned leases don't cause functional issues (they expire and are ignored), they create clutter and may confuse operators.

**Solution**: Use OwnerReference to tie leases to their Node objects:

```go
func (e *Election) createLease() error {
    // Get the Node object for OwnerReference
    node, err := e.client.CoreV1().Nodes().Get(ctx, e.config.NodeName, metav1.GetOptions{})
    if err != nil {
        return fmt.Errorf("failed to get node for owner reference: %w", err)
    }

    lease := &coordinationv1.Lease{
        ObjectMeta: metav1.ObjectMeta{
            Name:      e.leaseName,
            Namespace: e.config.Namespace,
            Annotations: map[string]string{
                SubnetsAnnotation: e.getSubnetsAnnotation(),
            },
            // OwnerReference ensures lease is garbage collected when Node is deleted
            OwnerReferences: []metav1.OwnerReference{
                {
                    APIVersion: "v1",
                    Kind:       "Node",
                    Name:       node.Name,
                    UID:        node.UID,
                    // BlockOwnerDeletion=false allows node deletion to proceed without waiting
                    BlockOwnerDeletion: ptr.To(false),
                    // Controller=false because we're not a controller for the Node
                    Controller: ptr.To(false),
                },
            },
        },
        Spec: coordinationv1.LeaseSpec{
            HolderIdentity:       &e.config.NodeName,
            LeaseDurationSeconds: ptr.To(int32(e.config.LeaseDuration.Seconds())),
            RenewTime:            &metav1.MicroTime{Time: time.Now()},
        },
    }

    _, err = e.client.CoordinationV1().Leases(e.config.Namespace).Create(ctx, lease, metav1.CreateOptions{})
    return err
}
```

**Behavior**:
- When `kubectl delete node worker-3` is run
- Kubernetes garbage collector automatically deletes the `purelb-node-worker-3` lease
- Other nodes' informers see the deletion and rebuild their election maps
- No manual cleanup required

**RBAC addition** for reading Node UID:
```yaml
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get"]  # Only need get, not list/watch
```

**Alternative approach** (simpler but less clean): Accept orphaned leases. They expire after `leaseDurationSeconds` and are ignored by election logic. Operators can manually delete with:
```bash
kubectl delete lease -n purelb -l app.kubernetes.io/component=lbnodeagent --field-selector 'spec.holderIdentity notin (node1,node2,node3)'
```

### 9. Address Lifetime Integration (IMPORTANT)

The codebase uses an address lifetime system to prevent CNI conflicts. This MUST be preserved.

**Key components in `internal/local/`:**

```go
// network.go - AddressOptions controls how addresses are added
type AddressOptions struct {
    ValidLft int       // valid lifetime in seconds, 0 = permanent
    PreferedLft int    // preferred lifetime in seconds
    NoPrefixRoute bool // prevents automatic prefix route creation
    NoDAD bool         // skip Duplicate Address Detection for IPv6 (NEW)
}

// addNetworkWithOptions() - ALWAYS use this instead of addNetwork()
func addNetworkWithOptions(lbIPNet net.IPNet, link netlink.Link, opts AddressOptions) error
```

```go
// announcer_local.go - Address renewal system
func (a *announcer) scheduleRenewal(svcName string, lbIPNet net.IPNet, link netlink.Link, opts AddressOptions)
func (a *announcer) renewAddress(key string)
func (a *announcer) cancelRenewal(svcName, ip string)

// Per-interface option getters (use these, don't hardcode)
func (a *announcer) getLocalAddressOptions() AddressOptions   // default: 300s, NoPrefixRoute=true
func (a *announcer) getDummyAddressOptions() AddressOptions   // default: permanent, NoPrefixRoute=false
```

**Why this matters:**
- Addresses with `ValidLft > 0` do NOT have `IFA_F_PERMANENT` flag
- This prevents Flannel CNI from incorrectly selecting VIPs as node addresses
- Addresses expire after lifetime, so `scheduleRenewal()` refreshes them at 50% lifetime
- Deleting addresses must call `cancelRenewal()` to stop the timer

**IPv6 Duplicate Address Detection (DAD) Configuration:**

When adding an IPv6 address, Linux runs DAD for ~1 second before the address becomes usable. During this time, traffic to the VIP may be dropped. Since VIPs are managed by PureLB (we guarantee uniqueness via election), DAD can optionally be skipped to reduce failover latency.

**This is configured per ServiceGroup**, allowing different pools to have different DAD policies:

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: fast-failover-pool
spec:
  local:
    # Skip DAD for faster IPv6 failover (~1s improvement)
    # Only set to true if you trust the election guarantees uniqueness
    # +kubebuilder:default=false
    skipIPv6DAD: false  # Default: perform DAD (safer)
    v6pools:
    - subnet: fd53:9ef0:8683::/64
      pool: fd53:9ef0:8683::100-fd53:9ef0:8683::1ff
```

**API Type addition** (in `ServiceGroupLocalSpec` and `ServiceGroupRemoteSpec`):
```go
type ServiceGroupLocalSpec struct {
    // ... existing pool fields ...

    // SkipIPv6DAD skips Duplicate Address Detection for IPv6 VIPs.
    // When true, IPv6 addresses become usable immediately (no ~1s DAD delay).
    // Safe when PureLB election is trusted to guarantee address uniqueness.
    // When false (default), standard DAD is performed for safety.
    // Only affects IPv6 addresses; IPv4 is unaffected.
    // +kubebuilder:default=false
    // +optional
    SkipIPv6DAD bool `json:"skipIPv6DAD,omitempty"`
}
```

**Implementation** - Pass DAD setting through pool type annotation:
```go
// Allocator sets annotation when allocating
if group.Spec.Local != nil {
    svc.Annotations[purelbv1.PoolTypeAnnotation] = "local"
    if group.Spec.Local.SkipIPv6DAD {
        svc.Annotations[purelbv1.SkipIPv6DADAnnotation] = "true"
    }
}

// Announcer reads annotation when adding address
func (a *announcer) getAddressOptions(svc *v1.Service, isLocal bool) AddressOptions {
    opts := a.getLocalAddressOptions()  // or getDummyAddressOptions()

    // Check ServiceGroup's DAD preference (passed via annotation)
    if svc.Annotations[purelbv1.SkipIPv6DADAnnotation] == "true" {
        opts.NoDAD = true
    }
    return opts
}

// addNetworkWithOptions - handle IPv6 NODAD flag
func addNetworkWithOptions(lbIPNet net.IPNet, link netlink.Link, opts AddressOptions) error {
    addr := &netlink.Addr{
        IPNet: &lbIPNet,
        Label: "",
    }

    // Set lifetime flags
    if opts.ValidLft > 0 {
        addr.ValidLft = opts.ValidLft
        addr.PreferedLft = opts.PreferedLft
    }

    // Set flags
    if opts.NoPrefixRoute {
        addr.Flags |= unix.IFA_F_NOPREFIXROUTE
    }

    // IPv6: Skip DAD only if explicitly configured for this pool
    if lbIPNet.IP.To4() == nil && opts.NoDAD {
        addr.Flags |= unix.IFA_F_NODAD
    }

    return netlink.AddrAdd(link, addr)
}
```

**New annotation** (in `pkg/apis/purelb/v1/annotations.go`):
```go
const SkipIPv6DADAnnotation = "purelb.io/skip-ipv6-dad"  // "true" if DAD should be skipped
```

**Default behavior:**
- `skipIPv6DAD: false` (default) - Standard DAD is performed for safety
- `skipIPv6DAD: true` - Skip DAD for ~1s faster IPv6 failover

**When to enable `skipIPv6DAD: true`:**
- Trusted environment where PureLB is the only source of these VIPs
- Fast failover is critical (sub-second requirements)
- You've verified no other systems might assign the same IPv6 addresses

**When to keep `skipIPv6DAD: false` (default):**
- Mixed environments where other systems might assign addresses
- Regulatory/compliance requirements mandate DAD
- You're unsure about address uniqueness guarantees

When adding an IPv6 address, Linux runs DAD for ~1 second before the address becomes usable. During this time, traffic to the VIP may be dropped. Since VIPs are managed by PureLB (we guarantee uniqueness via election), DAD is unnecessary and adds latency to failover.

```go
// addNetworkWithOptions - handle IPv6 NODAD flag
func addNetworkWithOptions(lbIPNet net.IPNet, link netlink.Link, opts AddressOptions) error {
    addr := &netlink.Addr{
        IPNet: &lbIPNet,
        Label: "",
    }

    // Set lifetime flags
    if opts.ValidLft > 0 {
        addr.ValidLft = opts.ValidLft
        addr.PreferedLft = opts.PreferedLft
    }

    // Set flags
    if opts.NoPrefixRoute {
        addr.Flags |= unix.IFA_F_NOPREFIXROUTE
    }

    // IPv6: Skip DAD for VIPs (we guarantee uniqueness via election)
    if lbIPNet.IP.To4() == nil && opts.NoDAD {
        addr.Flags |= unix.IFA_F_NODAD
    }

    return netlink.AddrAdd(link, addr)
}
```

**Integration requirements:**
1. When adding an address: `addNetworkWithOptions()` + `scheduleRenewal()`
2. When deleting an address: `cancelRenewal()` + `deleteAddr()`
3. Use `getLocalAddressOptions()`/`getDummyAddressOptions()` for correct defaults
4. The `addressRenewals sync.Map` tracks all active renewal timers

## Example Scenario

```
Node-A: eth0 = 192.168.1.0/24
Node-B: eth0 = 192.168.2.0/24
Node-C: eth0 = 192.168.1.0/24

Leases created:
  purelb-node-a: subnets="192.168.1.0/24"
  purelb-node-b: subnets="192.168.2.0/24"
  purelb-node-c: subnets="192.168.1.0/24"

subnetToNodes map:
  "192.168.1.0/24" → ["node-a", "node-c"]
  "192.168.2.0/24" → ["node-b"]

Service-1 gets IP 192.168.1.100:
  Winner("192.168.1.100"):
    - IP in 192.168.1.0/24 → candidates = ["node-a", "node-c"]
    - Hash election → winner = "node-c"
  Node-A: not winner → cancelRenewal() + deleteAddress()
  Node-B: not candidate → does nothing (no kubelb0)
  Node-C: winner → addNetworkWithOptions() + scheduleRenewal() on eth0

Service-2 gets IP 192.168.2.50:
  Winner("192.168.2.50"):
    - IP in 192.168.2.0/24 → candidates = ["node-b"]
    - Hash election → winner = "node-b"
  Node-A: not candidate → does nothing
  Node-B: winner → addNetworkWithOptions() + scheduleRenewal() on eth0
  Node-C: not candidate → does nothing
```

## Scaling Analysis

Same as before - scales with nodes only, not IPs:

| Scenario | Leases | API calls/s | Watch events/s |
|----------|--------|-------------|----------------|
| 30 nodes × 100 IPs | 30 | ~9 | ~129 |
| 100 nodes × 500 IPs | 100 | ~29 | ~1429 |

Subnet annotation updates only happen when node's interfaces change (rare).

## Failover SLA and Time Budget

This section documents the expected failover timing for different scenarios.

### Target SLA

| Scenario | Target | Maximum |
|----------|--------|---------|
| Graceful shutdown (rolling update) | < 5s | 10s |
| Abrupt node failure (crash/power loss) | < 15s | 20s |
| API server partition recovery | < 20s | 30s |

### Failover Time Budget: Abrupt Node Failure

When a node crashes without graceful shutdown:

```
Timeline for IP failover after node crash:

0s      Node crashes
        └─ Lease stops being renewed

5s      Lease renewal would have occurred (50% of 10s duration)
        └─ Missed - no effect yet

10s     Lease expires (leaseDurationSeconds)
        └─ Other nodes' informers receive watch event

10.5s   Election rebuildMaps() triggered
        └─ Crashed node removed from liveNodes
        └─ New winner computed for affected IPs

11s     OnMemberChange() callback fires
        └─ client.ForceSync() requeues all services

11.5s   Work queue processes affected services
        └─ New winner calls addNetworkWithOptions()

12s     Address added to interface
        └─ GARP sequence begins (if enabled)

12.2s   First GARP sent (after 200ms delay)
        └─ Upstream switches/routers update ARP tables

13s     Traffic flows to new winner
        └─ TOTAL: ~13 seconds

14s     Additional GARPs sent (if count > 1)
        └─ Handles slow network equipment
```

**Key timing parameters:**

| Parameter | Default | Impact |
|-----------|---------|--------|
| `leaseDurationSeconds` | 10s | Time to detect node failure |
| `renewDeadline` | 7s | How long to retry renewal |
| `retryPeriod` | 2s | Interval between renewal attempts |
| GARP delay | 200ms | Wait for loser withdrawal |
| IPv6 DAD | 0s (disabled) | Would add ~1s if enabled |

### Failover Time Budget: Graceful Shutdown

When a node is gracefully terminated (rolling update, drain):

```
Timeline for IP failover during graceful shutdown:

0s      SIGTERM received
        └─ Signal handler starts shutdown sequence

0.1s    election.MarkUnhealthy() called
        └─ Winner() returns "" for this node

0.2s    announcer.WithdrawAll() starts
        └─ Addresses deleted from interfaces

0.5s    election.DeleteOurLease() called
        └─ Other nodes see lease deletion via watch

1s      Other nodes' rebuildMaps() triggered
        └─ New winners computed

1.5s    New winners add addresses
        └─ GARP sequence begins

2s      First GARP sent
        └─ Traffic flows to new winners

2s      sleep(2s) in shutdown handler
        └─ Ensures new winners established

4s      Pod terminates
        └─ TOTAL: ~2 seconds of actual downtime
```

### Tuning for Faster Failover

For environments requiring faster failover (at cost of more API server load):

```yaml
# Aggressive settings (not recommended for large clusters)
env:
- name: PURELB_LEASE_DURATION
  value: "5"      # 5 second lease (default: 10)
- name: PURELB_RENEW_DEADLINE
  value: "3"      # 3 second renewal deadline
- name: PURELB_RETRY_PERIOD
  value: "1"      # 1 second retry

# Results in:
# - Node failure detection: ~5s (was ~10s)
# - Total failover: ~8s (was ~13s)
# - API calls doubled
```

### Monitoring Failover Performance

Use the Prometheus metrics to track actual failover times:

```promql
# Histogram of time between winner changes (approximates failover detection)
histogram_quantile(0.99, rate(purelb_election_winner_changes_total[5m]))

# Alert if failover takes too long
# (requires custom metric tracking time from node failure to new winner announcement)
```

## API Version: v1 → v2

This release introduces breaking changes that warrant a new API version. The `purelb.io/v2` API provides a cleaner configuration model without backward-compatibility baggage.

### Why v2?

| Change | Impact | v1 Compat Possible? |
|--------|--------|---------------------|
| `spec.local` behavior change | Semantic (subnet-aware election) | No - same config, different behavior |
| `sendgarp` → `garpConfig` | Schema change | Yes, but messy |
| Mutual exclusion validation | New constraint | Could reject valid v1 CRs |
| `spec.remote` (new) | Additive | Yes |
| `skipIPv6DAD` (new) | Additive | Yes |
| `interfaces[]` (new) | Additive | Yes |

**Decision**: Clean break to v2. Users must explicitly migrate, ensuring they understand the new behavior.

### ⚠️ v2 Implementation Gaps (Must Fix Before Release)

The following gaps exist between the plan and actual implementation:

| Gap | Status | Notes |
|-----|--------|-------|
| `V4Pools`/`V6Pools` arrays in v2 | ❌ Missing | Current v2 only has singular `V4Pool`/`V6Pool` |
| `iprange.go` in v2 package | ❌ Missing | `IPRange` type, `NewIPRange()` function needed |
| `config.go` in v2 package | ❌ Missing | `Config` struct for `SetConfig()` |
| Remote pool in allocator | ❌ Missing | `parsePool()` only handles Local and Netbox |
| v2 JSON tags | ⚠️ Needs Fix | Should use `localInterface`/`dummyInterface` |

**Before dropping v1 support**, these must be implemented in `pkg/apis/purelb/v2/`:
1. Add `V4Pools`/`V6Pools` arrays to `ServiceGroupLocalSpec` and `ServiceGroupRemoteSpec`
2. Copy `iprange.go` and `iprange_test.go` from v1, update package
3. Create `config.go` with `Config` struct
4. Update JSON tags to use clean names (`localInterface`, `dummyInterface`)
5. Implement Remote pool support in `internal/allocator/pool.go`

### v2 API Types

**`pkg/apis/purelb/v2/types.go`** (new package):

```go
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:storageversion
type ServiceGroup struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              ServiceGroupSpec `json:"spec,omitempty"`
}

// ServiceGroupSpec defines IP pools. Exactly one of Local, Remote, or Netbox must be set.
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type ServiceGroupSpec struct {
    // Local configures subnet-aware pools announced on real interfaces
    // +optional
    Local *ServiceGroupLocalSpec `json:"local,omitempty"`

    // Remote configures pools announced on dummy interface (for BGP/ECMP)
    // +optional
    Remote *ServiceGroupRemoteSpec `json:"remote,omitempty"`

    // Netbox configures pools managed by Netbox IPAM
    // +optional
    Netbox *ServiceGroupNetboxSpec `json:"netbox,omitempty"`
}

type ServiceGroupLocalSpec struct {
    // V4Pool defines a single IPv4 address pool (mutually exclusive with V4Pools)
    // +optional
    V4Pool *ServiceGroupAddressPool `json:"v4pool,omitempty"`

    // V4Pools defines multiple IPv4 address pools
    // +optional
    V4Pools []ServiceGroupAddressPool `json:"v4pools,omitempty"`

    // V6Pool defines a single IPv6 address pool (mutually exclusive with V6Pools)
    // +optional
    V6Pool *ServiceGroupAddressPool `json:"v6pool,omitempty"`

    // V6Pools defines multiple IPv6 address pools
    // +optional
    V6Pools []ServiceGroupAddressPool `json:"v6pools,omitempty"`

    // SkipIPv6DAD skips Duplicate Address Detection for IPv6 VIPs.
    // Default: false (DAD is performed for safety)
    // +kubebuilder:default=false
    // +optional
    SkipIPv6DAD bool `json:"skipIPv6DAD,omitempty"`
}

type ServiceGroupRemoteSpec struct {
    // Same pool structure as Local
    V4Pool  *ServiceGroupAddressPool  `json:"v4pool,omitempty"`
    V4Pools []ServiceGroupAddressPool `json:"v4pools,omitempty"`
    V6Pool  *ServiceGroupAddressPool  `json:"v6pool,omitempty"`
    V6Pools []ServiceGroupAddressPool `json:"v6pools,omitempty"`

    // SkipIPv6DAD - same as Local
    // +kubebuilder:default=false
    // +optional
    SkipIPv6DAD bool `json:"skipIPv6DAD,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:storageversion
type LBNodeAgent struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              LBNodeAgentSpec `json:"spec,omitempty"`
}

type LBNodeAgentSpec struct {
    Local *LBNodeAgentLocalSpec `json:"local,omitempty"`
}

type LBNodeAgentLocalSpec struct {
    // LocalInterface specifies how to find the local announcement interface.
    // "default" = auto-detect via default route
    // "none" = disable auto-detection, use only Interfaces list
    // +kubebuilder:default="default"
    // +kubebuilder:validation:Enum=default;none
    LocalInterface string `json:"localInterface,omitempty"`

    // Interfaces lists additional interfaces for election participation.
    // Subnets from these interfaces are included in lease annotations.
    // +optional
    Interfaces []string `json:"interfaces,omitempty"`

    // DummyInterface is the dummy interface for remote pool announcements.
    // +kubebuilder:default="kube-lb0"
    DummyInterface string `json:"dummyInterface,omitempty"`

    // GARPConfig configures gratuitous ARP behavior.
    // +optional
    GARPConfig *GARPConfig `json:"garpConfig,omitempty"`

    // AddressConfig controls address lifetime and flags.
    // +optional
    AddressConfig *AddressConfig `json:"addressConfig,omitempty"`
}

// GARPConfig controls gratuitous ARP packet behavior
type GARPConfig struct {
    // Enabled turns on gratuitous ARP
    // +kubebuilder:default=false
    Enabled bool `json:"enabled"`

    // Count is the number of GARP packets to send
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=10
    Count int `json:"count,omitempty"`

    // IntervalMs is the delay between GARP packets in milliseconds
    // +kubebuilder:default=500
    // +kubebuilder:validation:Minimum=100
    // +kubebuilder:validation:Maximum=5000
    IntervalMs int `json:"intervalMs,omitempty"`

    // DelayMs is the initial delay before first GARP
    // +kubebuilder:default=200
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=5000
    DelayMs int `json:"delayMs,omitempty"`
}
```

### v1 vs v2 Field Mapping

**ServiceGroup:**

| v1 Field | v2 Field | Notes |
|----------|----------|-------|
| `spec.local.v4pool` | `spec.local.v4pool` | Same |
| `spec.local.v4pools` | `spec.local.v4pools` | Same |
| `spec.local.v6pool` | `spec.local.v6pool` | Same |
| `spec.local.v6pools` | `spec.local.v6pools` | Same |
| (none) | `spec.local.skipIPv6DAD` | New in v2 |
| (none) | `spec.remote.*` | New in v2 |
| `spec.netbox.*` | `spec.netbox.*` | Same |

**LBNodeAgent:**

| v1 Field | v2 Field | Notes |
|----------|----------|-------|
| `spec.local.localint` | `spec.local.localInterface` | Renamed (camelCase) |
| `spec.local.extlbint` | `spec.local.dummyInterface` | Renamed (clearer) |
| `spec.local.sendgarp` | `spec.local.garpConfig.enabled` | Restructured |
| (none) | `spec.local.garpConfig.count` | New in v2 |
| (none) | `spec.local.garpConfig.intervalMs` | New in v2 |
| (none) | `spec.local.garpConfig.delayMs` | New in v2 |
| (none) | `spec.local.interfaces` | New in v2 |
| `spec.local.addressConfig` | `spec.local.addressConfig` | Same |

### Migration Guide: v1 to v2

#### Step 1: Install v2 CRDs (Additive)

The v2 CRDs can coexist with v1 during migration:

```bash
# Install v2 CRDs alongside v1
kubectl apply -f https://raw.githubusercontent.com/purelb/purelb/main/deployments/crds/purelb.io_servicegroups_v2.yaml
kubectl apply -f https://raw.githubusercontent.com/purelb/purelb/main/deployments/crds/purelb.io_lbnodeagents_v2.yaml
```

#### Step 2: Convert ServiceGroup CRs

**v1 ServiceGroup (before):**
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb
spec:
  local:
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
      aggregation: default
    v6pools:
    - subnet: fd53:9ef0:8683::/120
      pool: fd53:9ef0:8683::-fd53:9ef0:8683::3
      aggregation: default
```

**v2 ServiceGroup (after):**
```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb
spec:
  local:  # Or "remote:" if you want all-nodes-announce behavior
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
      aggregation: default
    v6pools:
    - subnet: fd53:9ef0:8683::/120
      pool: fd53:9ef0:8683::-fd53:9ef0:8683::3
      aggregation: default
    skipIPv6DAD: false  # Explicit (default)
```

**IMPORTANT BEHAVIOR CHANGE:**
- v1 `spec.local`: All nodes participate in election, winner uses local interface OR kubelb0 fallback
- v2 `spec.local`: Only nodes with matching subnet participate, NO kubelb0 fallback
- v2 `spec.remote`: All nodes announce to kubelb0 (BGP/ECMP use case)

**Decision required:** For each v1 ServiceGroup, decide:
- Use `spec.local` if IPs should be announced on real interfaces by nodes with matching subnets
- Use `spec.remote` if IPs should be announced on kubelb0 by all nodes (for BGP redistribution)

#### Step 3: Convert LBNodeAgent CRs

**v1 LBNodeAgent (before):**
```yaml
apiVersion: purelb.io/v1
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb
spec:
  local:
    localint: default
    extlbint: kube-lb0
    sendgarp: true
```

**v2 LBNodeAgent (after):**
```yaml
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb
spec:
  local:
    localInterface: default      # Renamed from localint
    dummyInterface: kube-lb0  # Renamed from extlbint
    interfaces: []               # NEW: Additional interfaces for election
    garpConfig:                  # Restructured from sendgarp
      enabled: true
      count: 1                   # NEW: Number of GARPs to send
      intervalMs: 500            # NEW: Delay between GARPs
      delayMs: 200               # NEW: Initial delay before first GARP
```

#### Step 4: Automated Migration Script

```bash
#!/bin/bash
# migrate-purelb-v1-to-v2.sh

set -e

NAMESPACE="${PURELB_NAMESPACE:-purelb}"

echo "=== PureLB v1 to v2 Migration ==="
echo ""

# Export v1 resources
echo "Step 1: Exporting v1 resources..."
kubectl get servicegroups.purelb.io -n "$NAMESPACE" -o yaml > servicegroups-v1-backup.yaml
kubectl get lbnodeagents.purelb.io -n "$NAMESPACE" -o yaml > lbnodeagents-v1-backup.yaml

# Convert ServiceGroups
echo "Step 2: Converting ServiceGroups..."
kubectl get servicegroups.purelb.io -n "$NAMESPACE" -o json | jq '
  .items[] | {
    apiVersion: "purelb.io/v2",
    kind: "ServiceGroup",
    metadata: {
      name: .metadata.name,
      namespace: .metadata.namespace
    },
    spec: (
      if .spec.local then
        { local: (.spec.local + { skipIPv6DAD: false }) }
      elif .spec.netbox then
        { netbox: .spec.netbox }
      else
        {}
      end
    )
  }
' > servicegroups-v2.json

# Convert LBNodeAgents
echo "Step 3: Converting LBNodeAgents..."
kubectl get lbnodeagents.purelb.io -n "$NAMESPACE" -o json | jq '
  .items[] | {
    apiVersion: "purelb.io/v2",
    kind: "LBNodeAgent",
    metadata: {
      name: .metadata.name,
      namespace: .metadata.namespace
    },
    spec: {
      local: {
        localInterface: (.spec.local.localint // "default"),
        dummyInterface: (.spec.local.extlbint // "kube-lb0"),
        interfaces: [],
        garpConfig: {
          enabled: (.spec.local.sendgarp // false),
          count: 1,
          intervalMs: 500,
          delayMs: 200
        }
      }
    }
  }
' > lbnodeagents-v2.json

echo ""
echo "=== Review generated files ==="
echo "  - servicegroups-v2.json"
echo "  - lbnodeagents-v2.json"
echo ""
echo "IMPORTANT: Review the 'local' vs 'remote' choice for each ServiceGroup!"
echo "  - 'local': Subnet-aware election (new behavior)"
echo "  - 'remote': All-nodes announce to kubelb0 (v1-like for BGP)"
echo ""
echo "When ready, apply with:"
echo "  kubectl apply -f servicegroups-v2.json"
echo "  kubectl apply -f lbnodeagents-v2.json"
echo ""
echo "After verification, delete v1 resources:"
echo "  kubectl delete servicegroups.purelb.io -n $NAMESPACE --all"
echo "  kubectl delete lbnodeagents.purelb.io -n $NAMESPACE --all"
```

#### Step 5: Upgrade PureLB

```bash
# Upgrade Helm release to v2-compatible version
helm upgrade purelb purelb/purelb \
  --namespace purelb \
  --set apiVersion=v2

# Verify pods are running
kubectl get pods -n purelb

# Check for any errors in logs
kubectl logs -n purelb -l app.kubernetes.io/name=lbnodeagent --tail=50
```

#### Step 6: Verify Services

```bash
# Check all LoadBalancer services still have IPs
kubectl get svc -A -o wide | grep LoadBalancer

# Verify announcements are working (check node logs)
kubectl logs -n purelb -l app.kubernetes.io/name=lbnodeagent | grep -i winner

# Test connectivity to a service
curl http://<service-ip>
```

#### Step 7: Remove v1 CRDs (Optional)

Once migration is verified:

```bash
# Delete v1 CRDs (this will fail if any v1 resources still exist)
kubectl delete crd servicegroups.purelb.io --field-selector metadata.name=servicegroups.purelb.io
# Note: v2 CRD has same name but different version
```

### Rollback Procedure

If issues occur during migration:

```bash
# Restore v1 resources from backup
kubectl apply -f servicegroups-v1-backup.yaml
kubectl apply -f lbnodeagents-v1-backup.yaml

# Downgrade Helm release
helm rollback purelb

# Delete v2 resources if created
kubectl delete servicegroups.purelb.io -n purelb --field-selector apiVersion=purelb.io/v2
kubectl delete lbnodeagents.purelb.io -n purelb --field-selector apiVersion=purelb.io/v2
```

### Side-by-Side Migration (Recommended)

**v1 API is no longer supported.** Instead of dual-version compatibility, use Kubernetes' LoadBalancerClass feature to run old and new PureLB side-by-side:

1. **Deploy new PureLB v2** with a different LoadBalancerClass (`purelb.io/purelb-v2`)
2. **Migrate services** one at a time by updating their `spec.loadBalancerClass`
3. **Remove old PureLB** once all services are migrated

```bash
# Install new PureLB with different LoadBalancerClass
helm install purelb-v2 purelb/purelb \
  --namespace purelb-v2 \
  --create-namespace \
  --set loadBalancerClass=purelb.io/purelb-v2

# Migrate a service
kubectl patch svc my-service -p '{"spec":{"loadBalancerClass":"purelb.io/purelb-v2"}}'

# After all services migrated, remove old PureLB
helm uninstall purelb --namespace purelb
```

This approach provides zero-downtime migration without CRD version conversion complexity.

## Files to Modify

### Core Election Package

**`internal/election/election.go`** - Rewrite:

```go
type Config struct {
    Namespace     string
    NodeName      string
    Client        kubernetes.Interface
    LeaseDuration time.Duration
    RenewDeadline time.Duration
    RetryPeriod   time.Duration
    Logger        log.Logger
    StopCh        chan struct{}
    OnMemberChange func()
    GetLocalSubnets func() []string  // callback to get this node's subnets
}

// electionState holds immutable snapshot of election data
// Used with atomic.Pointer for lock-free access
type electionState struct {
    liveNodes     []string
    subnetToNodes map[string][]string
    nodeToSubnets map[string][]string
}

type Election struct {
    config          Config
    state           atomic.Pointer[electionState]  // Lock-free via atomic swap
    leaseInformer   cache.SharedIndexInformer
    leaseHealthy    atomic.Bool                    // Tracks our own lease health
    renewFailures   atomic.Int32                   // Count of consecutive renewal failures
}

func (e *Election) Winner(key string) string      // Subnet-filtered election
func (e *Election) MemberCount() int              // For logging
func (e *Election) HasLocalCandidate(ip string) bool  // Check if any node can announce
func (e *Election) IsHealthy() bool               // Check if our lease is valid
func (e *Election) Start() error
func (e *Election) Shutdown()

// rebuildMaps creates new state and atomically swaps it (lock-free)
func (e *Election) rebuildMaps() {
    newState := &electionState{
        liveNodes:     make([]string, 0),
        subnetToNodes: make(map[string][]string),
        nodeToSubnets: make(map[string][]string),
    }
    // ... populate from lease informer cache ...
    e.state.Store(newState)  // Atomic swap
}

// Winner reads state atomically (lock-free)
// CRITICAL: Returns "" if our own lease is unhealthy to prevent split-brain
func (e *Election) Winner(ipStr string) string {
    // Self-health check: if our lease is unhealthy, we cannot participate
    // This prevents split-brain during API server partitions
    if !e.leaseHealthy.Load() {
        return ""  // Force withdrawal of all announcements
    }

    state := e.state.Load()  // Atomic load
    // ... use state.subnetToNodes ...
}

// IsHealthy reports whether this node's lease is currently valid
func (e *Election) IsHealthy() bool {
    return e.leaseHealthy.Load()
}
```

**Membership change notification:**
When leases change (node added/removed/subnet changed), the election must trigger service re-evaluation:
```go
// In lease informer event handlers:
func (e *Election) onLeaseAdd(obj interface{}) {
    e.rebuildMaps()
    e.config.OnMemberChange()  // Calls client.ForceSync()
}
```
The `OnMemberChange` callback replaces the current memberlist event handling in `watchEvents()`.

**`internal/election/subnets.go`** - New file for subnet detection:

```go
// GetLocalSubnets returns all subnets from configured interfaces.
// Leverages existing checkLocal() logic from internal/local/network.go
func GetLocalSubnets(interfaces []string, includeDefault bool) ([]string, error)

// SubnetsAnnotation formats subnets for lease annotation
func SubnetsAnnotation(subnets []string) string  // e.g., "192.168.1.0/24,10.0.0.0/24"

// ParseSubnetsAnnotation parses annotation back to slice
func ParseSubnetsAnnotation(s string) []string
```

**Implementation note:** The subnet detection can reuse patterns from `internal/local/network.go`:
- `netlink.AddrList(intf, family)` to get addresses
- Filter by IPv6 bad flags (DADFAILED, DEPRECATED, TENTATIVE)
- Extract network from `addr.IPNet`

### Local Announcer

**`internal/local/announcer_local.go`**:

- Remove kubelb0 fallback logic for IPs where `Winner()` returns ""
- Only use kubelb0 for explicitly remote pools
- Change election check at line 252:
  ```go
  // Before:
  if winner := a.election.Winner(lbIP.String()); winner != a.myNode {
      l.Log("msg", "notWinner", ...)
      return a.deleteAddress(nsName, "lostElection", lbIP)
  }

  // After:
  winner := a.election.Winner(lbIP.String())
  if winner == "" {
      // No eligible node has this subnet
      l.Log("msg", "noEligibleNodes", "ip", lbIP)
      return nil
  }
  if winner != a.myNode {
      l.Log("msg", "notWinner", ...)
      return a.deleteAddress(nsName, "lostElection", lbIP)
  }

  // PRESERVE existing address lifetime handling (lines 265-269):
  opts := a.getLocalAddressOptions()
  if err := addNetworkWithOptions(lbIPNet, announceInt, opts); err != nil {
      return err
  }
  a.scheduleRenewal(nsName, lbIPNet, announceInt, opts)
  ```

- Modify `SetBalancer()` to check pool type annotation before deciding announcement strategy:
  ```go
  // In SetBalancer(), check pool type annotation set by allocator
  poolType := svc.Annotations[purelbv1.PoolTypeAnnotation] // "local" or "remote"

  if poolType == "remote" {
      // Remote pool: all nodes announce to kubelb0 (existing behavior)
      return a.announceRemote(svc, epSlices, a.dummyInt, lbIP)
  }

  // Local pool: use subnet-aware election
  // Only announce if this node has matching subnet AND wins election
  if a.localNameRegex != nil {
      lbIPNet, localif, err := findLocal(a.localNameRegex, lbIP)
      if err == nil {
          return a.announceLocal(svc, localif, lbIP, lbIPNet)
      }
      // No local interface - with local pool, don't fall back to kubelb0
      return nil
  }
  // ... default interface logic ...
  ```
- Ensure `deleteAddress()` calls `cancelRenewal()` (already does at line 389)

### Main Entry Point

**`cmd/lbnodeagent/main.go`**:
- Remove memberlist setup
- Add lease timing configuration
- Pass `GetLocalSubnets` callback to election
- Call `election.Start()`

### Helm Templates

**`build/helm/purelb/templates/clusterrole-lbnodeagent.yaml`**:
```yaml
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update"]
```

**`build/helm/purelb/templates/daemonset.yaml`**:
- Remove: `PURELB_ML_LABELS`, `ML_GROUP`, `PURELB_HOST`
- Keep: `PURELB_NAMESPACE`
- Add: `PURELB_LEASE_DURATION`, `PURELB_RENEW_DEADLINE`, `PURELB_RETRY_PERIOD`

**`build/helm/purelb/values.yaml`**:
- Remove: `memberlistSecretKey`
- Add: `leaseElection` section

### API Types

**`pkg/apis/purelb/v1/types.go`** - Add Remote spec type:

```go
// ServiceGroupSpec configures the allocator. Exactly one of Local, Remote,
// or Netbox must be specified.
// +kubebuilder:validation:MaxProperties=1
// +kubebuilder:validation:MinProperties=1
type ServiceGroupSpec struct {
    // +optional
    Local *ServiceGroupLocalSpec `json:"local,omitempty"`
    // +optional
    Remote *ServiceGroupRemoteSpec `json:"remote,omitempty"`
    // +optional
    Netbox *ServiceGroupNetboxSpec `json:"netbox,omitempty"`
}

// ServiceGroupRemoteSpec configures pools for remote/BGP announcement.
// All nodes announce these addresses to kubelb0 for routing software.
type ServiceGroupRemoteSpec struct {
    // +optional
    V4Pool *ServiceGroupAddressPool `json:"v4pool,omitempty"`
    // +optional
    V6Pool *ServiceGroupAddressPool `json:"v6pool,omitempty"`
    // +optional
    V4Pools []*ServiceGroupAddressPool `json:"v4pools,omitempty"`
    // +optional
    V6Pools []*ServiceGroupAddressPool `json:"v6pools,omitempty"`
}
```

Note: `ServiceGroupRemoteSpec` mirrors `ServiceGroupLocalSpec` structure for consistency.

**`pkg/apis/purelb/v1/types.go`** - Add Interfaces field to LBNodeAgentLocalSpec:

```go
// LBNodeAgentLocalSpec configures the announcers to announce service
// addresses by configuring the underlying operating system.
type LBNodeAgentLocalSpec struct {
    // LocalInterface specifies how to find the local announcement interface.
    // - "default": auto-detect interface with default route (current behavior)
    // - "none": disable auto-detection, only use interfaces[]
    // +kubebuilder:default="default"
    // +optional
    LocalInterface string `json:"localint"`

    // ExtLBInterface specifies the interface for non-local (remote) announcements.
    // +kubebuilder:default="kube-lb0"
    // +optional
    ExtLBInterface string `json:"extlbint"`

    // SendGratuitousARP enables GARP messages when adding IPs.
    // +kubebuilder:default=false
    SendGratuitousARP bool `json:"sendgarp"`

    // EXISTING: AddressConfig configures how VIP addresses are added to interfaces.
    // Controls address lifetimes and flags to prevent conflicts with CNI plugins.
    // +optional
    AddressConfig *AddressConfig `json:"addressConfig,omitempty"`

    // NEW: Interfaces specifies additional interfaces for election participation.
    // Subnets from these interfaces are included in the lease annotation.
    // Combined with LocalInterface setting to determine full interface list.
    // +optional
    Interfaces []string `json:"interfaces,omitempty"`
}

// EXISTING: AddressConfig specifies how IP addresses should be configured on different
// interface types. This allows fine-grained control over address lifetimes
// and flags to avoid conflicts with CNI plugins like Flannel.
type AddressConfig struct {
    // LocalInterface configures addresses on the local interface (e.g., eth0).
    // +optional
    LocalInterface *InterfaceAddressConfig `json:"localInterface,omitempty"`

    // DummyInterface configures addresses on the dummy interface (e.g., kube-lb0).
    // +optional
    DummyInterface *InterfaceAddressConfig `json:"dummyInterface,omitempty"`
}

// EXISTING: InterfaceAddressConfig specifies address configuration for an interface type.
type InterfaceAddressConfig struct {
    // ValidLifetime is the valid lifetime in seconds. 0 means permanent.
    // Default: 300 for local interface, 0 for dummy interface.
    // +kubebuilder:validation:Minimum=0
    // +optional
    ValidLifetime *int `json:"validLifetime,omitempty"`

    // PreferredLifetime is the preferred lifetime in seconds. Must be <= ValidLifetime.
    // +kubebuilder:validation:Minimum=0
    // +optional
    PreferredLifetime *int `json:"preferredLifetime,omitempty"`

    // NoPrefixRoute prevents automatic prefix route creation when true.
    // Default: true for local interface, false for dummy interface.
    // +optional
    NoPrefixRoute *bool `json:"noPrefixRoute,omitempty"`
}
```

After modifying types, run:
```bash
make generate  # Regenerate client code
make crd       # Regenerate CRD manifests
```

### Allocator Changes

**`internal/allocator/`**:

The allocator must set a pool type annotation so the announcer knows how to handle the IP:

```go
// In pkg/apis/purelb/v1/annotations.go - Add new annotation
const PoolTypeAnnotation = "purelb.io/pool-type"  // "local" or "remote"
```

**When allocating from a ServiceGroup:**
```go
// After allocating IP, set pool type annotation
if group.Spec.Local != nil {
    svc.Annotations[purelbv1.PoolTypeAnnotation] = "local"
} else if group.Spec.Remote != nil {
    svc.Annotations[purelbv1.PoolTypeAnnotation] = "remote"
} else if group.Spec.Netbox != nil {
    // Netbox pools behave like remote (all nodes announce)
    svc.Annotations[purelbv1.PoolTypeAnnotation] = "remote"
}
```

**Files to modify:**
- `pkg/apis/purelb/v1/annotations.go` - Add `PoolTypeAnnotation` constant
- `internal/allocator/service.go` - Set annotation when allocating

### Cleanup

- Remove memberlist from go.mod
- Update package.go docs

## Implementation Sequence

1. **Phase 1**: Subnet detection
   - Implement `GetLocalSubnets()` using netlink (leverage patterns from `internal/local/network.go`)
   - Unit tests for subnet detection and annotation parsing

2. **Phase 2**: Lease-based election with subnet mapping
   - Create lease with subnet annotations
   - Watch leases and build subnet→nodes map
   - Implement filtered `Winner()`
   - Unit tests

3. **Phase 3**: Update announcer
   - Modify `announceLocal()` to handle `Winner() == ""`
   - **PRESERVE** existing `addNetworkWithOptions()` + `scheduleRenewal()` pattern
   - Remove kubelb0 fallback for local pools
   - Keep remote pool behavior

4. **Phase 4**: Wire up in main.go
   - Remove memberlist
   - Add lease config
   - Pass subnet callback

5. **Phase 5**: Helm/RBAC updates

6. **Phase 6**: Cleanup and testing
   - Verify address renewal still works correctly
   - Test CNI compatibility (Flannel IFA_F_PERMANENT issue)

## Phased Implementation Milestones

This breaks the full implementation into independent milestones that can be completed and tested separately. Each milestone is designed to be independently testable, deployable without breaking existing functionality, and reviewable as a separate PR if desired.

### Progress Tracking

| Milestone | Status | Notes |
|-----------|--------|-------|
| 1. Subnet Detection Foundation | ✅ Complete | subnets.go, leveled logging |
| 2. Lease-Based Election Core | ✅ Complete | election.go rewritten, k8s leases |
| 3. Subnet-Aware Winner Election | ✅ Complete | Winner() filters by subnet |
| 4. Announcer Integration | ✅ Complete | SetBalancer uses election, pool-type annotation |
| 5. Graceful Shutdown | ✅ Complete | MarkUnhealthy, WithdrawAll, DeleteOurLease |
| 6. GARP Enhancements | ✅ Complete | GARPConfig with count, interval, delay, verifyBeforeSend |
| 7. API v2 Types | 🔄 Partial | Types exist, need V4Pools/V6Pools arrays, iprange.go |
| 8. Allocator v2 Support | 🔄 Partial | pool-type annotation done, Remote pool not implemented |
| 9. Prometheus Metrics | ✅ Complete | metrics.go in election and local packages |
| 10. Helm & RBAC Updates | ✅ Complete | Leases RBAC added, memberlist removed |
| 11. Migration Tooling | ✅ Complete | scripts/migrate-v1-to-v2.sh, docs/migration-v1-to-v2.md |
| 12. Cleanup & Final Testing | 🔄 In Progress | Memberlist removed, v2 gaps remain |

### Execution Order

```
Parallel Track A (Core):     Parallel Track B (API):
━━━━━━━━━━━━━━━━━━━━━━━     ━━━━━━━━━━━━━━━━━━━━━━━
Milestone 1 (Subnet)         Milestone 7 (API v2 Types)
    ↓                            ↓
Milestone 2 (Lease Core)     Milestone 11 (Migration)
    ↓                            ↓
Milestone 3 (Subnet Winner)  Milestone 8 (Allocator v2)
    ↓
Milestone 4 (Announcer)
    ↓
Milestone 5 (Shutdown)
    ↓
Milestone 6 (GARP)
    ↓
Milestone 9 (Metrics)
    ↓
Milestone 10 (Helm)
    ↓
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                    ↓
            Milestone 12 (Cleanup)
```

---

### Milestone 1: Subnet Detection Foundation
**Goal:** Implement subnet detection without changing any existing behavior

**Files to Create/Modify:**
- `internal/election/subnets.go` (new)
- `internal/election/subnets_test.go` (new)

**Tasks:**
1. Create `GetLocalSubnets(interfaces []string, includeDefault bool) ([]string, error)`
   - Enumerate addresses on specified interfaces using netlink
   - Filter out IPv6 with bad flags (DADFAILED, DEPRECATED, TENTATIVE)
   - Return list of CIDR strings (e.g., "192.168.1.0/24")

2. Create annotation helpers:
   - `SubnetsAnnotation(subnets []string) string` - comma-separated
   - `ParseSubnetsAnnotation(s string) []string`

3. Unit tests for all functions

**Verification:**
```bash
go test -race -v ./internal/election/...
```

**Depends On:** Nothing (can start immediately)

---

### Milestone 2: Lease-Based Election Core
**Goal:** Replace memberlist with K8s Leases (without subnet filtering yet)

**Files to Create/Modify:**
- `internal/election/election.go` (rewrite)
- `internal/election/election_test.go` (rewrite)

**Tasks:**
1. Create `Election` struct with:
   - `atomic.Pointer[electionState]` for lock-free state
   - `atomic.Bool` for `leaseHealthy`
   - `atomic.Int32` for `renewFailures`
   - Lease informer via SharedIndexInformer

2. Implement lease lifecycle:
   - `createLease()` with OwnerReference to Node
   - `renewLease()` with failure recovery
   - `DeleteOurLease()` for graceful shutdown

3. Implement `rebuildMaps()` with atomic swap

4. Implement `Winner(ipStr string)` - initially WITHOUT subnet filtering
   - Just return hash-based winner from all live nodes
   - Add `leaseHealthy` check at the top

5. Add `OnMemberChange` callback integration

**Verification:**
```bash
go test -race -v ./internal/election/...
# Deploy to test cluster, verify leases are created/renewed
kubectl get leases -n purelb
```

**Depends On:** Milestone 1 (for annotation helpers)

---

### Milestone 3: Subnet-Aware Winner Election
**Goal:** Add subnet filtering to the election

**Files to Modify:**
- `internal/election/election.go`
- `internal/election/election_test.go`

**Tasks:**
1. Update `electionState` to include:
   - `subnetToNodes map[string][]string`
   - `nodeToSubnets map[string][]string`

2. Update `rebuildMaps()` to parse subnet annotations from leases

3. Update `Winner(ipStr string)` to:
   - Find all subnets containing the IP
   - Build candidate list from matching nodes
   - Return "" if no candidates
   - Run hash election on candidates

4. Add `HasLocalCandidate(ip string) bool` helper

5. Update lease creation to include subnet annotation

**Verification:**
```bash
go test -race -v ./internal/election/...
# Test with nodes on different subnets
```

**Depends On:** Milestone 2

---

### Milestone 4: Announcer Integration
**Goal:** Wire election changes into the announcer

**Files to Modify:**
- `internal/local/announcer_local.go`
- `internal/local/network.go`

**Tasks:**
1. Handle `Winner() == ""` case (no eligible nodes)
   - Don't fall back to kubelb0 for local pools
   - Log "noEligibleNodes"

2. Add pool type annotation check:
   - Read `purelb.io/pool-type` annotation
   - Route to `announceLocal()` or `announceRemote()` accordingly

3. Add `addressRenewal.cancelled atomic.Bool` for race fix

4. Update watchdog to verify election before re-adding

5. Add `WithdrawAll()` method for graceful shutdown

6. Add `NoDAD` support to `AddressOptions` and `addNetworkWithOptions()`

**Verification:**
```bash
go test -race -v ./internal/local/...
# Deploy and verify announcements work correctly
```

**Depends On:** Milestone 3

---

### Milestone 5: Graceful Shutdown
**Goal:** Implement proper shutdown sequence

**Files to Modify:**
- `cmd/lbnodeagent/main.go`
- `internal/election/election.go`
- `build/helm/purelb/templates/daemonset.yaml`

**Tasks:**
1. Add signal handler for SIGTERM/SIGINT in main.go
2. Implement shutdown sequence:
   - `MarkUnhealthy()`
   - `WithdrawAll()`
   - `DeleteOurLease()`
   - Sleep 2s
3. Update DaemonSet with `terminationGracePeriodSeconds: 30`
4. Add preStop hook

**Verification:**
```bash
# Rolling restart, verify no traffic loss
kubectl rollout restart daemonset/lbnodeagent -n purelb
```

**Depends On:** Milestone 4

---

### Milestone 6: GARP Enhancements
**Goal:** Implement configurable GARP with delay and repetition

**Files to Modify:**
- `pkg/apis/purelb/v1/types.go` (add GARPConfig to existing v1 for now)
- `internal/local/announcer_local.go`

**Tasks:**
1. Add `GARPConfig` struct to LBNodeAgentLocalSpec
2. Keep `sendgarp` for backward compatibility (deprecated)
3. Implement `sendGARPSequence()` with:
   - Initial delay
   - Re-verification before each GARP
   - Configurable count and interval

**Verification:**
```bash
# Configure garpConfig, verify multiple GARPs sent
```

**Depends On:** Milestone 4

---

### Milestone 7: API v2 Types
**Goal:** Create the v2 API package

**Files to Create:**
- `pkg/apis/purelb/v2/doc.go`
- `pkg/apis/purelb/v2/groupversion_info.go`
- `pkg/apis/purelb/v2/register.go`
- `pkg/apis/purelb/v2/types.go`
- `pkg/apis/purelb/v2/annotations.go`

**Tasks:**
1. Create v2 package structure
2. Define `ServiceGroup` with `Local`/`Remote`/`Netbox` (mutually exclusive)
3. Define `LBNodeAgent` with renamed fields and `GARPConfig`
4. Add `skipIPv6DAD` to `ServiceGroupLocalSpec`
5. Add kubebuilder validation markers
6. Run `make generate` and `make crd`

**Verification:**
```bash
make generate
make crd
# Inspect generated CRDs
```

**Depends On:** Nothing (can be done in parallel with Milestones 1-6)

---

### Milestone 8: Allocator v2 Support
**Goal:** Update allocator to work with v2 ServiceGroups

**Files to Modify:**
- `internal/allocator/service.go`
- `internal/allocator/pool.go`

**Tasks:**
1. Add type detection for v1 vs v2 ServiceGroups
2. Set `purelb.io/pool-type` annotation ("local" or "remote")
3. Set `purelb.io/skip-ipv6-dad` annotation when configured
4. Add v1 deprecation warning logs

**Verification:**
```bash
go test -race -v ./internal/allocator/...
# Create v2 ServiceGroup, verify annotations on Services
```

**Depends On:** Milestone 7

---

### Milestone 9: Prometheus Metrics
**Goal:** Add observability metrics

**Files to Create/Modify:**
- `internal/election/metrics.go` (new)
- `internal/election/election.go`
- `internal/local/announcer_local.go`

**Tasks:**
1. Define all metrics (election changes, lease health, renewals, GARP, watchdog)
2. Instrument election code
3. Instrument announcer code
4. Verify metrics endpoint works

**Verification:**
```bash
curl http://<pod-ip>:7472/metrics | grep purelb_
```

**Depends On:** Milestone 4

---

### Milestone 10: Helm & RBAC Updates
**Goal:** Update Helm chart for new features

**Files to Modify:**
- `build/helm/purelb/templates/clusterrole-lbnodeagent.yaml`
- `build/helm/purelb/templates/daemonset.yaml`
- `build/helm/purelb/values.yaml`

**Tasks:**
1. Add RBAC for `coordination.k8s.io/leases`
2. Add RBAC for `nodes` (get)
3. Remove memberlist env vars
4. Add lease config env vars
5. Add `terminationGracePeriodSeconds`
6. Package updated chart

**Verification:**
```bash
make helm
helm template purelb ./build/helm/purelb
```

**Depends On:** Milestone 5

---

### Milestone 11: Migration Tooling
**Goal:** Create migration script and documentation

**Files to Create:**
- `scripts/migrate-v1-to-v2.sh`
- `docs/migration-v1-to-v2.md`

**Tasks:**
1. Write jq-based migration script
2. Write user-facing migration guide
3. Document `local` vs `remote` decision

**Verification:**
```bash
# Test migration script on sample v1 resources
./scripts/migrate-v1-to-v2.sh
```

**Depends On:** Milestone 7

---

### Milestone 12: Cleanup & Final Testing
**Goal:** Remove memberlist, comprehensive testing

**Files to Modify:**
- `go.mod` (remove memberlist)
- Various cleanup

**Tasks:**
1. Remove memberlist dependency
2. Remove old election code
3. Full integration test suite
4. Manual testing per checklist
5. Update CLAUDE.md with final guidance

**Verification:**
- All tests pass
- SLA targets met (graceful < 5s, abrupt < 15s)
- Rolling upgrade works
- API server partition recovery works

**Depends On:** All previous milestones

---

## Decisions Made

1. **Subnet change detection**: Watch netlink for `RTM_NEWADDR`/`RTM_DELADDR` events for real-time updates

2. **Pool types**: Mutually exclusive `spec.local` and `spec.remote` in ServiceGroup CR
   - `local`: Subnet-filtered election, IP on real interface
   - `remote`: All nodes announce to kubelb0 (BGP/ECMP)
   - Enforced via kubebuilder validation

3. **Overlapping subnets**: If Node-A has 10.0.0.0/16 and Node-B has 10.0.1.0/24:
   - IP 10.0.1.50 matches both subnets
   - Both nodes are candidates for election
   - Hash election decides winner deterministically
   - **This is intentional behavior**: We do NOT implement longest-prefix-match because:
     - Both nodes can legitimately route to the IP
     - Longest-prefix-match would reduce redundancy (fewer candidates)
     - The hash election ensures consistent winner across all nodes
   - **Network design guidance**: If you want only specific nodes to handle certain IPs:
     - Use separate ServiceGroups with non-overlapping pools
     - Or use node selectors/tolerations on your Services
   - **Example scenarios**:
     ```
     Scenario A: Node-A (10.0.0.0/16), Node-B (10.0.1.0/24)
       IP 10.0.1.50 → candidates = [Node-A, Node-B] → hash picks one

     Scenario B: Node-A (192.168.1.0/24), Node-B (192.168.2.0/24)
       IP 192.168.1.50 → candidates = [Node-A] → Node-A wins (only candidate)

     Scenario C: Node-A (10.0.0.0/8), Node-B (10.0.0.0/8), Node-C (172.16.0.0/12)
       IP 10.1.2.3 → candidates = [Node-A, Node-B] → hash picks one
       IP 172.16.5.5 → candidates = [Node-C] → Node-C wins
     ```

4. **Multi-pool allocation**: Opt-in via `purelb.io/multi-pool: "true"` annotation on Service
   - Default: One IP per family (current behavior)
   - With annotation: One IP from each pool with matching subnet
   - Requires Allocator changes (not just lbnodeagent)

5. **Interface configuration**: `localInterface` controls auto-detect mode, `interfaces[]` adds extra interfaces
   - `localInterface: default` + `interfaces: []` = current behavior
   - `localInterface: none` + `interfaces: [eth0]` = explicit only

## Design Principle: Avoid Locks and Mutexes

**Locks and mutexes should be avoided wherever possible.** They introduce complexity, potential deadlocks, contention, and are difficult to reason about. When concurrent access is needed, prefer these alternatives in order:

### Preferred Approaches (Lock-Free)

1. **Single-goroutine ownership**: Design so only one goroutine accesses mutable state
   - Example: K8s work queue processes items sequentially
   - Example: `svcIngresses` map is only accessed from work queue goroutine

2. **Atomic operations**: Use `atomic.Bool`, `atomic.Int64`, `atomic.Pointer[T]`
   - Example: `addressRenewal.cancelled` uses `atomic.Bool`
   - Example: Election state uses `atomic.Pointer[electionState]` for copy-on-write

3. **sync.Map**: For simple key-value stores with concurrent access
   - Example: `addressRenewals sync.Map` for renewal timer tracking
   - Note: Not ideal for iteration-heavy workloads

4. **Channels**: For coordination between goroutines
   - Example: `stopCh chan struct{}` for shutdown signaling

5. **Immutable data + atomic swap**: Build new state, swap atomically
   - Example: `rebuildMaps()` creates new `electionState`, calls `state.Store(newState)`
   - Readers see old OR new state, never partial updates

### When Locks Might Seem Necessary

If you find yourself reaching for `sync.Mutex` or `sync.RWMutex`, first ask:

1. Can the data be owned by a single goroutine?
2. Can the operation be made atomic?
3. Can the data structure be replaced with atomic pointer swap?
4. Can the coordination be done via channels?

### If a Lock is Truly Required

Document it clearly with:
- Why lock-free alternatives don't work
- What goroutines contend for the lock
- Lock ordering if multiple locks exist
- Consider using `sync.RWMutex` if reads >> writes

**No locks are used in this implementation.** All concurrent access is handled via:
- `atomic.Pointer[electionState]` for election maps
- `atomic.Bool` for renewal cancellation
- `sync.Map` for address renewals

## Race Conditions & Mitigations

This section documents critical race conditions identified during plan review and required mitigations.

### Critical: Address Renewal Race

**Problem**: Timer fires to renew address, but election changed and node should delete instead. Timer goroutine may have already started before `cancelRenewal()` runs.

**Fix**: Add atomic cancelled flag to `addressRenewal`:
```go
type addressRenewal struct {
    // existing fields...
    cancelled atomic.Bool
}

func (a *announcer) renewAddress(key string) {
    val, ok := a.addressRenewals.Load(key)
    if !ok || val.(*addressRenewal).cancelled.Load() {
        return  // Cancelled or deleted
    }
    // ... proceed with renewal
}

func (a *announcer) cancelRenewal(svcName, ip string) {
    if val, loaded := a.addressRenewals.Load(key); loaded {
        val.(*addressRenewal).cancelled.Store(true)  // Set BEFORE Stop()
        val.(*addressRenewal).timer.Stop()
        a.addressRenewals.Delete(key)
    }
}
```

### Critical: Lease Renewal Failure Recovery

**Problem**: Node healthy but lease renewal fails (API unavailable). Other nodes consider it dead and take over. When API recovers, both nodes announce.

**Fix**: Self-withdrawal on renewal failure using atomic health flag:
```go
const maxRenewFailures = 3  // ~15s with 5s retry period

func (e *Election) renewLease() error {
    err := e.client.CoordinationV1().Leases(...).Update(...)
    if err != nil {
        failures := e.renewFailures.Add(1)
        if failures >= maxRenewFailures {
            // Mark ourselves unhealthy - Winner() will return "" for all queries
            // This is atomic and immediately affects all election decisions
            e.leaseHealthy.Store(false)
            e.logger.Log("op", "renewLease", "status", "unhealthy",
                "failures", failures, "action", "withdrawing all announcements")
            e.config.OnRenewalFailure()  // Triggers announcer to delete all addresses
        }
        return err
    }
    // Success - reset failures and mark healthy
    e.renewFailures.Store(0)
    if !e.leaseHealthy.Load() {
        e.logger.Log("op", "renewLease", "status", "recovered", "action", "re-enabling elections")
        e.leaseHealthy.Store(true)
        e.config.OnMemberChange()  // Re-evaluate all services
    }
    return nil
}
```

### High: GARP Timing and Repetition

**Problem**: Winner sends GARP immediately, but loser hasn't deleted address yet. Additionally, some network equipment (e.g., Cisco with 4-hour ARP cache) may ignore or miss single GARPs.

**Fix**: Delay GARP, re-verify winner, and support configurable repetition:

```go
// LBNodeAgentLocalSpec addition
type LBNodeAgentLocalSpec struct {
    // ... existing fields ...

    // GARPConfig configures gratuitous ARP behavior
    // +optional
    GARPConfig *GARPConfig `json:"garpConfig,omitempty"`
}

// GARPConfig controls GARP packet behavior
type GARPConfig struct {
    // Enabled turns on gratuitous ARP (replaces sendgarp bool)
    // +kubebuilder:default=false
    Enabled bool `json:"enabled"`

    // Count is the number of GARP packets to send (for stubborn network equipment)
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=10
    Count int `json:"count,omitempty"`

    // IntervalMs is the delay between GARP packets in milliseconds
    // +kubebuilder:default=500
    // +kubebuilder:validation:Minimum=100
    // +kubebuilder:validation:Maximum=5000
    IntervalMs int `json:"intervalMs,omitempty"`

    // DelayMs is the initial delay before first GARP (allows loser to withdraw)
    // +kubebuilder:default=200
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=5000
    DelayMs int `json:"delayMs,omitempty"`
}

// Implementation
func (a *announcer) sendGARPSequence(intf string, ip net.IP) {
    cfg := a.getGARPConfig()
    if !cfg.Enabled {
        return
    }

    // Initial delay to allow loser to withdraw
    time.AfterFunc(time.Duration(cfg.DelayMs)*time.Millisecond, func() {
        for i := 0; i < cfg.Count; i++ {
            // Re-verify we're still winner before each GARP
            if a.election.Winner(ip.String()) != a.myNode {
                a.logger.Log("op", "garp", "status", "aborted", "ip", ip, "reason", "no longer winner")
                return
            }

            if err := sendGARP(intf, ip); err != nil {
                a.logger.Log("op", "garp", "status", "error", "ip", ip, "error", err)
            } else {
                garpSent.WithLabelValues(a.myNode, intf, ip.String()).Inc()
                a.logger.Log("op", "garp", "status", "sent", "ip", ip, "seq", i+1, "of", cfg.Count)
            }

            // Delay between GARPs (except after last one)
            if i < cfg.Count-1 {
                time.Sleep(time.Duration(cfg.IntervalMs) * time.Millisecond)
            }
        }
    })
}
```

**Example configurations:**

```yaml
# Default: single GARP after 200ms delay
garpConfig:
  enabled: true

# Aggressive: 3 GARPs, 500ms apart, for stubborn network equipment
garpConfig:
  enabled: true
  count: 3
  intervalMs: 500
  delayMs: 200

# Conservative: longer initial delay for slow networks
garpConfig:
  enabled: true
  count: 2
  intervalMs: 1000
  delayMs: 500
```
```

### High: Informer Sync Order

**Problem**: Service informer triggers `SetBalancer()` before lease informer has synced.

**Fix**: Add lease informer's `HasSynced` to sync barrier:
```go
// In k8s.go Run(), add lease informer to syncFuncs
c.syncFuncs = append(c.syncFuncs, leaseInformer.HasSynced)

// In Election.Winner(), defensive check
func (e *Election) Winner(ipStr string) string {
    if !e.leaseInformer.HasSynced() {
        return ""  // Signal "unknown state" - will requeue
    }
    // ...
}
```

### Medium: Subnet Change Debouncing

**Problem**: During interface flap, netlink events arrive rapidly, causing churn.

**Fix**: Debounce subnet updates:
```go
func (w *subnetWatcher) onNetlinkEvent(update netlink.AddrUpdate) {
    // Don't apply immediately - debounce
    if w.debounceTimer != nil {
        w.debounceTimer.Stop()
    }
    w.debounceTimer = time.AfterFunc(2*time.Second, func() {
        // Re-read current state (don't trust event sequence)
        currentSubnets := GetLocalSubnets(w.interfaces, w.includeDefault)
        w.updateLeaseAnnotation(currentSubnets)
    })
}
```

### Critical: Graceful Shutdown

**Problem**: When lbnodeagent pod terminates (rolling update, node drain), it must withdraw announcements BEFORE dying. Otherwise:
- Other nodes don't know to take over immediately
- Traffic continues to route to terminating pod
- New winner's GARP may be ignored if old pod still responding

**Fix**: Implement graceful shutdown sequence with preStop hook:

```go
// In main.go - handle termination signals
func main() {
    // ... setup code ...

    // Setup signal handling for graceful shutdown
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

    go func() {
        <-sigCh
        logger.Log("op", "shutdown", "status", "starting", "action", "withdrawing announcements")

        // Step 1: Mark ourselves unhealthy so Winner() returns "" for us
        election.MarkUnhealthy()

        // Step 2: Withdraw all announcements (this triggers re-election on other nodes)
        announcer.WithdrawAll()

        // Step 3: Delete our lease so other nodes see us gone immediately
        election.DeleteOurLease()

        // Step 4: Brief pause to allow new winners to send GARPs
        time.Sleep(2 * time.Second)

        logger.Log("op", "shutdown", "status", "complete")
        os.Exit(0)
    }()
}

// Election method to mark self unhealthy
func (e *Election) MarkUnhealthy() {
    e.leaseHealthy.Store(false)
}

// Election method to delete our lease immediately
func (e *Election) DeleteOurLease() error {
    return e.client.CoordinationV1().Leases(e.config.Namespace).Delete(
        context.Background(),
        e.leaseName,
        metav1.DeleteOptions{},
    )
}

// Announcer method to withdraw all addresses
func (a *announcer) WithdrawAll() {
    a.addressRenewals.Range(func(key, val interface{}) bool {
        renewal := val.(*addressRenewal)
        a.logger.Log("op", "withdrawAll", "ip", renewal.ipNet.IP)
        a.cancelRenewal(renewal.svcName, renewal.ipNet.IP.String())
        deleteAddr(renewal.ipNet, renewal.link)
        return true
    })
}
```

**Helm DaemonSet changes** (`build/helm/purelb/templates/daemonset.yaml`):
```yaml
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 30  # Allow time for graceful shutdown
      containers:
      - name: lbnodeagent
        lifecycle:
          preStop:
            exec:
              # Signal handler in Go does the work, this just ensures minimum time
              command: ["/bin/sh", "-c", "sleep 5"]
```

### Medium: Address Lifetime Watchdog

**Problem**: Renewal timer delayed by GC pause, address expires.

**Fix**: Watchdog goroutine that checks addresses exist AND verifies election status:
```go
func (a *announcer) watchdogLoop() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        a.addressRenewals.Range(func(key, val interface{}) bool {
            renewal := val.(*addressRenewal)
            ipStr := renewal.ipNet.IP.String()

            // CRITICAL: Re-verify election before any action
            winner := a.election.Winner(ipStr)

            if winner != a.myNode {
                // We lost election - clean up stale renewal entry
                a.logger.Log("op", "watchdog", "action", "cleanup", "ip", ipStr,
                    "reason", "no longer winner", "winner", winner)
                a.cancelRenewal(renewal.svcName, ipStr)
                return true
            }

            if !a.addressExistsOnInterface(renewal.ipNet, renewal.link) {
                a.logger.Log("op", "watchdog", "action", "re-adding", "ip", ipStr)
                addNetworkWithOptions(renewal.ipNet, renewal.link, renewal.opts)
            }
            return true
        })
    }
}
```

## Testing Checklist

### Unit Tests
- [ ] Subnet detection (`GetLocalSubnets()`)
- [ ] Subnet annotation parsing (`SubnetsAnnotation()`, `ParseSubnetsAnnotation()`)
- [ ] Subnet→nodes mapping builds correctly
- [ ] Filtered `Winner()` returns correct candidates
- [ ] `Winner()` returns "" when no nodes have subnet

### Integration Tests
- [ ] Nodes with same subnet elect one winner
- [ ] Nodes with different subnets don't interfere
- [ ] Overlapping subnets: both nodes are candidates (10.0.0.0/16 and 10.0.1.0/24)
- [ ] IP not announced if no node has subnet (local pool)
- [ ] Remote pool IPs still announced on kubelb0 by all nodes
- [ ] Node failure triggers re-election among remaining candidates
- [ ] Address renewal still works (addresses don't disappear after 5 min)
- [ ] Pool type annotation set correctly by allocator
- [ ] Multi-pool allocation: service gets IPs from all matching pools
- [ ] Lease garbage collection: lease deleted when Node is deleted (OwnerReference)
- [ ] IPv6 addresses respect ServiceGroup's `skipIPv6DAD` setting
- [ ] IPv6 DAD performed by default (skipIPv6DAD: false)
- [ ] IPv6 DAD skipped when skipIPv6DAD: true (IFA_F_NODAD flag set)
- [ ] Prometheus metrics exported correctly

### CNI Compatibility
- [ ] VIPs don't have IFA_F_PERMANENT flag (Flannel issue)
- [ ] Flannel doesn't select VIPs as node addresses

### Race Condition Tests
- [ ] Renewal timer vs cancellation race (concurrent cancel and renew)
- [ ] GARP only sent after loser has withdrawn
- [ ] GARP sequence aborts if election changes mid-sequence
- [ ] Lease renewal failure triggers address withdrawal
- [ ] Lease recovery after API server partition (no split-brain)
- [ ] Interface flap doesn't cause announcement churn (debouncing works)
- [ ] Address watchdog detects and recovers missing addresses
- [ ] Address watchdog cleans up stale renewals when no longer winner
- [ ] Graceful shutdown withdraws all addresses before pod terminates

### API v2 Migration Tests
- [ ] v1 ServiceGroup still works during transition period (with deprecation warning)
- [ ] v1 LBNodeAgent still works during transition period
- [ ] v2 ServiceGroup with `spec.local` uses subnet-aware election
- [ ] v2 ServiceGroup with `spec.remote` announces on kubelb0 (all nodes)
- [ ] Migration script correctly converts v1 → v2
- [ ] Rollback from v2 to v1 works (within support period)
- [ ] v1→v2 conversion webhook works correctly (if implemented)

### Manual Testing
- [ ] Rolling upgrade from memberlist version
- [ ] Rolling upgrade from v1 API to v2 API
- [ ] API server unavailable for 30s, then recovers (no split-brain)
- [ ] Kill a node abruptly (not graceful), verify failover < 15s (SLA target)
- [ ] Graceful shutdown (rolling update), verify failover < 5s (SLA target)
- [ ] Graceful shutdown: announcements withdrawn before pod dies
- [ ] Verify Prometheus metrics are exported at `/metrics` endpoint
- [ ] Alert rules fire correctly (PureLBLeaseUnhealthy, etc.)

## Observability: Prometheus Metrics

Production debugging requires visibility into election state, lease health, and address management.

### Required Metrics

```go
// internal/election/metrics.go
package election

import "github.com/prometheus/client_golang/prometheus"

var (
    // Election state metrics
    electionWinnerChanges = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "purelb_election_winner_changes_total",
            Help: "Number of times the winner changed for a service IP",
        },
        []string{"service", "ip", "old_winner", "new_winner"},
    )

    electionCandidateCount = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "purelb_election_candidates",
            Help: "Number of candidate nodes for each subnet",
        },
        []string{"subnet"},
    )

    // Lease health metrics
    leaseRenewalFailures = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "purelb_lease_renewal_failures_total",
            Help: "Number of lease renewal failures",
        },
        []string{"node"},
    )

    leaseHealthy = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "purelb_lease_healthy",
            Help: "Whether this node's lease is healthy (1=healthy, 0=unhealthy)",
        },
        []string{"node"},
    )

    leaseLiveNodes = prometheus.NewGauge(
        prometheus.GaugeOpts{
            Name: "purelb_lease_live_nodes",
            Help: "Number of nodes with valid leases",
        },
    )

    // Address management metrics
    addressRenewalsActive = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "purelb_address_renewals_active",
            Help: "Number of active address renewal timers",
        },
        []string{"node"},
    )

    addressRenewalLatency = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "purelb_address_renewal_latency_seconds",
            Help:    "Time taken to renew an address",
            Buckets: []float64{0.001, 0.01, 0.1, 0.5, 1.0, 5.0},
        },
        []string{"node"},
    )

    addressWatchdogRecoveries = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "purelb_address_watchdog_recoveries_total",
            Help: "Number of times watchdog re-added a missing address",
        },
        []string{"node", "ip"},
    )

    // GARP metrics
    garpSent = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "purelb_garp_sent_total",
            Help: "Number of gratuitous ARP packets sent",
        },
        []string{"node", "interface", "ip"},
    )
)

func init() {
    prometheus.MustRegister(
        electionWinnerChanges,
        electionCandidateCount,
        leaseRenewalFailures,
        leaseHealthy,
        leaseLiveNodes,
        addressRenewalsActive,
        addressRenewalLatency,
        addressWatchdogRecoveries,
        garpSent,
    )
}
```

### Metric Usage Examples

```go
// When election winner changes
func (e *Election) rebuildMaps() {
    // ... build new state ...

    // Track winner changes
    for ip, oldWinner := range previousWinners {
        newWinner := e.computeWinner(ip, newState)
        if oldWinner != newWinner {
            electionWinnerChanges.WithLabelValues(svcName, ip, oldWinner, newWinner).Inc()
        }
    }

    // Update candidate counts per subnet
    for subnet, nodes := range newState.subnetToNodes {
        electionCandidateCount.WithLabelValues(subnet).Set(float64(len(nodes)))
    }

    leaseLiveNodes.Set(float64(len(newState.liveNodes)))
}

// When lease renewal fails
func (e *Election) renewLease() error {
    // ... renewal logic ...
    if err != nil {
        leaseRenewalFailures.WithLabelValues(e.config.NodeName).Inc()
        if !e.leaseHealthy.Load() {
            leaseHealthy.WithLabelValues(e.config.NodeName).Set(0)
        }
    } else {
        leaseHealthy.WithLabelValues(e.config.NodeName).Set(1)
    }
}

// When watchdog recovers an address
func (a *announcer) watchdogLoop() {
    // ... in recovery path ...
    addressWatchdogRecoveries.WithLabelValues(a.myNode, ipStr).Inc()
}
```

### Recommended Alerts

```yaml
# prometheus-alerts.yaml
groups:
- name: purelb
  rules:
  - alert: PureLBLeaseUnhealthy
    expr: purelb_lease_healthy == 0
    for: 30s
    labels:
      severity: warning
    annotations:
      summary: "PureLB node {{ $labels.node }} lease is unhealthy"

  - alert: PureLBLeaseRenewalFailures
    expr: rate(purelb_lease_renewal_failures_total[5m]) > 0.1
    for: 2m
    labels:
      severity: warning
    annotations:
      summary: "PureLB node {{ $labels.node }} experiencing lease renewal failures"

  - alert: PureLBWatchdogRecoveries
    expr: rate(purelb_address_watchdog_recoveries_total[5m]) > 0
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "PureLB watchdog recovering addresses on {{ $labels.node }}"

  - alert: PureLBNoLiveNodes
    expr: purelb_lease_live_nodes == 0
    for: 30s
    labels:
      severity: critical
    annotations:
      summary: "No PureLB nodes have valid leases"
```

## Effort Estimation

### Phase 1: Subnet Detection (Low-Medium Complexity)

| Work Item | Files | Complexity | Dependencies |
|-----------|-------|------------|--------------|
| `GetLocalSubnets()` function | `internal/election/subnets.go` (new) | Low | Reuses patterns from `network.go` |
| `SubnetsAnnotation()` / `ParseSubnetsAnnotation()` | Same file | Low | None |
| Netlink address subscription | Same file | Medium | `netlink.AddrSubscribe()` |
| Debouncing for interface flap | Same file | Low | Timer logic |
| Unit tests | `subnets_test.go` | Low | Standard table-driven tests |

**Existing code to leverage:** `internal/local/network.go:65-105` has `checkLocal()` with address listing and IPv6 flag filtering.

### Phase 2: Lease-Based Election (Medium-High Complexity)

| Work Item | Files | Complexity | Dependencies |
|-----------|-------|------------|--------------|
| `Election` struct with `atomic.Pointer[electionState]` | `internal/election/election.go` | Medium | Requires careful atomic design |
| `leaseHealthy` atomic flag for self-health | Same file | Low | Prevents split-brain |
| Lease informer setup | Same file | Medium | K8s client-go informers |
| `rebuildMaps()` with atomic swap | Same file | Medium | Copy-on-write pattern |
| `Winner()` with subnet filtering + health check | Same file | Medium | Must check `leaseHealthy` first |
| `IsHealthy()` and `MarkUnhealthy()` methods | Same file | Low | Simple atomic ops |
| `HasLocalCandidate()` helper | Same file | Low | Simple lookup |
| Lease creation with OwnerReference | Same file | Medium | K8s Lease API + Node lookup |
| Lease renewal with failure recovery | Same file | Medium | `OnRenewalFailure` callback |
| `DeleteOurLease()` for graceful shutdown | Same file | Low | Simple API call |
| Unit tests (mocked informer) | `election_test.go` | Medium | Fake client-go |

**This is the core architectural change** - replacing memberlist entirely with K8s Leases.

### Phase 3: Announcer Changes (Medium Complexity)

| Work Item | Files | Complexity | Dependencies |
|-----------|-------|------------|--------------|
| Handle `Winner() == ""` case | `internal/local/announcer_local.go` | Low | ~10 line change |
| Pool type annotation check | Same file | Low | `svc.Annotations[PoolTypeAnnotation]` |
| Remove kubelb0 fallback for local pools | Same file | Low | Conditional logic change |
| Address renewal race fix (`atomic.Bool`) | Same file | Low | Add cancelled flag |
| GARP config struct and multi-GARP | Same file + types.go | Medium | New `GARPConfig` type |
| Address watchdog with election check | Same file | Medium | Must verify winner before re-add |
| Graceful shutdown (`WithdrawAll`) | Same file | Medium | New method, signal handling |
| IPv6 NODAD flag support | `internal/local/network.go` | Low | Read from service annotation |

**Must preserve:** Existing `addNetworkWithOptions()` + `scheduleRenewal()` pattern.

### Phase 4: Main Entry Point (Low Complexity)

| Work Item | Files | Complexity | Dependencies |
|-----------|-------|------------|--------------|
| Remove memberlist setup | `cmd/lbnodeagent/main.go` | Low | Delete ~30 lines |
| Add lease config (env vars) | Same file | Low | 3 new env vars |
| Pass `GetLocalSubnets` callback | Same file | Low | Function reference |
| Call `election.Start()` | Same file | Low | Replace `election.Join()` |

### Phase 5: API v2 & Helm Changes (Medium-High Complexity)

| Work Item | Files | Complexity | Dependencies |
|-----------|-------|------------|--------------|
| **API v2 Package Setup** | | | |
| Create `pkg/apis/purelb/v2/` package | New directory | Low | Copy structure from v1 |
| `ServiceGroup` v2 type | `pkg/apis/purelb/v2/types.go` | Medium | Clean redesign |
| `ServiceGroupLocalSpec` with `skipIPv6DAD` | Same file | Low | New field |
| `ServiceGroupRemoteSpec` (new) | Same file | Low | New struct |
| `LBNodeAgent` v2 type | Same file | Medium | Field renames |
| `GARPConfig` struct | Same file | Low | New struct |
| Kubebuilder validation (exactly one of local/remote/netbox) | Same file | Medium | Validation annotations |
| `register.go`, `doc.go`, `groupversion_info.go` | `pkg/apis/purelb/v2/` | Low | Boilerplate |
| **Annotations** | | | |
| `PoolTypeAnnotation` constant | `pkg/apis/purelb/v2/annotations.go` | Low | One line |
| `SkipIPv6DADAnnotation` constant | Same file | Low | One line |
| **v1 Deprecation** | | | |
| Add deprecation warnings to v1 types | `pkg/apis/purelb/v1/types.go` | Low | Comments + markers |
| v1→v2 conversion functions | `pkg/apis/purelb/v1/conversion.go` (new) | Medium | Hub/spoke pattern |
| **Allocator Changes** | | | |
| Support both v1 and v2 ServiceGroups | `internal/allocator/` | Medium | Type switching |
| Set pool type + DAD annotations | `internal/allocator/service.go` | Low | ~15 lines |
| **Observability** | | | |
| Prometheus metrics package | `internal/election/metrics.go` (new) | Medium | ~100 lines |
| Metrics instrumentation in election | `internal/election/election.go` | Low | Metric calls |
| Metrics instrumentation in announcer | `internal/local/announcer_local.go` | Low | Metric calls |
| **Code Generation** | | | |
| `make generate` for v2 client | `pkg/generated/` | Low | Must verify output |
| `make crd` for v2 CRDs | `deployments/crds/` | Low | Both v1 and v2 |
| **Helm Changes** | | | |
| RBAC for leases | `build/helm/.../clusterrole-lbnodeagent.yaml` | Low | Add apiGroup |
| RBAC for nodes (get) | Same file | Low | For OwnerReference |
| Remove memberlist env vars from DaemonSet | `build/helm/.../daemonset.yaml` | Low | Delete lines |
| Add lease config to values.yaml | `build/helm/purelb/values.yaml` | Low | New section |
| Add terminationGracePeriodSeconds | `build/helm/.../daemonset.yaml` | Low | For graceful shutdown |
| **Migration Tooling** | | | |
| Migration script (`migrate-v1-to-v2.sh`) | `scripts/` (new) | Medium | jq transformations |
| Migration documentation | `docs/migration-v1-to-v2.md` (new) | Low | User guide |

### Phase 6: Testing & Cleanup (Medium-High Complexity)

| Work Item | Complexity | Notes |
|-----------|------------|-------|
| Unit tests (subnet, election, announcer) | Medium | Standard Go tests |
| Integration tests (multi-node simulation) | High | Requires test cluster or envtest |
| Race condition tests | High | Concurrent timer cancellation, etc. |
| CNI compatibility test (Flannel) | Medium | Verify IFA_F_PERMANENT not set |
| Rolling upgrade test | High | From memberlist version |
| API server unavailability test | Medium | Network partition simulation |
| Node failure/recovery test | Medium | Verify <15s failover |
| Remove memberlist from go.mod | Low | `go mod tidy` |

### Complexity Summary

| Phase | Primary Complexity | Risk Level |
|-------|-------------------|------------|
| 1. Subnet Detection | Low-Medium | Low |
| 2. Lease Election | **Medium-High** | **Medium** |
| 3. Announcer Changes | Medium | Low |
| 4. Main Entry | Low | Low |
| 5. API v2 & Helm | **Medium-High** | **Medium** |
| 6. Testing | **Medium-High** | **Medium** |

**Critical Path:**
- Phase 2 (election) must be solid before Phase 3 (announcer) can be completed
- Phase 5 (API v2) can be developed in parallel with Phases 1-4
- Phase 6 (testing) depends on everything
- v1 deprecation warnings should be added early to give users time to migrate

### Risk Factors

1. **K8s Lease API nuances** - Renewal timing, holder identity semantics
2. **Atomic pointer correctness** - Must ensure no stale reads during transitions
3. **Race condition mitigations** - The 5 identified races need careful implementation
4. **Backward compatibility** - Rolling upgrade from memberlist version
5. **Test coverage** - Integration tests require real or simulated multi-node environment
6. **API v2 migration** - Users must understand `local` vs `remote` behavioral difference
7. **Dual-version support** - v1 + v2 coexistence adds complexity during transition
