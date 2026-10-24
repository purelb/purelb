# Egress allocator API load: results (Phase 3)

Run 2026-10-23 on `t2-kwok` (Proxmox VM 130: 16 vCPU, 31 GiB, Debian 13,
Docker 26.1.5), kwok v0.8.0 with Kubernetes v1.35.9 and etcd 3.6.10, a new
cluster per run. Harness and method: [README.md](README.md).

Shape: 500 nodes (one simulated agent each), 250 dual-stack Egress policies
(500 addresses, local pools, `sliceSize` 1024), 5,000 selected pods. Phases:
60 s steady; every pod replaced at 50/s (rollout); 200 new pods on nodes with
no pod of their policy (claim burst). Agents QPS 50/100, Lease client 5/10,
10 s Leases renewed every 5 s, acks debounced to 1/s, drained 5 s after a
slice starts draining. The allocator is `cmd/allocator` at QPS 200/400.

## Final build (`711ca3e4`), two runs, against the egress-free baseline

Rollout phase (the budget case):

| | baseline | egress v6a | egress v6b |
| --- | --- | --- | --- |
| Phase length (until quiet) | 104 s | 181 s | 178 s |
| All writes | 305/s | 668/s | 675/s |
| Egress writes (below) | 0 | 443/s | 452/s |
| — agent EgressNodeState status (acks, claims, drained) | — | 260/s | 262/s |
| — EgressEndpoints (allocator) | — | 57/s | 62/s |
| — EgressNodeState spec (allocator) | — | 47/s | 46/s |
| — events (allocator) | — | 73/s | 75/s |
| — Egress status, EgressMembers (allocator) | — | 6/s | 6/s |
| Watch traffic, all watchers | 1.81 MiB/s | 3.53 MiB/s | 3.57 MiB/s |
| — of which EgressNodeState | 0 | 2.0 MiB/s | 2.0 MiB/s |
| etcd put bytes | 0.29 MiB/s | 1.38 MiB/s | 1.39 MiB/s |
| 429s / APF rejections | 0 / 0 | 0 / 0 | 0 / 0 |
| Late Lease renewals / longest gap | 0 / 5.1 s | 0 / 5.9 s | 0 / 6.0 s |
| Claims, p50 / p99 / max | — | 12.4 / 30.1 / 43.8 s | 10.0 / 28.0 / 40.2 s |
| CPU cores: kube-apiserver / etcd / allocator | 3.09 / 1.02 / 0.20 | 6.87 / 0.87 / 1.59 | 6.87 / 0.89 / 1.61 |
| Peak memory MiB: kube-apiserver / etcd / allocator | 880 / 254 / 88 | 5,257 / 933 / 151 | 5,105 / 923 / 152 |

Claim burst: p50 / p99 3.0 / 3.6 s and 3.3 / 3.5 s (n = 384, 380), no 429s,
no late renewals. Steady: about 2 egress writes/s.

## What the measurement changed

The first run of the real allocator missed the Phase 0 prototype's claim
latency (spike 8: 12 / 25 s p50 / p99) by far. Each fix below was found from
the run's data (allocator log, a CPU profile) and is in `711ca3e4`:

| Run | Change | Rollout claims p50 / p99 | Rollout settles | Write errors |
| --- | --- | --- | --- | --- |
| v1 | Phase 3 as committed | 44 / 116 s | 463 s | 3,702 |
| v2 | EgressNodeState spec as a `/spec` patch (the agents' status writes made every spec update conflict); failed writes requeue on their own lane; no rewrite of a status already submitted | 26 / 85 s | 260 s | 865 |
| v3 | Indexes: pods by label (selectPods scanned all 5,000 pods per reconcile: 36% of CPU), owned objects by policy; no pool call when nothing is missing | 7.3 / 46 s | 167 s | 3,577 |
| v4 | Read-your-writes for EgressEndpoints | 8.8 / 50 s | 163 s | 3,513 |
| v5 | No second write planned while one is in flight for an address (it always conflicted) | 11.4 / 29 s | 192 s | 82 |
| v6a, v6b | v5, without the profiler | 12.4 / 30.1 s, 10.0 / 28.0 s | 181 s, 178 s | — |

v3's lower p50 came with a long tail of conflicts and retries; v5 trades a few
seconds of p50 for half the p99.

## API budget (approved by the user 2026-10-23)

For Verification 4 at 500 nodes, 500 addresses and a 5,000-pod rollout at
50/s, set about 20% above the worse of the two final runs:

| Measure | Budget | Measured |
| --- | --- | --- |
| Egress writes, rollout average | ≤ 550/s | 443–452/s |
| — allocator's own writes | ≤ 230/s | 183–190/s |
| — agent status writes | ≤ 320/s (0.65/s per node) | 260–262/s |
| Added watch traffic | ≤ 2.2 MiB/s | +1.72–1.76 MiB/s |
| Added etcd put bytes | ≤ 1.3 MiB/s | +1.09–1.10 MiB/s |
| Allocator | ≤ 2 cores, ≤ 200 MiB | 1.6 cores, 152 MiB |
| Claim latency (sets the CNI hold, D17) | rollout p99 ≤ 35 s; burst p99 ≤ 5 s | 28–30 s; 3.5–3.6 s |
| Rollout settles | ≤ 2× the baseline | 1.71–1.74× |
| 429s, APF rejections, late Lease renewals | 0 (hard) | 0 |

## kube-apiserver memory (explained; accepted and documented)

The egress runs peaked at 5.0–5.5 GiB of kube-apiserver memory against
0.86–0.89 GiB for the baseline. The cause is the API server's watch cache
holding the event history of the egress CRDs, mostly EgressNodeState. The
user decided (2026-10-23) to accept it and document the API server sizing,
rather than reduce the writes.

Measured on the merged build (`8c6f275b`) with `memsample.sh` beside
`run.sh`, three runs:

| | baseline v7 | egress v7 | egress v7, EgressNodeState watch cache off |
| --- | --- | --- | --- |
| Peak, steady / rollout / claim burst | 713 / 886 / 879 MiB | 1,846 / 5,433 / 5,503 MiB | 1,774 / 2,914 / 3,238 MiB |
| Live heap, steady / nearest the rollout peak | 242–323 MB (every profile) | 573 / 1,658 MB | 546 / 959 MB |
| Late Lease renewals | 0 | 0 | 0 |
| Claims p99, rollout / burst | — | 33.5 / 3.5 s | 33.8 / 26.2 s |

- **It is Go heap.** The container's memory is all anonymous (file 0), and
  heap in use follows it. Live heap is about half of it; the rest is the
  garbage collector's headroom (GOGC 100).
- **It is decoded CRD objects from etcd watch events.** In every egress heap
  profile, 55–60% of the live heap was allocated under
  `etcd3.(*watchChan).prepareObjs` → `decodeObj`, through the CRD-only
  schema coercion, conversion and `Unstructured.DeepCopy`. A CRD object is
  held as nested maps, many times its 2–3 KB of JSON; a Pod is a typed
  struct, which is why the baseline's 200 pod events/s cost far less.
- **The watch cache holds them.** Each resource's watch cache keeps at least
  75 s of events (`DefaultEventFreshDuration`, the 60 s bookmark interval plus
  15 s), doubling up to 102,400 events, and each event holds its decoded
  object (`k8s.io/apiserver` v0.36.1, `pkg/storage/cacher`). The buffer
  shrinks only when full and stale, so a burst's history stays until newer
  events overwrite it. That is why the steady phase, after the bootstrap's
  writes, already sat at about 1.8 GiB.
- **EgressNodeState is most of it.** Its watch cache received 380–390
  events/s at the rollout's peak (pods: about 200/s, EgressEndpoints 50–110/s),
  and the heap peaked at the same moment. With only that resource's watch
  cache turned off (`--extra-args
  kube-apiserver=watch-cache-sizes=egressnodestates.purelb.io#0` on
  `kwokctl create cluster`), the rollout peak fell by 2.5 GiB. The rest above
  the baseline is the same effect for EgressEndpoints, and headroom.
- **Turning the cache off is not a remedy.** It is a cluster-wide API server
  flag, and it sends those reads to etcd: the claim burst's p99 went from
  3.5 s to 26.2 s in that run.

**Sizing (documented):** at 500 nodes, 500 addresses and a 5,000-pod rollout
at 50/s, egress adds about 4.6 GiB to the kube-apiserver's peak memory (about
1.1 GiB in steady state). It scales with the rate of EgressNodeState and
EgressEndpoints writes: pods starting and stopping on nodes without a slice,
and the agents' acks (1/s per node at most).

## Open findings

1. **Events.** At 73–75/s they are the allocator's largest write, mostly the
   per-pod `PodSelected`/`PodDeselected` and per-slice events of §12.1, and
   recording them is 11% of the allocator's CPU (profile, v5).
2. **EgressNodeState status.** The agents' acks are 58% of egress writes and
   their watch fan-out (to the allocator's cluster-wide EgressNodeState
   watch) is 2.0 MiB/s, the largest egress watch stream. They are also most
   of the kube-apiserver memory above.

## Limits of the simulation

Each address has one announcer chosen by hash, not by the Lease election;
only local pools are exercised (no routed addresses, so no selector-node
acks, D7); there is no CNI hold, failure policy or datapath; a draining slice
reports drained after 5 s.
