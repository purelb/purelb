---
title: "PureLB"
description: "Description of PureLB"
weight: 10

hide: toc, breadcrumb
---

# What is PureLB?

**_PureLB_** is a Service Load Balancer for Kubernetes.  A LoadBalancer is a Service type that allows configuration of external network components to enable external network access
to the specified application resource. 

Service Load Balancers are key component in the K8s developer workflow.  They allow the configuration of the resources used to enable access to applications to be pre-configured
so they can be access on demand by developers via the service defination.  This simple operator can be undertaken on demand or as part of CI without custom configuraiton or tooling.   


### Features

* **Easy to Use.**
Expose applications to public addresses by creating a service type loadbalancer.

* **Leverages Linux Networking.**
Configures Linux Networking stack so its easy to observe behavior and troubleshoot.

* **Local Address support.**
Addresses matching the local host address are automatically added to the default interface for simple local access.

* **Routing.**
All non-local addresses are added to a virtual interface for distribution by routing software unlocking full routing functionality

* **Service Groups.**
Configurable policy, address & network configuration & load balancer behavior 

* **Easy Integration with CNI routing.**
Supports CNI's such as Calico that implement routing, simple tell the routing software to advertize the virtual interface addresses

* **Supports multiple Service Load Balancer controllers.**
Allows multiple Load Balancer Controllers to be installed in the same cluster for use in Public Clouds

* **Configured using Custom Resources.**
Use of CRD's simplifies configuration with validation

* **Native Support for IPv4 & IPv6.**
PureLB provides support for IPv4 & IPv6 (subject to your cluster & network configuraiton)

* **Extensible to support external IPAM.**
Integrate Service Load Balancer with Enterprise IP Address Management Systems