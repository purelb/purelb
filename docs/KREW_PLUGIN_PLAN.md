# Plan: kubectl-purelb Krew Plugin

## Context

PureLB operators currently need multiple `kubectl` commands and manual correlation to answer basic operational questions like "how full are my pools?", "which node announces which IP?", or "are all my BGP sessions healthy across all nodes?". A krew plugin consolidates these multi-step workflows into single commands.

The plugin binary is `kubectl-purelb`, invoked as `kubectl purelb <command>`.

**Key architectural decision**: BGP state is exposed via a new `BGPNodeStatus` CRD written by each k8gobgp instance, not via pod exec. This eliminates fragile text parsing, scales to any cluster size, requires only read RBAC, and works cross-platform.

---

## Commands (9 commands, bgp has 2 subcommands)

### 1. `kubectl purelb status`
**Single-pane cluster health overview — the "2am debugging starting point".**

Aggregates health from all other subsystems into one view. This is the first command an operator runs.

```
PureLB Cluster Status
=====================
Components:  Allocator 1/1 Running | LBNodeAgents 3/3 Running | k8gobgp 3/3 Running
Pools:       12/27 IPv4 (44%) | 2/16 IPv6 (12%) | no pools exhausted
Election:    3/3 nodes healthy | 3 subnets covered
BGP:         5/5 sessions established | netlinkImport OK | 0 export failures
Services:    17 services, 19 IPs | 1 problem
Validation:  1 WARN (bgp-pool range 10.100.1.0/28 uncovered)

Overall: WARNING (1 uncovered pool range -- run: kubectl purelb validate)
```

Data sources: Pods, ServiceGroups, Services, Leases, BGPNodeStatus CRs, BGPConfiguration CR

Flags: `-o json|yaml`

---

### 2. `kubectl purelb pools`
**Show all ServiceGroups with pool capacity, utilization, and per-range breakdown.**

Manual alternative: `kubectl get sg -o yaml` -> parse ranges -> `kubectl get svc -A` -> count per-pool -> calculate utilization.

Data sources: ServiceGroups, Services (filtered by `purelb.io/allocated-by`), Leases (for active-node subnet mapping)

```
SERVICEGROUP  TYPE    POOL RANGE                   SUBNET            FAMILY  SIZE  USED  AVAIL  UTIL%
default       local   192.168.1.240-192.168.1.250  192.168.1.0/24    IPv4      11     4      7    36%
default       local   fd00::f0-fd00::ff            fd00::/64         IPv6      16     2     14    12%
bgp-pool      remote  10.100.0.0/28                10.100.0.0/24     IPv4      16     8      8    50%
external      netbox  (managed by netbox)          netbox.example    -         -      5      -     -

Totals: 3 ServiceGroups, 3 local ranges, 12/27 IPv4 used (44%), 2/16 IPv6 used (12%)
       1 Netbox pool: 5 allocated (capacity managed externally)
```

Flags: `--service-group <name>`, `--show-services`, `-o json|yaml`

---

### 3. `kubectl purelb services`
**Show every PureLB-managed service with its IPs, announcing node, pool source, and status. Groups shared IPs together.**

Data sources: Services, Leases (for announcer health check)

```
NAMESPACE  SERVICE       IPS              POOL      TYPE    ANNOUNCING       SHARING      STATUS
default    web-front     192.168.1.240    default   local   node-a/eth0      -            OK
default    web-front     fd00::f0         default   local   node-a/eth0      -            OK
default    api-gw        192.168.1.241    default   local   node-b/eth0      -            OK
default    svc-a         192.168.1.242    default   local   node-b/eth0      group-1      OK
default    svc-b         192.168.1.242    default   local   node-b/eth0      group-1      OK
prod       broken-svc    192.168.1.243    default   local   -                -            NO ANNOUNCER
prod       pending-svc   <pending>        -         -       -                -            PENDING

Shared IPs:
  192.168.1.242 (sharing key: group-1): svc-a (TCP/80), svc-b (TCP/443)
```

Flags: `--pool <name>`, `--node <name>`, `--ip <addr>`, `--problems`, `-o json|yaml`

---

### 4. `kubectl purelb election`
**Show election state: node leases, subnets, health, subnet coverage, and drain simulation.**

Data sources: Leases (`purelb-node-*`), Services (announcement counts), ServiceGroups (subnet coverage)

```
NODE    SUBNETS                       RENEWED   EXPIRES  HEALTHY  ANNOUNCING
node-a  192.168.1.0/24,fd00::/64      5s ago    in 5s    Yes      3 IPs
node-b  192.168.1.0/24                3s ago    in 7s    Yes      2 IPs
node-c  192.168.1.0/24,10.100.0.0/24  4s ago    in 6s    Yes      2 IPs

Subnet Coverage:
  192.168.1.0/24 -> node-a, node-b, node-c  (covers: default v4pool)
  fd00::/64      -> node-a                   (covers: default v6pool)
  10.100.0.0/24  -> node-c                   (covers: bgp-pool v4pool)

Uncovered Pool Ranges:
  bgp-pool: 10.100.1.0/28 (subnet 10.100.1.0/24) - NO NODES WITH THIS SUBNET
```

**Drain simulation** (`--simulate-drain <node>`):

```
$ kubectl purelb election --simulate-drain node-a

SERVICE              IP               CURRENT    NEW WINNER   SUBNET
default/web-front    192.168.1.240    node-a  -> node-c       192.168.1.0/24
default/web-front    fd00::f0         node-a  -> (NONE)       fd00::/64  *** NO CANDIDATES ***
kube-system/dns      192.168.1.245    node-a  -> node-b       192.168.1.0/24

WARNING: 1 IP will have NO announcer after drain (fd00::f0 -- node-a is only node on fd00::/64)
3 services will move: 2 to node-c, 1 to node-b
```

Flags: `--node <name>`, `--check` (problems only), `--simulate-drain <node>`, `-o json|yaml`

---

### 5. `kubectl purelb bgp` (command group)

Running `kubectl purelb bgp` with no subcommand shows a combined summary.

All BGP data comes from the `BGPNodeStatus` CRDs (one per node, written by k8gobgp) and the `BGPConfiguration` CR. **No pod exec required.**

---

#### 5a. `kubectl purelb bgp sessions`
**Show live BGP neighbor session state across all nodes, highlighting inconsistencies.**

```
BGP Global: ASN 65000, Router ID: auto-detect

NODE    ROUTER ID      NEIGHBOR       PEER ASN  STATE        UPTIME  PREFIXES  SELECTOR
node-a  192.168.1.10   192.168.1.1    65001     Established  2d4h    12/0      zone=us-east-1a
node-b  192.168.1.11   192.168.1.1    65001     Active       -        0/0      zone=us-east-1a  *** DOWN ***
node-c  10.100.0.10    10.100.0.1     65002     Established  1d2h     8/0      zone=us-east-1b

Problems:
  node-b <-> 192.168.1.1 (AS 65001): not established (state: Active)
```

Data sources: BGPNodeStatus CRs, BGPConfiguration CR, Nodes (for nodeSelector evaluation)

Flags: `--node <name>`, `--check` (problems only), `-o json|yaml`

---

#### 5b. `kubectl purelb bgp dataplane`
**Show the full route data plane: kube-lb0 -> netlinkImport -> BGP RIB -> advertisements -> netlinkExport -> kernel.**

Answers: "My VIP isn't reachable from outside the cluster — where in the chain did it break?"

```
$ kubectl purelb bgp dataplane

=== Netlink Import ===
Config: enabled=true, interfaces=[kube-lb0]
LBNodeAgent dummy interface: kube-lb0 (matches import config)

NODE     INTERFACE   STATUS   ADDRESSES IMPORTED       IN BGP RIB?
node-a   kube-lb0    Up       10.100.0.1/32            Yes
node-a   kube-lb0    Up       10.100.0.2/32            Yes
node-a   kube-lb0    Up       10.100.0.3/32            Yes
node-c   kube-lb0    Up       10.100.0.5/32            Yes
node-c   kube-lb0    Up       10.100.0.6/32            Yes
node-c   kube-lb0    Up       10.100.0.9/32            NO  *** IMPORT FAILURE ***

=== BGP Advertisements ===
NODE     ROUTE              ADVERTISED TO              SERVICE
node-a   10.100.0.1/32      192.168.1.1 (AS 65001)     kube-system/dns-ext
node-a   10.100.0.2/32      192.168.1.1 (AS 65001)     default/api-gw
node-a   10.100.0.3/32      192.168.1.1 (AS 65001)     default/cache
node-c   10.100.0.5/32      10.100.0.1 (AS 65002)      prod/api
node-c   10.100.0.6/32      10.100.0.1 (AS 65002)      prod/web

=== Netlink Export ===
Config: enabled=true, protocol=186, dampening=100ms
Rules: "default" (metric=20, table=main, validateNexthop=true)

NODE     RECEIVED ROUTE     FROM PEER          IN KERNEL?  TABLE  METRIC
node-c   10.200.0.0/24      10.100.0.1 (65002) Yes         main   20
node-c   10.200.1.0/24      10.100.0.1 (65002) Yes         main   20
node-c   10.200.2.0/24      10.100.0.1 (65002) NO          -      -     *** EXPORT FAILURE ***

=== Cross-reference (remote pool services) ===
  10.100.0.1  kube-system/dns-ext    advertised by node-a  OK
  10.100.0.5  prod/api               advertised by node-c  OK
  10.100.0.7  prod/payments          NOT ON kube-lb0       *** MISSING ***

=== VRFs ===
  customer-a: RD 65000:100, 3 imported routes, 2 exported routes
  customer-b: RD 65000:200, 1 imported route, 0 exported routes

=== Problems ===
  node-c: 10.100.0.9/32 on kube-lb0 but not in BGP RIB (import failure)
  node-c: 10.200.2.0/24 received but not in kernel (export failure)
  prod/payments: 10.100.0.7 not on any node's kube-lb0
```

Data sources: BGPNodeStatus CRs (contains all netlink/RIB/advertisement data), BGPConfiguration CR, Services (remote pool cross-reference), LBNodeAgent (dummyInterface config)

Flags: `--node <name>`, `--check` (problems only), `--vrf <name>`, `--import-only`, `--export-only`, `-o json|yaml`

---

### 6. `kubectl purelb inspect <namespace/service>`
**Deep-dive into a single service: allocation chain, election, endpoints, sharing, and pending diagnostics.**

**For allocated services:**
```
Service: default/web-frontend (LoadBalancer, DualStack, ETP=Cluster)

Allocation:
  IPv4: 192.168.1.240 from default (local), range 192.168.1.240-250, subnet 192.168.1.0/24
  IPv6: fd00::f0 from default (local), range fd00::f0-ff, subnet fd00::/64
  Sharing: none

Election (192.168.1.240):
  Candidates: node-a, node-b, node-c (subnet 192.168.1.0/24)
  Winner: node-a (SHA256 hash)  [MATCHES ANNOUNCER]

Announcement:
  IPv4: node-a / eth0   (lease healthy, renewed 3s ago)
  IPv6: node-a / eth0   (lease healthy, renewed 3s ago)

Endpoints: 3 ready (node-a, node-b, node-c)

BGP (remote pool only):
  Route 10.100.0.5/32 in RIB on node-c: Yes
  Advertised to 10.100.0.1 (AS 65002): Yes
```

**For shared IP services:**
```
Sharing: key "group-1", shared with:
  default/svc-b using ports TCP/443
  This service uses ports TCP/80
```

**For PENDING services (diagnostic mode):**
```
Service: prod/stuck-svc (LoadBalancer, SingleStack IPv4, ETP=Cluster)
Status: PENDING - no IP allocated

Diagnosis:
  Requested pool: "bgp-pool" (from annotation purelb.io/service-group)
  Pool exists: Yes
  Pool type: remote
  Pool capacity: 16 total, 16 used, 0 available
  >>> POOL EXHAUSTED - no addresses available in bgp-pool <<<

  Requested IP: none (automatic allocation)
  Sharing key: none
```

Other pending diagnoses: ServiceGroup missing, IP outside range, IP already taken, sharing key conflict, port conflict on shared IP, allocator pod not running.

Flags: `-o json|yaml`

---

### 7. `kubectl purelb validate`
**Check ServiceGroup + LBNodeAgent + BGPConfiguration consistency — pure config analysis, usable in CI/CD.**

```
$ kubectl purelb validate

Checking 3 ServiceGroups, 1 LBNodeAgent, 1 BGPConfiguration...

PASS  ServiceGroup "default": pool ranges valid, subnets covered by 3 nodes
PASS  ServiceGroup "bgp-pool": pool ranges valid, remote type
WARN  ServiceGroup "bgp-pool": range 10.100.1.0/28 (subnet 10.100.1.0/24) not covered by any node
PASS  ServiceGroup "external": Netbox config valid (url reachable not checked)
PASS  LBNodeAgent "default": dummyInterface "kube-lb0" matches BGP netlinkImport
PASS  BGPConfiguration: netlinkImport enabled, watching [kube-lb0]
FAIL  BGPConfiguration: neighbor 10.99.0.1 (AS 65099) nodeSelector matches 0 nodes
PASS  No overlapping pool ranges detected

Result: 1 FAIL, 1 WARN, 6 PASS
```

Checks: overlapping pools, range not in subnet, multiPool+balancePools conflict, uncovered subnets, missing LBNodeAgent, netlinkImport disabled/mismatched, unreachable neighbor addresses, zero-match nodeSelectors, remote pools without BGPConfiguration.

Flags: `-o json|yaml`, `--strict` (warnings are failures, for CI/CD)

---

### 8. `kubectl purelb version`
**Show plugin version and all PureLB component versions.**

```
Plugin:      v0.16.0 (commit abc1234)
Allocator:   ghcr.io/purelb/purelb/allocator:v0.16.0 (1 pod, Running)
LBNodeAgent: ghcr.io/purelb/purelb/lbnodeagent:v0.16.0 (3 pods, all Running)
k8gobgp:     ghcr.io/purelb/k8gobgp:0.2.2 (3 sidecars, all Running)
CRDs:        purelb.io/v2 (ServiceGroup, LBNodeAgent), bgp.purelb.io/v1 (BGPConfiguration, BGPNodeStatus)

Version Consistency: OK (all components at v0.16.0)
```

Flags: `-o json|yaml`

---

## k8gobgp Changes: BGPNodeStatus CRD

### Why

Pod exec is an anti-pattern for status reporting:
- **Fragile**: parsing unversioned CLI text output from `gobgp` and `ip` commands breaks on version bumps
- **Slow**: N sequential HTTP upgrade connections, 200-500ms each, 50 nodes = 10-25 seconds
- **Security**: requires elevated RBAC (pods/exec), triggers audit log noise
- **Platform-dependent**: the plugin must know how to parse Linux CLI output

Instead, k8gobgp should write its state to a `BGPNodeStatus` CRD. The plugin reads standard K8s objects. Fast, stable, secure.

### BGPNodeStatus CRD Design

Implemented in k8gobgp branch `feat/bgpnodestatus`. Cluster-scoped resource (shortname: `bgpns`).

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPNodeStatus
metadata:
  name: node-a                    # one per node, same name as node
  ownerReferences:                # owned by the Node for GC
  - apiVersion: v1
    kind: Node
    name: node-a
spec: {}                          # no spec, status-only resource
status:
  # Identity
  nodeName: node-a
  routerID: "192.168.1.10"
  routerIDSource: "node-ipv4"     # explicit | template | node-ipv4 | hash-from-node-name
  asn: 65000
  lastUpdated: "2026-04-03T10:30:00Z"
  heartbeatSeconds: 60            # echoed from config so plugin can detect staleness

  # Neighbor sessions
  neighbors:
  - address: "192.168.1.1"
    peerASN: 65001
    localASN: 65000
    state: "Established"          # Idle | Connect | Active | OpenSent | OpenConfirm | Established
    sessionUpSince: "2026-04-01T06:30:00Z"
    prefixesSent: 12
    prefixesReceived: 0
    description: "Upstream router"
    lastError: ""                 # populated for non-Established neighbors
  neighborCount: 1

  # Netlink Import state
  netlinkImport:
    enabled: true
    interfaces:
    - name: "kube-lb0"
      exists: true
      operState: "up"             # up | down
    importedAddresses:            # max 500, truncated flag set if capped
    - address: "10.100.0.1/32"
      interface: "kube-lb0"
      inRIB: true
    - address: "10.100.0.9/32"
      interface: "kube-lb0"
      inRIB: false                # import failure
    totalImported: 2
    truncated: false

  # BGP RIB summary
  rib:
    localRoutes:                  # max 500, routes originating from this node
    - prefix: "10.100.0.1/32"
      nextHop: "0.0.0.0"
      advertisedTo:               # which peers this route is advertised to
      - "192.168.1.1"
    localRouteCount: 1
    receivedRoutes:               # max 100, routes received from peers
    - prefix: "10.200.0.0/24"
      nextHop: "10.100.0.1"
      fromPeer: "10.100.0.1"
      communities: ["65001:100"]
    receivedRouteCount: 1
    truncated: false

  # Netlink Export state
  netlinkExport:
    enabled: true
    protocol: 186
    rules:
    - name: "default"
      metric: 20
      tableID: 0
    exportedRoutes:               # max 500, routes installed in kernel
    - prefix: "10.200.0.0/24"
      table: "main"
      metric: 20
      installed: true
    - prefix: "10.200.2.0/24"
      table: "main"
      metric: 20
      installed: false
      reason: "nexthop unreachable"
    totalExported: 2
    truncated: false

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
    reason: "AllHealthy"
    message: "1 neighbor(s) established, netlinkImport active"
    lastTransitionTime: "2026-04-03T08:00:00Z"
```

### k8gobgp Implementation Changes

1. **New CRD definition**: Add `BGPNodeStatus` to the existing `bgp.purelb.io/v1` API group. Status-only resource (empty spec). CRD manifest goes in `deployments/components/gobgp/`.

2. **Write strategy: write-on-change, configurable via BGPConfiguration CR**:
   - **Default: ON.** k8gobgp writes BGPNodeStatus automatically, no user action needed.
   - **Write-on-change**: only write to API server when state changes (neighbor up/down, route added/removed, interface state change). Deep-compare snapshot against previous; skip write if identical.
   - **Configurable heartbeat**: periodic full sync updates `lastUpdated` to confirm agent is alive. Configured via `spec.global.nodeStatus.heartbeatSeconds` in BGPConfiguration CR (default: 60, minimum: 10).
   - **Opt-out via CR**: set `spec.global.nodeStatus.enabled: false` in BGPConfiguration to disable status writing entirely.
   - Steady-state load at default 60s: 1 write per node per 60 seconds. 20-node cluster = 0.33 writes/sec.

3. **Lifecycle**: Create BGPNodeStatus on startup. On shutdown, mark `healthy=false` with condition `Ready=False/AgentShutdown` (preserves diagnostic state — the plugin can distinguish "node shut down" from "node disappeared"). OwnerReference to the Node ensures eventual GC when the node is removed. If `nodeStatus.enabled` is false, skip creation entirely.

4. **RBAC**: k8gobgp already has RBAC in `deployments/components/gobgp/gobgp-rbac.yaml`. Add `get`, `create`, `update`, `delete` on `bgpnodestatuses` in the `bgp.purelb.io` API group.

5. **Backward compatibility**: The plugin handles two cases:
   - BGPNodeStatus CRD not installed (older k8gobgp): "BGPNodeStatus not found — upgrade k8gobgp to 0.3.0+ for BGP visibility."
   - CRD installed but no instances (status opted out): "BGP status reporting disabled (set spec.global.nodeStatus.enabled=true in BGPConfiguration)."

### What this eliminates

- Zero pod exec calls in the plugin
- Zero text parsing of CLI output
- Zero Linux-specific dependencies in the plugin
- Plugin RBAC is read-only for all commands
- BGP queries are a single K8s List call regardless of cluster size
- Status is always fresh (updated every reconcile cycle, not on-demand)

---

## Architecture

### Project Layout
```
cmd/kubectl-purelb/
  main.go                  # Cobra root command, kubeconfig setup via cli-runtime
  status.go                # status command (aggregation)
  pools.go                 # pools command
  services.go              # services command
  election.go              # election command
  bgp.go                   # bgp command group (root + sessions subcommand)
  bgp_dataplane.go         # bgp dataplane subcommand
  inspect.go               # inspect command
  validate.go              # validate command
  version.go               # version command
  client.go                # K8s client builder (core, coordination, dynamic, PureLB + BGP generated)
  output.go                # Table printer + JSON/YAML output
  util.go                  # Shared helpers (annotation parsing, IP range sizing)
```

All in one package (`package main`). `bgp` uses Cobra subcommands: `bgp sessions`, `bgp dataplane`. Running `bgp` alone shows a combined summary.

### Key Design Decisions

1. **CLI framework**: Use Cobra. Standard for kubectl plugins, integrates with `k8s.io/cli-runtime/pkg/genericclioptions` for `--kubeconfig`, `--context`, `--namespace` flags.

2. **All data via K8s API — no pod exec**: BGP state comes from BGPNodeStatus CRDs. Election state from Leases. Pool state from ServiceGroups + Services. This makes the plugin a pure K8s API consumer — fast, secure, cross-platform.

3. **Cross-platform builds**: Verify `purelb.io/pkg/apis/purelb/v2/iprange.go` compiles on macOS/Windows (imports `netlink/nl` for `AddrFamily()` constants). If not, extract `IPRange` and `AddrFamily` into platform-independent subpackage or define local constants.

4. **Election hash — shared package**: Move the `election()` hash function from [election.go:879-891](internal/election/election.go#L879-L891) into `pkg/election/` with no system dependencies. Both the real election code and the plugin import it. Eliminates drift risk. Package also contains `ParseSubnetsAnnotation()` / `FormatSubnetsAnnotation()` and annotation constants.

5. **Netbox pools**: Show URL/tenant and allocated count. Indicate capacity is managed externally.

### New Dependencies (plugin)
- `github.com/spf13/cobra` — CLI subcommand framework
- `k8s.io/cli-runtime` — kubectl-style flags
- Everything else already in go.mod

### Critical Files to Reuse
- [pkg/apis/purelb/v2/iprange.go](pkg/apis/purelb/v2/iprange.go) — `NewIPRange`, `Size()`, `Contains()`, `Family()`
- [pkg/apis/purelb/v2/annotations.go](pkg/apis/purelb/v2/annotations.go) — annotation constants
- [pkg/apis/purelb/v2/types.go](pkg/apis/purelb/v2/types.go) — ServiceGroup/LBNodeAgent types
- [pkg/generated/clientset/](pkg/generated/clientset/) — typed K8s client for PureLB CRDs
- [internal/election/election.go:879](internal/election/election.go#L879) — election hash (move to `pkg/election/`)

### RBAC Requirements
The plugin user needs a single ClusterRole with:
- `get`, `list` on: services, servicegroups, lbnodeagents, leases, nodes, pods, endpointslices, events, bgpconfigurations, bgpnodestatuses

**No pod exec required.** Read-only access to all resources.

A sample ClusterRole YAML included in the repo.

### Build & Distribution
- Add `cmd/kubectl-purelb` build entry to Makefile
- `go build ./cmd/kubectl-purelb` for local binary
- Verify cross-platform compilation (`GOOS=darwin`, `GOOS=windows`) before first release
- `.krew.yaml` plugin manifest at repo root for krew index submission
- Multi-arch: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64

---

## Implementation Order

### Workstream 1: k8gobgp BGPNodeStatus CRD
1. Define BGPNodeStatus CRD types and generate manifests
2. Add status writer reconcile loop to k8gobgp
3. Add RBAC for BGPNodeStatus to k8gobgp
4. Test: verify status is written on startup, updated on reconcile, deleted on shutdown

### Workstream 2: Shared packages (can run in parallel with WS1)
5. Create `pkg/election/` — move hash function + subnet annotation parsing from `internal/election/`
6. Update `internal/election/` to import from `pkg/election/`
7. Verify cross-platform compilation of `pkg/apis/purelb/v2/iprange.go`; extract if needed

### Workstream 3: Plugin implementation (depends on WS1 types, can start scaffolding in parallel)
8. Scaffolding — Cobra root, client builder, output helpers, RBAC sample
9. `pools` — ServiceGroup + Service correlation
10. `services` — annotation parsing, lease cross-reference, shared IP grouping
11. `election` — lease parsing, subnet coverage, drain simulation
12. `inspect` — pending diagnostics, deep single-service view, BGP route check for remote pools
13. `validate` — config consistency checks
14. `bgp sessions` — reads BGPNodeStatus CRs, evaluates nodeSelectors
15. `bgp dataplane` — reads BGPNodeStatus CRs for full import/RIB/advertisement/export pipeline
16. `status` — aggregation of all other commands
17. `version` — component version reporting

---

## Verification

- Unit tests for each command using fake k8s clientsets with known CRs
- Verify cross-platform compilation: `GOOS=darwin go build`, `GOOS=windows go build`
- Test dual-stack scenarios (services with both IPv4 and IPv6 IPs)
- Test multi-subnet scenarios (nodes on different subnets, uncovered pool ranges)
- Test drain simulation accuracy: drain a node, verify predicted winners match actual
- Test pending service diagnostics: pool exhausted, missing SG, IP conflict, port conflict, sharing key conflict
- Test validate: overlapping pools, uncovered subnets, misconfigured netlinkImport, zero-match nodeSelectors
- Test Netbox pool display (external management indicator)
- Test shared IP grouping with multiple services sharing same key
- Test bgp sessions with and without BGPNodeStatus CRDs present (graceful degradation for older k8gobgp)
- Test bgp dataplane: import failures, export failures, missing service routes
- Test bgp dataplane with VRFs (basic awareness: list VRFs, route counts)
- Integration test on real cluster: deploy PureLB + k8gobgp with BGPNodeStatus, run every command
- Test status command aggregation with various healthy/unhealthy scenarios
