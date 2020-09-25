---
title: "Virtual Network Addresses"
description: "Describe Operation"
weight: 20
hide: toc, nextpage
---


The Virtual Internet is important for clusters where a new IPNET is used for LoadBalancer services.  These addresses are added to the cluster and require routing to be accessed.  Use cases for this configuration are:

1. Cluster is installed behind network routers.  Where the cluster is installed behind network routers, this mechanism can be used to have the addresses added to the virtual interface dynamically advertized using any network routing protocol.
2. A CNI is used that uses a routing protocol, such as BGP to create the CNI network.  Larger clusters are often deployed over multiple networks with BGP routing used between nodes and Top-of-Rack switches.  There Virtual interface mechanism allows LoadBalancer addresses to be combined in advertisements used by the CNI to construct the network.
3.  Scaling & Redundancy.  Unlike a local address, an allocated virtual address is added to every node and that node can be advertized as a nexthop to the allocated address.  By enabling load balancing in the upstream router(s) the router can distribute the traffic among the nodes advertizing the address increasing capacity and redundancy

When PureLB identifies that the address provided by the Service Group is not part of a local interface subnet, it undertakes the following steps

1. Query the SG configuration.  Service Groups are identified in the Load Balancer Service definition using the annotation , however the PureLB also annotates the service indicating the IPAM and Service Group allocating the address.
2. Apply use Network address from Network or apply aggregate.  If the aggregate is set to to default the subnet network mask is used, if the aggregate is set it will be applied to the address.  This can be used to further subnet or supernet the address added to the virtual interface.
3.  Apply the address to the virtual interface, kube-lb0 by default.  The Linux routing stack automatically computes and applies the correct IPNET to the virtual interface.


```yaml
apiVersion: "purelb.io/v1"
kind: ServiceGroup
metadata:
  name: VirtualAddressRange
  namespace: purelb 
spec:
  name: 'virtaddrange'
  subnet: '172.22.0.0/24'
  pool: '172.22.0.10-172.22.0.25'
  aggregation: 'default'
```
```plaintext
$ k describe service echoserver-service22
Name:                     echoserver-service22
Namespace:                default
Labels:                   app=echoserver1
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: virtualaddrange
Selector:                 app=echoserver1
Type:                     LoadBalancer
IP:                       10.105.255.29
LoadBalancer Ingress:     172.22.0.11
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  30815/TCP
Endpoints:                10.128.2.67:8080
Session Affinity:         None
External Traffic Policy:  Cluster
Events:                   <none>

node1# ip -4 addr show dev kube-lb0 
4: kube-lb0: <BROADCAST,NOARP> mtu 1500 qdisc noop state UP group default qlen 1000
    inet 172.22.0.11/24 brd 172.22.0.255 scope global kube-lb0
       valid_lft forever preferred_lft forever
```

The configured aggregator is useful for providing additional address management functionality.  For example, multiple  service groups with subnets that can be aggregated into a single single address advertisement can be defined and by setting the aggregator a single subnet can is added multiple service groups resulting in a single route being advertised.  Conversely, a Service Group can be further subnetted into multiple networks that will be added to the virtual interface, including /32.   This functionality, when combined with Routing Software on the cluster enables complete routing address management and forwarding flexability


### External Traffic Policy:  
A Service Group with an address that is applied to the Virtual Interface supports External Traffic Policy.  When configured with _External Traffic Policy: Cluster_, PureLB adds the address to every node relying on k8s to send traffic to the destination PODs and load balancing as necessary. When configured for _External Traffic Policy: Local_  PureLB will only add the Load Balancer address to the virtual interface (kube-lb0) when a POD is located on the node and remove the address if the POD is not longer on the node.  Unlike a Service Group with a local address, access to these addresses is via the routing.  Put simply, the next hop which is the nodes local address resulting in a stable local network and the routed destination is changing, an expected behavior.  However, the forwarding behavior of the upstream routers depends upon how the address has been advertised, and therefore changing External Traffic Policy to Local can have no or adverse effects.  For example where a Service Group is using a address range where multiple addresses from the same range are added to the virtual interface a single routing advertisement will be made for the subnet containing both those addresses.  Should one of those services be configured for External Traffic Policy: Local and no POD present traffic reaching that POD will be discarded.  Configuring the aggregator to reduce the size of the the advertised subnet to /32(/128) will result in single route being advertized and withdrawn for that Service.  While this may seem like a simple solution, there are also other implications, for example many popular routers will not accept /32 routes over BGP.  When correctly used externalTrafficPolicy, Aggregators and nodeSelector can provide the complete flexibility of how external traffic is distributed.

#### Source Address Preservation
_External Traffic Policy: Local_ has another useful side effect, because traffic does not transit the CNI, kubeproxy does nto need to NAT, therefore the source client address is preserved and visible to the POD(s).  


