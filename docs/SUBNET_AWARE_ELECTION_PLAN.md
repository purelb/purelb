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
  namespace: purelb
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

```go
func (a *announcer) announceLocal(...) error {
    winner := a.election.Winner(lbIP.String())

    if winner == "" {
        // No node has this subnet - don't announce anywhere
        // (This replaces the kubelb0 fallback for local addresses)
        l.Log("msg", "noEligibleNodes", "ip", lbIP)
        return nil
    }

    if winner != a.myNode {
        return a.deleteAddress(nsName, "lostElection", lbIP)
    }

    // We won - announce on local interface
    addNetwork(lbIPNet, announceInt)
    // ...
}
```

### 5. Pool Type: Local vs Remote (Mutually Exclusive)

ServiceGroup now has mutually exclusive `local` and `remote` pool types:

**Local Pool** (Subnet-Aware Election):
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: local-pool
  namespace: purelb
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
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: bgp-pool
  namespace: purelb
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

New `interfaces` field allows explicit interface list for election participation:

```yaml
apiVersion: purelb.io/v1
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb
spec:
  local:
    # localint controls default route auto-detection:
    # - "default": auto-detect interface with default route (current behavior)
    # - "none": disable auto-detection
    localint: default

    # interfaces[] adds extra interfaces for election participation
    # Subnets from these interfaces are included in lease annotation
    # +optional
    interfaces:
    - eth1
    - bond0.100

    extlbint: kube-lb0
    sendgarp: false
```

**Behavior:**
- `localint: default` + no `interfaces`: Current behavior, auto-detect only
- `localint: default` + `interfaces: [eth1]`: Auto-detect + eth1 subnets
- `localint: none` + `interfaces: [eth0, eth1]`: Only listed interfaces, no auto-detect

**Lease annotation includes all configured interface subnets:**
```yaml
annotations:
  purelb.io/subnets: "192.168.1.0/24,192.168.2.0/24,10.0.0.0/24"
```

### 7. Subnet Change Detection

Watch netlink for real-time interface changes:
- Subscribe to `RTM_NEWADDR` and `RTM_DELADDR` events
- Filter events to configured interfaces only
- On change, update lease annotation with new subnets
- Trigger `ForceSync()` to re-evaluate services

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
  Node-A: not winner → deleteAddress()
  Node-B: not candidate → does nothing (no kubelb0)
  Node-C: winner → addNetwork() on eth0

Service-2 gets IP 192.168.2.50:
  Winner("192.168.2.50"):
    - IP in 192.168.2.0/24 → candidates = ["node-b"]
    - Hash election → winner = "node-b"
  Node-A: not candidate → does nothing
  Node-B: winner → addNetwork() on eth0
  Node-C: not candidate → does nothing
```

## Scaling Analysis

Same as before - scales with nodes only, not IPs:

| Scenario | Leases | API calls/s | Watch events/s |
|----------|--------|-------------|----------------|
| 30 nodes × 100 IPs | 30 | ~9 | ~129 |
| 100 nodes × 500 IPs | 100 | ~29 | ~1429 |

Subnet annotation updates only happen when node's interfaces change (rare).

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

type Election struct {
    config        Config
    mu            sync.RWMutex
    liveNodes     []string
    subnetToNodes map[string][]string
    nodeToSubnets map[string][]string
    leaseInformer cache.SharedIndexInformer
}

func (e *Election) Winner(key string) string      // Subnet-filtered election
func (e *Election) MemberCount() int              // For logging
func (e *Election) HasLocalCandidate(ip string) bool  // Check if any node can announce
func (e *Election) Start() error
func (e *Election) Shutdown()
```

**`internal/election/subnets.go`** - New file for subnet detection:

```go
func GetLocalSubnets() ([]string, error)  // Uses netlink to find interface subnets
func SubnetsAnnotation(subnets []string) string  // Format for annotation
func ParseSubnetsAnnotation(s string) []string   // Parse annotation
```

### Local Announcer

**`internal/local/announcer_local.go`**:

- Remove kubelb0 fallback logic for IPs where `Winner()` returns ""
- Only use kubelb0 for explicitly remote pools
- Change election check:
  ```go
  // Before (line 235):
  if winner := a.election.Winner(lbIP.String()); winner != a.myNode {

  // After:
  winner := a.election.Winner(lbIP.String())
  if winner == "" {
      // No eligible node has this subnet
      l.Log("msg", "noEligibleNodes", "ip", lbIP)
      return nil
  }
  if winner != a.myNode {
  ```

- Modify `SetBalancer()` to distinguish local-only pools from remote pools

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

    // Interfaces specifies additional interfaces for election participation.
    // Subnets from these interfaces are included in the lease annotation.
    // Combined with LocalInterface setting to determine full interface list.
    // +optional
    Interfaces []string `json:"interfaces,omitempty"`

    // ExtLBInterface specifies the interface for non-local (remote) announcements.
    // +kubebuilder:default="kube-lb0"
    // +optional
    ExtLBInterface string `json:"extlbint"`

    // SendGratuitousARP enables GARP messages when adding IPs.
    // +kubebuilder:default=false
    SendGratuitousARP bool `json:"sendgarp"`
}
```

After modifying types, run:
```bash
make generate  # Regenerate client code
make crd       # Regenerate CRD manifests
```

### Cleanup

- Remove memberlist from go.mod
- Update package.go docs

## Implementation Sequence

1. **Phase 1**: Subnet detection
   - Implement `GetLocalSubnets()` using netlink
   - Unit tests for subnet detection and annotation parsing

2. **Phase 2**: Lease-based election with subnet mapping
   - Create lease with subnet annotations
   - Watch leases and build subnet→nodes map
   - Implement filtered `Winner()`
   - Unit tests

3. **Phase 3**: Update announcer
   - Modify `announceLocal()` to handle `Winner() == ""`
   - Remove kubelb0 fallback for local pools
   - Keep remote pool behavior

4. **Phase 4**: Wire up in main.go
   - Remove memberlist
   - Add lease config
   - Pass subnet callback

5. **Phase 5**: Helm/RBAC updates

6. **Phase 6**: Cleanup and testing

## Decisions Made

1. **Subnet change detection**: Watch netlink for `RTM_NEWADDR`/`RTM_DELADDR` events for real-time updates

2. **Pool types**: Mutually exclusive `spec.local` and `spec.remote` in ServiceGroup CR
   - `local`: Subnet-filtered election, IP on real interface
   - `remote`: All nodes announce to kubelb0 (BGP/ECMP)
   - Enforced via kubebuilder validation

3. **Overlapping subnets**: If Node-A has 10.0.0.0/16 and Node-B has 10.0.1.0/24:
   - IP 10.0.1.50 matches both
   - Both nodes are candidates
   - Hash election decides winner (this is correct behavior)

4. **Multi-pool allocation**: Opt-in via `purelb.io/multi-pool: "true"` annotation on Service
   - Default: One IP per family (current behavior)
   - With annotation: One IP from each pool with matching subnet
   - Requires Allocator changes (not just lbnodeagent)

5. **Interface configuration**: `localint` controls auto-detect mode, `interfaces[]` adds extra interfaces
   - `localint: default` + `interfaces: []` = current behavior
   - `localint: none` + `interfaces: [eth0]` = explicit only

## Testing Checklist

- [ ] Unit tests for subnet detection
- [ ] Unit tests for subnet→nodes mapping
- [ ] Unit tests for filtered Winner()
- [ ] Integration: nodes with same subnet elect one winner
- [ ] Integration: nodes with different subnets don't interfere
- [ ] Integration: IP not announced if no node has subnet
- [ ] Integration: node failure triggers re-election among remaining candidates
- [ ] Manual: rolling upgrade from memberlist
