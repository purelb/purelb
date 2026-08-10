---
title: "ServiceGroup (Address Pools)"
description: "Configure IP address pools for PureLB using ServiceGroup CRDs."
weight: 10
---

A ServiceGroup defines a pool of IP addresses that PureLB can allocate to LoadBalancer Services. Each ServiceGroup specifies exactly one pool type: `local`, `remote`, or `external`.

## The Default ServiceGroup

PureLB uses the ServiceGroup named `default` when a Service has no `purelb.io/service-group` annotation. We recommend creating one ServiceGroup named `default` for your most common use case.

ServiceGroups are namespaced objects, but **PureLB reads them only from its own install namespace** (normally `purelb-system`). A ServiceGroup created anywhere else is ignored entirely: its pool is not allocatable, and if it is a `remote` group, addresses already allocated from it are withdrawn from every node.

{{< hint warning >}}
**Changed in v0.17.0.** Earlier releases read ServiceGroups from every namespace. That never provided any isolation — a ServiceGroup in `tenant-a` contributed a pool to the same cluster-wide table as one in `purelb-system` — while it did allow two ServiceGroups in different namespaces to collide over a pool name, including the name `default`.

Before upgrading, check for any that would be ignored:

```console
kubectl get servicegroups.purelb.io -A
```

To move one, apply it into the install namespace and delete the original. The two may safely coexist during the move, because the out-of-namespace copy is not read:

```console
kubectl get servicegroup <name> -n <other-ns> -o yaml \
  | sed 's/namespace: <other-ns>/namespace: purelb-system/' \
  | kubectl apply -f -
kubectl delete servicegroup <name> -n <other-ns>
```

Addresses return on the next reconcile. PureLB logs and emits a Warning event naming any ServiceGroup it ignores, and reports them in `purelb_servicegroups_out_of_scope`.
{{< /hint >}}

## Restricting a ServiceGroup to Namespaces

{{< hint danger >}}
**The primary tenancy control is RBAC, not this feature.** Restrict `create` and `patch` on `servicegroups.purelb.io` to cluster administrators. Anyone who can write a ServiceGroup can bind their own namespace to whatever they like, or define a pool over anyone's addresses — at which point everything below is decorative. That one ClusterRole rule matters more than every field on this page.

Kubernetes RBAC is verb × resource: it cannot see annotation *values*, so there is no way to express "may create Services but may not set `purelb.io/service-group: tenant-b-pool`". That is why the rest of this has to be enforced by the allocator.
{{< /hint >}}

`spec.namespaces` lists the Service namespaces a ServiceGroup serves. A Service in a listed namespace that carries no `purelb.io/service-group` annotation allocates from it. Omitting the field means every namespace, which is how every ServiceGroup behaved before v0.17.0.

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: tenant-a-pool
  namespace: purelb-system
spec:
  namespaces: [tenant-a, tenant-a-staging]
  local:
    v4pools:
    - subnet: 172.30.250.0/24
      pool: 172.30.250.100-172.30.250.150
    v6pools:
    - subnet: 2001:470:b8f3:250::/64
      pool: 2001:470:b8f3:250::100-2001:470:b8f3:250::150
```

**By itself this is a default, not a boundary.** An annotation still overrides it in both directions: a Service in `tenant-a` may ask for another ServiceGroup, and a Service elsewhere may ask for this one. Adding `namespaces` to a live cluster therefore changes nothing that already works.

### Several ServiceGroups may serve one namespace

This is normal, not a misconfiguration. A ServiceGroup is exactly one of `local`, `remote` or `external`, so a tenant that needs both L2 and BGP addresses needs **two** ServiceGroups, both listing its namespace. `spec.namespaces` establishes eligibility, not ownership.

Where two or more serve a namespace, `spec.namespaceDefault` picks which one an unannotated Service gets. Where only one does, it is the default and the field is ignored.

```yaml
# tenant-a-l2                          # tenant-a-bgp
spec:                                  spec:
  namespaces: [tenant-a]                 namespaces: [tenant-a]
  namespaceDefault: true                 remote:
  local:                                   v4pools: [...]
    v4pools: [...]
```

An unannotated Service in `tenant-a` gets `tenant-a-l2`; one annotated `purelb.io/service-group: tenant-a-bgp` gets the BGP pool.

If several serve a namespace and **none** is marked, an unannotated Service falls back to the `default` pool and PureLB emits a `ConfigurationWarning` on the Service naming the candidates. Nothing breaks, but the Service may land on a different subnet than you intended — mark one.

### Making it a boundary

`spec.enforceNamespaces: true` turns the list from a default into a boundary. A Service outside the list cannot reach the ServiceGroup by annotation **or** by pinning one of its addresses with `purelb.io/addresses`; and a Service inside cannot allocate from any ServiceGroup that does not serve its namespace.

{{< hint warning >}}
**Enabling this enforces nothing that is already allocated.** Addresses already held are never revoked, so on the day you turn it on the boundary is not true of any existing Service. It governs new allocations only. Existing Services are re-checked when they are recreated — which may be weeks later, during an unrelated redeploy.
{{< /hint >}}

Enforcement is a property of the **namespace**, not of one ServiceGroup. Setting `enforceNamespaces` on any one of a namespace's ServiceGroups fences that namespace completely, in both directions — otherwise fencing a tenant's L2 pool would leave its BGP pool reachable from everywhere. You do not need to set it on all of them.

This is an **allocation control, not RBAC**. PureLB ships no admission webhook, so a refused Service is created successfully and simply never gets an address; the reason appears as an `AllocationFailed` event on the Service.

### Things to know

- **Ranges must not overlap.** PureLB rejects a ServiceGroup whose addresses overlap an already-accepted one, so tenant pools partition the address space rather than sharing it.
- **A namespace list is a trust set, not an isolation set.** Namespaces listed on the same ServiceGroup can share each other's addresses via `purelb.io/allow-shared-ip` and block each other's ports.
- **Binding the `default` ServiceGroup while enforcing** denies every unannotated Service in every other namespace. It is legal, and almost never what you want.
- **PureLB does not verify that a listed namespace exists.** A typo simply never matches, and the namespace you meant falls back to `default`.
- **A namespace's ServiceGroups must between them cover the families you use.** `resolve` picks one ServiceGroup, and families are allocated within it — so a confined namespace whose pools are v4-only cannot satisfy a dual-stack Service.
- **`status.boundNamespaces`** reports the list the allocator parsed. It confirms what PureLB read, which is not the same question as what the API server accepted.

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

## External Pools

Use `spec.external` to allocate addresses from an external IPAM system via a
sidecar process. See [External IPAM (Sidecar)]({{< relref "/docs/configuration/external-ipam" >}}) for details.

```yaml
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: enterprise
  namespace: purelb-system
spec:
  external:
    provider: my-ipam       # cosmetic; surfaced in .status.ipam
    socket: /var/run/purelb/ipam.sock
    announce: remote        # "local" or "remote"
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

## ServiceGroup Status

PureLB writes a `.status` to every ServiceGroup it has parsed, and `kubectl get servicegroups` surfaces it:

```console
$ kubectl get servicegroups -n purelb-system
NAME      ANNOUNCE   IPAM      ADDRESSES                                    ALLOCATED-V4   ALLOCATED-V6
default   Local      Cluster   ["192.168.1.100-192.168.1.200","fc00::/120"] 3              1
```

`kubectl get servicegroups -o wide` adds the `Available-V4` and `Available-V6` columns.

| Field | Meaning |
|---|---|
| `announce` | How the addresses are announced: `Local` or `Remote` |
| `ipam` | Where addresses come from: `Cluster` for local pools, or the external provider name |
| `addresses` | The configured address ranges, as written in the spec |
| `allocatedIPv4` / `allocatedIPv6` | Addresses currently in use, per family |
| `availableIPv4` / `availableIPv6` | Addresses still free, per family. Absent when the pool's capacity is not knowable — an external IPAM provider that does not report a size |

Status is published as soon as the ServiceGroup is configured, so a new pool reports its addresses and capacity before anything has allocated from it. It is refreshed on every allocation and release, and whenever the ServiceGroup's spec changes.

{{< hint info >}}
**A blank status means PureLB did not accept the ServiceGroup.** If `kubectl get servicegroups` shows empty columns for a group, its spec failed to parse — check the allocator logs (`kubectl logs -n purelb-system deployment/allocator`) for the reason.
{{< /hint >}}

An IPv6-only pool reports `availableIPv4: 0`, and an IPv4-only pool reports `availableIPv6: 0`, because the pool has no capacity in that family. This is not the same as an exhausted pool — compare against `addresses` to tell them apart.

## Modifying ServiceGroups

Changing a ServiceGroup does **not** change services that have already been allocated. Modified ServiceGroups only affect subsequently created services. This is intentional: address changes should happen service by service, not by a pool change affecting all associated services.

To migrate a service to a new pool: create a new service pointing to the new ServiceGroup, redirect traffic, then delete the original service.

The same applies to `spec.namespaces` and `spec.enforceNamespaces`: editing them never revokes an address. A Service that no longer matches keeps what it has until it is recreated, so a namespace can hold addresses that its current binding would refuse. To move an existing Service without dropping its VIP, pin the new address with `purelb.io/addresses` rather than deleting and recreating it.

## Complete Field Reference

See the [CRD Reference]({{< relref "/docs/reference/crd-reference#servicegroup" >}}) for the complete field-by-field specification.
