---
title: "Address Types"
description: "Local addresses, remote addresses, and Netbox IPAM -- how PureLB handles different address types."
weight: 20
---

PureLB supports three types of address pools, each configured in a [ServiceGroup]({{< relref "/docs/configuration/service-groups" >}}). Each ServiceGroup specifies exactly one type.

## Local Addresses

**Use when:** The pool addresses are on the same subnet as your cluster nodes.

Local addresses are added to a single node's physical interface (e.g., `eth0`). The Linux networking stack responds to ARP (IPv4) and NDP (IPv6) requests for that address, making it reachable on the local network without any routing.

PureLB uses a [Lease-based election]({{< relref "/docs/overview/election" >}}) to choose which node announces each local address. Only one node holds a given address at a time. If that node fails, a new winner is elected and the address moves.

{{< mermaid >}}
graph TD
    subgraph "Cluster (same subnet: 192.168.1.0/24)"
        N1["Node 1<br/>192.168.1.101<br/><b>+ 192.168.1.240 (VIP)</b>"]
        N2["Node 2<br/>192.168.1.102"]
        N3["Node 3<br/>192.168.1.103"]
    end
    Client["Client<br/>192.168.1.50"] -->|"ARP: who has .240?"| N1
    style N1 fill:#90EE90
{{< /mermaid >}}

See [ServiceGroup local pools]({{< relref "/docs/configuration/service-groups#local-pools" >}}) for configuration.

### Characteristics

- **Single announcer:** Only one node holds the address (elected via Kubernetes Leases).
- **L2 reachability:** No routing needed -- works on a flat L2 network.
- **ExternalTrafficPolicy:** Only `Cluster` is supported. Local addresses are announced by a single node, so `Local` policy would drop traffic when target pods aren't on that node.
- **Failover:** When the announcing node fails, a new winner is elected and sends Gratuitous ARP to update switches.

### Multi-Subnet Local Addresses

Clusters often span multiple subnets. With `multiPool: true`, a single service gets a local address on **every subnet** that has active nodes -- making it reachable from all network segments without BGP or any routing. Each address is announced by a node on its respective subnet via the normal [election]({{< relref "/docs/overview/election" >}}).

Use `balancePools: true` instead if you want a single address but want PureLB to distribute allocations evenly across ranges. See [Multi-Pool Allocation]({{< relref "/docs/configuration/service-groups#multi-pool-allocation" >}}) for configuration.

### Address Lifetime

By default, PureLB configures local addresses with a non-permanent lifetime (300 seconds, auto-renewed). This prevents CNI plugins like Flannel and DHCP clients from mistaking a LoadBalancer VIP for the node's primary address -- a common source of hard-to-diagnose networking failures.

If your environment requires different lifetime behavior, see [Address Lifetime]({{< relref "/docs/configuration/lbnodeagent#address-lifetime" >}}) in the LBNodeAgent configuration.

## Remote Addresses

**Use when:** The pool addresses are on a different subnet from your cluster nodes, and you have a BGP router upstream.

Remote addresses are added to a dummy interface (`kube-lb0`) on **every** node. The k8gobgp sidecar advertises routes to these addresses via BGP. Upstream routers use ECMP (Equal-Cost Multi-Path) to distribute traffic across all nodes.

{{< mermaid >}}
graph TD
    Client["Client"] --> Router["Upstream Router<br/>(BGP + ECMP)"]
    Router -->|"next-hop .101"| N1["Node 1<br/>kube-lb0: 172.31.0.1/32"]
    Router -->|"next-hop .102"| N2["Node 2<br/>kube-lb0: 172.31.0.1/32"]
    Router -->|"next-hop .103"| N3["Node 3<br/>kube-lb0: 172.31.0.1/32"]
    style N1 fill:#90EE90
    style N2 fill:#90EE90
    style N3 fill:#90EE90
{{< /mermaid >}}

See [ServiceGroup remote pools]({{< relref "/docs/configuration/service-groups#remote-pools" >}}) for configuration.

### Characteristics

- **All nodes announce:** Every node adds the address to kube-lb0 and advertises it via BGP.
- **L3 reachability:** Requires BGP routing (k8gobgp) and an upstream router.
- **ExternalTrafficPolicy:** Both `Cluster` and `Local` are supported.
- **ECMP load balancing:** The upstream router distributes traffic across all advertising nodes.
- **Scalability:** Adding nodes automatically adds ECMP paths.

### Aggregation

The `aggregation` field controls the address mask, which determines what routes are advertised. Use `/32` (IPv4) or `/128` (IPv6) for host routes, `default` for the subnet mask. See [Aggregation]({{< relref "/docs/configuration/service-groups#aggregation" >}}) for details.

## Netbox Addresses

**Use when:** You manage IP addresses in an external Netbox IPAM system.

Instead of defining address pools locally, PureLB requests addresses from Netbox one at a time. Netbox tracks allocation and prevents conflicts across your infrastructure.

See [Netbox IPAM Integration]({{< relref "/docs/configuration/netbox" >}}) for configuration.

## ExternalTrafficPolicy

LoadBalancer Services support two external traffic policies:

Policy | Local Addresses | Remote Addresses | Source IP Preserved
-------|-----------------|------------------|--------------------
`Cluster` (default) | Supported | Supported | No (SNAT by kube-proxy)
`Local` | Not supported | Supported | Yes (Direct Server Return)

With `externalTrafficPolicy: Local` on a remote pool, traffic is delivered directly to pods without kube-proxy rewriting it (Direct Server Return). The original client source IP and destination address are preserved. PureLB only adds the address to nodes running target pods, so BGP only advertises routes from those nodes.

## Dual-Stack

ServiceGroups can contain both `v4pools` and `v6pools`. IPv4 and IPv6 addresses may be announced from different nodes for local pools, since election is per-address.

## Address Sharing

Multiple services can share a single IP address if they use different ports. Set the same `purelb.io/allow-shared-ip` annotation value on each service. ExternalTrafficPolicy is forced to `Cluster` for shared addresses.
