# Egress allocator API load under kwok

Measures the API load of the real egress allocator, as the Phase 3 API budget
requires: `cmd/allocator` runs as its own process against a kwok control plane
with N fake nodes, and `egress-kwok` simulates one lbnodeagent per node and
drives the load. Results are in [RESULTS.md](RESULTS.md).

## What is simulated

Each agent has its own client rate limiter (QPS 50/100) and a separate 5/10
client for its Lease (D-lease), and authenticates as the `lbnodeagent`
ServiceAccount, so API Priority and Fairness treats it as a workload. Each
agent:

- renews its Lease every half lease duration (10 s lease, as shipped);
- creates its EgressNodeState and reports `capability.egressCapable`, from
  which the allocator applies the `purelb.io/egress-capable` label (D29);
- watches its own EgressNodeState by name and acks its spec generation in
  `status.observedGeneration`;
- announces the addresses assigned to it (one owner per address, spread by
  hash: a stand-in for the Lease election), watches each one's EgressEndpoints
  by name and acks its generation in `status.announcing` (D6);
- claims a slice of each of a policy's addresses at a simulated CNI ADD when it
  has no Active slice of it (D17), and records the latency until the slice is
  Active in its spec;
- reports a Draining slice drained after `-drain-delay` (5 s).

Status is written at most once per `-ack-interval` (1 s), except claims, which
are written at once.

The driver creates P dual-stack Egress policies (2P addresses) in one
ServiceGroup (local pools, `sliceSize` 1024) and E selected pods spread at
random, then runs:

1. **steady**: 60 s with no change;
2. **rollout**: every selected pod replaced at 50/s on a random node (create,
   CNI ADD, delete the old one);
3. **claim-burst**: 200 new pods, each on a node with no pod of its policy.

A phase ends when no egress object has been written for 3 s and no claim is
open. Per phase it records writes and 429s by resource and verb, watch events
and bytes, APF rejections, etcd volume, CPU cores and peak memory of the
control plane, the allocator and the load generator, Lease renewal gaps and
claim latency.

`RUNS=baseline` repeats the run with no Egress policies, so the difference is
the load egress adds.

## Reproduce

On a dedicated host (16 vCPU for N=500; not a workstation):

```sh
GOOS=linux GOARCH=amd64 go build -o allocator ./cmd/allocator
GOOS=linux GOARCH=amd64 go build -o egress-kwok ./hack/egress-kwok
# copy allocator, egress-kwok, run.sh and deployments/crds/purelb.io_*.yaml (as crds/) to the
# host, with kwokctl v0.8.0 in .bin/
./run.sh
```
