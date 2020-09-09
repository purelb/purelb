---
title: "Using Routers"
description: "Describe Operation"
weight: 35
hide: toc, nextpage
---


Where the cluster spans multiple subnets routing is often used to provide connectivity between hosts and POD (CNI network).  Certain CNI's add routing by default while others leave routing software choice and implementation to the user. 

The Routing software enables the distribution of LoadBalancer addresses to other network devices.  This is achieved using a routing software function called redistribution. 

Redistribution is used to dynmically copy routing information between different routing protocols and Linux is one of those routers.  The LBNode adds an addition interface to linux network and linux can route from its other interfaces to kube-lb0 as well as other interface created by the CNI and KubeProxy in the case of IPVS mode, therefore Linux is a router.  Therefore those local networks can be added and then redistributed to other routes using configured routing protocols


Routing and K8s
The routing infrastructure should be designed to include k8s nodes as network devices.  In addition to the normal network topology questions, k8s presents another important question, should the routing software be run in a container or natively on the underlying host.  When routing software is run in a k8s cluster and the container is configured with _hostNetwork: true_, the routing software will have access to the same network interfaces, therefore the decision to container or host depends on when access to an interface is needed via routing.  If your cluster uses multiple network interfaces on each host for different roles, mgmt, service, storage, and the routing software is running in a container, access will be via linux networking only, likely a default router until the cluster is operational.  If the routing software is run on the host directly, routing will be active prior to the cluster starting and therefore access via routing will be available.  The decision does not impact k8s operation, each choice has tradeoffs.


BIRD
Bird is a popular opensource linux routing software.  Its used by Calico (see specific configuraiton example) and can be integrated with any k8s network.   BIRD has a protocol called _Direct_.  This protocol is used to generate routes from directly connected networks according to the list of interfaces provided.  Once in the BIRD routing table, they can be advertized using protocols supported by BIRD such as OSPF or BGP by exporting RTS_DEVICE.  The configuration snippit for the _Direct_ protocol is and an example for the desired protocol

protocol direct {
      ipv4;
      ipv6;
      interface "kube-lb0";
    }


export where source ~ [ RTS_STATIC, RTS_BGP, RTS_DEVICE ];

Note:  The PureLB repo includes BIRD packaged and configured when routing is required and the customer is not using routing software as part of the CNI.  It includes a complete configuration.


FRR
Free Range Routing (FRR) is another popular opensource linux routing software alternative.  Its has a more _tradition_ configuration style so will be more familar for some engineers  and it also has more implemented routing protocols.  To import routes from a linux interface, a specific protocol is chosen to have the routes distributed and a route map is used to select the interface,  other routing protocols then recieve routes from that protocol.  An example of redistribution into bgp is:


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



