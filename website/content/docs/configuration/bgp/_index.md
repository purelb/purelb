---
title: "BGP Routing with k8gobgp"
description: "Configure k8gobgp for BGP route advertisement of remote addresses."
weight: 30
---

PureLB ships [k8gobgp](https://github.com/purelb/k8gobgp) as a sidecar in the lbnodeagent DaemonSet. k8gobgp advertises [remote addresses]({{< relref "/docs/overview/address-types#remote-addresses" >}}) to upstream BGP routers, enabling routed access to LoadBalancer Services.

BGP is enabled by default. To disable it, install with the `-nobgp` manifest variant or set `gobgp.enabled=false` in Helm.

## How It Works

{{< mermaid >}}
graph LR
    PureLB["LBNodeAgent<br/>adds VIP to kube-lb0"] --> NL["Linux kernel<br/>routing table"]
    NL -->|"netlinkImport<br/>(kube-lb0)"| GoBGP["k8gobgp<br/>sidecar"]
    GoBGP -->|"BGP UPDATE"| Router["Upstream<br/>Router"]
    Router -->|"ECMP"| Clients["Clients"]
{{< /mermaid >}}

1. PureLB's LBNodeAgent adds remote addresses to the `kube-lb0` interface.
2. k8gobgp monitors `kube-lb0` via **netlinkImport** and picks up the new routes.
3. k8gobgp advertises these routes to configured BGP neighbors.
4. The upstream router installs ECMP routes (one next-hop per node) and distributes client traffic.

## BGPConfiguration CRD

k8gobgp is configured via the `BGPConfiguration` CRD (`bgp.purelb.io/v1`). Create one CR named `default` in `purelb-system`:

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: default
  namespace: purelb-system
spec:
  global:
    asn: 65000
    routerID: ""
    listenPort: 179
    families:
    - "ipv4-unicast"
    - "ipv6-unicast"

  netlinkImport:
    enabled: true
    interfaceList:
    - "kube-lb0"

  neighbors:
  - config:
      neighborAddress: "192.0.2.1"
      peerAsn: 65001
      description: "Upstream BGP router"
    afiSafis:
    - family: "ipv4-unicast"
      enabled: true
    - family: "ipv6-unicast"
      enabled: true
```

## Global Configuration

Field | Type | Default | Description
------|------|---------|------------
`asn` | int | Required | Local Autonomous System Number. Use a private ASN from 64512-65534.
`routerID` | string | Auto-detect | BGP router identifier. Leave empty for auto-detection from the node's internal IPv4 address, or set explicitly for multi-homed nodes.
`listenPort` | int | `179` | BGP listen port.
`families` | string array | `ipv4-unicast`, `ipv6-unicast` | Global address families. Optional: omit it to keep both IPv4 and IPv6 unicast. Values are lowercase and case-sensitive. A list without `ipv6-unicast` turns IPv6 advertisement off for every Service.

### Router ID Options

- **Empty string** (default): Auto-detect from the node's internal IPv4 address.
- **Explicit IP**: e.g., `"192.168.1.101"` -- use for multi-homed nodes to avoid ambiguity.
- **Template variable**: `"${NODE_IP}"`, `"${NODE_IPV4}"`, `"${NODE_EXTERNAL_IP}"` -- resolved per node.

## netlinkImport

> [!WARNING]
> `netlinkImport` is required. Without it, k8gobgp starts but advertises **no routes**. You must enable it and include the dummy interface name.

```yaml
netlinkImport:
  enabled: true
  interfaceList:
  - "kube-lb0"
```

The `interfaceList` must match the LBNodeAgent's `dummyInterface` value (default: `kube-lb0`).

## Neighbors

Each neighbor entry configures a BGP peering session:

```yaml
neighbors:
- config:
    neighborAddress: "192.0.2.1"    # Upstream router IP
    peerAsn: 65001                  # Upstream router's ASN
    description: "TOR switch"
  afiSafis:
  - family: "ipv4-unicast"
    enabled: true
  - family: "ipv6-unicast"
    enabled: true
```

### Key Neighbor Fields

Field | Description
------|------------
`config.neighborAddress` | IP address of the BGP peer (required)
`config.peerAsn` | Peer's ASN (required, must differ from local `asn` for eBGP)
`config.description` | Human-readable description
`afiSafis` | Address families to negotiate with this peer
`timers.config.holdTime` | BGP hold time (default: 90s). Changing it resets the session
`timers.config.keepaliveInterval` | Keepalive interval (default: 30s). Changing it resets the session
`transport.passiveMode` | Wait for peer to initiate (default: false)
`config.authPasswordSecretRef` | Reference to a Secret containing the BGP authentication password
`nodeSelector` | Kubernetes label selector to limit which nodes peer with this neighbor

### Node-Specific Peers

Use `nodeSelector` to peer different nodes with different routers (e.g., in a multi-rack topology):

```yaml
neighbors:
- config:
    neighborAddress: "10.1.1.1"
    peerAsn: 65001
    description: "Rack 1 TOR"
  nodeSelector:
    matchLabels:
      topology.kubernetes.io/zone: rack-1
  afiSafis:
  - family: "ipv4-unicast"
    enabled: true
- config:
    neighborAddress: "10.2.1.1"
    peerAsn: 65001
    description: "Rack 2 TOR"
  nodeSelector:
    matchLabels:
      topology.kubernetes.io/zone: rack-2
  afiSafis:
  - family: "ipv4-unicast"
    enabled: true
```

## BGPNodeStatus

k8gobgp writes a `BGPNodeStatus` CR per node, reporting:

- Neighbor state (Established, Active, Connect, etc.)
- Prefixes sent and received per neighbor
- Health status
- Last error messages

BGPNodeStatus is cluster-scoped (one per node, no namespace). Check status with:

```sh
kubectl get bgpnodestatus
kubectl purelb bgp sessions
```

## ECMP Load Balancing

When multiple nodes advertise the same route, the upstream router sees multiple next-hops with equal cost. With ECMP enabled, the router distributes traffic across all next-hops using a hash of the packet's source IP, destination IP, source port, and destination port (4-tuple).

Key considerations:

- Modern switches support large ECMP groups (hundreds of paths). Older routers may limit ECMP to less than 10 paths.
- Adding or removing a node changes the ECMP set, which may cause some flows to be rehashed.
- Verify your upstream router has ECMP enabled and supports the number of nodes in your cluster.

## Verification

```sh
# Check BGP session state
kubectl purelb bgp sessions

# Check route pipeline (import -> RIB -> advertise)
kubectl purelb bgp dataplane

# Check from inside the k8gobgp sidecar
kubectl purelb gobgp neighbor
kubectl purelb gobgp global rib
```

## Metrics & Health Checks

The k8gobgp sidecar runs two processes, and each serves its own metrics:

| Endpoint | Served by | Metrics |
|----------|-----------|---------|
| `http://<node-ip>:7473/metrics` | k8gobgp controller | `k8gobgp_*`: reconciliation, configuration, RIB size, router ID |
| `http://<node-primary-ip>:7475/metrics` | gobgpd | `bgp_*`: per-peer session state, messages and routes; BFD; netlink |
| `http://<node-ip>:7474/healthz` | k8gobgp controller | Liveness |
| `http://<node-ip>:7474/readyz` | k8gobgp controller | Readiness: gobgpd answers a `GetBgp` request |

gobgpd listens on the node's primary address only (the first InternalIP). The
Helm chart can create a ServiceMonitor for both metrics endpoints:
`Prometheus.gobgp.serviceMonitor.enabled`.

### Key Metrics

Metric | Port | Labels | Description
-------|------|--------|------------
`k8gobgp_gobgpd_connection_status` | 7473 | `endpoint` | 1 if the controller can reach gobgpd, 0 otherwise
`k8gobgp_configured_objects` | 7473 | `kind`, `name`, `namespace` | Objects from the BGPConfiguration pushed to this node's gobgpd, by `kind` (`neighbor`, `peer_group`, `dynamic_neighbor`, `vrf`, `policy`, `defined_set`)
`k8gobgp_rib_routes` | 7473 | `family` | Routes in the local RIB (`family="ipv4_unicast"`, `"ipv6_unicast"`). Polled every 60s
`k8gobgp_peer_apply_errors_total` | 7473 | `key`, `op` | Peers gobgpd refused. A refused peer never appears in the `bgp_*` metrics, so this is the only place it shows
`k8gobgp_global_restart_required` | 7473 | `field`, `name`, `namespace` | 1 when a global setting was edited and only takes effect after the pod restarts
`k8gobgp_nodestatus_write_total` | 7473 | `result` | BGPNodeStatus writes (`success`, `error`, `skipped`)
`bgp_peer_state` | 7475 | `peer`, `session_state`, `admin_state` | 1 per peer, labelled with its state. Established peers: `count(bgp_peer_state{session_state="SESSION_STATE_ESTABLISHED"})`
`bgp_routes_advertised` | 7475 | `peer`, `route_family` | Routes sent to each peer (`route_family="ipv4-unicast"`, `"ipv6-unicast"`)
`bgp_routes_received`, `bgp_routes_accepted` | 7475 | `peer`, `route_family` | Routes from each peer, before and after import policy

The two endpoints spell the family differently: `family="ipv4_unicast"` on 7473,
`route_family="ipv4-unicast"` on 7475.

The full list, ready-made queries, a scrape-time filter for 7475 and alert
rules are in k8gobgp's [metrics documentation](https://github.com/purelb/k8gobgp/blob/v0.2.6/docs/metrics.md),
[alerts](https://github.com/purelb/k8gobgp/blob/v0.2.6/docs/alerting/k8gobgp-alerts.yaml)
and [PodMonitors](https://github.com/purelb/k8gobgp/blob/v0.2.6/docs/monitoring/podmonitors.yaml).
Upgrading from k8gobgp v0.2.4 renames or removes several metrics; see the
[v0.17.0 migration guide]({{< relref "/docs/migration/v0-17-0" >}}).

### Health Checks

`/readyz` reports whether gobgpd is answering. It does **not** report BGP
sessions: a pod can be Ready with no session established. Watch
`bgp_peer_state` for sessions.

## Complete BGPConfiguration CRD Reference

This page covers the most common configuration fields. The BGPConfiguration CRD supports additional features including policy definitions, VRFs, route reflector configuration, graceful restart, and netlink export rules.

For the complete CRD field definitions, see the [k8gobgp repository](https://github.com/purelb/k8gobgp) and the CRD schema installed in your cluster:

```sh
kubectl explain bgpconfig.spec
kubectl explain bgpconfig.spec.neighbors
kubectl explain bgpconfig.spec.netlinkImport
```
