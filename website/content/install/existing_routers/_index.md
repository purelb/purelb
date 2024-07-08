---
title: "In-cluster Routers"
description: "Describe Operation"
weight: 25
hide: [ "toc", "footer" ]
---

If there is a router already present on the node (either running natively or in a container), PureLB can use it to advertise routes to LoadBalancer addresses. This depends on the routing software being able to manipulate the Linux Routing table or FIB, which is probably true since it's also needed to update the host's routing table.

## Exporting Routes
Use the redistribution or route import functions to import routes from the routing table that are attached to `kube-lb0`.

{{% notice danger %}}
Kube-proxy in ipvs mode creates a device and uses a similar name: `kube-ipvs0`. Take care to not import those routes as they include end-point routes and are not summarized (all /32). `kube-ipvs0` will add a lot of unnecessary and insecure routes and if a route with a more specific route (/32) is advertised it will be selected.
{{% /notice %}}

## Importing Routes
PureLB does not require any changes to import route configuration.
