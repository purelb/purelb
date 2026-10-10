# k8gobgp issues for v0.17.0

PureLB v0.17.0 bundles **k8gobgp v0.2.8** (gobgp-netlink v1.3.8). The open
findings were verified against the v0.2.5 tag and re-checked at v0.2.7 and
v0.2.8: the CRDs, RBAC and DaemonSet contract are unchanged since v0.2.5; v0.2.7
removes the controller's gRPC metrics poll loop (and its `--metrics-poll-interval`
flag); v0.2.8 changes no CRD, API or metric.

## Resolved in v0.2.5

The four issues filed against v0.2.4 are fixed (all in `d2be7a3`):

| Issue | Was | Now |
|---|---|---|
| purelb/k8gobgp#35 | `k8gobgp_neighbors_established{name,namespace}` always 0 | metric removed; use `bgp_peer_state` on 7475 |
| purelb/k8gobgp#36 | `*_configured` gauges used unfiltered CRD counts | `k8gobgp_configured_objects{kind}`, node-filtered |
| purelb/k8gobgp#37 | README metrics table inaccurate | table rewritten |
| purelb/k8gobgp#40 | "unspecified defined type" error on every start | `ListDefinedSet` called per type |

## Resolved in v0.2.6

| Issue | Was | Now |
|---|---|---|
| purelb/gobgp-netlink#57 | Regression in v0.2.5 (gobgp-netlink v1.3.x; v0.2.4 was fine): an IPv4 route over an IPv6 session was sent with a 16-byte NEXT_HOP (a 4-in-6 `::ffff:` address); FRR treats it as withdrawn, so no IPv4 VIP was advertised over IPv6-only peering | gobgp-netlink v1.3.6 sends a 4-byte IPv4 next hop. Verified on prox-purelb2: FRR installs the route with 5 ECMP next hops, no NEXT_HOP errors |

## Resolved in v0.2.7

| Issue | Was | Now |
|---|---|---|
| (no issue; fixed by purelb/k8gobgp#92) | Regression in v0.2.5/v0.2.6: with `spec.global.families` set, the metrics collector decoded gobgpd's family ordinals as `afi<<16\|safi`, every `GetTable` failed silently, and `k8gobgp_rib_routes` was never exported (no error recorded) | The count moved into gobgpd as `bgp_rib_paths{route_family}` on 7475, decoded next to the encoding; a family it can't read is reported in `promhttp_metric_handler_errors_total{cause="gathering"}` |
| purelb/k8gobgp#88 | `k8gobgp_rib_routes` was `Reset()` before the `GetTable` loop, so series briefly vanished | Gone with the poll loop: gobgpd builds `bgp_rib_paths` fresh on every scrape |

## Resolved in v0.2.8

| Issue | Was | Now |
|---|---|---|
| purelb/gobgp-netlink#61 (fixed in gobgp-netlink v1.3.8; k8gobgp#95) | Regression in v0.2.5 (gobgp-netlink v1.2.0's upstream v4.9.0 merge dropped two lines from `toPathApiUtil`): ListPath never flagged netlink-imported paths, so BGPNodeStatus reported every imported address `inRIB: false` and `kubectl purelb status` counted each as a "netlinkImport failure", although the routes were in the RIB and advertised | Imported paths are flagged again. Verified on prox-purelb2 with remote host routes in both families: `inRIB: true` on all 5 nodes, each address listed once, `status` reports "netlinkImport OK"; removing the VIP empties the list and withdraws the routes |
| (same gobgp-netlink v1.3.8 release) | Link-local next hops from a BGP peer were not installed in the kernel (the peer no longer recorded its interface), which hits unnumbered peering; `DeletePeer` by interface depended on the kernel neighbour table | Both restored. Not exercised by PureLB's e2e (no unnumbered peering on the test cluster) |

## Open against v0.2.8

| Issue | Finding | Effect on PureLB | PureLB handling |
|---|---|---|---|
| purelb/k8gobgp#82 | `k8gobgp_gobgpd_connection_status` is set to 1 after `grpc.NewClient`, which never dials. At v0.2.7 the metrics-controller site is gone; the reconciler still does it (`bgpconfiguration_controller.go:855-869`) | The gauge can read 1 while gobgpd isn't answering, resetting upstream's `K8GoBGPDaemonDisconnected` alert | Not relied on: e2e asserts sessions via `bgp_peer_state` |
| purelb/k8gobgp#83 | `GOBGPD_EXTRA_ARGS` is documented in `entrypoint.sh:74` but never read | No way to pass gobgpd flags (remote API opt-in, metrics tuning) | Unix-socket mode, which PureLB uses, is unaffected |
| purelb/k8gobgp#84 | `docs/metrics.md` migration table maps unreleased intermediate names | Upstream docs don't help v0.2.4 users | PureLB's migration guide carries the v0.2.4 → v0.2.8 table (no metric changed in v0.2.8) |
| purelb/k8gobgp#85 | Release notes call the CRD changes "additive" | A v0.2.4-valid BGPConfiguration can be rejected; v0.2.4 pods can't write BGPNodeStatus against the new CRD | Migration guide: `jq` pre-flight, CRD-with-image rollback |
| purelb/k8gobgp#86 | `"${NODE_IP}:7475"` is unbracketed, so gobgpd's metrics fail to bind on IPv6 nodes; only logged | No 7475 metrics on IPv6-primary nodes | PureLB sets `GOBGP_METRICS_HOST="[$(NODE_IP)]:7475"` |
| purelb/k8gobgp#87 | DaemonSet probes lack `timeoutSeconds`; kubelet's 1s undercuts the 3s `GetBgp` readiness check | Readiness flaps and a noisy error counter on busy nodes | PureLB sets `timeoutSeconds: 5` on startup and readiness |
| purelb/k8gobgp#89 | Release-notes pre-upgrade `jq` check runs `kubectl get bgpconfigurations` (resource is `configs`) and misses `authPassword: ""` | The check errors out, and misses the case it warns about | PureLB's pre-flight uses `bgpconfig` and `has("authPassword")` |
| purelb/k8gobgp#90 | `MultipleConfigurations` message says "per node"; the rule is cluster-wide (oldest wins) | Misleading for users | Migration guide says one per cluster |

## Image scan: `ghcr.io/purelb/k8gobgp:0.2.8` (trivy 0.75.0, 2026-10-07)

| Target | Findings |
|---|---|
| alpine 3.24.2 base | 1 MEDIUM: CVE-2026-85091, zlib `1.3.2-r0`, fixed in `1.3.2-r1` |
| `/usr/local/bin/manager` | 0 |
| `/usr/local/bin/gobgpd`, `/usr/local/bin/gobgp` | 8 each (4 HIGH, 4 MEDIUM, 0 CRITICAL): CVE-2026-30405, -37461, -37462, -41643, -49837, -49838, -7734, -7736 |

The zlib finding is a newly published advisory, not a regression in v0.2.8:
rescanning `0.2.7` with the same database reports it too (the 2026-10-03 scan of
0.2.7 found 0 in the base). The fix is a k8gobgp image rebuild on zlib
`1.3.2-r1`; no k8gobgp issue is filed yet.

The 16 gobgp findings are matched against module `github.com/osrg/gobgp/v4` at
the fork's pseudo-version `v4.0.0-20261005225620-6b99046fdd09`, with fixed
versions 4.3.0-4.7.0. The fork reports `base: gobgp-4.9.0`, newer than every
fixed version, so these are probably false positives of the pseudo-version. Not
confirmed fix by fix.
