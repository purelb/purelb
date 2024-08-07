---
title: "Initial Configuration"
description: "Describe Operation"
weight: 15
hide: [ "toc", "footer" ]
---

PureLB configuration uses Custom Resources.  A single LBNodeAgent resource configures the LBNodeAgent, and as many ServiceGroup resources as needed configure external IP address ranges.

```sh
$ kubectl api-resources --api-group=purelb.io
NAME            SHORTNAMES   APIGROUP    NAMESPACED   KIND
lbnodeagents    lbna         purelb.io   true         LBNodeAgent
servicegroups   sg           purelb.io   true         ServiceGroup
```

## LBNodeAgent
The installation procedure creates `lbnodeagent.purelb.io/default` which will work in most cases.

```sh
$ kubectl describe --namespace=purelb lbnodeagent.purelb.io/default
Name:         default
Namespace:    purelb
Kind:         LBNodeAgent
...
Spec:
  Local:
    extlbint:  kube-lb0
    localint:  default
    sendgarp:  false
```
parameter | type | Description
-------|----|---
extlbint | An interface name | The name of the virtual interface used for virtual addresses. The default is `kube-lb0`. If you change it, and are using the PureLB bird configuration, make sure you update `bird.cm`.
localint | An interface name regex | By default, PureLB automatically identifies the interface that is connected to the local network, and the address range used. To override this and specify the interface to which PureLB will add local addresses, specify the NIC's name or a regex.  If you specify this, you need to make sure that the interface has appropriate routing. PureLB will find the interface with the lowest-cost default route, i.e., the interface that is most likely to have global communications.
sendgarp | true/false (false by default) | Gratuitous ARP (GARP), required for EVPN/VXLAN environments.

## ServiceGroup
ServiceGroups contain the configuration required to allocate LoadBalancer addresses. In the case of locally allocated addresses, ServiceGroups contain address pools. In the case of NetBox, ServiceGroups contain the configuration necessary to contact Netbox so the Allocator can fetch addresses.

ServiceGroups are namespaced, however, PureLB watches all namespaces.  For simplicity we recommend adding them to the `purelb` namespace. In cases where RBAC restricts who can update `purelb`, ServiceGroups can be added to the namespaces of their users.

### Local Addresses
{{% notice danger %}} Note: PureLB does not install a default ServiceGroup because everyone's network environment is unique. You will need to make at least one ServiceGroup that works in your environment.{{% /notice %}}

A ServiceGroup is configured for each pool of addresses to be managed by PureLB.  ServiceGroups support Dual Stack, therefore a ServiceGroup can contain both IPv4 and IPv6 addresses, and it can contain multiple ranges of each. PureLB uses the ServiceGroup named `default` when no `purelb.io/service-group` annotation is present in the Service definition, so we recommend that you define one ServiceGroup named `default`.

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb
spec:
  local:
    v4pools:
    - subnet: 192.168.254.0/24
      pool: 192.168.254.230/32
      aggregation: default
    - subnet: 192.168.254.0/24
      pool: 192.168.254.231-192.168.254.240
      aggregation: default
    v6pools:
    - subnet: fd53:9ef0:8683::/120
      pool: fd53:9ef0:8683::-fd53:9ef0:8683::3
      aggregation: default
```

parameter | type | Description
-----|----|------
v4pools | IPv4 AFI | Array of configuration for IPv4 address ranges
v6pools | IPv6 AFI | Array of configuration for IPv6 address ranges

Each pool contains the following:

parameter | type | Description
-------|----|---
subnet | IPv4 or IPv6 CIDR| The subnet that contains all of the pool addresses. PureLB uses this information to compute how the address is added to the cluster.
pool | IPv4 or IPv6 CIDR or range | The specific range of addresses that will be allocated.  Can be expressed as a CIDR or range of addresses.
aggregation | "default" or subnet mask "/8" - "/128" | The aggregator changes the address mask of the allocated address from the subnet's mask to the specified mask.

#### Aggregation
Aggregation is a capability commonly used in routers to control how addresses are advertised.  When a ServiceGroup is defined with `aggregation: default` the subnet's prefix mask will be used. PureLB will create an address from the allocated address and subnet mask and add it to the appropriate interface. For example, if the Allocator allocates _192.168.1.100_, and `aggregation: default` is set, then PureLB will add _192.168.1.100/24_ to the appropriate interface. Similarly for IPv6, _fc:00:370:155:0:8000::/126_ will result in the address _fc:00:370:155:0:8000::/64_ being added.  Adding an address to an interface also updates the routing table, therefore if it's a new network (not a new address), a new routing table entry is added.  This is how routes are distributed into the network via the virtual interface and node routing software.

The primary purpose of aggregation is to change the way that routing distributes the address.  Changing the aggregator impacts when the prefix is added to the routing table.

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: agg-sample
  namespace: purelb
spec:
  local:
    v4pools:
    - subnet: '192.168.1.0/24'
      pool: '192.168.1.100-192.168.1.200'
      aggregation: /25
```
In the example above, the address range is further subnetted by changing the aggregation.  As the allocator adds addresses, when the first address is allocated the routing table will be updated with _192.168.1.0/25_. When the allocator adds _192.168.1.129_, the routing table will be updated with _192.168.1.128/25_.

This example is somewhat academic but illustrates the use:
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: team-1
  namespace: purelb
spec:
  local:
    v4pools:
    - subnet: '192.168.1.0/26'
      pool: '192.168.1.0-192.168.1.62'
      aggregation: /24
---
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: team-2
  namespace: purelb
spec:
  local:
    v4pools:
    - subnet: '192.168.1.64/26'
      pool: '192.168.1.64-192.168.1.126'
      aggregation: /24
```
In this example, the k8s cluster has been allocated the address range of _192.168.1.0/24_ and the network infrastructure expects this address to be advertised by the cluster.  However, the cluster administrators would like to break up the address range and allocate a subset of the addresses between two development teams.  The configuration above allocates half of the address space to two teams, leaving half unallocated for future use, advertising a single route _192.168.1.0/24_ to the network.

In certain cases it can be beneficial to advertise a host route, which is a specific route for one address:
```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: highavail
  namespace: purelb
spec:
  local:
    v4pools:
    - subnet: '172.30.0.144/28'
      pool: '172.30.0.144/28'
      aggregation: /32
    v6pools:
    - subnet: fc00:370:155:0:8000:1::/112
      pool: fc00:370:155:0:8000:1::/112
      aggregation: /128
```
In this example every address allocated from the pool will add a route to the routing table, _172.30.0.144/32_.  This functionality is useful when combined with `ExternalTrafficPolicy: Local`.  Note that some routers will not accept /32 routes over BGP and the upstream routers at your ISP will most certainly reject this route by configuration.  PureLB offers a couple of alternatives: waste a few addresses using an aggregator of /30 when the router does not allow /32 routes over BGP, or use an IGP such as OSPF instead to provide rapid failover for individual service changes.

### Modifying ServiceGroups
Changing a ServiceGroup does not change services that have already been created. Modified ServiceGroups will only impact services subsequently created. This is intentional: service address changes should happen service by service, not by an address range change in the ServiceGroup having the side effect of changing all of the associated services' external addresses. To migrate service addresses, add an additional service that will be allocated an address from the new pool; once traffic has been drained, remove the original service which will release the old address.
