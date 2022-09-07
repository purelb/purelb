---
title: "Overview"
description: "High Level Overview"
weight: 10
mermaid: true

hide: toc, nextpage
---


The PureLB LoadBalancer controller consists of two components that interact with the k8s API server.  These two components are:


 * **Allocator.**  The allocator watches the service API for LoadBalancer service events and allocates IP addresses

 * **LBnodeagent.**  The lbnodeagent runs on all nodes that packets for exposed services can transit, it watches service changes and configures networking behavior

 * **KubeProxy.** KubeProxy is important, but not part of PureLB.  KubeProxy watches service changes and adds the same addresses used by lbnodeagent to configure communication
 within the cluster.  

 {{% notice %}} _Instead of thinking of PureLB as advertising services, think of PureLB as attracting packets to allocated addresses with KubeProxy forwarding those packets within the cluster via
 the Container Network Interface Network (POD Network) between nodes._ {{% /notice %}}

{{<mermaid align="center">}}

  graph TD;
    A(allocator<br/>Deployment);
    B(kubeAPI);
    C(lbnodeagent<br/>Daemonset);
    D(kubeProxy<br/> Daemonset);
    E[kubectl expose ....];  
    A---B;
    B---C;
    E-->B;
    B---D;
{{</mermaid>}}


The PureLB allocator watches the k8s service API for services of type LoadBalancer with IP address set to nil. It responds with an address from a Service Group (Local IPAM) or sends a request for an IP address to the configured IPAM system.  The address returned is used by the Allocator to update the k8s service.

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

The Allocator is configured with a minimum of one "default" Service Group. Multiple service groups can be defined and are accessed using annotations.

* Service Group.  A Service Group represents an IPAM mechanism and its necessary configuration parameters.

When using the Allocators inbuilt IPAM each Service group can contain an address pool for IPv4 and/or IPv6, one is required.


 * v4pool.  Contains the IPv4 range for allocation
 * v6pool.  Contains the IPv6 range for allocation


Each pool conIn the case of the Allocator's inbuilt IPAM a Service Group consists of the following:

 * Name:  The name of the Service Group referenced by annotations in the service definition.  The default service group is used when no annotation is present.
 * local:  Indicates that PureLB will allocate these address from the information contained in the service group.
 * ipv4pool:  A container for a pool of IPv4 addresses
 * ipv6pool:  A container from a pool of IPv6 addresses
 * Subnet:  In the form of CIDR, the network that addresses are allocated
 * Pool:  The specific addresses to be allocated in the form of a range or CIDR
 * Aggregation:  Where the address is not local, this allows subnet to be aggregated

 Each Service Group is a Custom Resource (sg) and is namespaced.

```yaml
apiVersion: "purelb.io/v1"
kind: ServiceGroup
metadata:
  name: localaddrange
  namespace: purelb
spec:
 local:
    v4pool:
      aggregation: default
      pool: 192.168.10.240-192.168.10.243
      subnet: 192.168.10.0/24
    v6pool:
      aggregation: default
      pool: fc00:270:154:0:8100::4/64
      subnet: fc00:270:154::/64
```

Kubernetes support Dual Stack by default, therefore the section of IPv4, IPv6 or both are configured in the service.  Based upon the service configuration the allocator will provide an address from the requested address families.

Once the Allocator has provided address(es) (it will be visible in the service), the service is updated. lbnodeagent and KubeProxy watch the Service API.

KubeProxy makes the necessary configuration changes so when traffic arrives at nodes with the allocated destination address, it is forwarded to the correct POD(s). If KubeProxy is operating in 
its default mode, it will configure IPtables to match the allocated address before Linux routing (which would drop the packet) and forward to the Nodeport address. If configured in IPVS mode, the external address is added to the IPVS tables and the IPVS virtual interface.  

The lbnodeagent converts the address into an IPNet (192.168.100.1/24) and configures Linux Networking so other network devices can use the allocated address.  In PureLB the network configuration undertaken depends upon the allocated IPNET.

### Local Addresses  
Each Linux host is connected to a network and therefore has a CIDR address.  A Local address is a range of addresses that matches the subnet of that local address.  For example

> _DHCP allocates the IPv4 address 192.168.100.10/24 from an address pool of 192.168.100.2-192.168.100.100.  If a Service Group was created that used the same subnet, 192.168.100.0/24 with a pool of 192.168.100.200-192.168.100.250, the allocated address would be considered local_

The lbnodeagent identifies the interface with that subnet, elects a single node on that subnet and then adds it to the physical interface on that node.


### Virtual Addresses  
PureLB can use a range of addresses that is not currently in use in the cluster, these addresses are considered virtual addresses.  When the lbnodeagent receives a service with this _virtual address_, it adds that address to a virtual interface called _kube-lb0_.  This virtual interface is used in conjunction with routing software to advertize routes to these addresses to other routers to provide connectivity.  Any routing protocol or topology can be used based up the routing softwares capabilities.

The process of adding IP Addresses to either the _local physical interface_ or _virtual interface_ is algorithmic and undertaken by lbnodeagent. Its easy to see what addresses are allocated to interfaces that information is added to the service and can be viewed on the host using standard Linux iproute2 tools to show addresses.

Virtual addresses and local addresses can be used concurrently, there is no configuration other than adding the appropriate addresses to service groups.

### IPv6 Dual Stack
PureLB supports IPv6.  During the development of PureLB, the team made a specific effort to support IP address families in as simple and streamlined manner as possible.  With the advent of Dual Stack, the use of IPv4 and IPv6 address is integrated with Kubernetes. From a PureLB user perspective, the allocation behavior is very similar, however the _lbnodeagent_ does elect local interfaces independently for IPv4 and IPv6, therefore addresses can appear on different nodes.  A Cluster and CNI supporting Dual Stack IPv6 is required. 
    
### External Traffic Policy 
 LoadBalancer Service can be configured with an External Traffic Policy.  Its  purpose is to control how the distribution of external traffic in the cluster and requires support from the LoadBalancer controller to operator.  The default setting, Cluster is used to implement forwarding to POD's over the CNI network.  Any node can receive traffic, the node receiving the traffic distributes traffic to POD(s). Cluster mode depends on KubeProxy to distribute traffic to the correct POD(s) and load balances traffic among all POD(s).  The Local setting is used to constrain the LoadBalancer to send traffic to nodes that are running the target POD(s) only resulting in traffic not traversing the CNI.  **As traffic does not transit the CNI there is no need for kubeProxy to NAT, therefore the original Source IP address is retained**.  External Traffic Policy can be a useful tool in k8s edge design, especially when additional forms of load balancing are added using Ingress Controllers or a Service Mesh to further control which hosts receive traffic for distribution to PODs, consideration of network design is recommended before using this feature.  
 
 External Traffic Policy is ignored for Local Addresses. Virtual addresses support _ExternalTrafficPolicy: Local_ behavior.  
 
 PureLB service behavior is consistent with k8s when setting _ExternalTrafficPolicy: Cluster_, the address is added irrespective of the state of the POD identified in the selector.  However, when set to _ExternalTrafficPolicy: Local_, PureLB must identify if there are any PODs on the node prior to adding addresses to the virtual interface, therefore if no PODs are present, no addresses are added to the nodes. 

### Allocating Node Ports
Allocating node ports is often unnecessary for in LoadBalancer Services as the LoadBalancer Service will be used for access, not the NodePort.  PureLB supports setting allocateLoadBalancerNodePorts to false.


### Address Sharing
By adding a key to the service, multiple services can share a single IP address, as long as each service exposes different ports. External Traffic Policy is not supported and therefore ignored for shared addresses. This is necessary as the combination of address sharing and POD locality could result in traffic being presented at a node where kubeproxy had not configured forwarding which would cause traffic to be dropped.
