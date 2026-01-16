# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

PureLB is a Kubernetes service load balancer orchestrator that allocates IP addresses from configured pools and configures Linux networking to announce them. It consists of two main components:

- **Allocator** (`cmd/allocator`): Single cluster-wide pod that watches Services and ServiceGroups, manages IP allocation from pools (local or Netbox IPAM)
- **LBNodeAgent** (`cmd/lbnodeagent`): DaemonSet running on each node, configures local OS networking via netlink to announce allocated IPs

## Build Commands

All commands via `make`:

```bash
make check          # Run go vet and go test -race -short
make generate       # Generate k8s client code (pkg/generated/)
make crd            # Generate CRD manifests (deployments/crds/)
make image          # Build container images using ko
make manifest       # Generate k8s deployment YAML via kustomize
make helm           # Package Helm chart
make scan           # Run govulncheck for vulnerabilities
make run-allocator  # Run allocator locally (needs kubeconfig)
make run-lbnodeagent # Run node agent locally (set PURELB_NODE_NAME)
```

Run a single test:
```bash
go test -race -run TestName ./internal/allocator/...
```

For tests requiring Netbox integration, set `NETBOX_BASE_URL` and `NETBOX_USER_TOKEN` environment variables.

## Architecture

```
┌──────────────────┐         ┌─────────────────────┐
│ Allocator        │         │ LBNodeAgent (per node)│
│ - IP allocation  │         │ - netlink config    │
│ - Pool mgmt      │         │ - Election leader   │
└────────┬─────────┘         └──────────┬──────────┘
         │                              │
         └──────────┬───────────────────┘
                    │
          K8s API Server
                    │
    ┌───────────────┴───────────────┐
    │ CRDs: ServiceGroup, LBNodeAgent│
    └───────────────────────────────┘
```

## Key Internal Packages

- `internal/allocator/` - IP pool management and service allocation logic. Supports LocalPool (in-memory) and NetboxPool (external IPAM)
- `internal/local/` - Linux networking via netlink (interfaces, routes, ARP/NDP). Contains `LocalAnnouncer` implementation
- `internal/k8s/` - Kubernetes client integration using informers and work queues. The `Client` struct watches Services/Endpoints and invokes callbacks on changes
- `internal/election/` - Memberlist-based leader election for node agents. Uses SHA256 hash of (node name + service key) to deterministically elect a winner per service address

## Custom Resources

Defined in `pkg/apis/purelb/v1/`:

- **ServiceGroup**: Defines IP pools (local CIDR ranges or Netbox references), supports dual-stack IPv4/IPv6
- **LBNodeAgent**: Node-specific announcement configuration (interface selection, gratuitous ARP)

Key annotations in `pkg/apis/purelb/v1/annotations.go`:
- `purelb.io/service-group` - User sets to request allocation from specific pool
- `purelb.io/addresses` - User sets to request specific IP address(es)
- `purelb.io/allow-shared-ip` - User sets to enable IP sharing between services
- `purelb.io/allocated-by` - PureLB sets to mark services it manages
- `purelb.io/allocated-from` - PureLB sets to indicate source pool

## Code Generation

When modifying types in `pkg/apis/purelb/v1/`:

1. Run `make generate` to update client code in `pkg/generated/`
2. Run `make crd` to update CRD manifests in `deployments/crds/`

Generated code uses k8s.io/code-generator and controller-tools.

## Key Interfaces

- **Pool interface** (`internal/allocator/pool.go`): Both LocalPool and NetboxPool implement `Notify`, `Assign`, `AssignNext`, `Release`, `Contains`, `Overlaps`
- **Announcer interface** (`internal/lbnodeagent/announcer.go`): Abstract announcement strategy with `SetBalancer`, `DeleteBalancer`, `Shutdown`. Currently implemented by `LocalAnnouncer` in `internal/local/`

## Data Flow

1. User creates LoadBalancer Service
2. Allocator watches Service via k8s informer, allocates IP from configured ServiceGroup pool
3. Allocator updates Service status with allocated IP and sets `purelb.io/allocated-by` annotation
4. LBNodeAgents watch Service, each runs leader election for the service address
5. Winning node configures local networking (adds IP to interface, optionally sends GARP)

## Testing

Tests use testify assertions. Run with `make check` or directly:
```bash
go test -race -short ./...
```

Mock implementations exist in `internal/netbox/fake/` for Netbox testing.

## Known Issues

### IPv6 Local Interface Detection Bug
**File:** `internal/local/network.go:108`

The `checkLocal()` function filters IPv6 addresses with `addrs.Flags < 256`, which excludes addresses with `IFA_F_MANAGETEMPADDR` (0x100) flag. Modern Linux systems with SLAAC commonly have this flag on their primary IPv6 addresses, causing local IPv6 addresses to be incorrectly announced on kube-lb0 instead of the primary NIC.

**Impact:** IPv6 addresses from a "local" pool (same subnet as node) are placed on the dummy interface instead of the real network interface.

**To be addressed:** During the updated allocation process work.
