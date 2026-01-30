---
title: "Overview"
description: "High Level Overview"
weight: 10
mermaid: true

hide: [ "toc", "footer" ]
---

PureLB consists of two components:

 * **Allocator.**  The Allocator runs on a single node. It watches for LoadBalancer change events and allocates IP addresses.

 * **LBNodeAgent.**  The LBNodeAgent runs on all nodes that packets for exposed services can transit. It watches for LoadBalancer change events and configures networking behavior.

[Kubernetes' `kube-proxy`](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-proxy/) is important, but not part of PureLB.  `kube-proxy` watches for service changes and adds the addresses managed by PureLB to configure communication within the cluster.

{{% notice %}} _Instead of thinking of PureLB as **advertising services**, think of it as **attracting packets** to allocated addresses, with `kube-proxy` forwarding those packets within the cluster via
 the Container Network Interface Network (pod Network)._ {{% /notice %}}

{{<mermaid align="center">}}
  graph TD;
    A(Allocator);
    B(kubeAPI);
    C(LBNodeAgent);
    D(kube-proxy);
    E[cli: $ kubectl expose ....];
    A---B;
    B---C;
    E-->B;
    B---D;
{{</mermaid>}}

The PureLB Allocator watches kubeAPI for changes to Services of type LoadBalancer. It allocates an address from the requested ServiceGroup and adds it to the LoadBalancer. The LBNodeAgents see the address allocation and configure Linux networking on each node to allow packets to reach that node.

`kube-proxy` makes the necessary configuration changes so traffic arriving at a node with the LoadBalancer's address is forwarded to the correct pods. If `kube-proxy` is operating in default mode, it will configure IPtables to match the allocated address and forward to the Nodeport address. If operating in IPVS mode, the external address is added to the IPVS tables and the IPVS virtual interface.

To use a LoadBalancer the Service type is set to `type: LoadBalancer`.

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    purelb.io/service-group: localaddrange
  creationTimestamp: "2022-01-10T15:57:45Z"
  name: kuard-svc-dual-remote
  namespace: default
spec:
  allocateLoadBalancerNodePorts: false
  externalTrafficPolicy: Cluster
  internalTrafficPolicy: Cluster
  ipFamilies:
  - IPv4
  - IPv6
  ipFamilyPolicy: RequireDualStack
  ports:
  - port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: kuard
  sessionAffinity: None
  type: LoadBalancer
```

The Allocator is configured with one "default" ServiceGroup. Additional ServiceGroups can be defined and accessed using annotations.

## IP Address Management
IP Address Management (IPAM) is a critical function in any network. Ensuring that addresses are allocated to devices in a manner that results in the desired connectivity requires planning and ongoing management.  PureLB includes an integrated address allocator, and can also interface with external IPAM systems, allowing address pools to be managed by PureLB for some use cases, and retrieved from an external IPAM system in others. [ServiceGroups](../overview/#servicegroups) contain all address configuration. In the case of the local allocator, those ServiceGroups describe IP address pools. For external IPAM, the ServiceGroup contains the information to connect to the external IPAM system.

By moving IPAM out of the k8s cluster, PureLB can reduce the number of places where IP addresses are allocated, reducing the possibility of configuration errors and simplifying system management.

External IPAM is common in large-scale systems: the combination of static addresses, dynamic addresses, and routing, is too complex to manage with a spreadsheet. Add that public IPv4 addresses are increasingly scarce, causing many large systems re-use private address space behind network address translators, and a complex management picture begins to unfold.

PureLB supports [Netbox](https://github.com/netbox-community/netbox), a popular open source IPAM system. Instead of manually using Netbox to statically allocate a pool of addresses to PureLB, PureLB's Allocator can request addresses directly from Netbox one at a time.

PureLB's [IPAM configuration](../install/extipam) is defined in ServiceGroups. When PureLB fetches an address from Netbox it applies that addess to interfaces in the same way as an address from its internal allocator.

It is straightforward to add other external IPAM systems to PureLB, and we encourage you to add support for your IPAM system. If the demand is there, we will add more IPAM integrations.

## ServiceGroups
A PureLB ServiceGroup represents an IPAM mechanism and its configuration parameters. When using the Allocator's built-in IPAM a ServiceGroup consists of the following:

 * Name:  The name of the ServiceGroup (referenced by annotations in Service definitions)
 * local:  PureLB will allocate addresses from the information contained in the ServiceGroup
 * v4pools:  A pool of IPv4 addresses
 * v6pools:  A pool of IPv6 addresses
 * subnet:  In the form of CIDR, the network that addresses are allocated from
 * aggregation:  Where the address is not local, this allows subnet to be aggregated

Each ServiceGroup is a Custom Resource and is namespaced.

```yaml
apiVersion: "purelb.io/v1"
kind: ServiceGroup
metadata:
  name: localaddrange
  namespace: purelb-system
spec:
 local:
    v4pools:
    - aggregation: default
      pool: 192.168.10.240-192.168.10.243
      subnet: 192.168.10.0/24
    v6pools:
    - aggregation: default
      pool: fc00:270:154:0:8100::4/64
      subnet: fc00:270:154::/64
```

Kubernetes supports Dual Stack by default, so IPv4, IPv6, or both are configured in the ServiceGroup.  The Allocator allocates addresses from the requested address families.

### Local Addresses
Each Linux host is connected to a network and therefore has a CIDR address.  Addresses in the host's CIDR subnet are considered ["local addresses"](../localint).

> _For example: let's say that a host's CIDR address is 192.168.100.10/24. If a ServiceGroup used a pool of 192.168.100.200-192.168.100.250 from the same subnet (192.168.100.0/24), then addresses from that ServiceGroup would be considered local._

The LBNodeAgent identifies the interface with that subnet, [elects](../localint#memberlist) a single node on that subnet, and then adds the LoadBalancer address to the physical interface on that node.

### Virtual Addresses
PureLB can use ["virtual addresses"](../virtint), which are addresses that are not currently in use by the cluster. When the LBNodeAgent processes a Service with a virtual address, it adds that address to a virtual interface called `kube-lb0`. This interface is used with [routing software](../routers) to advertise routes to the address. Any routing protocol or topology can be used depending on the routing software's capabilities.

LBNodeAgent adds IP addresses to either a _local physical interface_ or a _virtual interface_. It's easy to see which addresses are allocated to which interfaces; that information is added to the LoadBalancer and can be viewed on the host using standard Linux iproute2 tools.

Virtual addresses and local addresses can be used concurrently. No configuration is needed other than adding the appropriate addresses to ServiceGroups.

### IPv6 Dual Stack
PureLB supports IPv6. From a PureLB user perspective, the allocation behavior is very similar, however the LBNodeAgent does elect local interfaces independently for IPv4 and IPv6, therefore addresses can appear on different nodes.  A Cluster and CNI supporting Dual Stack IPv6 is required.

### External Traffic Policy
LoadBalancer Services can be configured with an External Traffic Policy which controls the distribution of external traffic in the cluster. The default setting, `externalTrafficPolicy: Cluster`, uses `kube-proxy` to forward traffic to pods over the CNI network and load balance traffic among all pods. Any node can receive traffic, and that node distributes traffic to pods on all nodes. `externalTrafficPolicy: Local` constrains the LoadBalancer to send traffic only to nodes that are running target pods, resulting in traffic not traversing the CNI. **As traffic does not transit the CNI, `kube-proxy` does not NAT, therefore the original source IP address is retained**.

Local addresses [support](../localint#external-traffic-policy) only `externalTrafficPolicy: Cluster`. Virtual addresses [support](../virtint#external-traffic-policy) both `Cluster` and `Local` behavior.

External Traffic Policy can be a useful tool in k8s edge design, especially when additional forms of load balancing are added using Ingress Controllers or a Service Mesh to further control which hosts receive traffic. Consideration of network design is recommended before using this feature.

### Allocating Node Ports
Allocating node ports is often unnecessary for LoadBalancer Services as the LoadBalancer Service will be used for access, not the NodePort.  PureLB supports setting `allocateLoadBalancerNodePorts: false`.

### Address Sharing
Multiple services can share a single IP address, as long as each service exposes different ports. External Traffic Policy is not supported and therefore ignored for shared addresses. This is necessary as the combination of address sharing and pod locality could result in traffic being presented at a node where `kube-proxy` had not configured forwarding, which would cause traffic to be dropped.

Address sharing is enabled by adding a "sharing key" to the service.
