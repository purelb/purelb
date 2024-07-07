---
title: "Using Routers"
description: "Describe Operation"
weight: 35
hide: [ "toc", "footer" ]
---


Where the cluster spans multiple subnets routing is often used to provide connectivity between hosts and pod (CNI network).  Certain CNIs add routing by default while others leave routing software choice and implementation to the user. 

The Routing software enables the distribution of LoadBalancer addresses to other network devices.  This is achieved using a routing software function called redistribution.

{{<mermaid align="center">}}

  graph BT;
    
    A(Router/Switch);
    A---C4;
    A---D4;
    C3-.-|Advertizes IPNET|A;
    D3-.-|Advertizes IPNET|A;
    subgraph k8s-Node-1
      C1-->|add ipnet|C2;
      C2-->|imports to kube-lb0|C3;
      C1(PureLB-Node);  
      C3(Bird);
      subgraph linux-networking
         C4[eth0];
         C2[kube-lb0];
         C5[ipvs0];
         C6[cni0];
      end
    end
    subgraph k8s-Node-2
      D1-->|add ipnet|D2;
      D2-->|imports from kube-lb0|D3;
      D1(PureLB-Node);
      
      D3(Bird);
     
      subgraph linux-networking
         D4[eth0];
         D2[kube-lb0];
         D5[ipvs0];
         D6[cni0];

      end
    end
   

{{</mermaid>}}

PureLB adds the allocated addresses to kube-lb0 as IP Networks (192.168.100.1/24) therefore creating IPNET entries on the interface and routing entries in the routing table.  Redistribution dynamically copies routing information between different routing protocols and the Linux kernel routing table is a source.  The routing software "redistributes" entries from the linux routing table using some form of selector, in this case the linux interface the route is attached.  The PureLB-Node adds an additional interface to linux, kube-lb0, and linux can route from its other interfaces to kube-lb0 as well as other interface created by the CNI and kubeProxy in the case of IPVS mode.  The redistributed destinations are distributed according to the routes protocol configuration.


## Routing and K8s
The routing infrastructure should be designed to include k8s nodes as network devices.  In addition to the normal network topology questions, k8s presents another important question, should the routing software be run in a container or natively on the underlying host.  When routing software is run in a k8s cluster and the container is configured with _hostNetwork: true_, the routing software will have access to the host network interfaces. The decision to run routing in a container or host depends on when access to an interface is needed via routing.  If your cluster uses multiple network interfaces on each host for different roles, mgmt, service, storage, and the routing software is running in a container, access will be limited to interfaces via the default route until the cluster is operational.  If the routing software is run on the host directly, routing will be active prior to the cluster starting and therefore access via routing will be available.  The decision does not impact k8s operation, each choice has tradeoffs.


### BIRD
Bird is a popular open source linux routing software.  Its used by Calico (see specific configuration example) and can be integrated with any k8s network.   BIRD has a protocol called _Direct_.  This protocol is used to redistribute routes from directly connected networks identified by a list of interfaces.  Once in the BIRD routing table, they can be advertized using protocols supported by BIRD such as OSPF or BGP by exporting **RTS_DEVICE**.  The configuration snippit for the _Direct_ protocol is and an example for the desired protocol.

```plaintext
protocol direct {
      ipv4;
      ipv6;
      interface "kube-lb0";
    }

export where source ~ [ RTS_STATIC, RTS_BGP, RTS_DEVICE ];
```
Note:  The PureLB repo includes BIRD packaged and configured when routing is required and the cluster is not using routing software as part of the CNI.  It includes a usable configuration.


### FRR
Free Range Routing (FRR) is another popular open source linux routing software alternative.  Its has a more _traditional_ configuration style so will be more familiar for some engineers  and it also has more implemented routing protocols.  To import routes from a linux interface, a specific protocol is chosen to have the routes distributed and a route map is used to select the interface,  other routing protocols then receive routes from that protocol.  An example of redistribution into bgp is:

```plaintext
router bgp 65552
 neighbor 172.30.250.1 remote-as 65552
 !
 address-family ipv4 unicast
  redistribute connected route-map k8slb
 exit-address-family

!
route-map k8slb permit 10
 match interface kube-lb0
!
route-map k8slb deny 20
```

### How Routers load balance
Routers are often part of the Load Balancing structure, a router load balances using Equal Cost Multipath (ECMP).  Routers can have multiple _routes_ to the same destination via different next hop addresses.  When the cost of those route is equal they are candidates for ECMP.  Router defaults vary, but when enabled, the router will select all equal cost routes upto its maximum equal path count and distribute traffic over those paths.

```plaintext
Codes: K - kernel route, C - connected, S - static, R - RIP,
       O - OSPF, I - IS-IS, B - BGP, E - EIGRP, N - NHRP,
       T - Table, v - VNC, V - VNC-Direct, A - Babel, D - SHARP,
       F - PBR, f - OpenFabric,
       > - selected route, * - FIB route, q - queued route, r - rejected route

K>* 0.0.0.0/0 [0/0] via 172.30.255.1, enp1s0, 3d01h36m
B>* 10.0.8.0/24 [20/0] via 172.30.255.1, enp1s0, 3d01h36m
B>* 10.0.8.2/32 [20/1] via 172.30.255.1, enp1s0, 3d01h36m
B>* 98.179.160.0/23 [20/1] via 172.30.255.1, enp1s0, 19:56:03
O   172.30.250.0/24 [110/1] is directly connected, enp6s0, 3d01h36m
C>* 172.30.250.0/24 is directly connected, enp6s0, 3d01h36m
C>* 172.30.255.0/24 is directly connected, enp1s0, 3d01h36m
B>* 172.31.1.0/32 [20/0] via 172.30.250.101, enp6s0, 00:01:21
  *                      via 172.30.250.102, enp6s0, 00:01:21
  *                      via 172.30.250.105, enp6s0, 00:01:21
O   172.31.1.0/32 [110/10000] via 172.30.250.101, enp6s0, 00:01:21
                              via 172.30.250.102, enp6s0, 00:01:21
                              via 172.30.250.105, enp6s0, 00:01:21
B>* 172.31.1.1/32 [20/0] via 172.30.250.101, enp6s0, 00:00:02
  *                      via 172.30.250.102, enp6s0, 00:00:02
  *                      via 172.30.250.103, enp6s0, 00:00:02
  *                      via 172.30.250.104, enp6s0, 00:00:02
  *                      via 172.30.250.105, enp6s0, 00:00:02
O   172.31.1.1/32 [110/10000] via 172.30.250.101, enp6s0, 00:00:02
                              via 172.30.250.102, enp6s0, 00:00:02
                              via 172.30.250.103, enp6s0, 00:00:02
                              via 172.30.250.104, enp6s0, 00:00:02
                              via 172.30.250.105, enp6s0, 00:00:02
B>* 192.168.100.0/24 [20/1] via 172.30.255.1, enp1s0, 19:56:03
B>* 192.168.151.0/24 [20/1] via 172.30.255.1, enp1s0, 3d01h36m
```

In the example above, 172.31.1.1/32 has 5 equal cost routes indicated by _*_ showing that they are Forwarding Information Base routes.  Note this router is receiving these routes via two protocols, in this case the cluster has been configured with OSPF and BGP, BGP will take precedence therefore it provides the selected and FIB routes.

Depending on the router and its configuration, load balancing techniques will vary however they are all generally based upon a 4 tuple hash of sourceIP, sourcePort, destinationIP, destinationPort.  The router will also have a limit to the number of ECMP paths that can be used, in modern TOR switches, this can be set to a size larger than a /24 subnet, however in old routers, the count can be less than 10.  This needs to be considered in the infrastructure design and PureLB combined with routing software can help create a design that avoids this limitation.  Another important consideration can be how the router load balancer cache is populated and updated when paths are removed, again modern devices provide better behavior.



