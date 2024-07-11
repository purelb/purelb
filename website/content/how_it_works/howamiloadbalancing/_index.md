---
title: "How Am I Loadbalancing?"
description: "Describe Operation"
weight: 35
hide: [ "toc", "footer" ]
---

Load balancers accept traffic and distribute it among endpoints. PureLB is not a load balancer per se since it doesn't handle data traffic, but it works with Kubernetes and routers to provide load balancing. PureLB configures Kubernetes' load balancing functionality and can combine that with router-provided Equal Cost Multipath (ECMP) load balancing.

## Kubernetes Load Balancing

[Kubernetes Services](https://kubernetes.io/docs/concepts/services-networking/service/) enable IP access to pods: a pod without a Service is "headless". Adding a Service in front of a set of pods allows traffic arriving at a Kubernetes node to be distributed to those pods, running on that host and others in the cluster. This functionality is provided by [kube-proxy](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-proxy/) or, in some cases, by other networking components such as OVS in OVN CNI (used by Openshift). kube-api informs controllers about configuration change events and those controllers respond by making the necessary configuration changes.

kube-proxy watches for `v1.service` events just like the PureLB [LBNodeAgent](../#overview) does. When kube-proxy receives a Service message, and it has been configured to use [iptables](https://kubernetes.io/docs/reference/networking/virtual-ips/#proxy-mode-iptables), it adds rules that allow forwarding IP addresses that are not on the host (iptables pre-routing), chained to rules that distribute traffic equally to each of the nodes where the associated endpoint exists. kube-proxy in [IPVS mode](https://kubernetes.io/docs/reference/networking/virtual-ips/#proxy-mode-ipvs) does the same thing, however the address is added to the host interface `kube-ipvs0` using smaller IPtables and IPset.  IPVS virtual interfaces operate in the same manner to the PureLB virtual interface _kube-lb0_.  This kube-proxy operation is similar for [NodePorts](https://kubernetes.io/docs/concepts/services-networking/service/#type-nodeport).

When using [local addresses](../localint/), PureLB "attracts" a Service's traffic to a single node by adding an IP address to that node, and the traffic is then load-balanced inside the cluster by Kubernetes. As the address is on the local network, and the upstream routers do not participate, the address can only be added to a single node so all of that Service's traffic enters the cluster through that single node only.

However, by adding routers to the cluster, peering to the networks router (upstream) and [allocating addresses from a new IPNET](../virtint/), the router can advertise the same address from each node with an equal cost, enabling Equal Cost Multipath load balancing in the router.

{{<mermaid align="center">}}
graph BT;

    subgraph Router
        A(a.a.a.a.1 via x.x.x.1 <br/> via x.x.x.2 <br/> a.a.a.2 via x.x.x.1 <br/> via x.x.x.2)
    end
    subgraph k8s-node-2
        B[eth0 x.x.x.2]
        C[kubeproxy]
        D[pod a.a.a.2]
    end
    subgraph k8s-node-1
        E[eth0 x.x.x.1]
        F[kubeproxy]
        G[pod a.a.a.1]
    end
    E---F
    F---G
    B---C
    C---D
    C-.-F
    D-->|advertise a.a.a.1 & a.a.a.2 next-hop x.x.x.1|A
    E-->|advertise a.a.a.1 & a.a.a.2 next-hop x.x.x.2|A

{{</mermaid>}}

As shown in the diagram, the routing table shows that each pod has multiple next-hops that can
reach the destination. The router will load balance between those two destinations equally (usually hashing on SRCIP/PORT & DESTIP/PORT). ECMP load balancing is a forwarding function of the router and often the implementations are more complex to ensure invariance and stability of forwarding in steady state and single link failure. However even a Linux host with routing software can be configured to provide ECMP forwarding.
