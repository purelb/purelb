---
title: "v0.16.x to v0.17.0"
description: "Upgrade an existing purelb.io/v2 install to v0.17.0: removed API fields, ServiceGroup namespace scoping, and manifest changes."
weight: 10
---

This page covers upgrading an install that is **already on `purelb.io/v2`**
(v0.16.x) to v0.17.0.

If you are coming from v0.13 or any other pre-`v2` release, follow the
[v1 to v2 migration guide]({{< ref "/docs/migration" >}}) instead — it
supersedes this page and installs v0.17.0 directly.

## Read this first

**v0.17.0 changes the `v2` API in place.** There is no version bump and no
conversion webhook: `v2` remains the served and stored version, and fields
have been removed from it.

The practical consequence is that the *order* of the upgrade matters. Objects
that use a removed field are rejected by the new CRDs, so find them before you
apply anything.

## Pre-upgrade check

Run both queries. Empty output from each means nothing on this page needs
action from you.

```bash
# ServiceGroups still using the removed in-tree Netbox IPAM
kubectl get servicegroups.purelb.io -A -o json |
  jq -r '.items[] | select(.spec.netbox != null)
         | "netbox: \(.metadata.namespace)/\(.metadata.name)"'

# ServiceGroups outside the install namespace — these stop being read.
# Change purelb-system if you installed PureLB somewhere else.
kubectl get servicegroups.purelb.io -A -o json |
  jq -r '.items[] | select(.metadata.namespace != "purelb-system")
         | "out-of-scope: \(.metadata.namespace)/\(.metadata.name)"'
```

Take a backup regardless — you will want it if you roll back:

```bash
kubectl get servicegroups.purelb.io -A -o yaml > servicegroups-pre-0.17.yaml
kubectl get lbnodeagents.purelb.io -A -o yaml > lbnodeagents-pre-0.17.yaml
```

## Removed fields

| Removed | Replacement |
|---|---|
| `spec.netbox` | `spec.external` — an IPAM sidecar speaking the gRPC IPAM contract |
| `status.allocatedCount` | `status.allocatedIPv4` / `status.allocatedIPv6`, plus `availableIPv4` / `availableIPv6` |

`spec.netbox` is not merely ignored. The new CRD requires exactly one of
`local`, `remote` or `external`, so a ServiceGroup that still sets `netbox` is
**rejected on its next write**, and the field is pruned when the object is read
back. Convert those groups to a sidecar — see
[External IPAM]({{< ref "/docs/configuration/external-ipam" >}}) — or delete
them. Either way they allocate nothing once v0.17.0 is running.

`status.allocatedCount` is gone rather than renamed, so anything reading it —
dashboards, scripts, alert rules — gets nothing rather than a wrong number.
Move those to the per-family fields.

## ServiceGroups are read only from the install namespace

Earlier releases read ServiceGroups from every namespace. That provided no
isolation — a group in `tenant-a` fed the same cluster-wide pool table — while
allowing two groups in different namespaces to collide over one pool name,
including `default`.

A ServiceGroup outside the install namespace is now ignored. Its pool is not
allocatable and, **if it is a `remote` group, its addresses are withdrawn from
every node.**

The fix is to move the object into the install namespace. Addresses return on
the next reconcile, and the two copies may coexist while you move it.

PureLB reports ignored groups three ways: a log line, a Warning event on the
group itself, and the metric
`purelb_servicegroups_out_of_scope{namespace,name,type}`.

This scoping depends on the allocator knowing its own namespace. It reads
`--namespace` / `PURELB_NAMESPACE`, which the chart and the kustomize
manifests supply via the downward API. If it cannot be determined, PureLB does
**not** filter at all rather than risk discarding every ServiceGroup, and says
so in its log.

## Manifest and chart changes

- The allocator Deployment is pinned to `replicas: 1` with
  `strategy: Recreate`. Under the previous `RollingUpdate`, `maxSurge` rounds
  up to 1, so every upgrade briefly ran two allocators handing out addresses
  from the same pools. A short gap in allocation is the safer trade.
- `PURELB_NAMESPACE` is added to the allocator via the downward API.
- `NETBOX_USER_TOKEN` and its `netbox-client` Secret reference are removed
  from the allocator Deployment. The Secret itself is not deleted for you.
- The allocator ClusterRole gains `servicegroups/status` (`update`, `patch`).
- Both ClusterRoles drop an unused `namespaces` (`get`, `list`) grant.

**If you maintain your own RBAC** rather than using the shipped ClusterRoles,
add `servicegroups/status` before upgrading. Without it every status write is
refused: allocation and announcement keep working, but ServiceGroup `.status`
stays empty and
`purelb_allocator_sg_status_writes_total{outcome="forbidden"}` climbs.

## Announcing annotation format

`purelb.io/announcing-<family>` is now keyed by IP address. Each entry is
`node,interface,ip`, and entries are space-separated:

```yaml
purelb.io/announcing-IPv4: "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11"
```

Keying by IP is what lets a dual-stack or multi-pool Service record a
different announcing node per address. The earlier one-field (`kube-lb0`) and
two-field (`node,iface`) forms are dropped on read and do not survive the
first write.

No action is required — the node agents rewrite the annotation as they
reconcile — but anything parsing it needs updating.

## Rolling back to v0.16.x

Reinstall the v0.16.x CRDs and workloads.

Because `v2` changed in place, a ServiceGroup created against v0.17.0 that
uses `spec.external` fails validation on the older CRD. Delete those objects
before rolling back, or restore the backup you took above:

```bash
kubectl apply -f servicegroups-pre-0.17.yaml
kubectl apply -f lbnodeagents-pre-0.17.yaml
```
