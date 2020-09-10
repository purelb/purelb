---
title: "Overview"
description: "High Level Overview"
weight: 10
mermaid: true

hide: toc, nextpage
---


The PureLB LoadBalancer controller consists of two components that interact with the k8s API server.  These two compoenents are:


 * **Allocator.**  The allocator watches the service API for LoadBalancer service and replies with an IP address

 * **LBnode.**  The LBNode runs on all nodes that packets for exposes services can transit, it watches service changes and configures networking behavior

 * **KubeProxy.** Important, put not part of PureLB is KubeProxy.  KubeProxy is also watching service changes and adds those the same addresses used by LBNode to configure communication
 within the cluster.  

 {{% notice %}} _Instead of thinking of PureLB advertising service, think of PureLB attracting packets to allocated addresses with KubeProxy forwarding those packets within the cluster via
 the Container Network Interface Network (POD Network) between nodes._ {{% /notice %}}




{{<mermaid align="center">}}

  graph TD;
    A(PureLB Allocator<br/>Deployment);
    B(kubeAPI);
    C(PureLB NodeLB<br/>Daemonset);
    D(kubeProxy<br/> Daemonset);
    E[kubectl expose ....];  
    A---B;
    B---C;
    E-->B;
    B---D;
{{</mermaid>}}


The PureLB allocator watches the k8s service API for services of type LoadBalancer with IP address set to nil. It sends a request for an IP address to the configured IPAM systems.  The address returned from IPAM is used by the Allocator to update the k8s service. The PureLB allocation also has a built in IPAM.  

To use a Service Load Balancer the service type is set to LoadBalancer. 

```yaml

apiVersion: v1
kind: Service
metadata:
  name: echoserver-service1
  namespace: default
  annotations:
      purelb.io/address-pool: localaddrange
spec:
  externalTrafficPolicy: Cluster
  ports:
  - port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: echoserver1
  type: LoadBalancer

```

The Allocator is configured with a minimum of one "default" Service Group,  multiple service groups can be defined and are accessed using annotations.

* Service Group.  A Service Group represents an IPAM mechanism and its necessary configuration parameters.

In the case of the Allocators inbuilt IPAM a Service Groups consists of the following

 * Name:  The name of the Service Group referenced by annotations in the service defination.  The default service group is used when no annotation is present.
 * Subnet:  In the form of CIDR, the network that addresses are allocated
 * Pool:  The specific addresses to be allocated in the form of a range or CIRD
 * Aggregration:  Where the address is not local, this allows subnet to be aggregated

 Each Service Group is a Custom Resource (sg) and is namespaced.

```yaml
apiVersion: "purelb.io/v1"
kind: ServiceGroup
metadata:
  name: LocalAddressRange
  namespace: purelb 
spec:
  name: 'localaddrange'
  subnet: '192.168.151.0/24'
  pool: '192.168.151.240-192.168.151.250'
  aggregation: 'default'
```


Once the Allocator has provide an address (it wll be visable in the service), the service is updated,  LBNode and KubeProxy is watching the Service API.  

KubeProxy makes the necessary configuration changes so when traffic arrives at nodes with the allocated destination address, it is forwarded to the correct POD(s). If KubeProxy is operating in 
its default mode, it will configure IPtables to match the allocated address before Linux routing (which would drop the packet) and forward to the Nodeport address, if configured in IPVS mode, the external address is added to the IPVS tables and the IPVS virtual interface.  

The PureLB-Node converts the address into an IPNet (192.168.100.1/24) and configures Linux Networking so other network devices can use the allocated address.  In PureLB the network configuration undertaken depends upon the allocated IPNET.

### Local Addresses.  
Each Linux host is connected to a network and therefore has a CIDR address.  A Local address is a range of addresses that matches the subnet of that local address.  For example

> _DHCP allocates the address 192.168.100.10/24 from an address pool of 192.168.100.2-192.168.100.100.  If a Service Group was created that used the same subnet, 192.168.100.0/24 with a pool of 192.168.100.200-192.168.100.250, the allocated address would be considered local_

LBnode identifies the interface with that subnet, elects a single node on that subnet and then adds it to the physical interface on that node.


### Virtual Addresses.  
PureLB can use a range of addresses that is not currently in use in the cluster, these addresses are considered virtual addresses.  When the node recieves a service with this a Virtual
Address, it adds that address to a virtual interface called kube-lb0.  This virtual interface is used in conjuction with routing software to advertize routes to these addresses to other routes to provide connectivity.  Any routing protocol or configuration can be used based up the routing softwares cababilites.

The process of adding IP Addresses to either the local physical interface or virtual interface is algorithmic and undertaken by LBnode. Its easy to see what addresses are allocated to interfaces, simply use standing Linux iproute2 tools to show addresses.
    
### External Traffic Policy. 
 LoadBalancer Service can be configured with an External Traffic Policy.  Its  purpose is to control how the distribution of external traffic within the cluster and requires support from the LoadBalancer controller to operator.  The default, Cluster is used to implement forwarding to POD's over the CNI network, therefore the node recieving the traffic does not need to have a POD for that service running on that node.  The Local setting is used to constrain the LoadBalancer to send traffic to nodes that are running the target POD(s) only resulting in traffic not traversing the CNI except during transient failure.  In Cluster mode, traffic distribution depends on KubeProxy to distribute traffic to the correct POD(s) and implements load balancing.  External Traffic Policy can be a useful tool in k8s edge design, especially when additional forms of load balancing are added using Ingress Controllers or a Service Mesh to futher control which hosts recieve traffic for distribution to PODs, consideration of network design is recommended before using this feature.






