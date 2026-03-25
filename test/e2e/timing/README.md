# PureLB Timing Behavior Tests

This test suite characterizes timing behavior in PureLB's lbnodeagent, specifically for ETP Local (externalTrafficPolicy: Local) scenarios.

## Purpose

Unlike pass/fail E2E tests, this suite:
- Documents and verifies timing guarantees PureLB provides
- Characterizes delays at each stage of the service lifecycle
- Establishes baselines for expected timing behavior
- Provides evidence for test timeout adjustments

## Test Categories

### Category B: EndpointSlice Cache Timing

| Test | Purpose | Expected Latency |
|------|---------|------------------|
| B1: Cache Sync Latency | Time from pod Ready to VIP placement | 500-2000 ms |
| B3: Endpoint Termination | Time from pod delete to VIP withdrawal | 500-3000 ms |

### Category D: kube-proxy nftables Timing

| Test | Purpose | Expected Latency |
|------|---------|------------------|
| D1: Rule Programming | Time from VIP placement to nftables ready | 100-1000 ms |
| D3: End-to-End Traffic | Time from service create to first curl success | 1000-5000 ms |

### Category E: ETP Local Stress Tests

| Test | Purpose |
|------|---------|
| E1: Rapid Scaling | Verify correctness under 0→2→0→1→3→1→0 scaling |
| E3: Endpoint Migration | Measure VIP migration when pod moves to different node |

## Prerequisites

- SSH access to all cluster nodes
- kubectl configured with cluster access
- nginx deployment in test namespace
- PureLB deployed and running

The test script will automatically create the required ServiceGroups
(`default` and `remote`) if they do not exist.

**Important:** `servicegroup-default.yaml` defines a local pool whose
`subnet` and `pool` must match the network configured on the cluster
nodes' default interface. If your cluster uses a different subnet than
`172.30.255.0/24`, edit the file before running the tests. The IPv6 pool
must similarly match the node interface's IPv6 subnet.

## Running Tests

```bash
# Run with default 3 iterations
./test-timing-behavior.sh

# Run with custom iteration count
./test-timing-behavior.sh 5
```

## Files

| File | Description |
|------|-------------|
| `test-timing-behavior.sh` | Main test script |
| `servicegroup-default.yaml` | Local pool ServiceGroup (v2 API) — must match cluster network |
| `servicegroup-remote.yaml` | Remote pool ServiceGroup (v2 API) |
| `svc-etp-local.yaml` | Fixed-name ETP Local service (used by B3, E1, E3) |
| `svc-timing-etp-local.yaml` | Per-iteration ETP Local service template (used by B1) |
| `svc-timing-standard.yaml` | Per-iteration standard LB service template (used by D1, D3) |
| `timing-results-*.csv` | Historical timing measurements |

## Output

Results are saved to `timing-results-YYYYMMDD-HHMMSS.csv` with:
- Per-iteration timing measurements
- Summary statistics (min, max, avg, p95)

## Benchmark Results

Results from 8 test runs on a 5-node cluster (3 iterations each).
Cluster: Proxmox VMs, Kubernetes 1.32, kube-proxy nftables mode, flannel CNI.

### Summary (averages across runs, in ms)

| Test | Metric | Jan 21-28 (6 runs) | Mar 2 (2 runs) |
|------|--------|-------------------|----------------|
| B1 | Pod Ready → VIP placed | 679 | 732 |
| B3 | Pod delete → VIP removed | 1536 | 1692 |
| D1 | VIP → nftables ready | 508 | 346 |
| D3 | Service create → traffic OK | 1159 | 1257 |
| E3 | Migration total | 2743 | 2754 |

### Per-Run Detail (avg / p95, in ms)

| Date | B1 | B3 | D1 | D3 | E3 |
|------|----|----|----|----|-----|
| Jan 21 10:35 | 856 / 1105 | 1712 / 1903 | 373 / 404 | 985 / 1546 | 2694 / 2855 |
| Jan 21 10:39 | 604 / 1061 | 1542 / 1972 | 510 / 821 | 1156 / 1511 | 2696 / 2833 |
| Jan 21 13:46 | 599 / 1044 | 1613 / 1714 | 656 / 821 | 1299 / 1621 | 2901 / 3177 |
| Jan 22 18:35 | 1037 / 1054 | 1446 / 1982 | 645 / 794 | 1018 / 1525 | 2640 / 3016 |
| Jan 22 18:40 | 606 / 1055 | 1586 / 1754 | 357 / 366 | 1211 / 1516 | 2661 / 3076 |
| Jan 28 13:17 | 370 / 379 | 1316 / 1385 | 505 / 821 | 1285 / 1581 | 2866 / 3130 |
| Mar 2 10:44 | 609 / 1076 | 1660 / 1792 | 342 / 362 | 1228 / 1478 | 2600 / 3113 |
| Mar 2 10:49 | 855 / 1114 | 1724 / 1880 | 350 / 365 | 1286 / 1589 | 2908 / 3217 |

### Notes

- **B1 first-iteration warmup**: The first iteration of each B1 run typically
  takes ~1050ms due to EndpointSlice informer cache population. Subsequent
  iterations settle to ~370ms. The Jan 28 run is an outlier where all three
  iterations hit ~370ms (likely warm cache from a prior test).
- **E1 (Rapid Scaling)**: All runs passed correctness checks — VIP count
  always matched endpoint count at every scaling step.
- Jan 21-28 runs used pre-subnet-aware-election code (memberlist).
  Mar 2 runs used lease-based subnet-aware election. No significant
  regressions observed.

## Interpreting Results

Key metrics to watch:

| Metric | Healthy Range | Warning Threshold |
|--------|--------------|-------------------|
| Pod Ready → VIP placed | < 2000 ms | > 5000 ms |
| Pod delete → VIP removed | < 3000 ms | > 10000 ms |
| VIP → nftables ready | < 1000 ms | > 3000 ms |
| Service → traffic OK | < 5000 ms | > 10000 ms |
| Migration total time | < 15000 ms | > 30000 ms |

If timing exceeds warning thresholds consistently, investigate:
1. Network latency between nodes
2. API server performance
3. kube-proxy sync interval
4. lbnodeagent work queue depth
