# PureLB E2E Tests

End-to-end functional tests for PureLB.

## Test Suites

| Directory | Description |
|-----------|-------------|
| [local/](local/) | Tests for local IP allocation mode (addresses on physical NIC) |
| [remote/](remote/) | Tests for remote IP allocation mode (addresses on kube-lb0) |
| [timing/](timing/) | Tests for ETP Local timing behavior and latency characterization |
| [address-lifetime/](address-lifetime/) | Tests for address lifetime/flags to prevent CNI conflicts (Flannel) |

## Running Tests

Each test suite has its own README with specific instructions. Generally:

```bash
cd <test-directory>
./<test-script>.sh
```

## Testing Methodology

These E2E tests use **SSH-based connectivity testing** rather than external routing (BGP, static routes). This approach:

### What We Test
- PureLB allocates IPs correctly from configured pools
- IPs are placed on the correct interface (eth0 for local, kube-lb0 for remote)
- Correct number of nodes announce each IP (1 for local with election, all for remote)
- externalTrafficPolicy: Local works correctly for remote addresses
- kube-proxy programs nftables rules for LoadBalancer IPs
- Services are reachable via their VIP (tested from cluster nodes)
- Cleanup removes IPs from interfaces and nftables rules

### What We Don't Test
- External BGP route propagation
- Traffic from truly external clients (outside the cluster)
- ARP/NDP announcement to upstream routers
- Integration with external routing infrastructure

### Why SSH-Based Testing

External routing depends on network infrastructure outside PureLB's control. SSH-based testing:
1. **Isolates PureLB** - Tests PureLB's functionality without conflating network issues
2. **Self-contained** - No special routing configuration needed on the test host
3. **Deterministic** - No flaky failures from routing cache or ECMP selection
4. **Validates the essentials** - If the VIP is on the interface and kube-proxy rules exist, external routing will work

### Prerequisites

- SSH access to all cluster nodes
- kubectl configured for cluster access
- kube-proxy running in **nftables mode** (for remote tests)

## Adding New Tests

When adding new test suites:

1. Create a subdirectory under `test/e2e/` for the feature being tested
2. Include a README.md documenting the tests
3. Include any required Kubernetes manifests
4. Ensure the test script cleans up after itself
5. Follow the testing patterns in existing test suites
