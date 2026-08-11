# PureLB E2E Tests

End-to-end functional tests for PureLB.

## Test Suites

| Directory | Description |
|-----------|-------------|
| [local/](local/) | Tests for local IP allocation mode (addresses on physical NIC) |
| [remote/](remote/) | Tests for remote IP allocation mode (addresses on kube-lb0) |
| [py/](py/) | **pytest** harness. External (sidecar) IPAM lives here — also the acceptance test when adding a new sidecar IPAM implementation. The bash suites are being migrated into it. |
| [timing/](timing/) | Tests for ETP Local timing behavior and latency characterization |
| [address-lifetime/](address-lifetime/) | Tests for address lifetime/flags to prevent CNI conflicts (Flannel) |

Suites are being migrated to pytest under [py/](py/). A suite is only
deleted once `scripts/e2e-dualrun.sh` reports every one of its bash
assertions agreeing with a passing pytest counterpart; the mapping that
enforces this is [dualrun-map.yaml](dualrun-map.yaml).

## Shared Code

| File | Contents | Who sources it |
|------|----------|----------------|
| [lib.sh](lib.sh) | Colours, `pass`/`fail`/`info`, the `kubectl` context wrapper, and all metric and log assertions. **Nothing runs at source time.** | Everything, directly or via `common.sh` |
| [common.sh](common.sh) | `lib.sh` plus SSH-based discovery of nodes, interfaces, subnets and IPv6, ServiceGroup generation and VIP helpers. **Sourcing it contacts every node.** | `local/`, `remote/`, `timing/` |

`router/` and `single-node/` source `lib.sh` only: they are driven from the
workstation and should not depend on being able to SSH to every node.

Add a new assertion to `lib.sh`, not to a suite. These helpers previously
existed in up to five copies with four distinct implementations, so a fix
applied in one suite silently left the others asserting the old way.

To say something useful when your suite fails, override `dump_debug_state`
after sourcing — `fail()` calls it just before exiting.

## Resetting the cluster

Run before any suite, so results are reproducible:

```bash
./scripts/reset-test-cluster.sh --context <name> [--yes]
```

It removes Gateways (whose controllers otherwise recreate their LoadBalancer
Services within seconds), every LoadBalancer Service outside the install
namespace, every ServiceGroup and LBNodeAgent, the scratch namespaces, and
stale `purelb-test` taints — then verifies via SSH that no PureLB address is
still configured on any node. It deliberately does **not** touch the `test`
namespace, which holds the nginx backend the suites require but do not create.

## Running Tests

Each test suite has its own README with specific instructions. Generally:

```bash
cd <test-directory>
./<test-script>.sh
```

### Multi-interface test (gated)

`test_multi_interface` in the local suite exercises nodeSelector-scoped
LBNodeAgent CRs and multi-NIC announcement on a dual-homed node. It only
runs when the `MULTI_IF_*` environment variables are set (all six are
required together — a partial set fails loudly rather than silently
skipping). One-command invocation for the prox-purelb2 profile
(purelb2-3 is dual-homed: eth1 on the 251 subnet, eth0 on the 250
subnet; nodes 1-2 provide the other-node-on-subnet the migration
assertion needs):

The pool ranges must not overlap the suite's generated default
ServiceGroup (`.200-.220` and `:a::1-:a::20` per subnet) or the per-test
ranges (`.230-.240`, `:b::1-:b::20`) — the allocator rejects overlapping
ServiceGroups outright.

```bash
cd local
MULTI_IF_NODE=purelb2-3 \
MULTI_IF_IFACE=eth0 \
MULTI_IF_SUBNET=172.30.250.0/24 \
MULTI_IF_SUBNET6=2001:470:b8f3:250::/64 \
MULTI_IF_POOL_V4=172.30.250.244-172.30.250.247 \
MULTI_IF_POOL_V6=2001:470:b8f3:250:c::1-2001:470:b8f3:250:c::10 \
./test-local-allocation.sh
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
