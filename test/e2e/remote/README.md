# PureLB Remote Address E2E Tests

End-to-end functional tests for PureLB's **remote address allocation** - addresses that don't match any physical NIC subnet and are placed on the dummy interface (kube-lb0).

## Local vs Remote Addresses

| Aspect | Local Addresses | Remote Addresses |
|--------|-----------------|------------------|
| Detection | IP matches a physical NIC subnet | IP does NOT match any NIC subnet |
| Interface | Physical NIC (eth0) | Dummy interface (kube-lb0) |
| Election | Leader election - only 1 node announces | NO election - ALL nodes announce |
| externalTrafficPolicy: Local | Not supported | Fully supported |
| Aggregation | Uses interface subnet mask | Uses ServiceGroup config (/32, /128) |

## Prerequisites

### Required
- SSH access to all cluster nodes (passwordless)
- kubectl configured for cluster access
- PureLB deployed and running
- Test namespace with nginx deployment

### kube-proxy Mode
**IMPORTANT**: This test suite requires kube-proxy in **nftables mode**.

Verify with:
```bash
ssh <node> "sudo nft list tables | grep kube-proxy"
```

If using iptables or IPVS mode, the nftables verification functions will fail.

## Testing Methodology

### SSH-Based Connectivity Testing

These tests use SSH to verify functionality from **within the cluster** rather than testing from an external client. This approach:

**Validates:**
- IP allocated from correct pool
- IP placed on kube-lb0 (not eth0)
- IP on correct nodes (all for Cluster, endpoint-only for Local)
- Correct aggregation prefix (/32 for IPv4, /128 for IPv6)
- kube-proxy nftables rules programmed
- Service reachable via VIP from nodes
- Cleanup removes IPs and nftables rules

**Does NOT validate:**
- BGP route announcement to external routers
- Traffic from external clients (outside cluster network)
- ARP/NDP responses to upstream network equipment
- Integration with external routing infrastructure

### Why Not External Routing Tests?

External routing (BGP, static routes, ECMP) depends on infrastructure outside PureLB's control:

1. **ECMP caching**: Kernel caches nexthop selection, causing false failures when VIPs move
2. **BGP convergence**: Route propagation timing varies, making tests non-deterministic
3. **Infrastructure dependency**: Tests would require specific network setup

SSH-based testing isolates PureLB's functionality. If the VIP is correctly placed on the interface and kube-proxy has programmed the DNAT rules, external routing will work once configured.

## Running Tests

```bash
./test-remote-allocation.sh
```

## Test Cases

| Test | Description |
|------|-------------|
| 0 | Prerequisites (SSH, kubectl, nftables mode, PureLB running) |
| 1-4 | Core remote functionality (IPv4, IPv6, dual-stack, all-node announcement) |
| 5-7 | externalTrafficPolicy: Local (basic, migration, zero endpoints) |
| 8-9 | Aggregation verification with explicit config (/32, /128 prefixes) |
| 10-11 | Aggregation verification with default (uses subnet mask: /24, /64) |
| 12-14 | Service lifecycle (deletion, IP sharing, specific IP request) |
| 15-17 | Mixed pools and node failure |
| 18-20 | Negative tests (pool exhaustion, invalid requests) |
| 21 | Final validation |
| 22 | ETP Cluster to Local transition |
| 23 | Add sharing annotation to existing service |
| 24 | SingleStack to DualStack transition |
| 25-26 | LBNodeAgent restart recovery |

## Configuration

There are two ServiceGroups for testing:

1. **remote** - With explicit aggregation (`servicegroup-remote.yaml`)
2. **remote-default-aggr** - Without explicit aggregation (`servicegroup-remote-default-aggr.yaml`)

Edit `servicegroup-remote.yaml` to match your test environment:

```yaml
spec:
  local:
    v4pools:
    - aggregation: /32           # Host routes (common production config)
      pool: 10.255.0.100-10.255.0.150
      subnet: 10.255.0.0/24
    v6pools:
    - aggregation: /128
      pool: fd00:10:255::100-fd00:10:255::150
      subnet: fd00:10:255::/64
```

## Cleanup

The test script cleans up automatically. For manual cleanup:

```bash
kubectl delete svc -n test -l test-suite=remote
kubectl delete servicegroup -n purelb remote
```

## Troubleshooting

### "kube-proxy not in nftables mode"
Verify kube-proxy configuration. These tests require `--proxy-mode=nftables`.

### SSH failures
Ensure passwordless SSH to all nodes: `ssh <node> hostname`

### VIP not appearing on kube-lb0
Check lbnodeagent logs: `kubectl logs -n purelb -l app.kubernetes.io/component=lbnodeagent`

### nftables rules missing
Check kube-proxy logs and verify service has an external IP assigned.

## Test Assertions

Every remote IP test verifies:
- IP is on kube-lb0 (not eth0)
- IP has correct aggregation prefix (/32 or /128)
- Expected number of nodes have IP (all for Cluster, endpoint-only for Local)
- Service annotations are correct

Every cleanup test verifies:
- IP removed from ALL expected nodes
- No orphaned IPs on kube-lb0
- nftables rules cleaned up

## Critical Files Reference

- `test/e2e/local/test-local-allocation.sh` - Pattern followed by this test
- `internal/local/announcer_local.go:269-308` - announceRemote() logic
- `internal/local/network.go:223-285` - addVirtualInt() aggregation handling
- `pkg/apis/purelb/v1/types.go:206-222` - ServiceGroupAddressPool structure
