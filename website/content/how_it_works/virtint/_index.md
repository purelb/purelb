---
title: "Virtual Network Addresses"
description: "Describe Operation"
weight: 20
hide: [ "toc", "footer" ]
---

The Virtual Internet is important for clusters where a new Prefix is used for LoadBalancer services.  These addresses are added to the cluster and require routing to be accessed.  Use cases for this configuration are:

1. Cluster is installed behind network routers.  Where the cluster is installed behind network routers, this mechanism can be used to have the addresses added to the virtual interface dynamically advertised using any network routing protocol.
2. A CNI that uses a routing protocol, such as BGP to create the CNI network.  Larger clusters are often deployed over multiple networks with BGP routing used between nodes and Top-of-Rack switches.  In this case, the Virtual interface mechanism allows LoadBalancer addresses to be combined in advertisements used by the CNI to construct the network.
3.  Scaling & Redundancy.  Unlike a local address, an allocated virtual address is added to every node and that node can be advertised as a nexthop to the allocated address.  By enabling load balancing in the upstream router(s) the router can distribute the traffic among the nodes advertising the address which increases capacity and redundancy.

When PureLB identifies that the address provided by the Service Group is not part of a local interface subnet, it undertakes the following steps:

1. Query the SG configuration.  Service Groups are identified in the Load Balancer Service definition using the `purelb.io/service-group` annotation; PureLB also annotates the service indicating the IPAM and Service Group allocating the address.
2. Apply the mask from the subnet or aggregation service group query.  If aggregation is set to default the subnet network mask is used, otherwise the explicitly-specified mask will be applied to the address.  This can be used to further subnet or supernet the address added to the virtual interface.
3.  Add the address to the virtual interface, _kube-lb0_ by default.  The Linux routing stack automatically computes and applies the correct prefix to the virtual interface.

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: virtual-address-range
  namespace: purelb
spec:
  local:
    v4pools:
    - aggregation: /32
      pool: 172.32.100.225-172.30.100.229
      subnet: 172.32.100.0/24
    v6pools:
    - aggregation: /128
      pool: fc00:370:155:0:8000::/126
      subnet: fc00:370:155::/64
```

```plaintext
$ kubectl describe service kuard-svc-dual-remote 
Name:                     kuard-svc-dual-remote
Namespace:                test
Labels:                   <none>
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: remotedual
                          purelb.io/service-group: remotedual
Selector:                 app=kuard
Type:                     LoadBalancer
IP Family Policy:         RequireDualStack
IP Families:              IPv4,IPv6
IP:                       10.152.183.31
IPs:                      10.152.183.31,fd98::b919
LoadBalancer Ingress:     172.32.100.225, fc00:370:155:0:8000::
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
Endpoints:                10.1.217.204:8080,10.1.217.205:8080,10.1.238.137:8080
Session Affinity:         None
External Traffic Policy:  Cluster
Events:
  Type    Reason              Age              From                Message
  ----    ------              ----             ----                -------
  Normal  AddressAssigned     5s               purelb-allocator    Assigned {Ingress:[{IP:172.32.100.225 Hostname: Ports:[]} {IP:fc00:370:155:0:8000:: Hostname: Ports:[]}]} from pool remotedual
  Normal  AnnouncingNonLocal  4s (x3 over 4s)  purelb-lbnodeagent  Announcing 172.32.100.225 from node node2 interface kube-lb0
  Normal  AnnouncingNonLocal  4s (x3 over 4s)  purelb-lbnodeagent  Announcing 172.32.100.225 from node node1 interface kube-lb0
  Normal  AnnouncingNonLocal  4s (x3 over 4s)  purelb-lbnodeagent  Announcing fc00:370:155:0:8000:: from node node2 interface kube-lb0
  Normal  AnnouncingNonLocal  4s (x3 over 4s)  purelb-lbnodeagent  Announcing fc00:370:155:0:8000:: from node node1 interface kube-lb0
  Normal  AnnouncingNonLocal  4s (x3 over 4s)  purelb-lbnodeagent  Announcing 172.32.100.225 from node node3 interface kube-lb0
  Normal  AnnouncingNonLocal  4s (x3 over 4s)  purelb-lbnodeagent  Announcing fc00:370:155:0:8000:: from node node3 interface kube-lb0


node1:~$ ip addr show dev kube-lb0
25: kube-lb0: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether 16:5f:c1:ff:9c:b3 brd ff:ff:ff:ff:ff:ff
    inet 172.32.100.225/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet6 fc00:370:155:0:8000::/128 scope global 
       valid_lft forever preferred_lft forever
    inet6 fe80::145f:c1ff:feff:9cb3/64 scope link 
       valid_lft forever preferred_lft forever

node2:~$ ip addr show dev kube-lb0
33: kube-lb0: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether 7a:a3:f5:06:fd:85 brd ff:ff:ff:ff:ff:ff
    inet 172.32.100.225/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet6 fc00:370:155:0:8000::/128 scope global 
       valid_lft forever preferred_lft forever
    inet6 fe80::78a3:f5ff:fe06:fd85/64 scope link 
       valid_lft forever preferred_lft forever


node3:~$ ip addr show dev kube-lb0
39: kube-lb0: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether 66:57:a0:1a:cf:d5 brd ff:ff:ff:ff:ff:ff
    inet 172.32.100.225/32 scope global kube-lb0
       valid_lft forever preferred_lft forever
    inet6 fc00:370:155:0:8000::/128 scope global 
       valid_lft forever preferred_lft forever
    inet6 fe80::6457:a0ff:fe1a:cfd5/64 scope link 
       valid_lft forever preferred_lft forever
```

The configured aggregator is useful for providing additional address management functionality.  For example, multiple service groups with subnets that can be aggregated into a single single address advertisement can be defined.  By setting the aggregator, a single subnet can be added to multiple service groups resulting in a single route being advertised.  Conversely, a Service Group can be further subnetted into multiple networks that will be added to the virtual interface, including /32 or /128.   This functionality, when combined with Routing Software on the cluster enables complete routing address management and forwarding flexibility.


### External Traffic Policy
A Service Group with an address that is applied to the Virtual Interface supports External Traffic Policy.  When configured with _External Traffic Policy: Cluster_, PureLB will only add the LoadBalancer address to the virtual interface (kube-lb0) when a pod is located on the node and remove the address if the pod is not longer on the node.  Unlike a Service Group with a local address, access to these addresses is via the routing.  Put simply, the next hop is the node's local address resulting in a stable local network and the routed destination is changing, an expected behavior.  However, the forwarding behavior of the upstream routers depends upon how the address has been advertised, and therefore changing External Traffic Policy to Local can have no or adverse effects.  For example where a Service Group is using a address range where multiple addresses from the same range are added to the virtual interface a single routing advertisement will be made for the subnet containing both those addresses.  Should one of those services be configured for External Traffic Policy: Local and no pod present traffic reaching that pod will be discarded.  Configuring the aggregator to reduce the size of the the advertised subnet to /32(/128) will result in single routes being advertised and withdrawn for that Service.  While this may seem like a simple solution, there are also other implications, for example many popular routers will not accept /32 routes over BGP.  When correctly used, externalTrafficPolicy, Aggregators, and nodeSelector can provide complete control over how external traffic is distributed.

#### Direct Server Return, Source Address Preservation
_External Traffic Policy: Local_ has another useful side effect: Direct Server Return.  Traffic does not transit the CNI, kubeproxy does not need to NAT, therefore the source client address is preserved and visible to the pod(s).  
