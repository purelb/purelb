# k8gobgp issues for v0.17.0

PureLB v0.17.0 bundles **k8gobgp v0.2.6** (gobgp-netlink v1.3.6). The open
findings were verified against the v0.2.5 tag; v0.2.6 changes only the
gobgp-netlink version, not the controller, CRDs, RBAC or entrypoint, so they and
their line numbers still apply.

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
| purelb/gobgp-netlink#57 | An IPv4 route over an IPv6 session was sent with a 16-byte NEXT_HOP (a 4-in-6 `::ffff:` address); FRR treats it as withdrawn, so no IPv4 VIP was advertised over IPv6-only peering | gobgp-netlink v1.3.6 sends a 4-byte IPv4 next hop. Verified on prox-purelb2: FRR installs the route with 5 ECMP next hops, no NEXT_HOP errors |

## Open against v0.2.6

| Issue | Finding | Effect on PureLB | PureLB handling |
|---|---|---|---|
| purelb/k8gobgp#82 | `k8gobgp_gobgpd_connection_status` is set to 1 after `grpc.NewClient`, which never dials (`bgpmetrics_controller.go:167-176`, `bgpconfiguration_controller.go:855-869`) | The gauge can read 1 while gobgpd isn't answering, resetting upstream's `K8GoBGPDaemonDisconnected` alert | Not relied on: e2e asserts sessions via `bgp_peer_state` |
| purelb/k8gobgp#83 | `GOBGPD_EXTRA_ARGS` is documented in `entrypoint.sh:74` but never read | No way to pass gobgpd flags (remote API opt-in, metrics tuning) | Unix-socket mode, which PureLB uses, is unaffected |
| purelb/k8gobgp#84 | `docs/metrics.md` migration table maps unreleased intermediate names | Upstream docs don't help v0.2.4 users | PureLB's migration guide carries the v0.2.4 → v0.2.6 table |
| purelb/k8gobgp#85 | Release notes call the CRD changes "additive" | A v0.2.4-valid BGPConfiguration can be rejected; v0.2.4 pods can't write BGPNodeStatus against the new CRD | Migration guide: `jq` pre-flight, CRD-with-image rollback |
| purelb/k8gobgp#86 | `"${NODE_IP}:7475"` is unbracketed, so gobgpd's metrics fail to bind on IPv6 nodes; only logged | No 7475 metrics on IPv6-primary nodes | PureLB sets `GOBGP_METRICS_HOST="[$(NODE_IP)]:7475"` |
| purelb/k8gobgp#87 | DaemonSet probes lack `timeoutSeconds`; kubelet's 1s undercuts the 3s `GetBgp` readiness check | Readiness flaps and a noisy error counter on busy nodes | PureLB sets `timeoutSeconds: 5` on startup and readiness |
| purelb/k8gobgp#88 | `k8gobgp_rib_routes` is `Reset()` before the `GetTable` loop | Series briefly absent; absent reads as 0 | e2e reads a value only once the series exists |
| purelb/k8gobgp#89 | Release-notes pre-upgrade `jq` check runs `kubectl get bgpconfigurations` (resource is `configs`) and misses `authPassword: ""` | The check errors out, and misses the case it warns about | PureLB's pre-flight uses `bgpconfig` and `has("authPassword")` |
| purelb/k8gobgp#90 | `MultipleConfigurations` message says "per node"; the rule is cluster-wide (oldest wins) | Misleading for users | Migration guide says one per cluster |
