---
title: "Overview"
description: "High Level Overview"
weight: 10
mermaid: true

hide: [ "toc", "footer" ]
---

PureLB consists of two components:

 * **Allocator.**  The Allocator runs on a single node. It watches the service API for LoadBalancer service events and allocates IP addresses.

 * **LBNodeAgent.**  The LBNodeAgent runs on all nodes that packets for exposed services can transit. It watches service changes and configures networking behavior.

KubeProxy is important, but not part of PureLB.  KubeProxy watches service changes and adds the addresses managed by PureLB to configure communication within the cluster.

{{% notice %}} _Instead of thinking of PureLB as **advertising services**, think of it as **attracting packets** to allocated addresses, with KubeProxy forwarding those packets within the cluster via
 the Container Network Interface Network (pod Network)._ {{% /notice %}}

{{<mermaid align="center">}}
  graph TD;
    A(Allocator);
    B(kubeAPI);
    C(LBNodeAgent);
    D(KubeProxy);
    E[cli: $ kubectl expose ....];
    A---B;
    B---C;
    E-->B;
    B---D;
{{</mermaid>}}

The PureLB allocator watches the k8s service API for services of type LoadBalancer with IP address set to nil. It allocates an address from a ServiceGroup (Local IPAM) or sends a request for an IP address to the configured IPAM system.  The address returned is used to update the k8s service.

To use a Service Load Balancer the service type is set to LoadBalancer.

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

The Allocator is configured with one "default" ServiceGroup. Multiple ServiceGroups can be defined and accessed using annotations.

## ServiceGroups

A PureLB ServiceGroup represents an IPAM mechanism and its configuration parameters. When using the Allocator's built-in IPAM a ServiceGroup consists of the following:

 * Name:  The name of the ServiceGroup referenced by annotations in the service definition.  The default ServiceGroup is used when no annotation is present
 * local:  PureLB will allocate addresses from the information contained in the ServiceGroup
 * v4pools:  A pool of IPv4 addresses
 * v6pools:  A pool of IPv6 addresses
 * Subnet:  In the form of CIDR, the network that addresses are allocated from
 * Aggregation:  Where the address is not local, this allows subnet to be aggregated

Each ServiceGroup is a Custom Resource and is namespaced.

```yaml
apiVersion: "purelb.io/v1"
kind: ServiceGroup
metadata:
  name: localaddrange
  namespace: purelb
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

Kubernetes supports Dual Stack by default, so IPv4, IPv6, or both are configured in the ServiceGroup.  The allocator provides addresses from the requested address families.

Once the Allocator has allocated address(es) (which will be visible in the service), it updates the service. The LBNodeAgent and KubeProxy see this update and respond.

The LBNodeAgent converts the address into an IPNet (192.168.100.1/24) and configures Linux to use it.

KubeProxy makes the necessary configuration changes so traffic arriving at a node with the allocated address is forwarded to the correct pods. If KubeProxy is operating in default mode, it will configure IPtables to match the allocated address and forward to the Nodeport address. If operating in IPVS mode, the external address is added to the IPVS tables and the IPVS virtual interface.

### Local Addresses
Each Linux host is connected to a network and therefore has a CIDR address.  A Local address is any address that matches the subnet of the host's address.

> _For example: let's say that DHCP allocates 192.168.100.10/24 from an address pool of 192.168.100.2-192.168.100.100. If a ServiceGroup used the same subnet (192.168.100.0/24 with a pool of 192.168.100.200-192.168.100.250), addresses from that ServiceGroup would be considered local._

The LBNodeAgent identifies the interface with that subnet, elects a single node on that subnet, and then adds it to the physical interface on that node.

### Virtual Addresses
PureLB can use "virtual addresses", which are addresses that are not currently in use by the cluster. When the LBNodeAgent receives a Service with a _virtual address_, it adds that address to a virtual interface called _kube-lb0_.  This virtual interface is used in conjunction with routing software to advertise routes to these addresses.  Any routing protocol or topology can be used based on the routing software's capabilities.

LBNodeAgent adds IP Addresses to either the _local physical interface_ or _virtual interface_. It's easy to see which addresses are allocated to interfaces; that information is added to the service and can be viewed on the host using standard Linux iproute2 tools.

Virtual addresses and local addresses can be used concurrently. No configuration is needed other than adding the appropriate addresses to ServiceGroups.

### IPv6 Dual Stack
PureLB supports IPv6. With the advent of Dual Stack, the use of IPv4 and IPv6 addresses is integrated with Kubernetes. From a PureLB user perspective, the allocation behavior is very similar, however the _LBNodeAgent_ does elect local interfaces independently for IPv4 and IPv6, therefore addresses can appear on different nodes.  A Cluster and CNI supporting Dual Stack IPv6 is required.

### External Traffic Policy
LoadBalancer Service can be configured with an External Traffic Policy.  Its purpose is to control the distribution of external traffic in the cluster and requires support from the LoadBalancer controller.  The default setting, "Cluster", is used to implement forwarding to pods over the CNI network.  Any node can receive traffic, and the node receiving the traffic distributes traffic to pod(s). Cluster mode depends on KubeProxy to distribute traffic to the correct pod(s) and load balances traffic among all pod(s).  The "Local" setting is used to constrain the LoadBalancer to send traffic only to nodes that are running the target pod(s), resulting in traffic not traversing the CNI.  **As traffic does not transit the CNI, KubeProxy does not NAT, therefore the original source IP address is retained**.  External Traffic Policy can be a useful tool in k8s edge design, especially when additional forms of load balancing are added using Ingress Controllers or a Service Mesh to further control which hosts receive traffic for distribution to pods. Consideration of network design is recommended before using this feature.

External Traffic Policy is ignored for Local Addresses. Virtual addresses support _ExternalTrafficPolicy: Local_ behavior.

PureLB Service behavior is consistent with k8s when setting _ExternalTrafficPolicy: Cluster_: the address is added irrespective of the state of the pod identified in the selector.  However, when set to _ExternalTrafficPolicy: Local_, PureLB must identify if there are any pods on the node prior to adding addresses to the virtual interface, therefore if no pods are present, no addresses are added to the nodes.

### Allocating Node Ports
Allocating node ports is often unnecessary for LoadBalancer Services as the LoadBalancer Service will be used for access, not the NodePort.  PureLB supports setting `allocateLoadBalancerNodePorts: false`.

### Address Sharing
By adding a "sharing key" to the service, multiple services can share a single IP address, as long as each service exposes different ports. External Traffic Policy is not supported and therefore ignored for shared addresses. This is necessary as the combination of address sharing and pod locality could result in traffic being presented at a node where KubeProxy had not configured forwarding, which would cause traffic to be dropped.
