---
title: "Local Network Addresses"
description: "Describe Operation"
weight: 15
hide: toc, nextpage
---
The local interface mechanism uses standard Linux capabilities to add load-balancer addresses that match the hosts configured subnet as secondary address on the hosts interface.  Using this mechanism a cluster can use the same local address allocation for LoadBalancer services.  When added to an existing physical interface, the Linux Networking stack will respond to ARP/ND messages for that address and the address will be visible on the physical network interface.  

To use a Local Address, create a Service Group that uses the same subnet as the host interface and allocates a number of addresses from that prefix.

PureLB's lbnodeagent uses the following algorithm to identify and add local addresses.

1.  Find the target interface.  The default configuration identified the interface with the default route.  The can be overridden by configuration/
2.  Get the IP Prefix for the host address on the default interface.  This is a simple process for IPv4, however IPv6 requires additional steps as the host address is a /128 and the matching globally routable /64 needs to be identified.  IPv4 and IPv6 have slightly different algorithms
3.  Identify if the address provided by the allocator is part of the host prefix.  If it is not, it is assumed to be a virtual address and allocated to the LoadBalancer virtual interface
4.  Elect a node on the same subnet.  The address can only be applied to a single node on the subnet.  The lbnodeagent elects a node using an algorithm than identifies a single node on the subnet for the address to be added as a secondary.  The algorithm also hashes the service name, so services are distributed over hosts with lbnodeagent running on the same subnet. 
5.  Add the secondary IP address to the interface

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: LocalAddressRange
  namespace: purelb
spec:
  local:
    v4pool:
      aggregation: default
      pool: 192.168.10.225-192.168.10.229
      subnet: 192.168.10.0/24
    v6pool:
      aggregation: default
      pool: fc00:270:154:0:8000::4/126
      subnet: fc00:270:154:0::/64
```
```plaintext

$ kubectl describe service kuard-svc-dual-remote 
Name:                     kuard-svc-dual-remote
Namespace:                adamd
Labels:                   <none>
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: localdual
                          purelb.io/announcing-IPv4: node1,enp1s0
                          purelb.io/announcing-IPv6: node3,enp1s0
                          purelb.io/service-group: localdual
Selector:                 app=kuard
Type:                     LoadBalancer
IP Family Policy:         RequireDualStack
IP Families:              IPv4,IPv6
IP:                       10.152.183.78
IPs:                      10.152.183.78,fd98::2c89
LoadBalancer Ingress:     192.168.10.226, fc00:270:154:0:8000::5
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
Endpoints:                10.1.217.204:8080,10.1.217.205:8080,10.1.238.137:8080
Session Affinity:         None
External Traffic Policy:  Cluster
Events:
  Type    Reason                 Age              From                Message
  ----    ------                 ----             ----                -------
  Normal  AddressAssigned        7s               purelb-allocator    Assigned {Ingress:[{IP:192.168.10.226 Hostname: Ports:[]} {IP:fc00:270:154:0:8000::5 Hostname: Ports:[]}]} from pool localdual
  Normal  ExternalTrafficPolicy  6s               service-controller  Local -> Cluster
  Normal  AnnouncingLocal        5s (x5 over 6s)  purelb-lbnodeagent  Node node3 announcing fc00:270:154:0:8000::5 on interface enp1s0
  Normal  AnnouncingLocal        4s (x6 over 6s)  purelb-lbnodeagent  Node node1 announcing 192.168.10.226 on interface enp1s0


node1:~$ ip -4 addr show dev enp1s0
2: enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP group default qlen 1000
    inet 192.168.10.151/24 brd 192.168.10.255 scope global enp1s0
       valid_lft forever preferred_lft forever
    inet 192.168.10.226/24 brd 192.168.10.255 scope global secondary enp1s0
       valid_lft forever preferred_lft forever


node3:~$ ip -6 addr show dev enp1s0
2: enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP group default qlen 1000
    inet6 fc00:270:154:0:8000::5/64 scope global 
       valid_lft forever preferred_lft forever
    inet6 fc00:270:154::1:f2dc/128 scope global dynamic noprefixroute 
       valid_lft 2144285sec preferred_lft 157085sec
    inet6 fc00:270:154:0:5054:ff:fec9:7073/64 scope global dynamic mngtmpaddr noprefixroute 
       valid_lft 2591814sec preferred_lft 604614sec
    inet6 fc00:270:154::153/64 scope global 
       valid_lft forever preferred_lft forever
    inet6 fe80::5054:ff:fec9:7073/64 scope link 
       valid_lft forever preferred_lft forever




```

### Memberlist
PureLB uses [Memberlist](https://github.com/hashicorp/memberlist) to elect the node where a local network address will be added.  At startup, each lbnodeagent retrieves a list of 5 random nodes where lbnodeagent is running and attempts to connect to those nodes.  In the response, the contacted nodes also inform the new lbnodeagent of other members of the memberlist enabling each lbnodeagent to construct an identical memberlist.  When a service is added or changed, each lbnodeagent runs an invariant sort that retrieves the memberlist, combines the allocated IPADDR  and sorts it into the same order.  The first result of the sort is the winner and the address is allocated to that node.  Memberlist also keeps the list up to date exchanging UDP and TCP messages on port 7934, should the memberlist change, PureLB is notified and the allocation of addresses to nodes is recomputed and local network addresses in use are reallocated to working nodes.

Kubernetes standard pod failure mechanism is not suitable for local network addresses, the POD timeout that indicates that a node has failed would result in lengthy periods of loss of connectivity, memberlist addresses this this problem.


_Note: PureLB does not send GARP messages when addresses are added to interfaces._ 

### External Traffic Policy.  
Service Groups that contain local network addresses ignore External Traffic Policy.  The algorithm will elect a node on the matching local network irrespective of the POD locality.  The reason for this is to prefer connection stability over locality.  Many applications consist of more than one POD for both performance and reliability purposes and those PODs are spread over multiple nodes.  As local Load Balancer addresses can only be applied to a single node on the subnet, POD locality can only match one POD and traffic to other PODs will be distributed by KubeProxy over the CNI irrespective of the use of POD locality.  PODs can move between Nodes for a variety of reasons and k8s is designed to enable PODs to move between nodes, however IP networking prefers a more stable local network environment.  Enabling External Traffic Policy: Local would result in the the local network address moving from one functional node to another functional node, using a network mechanism that implies failure to rapidly update devices on the local network.  By ignoring External Traffic Policy, PODs can move between nodes retaining a stable local IP address for access.  If a service using a pool with local addresses is configured for ExternalTrafficPolicy: Local, PureLB will reset it to Cluster.
