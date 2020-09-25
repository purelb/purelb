---
title: "Local Network Addresses"
description: "Describe Operation"
weight: 15
hide: toc, nextpage
---
The local interface mechanism uses standard Linux capabilities to add load-balancer addresses that match the hosts configured subnet as secondary address on the hosts interface.  Using this mechanism a cluster can use the same local address allocation for LoadBalancer services.  When added to an existing physical interface, the Linux Networking stack will respond to ARP/ND messages for that address and the address will be visible on the physical network interface.  

To use a Local Address, create a Service Group is created that uses the same subnet as the host interface and allocates a number of addresses from that IPNET.

PureLB's lbnodeagent uses the following algorithm to identify and add local addresses.

1.  Find the default interface.  The default interface is the interface with the default route  (note that allocation will fail if more than one default route has been installed)
2.  Get the IPNET for the host address on the default interface.  This is a simple process for IPv4, however IPv6 requires additional steps as the host address is a /128 and the matching globally routable /64 needs to be identified.  IPv4 and IPv6 have slightly different algorithms
3.  Identify if the address provided by the allocator is part of the host IPNET.  If it is not it is assumed to be a virtual address and allocated to the LoadBalancer virtual interface
4.  Elect a node on the same subnet.  The address can only be applied to a single node on the subnet.  The lbnodeagent elects a node using an algorithm than identifies a single node on the subnet for the address to be added as a secondary.  The algorithm also hashes the service name, so services are distributed over hosts with lbnodeagent running on the same subnet. 
5.  Add the secondary IP address to the interface

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
```plaintext
$ kubectl describe service echoserver-service1
Name:                     echoserver-service1
Namespace:                default
Labels:                   app=echoserver1
Annotations:              purelb.io/allocated-by: PureLB
                          purelb.io/allocated-from: localaddrange
                          purelb.io/added-to: node1/enp1s0
Selector:                 app=echoserver1
Type:                     LoadBalancer
IP:                       10.102.207.235
LoadBalancer Ingress:     192.168.151.240
Port:                     <unset>  80/TCP
TargetPort:               8080/TCP
NodePort:                 <unset>  32416/TCP
Endpoints:                10.128.2.67:8080
Session Affinity:         None
External Traffic Policy:  Cluster
Events:                   <none>

node1# ip -4 addr show dev enp1s0
2: enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP group default qlen 1000
    inet 192.168.151.140/24 brd 192.168.151.255 scope global dynamic enp1s0
       valid_lft 6454sec preferred_lft 6454sec
    inet 192.168.151.240/24 brd 192.168.151.255 scope global secondary enp1s0
       valid_lft forever preferred_lft forever
```
Should a node fail, PureLB-Node is notified that the list of nodes that are candidates for election has changed and a new node will be elected.  The LBNode algorithm is invariant (everybody computes the same result), therefore the change will result in a consist configuration.  By default PureLB sends a  Gratuitous ARP (GARP)/unsolicited Neighbor Advertisement (NA) when these events occur to update other devices on the network immediately.  As services are used to provide access into the cluster, updating other devices as quickly as possible is logical, if you disagree it can be disabled.  This is a common mechanism used by other linux high availability mechanisms

### External Traffic Policy.  
Service Groups that contain local network addresses ignore External Traffic Policy.  The algorithm will elect a node on the matching local network irrespective of the POD locality.  The reason for this is to prefer connection stability over locality.  Many applications consist of more than one POD for both performance and reliability purposes and those PODs are spread over multiple nodes.  As local Load Balancer addresses can only be applied to a single node on the subnet, POD locality can only match one POD and traffic to other PODs will be distributed by KubeProxy over the CNI irrespective of the use of POD locality.  PODs can move between Nodes for a variety of reasons and k8s is designed to enable POD's to move between nodes, however IP networking prefers a more stable local network environment.  Enabling External Traffic Policy: Local would result in the the local network address moving from one functional node to another functional node, using a network mechanism that implies failure to rapidly update devices on the local network.  By ignoring External Traffic Policy, PODs can move between nodes retaining a stable local IP address for access.  If a service using a pool with local addresses is configured for ExternalTrafficPolicy: Local, Purelb will reset it to Cluster.
