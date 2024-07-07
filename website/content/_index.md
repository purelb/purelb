---
title: "PureLB"
description: "PureLB is a lightweight Kubernetes Service LoadBalancer for non-cloud deployments. It provides external access to your application, using Linux networking to add addresses to Network Interface Cards (enabling access from the local network) or to virtual interfaces (so the address can be distributed to routers)."
weight: 10

hide: [ "toc", "breadcrumb", "nextpage", "footer" ]
---

<img align="right" src="images/purelb.png">

PureLB is a lightweight Kubernetes [Service LoadBalancer](https://kubernetes.io/docs/concepts/services-networking/service/#loadbalancer) for non-cloud deployments. It provides external access to your application, using Linux networking to add addresses to Network Interface Cards (enabling access from the local network) or to virtual interfaces (so the address can be distributed to routers).

### Features

* **Easy to Use.**
Expose applications by allocating addresses to services using type LoadBalancer.

* **Leverages Linux Networking.**
Works with Linux networking for easy observation and troubleshooting.

* **Local Address Support.**
Local addresses are added to host interfaces for simple local access.

* **Routing.**
Non-local addresses are added to a virtual interface for distribution by routing software or CNI, unlocking full routing functionality.

* **Easy Integration with CNI Routing.**
Supports CNIs such as Calico that implement routing.

* **Works With Other Service Load Balancer Controllers.**
Implements LoadBalancerClass which allows multiple LoadBalancer Controllers to be installed in the same cluster.

* **Configured using Custom Resources.**
Use of CRDs simplifies configuration and provides input validation.

* **Dual Stack Support for IPv4 and IPv6.**
Supports Dual Stack IPv6 if your cluster has IPv6.

* **Supports GARP for Datacenters using EVPN/VXLAN.**
GARP can be enabled to support ARP suppression mechanisms used in EVPN/VXLAN.

* **Supports External IPAM.**
Integrates with Enterprise IP Address Management Systems.
