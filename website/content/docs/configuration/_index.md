---
title: "Configuration"
description: "Configure PureLB address pools, node agent behavior, BGP routing, and external IPAM."
weight: 30
---

PureLB is configured using Custom Resources in the `purelb-system` namespace.

* [ServiceGroup](service-groups) -- Define IP address pools (local, remote, or Netbox).
* [LBNodeAgent](lbnodeagent) -- Configure node agent behavior: interface selection, GARP, and address lifetimes.
* [BGP Routing](bgp) -- Configure k8gobgp for BGP route advertisement of remote addresses.
* [Netbox IPAM](netbox) -- Integrate with an external Netbox system for IP address management.
