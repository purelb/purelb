---
title: "Virtual Network Addresses"
description: "Describe Operation"
weight: 20
hide: [ "toc", "footer" ]
---
Virtual network addresses are more complex to configure than [local addresses](/purelb/how_it_works/localint) but allow for more robust and scalable deployments since more than one node can receive traffic from the upstream routers.

Use cases for virtual addresses include:

* High-performance clusters. Unlike a local address, a virtual address is added to every node so they all receive traffic for that address. Enabling load balancing in the upstream routers allows them to distribute traffic to every node, increasing capacity and redundancy.
* A CNI that uses a routing protocol, such as BGP, to create the CNI network. Larger clusters are often deployed over multiple networks, using BGP routing between nodes and Top-of-Rack switches. In this case, virtual addresses allow LoadBalancer addresses to be combined in the advertisements used by the CNI to construct the network.

To use virtual addresses, create a ServiceGroup whose address pool uses a different subnet than the host interface. Virtual addresses require routing to be accessed, so the cluster must be installed behind network routers, and a [mechanism that advertises routes](/purelb/how_it_works/routers) must be added to the nodes.

Here’s how LBNodeAgent handles virtual addresses:

1. Query the ServiceGroup configuration.  ServiceGroups are chosen by the LoadBalancer Service using the `purelb.io/service-group` annotation; PureLB also annotates the service indicating the IPAM and ServiceGroup allocating the address.
1. Apply the mask from the subnet or aggregation ServiceGroup query.  If aggregation is set to `default` then the subnet mask is used, otherwise the explicitly-specified mask will be applied to the address.  This can be used to further subnet or supernet the address added to the virtual interface.
1. Add the address to the virtual interface (`kube-lb0` by default).  The Linux routing stack automatically computes and applies the correct prefix to the virtual interface.

Here’s an example using a "remotedual" ServiceGroup that has both IPV6 and IPV4 pools:
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: remotedual
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

A LoadBalancer has been created that uses that ServiceGroup (since it has the `purelb.io/service-group: remotedual` annotation). PureLB’s Allocator has assigned both IPv6 and IPv4 addresses to this LoadBalancer, which you can see as `LoadBalancer Ingress: 172.32.100.225, fc00:370:155:0:8000::`:

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
```
You can ssh into each node and verify that 172.32.100.225 and fc00:370:155:0:8000:: have been added to `kube-lb0`:
```plaintext
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

Aggregation is useful for providing additional address management functionality.  Using aggregation the same subnet can be added to multiple ServiceGroups, resulting in a single route being advertised. Conversely, a ServiceGroup can be further subnetted into multiple networks that will be added to the virtual interface, including /32 or /128. This functionality, when combined with routing software on the cluster, enables complete routing address management and forwarding flexibility.

### External Traffic Policy
Virtual address ServiceGroups support both External Traffic Policy modes. When a LoadBalancer is configured with `externalTrafficPolicy: Cluster`, PureLB will add its address to each node's virtual interface. When configured with `externalTrafficPolicy: Local`, however, PureLB will add its address to a node's virtual interface only when one or more of the LoadBalancer's pods are running on that node.

Unlike local addresses, access to virtual addresses is via routing: the next hop is the node's address (resulting in a stable local network) but the routed destination can change, an expected behavior.  However, the forwarding behavior of the upstream routers depends upon how the address has been advertised, and therefore changing External Traffic Policy to `Local` can have no or adverse effects. For example, where a ServiceGroup is using an address range where multiple LoadBalancer addresses from the same range are added to the virtual interface, a single routing advertisement will be made for the subnet containing all of those addresses. Should one of those LoadBalancers be configured for `externalTrafficPolicy: Local` and have no pod running on a host, traffic reaching that host will be lost.

Configuring the aggregator to reduce the size of the the advertised subnet to /32(/128) will result in single routes being advertised and withdrawn for that Service.  While this may seem like a simple solution, there are other implications: many popular routers, for example, will not accept /32 routes over BGP. When correctly used, externalTrafficPolicy, Aggregators, and nodeSelector can provide complete control over how external traffic is distributed.

#### Direct Server Return, Source Address Preservation
`externalTrafficPolicy: Local` has a useful side effect: [Direct Server Return](https://kubernetes.io/docs/tasks/access-application-cluster/create-external-load-balancer/#preserving-the-client-source-ip). Traffic does not transit the CNI, so kubeproxy does not need to NAT, so the source client address is preserved and visible to the pods.
