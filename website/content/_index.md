---
title: "PureLB"
type: docs
---

# PureLB

PureLB is a lightweight Kubernetes [Service LoadBalancer](https://kubernetes.io/docs/concepts/services-networking/service/#loadbalancer) for non-cloud deployments. It allocates IP addresses from configured pools and uses standard Linux networking to announce them, giving your services external reachability without a cloud provider.

**[GitHub](https://github.com/purelb/purelb)** | **[#purelb-users on Kubernetes Slack](https://kubernetes.slack.com/messages/purelb-users)**

## Features

* **Local and Remote Addresses.**
[Local addresses]({{< relref "/docs/overview/address-types#local-addresses" >}}) are added to host interfaces for same-subnet access. [Remote addresses]({{< relref "/docs/overview/address-types#remote-addresses" >}}) are added to a dummy interface and advertised via BGP for routed topologies.

* **Multi-Subnet Local Addresses.**
[Multi-pool allocation]({{< relref "/docs/overview/address-types#multi-subnet-local-addresses" >}}) gives a single service local addresses on every subnet in your cluster, making it reachable from all network segments without routing.

* **Address Lifetime Control.**
[Non-permanent address lifetimes]({{< relref "/docs/configuration/lbnodeagent#address-lifetime" >}}) prevent conflicts with Flannel, DHCP, and other systems that inspect address flags to select a node's primary IP.

* **Integrated BGP Routing.**
Ships with [k8gobgp]({{< relref "/docs/configuration/bgp" >}}) as a sidecar, providing BGP route advertisement with no external routing software required.

* **Dual-Stack IPv4 and IPv6.**
Full support for IPv4, IPv6, and [dual-stack]({{< relref "/docs/configuration/service-groups#dual-stack" >}}) deployments.

* **CRD-Based Configuration.**
Configured using [Custom Resource Definitions]({{< relref "/docs/configuration" >}}) with schema validation.

* **External IPAM Integration.**
Integrates with [Netbox]({{< relref "/docs/configuration/netbox" >}}) for enterprise IP address management.

* **Prometheus Metrics.**
Built-in [metrics]({{< relref "/docs/operations/monitoring" >}}) for pool utilization, election health, and node agent activity.

* **kubectl Plugin.**
The [kubectl-purelb]({{< relref "/docs/operations/kubectl-plugin" >}}) plugin provides operational visibility: pool status, election state, BGP sessions, and troubleshooting.

* **Multiple LoadBalancer Controllers.**
Implements `LoadBalancerClass` so PureLB can coexist with other load balancer controllers.

* **GARP Support.**
Configurable [Gratuitous ARP]({{< relref "/docs/configuration/lbnodeagent#garp-configuration" >}}) for EVPN/VXLAN environments.

## Get Started

1. [Prerequisites]({{< relref "/docs/installation/prerequisites" >}})
2. [Install PureLB]({{< relref "/docs/installation/manifest" >}})
3. [Create your first LoadBalancer Service]({{< relref "/docs/installation/first-service" >}})
