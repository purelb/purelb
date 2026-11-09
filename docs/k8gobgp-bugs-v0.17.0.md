# k8gobgp issues for v0.17.0

PureLB v0.17.0 bundles **k8gobgp v0.2.9** (gobgp-netlink v1.3.10). The open
findings were verified against the v0.2.5 tag and re-checked at v0.2.7, v0.2.8
and v0.2.9: the CRDs, RBAC and DaemonSet contract are unchanged since v0.2.5;
v0.2.7 removes the controller's gRPC metrics poll loop (and its
`--metrics-poll-interval` flag); v0.2.8 and v0.2.9 change no CRD, API or metric
name (v0.2.9's install manifest differs from v0.2.8's only in the image tag).

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

## Resolved in v0.2.9

| Issue | Was | Now |
|---|---|---|
| purelb/gobgp-netlink#65 (fixed in gobgp-netlink v1.3.9) | With BFD on, a BGP session that dropped never got working BFD back: against FRR dynamic neighbors, gobgpd bound to a short-lived BFD session and rejected the real one, and a BFD expiry before Up reset BGP, so the peer flapped for as long as BFD stayed configured. PureLB's docs and `kubectl purelb validate` warned against BFD on routed egress | BFD expiry resets BGP only from Up; in Init gobgpd follows a new discriminator; a remote AdminDown or a Down with diagnostic 3 leaves BGP alone. PureLB's warnings are removed |
| (no issue; k8gobgp `2376a919`) | An unclean stop (power loss, SIGKILL, OOM kill) left `gobgp.sock` in the pod's emptyDir; the entrypoint took the file for a ready gobgpd, whose bind had failed, so gobgpd ran with no API on every restart until the pod was deleted | The entrypoint removes the socket before starting gobgpd and asks its API for readiness |

## Still present in v0.2.9

These were closed upstream on 2026-10-10 with no linked change, and no commit
between v0.2.8 and v0.2.9 touches them, so PureLB keeps its handling.

| Issue | Finding | Effect on PureLB | PureLB handling |
|---|---|---|---|
| purelb/k8gobgp#82 | `k8gobgp_gobgpd_connection_status` is set to 1 after `grpc.NewClient`, which never dials. At v0.2.7 the metrics-controller site is gone; the reconciler still does it (`bgpconfiguration_controller.go:855-869`) | The gauge can read 1 while gobgpd isn't answering, resetting upstream's `K8GoBGPDaemonDisconnected` alert | Not relied on: e2e asserts sessions via `bgp_peer_state` |
| purelb/k8gobgp#83 | `GOBGPD_EXTRA_ARGS` is documented in `entrypoint.sh:74` but never read | No way to pass gobgpd flags (remote API opt-in, metrics tuning) | Unix-socket mode, which PureLB uses, is unaffected |
| purelb/k8gobgp#84 | `docs/metrics.md` migration table maps unreleased intermediate names | Upstream docs don't help v0.2.4 users | PureLB's migration guide carries the v0.2.4 → v0.2.9 table (no metric name changed in v0.2.8 or v0.2.9) |
| purelb/k8gobgp#85 | Release notes call the CRD changes "additive" | A v0.2.4-valid BGPConfiguration can be rejected; v0.2.4 pods can't write BGPNodeStatus against the new CRD | Migration guide: `jq` pre-flight, CRD-with-image rollback |
| purelb/k8gobgp#86 | `"${NODE_IP}:7475"` is unbracketed, so gobgpd's metrics fail to bind on IPv6 nodes; only logged | No 7475 metrics on IPv6-primary nodes | PureLB sets `GOBGP_METRICS_HOST="[$(NODE_IP)]:7475"` |
| purelb/k8gobgp#87 | DaemonSet probes lack `timeoutSeconds`; kubelet's 1s undercuts the 3s `GetBgp` readiness check | Readiness flaps and a noisy error counter on busy nodes | PureLB sets `timeoutSeconds: 5` on startup and readiness |
| purelb/k8gobgp#89 | Release-notes pre-upgrade `jq` check runs `kubectl get bgpconfigurations` (resource is `configs`) and misses `authPassword: ""` | The check errors out, and misses the case it warns about | PureLB's pre-flight uses `bgpconfig` and `has("authPassword")` |
| purelb/k8gobgp#90 | `MultipleConfigurations` message says "per node"; the rule is cluster-wide (oldest wins) | Misleading for users | Migration guide says one per cluster |

## Image scan: `ghcr.io/purelb/k8gobgp:0.2.9` (trivy 0.75.0, 2026-11-09)

| Target | Findings |
|---|---|
| alpine 3.24.4 base | 1 MEDIUM: CVE-2026-4738, zlib `1.3.2-r1`, fixed in `1.4-r0` |
| `/usr/local/bin/manager` | 0 |
| `/usr/local/bin/gobgpd`, `/usr/local/bin/gobgp` | 8 each (4 HIGH, 4 MEDIUM, 0 CRITICAL): CVE-2026-30405, -37461, -37462, -41643, -49837, -49838, -7734, -7736 |

The same database finds more in `0.2.8`: 3 HIGH and 1 MEDIUM in its base (musl
CVE-2026-107891, zlib CVE-2026-4738 and CVE-2026-85091), and Go standard library
and `golang.org/x/net` findings in all three binaries, which v0.2.9's Go 1.27.3
and `golang.org/x/net` v0.62.0 clear. The zlib finding left in 0.2.9 is fixed in
an Alpine package newer than the image's base; no k8gobgp issue is filed yet.

The 16 gobgp findings are matched against module `github.com/osrg/gobgp/v4` at
the fork's pseudo-version `v4.0.0-20261106200837-fb5f01ac0ad4`, with fixed
versions 4.3.0-4.7.0. The fork reports `base: gobgp-4.9.0`, newer than every
fixed version, so these are probably false positives of the pseudo-version. Not
confirmed fix by fix.
