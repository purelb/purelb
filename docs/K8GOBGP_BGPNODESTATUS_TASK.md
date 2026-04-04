# Task: Add BGPNodeStatus CRD to k8gobgp

## Background

We are building a `kubectl purelb` krew plugin for PureLB (a Kubernetes LoadBalancer orchestrator). The plugin needs to display BGP state from every node in the cluster — neighbor sessions, netlink import/export pipeline, RIB contents, and route advertisements.

Rather than having the plugin `kubectl exec` into each k8gobgp pod (fragile text parsing, slow at scale, elevated RBAC), **k8gobgp should write its own status to a Kubernetes CRD** that the plugin simply reads.

## Why a per-node status CRD (industry precedent)

This is an established pattern in the Kubernetes networking ecosystem:

- **Calico** uses `CalicoNodeStatus` — per-node CRD reporting BGP peer sessions, routes, and agent state (projectcalico.org/v3)
- **Cilium** uses `CiliumNode` — per-node CRD for IPAM and networking state
- **Antrea** uses `AntreaAgentInfo` — per-node CRD for agent status
- **MetalLB/FRR-k8s** uses `BGPSessionState` — per-session CRD (one per peer per node), more granular but requires multiple CRD types

We chose the per-node CRD pattern (like Calico) rather than per-session (like MetalLB) because k8gobgp needs to expose more than just session state — it needs sessions + netlinkImport pipeline + RIB + netlinkExport pipeline + VRFs. A per-session approach would require 3-4 separate CRD types. One per-node CRD keeps it simple.

The alternative of pod exec was rejected because:
- **Fragile**: parsing unversioned CLI text output from `gobgp` and `ip` breaks on version bumps
- **Slow**: N sequential HTTP upgrade connections, 200-500ms each, 50 nodes = 10-25 seconds
- **Security**: requires elevated RBAC (pods/exec), triggers audit log noise
- **Platform-dependent**: the plugin must know how to parse Linux CLI output

## What is k8gobgp

k8gobgp is a BGP sidecar that runs alongside PureLB's `lbnodeagent` DaemonSet. It:
- Runs as a container in each lbnodeagent pod (one per node)
- Reads a `BGPConfiguration` CR (`bgp.purelb.io/v1`, resource name `configs`) for its config
- Runs an embedded GoBGP instance (gRPC API on unix socket `/var/run/gobgp/gobgp.sock`)
- Uses **netlinkImport** to watch interfaces (typically `kube-lb0`) and import connected routes into BGP
- Advertises imported routes to configured BGP neighbors
- Optionally uses **netlinkExport** to install received BGP routes into the Linux kernel routing table
- Supports VRFs with per-VRF netlink import/export
- Neighbors can have `nodeSelector` fields so different nodes peer with different neighbors
- Exposes Prometheus metrics on port 7473, health on port 7474

## What you need to build

### 1. BGPNodeStatus CRD

Add a new CRD to the `bgp.purelb.io/v1` API group. One instance per node, written by k8gobgp, read by the kubectl plugin.

**Key properties:**
- Name: same as the node name (e.g., `node-a`)
- Namespace: `purelb-system`
- Status-only resource (spec is empty)
- OwnerReference to the Node object (enables garbage collection when nodes are removed)

Here is the complete status schema:

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPNodeStatus
metadata:
  name: node-a
  namespace: purelb-system
  ownerReferences:
  - apiVersion: v1
    kind: Node
    name: node-a
    uid: <node-uid>
    blockOwnerDeletion: false
    controller: false
spec: {}
status:
  # Identity
  nodeName: node-a
  routerID: "192.168.1.10"
  routerIDSource: "node-ipv4"       # explicit | template | node-ipv4 | hash-from-node-name
  lastUpdated: "2026-04-03T10:30:00Z"

  # Neighbor sessions
  neighbors:
  - address: "192.168.1.1"
    peerASN: 65001
    localASN: 65000
    state: "Established"            # Idle | Connect | Active | OpenSent | OpenConfirm | Established
    uptime: "2d4h"
    prefixesSent: 12
    prefixesReceived: 0
    description: "Upstream router"

  # Netlink Import state
  netlinkImport:
    enabled: true
    interfaces:
    - name: "kube-lb0"
      exists: true
      operState: "up"               # up | down | unknown
    importedAddresses:
    - address: "10.100.0.1/32"
      interface: "kube-lb0"
      inRIB: true
    - address: "10.100.0.9/32"
      interface: "kube-lb0"
      inRIB: false                  # import failure

  # BGP RIB summary
  rib:
    localRoutes:                    # routes originating from this node
    - prefix: "10.100.0.1/32"
      nextHop: "0.0.0.0"
      advertisedTo:
      - "192.168.1.1"
    receivedRoutes:                 # routes received from peers
    - prefix: "10.200.0.0/24"
      nextHop: "10.100.0.1"
      fromPeer: "10.100.0.1"
      communities: ["65001:100"]

  # Netlink Export state
  netlinkExport:
    enabled: true
    protocol: 186
    rules:
    - name: "default"
      metric: 20
      tableID: 0
    exportedRoutes:
    - prefix: "10.200.0.0/24"
      table: "main"
      metric: 20
      installed: true
    - prefix: "10.200.2.0/24"
      table: "main"
      metric: 20
      installed: false
      reason: "nexthop unreachable"

  # VRF summary
  vrfs:
  - name: "customer-a"
    rd: "65000:100"
    importedRouteCount: 3
    exportedRouteCount: 2

  # Overall health
  healthy: true
  conditions:
  - type: "Ready"
    status: "True"
    reason: "AllNeighborsEstablished"
    message: "1 neighbor established, netlinkImport active"
    lastTransitionTime: "2026-04-03T08:00:00Z"
```

### 2. Write strategy: write-on-change with opt-out

**Default behavior: ON.** k8gobgp writes BGPNodeStatus automatically. No user action needed.

**Write-on-change**: Only write to the API server when state actually changes:
- Neighbor session state change (Established -> Active, etc.)
- Neighbor added/removed
- Route added/removed from RIB
- Address added/removed from kube-lb0
- netlinkExport route installed/failed
- Interface state change (up/down)

**Periodic heartbeat**: Full sync every 60 seconds even if nothing changed. Updates `lastUpdated` to confirm the agent is alive. The plugin can use `lastUpdated` age to detect stale/dead agents.

**Configuration via BGPConfiguration CR**: Add a `nodeStatus` section to `spec.global`:

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: default
  namespace: purelb-system
spec:
  global:
    asn: 65000
    # ... existing fields ...

    # Node status reporting (NEW)
    nodeStatus:
      enabled: true              # default: true. Set false to disable BGPNodeStatus writes entirely.
      heartbeatSeconds: 60       # default: 60. Periodic full sync interval even when state is unchanged.
                                 # Minimum: 10. The plugin uses lastUpdated age to detect stale agents.
```

When `nodeStatus.enabled` is false (or the entire `nodeStatus` block is absent and the default is true), no BGPNodeStatus is created. The plugin detects this and shows: "BGP status reporting disabled (set spec.global.nodeStatus.enabled=true in BGPConfiguration)."

The heartbeat interval is configurable so operators can tune for their environment:
- **60s** (default): good for most clusters, minimal API server load
- **10s**: near-real-time status, higher load — useful during active debugging
- **300s**: very low overhead for large clusters where BGP status is rarely checked

**Load analysis (write-on-change)**:
- Steady state (BGP established, routes stable): **1 write per node per heartbeatSeconds** (heartbeat only)
- 20-node cluster at default 60s: 0.33 writes/sec — negligible
- During changes (neighbor flap, route added/removed): writes happen immediately regardless of heartbeat interval
- Object size: ~3KB (small cluster) to ~30KB (100 VIPs, 50 received routes)

**Implementation**: After each reconcile cycle, compare the new status snapshot against the previous one. If identical (deep equal, ignoring `lastUpdated`), skip the API call unless `heartbeatSeconds` have elapsed since the last write.

**Data collection for each reconcile:**

| Status field | Data source |
|---|---|
| `routerID`, `routerIDSource` | Already known from BGPConfiguration reconciliation |
| `neighbors[].state/uptime/prefixes` | GoBGP gRPC API: `ListPeer()` |
| `netlinkImport.interfaces[].exists/operState` | netlink: `LinkByName()` + `link.Attrs().OperState` |
| `netlinkImport.importedAddresses` | netlink: `AddrList()` on watched interfaces |
| `netlinkImport.importedAddresses[].inRIB` | GoBGP gRPC API: `ListPath()` — check if address exists as prefix in global RIB |
| `rib.localRoutes` | GoBGP gRPC API: `ListPath()` for locally originated routes |
| `rib.localRoutes[].advertisedTo` | GoBGP gRPC API: `ListPath()` with neighbor filter or `ListPeer()` adj-out |
| `rib.receivedRoutes` | GoBGP gRPC API: `ListPath()` for received routes |
| `netlinkExport.exportedRoutes` | netlink: `RouteList()` filtered by protocol (`RTPROT_BGP = 186`) |
| `netlinkExport.exportedRoutes[].installed` | Compare received routes against kernel routes |
| `vrfs` | GoBGP gRPC API: list VRF routes, count per VRF |
| `healthy` | true if all neighbors are Established and no import/export failures |
| `conditions` | Standard K8s conditions reflecting overall state |

### 3. Lifecycle

- **Startup**: Create BGPNodeStatus with OwnerReference to the Node. If it already exists (pod restart), update it.
- **Reconcile**: Collect state every reconcile cycle. Write to API server only if state changed or 60s heartbeat elapsed.
- **Shutdown**: Mark `healthy=false` with condition `Ready=False/AgentShutdown` (preserves diagnostic state — plugin can distinguish "shut down" from "disappeared"). OwnerReference to Node handles eventual GC.
- **Opt-out**: If `spec.global.nodeStatus.enabled` is false in BGPConfiguration, skip creation entirely.

### 4. RBAC changes

The existing ClusterRole `purelb:k8gobgp` in `gobgp-rbac.yaml` currently has:

```yaml
rules:
- apiGroups: [bgp.purelb.io]
  resources: [configs, configs/finalizers, configs/status]
  verbs: [create, delete, get, list, patch, update, watch]
- apiGroups: ['']
  resources: [nodes]
  verbs: [get, list, watch]
# ... plus pods, secrets, events
```

Add a new rule:

```yaml
- apiGroups: [bgp.purelb.io]
  resources: [bgpnodestatuses, bgpnodestatuses/status]
  verbs: [create, delete, get, list, patch, update, watch]
```

### 5. CRD manifest

Add a new file alongside the existing `gobgp-bgpconfig-crd.yaml`. Use controller-gen or write manually. The CRD should be added to the kustomization.yaml resources list.

Key CRD properties:
- Group: `bgp.purelb.io`
- Version: `v1`
- Kind: `BGPNodeStatus`
- Plural: `bgpnodestatuses`
- ShortNames: `bgpns`
- Scope: `Namespaced`
- Subresources: `status: {}`

### 6. Update kustomization.yaml

Add the new CRD manifest to `deployments/components/gobgp/kustomization.yaml`:

```yaml
resources:
- gobgp-bgpconfig-crd.yaml
- gobgp-bgpnodestatus-crd.yaml    # NEW
- gobgp-rbac.yaml
```

## Existing k8gobgp architecture reference

### Deployment
- Runs as sidecar container `k8gobgp` in the `lbnodeagent` DaemonSet
- Image: `ghcr.io/purelb/k8gobgp:0.2.2`
- Shares `emptyDir` volume at `/var/run/gobgp` with lbnodeagent (GoBGP gRPC socket)
- Environment variables: `NODE_NAME`, `POD_NAME`, `POD_NAMESPACE` (from downward API)
- Ports: 7473 (metrics), 7474 (health)
- Uses `lbnodeagent` ServiceAccount in `purelb-system` namespace

### BGPConfiguration CRD (existing)
- API: `bgp.purelb.io/v1`, kind `BGPConfiguration`, plural `configs`, shortname `bgpconfig`
- Typically one CR named `default` in `purelb-system`
- Contains: `spec.global` (ASN, routerID, families), `spec.neighbors` (with `nodeSelector`), `spec.netlinkImport`, `spec.netlinkExport`, `spec.vrfs`, `spec.peerGroups`, `spec.policyDefinitions`
- Status already has: `establishedNeighbors`, `neighborCount`, `conditions`, `routerIDSource`, `routerIDResolutionTime`, `observedGeneration`, `lastReconcileTime`, `message`

### Why BGPNodeStatus is a separate CRD (not on BGPConfiguration)
- BGPConfiguration is cluster-wide (one CR), but BGP state is per-node
- Each k8gobgp instance needs to write its own status independently
- Multiple writers to the same CR's status would cause conflicts
- Separate CRD scales cleanly: 1 BGPNodeStatus per node, no contention

## Design constraints

- **No locks/mutexes**: PureLB project avoids locks. Use atomic operations, channels, or single-goroutine ownership for concurrent access.
- **Leveled logging**: Info for operations, Debug for code-level troubleshooting.
- **IPv6 parity**: All features must work equally for IPv4 and IPv6.
- **The status update should not block the main reconcile loop.** If the K8s API call to update BGPNodeStatus fails, log the error and continue — don't crash or retry aggressively. The next reconcile cycle will try again.

## Acceptance criteria

1. BGPNodeStatus CRD is defined and deployed alongside BGPConfiguration CRD
2. Each k8gobgp instance creates a BGPNodeStatus for its node on startup (when enabled)
3. Status is written on state change; periodic heartbeat writes `lastUpdated` even when state is stable
4. BGPNodeStatus is deleted on graceful shutdown
5. OwnerReference to Node ensures GC on node removal
6. RBAC allows k8gobgp to manage BGPNodeStatus resources
7. The status accurately reflects: neighbor sessions, netlinkImport pipeline (interface exists + addresses + RIB membership), RIB routes with advertisement targets, netlinkExport pipeline (received routes + kernel installation), VRF summary
8. `healthy` field is false when any neighbor is not Established or any import/export failure exists
9. `conditions` follow standard K8s condition patterns
10. `kubectl get bgpns -n purelb-system` shows the list of per-node statuses
11. `spec.global.nodeStatus.enabled` in BGPConfiguration controls opt-out (default: true)
12. `spec.global.nodeStatus.heartbeatSeconds` in BGPConfiguration controls heartbeat interval (default: 60, minimum: 10)
13. When `nodeStatus.enabled` is false, no BGPNodeStatus is created and no API server writes occur
14. BGPConfiguration CRD schema is updated with the new `nodeStatus` fields
