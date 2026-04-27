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
`families` | string array | Required | Address families to enable: `"ipv4-unicast"`, `"ipv6-unicast"`.

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
`timers.holdTime` | BGP hold time (default: 90s)
`timers.keepaliveInterval` | Keepalive interval (default: 30s)
`transport.passiveMode` | Wait for peer to initiate (default: false)
`authPasswordSecretRef` | Reference to a Secret containing the BGP authentication password
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

Check status with:

```sh
kubectl get bgpnodestatus -n purelb-system
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

## Complete BGPConfiguration CRD Reference

This page covers the most common configuration fields. The BGPConfiguration CRD supports additional features including policy definitions, VRFs, route reflector configuration, graceful restart, and netlink export rules.

For the complete CRD field definitions, see the [k8gobgp repository](https://github.com/purelb/k8gobgp) and the CRD schema installed in your cluster:

```sh
kubectl explain bgpconfiguration.spec
kubectl explain bgpconfiguration.spec.neighbors
kubectl explain bgpconfiguration.spec.netlinkImport
```
