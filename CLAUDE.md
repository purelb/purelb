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
make image          # Build container images using ko (see below for deployment)
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

## Building and Deploying to Test Cluster

**IMPORTANT**: The default `make image` builds to `ko.local/` which requires local Docker. For deploying to the test cluster, you must use `ko` directly with the correct registry and tag.

### Check Current Cluster Image Tags

First, check what image tags the cluster is currently using:
```bash
kubectl --context proxmox get daemonset lbnodeagent -n purelb-system-o jsonpath='{.spec.template.spec.containers[0].image}'
# Example output: ghcr.io/purelb/purelb/lbnodeagent:general_k8_update
```

### Build and Push with ko

Use `ko` directly with the correct registry (`ghcr.io/purelb/purelb`) and tag (matching the current branch/deployment):
```bash
# Set the registry and TAG (both required - TAG is used by .ko.yaml for ldflags)
export KO_DOCKER_REPO=ghcr.io/purelb/purelb
export TAG=general_k8_update  # Must match the tag you're deploying

# Build and push with the correct tag (match current cluster deployment)
go run github.com/google/ko@v0.17.1 build --base-import-paths --tags=$TAG ./cmd/lbnodeagent
go run github.com/google/ko@v0.17.1 build --base-import-paths --tags=$TAG ./cmd/allocator
```

### Restart Pods to Pick Up New Images

After pushing new images, restart the pods to pull the updated images:
```bash
kubectl --context proxmox rollout restart daemonset/lbnodeagent -n purelb
kubectl --context proxmox rollout restart deployment/allocator -n purelb

# Wait for rollout to complete
kubectl --context proxmox rollout status daemonset/lbnodeagent -n purelb
kubectl --context proxmox rollout status deployment/allocator -n purelb
```

### Common Mistakes to Avoid

1. **Don't use `make image`** for cluster deployment - it builds to `ko.local/` which requires local Docker daemon
2. **Always check the current image tag** before building - use the same tag the cluster expects
3. **Remember to restart pods** after pushing - Kubernetes won't automatically pull updated images with the same tag

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
- `internal/election/` - Lease-based subnet-aware leader election for node agents. Each node maintains a Kubernetes Lease with its subnets annotated; uses SHA256 hash of (node name + service key) to deterministically elect a winner from nodes that have the IP's subnet

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


## Design Principle: Avoid Locks and Mutexes

**Locks and mutexes should be avoided wherever possible.** They introduce complexity, potential deadlocks, contention, and are difficult to reason about. When concurrent access is needed, prefer these alternatives in order:

### Preferred Approaches (Lock-Free)

1. **Single-goroutine ownership**: Design so only one goroutine accesses mutable state
   - Example: K8s work queue processes items sequentially
   - Example: `svcIngresses` map is only accessed from work queue goroutine

2. **Atomic operations**: Use `atomic.Bool`, `atomic.Int64`, `atomic.Pointer[T]`
   - Example: `addressRenewal.cancelled` uses `atomic.Bool`
   - Example: Election state uses `atomic.Pointer[electionState]` for copy-on-write

3. **sync.Map**: For simple key-value stores with concurrent access
   - Example: `addressRenewals sync.Map` for renewal timer tracking
   - Note: Not ideal for iteration-heavy workloads

4. **Channels**: For coordination between goroutines
   - Example: `stopCh chan struct{}` for shutdown signaling

5. **Immutable data + atomic swap**: Build new state, swap atomically
   - Example: `rebuildMaps()` creates new `electionState`, calls `state.Store(newState)`
   - Readers see old OR new state, never partial updates

### When Locks Might Seem Necessary

If you find yourself reaching for `sync.Mutex` or `sync.RWMutex`, first ask:

1. Can the data be owned by a single goroutine?
2. Can the operation be made atomic?
3. Can the data structure be replaced with atomic pointer swap?
4. Can the coordination be done via channels?

### If a Lock is Truly Required

Document it clearly with:
- Why lock-free alternatives don't work
- What goroutines contend for the lock
- Lock ordering if multiple locks exist
- Consider using `sync.RWMutex` if reads >> writes

**No locks are used in this implementation.** All concurrent access is handled via:
- `atomic.Pointer[electionState]` for election maps
- `atomic.Bool` for renewal cancellation
- `sync.Map` for address renewals

## Election System

The election system (`internal/election/`) determines which node announces each LoadBalancer IP address. It uses Kubernetes Leases for distributed coordination.

### How It Works

1. Each lbnodeagent creates a Lease in its namespace with:
   - Node name as holder identity
   - Local subnets as annotation (`purelb.io/subnets`)
   - Periodic renewal timestamp

2. When determining which node should announce an IP:
   - Find all nodes with valid (non-expired) leases
   - Filter to nodes that have the IP's subnet in their annotations
   - Use deterministic hash of (node name + service key) to pick winner

3. Graceful shutdown:
   - Node marks itself unhealthy (Winner() returns "")
   - ForceSync withdraws all addresses
   - Lease is deleted so other nodes see it gone immediately

### Key Configuration

- `PURELB_LEASE_DURATION` - How long a lease is valid (default: 10s)
- `PURELB_RENEW_DEADLINE` - How long to retry renewals (default: 7s)
- `PURELB_RETRY_PERIOD` - Interval between renewal attempts (default: 2s)

### Monitoring

Election metrics available at `/metrics`:
- `purelb_election_lease_healthy` - Whether this node's lease is valid
- `purelb_election_member_count` - Number of healthy nodes in election
- `purelb_election_lease_renewals_total` - Successful lease renewals

## Logging
Logging must be implemented, two level info and debug.  Info for normal operation, debug for codelevel troubleshooting.