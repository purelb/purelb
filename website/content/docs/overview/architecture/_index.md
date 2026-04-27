---
title: "Architecture"
description: "PureLB components, data flow, and how they integrate with Kubernetes."
weight: 10
---

PureLB consists of two components that work together with the Kubernetes API server:

* **Allocator.** A single-replica Deployment that watches for LoadBalancer Services and allocates IP addresses from configured pools.

* **LBNodeAgent.** A DaemonSet running on every node that configures Linux networking to announce allocated addresses. When BGP is enabled, each LBNodeAgent pod includes a **k8gobgp** sidecar that advertises routes to upstream routers.

[Kubernetes `kube-proxy`](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-proxy/) is not part of PureLB but plays an important role: once PureLB attracts external traffic to a node, kube-proxy's nftables rules forward it to the correct pods within the cluster.

> [!NOTE]
> _Think of PureLB as **attracting packets** to allocated addresses, with kube-proxy forwarding those packets within the cluster via the pod network._

## Data Flow

{{< mermaid >}}
graph LR
    User["User creates<br/>LoadBalancer Service"] --> API["Kubernetes<br/>API Server"]
    API --> Alloc["Allocator<br/>(watches Services)"]
    Alloc -->|"Allocates IP<br/>from ServiceGroup"| API
    API --> LBN["LBNodeAgent<br/>(watches Services)"]
    LBN -->|"Configures<br/>Linux networking"| Node["Node interfaces<br/>(eth0 / kube-lb0)"]
    API --> KP["kube-proxy<br/>(watches Services)"]
    KP -->|"Configures<br/>nftables rules"| NFT["Packet forwarding<br/>to pods"]
{{< /mermaid >}}

### Step by Step

1. A user creates a Service with `type: LoadBalancer`.
2. The **Allocator** sees the new Service, selects a [ServiceGroup]({{< relref "/docs/configuration/service-groups" >}}) pool, and allocates an IP address. It writes the address to the Service's `.status.loadBalancer.ingress` and sets the `purelb.io/allocated-by` annotation.
3. The **LBNodeAgents** see the updated Service. For [local addresses]({{< relref "/docs/overview/address-types#local-addresses" >}}), the [election system]({{< relref "/docs/overview/election" >}}) picks a single winner node; for [remote addresses]({{< relref "/docs/overview/address-types#remote-addresses" >}}), all nodes participate.
4. The winning node(s) configure Linux networking using netlink: adding the IP to the physical interface (local) or dummy interface (remote), and optionally sending Gratuitous ARP.
5. **kube-proxy** independently sees the Service and configures nftables rules to forward traffic arriving at the LoadBalancer address to the correct backend pods.

## Custom Resource Definitions

PureLB uses CRDs for all configuration:

CRD | API Group | Purpose
----|-----------|--------
ServiceGroup | `purelb.io/v2` | Defines IP address pools (local, remote, or Netbox)
LBNodeAgent | `purelb.io/v2` | Configures node agent behavior (interfaces, GARP, address lifetime)
BGPConfiguration | `bgp.purelb.io/v1` | Configures k8gobgp BGP peering (when BGP is enabled)
BGPNodeStatus | `bgp.purelb.io/v1` | Per-node BGP status (written by k8gobgp, read-only)

## Namespace

All PureLB components run in the `purelb-system` namespace. ServiceGroups and LBNodeAgents are namespaced resources -- we recommend placing them in `purelb-system` for simplicity, but they can be created in other namespaces if RBAC requires it.

## Security

Component | Runs As | Capabilities | Network
----------|---------|-------------|--------
Allocator | Non-root (UID 65534), read-only filesystem | None | Cluster-internal only
LBNodeAgent | Root (required for netlink) | `NET_ADMIN`, `NET_RAW` | Host network
k8gobgp | Root (required for BGP port 179) | `NET_ADMIN`, `NET_BIND_SERVICE`, `NET_RAW` | Host network
