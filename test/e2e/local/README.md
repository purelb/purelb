# PureLB Local Allocation E2E Tests

End-to-end functional tests for PureLB's local IP allocation mode.

## Prerequisites

- A Kubernetes cluster with PureLB deployed
- `kubectl` configured to access the cluster
- SSH access to cluster nodes (for verifying VIP placement)
- A `test` namespace with an nginx deployment

## Test Suite

Run the complete test suite:

```bash
./test-local-allocation.sh
```

### Tests Included

| Test | Description |
|------|-------------|
| **Test 1: IPv4 Single-Stack** | Allocates an IPv4-only LoadBalancer service and verifies connectivity |
| **Test 2: IPv6 Single-Stack** | Allocates an IPv6-only LoadBalancer service and verifies connectivity |
| **Test 3: Dual-Stack** | Allocates a dual-stack service with both IPv4 and IPv6 addresses |
| **Test 4: Leader Election** | Verifies only one node announces each VIP (no split-brain) |
| **Test 5: Service Deletion** | Verifies VIPs are removed from nodes when services are deleted |
| **Test 6: IP Sharing** | Tests `purelb.io/allow-shared-ip` annotation for sharing IPs between services with different ports, and verifies port conflicts are rejected |
| **Test 7: Load Balancing** | Verifies traffic is distributed across multiple backend pods |
| **Test 8: Node Failover** | Simulates node failure and verifies VIP moves to another node |
| **Test 9: Specific IP Request** | Tests `purelb.io/addresses` annotation for requesting specific IPs |
| **Test 10: Split-Brain Check** | Final verification that no VIPs are duplicated across nodes |

## Configuration

The tests use a ServiceGroup configured for a local subnet. Edit `servicegroup.yaml` to match your network:

```yaml
spec:
  local:
    subnet: '172.30.255.0/24'      # Your local subnet
    pool: '172.30.255.150-172.30.255.200'  # Available IP range
    v6subnet: '2001:470:b8f3:2::/64'       # IPv6 subnet (optional)
    v6pool: '2001:470:b8f3:2:a::-2001:470:b8f3:2:a::ff'  # IPv6 range
```

## Files

- `test-local-allocation.sh` - Main test script
- `servicegroup.yaml` - ServiceGroup configuration for the test pool
- `nginx-test.yaml` - Test nginx deployment and namespace
- `nginx-svc-*.yaml` - Service definitions (used by kustomize)
- `kustomization.yaml` - Kustomize configuration

## Cleanup

The test script automatically cleans up test services on successful completion. If a test fails, some resources may remain. Clean up manually with:

```bash
kubectl delete svc -n test --all
kubectl taint nodes --all purelb-test- 2>/dev/null || true
```
