---
title: "How am I Loadbalancing?"
description: "Describe Operation"
weight: 40
hide: toc, nextpage
---


A Load Balancer accepts traffic and distributes it among endpoints. PureLB is not strictly a Load Balancer, it is a controller of Load Balancers that interacts with the v1.Service API.

More simply, PureLB either uses the LoadBalancing functionality provided natively by k8s and/or combines k8s LoadBalancing with the routers Equal Cost Multipath (ECMP) load-balancing.

## k8s Load Balancing

A service is used to enable access to IP ports on PODs, without a service the POD is considered headless.  A load balancer is one type of service, however other such as nodeport exist (add k8s link).  Traffic that is attracted to a k8s node is distributed to other nodes where PODS are located, this functionality is provided by kubeproxy or in some cases other networking components such as OVS in OVN CNI (used by Openshift).  In each case, kube-api informs the collection of controllers required to ensure traffic presented at a node for that address/port combination reaches the correct POD.  In the default case of kubeProxy, this is undertaken using IP tables filters.  KubeProxy is watching for v1.service events just like the Purelb lbnodeagent.  When kubeProxy receives a service message, and it is using iptables, it adds rules that allow the forwarding of an IP address that is not on the host (iptables pre-routing), chained to rules that distribute traffic equally to each of the nodes where the associated endpoint exists.   IPVS does the same thing, however the address is added to the host interface _kube-ipvs0_ using smaller IPtables and IPset.  The IPVS virtual interfaces operates in the same manner to the PureLB virtual interface _kube-lb0_.  This kubeproxy operation is similar for Nodeports.  

Therefore when PureLB "attracts" traffic to nodes by advertizing routes, once the traffic reaches the node it is load-balanced by k8s among PODs.  This is the level of load-balancing that is provided for Local Addresses.  As the address is on the local network, and the upstream routers do not participate, the address can only be allocated to a single node on the subnet.

However, by adding routers to the cluster, peering to the networks router (upstream) and allocating addresses from a new IPNET, the router can advertise the same address from each node with an equal cost therefore enabling Equal Cost Multipath load-balancing in the router.

{{<mermaid align="center">}}
graph BT;

    subgraph Router
        A(a.a.a.a.1 via x.x.x.1 <br/> via x.x.x.2 <br/> a.a.a.2 via x.x.x.1 <br/> via x.x.x.2)
    end
    subgraph k8s-node-2
        B[eth0 x.x.x.2]
        C[kubeproxy]
        D[POD a.a.a.2]
    end
    subgraph k8s-node-1
        E[eth0 x.x.x.1]
        F[kubeproxy]
        G[POD a.a.a.1]
    end
    E---F
    F---G
    B---C
    C---D
    C-.-F
    D-->|advertise a.a.a.1 & a.a.a.2 next-hop x.x.x.1|A
    E-->|advertise a.a.a.1 & a.a.a.2 next-hop x.x.x.2|A
  
{{</mermaid>}}


As shown in the diagram, the routing table shows that each POD has multiple next-hops that can
reach the destination. The router will load balance between those two destinations equally usually hashing on SRCIP/PORT & DESTIP/PORT.  ECMP load balancing is a forwarding function of the router and often the implementations are more complex to ensure invariance and stability of forwarding in steady state and single link failure.  However even a Linux host with routing software can be configured to provide ECMP forwarding.
