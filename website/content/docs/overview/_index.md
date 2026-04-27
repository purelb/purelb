---
title: "Overview"
description: "How PureLB works: architecture, address types, and election system."
weight: 10
---

PureLB is a load-balancer orchestrator for Kubernetes clusters. It allocates IP addresses from configured pools and configures Linux networking to announce them, making your LoadBalancer Services reachable from outside the cluster.

This section explains how PureLB works:

* [Architecture](architecture) -- The Allocator and LBNodeAgent components, how they interact with the Kubernetes API, and the role of kube-proxy.
* [Address Types](address-types) -- Local addresses (same subnet), remote addresses (routed via BGP), and Netbox IPAM.
* [Election System](election) -- How PureLB uses Kubernetes Leases to elect which node announces each local address.
