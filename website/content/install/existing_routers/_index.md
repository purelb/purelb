---
title: "Integrating existing in-cluster routers"
description: "Describe Operation"
weight: 25
hide: [ "toc", "footer" ]
---

If there is a router already present on the node either running natively or in a container, PureLB can use that router to advertise routes to load-balancer addresses.  This functionality depends on the routing software being able to manipulate the Linux Routing table or FIB.  This is a necessary requirement to update the hosts routing table so it therefore likely.


## Exporting routes from PureLB.
Using the redistribution or route import functions import routes from the routing table that are attached to the device kube-lb0. 

{{% notice danger %}}
Kube-proxy in ipvs mode creates a device and uses a similar name _kube-ipvs0_.  Take care to not import those routes as they include end-point routes and are not summarized (all /32).  kube-ipvs0 will add a lot of unnecessary and insecure routes and if a route with a more specific route (/32) is advertised it will be selected
{{% /notice %}}

## Importing routes to PureLB
PureLB does not require any changes to route import configuration.  

