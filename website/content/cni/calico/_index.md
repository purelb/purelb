---
title: "Calico"
description: "Describe Operation"
weight: 10
hide: [ "toc", "footer" ]
---
Calico is a CNI bundled with a number of Kubernetes packages including the popular MicroK8s or can be added independently. The primary role of a CNI is to establish connectivity between pods across nodes.  All CNI's operate in a similar manner, allocating IP addresses to pods and establishing connectivity between nodes.

### Background
There are two mechanisms for establishing connectivity between Nodes.

* Tunnels.  Most packaged installations default to the use of tunnels to provide connectivity between Nodes.  This avoids any local network configuration or conflict with the existing network infrastructure
* Flat Networking.  Flat networking uses the existing IP infrastructure without tunnels to establish communications between pods.

In both cases, the CNI allocates addresses to the pods and updates routing tables on nodes so that each node can reach all of the pods.

Calico operates in both tunneled and a flat networking manner, with two flat networking variants.

* BGP Node Mesh.  Calico established BGP peering sessions between all nodes to distribute pod network routes
* TOR.  Each node peers with one or more BGP routers, often TOR switches.

The Calico node pod contains the BIRD router (unfortunately version 1x, IPv4 & IPv6 are in separate processes) that is stated when a BGP is enabled.  The BIRD configuration is managed via Calico CRD's.  

_Note:BGP is enabled by default in a Calico installation but disabled in the Microk8s package in favor of VXLAN overlays._

_See the [Calico Documentation](https://docs.projectcalico.org/networking/determine-best-networking) for more detail on network configuration options._


### Integrating with PureLB
Using Calico's TOR style configuration is a logical choice when PureLB is used with virtual addresses and the addresses need to be advertised using BGP.  

Configuring the Calico CNI to peer with TOR BGP routers will result in the Cluster Network on each node being advertised establishing pod network connectivity. 

The Calico CNI pod uses the host networking namespace just like the PureLB node agent.  Both Calico and PureLB modify the host networking stack in a similar manner, adding interfaces and addresses to the host.  When using Calico with BGP, the BIRD router imports those interfaces, addresses and routes.  This removes the need for an additional BGP process to be used for Virtual addresses, BIRD in the Calico Node pod will import the addresses created by PureLB for external LoadBalance Services.

#### Required Calico Configuration
As with any BGP routing configuration, care is taken not to advertise routes that are not required.  The Calico BIRD configuration imports the routes to addresses allocated by PureLB to _kube-lb0_ but does not advertise the routes to those addresses.  To enable the advertisement of the address range allocated in the PureLB ServiceGroups, a Calico _IPPool_ is created.  


The Calico IPPool is set to disabled so Calico does not use the pool for any of its address allocation purposes.   However the configuration does add export rules to the BIRD BGP configuration for this CIDR and when addresses are allocated by PureLB to _kube-lb0_, routes will be advertised to the peered routers. 


PurelLB IPv4 ServiceGroup
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: ipv4-routed
  namespace: purelb
spec:
  local:
    aggregation: /32
    pool: 172.30.200.155-172.30.200.160
    subnet: 172.30.200.0/24
```
Calico IPv4 Pool
```yaml
apiVersion: crd.projectcalico.org/v1
kind: IPPool
metadata:
  name: purelb-ipv4
spec:
  cidr: 172.30.200.0/24
  disabled: true
```
PureLB IPv6 ServiceGroup
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: ipv6-routed
  namespace: purelb
spec:
  local:
    aggregation: /128
    pool: 2001:470:8bf5:2:2::1-2001:470:8bf5:2:2::ffff
    subnet: 2001:470:8bf5:2::/64
```
Calico IPv6 Pool
```yaml
apiVersion: crd.projectcalico.org/v1
kind: IPPool
metadata:
  name: purelb-ipv6
spec:
  cidr: 2001:470:8bf5:2::/64
  disabled: true
```

_Note:  This configuration is ideally suited to a Dual Stack Kubernetes deployment._

#### Recommended Calico Configuration
Calico uses BGP to advertise the pod Network, this network contains internal addresses that are likely RFC1918 allocations.  One of the key reasons to use an external LoadBalancer is to separate internal addresses from external public addresses.  At the external network boundary the pod Network routes would be filter and not advertised.  This type of route manipulation is commonly undertaken with BGP communities.  The route is tagged with a community and based upon the community is either advertised or dropped at BGP border routers.   Its recommended that PureLB routes be tagged with a community when they are initially advertised allowing network administrators to manage the distribution of these routes without needing to implement address specific filters in the board routers.  BGP communities can be configured using Calico CRs

Calico BGP default configuration including community 
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
In the example above routes created for PureLB external address allocations will be tagged with the community _100:100_  The BGP boarder router is configured to match this community and advertise those address.  


_Note:  This configuration is ideally suited to a Dual Stack Kubernetes deployment._
