---
title: "Calico CNI"
description: "Describe Operation"
weight: 50
hide: [ "toc", "footer" ]
---
[Calico](https://www.tigera.io/project-calico/) is a popular CNI. The primary role of a CNI is to establish connectivity between pods across nodes. All CNIs operate in a similar manner: allocating IP addresses to pods and establishing connectivity between nodes.

## Background
There are two mechanisms for establishing connectivity between nodes:

* Tunnels. Most packaged installations default to the use of tunnels to provide connectivity between nodes. This avoids any local network configuration or conflict with network infrastructure.
* Flat Networking. Flat networking uses the existing IP infrastructure without tunnels to establish communication between pods.

In both cases, the CNI allocates addresses to pods and updates routing tables on nodes so that each node can reach all of the pods.

Calico can use either tunneled or flat networking, with two flat networking variants:

* BGP Node Mesh. Calico establishes BGP peering sessions between all nodes to distribute pod network routes.
* TOR. Each node peers with one or more BGP routers, often TOR switches.

The Calico node pod contains the BIRD router (unfortunately version 1x, IPv4 & IPv6 are in separate processes) that is started when a BGP is enabled. BIRD configuration is managed via Calico CRD's.

_Note:BGP is enabled by default in a Calico installation but disabled in the Microk8s package in favor of VXLAN overlays._

_See the [Calico Documentation](https://docs.projectcalico.org/networking/determine-best-networking) for more detail on network configuration options._

## Integrating With PureLB
Using Calico's TOR style configuration is a logical choice when PureLB is used with virtual addresses and the addresses need to be advertised using BGP. Configuring the Calico CNI to peer with TOR BGP routers will result in the cluster network on each node being advertised, establishing pod network connectivity.

The Calico CNI pod uses the host networking namespace just like the PureLB LBNodeAgent. Both Calico and PureLB modify the host networking stack in a similar manner, adding interfaces and addresses to the host. When using Calico with BGP, the BIRD router imports those interfaces, addresses, and routes. This removes the need for an additional BGP process to be used for [virtual addresses](../../how_it_works/virtint/): BIRD in the Calico node pod will import the addresses created by PureLB for LoadBalancer Services.

### Required Calico Configuration
As with any BGP routing configuration, care is taken to *not* advertise routes that are not required.  The Calico BIRD configuration imports the routes to addresses allocated by PureLB to `kube-lb0` but does not advertise routes to those addresses. To enable the advertisement of the address range allocated in the PureLB ServiceGroups, a Calico _IPPool_ must be created.

The Calico IPPool is set to disabled so Calico does not use the pool for any of its address allocation purposes. However the configuration does add export rules to the BIRD BGP configuration for this CIDR so when addresses are added by PureLB to `kube-lb0`, routes will be advertised to the peered routers. 

Here's an example PureLB IPv4 ServiceGroup:
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: ipv4-routed
  namespace: purelb-system
spec:
  local:
    aggregation: /32
    pool: 172.30.200.155-172.30.200.160
    subnet: 172.30.200.0/24
```
...and its corresponding Calico IPv4 Pool:
```yaml
apiVersion: crd.projectcalico.org/v1
kind: IPPool
metadata:
  name: purelb-ipv4
spec:
  cidr: 172.30.200.0/24
  disabled: true
```
Here's an example IPv6 ServiceGroup:
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: ipv6-routed
  namespace: purelb-system
spec:
  local:
    aggregation: /128
    pool: 2001:470:8bf5:2:2::1-2001:470:8bf5:2:2::ffff
    subnet: 2001:470:8bf5:2::/64
```
...and its corresponding Calico IPv6 Pool
```yaml
apiVersion: crd.projectcalico.org/v1
kind: IPPool
metadata:
  name: purelb-ipv6
spec:
  cidr: 2001:470:8bf5:2::/64
  disabled: true
```

_Note: This configuration is ideally suited to a dual stack Kubernetes deployment._

### Recommended Calico Configuration
Calico uses BGP to advertise the pod network, which contains internal addresses that are likely RFC1918 allocations. One of the key reasons to use an external LoadBalancer is to separate internal addresses from external public addresses. At the external network boundary the pod network routes should be filtered and not advertised. This type of route manipulation is commonly undertaken with BGP communities. Each route is tagged with a community and is either advertised or dropped at BGP border routers depending on that community. We recommend that PureLB routes be tagged with a community when they are initially advertised, allowing network administrators to manage the distribution of these routes without needing to implement address-specific filters in their border routers.

BGP communities can be configured using Calico CRs. Here's an example Calico BGP configuration including communities:
```yaml
apiVersion: crd.projectcalico.org/v1
kind: BGPConfiguration
metadata:
  name: default
spec:
  asNumber: 4200000101
  listenPort: 179
  logSeverityScreen: Info
  nodeToNodeMeshEnabled: false
  prefixAdvertisements:
  - cidr: 172.30.200.0/24
    communities:
    - purelb
    - 100:100
  - cidr: 2001:470:8bf5:2::/64
    communities:
    - purelb
    - 100:100
```
In the example above, routes created for PureLB address allocations will be tagged with the community `100:100`. The BGP border router is configured to match this community and advertise those addresses.

_Note: This configuration is ideally suited to a dual stack Kubernetes deployment._
