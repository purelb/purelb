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

## Running Tests

```bash
# Run with default 3 iterations
./test-timing-behavior.sh

# Run with custom iteration count
./test-timing-behavior.sh 5
```

## Prerequisites

- SSH access to all cluster nodes
- kubectl configured with cluster access
- nginx deployment in test namespace
- PureLB deployed and running

## Output

Results are saved to `timing-results-YYYYMMDD-HHMMSS.csv` with:
- Per-iteration timing measurements
- Summary statistics (min, max, avg, p95)

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
