---
title: "Adding a Bird Router"
description: "Describe Operation"
weight: 45
hide: [ "toc", "footer" ]
---


  A packaged BIRD Daemonset is located in the [PureLB Gitlab Repo](https://gitlab.com/purelb/bird_router).  Instead of implementing routing protocols directly into the load balancer software, PureLB uses proven linux routing software.  This increases the functionality and reliability of PureLB, as well as simplifying the k8s network implementation.  Bird was selected because of its configuration model.  Bird is configured from a single file, and when dynamically loaded will update the current configuration with restarting the routing processes and impacting reachability.

{{<mermaid align="center">}}

  graph BT;

    A(Router/Switch 1);
    B(Router/Switch 2);

    subgraph k8s-Node-1
      C1(Bird)
    end

    subgraph k8s-Node-2
      D1(Bird)
    end

    subgraph k8s-Node-3
      E1(Bird)
    end

    C1-.-|peer/neighbor|A;
    C1-.-|peer/neighbor|B;
    D1-.-|peer/neighbor|A;
    D1-.-|peer/neighbor|B;
    E1-.-|peer/neighbor|A;
    E1-.-|peer/neighbor|B;


{{</mermaid>}}

## Bird Pod
For simplicity, the repo packages the bird router container, daemonset configuration and sample configmap.  This can either be loaded with simple configuration changes or used as a template for more complex network configurations.  A key component of the operation of k8s infrastructure networking is the use of _hostNetwork: true_ in the pod configuration.  This combined with securityContext capabilities enables the pod to access the host network namespace. Using this technique, the Bird router process is isolated but has access to access to the host network.  PureLB relies on the same functionality.

Bird pods read their configuration from a configmap projected into the BIRD container in the same namespace.  Ionotify watches the projected file and reloads the bird router process on change.  The container also includes the birdc, the bird command line, useful for troubleshooting.


### The Sample Configuration
The sample configuration provides the following:

* Import Routes from kube-lb0
* Sets RouterID to node address
* RIP template
* OSPF template
* BGP template
* IPv4 & IPv6 support

Each of the routing templates is configured to advertise routes learned from kube-lb0 but not add any routes learned from neighbors or peers, PureLB does not need to learn any routes. 

The choice of routing protocols will depend upon how the k8s cluster is integrated with  network infrastructure.  In a configuration with IGP (interior gateway protocol) such as OSPF or RIP, OSPF is an ideal alternative.  Each node will establish adjacencies with the OSPF routers and advertise routes.  A solution often used in larger cloud systems uses the EGP (exterior gateway protocol), BGP as both an EGP and IGP, in this case each node establishes a peering relationship with the neighboring BGP router and once established advertises routes.  Each of these alternatives has benefits, once OSPF is configured routes are distributed within the OSPF network, however there are few controls over which routes are distributed and how distribution is controlled.  BGP provides total control over how routes are configure, especially where peering is E-BGP, however configuration is required to distribute routes, by default BGP does nothing.  

A template for RIP is included however if either BGP or OSPF can be used, they are recommended.  In some cases older routes have limited capabilities/functionality, in this case RIP can help as its simple and widely supported.





