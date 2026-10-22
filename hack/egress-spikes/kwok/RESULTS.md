# Spike 8 results: egress control-plane load under kwok

Run 2026-10-19 on a dedicated Proxmox VM (`t2-kwok`, VMID 130: 8 vCPU, raised
to 16 vCPU for the valid N=500 runs; 31 GiB, Debian 13, kernel 6.12.113, Docker 26.1.5), with a fresh kwok cluster
(Kubernetes v1.35.9, etcd 3.6.10) for every run. 5,000 selected pods, three
phases per run (README). The tables at the end are `loadgen -mode report`
output. Raw results: `results/*.json` and `*.log`; `results/vm-mem.log` is the
VM's memory every 10 s.

`results/workstation-2026-10-18/` holds an earlier, partial run on a
workstation that also ran a Windows VM and went into swap until the OOM killer
fired. Those numbers are not comparable and are kept only as a record.

## Which numbers are valid

| Rows | Valid? | Why |
|---|---|---|
| N=100, both designs | **yes** | The VM had headroom: the decided design used 3.8-6.4 of 8 cores, the original 5.7-7.1. No 429s. No late lease renewals for the decided design (max gap 4.0 s against a 2.5 s cadence) |
| N=500 on 8 vCPUs (`results/n500-8vcpu/`) | **no** | Every rollout used 7.2-7.5 of 8 cores. Without a baseline these runs cannot say whether the rig or the design was the limit |
| N=500 on 16 vCPUs, with the egress-free baseline | **yes** | The baseline rollout used 7.9 of 16 cores with no 429s and no late renewals, so the rig handles the pod churn; what the egress runs add on top is the egress design's load (section below) |

The original design was not run at N=500: its N=100 apiserver peak (11 GB)
scaled by the watcher count would not fit the VM.

**Two things to know when reading every table here.** The N=500 target was
chosen by the assistant (in a 2026-10-16 review prompt), not by the user or the
design document, which names 5,000 pods and 500 addresses but no node count. (The
user confirmed 500 nodes as the target on 2026-10-19.)
And the harness uses a 5 s Lease renewed every 2.5 s (the code default; the
manifests ship 10 s), so "late lease renewals" counts gaps over 5 s.

## Findings (N=100)

1. **The decided design (D5, D6, D17) cuts ack writes, etcd write volume,
   control-plane memory and ack latency against the original design.** Watch
   bytes are lower at 100 addresses but not at 500, where the decided design's
   per-address objects produce more, smaller events. Rollout, QPS 50/100:

   | | 100 addresses: original | decided | 500 addresses: original | decided |
   |---|---|---|---|---|
   | EgressNodeState writes/s | 135.6 | 82.5 | 167.4 | 108.0 |
   | egress watch bytes/s | 4.3 MB | 3.1 MB | 3.7 MB | 4.0 MB |
   | etcd put bytes/s | 830 kB | 519 kB | 3.5 MB | 618 kB |
   | apiserver peak memory | 2.8 GB | 1.8 GB | **10.5 GB** | 1.9 GB |
   | etcd peak memory | 865 MB | 371 MB | 2.1 GB | 452 MB |
   | ack p50 / p99 | 35.8 / 95.3 s | **1.1 / 2.3 s** | 43.8 / 136.2 s | **1.1 / 5.9 s** |

   The original design's 500-address runs are what exhausted the workstation:
   apiserver 10.5-11.3 GB plus etcd about 2 GB.

2. **The agents' client QPS must be raised: client-go's default (5/10) is not
   enough.** With the decided design at 500 addresses, QPS 5/10 did not
   quiesce within the 300 s limit after a 5,000-pod rollout (claim p50 258 s,
   p99 331 s, ack p99 56 s) with no 429s and 5.4 cores used, so the agents'
   own rate limiter was the bottleneck. QPS 50/100 quiesced in 190 s. PureLB
   sets no QPS today (plan Phase 1.3 sets it); 50/100 is the tested value.

3. **Claim latency stays in the tens of seconds under churn.** Decided design,
   QPS 50/100: claim p50/p99 31/48 s (100 addresses) and 80/114 s (500
   addresses) during the rollout, 13/29 s for the 200-pod claim burst at 500
   addresses. D17 holds a pod's CNI ADD until its slice is Active and sets
   the hold from the measured p99, so with these numbers a pod that needs a
   new slice during a large rollout waits about two minutes. That is a
   design input for D17, not something this spike decides.

4. **Lease renewals compete with egress writes.** The agents' Lease renewals go
   through the same client (rate limiter and APF flow) as their acks and
   claims. The original design delayed renewals in two N=100 runs (21 in
   one, 1 in another); the decided design delayed none at N=100. At N=500, where the rig was saturated,
   renewals were up to 35-61 s late, which would fail PureLB's 10 s lease. Whether
   that happens on a correctly sized control plane is not measured. Decided
   (plan D-lease, 2026-10-19): the election gets its own client and limiter,
   and a shipped FlowSchema moves PureLB's Lease requests from `workload-low`
   (where the `service-accounts` FlowSchema puts them today) to the
   `leader-election` priority level.

5. **Memory to reproduce:** the N=100 matrix (both designs) peaked at 13.0 GB
   used on the VM; the N=500 decided matrix at 9.4 GB. Neither swapped. The
   N=500 rows need more than 8 vCPUs, or loadgen on a separate host from the
   control plane, to be valid.

## N=500 on 16 vCPUs: where the egress load comes from, and the scoped design

All rows: 500 nodes, 5,000 pods, a 50 pods/s rollout, agent QPS 50/100, a
fresh cluster per run. "Baseline" is `-design none` (the same pods and Lease
renewals, no egress). Rollout phase:

| run | settled | rollout s | egress writes/s | acks | ack p99 s | claim p50 / p99 s | 429s | late leases (max gap) | CPU total / apiserver | apiserver peak GB |
|---|---|---|---|---|---|---|---|---|---|---|
| baseline | yes | 102 | 0 | 0 | 0.0 | - | 0 | 0 (3 s) | 7.9 / 4.7 | 1.16 |
| decided, 1 s ack | **no** | 401 | 397 | 153,228 | 170.8 | 199 / 201 | 1,959 | 1,340 (67 s) | 13.7 / 10.5 | 7.65 |
| decided, 2 s ack | yes | 381 | 259 | 91,676 | 107.5 | 247 / 304 | 209 | 143 (16 s) | 13.1 / 8.5 | 6.93 |
| decided, 5 s ack | yes | 234 | 121 | 21,177 | 6.2 | 112 / 152 | 0 | 0 (4 s) | 12.1 / 6.3 | 6.01 |
| **scoped** | yes | 175 | 84 | 5,561 | 1.0 | 89 / 128 | 0 | 0 (4 s) | 9.1 / 6.0 | 1.90 |
| decided, 1 s ack, 100 addresses | yes | 362 | 322 | 111,270 | 166.6 | 295 / 327 | 1,651 | 990 (36 s) | 13.1 / 9.3 | 5.50 |
| **scoped**, 100 addresses | yes | 143 | 69 | 3,207 | 0.9 | 56 / 83 | 0 | 0 (3 s) | 9.5 / 6.1 | 1.75 |

Rows without "100 addresses" have 500 addresses.

**Where the decided design's load comes from: amplification, not egress
changes.** In the decided 1 s run the allocator made about 1,560 real changes,
and they produced 153,228 acks and 1,124 EgressEndpoints watch events/s
(4.4 MB/s). Two choices cause it:

1. **Every node acks every change.** D6's allocator stamps each write with one
   cluster-wide epoch, and every eligible node must ack it before a slice
   activates, so any change costs an ack from all N nodes. The 1-per-second
   cap only limits the rate: during a rollout every node acks about every
   second, so acks grow with N.
2. **Every node receives every whole object.** Every agent watches every
   EgressEndpoints object, and each object lists the slices of every node
   running the policy's pods (4.7 KB at 500 addresses, 15.8 KB at 100
   addresses, at N=500). One slice moving resends the whole object to every
   node, so this traffic grows with about N² (100 addresses: 721 events/s,
   9.1 MB/s).

The ack-interval runs show that a longer interval only slows the
amplification (2 s still made 41 acks per change) and adds latency to every
change.

**The scoped design** (`-design scoped`, the prototype of review option
"A + E"):

- each (policy, address) slice table is labelled with its announcer nodes
  and watched only by them (label-selector watch);
- each node's own slices are in its EgressNodeState `spec`, written by the
  allocator and watched only by that node (field-selector watch);
- each address and each node has its own version (`metadata.generation`)
  instead of the global epoch; activating a slice waits only for that
  address's announcers, and a claim completes when the claiming node acks its
  own spec with the slice Active.

At N=500 it settles, with no 429s and no late lease renewals, 27x fewer acks
than the decided design, ack p99 under 1 s, slice-table watch traffic down
from 1,124 to 44 events/s (4.4 to 0.1 MB/s), and apiserver memory and CPU
close to the baseline: egress costs about 1.3 apiserver cores over load balancing
alone, against 5.8 for the decided design.

**Not modelled by the prototype:**

- *The takeover re-read.* A node that wins an address's election re-reads
  its slice table before announcing. Announcers do not change in these runs,
  so its cost (one read per failover) is not measured.
- *The pod nodes' routing view* (which advertisers serve an address). It does
  not change in these runs.

**Remaining limit: claim latency (p50 89 s, p99 128 s at 500 addresses).**
Acks no longer explain it (p99 under 1 s). The allocator made about 4,400
writes in the 175 s rollout, about 25/s, well under its 50 QPS limit. That
fits a single worker that writes one object at a time and waits for each
round trip (about 40 ms). This is an inference from the write rate; the
allocator's queue depth was not measured.

## N=500 with PureLB's shipped 10 s lease, and the claim path (2026-10-19)

Same rollout as above (500 nodes, 500 addresses, QPS 50/100, 16 vCPUs), with
the 10 s lease renewed every 5 s, as the manifests ship it. The scoped design
is also run with 4 allocator writers (`-alloc-writers 4`) and with a priority
lane for claims, activations and the claimant's spec (`-alloc-priority`).

| run | settled | rollout s | acks | ack p99 s | claim p50 / p99 s | burst claim p50 / p99 s | 429s | late leases >10 s (max gap) | CPU total / apiserver | apiserver peak GB | allocator writes/s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| baseline | yes | 102 | 0 | - | - | - | 0 | 0 (6 s) | 6.9 / 3.9 | 0.97 | 0 |
| decided, 1 s ack | **no** | 402 | 168,015 | 123.3 | 320 / 373 | 317 / 362 | 509 | 0 (9 s) | 12.8 / 9.6 | 7.78 | 5 |
| scoped, 1 writer | yes | 167 | 5,902 | 1.0 | 79 / 115 | 24 / 28 | 0 | 0 (6 s) | 8.4 / 5.5 | 1.87 | 28 |
| scoped, 4 writers | yes | 140 | 8,941 | 1.0 | 43 / 63 | 13 / 16 | 0 | 0 (5 s) | 8.3 / 5.5 | 2.13 | 50 |
| scoped, 4 writers + priority | yes | 143 | 10,967 | 1.1 | 28 / 42 | 9 / 13 | 0 | 0 (6 s) | 11.0 / 7.4 | 2.25 | 50 |

- **The lease length does not change the design comparison**: the decided
  design still does not settle, the scoped one does.
- **With the real 10 s lease no run renewed late**, the decided design
  included (longest gap 9 s). The 5 s harness lease produced the late counts
  in the earlier sections.
- **The claim path is queue-bound, then rate-bound.** One writer leaves
  claims behind about 1,000 queued rewrites (queue depth measured, mean
  about 1,030 of about 1,250 keys); 4 writers halve claim latency; more than
  4 do not help (16 measured with the 5 s lease); the priority lane cuts it
  again, to 28 / 42 s. With 4 writers the allocator writes exactly 50/s:
  its client-go limit (the harness gives the allocator the agents' QPS).
  A 5,000-pod rollout at 50 pods/s raises about 100 claims/s, each needing
  several allocator writes, so the allocator's own QPS is now the bound.

### The claim path's remaining levers (2026-10-19)

Rollout of 5,000 pods; each claim needs an allocator write of the slice
table, the announcers' ack, the claiming node's spec write and its ack.

| scoped design, N=500, 500 addresses, 10 s lease | claim p50 / p99 s | burst claim p50 / p99 s | allocator writes/s | queue mean | 429s | late leases | CPU total / apiserver | apiserver peak GB |
|---|---|---|---|---|---|---|---|---|
| 4 writers + priority (reference) | 28 / 42 | 9 / 13 | 50 | 1029 | 0 | 0 | 11.0 / 7.4 | 2.25 |
| + allocator QPS 200 | 23 / 36 | 9 / 11 | 62 | 928 | 0 | 0 | 11.5 / 7.8 | 2.54 |
| + 4 reserves | 19 / 32 | 8 / 13 | 50 | 992 | 0 | 0 | 11.0 / 7.4 | 2.23 |
| + both | 16 / 28 | 8 / 11 | 60 | 888 | 0 | 0 | 11.5 / 7.8 | 2.31 |
| + both, corrected (reserve claims wait for the announcers' ack) | 12 / 25 | 7 / 9 | 73 | 923 | 0 | 0 | 11.7 / 8.0 | 2.82 |

- A higher allocator QPS and more reserve slices per address (so a claim
  takes an already-acked slice instead of waiting for the announcers) both
  help and combine, but each adds less: 28 / 42 s down to 16 / 28 s.
- **Correction.** In the runs above, a claim that took an existing reserve
  completed on the claimant's spec ack alone, without waiting for the
  announcers to learn the slice's new owner (owner 0, so replies would be
  dropped). Fixed (`-fix` results): the corrected decided configuration
  measures 12 / 25 s (burst 7 / 9 s); 1 writer (79 / 115 s) and 4 writers
  with priority (28 / 42 s) reproduce within a second. Why the corrected
  run is faster than the flawed one was not analysed.
- At QPS 200 the allocator writes only about 60/s (50/s at QPS 50, its
  limit): with 4 writers each waiting on an API round trip under this load,
  the round-trip time, not the client limit, is now the bound.
- No run had 429s or late lease renewals.

## Tables (`loadgen -mode report`)

### Phase: rollout

| N | addrs | QPS/Burst | design | dur s | quiesced | EgressEndpoints w/s | EgressMembers w/s | EgressNodeState w/s | Lease w/s | all writes w/s | egress watch ev/s | egress watch B/s | etcd put B/s | etcd put / 5 min | ack p50 / p99 s | claim n, p50 / p99 s | 429s | late lease renewals | CPU apiserver / etcd / loadgen | mem peak MiB apiserver / etcd / loadgen |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 100 | 100 | 5/10 | orig | 303 | yes | 1.8 | 0.0 | 150.6 | 43 | 283 | 446 | 5.0 MB/s | 924 kB/s | 277.1 MB | 50.87 / 98.23 | 2304, 159.98 / 240.02 | 0 | 0 | 4.2 / 0.6 / 1.1 | 3625 / 846 / 201 |
| 100 | 100 | 5/10 | decided | 182 | yes | 2.7 | 2.1 | 88.9 | 44 | 274 | 452 | 2.4 MB/s | 466 kB/s | 139.9 MB | 1.11 / 2.51 | 2304, 45.18 / 64.23 | 0 | 0 | 3.7 / 0.7 / 1.0 | 1584 / 369 / 199 |
| 100 | 100 | 50/100 | scoped | 118 | yes | 8.8 | 4.4 | 41.0 | 43 | 309 | 157 | 904 kB/s | 881 kB/s | 264.3 MB | 0.05 / 1.00 | 2304, 20.51 / 32.25 | 0 | 0 | 5.6 / 1.1 / 0.9 | 1463 / 411 / 286 |
| 100 | 100 | 50/100 | orig | 336 | yes | 1.7 | 0.0 | 135.6 | 43 | 256 | 362 | 4.3 MB/s | 830 kB/s | 249.0 MB | 35.82 / 95.30 | 2304, 178.57 / 251.50 | 0 | 21 | 4.3 / 0.6 / 1.1 | 2798 / 865 / 209 |
| 100 | 100 | 50/100 | decided | 168 | yes | 3.6 | 5.5 | 82.5 | 44 | 281 | 536 | 3.1 MB/s | 519 kB/s | 155.6 MB | 1.14 / 2.25 | 2304, 30.81 / 47.92 | 0 | 0 | 3.7 / 0.7 / 1.0 | 1758 / 371 / 186 |
| 100 | 500 | 5/10 | orig | 375 | yes | 3.2 | 0.0 | 151.9 | 43 | 265 | 518 | 3.6 MB/s | 3.2 MB/s | 946.8 MB | 44.50 / 124.23 | 7564, 268.69 / 322.14 | 0 | 1 | 5.6 / 0.4 / 0.7 | 10856 / 1945 / 268 |
| 100 | 500 | 5/10 | decided | 401 | **no** | 3.2 | 1.4 | 102.7 | 43 | 213 | 535 | 1.7 MB/s | 548 kB/s | 164.3 MB | 1.24 / 56.17 | 7232, 257.99 / 331.05 | 0 | 0 | 3.6 / 0.5 / 0.8 | 2730 / 706 / 200 |
| 100 | 500 | 50/100 | orig | 365 | yes | 3.1 | 0.0 | 167.4 | 43 | 288 | 535 | 3.7 MB/s | 3.5 MB/s | 1038.4 MB | 43.78 / 136.24 | 7564, 258.53 / 324.82 | 0 | 0 | 5.3 / 0.4 / 0.7 | 10510 / 2059 / 236 |
| 100 | 500 | 50/100 | decided | 190 | yes | 9.2 | 4.5 | 108.0 | 43 | 296 | 1152 | 4.0 MB/s | 618 kB/s | 185.4 MB | 1.08 / 5.86 | 7564, 80.35 / 114.28 | 0 | 0 | 3.8 / 0.7 / 1.2 | 1906 / 452 / 196 |
| 500 | 100 | 5/10 | decided | 376 | yes | 1.2 | 1.2 | 316.9 | 188 | 575 | 1132 | 8.2 MB/s | 883 kB/s | 265.0 MB | 84.38 / 152.49 | 7442, 295.78 / 343.91 | 2206 | 1552 | 9.2 / 1.0 / 2.0 | 5948 / 975 / 600 |
| 500 | 100 | 50/100 | scoped | 143 | yes | 4.3 | 2.1 | 62.3 | 211 | 454 | 216 | 700 kB/s | 757 kB/s | 227.0 MB | 0.03 / 0.95 | 7442, 56.22 / 82.83 | 0 | 0 | 6.1 / 1.2 / 1.0 | 1787 / 474 / 803 |
| 500 | 100 | 50/100 | decided | 362 | yes | 1.4 | 2.3 | 318.4 | 186 | 577 | 1221 | 9.7 MB/s | 946 kB/s | 283.9 MB | 79.86 / 166.55 | 7442, 294.77 / 327.24 | 1651 | 990 | 9.3 / 1.0 / 2.2 | 5634 / 1001 / 596 |
| 500 | 500 | 5/10 | none | 102 | yes | 0.0 | 0.0 | 0.0 | 212 | 457 | 0 | 4 B/s | 528 kB/s | 158.5 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 4.7 / 1.2 / 0.4 | 1164 / 325 / 189 |
| 500 | 500 | 50/100 | scoped | 175 | yes | 11.8 | 5.0 | 66.9 | 212 | 439 | 258 | 819 kB/s | 744 kB/s | 223.1 MB | 0.03 / 0.98 | 9444, 89.35 / 128.02 | 0 | 0 | 6.0 / 1.1 / 1.0 | 1949 / 538 / 836 |
| 500 | 500 | 50/100 | none | 102 | yes | 0.0 | 0.0 | 0.0 | 212 | 457 | 0 | 4 B/s | 529 kB/s | 158.6 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 4.7 / 1.2 / 0.4 | 1187 / 324 / 190 |
| 500 | 500 | 50/100 | decided | 401 | **no** | 2.2 | 1.7 | 392.6 | 181 | 640 | 1392 | 4.7 MB/s | 1.2 MB/s | 369.7 MB | 88.96 / 170.81 | 319, 199.30 / 201.44 | 1959 | 1340 | 10.5 / 1.1 / 1.7 | 7837 / 1205 / 601 |
| 500 | 500 | 50/100 | decided (debounce 2s) | 381 | yes | 4.0 | 1.9 | 253.0 | 211 | 537 | 2423 | 8.9 MB/s | 807 kB/s | 242.0 MB | 5.74 / 107.47 | 9444, 247.17 / 303.92 | 209 | 143 | 8.5 / 1.0 / 2.8 | 7094 / 929 / 613 |
| 500 | 500 | 50/100 | decided (debounce 5s) | 234 | yes | 6.8 | 3.7 | 110.7 | 211 | 438 | 3643 | 14.8 MB/s | 546 kB/s | 163.9 MB | 5.12 / 6.21 | 9444, 112.40 / 151.80 | 0 | 0 | 6.3 / 0.9 / 4.1 | 6158 / 540 / 596 |

### Phase: claim-burst

| N | addrs | QPS/Burst | design | dur s | quiesced | EgressEndpoints w/s | EgressMembers w/s | EgressNodeState w/s | Lease w/s | all writes w/s | egress watch ev/s | egress watch B/s | etcd put B/s | etcd put / 5 min | ack p50 / p99 s | claim n, p50 / p99 s | 429s | late lease renewals | CPU apiserver / etcd / loadgen | mem peak MiB apiserver / etcd / loadgen |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 100 | 100 | 5/10 | orig | 59 | yes | 1.8 | 0.0 | 167.3 | 43 | 219 | 516 | 6.2 MB/s | 825 kB/s | 247.5 MB | 18.66 / 24.34 | 400, 51.22 / 54.88 | 0 | 0 | 4.3 / 0.5 / 1.4 | 4040 / 916 / 203 |
| 100 | 100 | 5/10 | decided | 53 | yes | 3.8 | 1.0 | 95.8 | 43 | 151 | 575 | 3.6 MB/s | 246 kB/s | 73.9 MB | 1.03 / 1.52 | 400, 35.74 / 48.51 | 0 | 0 | 2.5 / 0.4 / 1.0 | 1597 / 399 / 204 |
| 100 | 100 | 50/100 | scoped | 17 | yes | 12.0 | 3.4 | 43.3 | 49 | 132 | 169 | 1.1 MB/s | 657 kB/s | 197.0 MB | 0.04 / 0.97 | 400, 13.41 / 13.63 | 0 | 0 | 3.2 / 0.5 / 0.7 | 1550 / 448 / 273 |
| 100 | 100 | 50/100 | orig | 55 | yes | 1.9 | 0.0 | 169.5 | 43 | 240 | 529 | 6.5 MB/s | 839 kB/s | 251.8 MB | 13.69 / 22.15 | 400, 48.78 / 51.94 | 0 | 0 | 4.2 / 0.5 / 1.5 | 2764 / 918 / 215 |
| 100 | 100 | 50/100 | decided | 26 | yes | 7.7 | 2.1 | 90.9 | 40 | 165 | 964 | 7.2 MB/s | 308 kB/s | 92.5 MB | 1.06 / 1.53 | 400, 17.54 / 22.18 | 0 | 0 | 3.1 / 0.4 / 1.8 | 1881 / 395 / 185 |
| 100 | 500 | 5/10 | orig | 155 | yes | 1.8 | 0.0 | 111.8 | 43 | 160 | 406 | 3.9 MB/s | 2.1 MB/s | 615.3 MB | 56.59 / 88.13 | 400, 87.91 / 145.05 | 0 | 0 | 4.6 / 0.3 / 0.7 | 11268 / 2177 / 381 |
| 100 | 500 | 5/10 | decided | 149 | yes | 4.0 | 0.9 | 99.4 | 43 | 151 | 606 | 2.0 MB/s | 218 kB/s | 65.4 MB | 1.02 / 1.79 | 686, 114.67 / 333.28 | 0 | 0 | 2.4 / 0.4 / 0.7 | 2303 / 761 / 207 |
| 100 | 500 | 50/100 | orig | 146 | yes | 1.9 | 0.0 | 129.7 | 43 | 178 | 450 | 4.3 MB/s | 2.4 MB/s | 712.0 MB | 51.93 / 76.80 | 400, 78.29 / 134.91 | 0 | 0 | 4.5 / 0.3 / 0.8 | 11220 / 2328 / 239 |
| 100 | 500 | 50/100 | decided | 35 | yes | 15.6 | 3.9 | 93.4 | 44 | 169 | 1772 | 7.0 MB/s | 285 kB/s | 85.6 MB | 1.03 / 1.29 | 400, 12.71 / 29.43 | 0 | 0 | 2.8 / 0.4 / 1.8 | 1899 / 479 / 202 |
| 500 | 100 | 5/10 | decided | 110 | yes | 1.8 | 0.5 | 370.3 | 200 | 578 | 1651 | 15.1 MB/s | 738 kB/s | 221.5 MB | 21.15 / 54.45 | 400, 101.86 / 106.56 | 225 | 118 | 9.0 / 0.9 / 2.9 | 5640 / 1102 / 613 |
| 500 | 100 | 50/100 | scoped | 23 | yes | 8.6 | 2.3 | 43.1 | 237 | 308 | 201 | 697 kB/s | 509 kB/s | 152.8 MB | 0.03 / 0.41 | 400, 20.02 / 20.62 | 0 | 0 | 4.4 / 0.7 / 0.8 | 1932 / 505 / 820 |
| 500 | 100 | 50/100 | decided | 85 | yes | 2.6 | 0.6 | 392.5 | 211 | 612 | 2096 | 21.6 MB/s | 790 kB/s | 237.1 MB | 21.66 / 39.88 | 400, 79.87 / 81.83 | 168 | 34 | 8.7 / 0.9 / 3.5 | 5303 / 1101 / 603 |
| 500 | 500 | 5/10 | none | 5 | yes | 0.0 | 0.0 | 0.0 | 215 | 300 | 0 | 0 B/s | 272 kB/s | 81.6 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 2.4 / 0.5 / 0.6 | 1166 / 330 / 196 |
| 500 | 500 | 50/100 | scoped | 32 | yes | 17.1 | 4.4 | 53.1 | 222 | 309 | 215 | 785 kB/s | 514 kB/s | 154.1 MB | 0.03 / 0.91 | 400, 24.50 / 28.81 | 0 | 0 | 4.6 / 0.7 / 0.8 | 1978 / 575 / 842 |
| 500 | 500 | 50/100 | none | 5 | yes | 0.0 | 0.0 | 0.0 | 214 | 299 | 0 | 0 B/s | 271 kB/s | 81.4 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 3.3 / 0.5 / 0.6 | 1188 / 329 / 188 |
| 500 | 500 | 50/100 | decided | 304 | **no** | 2.2 | 0.5 | 348.2 | 178 | 532 | 1651 | 6.0 MB/s | 683 kB/s | 204.8 MB | 231.95 / 274.00 | 1833, 565.39 / 623.62 | 1348 | 841 | 9.6 / 0.9 / 2.0 | 8336 / 1288 / 609 |
| 500 | 500 | 50/100 | decided (debounce 2s) | 91 | yes | 5.9 | 1.6 | 246.5 | 214 | 475 | 3483 | 14.9 MB/s | 551 kB/s | 165.3 MB | 2.36 / 3.89 | 400, 36.87 / 87.01 | 0 | 0 | 7.2 / 0.8 / 4.1 | 6248 / 983 / 622 |
| 500 | 500 | 50/100 | decided (debounce 5s) | 59 | yes | 9.2 | 2.3 | 102.1 | 220 | 341 | 4820 | 22.4 MB/s | 339 kB/s | 101.7 MB | 3.97 / 6.25 | 400, 26.44 / 51.64 | 0 | 0 | 5.3 / 0.6 / 5.5 | 6003 / 593 / 600 |

### Phase: steady

| N | addrs | QPS/Burst | design | dur s | quiesced | EgressEndpoints w/s | EgressMembers w/s | EgressNodeState w/s | Lease w/s | all writes w/s | egress watch ev/s | egress watch B/s | etcd put B/s | etcd put / 5 min | ack p50 / p99 s | claim n, p50 / p99 s | 429s | late lease renewals | CPU apiserver / etcd / loadgen | mem peak MiB apiserver / etcd / loadgen |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 100 | 100 | 5/10 | orig | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 235 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.3 / 0.1 / 0.1 | 1296 / 141 / 255 |
| 100 | 100 | 5/10 | decided | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 226 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.3 / 0.1 / 0.1 | 1026 / 137 / 229 |
| 100 | 100 | 50/100 | scoped | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 3 | 448 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.4 / 0.1 / 0.1 | 550 / 148 / 249 |
| 100 | 100 | 50/100 | orig | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 235 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.3 / 0.1 / 0.1 | 1542 / 143 / 239 |
| 100 | 100 | 50/100 | decided | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 229 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.4 / 0.1 / 0.1 | 1086 / 136 / 206 |
| 100 | 500 | 5/10 | orig | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 235 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.4 / 0.1 / 0.1 | 2199 / 146 / 405 |
| 100 | 500 | 5/10 | decided | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 229 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.4 / 0.1 / 0.1 | 1410 / 141 / 286 |
| 100 | 500 | 50/100 | orig | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 235 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.4 / 0.1 / 0.1 | 1653 / 147 / 236 |
| 100 | 500 | 50/100 | decided | 62 | yes | 0.0 | 0.0 | 0.0 | 43 | 43 | 2 | 226 B/s | 21 kB/s | 6.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 0.4 / 0.1 / 0.1 | 744 / 144 / 180 |
| 500 | 100 | 5/10 | decided | 62 | yes | 0.0 | 0.0 | 0.0 | 214 | 222 | 8 | 1 kB/s | 115 kB/s | 34.6 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.8 / 0.4 / 0.3 | 3055 / 210 / 719 |
| 500 | 100 | 50/100 | scoped | 62 | yes | 0.0 | 0.0 | 0.0 | 213 | 223 | 16 | 2 kB/s | 120 kB/s | 36.0 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.9 / 0.4 / 0.3 | 933 / 184 / 663 |
| 500 | 100 | 50/100 | decided | 62 | yes | 0.0 | 0.0 | 0.0 | 218 | 226 | 8 | 1 kB/s | 117 kB/s | 35.1 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.8 / 0.4 / 0.3 | 3353 / 180 / 850 |
| 500 | 500 | 5/10 | none | 62 | yes | 0.0 | 0.0 | 0.0 | 211 | 224 | 0 | 7 B/s | 125 kB/s | 37.6 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.8 / 0.4 / 0.2 | 693 / 174 / 197 |
| 500 | 500 | 50/100 | scoped | 62 | yes | 0.0 | 0.0 | 0.0 | 219 | 226 | 16 | 2 kB/s | 117 kB/s | 35.0 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.9 / 0.4 / 0.3 | 863 / 188 / 654 |
| 500 | 500 | 50/100 | none | 62 | yes | 0.0 | 0.0 | 0.0 | 211 | 224 | 0 | 7 B/s | 126 kB/s | 37.7 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.8 / 0.4 / 0.2 | 707 / 171 / 205 |
| 500 | 500 | 50/100 | decided | 62 | yes | 0.0 | 0.0 | 0.0 | 215 | 222 | 8 | 1 kB/s | 111 kB/s | 33.4 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.8 / 0.4 / 0.3 | 5597 / 185 / 650 |
| 500 | 500 | 50/100 | decided (debounce 2s) | 62 | yes | 0.0 | 0.0 | 0.0 | 211 | 217 | 8 | 1 kB/s | 108 kB/s | 32.3 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.7 / 0.3 / 0.3 | 6528 / 188 / 912 |
| 500 | 500 | 50/100 | decided (debounce 5s) | 62 | yes | 0.0 | 0.0 | 0.0 | 211 | 216 | 8 | 1 kB/s | 107 kB/s | 32.2 MB | 0.00 / 0.00 | 0, 0.00 / 0.00 | 0 | 0 | 1.7 / 0.3 / 0.3 | 5612 / 190 / 619 |

### Object sizes (JSON as served, includes managedFields)

| N | addrs | design | resource | count | avg bytes | max bytes |
|---|---|---|---|---|---|---|
| 100 | 100 | orig | origegressendpoints | 50 | 30899 | 31848 |
| 100 | 100 | orig | origegressnodestates | 100 | 4256 | 4256 |
| 100 | 100 | decided | egressendpoints | 100 | 8984 | 9436 |
| 100 | 100 | decided | egressmembers | 50 | 14838 | 15514 |
| 100 | 100 | decided | egressnodestates | 100 | 1544 | 1544 |
| 100 | 100 | scoped | egressendpoints | 100 | 7536 | 7894 |
| 100 | 100 | scoped | egressmembers | 50 | 15254 | 15959 |
| 100 | 100 | scoped | egressnodestates | 100 | 10133 | 10940 |
| 100 | 500 | orig | origegressendpoints | 250 | 10201 | 12058 |
| 100 | 500 | orig | origegressnodestates | 100 | 17806 | 17806 |
| 100 | 500 | decided | egressendpoints | 500 | 4241 | 4905 |
| 100 | 500 | decided | egressmembers | 250 | 3611 | 4154 |
| 100 | 500 | decided | egressnodestates | 100 | 1546 | 1546 |
| 500 | 100 | decided | egressendpoints | 100 | 15843 | 16916 |
| 500 | 100 | decided | egressmembers | 50 | 14994 | 15582 |
| 500 | 100 | decided | egressnodestates | 500 | 1547 | 1547 |
| 500 | 100 | scoped | egressendpoints | 100 | 13020 | 13872 |
| 500 | 100 | scoped | egressmembers | 50 | 15388 | 16006 |
| 500 | 100 | scoped | egressnodestates | 500 | 4821 | 6507 |
| 500 | 500 | scoped | egressendpoints | 500 | 4125 | 4388 |
| 500 | 500 | scoped | egressmembers | 250 | 3721 | 4193 |
| 500 | 500 | scoped | egressnodestates | 500 | 5379 | 7842 |
| 500 | 500 | decided | egressendpoints | 500 | 4693 | 5008 |
| 500 | 500 | decided | egressmembers | 250 | 3721 | 4203 |
| 500 | 500 | decided | egressnodestates | 500 | 1581 | 1758 |

## Option A: host-route advertisers in EgressNodeState spec (2026-10-21)

**Superseded (user decision 2026-10-21):** option C replaces option A (every
live selector node advertises every routed host-route address; no
`spec.advertisers`). Kept for the record; option C is measured below.

Scoped design, decided allocator settings (4 writers, priority lane, QPS
200, 4 reserves), N=500, 500 addresses, 10 s lease, one fresh cluster per run,
t2-kwok (16 vCPU). New harness flags `-hostroute-frac`, `-selector-nodes`,
`-selector-nopods`; scripts `optiona-n500.sh`, `optiona-repeat-n500.sh`;
results in `results/option-a/`. Rollout phase (burst in parentheses):

| run | egress writes/s | advertiser-only spec writes | set changes | holders per change max / mean | slice-table watch | ENS watch MB/s | apiserver CPU | claim p50 / p99 s |
|---|---|---|---|---|---|---|---|---|
| no egress (same day) | 0 | -- | -- | -- | 0 | 0 | 3.29 | -- |
| R0b scoped control | 216 | 0 | 0 | -- | 0.365 MB/s | 1.664 | 8.02 | 12.59 / 21.54 |
| R0 with option A code removed | 207 | -- | -- | -- | 0.342 | 1.590 | 7.95 | 13.01 / 22.25 |
| R1: 10 selector nodes, no pods on them | 206 | 0 | 0 | -- | 0.347 | 3.243 | 8.03 | 12.79 / 23.70 |
| R2: 10 selector nodes with pods | 165 | 88 (0.58/s; burst 2.76/s) | 383 | 40 / 31.1 | 0.251 | 2.253 | 8.25 | 18.18 / 33.25 |
| R3: 50 selector nodes with pods | 177 | 134 (0.92/s; burst 11.73/s) | 1,511 | 40 / 30.2 | 0.279 | 2.647 | 8.42 | 16.33 / 27.88 |

(R0 and a repeat of R2 ran in a second, unexplained mode with more
EgressMembers/EgressEndpoints writes and a shorter rollout: R0 521 writes/s,
R2b 286; see the logs.) No run had 429s or late lease renewals.

- **Option A's own writes are small** (0-0.92/s in the rollout, up to
  11.73/s in the burst): the allocator's work queue (about 900-1,000 dirty
  keys) coalesces the many set changes into few spec writes.
- **Its real cost is object size:** the advertiser list roughly doubles the
  average EgressNodeState (5,381 B to 10,572-12,430 B; largest 20,591 B), so
  the allocator's watch of all EgressNodeStates carries about twice the
  bytes (1.66 to 2.25-3.97 MB/s).
- Claim p50 in R2 and R3 is 4-6 s above the control (one run each; suggestive,
  not established, against ~3 s run-to-run noise).
- **Not modelled:** drained slices are never freed (holders accumulate to
  about 40 per address); the slice-table watcher/acker set does not follow
  the D7 advertiser set (in R1 all 10 selector nodes would advertise, watch
  and gate every slice table -- probably a larger cost than option A, and
  unmeasured).
- **A gap in option A as specified:** a selector node that holds no slice of
  an address gets no `spec.advertisers` entry for it, so it cannot learn that
  the selector-and-holders set is empty, which is exactly when D7 says every
  selector node advertises.
- **The control itself exceeds the approved spike 10 API budget** (<= 120
  egress writes/s, slice-table watch <= 0.2 MB/s, apiserver <= +2 cores): 207-216
  writes/s, 0.34-0.37 MB/s, +4.7 cores. That budget was set from the
  one-writer scoped run (84 writes/s, 0.1 MB/s, 6.0 vs 4.7 cores), before
  accounting for the allocator settings decided on 2026-10-19.

## Where the egress writes come from (2026-10-21, from existing runs)

Rollout phase, N=500, 500 addresses, scoped design, stepping from the
one-writer run (the spike 10 API budget's basis) to the decided allocator
settings:

| run | slice-table writes/s | EgressMembers/s | node-spec writes/s | node-status writes/s (acks + claims) | egress writes/s | acks |
|---|---|---|---|---|---|---|
| scoped, 1 writer, 5 s lease | 11.8 | 5.0 | 8.3 | 58.7 | 83.7 | 5,561 |
| 1 writer, 10 s lease | 13.3 | 5.5 | 9.1 | 64.0 | 91.9 | 5,912 |
| 4 writers | 23.7 | 9.1 | 16.8 | 97.5 | 147.2 | 8,941 |
| 4 writers + priority | 29.9 | 5.1 | 14.7 | 105.7 | 155.4 | 10,943 |
| + allocator QPS 200 | 35.7 | 9.2 | 16.8 | 120.0 | 181.7 | 12,571 |
| + 4 reserves (decided) | 43.8 | 8.9 | 20.5 | 140.4 | 213.6 | 14,944 |
| same-day control R0b | 44.3 | 8.4 | 21.0 | 142.4 | 216.1 | 14,806 |

The totals follow writes = 3 x slice-table versions (each acked by its 2
announcers) + 2 x node-spec versions (each acked by its node) + EgressMembers
+ claims; for R0b, 3 x 6,069 + 2 x 2,874 + 1,218 + 4,722 = 29,895 in 137 s
= 218/s (measured 216). Claims are constant (4,722, one per pod that needs a
slice). What grew is versions: slice-table versions per address in the
rollout went from about 4 (one writer) to about 12 (decided settings), and
node-spec versions doubled. A single slow writer coalesces many slice changes
into one version per object; faster writing emits more versions, and every
version brings its acks. The extra writes are the cost of the faster claims
(89 / 128 s to 12 / 25 s). Under option C each routed slice-table version is
acked by K selector nodes, so its term becomes (K + 1) x versions.

## Option C: every selector node advertises every routed address (2026-10-21)

Same settings (decided allocator, N=500, 500 addresses, 10 s lease, fresh
cluster per run, t2-kwok). `-hostroute-model C`: no `spec.advertisers`; each
routed address's slice table is announced, watched (one label-selector watch
per selector node) and acked by all K selector nodes; activation gates on all
K. Script `optionc-n500.sh`; results `results/option-c/`. Rollout phase
(claim burst in brackets); no 429s and no late renewals in any run:

| run | egress writes/s | acks (by selector nodes) | slice-table watch | per selector node | ENS watch MB/s | apiserver cores / peak MiB (baseline 3.77 / 1,001) | claim p50 / p99 s | ack p50 / p99 s |
|---|---|---|---|---|---|---|---|---|
| C0 control (no routed addresses) | 189 (176) | 12,770 | 0.30 MB/s | -- | 1.45 | 8.04 / 2,795 | 15.13 / 27.08 (7.09 / 10.01) | 0.08 / 1.10 |
| C1: K=10, no pods on selector nodes | 155 (128) | 4,219 (1,136) | 1.99 MB/s | 193 kB/s | 1.27 | 7.68 / 2,758 | 11.01 / 18.80 (4.65 / 6.91) | 0.67 / 1.87 |
| C1b: repeat | 171 (128) | 4,445 (1,150) | 2.40 MB/s | 233 kB/s | 1.37 | 7.67 / 2,876 | 9.36 / 14.98 (4.75 / 6.69) | 0.53 / 1.35 |
| C2: K=10, pods on selector nodes | 153 (142) | 4,011 (1,085) | 1.97 MB/s | 192 kB/s | 1.32 | 7.74 / 3,063 | 11.39 / 18.88 (4.11 / 5.55) | 1.01 / 1.75 |
| C3: K=50, pods on selector nodes | 186 (138) | 8,124 (5,457) | 11.55 MB/s | 229 kB/s | 3.12 | 8.09 / 5,763 | 11.28 / 17.15 (6.26 / 8.64) | 1.08 / 2.48 |

- **Gating on K acks does not raise claim latency**: claims wait on the
  allocator queue (mean about 920 keys in every run), not on acks; ack
  latency rises (each selector node acks at most once a second) but stays
  under 2.5 s p99.
- **Writes fall**: total acks drop about 3x at K=10, because with every
  address routed only selector nodes ack slice tables, and each node's
  status acks all its addresses in one debounced write.
- **The cost is slice-table watch traffic**: every selector node receives
  every routed slice-table change, 0.19-0.25 MB/s per node, so the total
  grows linearly with K (2.0-2.4 MB/s at K=10, 11.6 MB/s at K=50), and
  apiserver peak memory reaches 5.8 GB at K=50 (2.8 GB at K=10).
- Limits: one label-selector watch per selector node (the real design would
  open K x addresses by-name watches); drained slices never freed; no
  selector node lost; loadgen used 2.6 cores at K=50 on the same VM.

## Merge window for slice-table versions: no effect (2026-10-21)

Flags `-ep-merge` / `-spec-merge` hold an ordinary-queue write of an object
until `lastWrite + window`; priority-lane writes (claims, activations, the
claimant's spec) are never held. Scripts `mergewindow-n500.sh`,
`mergewindow2-n500.sh`; results `results/merge-window/`. Decided settings,
N=500, 500 addresses, scoped, fresh cluster each; no 429s, no late renewals.

| run (rollout) | egress writes/s | slice-table versions per address | held by the window | claim p50 / p99 s |
|---|---|---|---|---|
| M0 / M0c controls | 530 / 512 | 31.5 / 30.5 | -- | 4.34 / 7.29, 4.35 / 7.86 |
| M250 | 478 | 27.8 | 0 | 5.21 / 8.80 |
| M500 | 495 | 28.7 | 2 | 4.82 / 8.55 |
| M1000, M1000b, M1000c | 473, 511, 522 | 27.7, 30.2, 30.9 | 0 | 4.47-5.19 / 7.70-9.27 |
| M1000 + spec-merge 1 s | 507 | 29.8 | 6 | 4.63 / 8.17 |
| option C (K=10) + ep-merge 1 s | 257 | 28.6 | 39 | 4.77 / 8.35 |

The window never engages: lane counters show 15,247 of 15,257 slice-table
writes and 4,020 of 4,238 node-spec writes on the priority lane (M0c;
`lanes: slice-table writes priority=15413 ordinary=12` in M1000c).
Pod-driven slice changes ride along on the next claim or activation write of
the same object, because the allocator rewrites the whole object; by the time
an ordinary-queue entry reaches the head there is nothing left to write.
Versions come from claims and activations themselves (about 30 per address
against about 19 claims per address). The only remaining lever is merging
inside the priority lane, which adds up to the window to claim latency (not
measured).

**Two run modes (correction to "Where the egress writes come from").** All
nine runs above are in a "fast" mode: about 253 EgressMembers writes, a
110 s rollout, about 30 slice-table versions per address, claim p50
4.3-5.2 s, egress writes 473-530/s. R0b and the ladder's decided-settings run
were in a "slow" mode: about 1,000-1,450 EgressMembers writes, 137-140 s, about
12 versions per address, claim p50 12.6-15 s, egress writes about 216/s. Same
settings, different builds and days; the cause is not established. The
breakdown formula holds in both modes, but the absolute write rate differs by
about 2.4x, so no API budget should be set from either mode until the cause is
known.

