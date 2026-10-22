# Egress spike 8: control-plane load under kwok

Phase 0b spike 8 of the egress plan. It measures the Kubernetes API write rate,
watch traffic, etcd write volume and ack latency of the egress control-plane
handshake, for two designs, against a kwok control plane with simulated nodes.
Results and conclusions are in [RESULTS.md](RESULTS.md).

This directory is self-contained: its own Go module, throwaway CRDs in the
`spike.egress.purelb.io` group, and a kwok work directory under `.work/`. It
does not use `pkg/apis` or the repo's `go.mod`, and it writes nothing to
`~/.kube` or `~/.kwok`.

## What is simulated

One process (`loadgen`) runs:

- **The allocator**: a single writer with one worker goroutine and a keyed,
  de-duplicating FIFO (the same coalescing a client-go workqueue gives). It
  watches Pods, the PureLB Leases and every EgressNodeState, and writes
  EgressEndpoints (and, in the decided design, EgressMembers).
- **N node agents**: each has its own client-go rate limiter (QPS/Burst), its
  own watch of EgressEndpoints, an ack writer, and a Lease renewer at PureLB's
  cadence (renew every LeaseDuration/2 = 2.5 s, through the same client).
- **A driver** with an admin client. It creates the pods, simulates CNI ADD by
  calling the agent on the pod's node in-process (CNI ADD is local and costs no
  API call), and runs the phases.

Agents and the allocator authenticate as ServiceAccounts, so APF classifies
them as ordinary workload traffic rather than as exempt `system:masters`.

| | original (§4.5, §4.6, §5.7) | decided (D5, D6, D17) |
|---|---|---|
| EgressEndpoints | one per policy, in the policy namespace: addresses, slices **and** the endpoint (pod) list (split at 500 endpoints) | one per (policy, address), in `purelb-system`: addresses and slices only |
| Pod list | inside EgressEndpoints, so every agent receives it | `EgressMembers`, one per policy, written by the allocator; agents do not watch it |
| Ack | `EgressNodeState.status.observed`: one `{egressUID, generation}` per egress, rewritten whenever any generation changes (limited only by the client rate limiter) | `EgressNodeState.status.observedEpoch`: one integer, rate-capped to 1 write per `-ack-interval` (default 1 s). The allocator stamps every EgressEndpoints write with a global epoch |
| Gating set | every eligible node | eligible nodes with a live Lease |
| Claims | in EgressNodeState, written right away | same |
| Concurrent claims | coalesced by the workqueue | same: the first claim takes the Active reserve, later ones get Pending slices, and the reserve is replenished, all in one generation |

`-design none` is the **baseline**: the same pods, rollout, claim-burst pods
and per-node Lease renewals (through each agent's rate-limited client, as an
LB-only lbnodeagent does), with no egress object created, watched or written.
Subtracting a baseline row from a design row at the same N and QPS gives the
load the egress control plane adds. `followup-n500.sh` runs the N=500
baseline and the decided design at ack intervals of 1, 2 and 5 s.

## Phases (per run)

1. **steady**: 60 s with no churn.
2. **rollout**: a rolling restart of all 5,000 selected pods at 50 replacements/s
   (create the replacement on a random node, CNI ADD, delete the old pod),
   interleaved across policies. The phase runs until quiescent: every write is
   acked, the allocator queue is empty, and this holds for 3 s.
3. **claim-burst**: 200 new pods created at once, each on a node that never ran
   a pod of its policy, so each one claims a reserve slice for every address.

For each phase, apiserver and etcd metrics are sampled before and after:

- `apiserver_request_total`: writes by resource and verb, plus 429s.
- `apiserver_watch_events_total` and `apiserver_watch_events_sizes_sum`:
  events and bytes delivered, summed over every watcher.
- etcd `etcd_debugging_mvcc_total_put_size_in_bytes`,
  `etcd_network_client_grpc_received_bytes_total` and DB size.
- cgroup CPU for the apiserver and etcd containers, and rusage for loadgen.
- Peak memory per phase: the containers' cgroup `memory.current` and
  loadgen's `VmRSS`, sampled every second.

Ack latency runs from an allocator write returning to the moment the allocator
sees the last gating node's ack. Claim latency runs from the CNI ADD to the
point where the claimant's slice is Active and that write is acked by every
gating node.

## Reproduce

Requirements: Go ≥ 1.26 (or a prebuilt `loadgen` with `NO_BUILD=1`), Docker,
and a host with nothing else on it. Measured peaks (RESULTS.md): 13 GB used for
the N=100 matrix with both designs, 9.4 GB for N=500 with the decided design;
8 vCPUs are not enough for valid N=500 rows. Do not run it on a workstation.

```sh
cd hack/egress-spikes/kwok
./run-matrix.sh                       # full matrix: N=100,500 × {100,500} addresses × QPS {5/10, 50/100} × 2 designs
NODES=100 QPSES="50:100" ./run-matrix.sh    # a subset
DESIGNS=decided EXTRA="-ack-interval 250ms" SHAPES="250:2" NODES=500 QPSES="50:100" ./run-matrix.sh   # debounce ablation
go build -o loadgen . && ./loadgen -mode report -out results > tables.md     # markdown tables
```

`run-matrix.sh` installs kwokctl/kwok v0.8.0 into `.bin/` if missing. For
every run it creates a new docker-runtime kwok cluster (Kubernetes v1.35.9,
etcd 3.6.10) named `spike`, with all state in `.work/`, applies
`crds/crds.yaml` and creates N fake nodes; a shared cluster would carry the
apiserver's heap and etcd's cached pages from one run into the next. It writes
`results/<design>-n<N>-a<addrs>-q<qps>.{json,log}` and deletes the cluster at
the end. Set `KEEP=1` to keep the last cluster, and `NO_BUILD=1` to use a
prebuilt `./loadgen` on a host without Go.

Note: an ablation run writes to the same result file name as the matching
matrix run. Rename the earlier files first, or set `NAME`/the output yourself
by calling `loadgen -mode run ... -out <file>` directly.
