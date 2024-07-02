---
title: ""
description: "Description of PureLB"
weight: 10

hide: toc, breadcrumb
---


<img align="left" src="images/purelb.png">


</br>

**_PureLB_** is a Service Load Balancer Controller for Kubernetes.  A LoadBalancer is a Service type that allows configuration of network components external to Kubernetes to enable network access to the specified application resources. 

The resources that PureLB controls is the host Linux networking stack, adding addresses to either Network Interface Cards enabling access from the local host network or to a virtual interface named kube-lb0 so the address can be distributed to routers.

Service Load Balancers are key component in the K8s developer workflow.  They allow access resources to be pre-defined so they can be accessed on demand by developers via service definition.  This simple operation can be undertaken on demand or as part of CI without custom configuration or tooling.   

</br>

### Features

* **Easy to Use.**
Expose applications by allocating addresses to services using type LoadBalancer.

* **Leverages Linux Networking.**
Configures Linux Networking stack so its easy to observe behavior and troubleshoot.

* **Local Address support.**
Addresses matching the local host address are automatically added to the default or configured interface for simple local access.

* **Routing.**
All non-local addresses are added to a virtual interface for distribution by routing software or CNI unlocking full routing functionality.

* **Service Groups.**
Configurable policy, address & network configuration & load balancer behavior.

* **Easy Integration with CNI routing.**
Supports CNIs such as Calico that implement routing, CNI distributes LoadBalancer alongside other routes.

* **Supports multiple Service Load Balancer controllers.**
Implements LoadBalancerClass allowing multiple Load Balancer Controllers to be installed in the same cluster for use in Public Clouds.

* **Configured using Custom Resources.**
Use of CRDs simplifies configuration with validation

* **Dual Stack Support for IPv4 & IPv6.**
PureLB provides Dual Stack IPv6 support (subject to your cluster & network configuration)

* **Supports GARP for Datacenters using EVPN/VXLAN.**
GARP can be enabled to support ARP suppression mechanisms used in EVPN/VXLAN


* **Extensible to support external IPAM.**
Integrate Service Load Balancer with Enterprise IP Address Management Systems
