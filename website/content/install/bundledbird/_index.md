---
title: "BIRD Router"
description: "Describe Operation"
weight: 45
hide: [ "toc", "footer" ]
---
Instead of re-implementing routing protocols in PureLB, we use proven Linux routing software like [BIRD](https://bird.network.cz/). This increases PureLB's functionality and reliability, and simplifies the Kubernetes network implementation. We like BIRD's configuration model, but PureLB works with [several routers](../../how_it_works/routers). BIRD is configured from a single file, and when dynamically loaded will update its configuration without restarting the routing processes and impacting reachability.

{{<mermaid align="center">}}

  graph BT;

    A(Router/Switch 1);
    B(Router/Switch 2);

    subgraph Node-1
      C1(BIRD)
    end

    subgraph Node-2
      D1(BIRD)
    end

    subgraph Node-3
      E1(BIRD)
    end

    C1-.-|peer/neighbor|A;
    C1-.-|peer/neighbor|B;
    D1-.-|peer/neighbor|A;
    D1-.-|peer/neighbor|B;
    E1-.-|peer/neighbor|A;
    E1-.-|peer/neighbor|B;


{{</mermaid>}}

## BIRD Pod
We've built a [BIRD package](https://gitlab.com/purelb/bird_router) that includes the BIRD router container, a [DaemonSet to run BIRD](https://gitlab.com/purelb/bird_router/-/blob/main/bird.yml?ref_type=heads), and a [sample configmap](https://gitlab.com/purelb/bird_router/-/blob/main/bird-cm.yml?ref_type=heads). This can be used with configuration changes, or used as a template for more complex network configurations.

A key feature of Kubernetes networking is `hostNetwork: true` in the pod configuration.  Combined with securityContext capabilities, this lets the pod access the host network namespace, so the BIRD router pod is isolated but has access to the host network.  PureLB relies on the same functionality.

BIRD pods read their configuration from a configmap projected into the BIRD container.  Inotify watches the projected file and reloads the BIRD router process when the file changes. The container also includes `birdc`, the BIRD command line, which is useful for troubleshooting.

### The Sample Configuration
The sample configuration provides the following:

* Import Routes from `kube-lb0`
* Set RouterID to node address
* RIP template
* OSPF template
* BGP template
* IPv4 & IPv6 support

Each of the routing templates is configured to advertise routes learned from `kube-lb0` but not add any routes learned from neighbors or peers. PureLB does not need to learn any routes.

Your choice of routing protocols will depend upon how your Kubernetes cluster is integrated with your network. In a configuration with IGP (interior gateway protocol) such as OSPF or RIP, OSPF is ideal. Each node will establish adjacencies with the OSPF routers and advertise routes. A solution often used in larger cloud systems uses EGP (exterior gateway protocol), BGP as both an EGP and IGP, in this case each node establishes a peering relationship with the neighboring BGP router and advertises routes.  Each of these alternatives has benefits: once OSPF is configured, routes are distributed within the OSPF network, however there are few controls over which routes are distributed and how distribution is controlled. BGP provides total control over how routes are configured, especially where peering is E-BGP, however configuration is required to distribute routes (since BGP does nothing by default).

We recommend that you use BGP or OSPF but in some cases older routers have limited capabilities, and in those cases RIP can help as it's simple and widely supported.
