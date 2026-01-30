# PureLB Router-Based E2E Tests

End-to-end tests that verify PureLB's functionality through **actual connectivity** via BGP routing infrastructure. These tests validate what the `remote/` tests cannot - that traffic reaches services through BGP-learned routes.

## Two Test Versions

| Script | Router CLI Required | Use Case |
|--------|---------------------|----------|
| `test-router-connectivity.sh` | No | Quick connectivity validation |
| `test-router-connectivity-frr.sh` | Yes (FRR) | Full BGP route verification |

### Basic Version (`test-router-connectivity.sh`)
Tests connectivity without needing router CLI access. Proves that traffic flows correctly but doesn't verify the BGP routing table directly.

### FRR Version (`test-router-connectivity-frr.sh`)
Extended tests that query the FRR router's RIB to verify:
- Routes appear/disappear correctly
- Correct number of ECMP next-hops
- Proper /32 aggregation
- Route withdrawal timing

## Architecture

```
┌─────────────────┐
│ This Host       │  <-- runs the test script, has kubectl
│ (workstation)   │  <-- curls VIPs directly
└────────┬────────┘
         │
┌────────▼────────┐
│   FRR Router    │  <-- ROUTER_HOST (FRR version only)
│   (BGP peer)    │
│   Receives routes│
└────────┬────────┘
         │
┌────────┴────────────────────────────────────────┐
│                                                 │
▼                    ▼                    ▼
┌─────────────┐    ┌─────────────┐    ┌─────────────┐
│   Node 1    │    │   Node 2    │    │   Node 3    │
│  GoBGP      │    │  GoBGP      │    │  GoBGP      │
│  VIP on     │    │  VIP on     │    │  VIP on     │
│  kube-lb0   │    │  kube-lb0   │    │  kube-lb0   │
└─────────────┘    └─────────────┘    └─────────────┘
```

**Key points:**
- Nodes run **GoBGP** (integrated into lbnodeagent)
- External router is **FRR** (Free Range Routing)
- Connectivity tests run from where the script runs (your workstation)

## Comparison with Other E2E Tests

| Test Suite | What it Validates | Connectivity Source |
|------------|-------------------|---------------------|
| `local/` | IP on eth0, leader election | From cluster nodes |
| `remote/` | IP on kube-lb0, nftables rules | From cluster nodes/pods |
| **`router/`** | **Connectivity via BGP routes** | **From this host** |

## Prerequisites

### Common Requirements
- SSH access to all cluster nodes (passwordless)
- PureLB deployed with BGP configuration
- `remote` ServiceGroup configured (or custom via `SERVICE_GROUP`)
- This host can reach VIPs via the router

### FRR Version Additional Requirements
- SSH access to FRR router
- `vtysh` accessible (directly or via sudo)

## Running Tests

### Basic Version
```bash
./test-router-connectivity.sh

# Run specific test
./test-router-connectivity.sh --test 1
```

### FRR Version
```bash
export ROUTER_HOST="frr-router"
./test-router-connectivity-frr.sh

# Run specific test
./test-router-connectivity-frr.sh --test 2
```

## Test Cases

### Basic Version Tests

| Test | Description |
|------|-------------|
| 0 | Prerequisites (SSH, PureLB, ServiceGroup) |
| 1 | Basic Connectivity (IPv4) |
| 2 | ECMP Traffic Distribution |
| 3 | Node Failure and Recovery |
| 4 | ETP Local Connectivity |
| 5 | Service Deletion Cleanup |
| 6 | Full Lifecycle Test |

### FRR Version Tests

| Test | Description |
|------|-------------|
| 0 | Prerequisites (+ FRR vtysh, BGP sessions) |
| 1 | Basic Connectivity with Route Verification |
| 2 | ECMP with Next-Hop Verification |
| 3 | Node Failure with Route Withdrawal |
| 4 | Service Deletion with Route Withdrawal |
| 5 | Route Aggregation Verification (/32) |

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `ROUTER_HOST` | FRR only | - | FRR router for route queries |
| `SERVICE_GROUP` | No | `remote` | ServiceGroup to use |
| `BGP_CONVERGE_TIMEOUT` | No | `30` | Seconds to wait for BGP |
| `ECMP_TEST_REQUESTS` | No | `100` | Requests for ECMP test |
| `VIP_SUBNET` | No | `10.255.0.0/24` | Subnet for FRR queries |

## Troubleshooting

### "Cannot reach VIP"
- Verify routing: `ip route get <VIP>`
- Check if VIP is on nodes: `ssh node "ip addr show kube-lb0"`
- Verify BGP is working

### "Cannot access FRR vtysh"
```bash
ssh $ROUTER_HOST "vtysh -c 'show version'"
ssh $ROUTER_HOST "sudo vtysh -c 'show version'"
```

### "BGP sessions not established"
```bash
# On FRR router
vtysh -c "show bgp summary"

# On cluster node (GoBGP)
kubectl logs -n purelb-system-l component=lbnodeagent | grep -i bgp
```

### "ECMP not distributing traffic"
- ECMP is flow-based (src IP + src port + dst IP + dst port)
- Verify FRR has `maximum-paths` configured
- Different source ports should create different flows

## Related Files

- `../remote/` - Tests that verify IP placement without external routing
- `../remote/servicegroup-remote.yaml` - Default ServiceGroup used by these tests
