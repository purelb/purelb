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

## BGP: k8gobgp v0.2.6

v0.17.0 ships k8gobgp v0.2.6 (from v0.2.4). It moves to gobgp-netlink v1.3.6
and changes metrics, validation and some behaviour. Remote pools without BGP are
unaffected.

IPv4 routes can now be advertised over an IPv6 BGP session. Before
gobgp-netlink v1.3.6 they were sent with a malformed 16-byte NEXT_HOP, which
the router discards (FRR logs `Nexthop attribute length isn't four`), so a
cluster peering only over IPv6 advertised no IPv4 addresses.

### Check your BGPConfiguration before upgrading

v0.2.6's CRD validates more strictly, and some fields that v0.2.4 accepted and
**ignored** now take effect. Run this before applying the new CRDs:

```bash
kubectl get bgpconfig -A --sort-by=.metadata.creationTimestamp
kubectl get bgpconfig -A -o json | jq -r -f bgp-preflight.jq
```

with `bgp-preflight.jq`:

```jq
def gf: ["ipv4-unicast","ipv6-unicast","ipv4-labeled","ipv6-labeled","ipv4-vpn","ipv6-vpn","l2vpn-vpls","l2vpn-evpn"];
def nf: gf + ["ipv4-flowspec","ipv6-flowspec","rtc"];
.items[] | (.metadata.namespace + "/" + .metadata.name) as $cr | .spec as $s |
(
  (($s.global.families // [])[] | select(. as $f | gf | index($f) | not)
    | "REJECTED  global.families has \(.) (lowercase, supported values only)"),
  (([$s.neighbors[]?, $s.peerGroups[]?] | .[] | .afiSafis[]?.family) | select(. as $f | nf | index($f) | not)
    | "REJECTED  afiSafis family \(.) (lowercase, supported values only)"),
  ($s | .. | objects | select(has("restartTime")) | select(.restartTime > 4095)
    | "REJECTED  gracefulRestart.restartTime \(.restartTime) > 4095"),
  (($s.global.listenAddresses // [])[] | select(test("^[0-9a-fA-F:.]+(%[0-9a-zA-Z._-]+)?$") | not)
    | "REJECTED  global.listenAddresses \(.) is not an IP address"),
  ($s.global | select(.useMultiplePaths == true and ((.ebgpMaximumPaths // 0) == 0) and ((.ibgpMaximumPaths // 0) == 0))
    | "NO BGP    global.useMultiplePaths needs ebgpMaximumPaths or ibgpMaximumPaths, or BGP will not start"),
  ($s.global | ("applyPolicy","gracefulRestart","bindToDevice","routeSelectionOptions","confederation") as $k
    | select(has($k)) | "NOW APPLIED global.\($k) was ignored before v0.2.5 and now takes effect"),
  ($s | .. | objects | select(has("prefixLimit") or has("addPaths"))
    | "NOW APPLIED afiSafis \(.family) prefixLimit/addPaths were ignored before and now take effect"),
  ($s | .. | objects | select(.match? == "all" or .match? == "invert")
    | "NOW APPLIED a policy condition with match: \(.match) (was treated as any)"),
  ($s | .. | objects | select(.type? == "remove" or .type? == "replace")
    | "NOW APPLIED a community/MED action of type \(.type) (was ignored)"),
  ($s.neighbors[]? | select(.config.peerGroup? and (.config | has("localAsn") or has("authPassword") or has("description")))
    | "SESSIONS  grouped neighbor \(.config.neighborAddress) sets its own \([.config | keys[] | select(. == "localAsn" or . == "authPassword" or . == "description")] | join(", ")), which now overrides the peer group (authPassword: \"\" removes the group password)")
) | "\($cr): \(.)"
```

- `REJECTED`: the object breaks a new validation rule. Fix it **before**
  applying the new CRDs; on API servers older than 1.30 an invalid object can
  no longer be updated, which also blocks its finalizer and its deletion.
- `NO BGP`: k8gobgp refuses the configuration and the node runs no BGP at all.
- `NOW APPLIED`: the setting was silently ignored and starts working, which can
  change what is advertised or filtered. Check it is what you want.
- `SESSIONS`: a grouped neighbor's own `localAsn`, `authPassword` or
  `description` now overrides its peer group, which can drop the session.
  An explicit `authPassword: ""` now means "no password", not "inherit".

Only one BGPConfiguration is honoured: the oldest. Others report
`Ready=False` (`MultipleConfigurations`) and their neighbors are not
configured. If the first command lists more than one, merge them first.

### Behaviour changes

- Changing a neighbor's `timers.config.holdTime` or `keepaliveInterval`, or its
  BFD settings, resets the session. On a peer group it resets every member at
  once; enable graceful restart first.
- Global settings only apply when the pod starts. An edit afterwards is
  reported by `k8gobgp_global_restart_required` until the lbnodeagent pod is
  restarted.
- A neighbor's `nodeSelector` reacts to node label changes immediately, adding
  or dropping the session.
- Some fields are accepted but inert and log an `InertSettings` warning event:
  `global.defaultRouteDistance`, `routeSelectionOptions.advertiseInactiveRoutes`,
  `enableAigp`, `ignoreNextHopIgpMetric`, `timers.config.minimumAdvertisementInterval`,
  `routeFlapDamping`.
- The k8gobgp readiness probe now asks gobgpd whether it is answering. It does
  not check BGP sessions: Ready does not mean sessions are up, and NotReady does
  not affect local (non-BGP) announcements.
- The lbnodeagent DaemonSet now sets `minReadySeconds: 30`, so a rollout waits
  30 seconds per node for BGP sessions to re-establish. For a hand-paced canary,
  set `updateStrategy.type: OnDelete`, delete one pod, check
  `kubectl purelb bgp sessions`, then continue.

### Metrics

gobgpd now serves its own metrics on a new port, **7475** (`bgp_*`), and the
k8gobgp controller (7473) drops the per-peer metrics it used to copy from it:

v0.2.4 (port 7473) | v0.17.0
---|---
`k8gobgp_neighbors_established_total` | `count(bgp_peer_state{session_state="SESSION_STATE_ESTABLISHED"})` (7475)
`k8gobgp_neighbors_established{name,namespace}` | removed (it was always 0); use the line above
`k8gobgp_neighbors_total`, `_active`, `_idle` | `count(bgp_peer_state)`, by `session_state` (7475)
`k8gobgp_neighbors_configured`, `k8gobgp_peer_groups_configured`, `k8gobgp_dynamic_neighbors_configured`, `k8gobgp_vrfs_configured`, `k8gobgp_policies_configured`, `k8gobgp_defined_sets_configured` | `k8gobgp_configured_objects{kind="neighbor"\|"peer_group"\|"dynamic_neighbor"\|"vrf"\|"policy"\|"defined_set"}`
`k8gobgp_rib_route_count{family}` | `k8gobgp_rib_routes{family}` (same `family` values)
`k8gobgp_routes_received_total`, `_accepted_total`, `_advertised_total` | `sum(bgp_routes_received)`, `sum(bgp_routes_accepted)`, `sum(bgp_routes_advertised)` (7475, labelled `peer`, `route_family`)
`k8gobgp_neighbor_routes_*{neighbor,family}` | `bgp_routes_*{peer,route_family}` (7475)
`k8gobgp_router_id_source{source}` | `count by (source) (k8gobgp_router_id_info)`
`k8gobgp_nodestatus_last_successful_write_timestamp` | `k8gobgp_nodestatus_last_successful_write_timestamp_seconds`
`k8gobgp_metrics_collection_skipped_total`, `k8gobgp_metrics_cardinality_limit_hit_total` | removed

Label values differ between the ports: `family="ipv4_unicast"` on 7473,
`route_family="ipv4-unicast"` on 7475. `k8gobgp_router_id_info` gained `name`
and `namespace` labels. New on 7473: `k8gobgp_peer_apply_errors_total` (a peer
gobgpd refused) and `k8gobgp_global_restart_required`.

The k8gobgp metrics poll interval is now 60s (was 15s), so `k8gobgp_rib_routes`
lags by up to a minute; the `bgp_*` metrics on 7475 are at most 15s old.

**Port names changed** to match upstream k8gobgp: 7473 is now `metrics` (was
`gobgp-metrics`), 7474 is `health` (was `gobgp-health`), and the new 7475 is
`gobgp-metrics`. A PodMonitor or ServiceMonitor that selected port
`gobgp-metrics` keeps scraping without error but now gets gobgpd's `bgp_*`
metrics instead of `k8gobgp_*`, so `k8gobgp_*` alerts go quiet. Find them with:

```bash
kubectl get podmonitors,servicemonitors -A -o yaml | grep -nE 'gobgp-(metrics|health)'
```

### Upgrade order

Apply the CRDs, then the workloads. On Helm, apply the CRDs by hand: Helm never
upgrades them (see [Upgrading]({{< relref "/docs/installation/helm#upgrading" >}})).
While the rollout is in progress, an old v0.2.4 pod cannot write an unhealthy
node's BGPNodeStatus against the new CRD (`healthy` is now required); this
clears as each pod is replaced.

## Rolling back to v0.16.x

Reinstall the v0.16.x CRDs and workloads.

Because `v2` changed in place, a ServiceGroup created against v0.17.0 that
uses `spec.external` fails validation on the older CRD. Delete those objects
before rolling back, or restore the backup you took above:

```bash
kubectl apply -f servicegroups-pre-0.17.yaml
kubectl apply -f lbnodeagents-pre-0.17.yaml
```

**Roll back the BGP CRDs together with the workloads.** The v0.16.x manifests
carry k8gobgp v0.2.4's CRDs; re-apply them along with the v0.16.x workloads.
A v0.2.4 sidecar running against v0.2.6's CRDs cannot write an unhealthy
node's BGPNodeStatus (`healthy` is now required), so `kubectl get
bgpnodestatus` would keep showing the last healthy state. Fields added in
v0.2.5 and later (for example `ebgpMaximumPaths`) are dropped by the older CRD.
