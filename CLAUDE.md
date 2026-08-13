# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Importance of IPv6
Always include development of equal IPv6 functionality in every activity.

## Building and Deploying to Test Cluster

See the `deploy-test-cluster` skill. `make image` builds to `ko.local/` and is
NOT how you deploy to a cluster.

## Code Generation

When modifying types in `pkg/apis/purelb/v1/`:

1. Run `make generate` to update client code in `pkg/generated/`
2. Run `make crd` to update CRD manifests in `deployments/crds/`

Generated code uses k8s.io/code-generator and controller-tools.

## Data Flow

1. User creates LoadBalancer Service
2. Allocator watches Service via k8s informer, allocates IP from configured ServiceGroup pool
3. Allocator updates Service status with allocated IP and sets `purelb.io/allocated-by` annotation
4. LBNodeAgents watch Service, each runs leader election for the service address
5. Winning node configures local networking (adds IP to interface, optionally sends GARP)

## Testing

Tests use testify assertions.

For sidecar IPAM tests, `internal/allocator/sidecarpool_test.go` runs an
in-process fake IPAM gRPC server (bufconn); `cmd/test-sidecar` is a
standalone in-memory sidecar image for cluster E2E tests.

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
