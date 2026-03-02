# PureLB Local Allocation E2E Tests

End-to-end tests for PureLB's local IP allocation mode, including functional
tests and failover stress testing.

## Prerequisites

- A Kubernetes cluster with PureLB deployed (tested on a 5-node proxmox cluster)
- `kubectl` configured with the `proxmox` context
- SSH access to cluster nodes (`purelb1`–`purelb5`) for verifying VIP placement
- A `test` namespace with an nginx deployment

## Test Scripts

### test-local-allocation.sh

Comprehensive functional test suite covering IP allocation, election, failover,
connectivity, and CNI compatibility.

```bash
./test-local-allocation.sh                # Single run
./test-local-allocation.sh -n 5           # Run 5 iterations
./test-local-allocation.sh -i             # Interactive mode (pause between groups)
./test-local-allocation.sh -i -n 3        # Both
```

**Options:**

| Flag | Description |
|------|-------------|
| `-i`, `--interactive` | Pause after each test group for manual review |
| `-n`, `--iterations N` | Run the full suite N times (default: 1) |
| `-h`, `--help` | Show help |

**Log output:** `/tmp/test-local-<timestamp>/output.log`

Each iteration runs in a subshell so a failure in one iteration does not abort
the remaining iterations. A pass/fail summary is printed at the end of
multi-iteration runs.

#### Tests Included

**Subnet-Aware Election**

| Test | Description |
|------|-------------|
| Lease Verification | Confirms 5 leases exist with `purelb.io/subnets` annotations |
| Local Pool No Matching Subnet | Allocates from a pool whose subnet matches no node; verifies IP is NOT announced |
| Remote Pool | Allocates from a remote pool; verifies IP appears on `kube-lb0` (not `eth0`) |

**Core Functionality**

| Test | Description |
|------|-------------|
| IPv4 Single-Stack | Allocates an IPv4-only LoadBalancer service and verifies connectivity |
| IPv6 Single-Stack | Allocates an IPv6-only LoadBalancer service and verifies connectivity |
| Dual-Stack | Allocates a dual-stack service with both IPv4 and IPv6 addresses |
| Leader Election | Verifies only one node announces each VIP (no split-brain) |
| Service Cleanup | Verifies VIPs are removed from nodes when services are deleted |
| IP Sharing | Tests `purelb.io/allow-shared-ip` for sharing IPs between services; verifies port conflicts are rejected |
| Specific IP Request | Tests `purelb.io/addresses` annotation for requesting specific IPs |
| Multi-Pod Load Balancing | Verifies traffic is distributed across multiple backend pods |

**Failover & High Availability**

| Test | Description |
|------|-------------|
| Node Failover | Taints a node to simulate failure, verifies VIP moves to another node |
| Graceful Failover | Deletes a pod and verifies lease-based failover within 15 seconds |

**Additional Functionality**

| Test | Description |
|------|-------------|
| ETP Local Override | Tests `externalTrafficPolicy: Local` behavior |
| No Duplicate VIPs | Final check that no VIP is present on more than one node |

**Address Lifetime & CNI Compatibility**

| Test | Description |
|------|-------------|
| Local VIP Address Flags | Verifies VIP addresses have correct kernel flags on `eth0` |
| Address Renewal Timer | Confirms address renewal keeps VIPs alive across lease cycles |
| Flannel Node IP | Validates VIPs coexist with flannel's node IP configuration |

**Cross-Node Connectivity**

| Test | Description |
|------|-------------|
| Cross-Node Connectivity | Verifies VIP is reachable from every node in the cluster |
| Pod Connectivity | Verifies VIP is reachable from inside a pod |

---

### stress-failover.sh

Stress test for failover race conditions. Runs varied failure scenarios across
multiple iterations to surface timing-dependent bugs.

```bash
./stress-failover.sh          # Default: 10 iterations
./stress-failover.sh 25       # 25 iterations
```

**Log output:** `/tmp/failover-stress-<timestamp>/` (one file per iteration plus summary)

#### Test Modes

Each iteration randomly selects from these scenarios:

| Mode | Description |
|------|-------------|
| Basic failover (graceful) | Deletes the VIP-holding pod and waits for recovery |
| Force kill | Kills the pod with `--grace-period=0` to simulate a hard crash |
| Cascading failover | Kills the first winner, then immediately kills the new winner |
| Election noise | Deletes random pods during failover to create contention |
| Multiple VIPs | Creates extra services to test election under contention |
| Node tainting | Taints the node to prevent pod rescheduling (simulates true node loss) |

Tainted tests verify the VIP moves to a different node. Non-tainted tests
accept same-node recovery (the DaemonSet recreates the pod on the same node).

## Configuration

The tests use ServiceGroups configured for a local subnet. Edit
`servicegroup.yaml` to match your network:

```yaml
spec:
  local:
    v4pool:
      subnet: '172.30.255.0/24'
      pool: '172.30.255.150-172.30.255.200'
    v6pool:
      subnet: '2001:470:b8f3:2::/64'
      pool: '2001:470:b8f3:2:a::-2001:470:b8f3:2:a::ff'
```

## Files

| File | Purpose |
|------|---------|
| `test-local-allocation.sh` | Functional test suite |
| `stress-failover.sh` | Failover stress test |
| `servicegroup.yaml` | ServiceGroup for the local pool |
| `servicegroup-no-match.yaml` | ServiceGroup with a subnet matching no node (for negative testing) |
| `servicegroup-remote.yaml` | ServiceGroup for a remote pool |
| `nginx-test.yaml` | Test nginx deployment and namespace |
| `nginx-svc-ipv4.yaml` | IPv4 single-stack service |
| `nginx-svc-ipv6.yaml` | IPv6 single-stack service |
| `nginx-svc-dualstack.yaml` | Dual-stack service |
| `nginx-svc-no-match.yaml` | Service using the no-match ServiceGroup |
| `nginx-svc-remote.yaml` | Service using the remote ServiceGroup |
| `kustomization.yaml` | Kustomize configuration |

## Cleanup

Both scripts clean up test services on success. If a test fails mid-run, clean
up manually:

```bash
kubectl delete svc -n test --all
kubectl taint nodes --all purelb-test- 2>/dev/null || true
```
