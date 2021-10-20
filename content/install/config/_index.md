---
title: "Initial Configuration"
description: "Describe Operation"
weight: 15
hide: toc, nextpage
---

PureLB configuration is simple and uses Custom Resources.  A single LBNodeAgent CR enables default overrides, and as many ServiceGroup CRs as needed configure external IP address ranges.

```plaintext
$ kubectl api-resources --api-group=purelb.io
NAME            SHORTNAMES   APIGROUP    NAMESPACED   KIND
lbnodeagents    lbna         purelb.io   true         LBNodeAgent
servicegroups   sg           purelb.io   true         ServiceGroup
```

### LBNodeAgent
The installation procedure will have created lbnodeagent.purelb.io/default. In most cases these defaults are suitable.

```plaintext
$ kubectl describe --namespace=purelb lbnodeagent.purelb.io/default
Name:         default
Namespace:    purelb
Kind:         LBNodeAgent
Spec:
  Local:
    extlbint:  kube-lb0
    localint:  default
```
parameter | type | Description
-------|----|---
extlbint | An interface name | The name of the virtual interface used for virtual addresses.  The default is kube-lb0. If you change it, and are using the purelb bird configuration, make sure you update the bird.cm.
localint | An interface name | PureLB automatically identifies the interface that is connected to the local network and the address range used.  The default setting enables this automatic functionality.  If you wish to override this functionality and specify the interface to which PureLB will add local addresses, specify it here. Currently it must be uniform throughout the cluster. You need to make sure that this interface has appropriate routing. The algorithmic selector finds the interface with the lowest-cost default route, i.e., the interface that is most likely to have global communications.

## ServiceGroup
ServiceGroup CRs contain the configuration required to allocate Load Balancer addresses.  Service groups are Custom Resources and their contents depend on the source of the allocated addresses.  In the case of locally allocated addresses, pools of addresses are contained in the ServiceGroup. In the case of NetBox the ServiceGroup contains the configuration necessary to contact Netbox so the allocator can return an address for use.

### Integrated IPAM
{{% notice danger %}} Note: PureLB requires the creation of at least one service group for operation {{% /notice %}}
**PureLB does not install a default service group and requires a default service group for operation as per the example below.**  The default group is used where no purelb.io/service-group annotation is present in the service definition.

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb
spec:
  local:
    subnet: '192.168.1.0/24'
    pool: '192.168.1.100-192.168.1.200'
    aggregation: default
```

A Service Group is configured for each pool of addresses needed by the k8s cluster. 

parameter | type | Description
-------|----|---
subnet | IPv4 or IPv6 CIDR.|  Configured with the subnet that the pool addresses will be allocated.  PureLB uses this information to compute how the address is added to the cluster.
pool | ipv4 or IPv6 CIDR or range | The specific range of addresses that will be allocated.  Can be expressed as a CIRD or range of addresses.
Aggregation | "default" or int 8-128 | The aggregator changes the address mask of the allocated address from the subnet mask to the specified mask.

#### Configuring Aggregation
Aggregation configuration brings a capability commonly used in routers to control how addresses are advertised.  When a Service Group is defined with _aggregation: default_ the IPNET mask will be used from the subnet. PureLB will create IPNET from the allocated address and subnets mask that is applied to the appropriate interface.  Put simply, the services API provides an address, _192.168.1.100_, if _aggregation: default_ the resulting address applied to the interface by PureLB will be _192.168.151.100/24_. (added to the local interface if it matches the subnet otherwise the virtual interface).  Adding an address to an interface also updates the routing table, therefore if its a new network, (not new address), a new routing table entry is added.  This is how routes are further distributed into the network via the virtual interface and node routing software.

The primary purpose of of Aggregation is to change the way that routing distributes the address.  Changing the aggregator impacts when the IPNET is added to the routing table.  

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb
spec:
  local:
    subnet: '192.168.1.0/24'
    pool: '192.168.1.100-192.168.1.200'
    aggregation: /25
```
In the example above, the range is further subnetted by changing the aggregator.  As the allocator adds address, when the first address is allocated, the routing table will be updated with _192.168.1.0/25_ when the allocator _192.168.1.129_, the routing table will be updated with _192.168.1.128/25_  This example is somewhat academic but illustrates the use.

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: Team-1
  namespace: purelb
spec:
  local:
    subnet: '192.168.1.0/26'
    pool: '192.168.1.0-192.168.1.62'
    aggregation: /24
---
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: Team-2
  namespace: purelb
spec:
  local:
    subnet: '192.168.1.64/26'
    pool: '192.168.1.64-192.168.1.126'
    aggregation: /24
```
In the example above, the k8s cluster has been allocated the address range of _192.168.1.0/24_ and the network infrastructure expects this address to be advertized by the cluster.  However, the cluster administrators would like to break up the address range and allocate a subset of the addresses between two development teams.  The configuration above allocates half of the address space to two teams, leaving half unallocated for future use, advertizing a single router _192.168.1.0/24_ to the network.

```yaml
apiVersion: purelb.io/v1
kind: ServiceGroup
metadata:
  name: highavail
  namespace: purelb
spec:
  local:
    subnet: '172.30.0.144/28'
    pool: '172.30.0.144/28'
    aggregation: /32
```
In certain cases it can be beneficial to advertize a host route, this is a specific route for one address.  In this example every address allocated from the pool will add a route to the routing table, _172.30.0.144/32_.  This functionality is useful when combined with ExternalTrafficPolicy:Local.  Note that some routers will not accept /32 routes over BGP and the upstream routers at your ISP will most certainly reject this route by configuration.  PureLB offers a couple of alternatives, waste a few addresses using an aggregator of /30 when the router does not allow /32 routes over BGP or use an IGP such as OSPF instead to provide rapid failover for individual service changes.

### Creating the Service Group
Service Groups are a custom resource, the following creates a service group

```plaintext
$ kubectl apply -f myservicegroup.yml
$ kubectl --namespace=purelb describe sg virtualsg
Name:         virtualsg
Namespace:    purelb
Labels:       <none>
Annotations:  <none>
API Version:  purelb.io/v1
Kind:         ServiceGroup
Metadata:
  Creation Timestamp:  2020-09-20T20:49:01Z
  Generation:          1
  Managed Fields:
    API Version:  purelb.io/v1
    Fields Type:  FieldsV1
    fieldsV1:
      f:metadata:
        f:annotations:
          .:
          f:kubectl.kubernetes.io/last-applied-configuration:
      f:spec:
        .:
        f:local:
          .:
          f:aggregation:
          f:pool:
          f:subnet:
    Manager:         kubectl-client-side-apply
    Operation:       Update
    Time:            2020-09-20T20:49:01Z
  Resource Version:  3573405
  Self Link:         /apis/purelb.io/v1/namespaces/purelb/servicegroups/virtualsg
  UID:               0ff8be8d-f21c-45a0-a10a-2171da7aa058
Spec:
  Local:
    Aggregation:  default
    Pool:         172.31.0.0/24
    Subnet:       172.31.0.0/24
Events:           <none>
```
Service groups are namespaced, however PureLB will check all namespaces.  For simplicity we recommend adding them to the purelb namespace however in cases where RBAC controls who can update namespace, service groups can to the namespaces of their users.

### Changing a Service Group
Changing a Service Group does not affect the services that have already been created.  The modified service group will only impact services subsequently created.  This is intentional: service address changes should be initiated on a per service basis, not by an address range change in the service group having the side effect of changing all of the associated services' external addresses.
