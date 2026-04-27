---
title: "ServiceGroup (Address Pools)"
description: "Configure IP address pools for PureLB using ServiceGroup CRDs."
weight: 10
---

A ServiceGroup defines a pool of IP addresses that PureLB can allocate to LoadBalancer Services. Each ServiceGroup specifies exactly one pool type: `local`, `remote`, or `netbox`.

## The Default ServiceGroup

PureLB uses the ServiceGroup named `default` when a Service has no `purelb.io/service-group` annotation. We recommend creating one ServiceGroup named `default` for your most common use case.

ServiceGroups are namespaced. We recommend placing them in `purelb-system`, but they can be created in other namespaces if RBAC requires it.

## Local Pools

Use `spec.local` when the pool addresses are on the **same subnet** as your cluster nodes. Addresses are announced on the node's physical interface via ARP/NDP.

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
      aggregation: default
    v6pools:
    - subnet: fd00:1::/64
      pool: fd00:1::f0-fd00:1::ff
      aggregation: default
```

## Remote Pools

Use `spec.remote` when the pool addresses are on a **different subnet** from your cluster nodes. Addresses are announced on the dummy interface (`kube-lb0`) and advertised via BGP. Requires [k8gobgp]({{< relref "/docs/configuration/bgp" >}}).

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: routed
  namespace: purelb-system
spec:
  remote:
    v4pools:
    - subnet: 172.31.0.0/24
      pool: 172.31.0.1-172.31.0.100
      aggregation: /32
    v6pools:
    - subnet: fd00:2::/112
      pool: fd00:2::1-fd00:2::64
      aggregation: /128
```

## Netbox Pools

Use `spec.netbox` to allocate addresses from an external Netbox IPAM system. See [Netbox IPAM Integration]({{< relref "/docs/configuration/netbox" >}}) for details.

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: enterprise
  namespace: purelb-system
spec:
  netbox:
    url: https://netbox.example.com/api
    tenant: kubernetes-cluster
```

## Address Pool Fields

Each pool (in `v4pools`, `v6pools`, `v4pool`, or `v6pool`) contains:

Field | Type | Required | Description
------|------|----------|------------
`pool` | string | Yes | Range of addresses. CIDR (`192.168.1.240/29`) or range (`192.168.1.240-192.168.1.250`).
`subnet` | string | Yes | CIDR of the network containing the pool (e.g., `192.168.1.0/24`). All pool addresses must be within this subnet.
`aggregation` | string | No | Controls the address mask. `default` uses the subnet mask. A value like `/32` or `/128` creates host routes.

### Singular vs Array Fields

For convenience, you can use singular fields when you have one pool per family:

```yaml
spec:
  local:
    v4pool:
      subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
      aggregation: default
```

Use the array form (`v4pools`, `v6pools`) when you need multiple ranges:

```yaml
spec:
  local:
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.245
      aggregation: default
    - subnet: 192.168.1.0/24
      pool: 192.168.1.246-192.168.1.250
      aggregation: default
```

## Dual-Stack

A ServiceGroup can contain both `v4pools` and `v6pools`. The Allocator allocates one address per requested IP family. The Local Pools example above shows a dual-stack configuration.

## Aggregation

Aggregation controls the address mask applied to the interface, which determines what routes are created in the kernel routing table.

Aggregation | Example Address | Address on Interface | Route Created
------------|----------------|---------------------|---------------
`default` | 192.168.1.240 | 192.168.1.240/24 | 192.168.1.0/24
`/25` | 192.168.1.240 | 192.168.1.240/25 | 192.168.1.128/25
`/32` | 192.168.1.240 | 192.168.1.240/32 | 192.168.1.240/32

For remote pools with BGP, `/32` (IPv4) or `/128` (IPv6) aggregation creates host routes, giving the finest control over route advertisement and withdrawal. This is required for `externalTrafficPolicy: Local` to work correctly with remote addresses.

> [!NOTE]
> Some BGP routers reject `/32` routes by default. If your upstream router filters these, use `/30` or configure the router to accept host routes.

## Multi-Pool Allocation

When `multiPool: true`, a service gets one IP from **each address range** (per family) that has active nodes. This makes the service reachable from every subnet in a multi-subnet cluster.

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: multi-subnet
  namespace: purelb-system
spec:
  local:
    multiPool: true
    v4pools:
    - subnet: 192.168.1.0/24
      pool: 192.168.1.240-192.168.1.250
      aggregation: default
    - subnet: 192.168.2.0/24
      pool: 192.168.2.240-192.168.2.250
      aggregation: default
```

## Balanced Allocation

When `balancePools: true`, new allocations pick the range with the fewest IPs currently in use, distributing services evenly across ranges.

> [!WARNING]
> `multiPool` and `balancePools` are mutually exclusive. Setting both is a validation error.

## Skip IPv6 DAD

Setting `skipIPv6DAD: true` (local pools only) disables IPv6 Duplicate Address Detection. This speeds up address configuration but should only be used when you are certain there are no address conflicts on the network.

## Modifying ServiceGroups

Changing a ServiceGroup does **not** change services that have already been allocated. Modified ServiceGroups only affect subsequently created services. This is intentional: address changes should happen service by service, not by a pool change affecting all associated services.

To migrate a service to a new pool: create a new service pointing to the new ServiceGroup, redirect traffic, then delete the original service.

## Complete Field Reference

See the [CRD Reference]({{< relref "/docs/reference/crd-reference#servicegroup" >}}) for the complete field-by-field specification.
