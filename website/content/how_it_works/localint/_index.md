---
title: "Local Network Addresses"
description: "Describe Operation"
weight: 15
hide: [ "toc", "footer" ]
---
The local address mechanism is easy to configure and use. It adds each LoadBalancer's address to one host's network interface. The Linux networking stack then responds to ARP/ND messages for that address and the address is visible using command-line tools. Using this approach, a cluster can use the same address scheme for LoadBalancer services that it uses for host addresses.

Local addresses are a popular choice for homelabs and small deployments but are not as robust or scalable as [virtual addresses](/purelb/how_it_works/virtint/). To use a local address, create a ServiceGroup whose address pool uses the same subnet as the host interface.

Here's how LBNodeAgent adds local addresses:

1. Find the target interface.  By default, PureLB finds the interface with the lowest-cost default route but this can be overridden by configuration.
1. Get the IP prefix for the address on the target interface.  This is a simple process for IPv4, however IPv6 requires additional steps as the host address is a /128 and the matching globally routable /64 needs to be identified.
1. Check that the LoadBalancer address is part of the target interface's subnet.  If not, then it is a [virtual address](/purelb/how_it_works/virtint/).
1. Elect a "winner" node on the subnet. The address can only be applied to a single node on the subnet, so LBNodeAgent chooses that node using an [election algorithm](#memberlist).
1. Add the address as a secondary IP address to the "winner" node's network interface.

Here's an example using a "localdual" ServiceGroup that has both IPV6 and IPV4 pools:

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: localdual
  namespace: purelb-system
spec:
  local:
    v4pools:
    - aggregation: default
      pool: 192.168.10.225-192.168.10.229
      subnet: 192.168.10.0/24
    v6pools:
    - aggregation: default
      pool: fc00:270:154:0:8000::4/126
      subnet: fc00:270:154:0::/64
```

A LoadBalancer has been created that uses that ServiceGroup (since it has the `purelb.io/service-group: localdual` annotation). PureLB's Allocator has assigned both IPv6 and IPv4 addresses to this LoadBalancer, which you can see as `LoadBalancer Ingress: 192.168.10.226, fc00:270:154:0:8000::5`:
```plaintext
$ kubectl describe service kuard-svc-dual-remote
Name:                     kuard-svc-dual-remote
Namespace:                adamd
Labels:                   <none>
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: localdual
                          purelb.io/announcing-IPv4: node1,enp1s0
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
```
PureLB has added a `purelb.io/announcing-IPv4: node1,enp1s0` annotation to the LoadBalancer. This indicates that the LBNodeAgent has elected `node1` as the IPv4 "winner" node, and chosen `enp1s0` as the interface. You can ssh into `node1` and use the Linux `ip` command to show the LoadBalancer's IPV4 address, which is 192.168.10.226/24:
```plaintext
node1:~$ ip -4 addr show dev enp1s0
2: enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP group default qlen 1000
    inet 192.168.10.151/24 brd 192.168.10.255 scope global enp1s0
       valid_lft forever preferred_lft forever
    inet 192.168.10.226/24 brd 192.168.10.255 scope global secondary enp1s0
       valid_lft forever preferred_lft forever
```
The `purelb.io/announcing-IPv6: node3,enp1s0` annotation indicates that `node3` is the IPV6 "winner" and `enp1s0` is the interface. Sure enough, fc00:270:154:0:8000::5/64 has been added to that interface:
```plaintext
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
PureLB uses [Memberlist](https://github.com/hashicorp/memberlist) to elect the node where a local network address will be added. Kubernetes' standard pod failure mechanism is not suitable for local network addresses because the pod timeout that indicates that a node has failed would result in lengthy periods of connectivity loss. Memberlist addresses this this problem and provides much faster recovery from node or pod failures.

At startup, the LBNodeAgent running on each node retrieves a list of 5 random nodes where LBNodeAgent is running and connects to those nodes.  The contacted nodes inform the new LBNodeAgent of other members of the memberlist, so each LBNodeAgent constructs an identical memberlist. When a service is added or changed, each LBNodeAgent retrieves the memberlist, combines the allocated IP address, and sorts it into the same order.  The first node in the sorted list is the "winner" and the address is added to that node.  Memberlist exchanges UDP and TCP messages on port 7934 to keep the list up to date. If the "winner" node has problems, memberlist notifies PureLB, a new "winner" is found, and the local network address is added to it.

### External Traffic Policy
Local address ServiceGroups always use `externalTrafficPolicy: Cluster`.  If a service using a pool with local addresses is configured for `externalTrafficPolicy: Local`, PureLB will reset it to `Cluster`.

Many applications consist of multiple pods, those pods can run on multiple nodes, and K8s sometimes moves pods between nodes. Local addresses can only be applied to a single node so all incoming traffic would reach only that node's pods. `kube-proxy` solves this problem with `externalTrafficPolicy: Cluster`. It distributes traffic over the CNI so the other nodes' pods can contribute.

Enabling `externalTrafficPolicy: Local` would result in the the local network address moving from node to node, which would result in an unstable, poor-performing network.  By using only `externalTrafficPolicy: Cluster`, pods on all nodes can handle requests and the network retains a stable local IP address.
