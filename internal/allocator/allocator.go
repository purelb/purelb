// Copyright 2017 Google Inc.
// Copyright 2020-2026 Acnodal Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package allocator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

const (
	defaultPoolName string = "default"

	// defaultPoolTimeout bounds a single Pool mutating operation when no
	// API-context source is wired (unit tests). Matches the standard PureLB
	// API timeout so a sidecar gets the same grace as the apiserver.
	defaultPoolTimeout = 10 * time.Second
)

// ActiveSubnetsFunc is a function that returns the set of subnets with
// active lbnodeagents. Used for multi-pool allocation to avoid allocating
// IPs on subnets with no healthy nodes.
type ActiveSubnetsFunc func(namespace string) ([]string, error)

// ListServicesFunc returns all services from the informer cache.
// Used to populate pool state from existing allocations.
type ListServicesFunc func() []v1.Service

// allocatorState holds the pool + ServiceGroup maps that are atomically
// swapped by SetPools (G2) and loaded per-cycle by SetBalancer/DeleteBalancer
// (G1). This ensures G1 always sees a consistent pool snapshot within one
// processing cycle, even if G2 swaps mid-cycle.
//
// sgByName is parallel to pools (same keys, same lifetime) and holds
// DeepCopies of the ServiceGroup objects from the informer cache.
// DeepCopying is mandatory — informer-cache objects must never be
// mutated per the client-go contract. The status writer uses these only
// for identity (name/namespace); status writes are server-side applies
// and carry no ResourceVersion, so nothing here goes stale.
type allocatorState struct {
	pools    map[string]Pool
	sgByName map[string]*purelbv2.ServiceGroup

	// nsToPools maps a Service namespace to the ServiceGroups serving it.
	// Absent means the namespace is bound by nothing and falls back to the
	// pool named "default", exactly as before this feature existed.
	nsToPools map[string]nsBinding

	// confinedGroups holds every ServiceGroup that serves at least one
	// confined namespace. Exclusion is derived from this rather than from a
	// group's own EnforceNamespaces: fencing a tenant's L2 pool while
	// leaving its BGP pool reachable cluster-wide would be a half-open
	// fence, so enforcing on any one of a namespace's groups fences all of
	// them.
	confinedGroups map[string]bool
}

// nsBinding is what a namespace resolves to. eligible is a set, because
// several ServiceGroups serving one namespace is normal -- a ServiceGroup is
// exactly one of local, remote or external, so an L2 + BGP tenant needs two.
// Only def needs a single pick.
type nsBinding struct {
	eligible []string // every ServiceGroup serving this namespace, sorted
	def      string   // the unannotated default; "" when unresolvable
	enforce  bool     // any eligible ServiceGroup sets EnforceNamespaces
	defErr   string   // why def is empty, verbatim into the Service event
}

// An Allocator tracks IP address pools and allocates addresses from them.
type Allocator struct {
	client          k8s.ServiceEvent
	sgStatusClient  k8s.ServiceGroupStatusWriter // for status subresource writes
	logger          log.Logger
	state           atomic.Pointer[allocatorState]
	populated       atomic.Bool
	activeSubnets   ActiveSubnetsFunc
	purelbNamespace string
	listServices    ListServicesFunc

	// lastWritten caches the most recently successfully-written status per
	// ServiceGroup name. Used by writeSGStatus to elide no-op API writes
	// when the computed status equals what was last sent. Written only by
	// G1 (queue worker); G2 deletes entries for pools that config removed
	// (SetPools), which is why this is a sync.Map rather than a plain map.
	lastWritten sync.Map // map[svcGroupName]purelbv2.ServiceGroupStatus

	// sidecarConns holds one shared *grpc.ClientConn per external-IPAM
	// sidecar socket. Multiple ServiceGroups pointing at the same socket
	// share a connection. Connections live until allocator shutdown
	// (closeAllSidecarConns); gRPC handles reconnection transparently.
	sidecarConns sync.Map // map[socket string]*grpc.ClientConn
}

// New returns an Allocator managing no pools.
func New(log log.Logger) *Allocator {
	a := &Allocator{
		logger: log,
	}
	a.state.Store(&allocatorState{
		pools:    map[string]Pool{},
		sgByName: map[string]*purelbv2.ServiceGroup{},
	})
	return a
}

// SetServiceGroupStatusWriter wires the ServiceGroup status-subresource
// client used by maybeUpdateSGStatus. If unset, status writes are skipped
// silently (allocator still runs; SG status fields stay empty).
func (a *Allocator) SetServiceGroupStatusWriter(w k8s.ServiceGroupStatusWriter) {
	a.sgStatusClient = w
}

// Pools returns the current pool snapshot. Retained for the release paths,
// which are namespace-blind by design and must not see the binding index.
// Nil-safe for startup before first config arrives.
func (a *Allocator) Pools() map[string]Pool {
	return a.Snapshot().pools
}

// Snapshot returns the whole config snapshot -- pools, ServiceGroups and the
// namespace binding index. Call once per processing cycle and pass the result
// to every allocator method in that cycle, so that a Service is never
// authorized against one generation's bindings and allocated from another's.
//
// Never returns nil, because callers dereference it on the allocation path.
func (a *Allocator) Snapshot() *allocatorState {
	if s := a.state.Load(); s != nil {
		return s
	}
	return &allocatorState{}
}

// SetClient sets this Allocator's client field.
func (a *Allocator) SetClient(client k8s.ServiceEvent) {
	a.client = client
}

// SetActiveSubnets sets the function used to query active subnets
// for multi-pool allocation.
func (a *Allocator) SetActiveSubnets(fn ActiveSubnetsFunc, namespace string) {
	a.activeSubnets = fn
	a.purelbNamespace = namespace
}

// SetListServices sets the function used to list services from the
// informer cache, used by SetPools to populate pool state.
func (a *Allocator) SetListServices(fn ListServicesFunc) {
	a.listServices = fn
}

// SetPools updates the set of address pools that the allocator owns.
// Called from G2 (CR controller goroutine). The new pools are atomically
// swapped in so G1 readers always see a consistent snapshot.
func (a *Allocator) SetPools(groups []*purelbv2.ServiceGroup) error {
	pools := a.parseGroups(groups)

	// If we have groups but they're all bogus then let the user know.
	if len(groups) > 0 && len(pools) == 0 {
		return fmt.Errorf("No valid pools found")
	}

	// Build sgByName parallel to pools. DeepCopy each SG — informer-cache
	// objects must never be mutated (client-go contract), and the status
	// writer holds onto these pointers across the atomic swap.
	sgByName := make(map[string]*purelbv2.ServiceGroup, len(pools))
	for _, g := range groups {
		if _, kept := pools[g.Name]; !kept {
			continue // group failed parse; not in pools map
		}
		sgByName[g.Name] = g.DeepCopy()
	}

	// Clean up metrics + lastWritten cache for removed pools
	oldState := a.state.Load()
	if oldState != nil {
		for n := range oldState.pools {
			if pools[n] == nil {
				poolCapacity.DeleteLabelValues(n)
				poolActive.DeleteLabelValues(n)
				a.lastWritten.Delete(n)
			}
		}
	}

	nsToPools, confinedGroups := a.buildNSIndex(groups, pools)

	// Atomic swap — after this, G1's next Pools() call sees the new map.
	// Everything derived from this config is published together so a reader
	// can never mix a binding index from one generation with pools from
	// another.
	a.state.Store(&allocatorState{
		pools:          pools,
		sgByName:       sgByName,
		nsToPools:      nsToPools,
		confinedGroups: confinedGroups,
	})
	a.populated.Store(false)

	// Set capacity for new pools (size is static, safe from G2).
	// InUse metrics will be set by G1 via ForceSync → updateStats.
	for _, p := range pools {
		poolCapacity.WithLabelValues(p.String()).Set(float64(p.Size()))
	}

	return nil
}

// buildNSIndex resolves, once per config change, which ServiceGroups serve
// each namespace, which of them is the unannotated default, and whether the
// namespace is confined. Runs on G2 from SetPools and is published by the
// same atomic swap as pools, so an index can never be paired with a
// different generation's pools.
//
// Only ServiceGroups that survived parseGroups contribute: a group whose
// spec is broken has no pool, and binding a namespace to it would deny every
// Service in that namespace on the strength of a YAML error elsewhere.
func (a *Allocator) buildNSIndex(groups []*purelbv2.ServiceGroup, pools map[string]Pool) (map[string]nsBinding, map[string]bool) {
	idx := map[string]nsBinding{}
	marked := map[string][]string{}

	for _, g := range groups {
		if _, parsed := pools[g.Name]; !parsed {
			continue
		}
		// Dedupe: +listType=set is deliberately not used (under server-side
		// apply it gives per-element ownership, so an entry deleted from Git
		// would silently persist). The API server therefore accepts
		// `namespaces: [a, a]`, and without this a single ServiceGroup with a
		// copy-pasted entry would appear twice in eligible, look like two
		// groups competing, and deny its own namespace.
		for _, ns := range slices.Compact(slices.Sorted(slices.Values(g.Spec.Namespaces))) {
			b := idx[ns]
			b.eligible = append(b.eligible, g.Name)
			b.enforce = b.enforce || g.Spec.EnforceNamespaces
			idx[ns] = b
			if g.Spec.NamespaceDefault {
				marked[ns] = append(marked[ns], g.Name)
			}
		}
	}

	for ns, b := range idx {
		// Sorted for stable event text and inspect output only; it never
		// selects anything, so the build stays order-independent.
		slices.Sort(b.eligible)
		m := marked[ns]
		slices.Sort(m)
		switch {
		case len(b.eligible) == 1:
			b.def = b.eligible[0]
		case len(m) == 1:
			b.def = m[0]
		case len(m) == 0:
			b.defErr = fmt.Sprintf("namespace %q is served by %v and none sets namespaceDefault; exactly one must", ns, b.eligible)
		default:
			b.defErr = fmt.Sprintf("namespace %q: %v all set namespaceDefault; exactly one must", ns, m)
		}
		idx[ns] = b
	}

	// Exclusion is derived from namespace confinement, not from a group's own
	// EnforceNamespaces. Reading the target's own flag would fence a tenant's
	// L2 pool while leaving its BGP pool reachable from every namespace in
	// the cluster.
	confined := map[string]bool{}
	for _, b := range idx {
		if !b.enforce {
			continue
		}
		for _, name := range b.eligible {
			confined[name] = true
		}
	}

	if len(idx) == 0 {
		return nil, nil
	}
	return idx, confined
}

// populateFromExisting scans the service cache and registers all
// existing PureLB-allocated IPs with the appropriate pools. Runs once
// after the informer cache is warm, then skips subsequent calls since
// individual services are notified via SetBalancer → NotifyExisting.
func (a *Allocator) populateFromExisting(pools map[string]Pool) {
	if a.populated.Load() || a.listServices == nil {
		return
	}
	count := 0
	for _, svc := range a.listServices() {
		if svc.Annotations[purelbv2.BrandAnnotation] != purelbv2.Brand {
			continue
		}
		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			continue
		}
		if err := a.NotifyExisting(pools, &svc); err != nil {
			logging.Info(a.logger, "op", "populateFromExisting", "error", err,
				"svc", svc.Namespace+"/"+svc.Name)
			continue
		}
		count++
	}
	a.populated.Store(true)
	logging.Info(a.logger, "op", "populateFromExisting", "count", count)
}

// updateStats unconditionally updates internal state to reflect svc's
// allocation of alloc. Caller must ensure that this call is safe.
func (a *Allocator) updateStats(pool Pool) {
	poolCapacity.WithLabelValues(pool.String()).Set(float64(pool.Size()))
	poolActive.WithLabelValues(pool.String()).Set(float64(pool.InUse()))
	a.maybeUpdateSGStatus(pool)
}

// maybeUpdateSGStatus writes the ServiceGroup's .status to reflect the
// pool's current state. Synchronous (runs from G1, the allocation
// hot-path). The lastWritten cache elides API calls when the computed
// status is unchanged — at steady state most updateStats invocations
// are no-ops. Errors are logged + counted, never block allocation.
//
// Skipped silently if no sgStatusClient is wired (e.g., in unit tests).
func (a *Allocator) maybeUpdateSGStatus(pool Pool) {
	if a.sgStatusClient == nil {
		return
	}
	s := a.state.Load()
	if s == nil {
		return
	}
	sg, ok := s.sgByName[pool.String()]
	if !ok || sg == nil {
		return
	}

	ctx, cancel := a.sgStatusClient.APIContext()
	defer cancel()

	// refresh=true: this call follows an allocation or release from
	// this pool, so a sidecar's Stats are known to be out of date.
	if _, err := a.writeSGStatus(ctx, sg, pool, true); err != nil {
		if !a.sgStatusClient.IsShutdownError(err) {
			logging.Info(a.logger, "op", "updateSGStatus", "error", err, "sg", sg.Name)
		}
	}
}

// PublishAllSGStatus writes .status for every configured pool,
// including pools that have no allocations. This is what makes a
// freshly-created ServiceGroup show its announce method, IPAM source,
// addresses and capacity immediately, rather than staying blank until
// the first Service happens to allocate from it.
//
// Runs from G1 (the work queue goroutine) via the poolStatus queue
// item, so it shares the allocation hot path's single-writer
// discipline. Best-effort: it returns an aggregate error for logging,
// but the caller must not requeue on it (see the poolStatus case in
// k8s.Client.sync for why).
//
// ctx bounds the whole sweep, not each ServiceGroup, so one queue item
// stays bounded at the standard API timeout however many pools are
// configured. Pools not reached before the deadline are picked up by
// the next publish.
func (a *Allocator) PublishAllSGStatus(ctx context.Context) error {
	if a.sgStatusClient == nil {
		return nil
	}
	s := a.state.Load()
	if s == nil {
		return nil
	}

	// Register the IPs of Services that already exist before computing
	// counts. A publish is normally triggered by a config change, which
	// can land before the work queue has processed the Service backlog
	// (and, at startup, before the controller is even marked synced, so
	// SetBalancer refuses to allocate). Without this the sweep would
	// report every pool as empty and lastWritten would cache that as
	// authoritative. populateFromExisting reads the Service informer
	// cache directly, so it does not depend on either.
	a.populateFromExisting(s.pools)

	var (
		errs    []error
		written int
		elided  int
	)
	for name, pool := range s.pools {
		sg, ok := s.sgByName[name]
		if !ok || sg == nil {
			continue
		}

		// Only pay for a sidecar Stats RPC when we have nothing cached.
		// A sweep runs on every config change and every Service
		// deletion; refreshing unconditionally would put one blocking
		// gRPC per external-IPAM pool on the allocation goroutine each
		// time. A cold cache means a restart or a fresh config, which
		// is exactly when a sidecar pool needs its first read — and
		// because SidecarPool.Contains reports false, NotifyExisting
		// never reaches updateStats for these pools, so this is their
		// only refresh path at startup.
		refresh := false
		if sp, isSidecar := pool.(*SidecarPool); isSidecar {
			refresh = sp.stats.Load() == nil
		}

		wrote, err := a.writeSGStatus(ctx, sg, pool, refresh)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			if !a.sgStatusClient.IsShutdownError(err) {
				logging.Info(a.logger, "op", "publishPoolStatus", "error", err, "sg", name)
			}
		case wrote:
			written++
		default:
			elided++
		}
	}

	logging.Debug(a.logger, "op", "publishPoolStatus", "pools", len(s.pools),
		"written", written, "unchanged", elided, "errors", len(errs))
	return errors.Join(errs...)
}

// writeSGStatus computes pool's status and writes it to sg. Reports
// whether an API write was actually issued — false when the lastWritten
// cache elided it as a no-op.
//
// refresh asks an external-IPAM sidecar for fresh Stats before the
// status is computed. Callers on the allocation path pass true (the
// allocation just changed the numbers); the bulk publish passes true
// only for a cold cache, to keep a blocking RPC off the hot path.
//
// Caller must hold a non-nil sgStatusClient and a live sg.
func (a *Allocator) writeSGStatus(ctx context.Context, sg *purelbv2.ServiceGroup, pool Pool, refresh bool) (bool, error) {
	// Pure display accessors read the Stats cache; this is the only
	// place an RPC fires for status. Best-effort — on failure we
	// proceed with the last-good cache (the interceptor records the
	// RPC error metric).
	if sp, isSidecar := pool.(*SidecarPool); isSidecar && refresh {
		if err := sp.refreshStats(ctx); err != nil {
			logging.Info(a.logger, "op", "refreshSidecarStats", "error", err, "sg", sg.Name)
		}
	}

	newStatus := buildStatus(pool)
	// Set here rather than in buildStatus, which is a pure Pool function with
	// no ServiceGroup in scope. Copied from the spec slice, never assembled
	// from a map: statusEqual compares element-wise, so a varying order would
	// defeat the elision cache and produce one apply per ServiceGroup on every
	// sweep, forever.
	newStatus.BoundNamespaces = slices.Clone(sg.Spec.Namespaces)

	if prev, loaded := a.lastWritten.Load(sg.Name); loaded {
		if statusEqual(prev.(purelbv2.ServiceGroupStatus), newStatus) {
			return false, nil // no-op
		}
	}

	sgCopy := sg.DeepCopy()
	sgCopy.Status = newStatus
	if err := a.sgStatusClient.UpdateServiceGroupStatus(ctx, sgCopy); err != nil {
		sgStatusWritesTotal.WithLabelValues(classifyStatusErr(err)).Inc()
		// Do NOT update lastWritten — the next publish retries.
		return false, err
	}
	sgStatusWritesTotal.WithLabelValues("success").Inc()
	a.lastWritten.Store(sg.Name, newStatus)
	logging.Info(a.logger, "op", "updateSGStatus", "sg", sg.Name,
		"announce", newStatus.Announce, "ipam", newStatus.IPAM,
		"allocatedV4", newStatus.AllocatedIPv4, "allocatedV6", newStatus.AllocatedIPv6)
	return true, nil
}

// buildStatus computes the ServiceGroupStatus payload for a pool. Pure
// reads from the Pool interface; no I/O. For SidecarPool (Part C),
// callers must refresh the Stats cache before calling buildStatus.
func buildStatus(pool Pool) purelbv2.ServiceGroupStatus {
	s := purelbv2.ServiceGroupStatus{
		Announce:      capitalize(pool.PoolType()),
		IPAM:          pool.IPAMSource(),
		Addresses:     pool.DisplayAddresses(),
		AllocatedIPv4: int64(pool.InUseV4()),
		AllocatedIPv6: int64(pool.InUseV6()),
	}
	if pool.HasKnownCapacity() {
		availV4 := int64(pool.SizeV4()) - s.AllocatedIPv4
		availV6 := int64(pool.SizeV6()) - s.AllocatedIPv6
		s.AvailableIPv4, s.AvailableIPv6 = &availV4, &availV6
	}
	return s
}

// capitalize returns s with the first byte upper-cased. Pool.PoolType()
// returns "local" / "remote" by convention; status display wants
// "Local" / "Remote" to match user-facing capitalization.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}

// statusEqual reports whether two ServiceGroupStatus values are
// semantically equal. Hand-written instead of reflect.DeepEqual so
// *int64 fields are compared by dereferenced value, not by pointer
// identity (two separate &int64 allocations holding the same value
// must compare equal so the lastWritten cache elides redundant writes).
func statusEqual(a, b purelbv2.ServiceGroupStatus) bool {
	if a.Announce != b.Announce ||
		a.IPAM != b.IPAM ||
		a.AllocatedIPv4 != b.AllocatedIPv4 ||
		a.AllocatedIPv6 != b.AllocatedIPv6 {
		return false
	}
	if !int64PtrEqual(a.AvailableIPv4, b.AvailableIPv4) ||
		!int64PtrEqual(a.AvailableIPv6, b.AvailableIPv6) {
		return false
	}
	if len(a.Addresses) != len(b.Addresses) {
		return false
	}
	for i := range a.Addresses {
		if a.Addresses[i] != b.Addresses[i] {
			return false
		}
	}
	// This list is hand-maintained, so a new status field that is not
	// compared here is written once and then frozen: the elision cache would
	// report the status unchanged forever while the allocator acted on a
	// newer spec. Editing spec.namespaces changes nothing else, so without
	// this the confirmation signal would show the old list indefinitely.
	if !slices.Equal(a.BoundNamespaces, b.BoundNamespaces) {
		return false
	}
	return true
}

// int64PtrEqual returns true when a and b point to equal values or are
// both nil. Distinct from `a == b` which compares pointer identity.
func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// classifyStatusErr categorises a status-write error for the
// sgStatusWritesTotal counter. Conservative: anything we don't
// recognise lands in "other".
func classifyStatusErr(err error) string {
	if err == nil {
		return "success"
	}
	switch {
	case apierrors.IsConflict(err):
		return "conflict"
	case apierrors.IsForbidden(err):
		return "forbidden"
	default:
		return "other"
	}
}

// NotifyExisting notifies the allocator of existing IP assignments,
// for example, at startup time.
func (a *Allocator) NotifyExisting(pools map[string]Pool, svc *v1.Service) error {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		// validate the allocated address
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			logging.Info(a.logger, "op", "setBalancer", "error", "invalid LoadBalancer IP", "ip", ingress.IP)
			continue
		}

		// Find the pool which contains the address
		pool := poolFor(pools, lbIP)
		if pool == nil {
			logging.Info(a.logger, "op", "setBalancer", "error", "unknown LoadBalancer IP: no pool found", "ip", ingress.IP)
			continue
		}

		// Tell the pool about the assignment
		ctx, cancel := a.poolContext()
		err := pool.Notify(ctx, svc)
		cancel()
		if err != nil {
			return err
		}
		a.updateStats(pool)
	}
	return nil
}

// Allocate allocates an IP address for svc based on svc's
// annotations and current configuration. If the user asks for a
// specific IP then we'll attempt to use that, and if not we'll use
// the pool specified in the purelbv2.DesiredGroupAnnotation
// annotation. If neither is specified then we will attempt to
// allocate from a pool named "default", if it exists.
func (a *Allocator) Allocate(st *allocatorState, svc *v1.Service) error {
	pools := st.pools

	// Ensure pool state reflects all existing allocations before
	// assigning a new IP. This is called here (not in SetPools) because
	// SetPools runs on the CR controller goroutine where the service
	// cache may not be synced yet. Allocate only runs from SetBalancer
	// on the main queue thread, after WaitForCacheSync, so the cache
	// is guaranteed warm.
	a.populateFromExisting(pools)

	// If the user asked for a specific IP, allocate that.
	allocated, err := a.allocateSpecificIP(st, svc)
	if err != nil {
		return err
	}

	// The user didn't ask for a specific IP so we can allocate one from
	// a pool.
	if !allocated {
		pool, via, err := st.resolve(svc)
		if err != nil {
			return err
		}
		if via == viaAmbiguous {
			// Falling back to "default" because the namespace could not
			// resolve one of its own. Nothing is denied, so without this the
			// operator who most needs namespaceDefault -- the L2 + BGP tenant
			// -- gets silence and Services quietly landing on another subnet.
			a.client.Errorf(svc, "ConfigurationWarning", "%s; allocated from %q instead",
				st.nsToPools[svc.Namespace].defErr, defaultPoolName)
		}

		// Multi-pool and balancePools are mutually exclusive
		if isMultiPool(svc, pool) && pool.BalancePools() {
			return fmt.Errorf("multi-pool and balancePools allocation are mutually exclusive for pool %s", pool)
		}

		// Check if this should be a multi-pool allocation
		if isMultiPool(svc, pool) {
			if err = a.allocateMultiPool(pools, svc, pool); err != nil {
				return err
			}
		} else if err = a.allocateFromPool(pools, svc, pool); err != nil {
			return err
		}
	}

	return nil
}

// allocateSpecificIP assigns the requested ip to svc, if the
// assignment is permissible by sharingKey. If the user didn't ask for
// a specific address then the return values will be ("", nil). If an
// address was allocated then the string return value will be
// non-"". If an error happened then the error return will be non-nil.
func (a *Allocator) allocateSpecificIP(st *allocatorState, svc *v1.Service) (bool, error) {
	pools := st.pools
	poolNames := ""
	var firstPool Pool // Track first pool for annotations

	// See if the user configured a specific address and return if not.
	ips, err := a.serviceAddresses(svc)
	if err != nil {
		return false, err
	}
	if len(ips) == 0 { // no user-configured address
		return false, nil
	}

	// Warn if the user provided the group annotation - the IP
	// annotation overrides it.
	if _, exists := svc.Annotations[purelbv2.DesiredGroupAnnotation]; exists {
		a.client.Infof(svc, "ConfigurationWarning", "Both the addresses annotation and the service-group annotation were provided. service-group will be ignored.")
		logging.Info(a.logger, "op", "allocateSpecificIP", "warning", "addresses annotation overrides service-group annotation")
	}

	// Resolve and authorize every requested address BEFORE releasing
	// anything. The Unassign below frees this Service's current addresses in
	// pool accounting while .status still advertises them, so returning an
	// error from inside the assign loop leaves a double-allocation window.
	// Pinning a neighbour's address is a routine misconfiguration, not a rare
	// one, so it must not reach that loop.
	resolved := make([]Pool, len(ips))
	for i, ip := range ips {
		pool := poolFor(pools, ip)
		if pool == nil {
			return false, fmt.Errorf("%q does not belong to any group", ip)
		}
		// Same gate as the annotation path: an address is a way of naming a
		// ServiceGroup, so pinning must not bypass the boundary that naming
		// it would hit.
		if _, err := st.checked(svc.Namespace, pool.String()); err != nil {
			return false, fmt.Errorf("%s: %w", ip, err)
		}
		resolved[i] = pool
	}

	// If the service had addresses before, release them.
	if err := a.Unassign(pools, namespacedName(svc)); err != nil {
		return false, err
	}

	for i, ip := range ips {
		pool := resolved[i]

		// Track the first pool for setting annotations
		if firstPool == nil {
			firstPool = pool
		}

		// Does the IP already have allocs? If so, needs to be the same
		// sharing key, and have non-overlapping ports. If not, the proposed
		// IP needs to be allowed by configuration.
		ctx, cancel := a.poolContext()
		err := pool.Assign(ctx, ip, svc)
		cancel()
		if err != nil {
			return false, err
		}

		a.updateStats(pool)

		// annotate the pool from which the address came
		a.client.Infof(svc, "AddressAssigned", "Assigned %+v from pool %s", svc.Status.LoadBalancer, pool)
		if poolNames == "" {
			poolNames = pool.String()
		} else {
			poolNames = poolNames + ", " + pool.String()
		}
	}

	svc.Annotations[purelbv2.PoolAnnotation] = poolNames

	// Set pool type annotation based on the first pool
	// (in practice, specific IPs should come from pools of the same type)
	if firstPool != nil {
		svc.Annotations[purelbv2.PoolTypeAnnotation] = firstPool.PoolType()

		// Set skip-ipv6-dad annotation if the pool has it enabled
		if firstPool.SkipIPv6DAD() {
			svc.Annotations[purelbv2.SkipIPv6DADAnnotation] = "true"
		} else {
			delete(svc.Annotations, purelbv2.SkipIPv6DADAnnotation)
		}
	}

	return true, nil
}

// AllocateFromPool assigns an available IP from pool to service.
func (a *Allocator) allocateFromPool(pools map[string]Pool, svc *v1.Service, pool Pool) error {
	// Only release existing IPs if service has no ingress addresses.
	// If it already has addresses, this might be an IP family transition
	// (e.g., SingleStack → DualStack) where we want to keep existing IPs
	// and only allocate missing families.
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		if err := a.Unassign(pools, namespacedName(svc)); err != nil {
			return err
		}
	}

	// AssignNext commits each family as it goes (assignFamily -> Assign ->
	// addIngress + Notify) and returns on the first failure without
	// unwinding. Left alone, a dual-stack Service against a single-family
	// pool keeps the family that succeeded in .status -- which SetBalancer
	// persists and the node agent announces, because it does not check the
	// brand -- while BrandAnnotation is never set, since that happens only
	// after a successful allocation. populateFromExisting then skips the
	// Service, so after an allocator restart the address is handed out
	// again: two Services, one VIP.
	//
	// Roll back only what this call added. Pre-existing addresses from an
	// IP-family transition must survive, so this cannot be Unassign.
	committed := len(svc.Status.LoadBalancer.Ingress)
	ctx, cancel := a.poolContext()
	err := pool.AssignNext(ctx, svc)
	cancel()
	if err != nil {
		a.rollbackIngress(pool, svc, committed)
		return err
	}

	// annotate the pool from which the address came
	a.client.Infof(svc, "AddressAssigned", "Assigned %+v from pool %s", svc.Status.LoadBalancer, pool)
	svc.Annotations[purelbv2.PoolAnnotation] = pool.String()

	// Set pool type annotation so lbnodeagent knows which interface to use
	svc.Annotations[purelbv2.PoolTypeAnnotation] = pool.PoolType()

	// Set skip-ipv6-dad annotation if the pool has it enabled
	if pool.SkipIPv6DAD() {
		svc.Annotations[purelbv2.SkipIPv6DADAnnotation] = "true"
	} else {
		// Remove the annotation if it was previously set
		delete(svc.Annotations, purelbv2.SkipIPv6DADAnnotation)
	}

	a.updateStats(pool)

	return nil
}

// rollbackIngress releases every address added to svc beyond keep and
// truncates .status back to that length, undoing a partial allocation.
//
// Truncating by index is safe because addIngress only ever appends and
// nothing between the snapshot and here reorders the slice.
func (a *Allocator) rollbackIngress(pool Pool, svc *v1.Service, keep int) {
	ingress := svc.Status.LoadBalancer.Ingress
	if keep >= len(ingress) {
		return
	}
	for _, added := range ingress[keep:] {
		ip := net.ParseIP(added.IP)
		if ip == nil {
			continue
		}
		ctx, cancel := a.poolContext()
		err := pool.ReleaseIP(ctx, namespacedName(svc), ip)
		cancel()
		if err != nil {
			logging.Info(a.logger, "op", "rollbackIngress", "error", err,
				"svc", namespacedName(svc), "ip", added.IP)
		}
	}
	logging.Info(a.logger, "op", "rollbackIngress", "svc", namespacedName(svc),
		"released", len(ingress)-keep, "msg", "partial allocation rolled back")
	svc.Status.LoadBalancer.Ingress = ingress[:keep]
}

// isMultiPool determines whether a service should use multi-pool allocation.
// The service annotation overrides the pool's default setting.
func isMultiPool(svc *v1.Service, pool Pool) bool {
	if ann, ok := svc.Annotations[purelbv2.MultiPoolAnnotation]; ok {
		return ann == "true"
	}
	return pool.MultiPool()
}

// allocateMultiPool performs multi-pool allocation: one IP per range
// per family from ranges with active nodes.
func (a *Allocator) allocateMultiPool(pools map[string]Pool, svc *v1.Service, pool Pool) error {
	// Multi-pool and IP sharing are mutually exclusive
	if SharingKey(svc) != "" {
		return fmt.Errorf("multi-pool allocation cannot be combined with IP sharing (annotation %s)", purelbv2.SharingAnnotation)
	}

	if a.activeSubnets == nil {
		return fmt.Errorf("multi-pool allocation requires ActiveSubnets to be configured")
	}

	// Release any existing allocations for this service
	if err := a.Unassign(pools, namespacedName(svc)); err != nil {
		return err
	}
	svc.Status.LoadBalancer.Ingress = nil

	// Get the current set of subnets with active nodes
	activeSubnets, err := a.activeSubnets(a.purelbNamespace)
	if err != nil {
		return fmt.Errorf("querying active subnets: %w", err)
	}

	logging.Info(a.logger, "op", "allocateMultiPool", "activeSubnets", strings.Join(activeSubnets, ","),
		"svc", namespacedName(svc))

	ctx, cancel := a.poolContext()
	err = pool.AssignNextPerRange(ctx, svc, activeSubnets)
	cancel()
	if err != nil {
		return err
	}

	// Set annotations
	a.client.Infof(svc, "AddressAssigned", "Multi-pool: assigned %d IPs from pool %s", len(svc.Status.LoadBalancer.Ingress), pool)
	svc.Annotations[purelbv2.PoolAnnotation] = pool.String()
	svc.Annotations[purelbv2.PoolTypeAnnotation] = pool.PoolType()

	if pool.SkipIPv6DAD() {
		svc.Annotations[purelbv2.SkipIPv6DADAnnotation] = "true"
	} else {
		delete(svc.Annotations, purelbv2.SkipIPv6DADAnnotation)
	}

	// Update metrics
	multipoolAllocations.WithLabelValues(pool.String()).Inc()
	a.updateStats(pool)

	return nil
}

// poolHoldingAll returns the pool containing every one of svc's ingress
// addresses. It answers the identity question — which pool does this Service
// actually hold — from the addresses themselves rather than from
// purelb.io/allocated-from, which lives on the Service and is therefore
// editable by whoever owns it.
//
// SidecarPool.Contains is hardcoded false, so an external pool can never be
// resolved this way. Callers must reject external pools explicitly; treating
// a nil result as "must be external" would hand a free pass to any address
// that belongs to no configured pool at all.
func poolHoldingAll(pools map[string]Pool, svc *v1.Service) (Pool, error) {
	var held Pool
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ip := net.ParseIP(ingress.IP)
		if ip == nil {
			return nil, fmt.Errorf("unparseable ingress address %q", ingress.IP)
		}
		p := poolFor(pools, ip)
		if p == nil {
			return nil, fmt.Errorf("address %s does not belong to any pool", ingress.IP)
		}
		if held == nil {
			held = p
		} else if held.String() != p.String() {
			return nil, fmt.Errorf("addresses span more than one pool (%s, %s)", held, p)
		}
	}
	if held == nil {
		return nil, fmt.Errorf("service has no ingress addresses")
	}
	return held, nil
}

// IncrementalMultiPool attempts to allocate additional IPs from newly
// available ranges for an existing multi-pool service. It does NOT
// release existing IPs — AssignNextPerRange skips ranges where the
// service already has an IP. Returns true if new IPs were added.
func (a *Allocator) IncrementalMultiPool(st *allocatorState, svc *v1.Service) (bool, error) {
	pools := st.pools
	// Resolve the pool from the addresses the Service holds, not from
	// purelb.io/allocated-from. Trusting the annotation let anyone with
	// patch on their own Service draw addresses from any pool in the
	// cluster. Rewriting the annotation from the result below also
	// repairs it if it had been tampered with.
	pool, err := poolHoldingAll(pools, svc)
	if err != nil {
		// A constant label, never the annotation value: the annotation is
		// user-supplied, so using it here would let a Service mint an
		// unbounded number of Prometheus series.
		allocationRejected.WithLabelValues("<unknown>", "multipool_pool_mismatch").Inc()
		return false, err
	}

	// External pools have no per-range concept: SidecarPool.AssignNextPerRange
	// just calls AssignNext and addIngress never dedupes, so each call would
	// append another address and the resulting Status write would re-enqueue
	// the Service — one new address per work-queue turn, forever.
	// SidecarPool.MultiPool() is false, so the annotation was the only way in.
	// Repair the annotation if it disagrees with the addresses. A mismatch is
	// either tampering or a stale value after a config change; correcting it
	// is safe either way, and leaving it would mislead `kubectl purelb` and
	// any operator inventory built on allocated-from.
	if named := svc.Annotations[purelbv2.PoolAnnotation]; named != pool.String() {
		logging.Info(a.logger, "op", "incrementalMultiPool", "msg", "correcting allocated-from",
			"svc", namespacedName(svc), "claimed", named, "actual", pool.String())
		svc.Annotations[purelbv2.PoolAnnotation] = pool.String()
	}

	if _, external := pool.(*SidecarPool); external {
		allocationRejected.WithLabelValues(pool.String(), "multipool_refused").Inc()
		return false, fmt.Errorf("multi-pool allocation is not supported for external pool %q", pool)
	}

	if a.activeSubnets == nil {
		return false, fmt.Errorf("activeSubnets not configured")
	}

	activeSubnets, err := a.activeSubnets(a.purelbNamespace)
	if err != nil {
		return false, fmt.Errorf("querying active subnets: %w", err)
	}

	existingCount := len(svc.Status.LoadBalancer.Ingress)

	ctx, cancel := a.poolContext()
	err = pool.AssignNextPerRange(ctx, svc, activeSubnets)
	cancel()
	if err != nil {
		return false, err
	}

	newCount := len(svc.Status.LoadBalancer.Ingress)
	if newCount > existingCount {
		added := newCount - existingCount
		a.client.Infof(svc, "AddressAssigned", "Incremental multi-pool: added %d IPs (total %d) from pool %s", added, newCount, pool)
		svc.Annotations[purelbv2.PoolAnnotation] = pool.String()
		svc.Annotations[purelbv2.PoolTypeAnnotation] = pool.PoolType()
		if pool.SkipIPv6DAD() {
			svc.Annotations[purelbv2.SkipIPv6DADAnnotation] = "true"
		} else {
			delete(svc.Annotations, purelbv2.SkipIPv6DADAnnotation)
		}
		multipoolAllocations.WithLabelValues(pool.String()).Inc()
		a.updateStats(pool)
		return true, nil
	}

	return false, nil
}

// poolContext returns a bounded, cancellable context for a Pool mutating
// operation.
//
// Pool methods take a context precisely so that implementations backed by
// network I/O can be bounded; passing context.Background() defeated that.
// The allocator's work queue is single-threaded, so an external IPAM
// sidecar that accepts a connection and never replies would otherwise wedge
// every Service reconcile, status write and release in the cluster.
//
// Prefers the client's API context, which shortens to 500ms during
// shutdown; falls back to a fixed timeout when no client is wired (tests).
// Callers MUST invoke the returned CancelFunc.
func (a *Allocator) poolContext() (context.Context, context.CancelFunc) {
	if a.sgStatusClient != nil {
		return a.sgStatusClient.APIContext()
	}
	return context.WithTimeout(context.Background(), defaultPoolTimeout)
}

// Unassign frees the IP associated with service, if any.
//
// Release failures are returned, not discarded. LocalPool.Release cannot
// fail, so in practice this reports only external-IPAM failures -- exactly
// the case where dropping the error leaks an address in a system PureLB
// does not own. Every pool is still attempted before returning, so one
// unreachable sidecar cannot strand releases in the others.
func (a *Allocator) Unassign(pools map[string]Pool, svc string) error {
	var errs []error

	// tell the pools that the address has been released. there might
	// not be a pool, e.g., in the case of a config change that moves
	// addresses from one pool to another
	for name, p := range pools {
		ctx, cancel := a.poolContext()
		err := p.Release(ctx, svc)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			logging.Info(a.logger, "op", "unassign", "error", err, "pool", name, "svc", svc,
				"msg", "pool failed to release the address")
			continue
		}
		a.updateStats(p) // This pool released the address
	}

	return errors.Join(errs...)
}

// ReleaseIPs releases specific IP addresses for a service. Used during
// IP family transitions (e.g., DualStack → SingleStack) when only some
// addresses need to be released.
func (a *Allocator) ReleaseIPs(pools map[string]Pool, svc string, ips []string, namedPool string) error {
	var errs []error

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			logging.Info(a.logger, "op", "releaseIPs", "error", "invalid IP", "ip", ipStr)
			continue
		}

		// Find which pool contains this IP and release it
		pool := poolFor(pools, ip)
		if pool == nil {
			// SidecarPool.Contains is hardcoded false, so an external pool can
			// never be found by address and this path would silently drop the
			// release -- the address vanishes from the Service while the
			// external IPAM still holds it. Fall back to the pool the Service
			// says it came from, but only when that pool is external: for any
			// other pool poolFor is authoritative, and trusting a user-editable
			// annotation there would let a Service free a neighbour's address.
			if p, ok := pools[namedPool]; ok {
				if _, external := p.(*SidecarPool); external {
					pool = p
				}
			}
		}
		if pool == nil {
			logging.Info(a.logger, "op", "releaseIPs", "warning", "no pool found for IP", "ip", ipStr)
			continue
		}

		ctx, cancel := a.poolContext()
		err := pool.ReleaseIP(ctx, svc, ip)
		cancel()
		if err != nil {
			// Returned, not just logged: for an external pool this is a
			// leaked address in a system PureLB does not own.
			errs = append(errs, fmt.Errorf("%s: %w", ipStr, err))
			logging.Info(a.logger, "op", "releaseIPs", "error", err, "ip", ipStr)
			// Continue releasing other IPs even if one fails
			continue
		}
		a.updateStats(pool)
	}

	return errors.Join(errs...)
}

// poolFor returns the pool that owns the requested IP, or "" if none.
func poolFor(pools map[string]Pool, ip net.IP) Pool {
	for _, p := range pools {
		if p.Contains(ip) {
			return p
		}
	}
	return nil
}

// serviceAddresses returns any IP addresses configured in the provided
// service. There can be 0-2 addresses: the deprecated
// svc.Spec.LoadBalancer field can contain one, and the
// purelbv2.DesiredAddressAnnotation can contain one or two, separated
// by commas.
func (a *Allocator) serviceAddresses(svc *v1.Service) ([]net.IP, error) {
	ips := []net.IP{}

	// Try our annotation first.
	rawAddrs, exists := svc.Annotations[purelbv2.DesiredAddressAnnotation]
	if !exists {
		// There's no DesiredAddressAnnotation so try the (deprecated)
		// LoadBalancerIP field.
		rawAddrs = svc.Spec.LoadBalancerIP
		if rawAddrs == "" {
			return nil, nil
		}

		// Warn the user about the deprecated LoadBalancerIP field
		a.client.Infof(svc, "DeprecationWarning", "Service.Spec.LoadBalancerIP is deprecated, please use the \"%s\" annotation instead", purelbv2.DesiredAddressAnnotation)
		logging.Info(a.logger, "op", "serviceAddresses", "svc", svc.Name, "deprecation", "Service.Spec.LoadBalancerIP is deprecated, use "+purelbv2.DesiredAddressAnnotation+" annotation")
	}

	for _, rawAddr := range strings.Split(rawAddrs, ",") {
		ip := net.ParseIP(rawAddr)
		if ip == nil {
			return nil, fmt.Errorf("invalid user-specified address: \"%q\"", rawAddr)
		}
		ips = append(ips, ip)
	}

	return ips, nil
}

// getOrDialSidecar returns a shared gRPC connection to the sidecar at
// socket, dialing one if it doesn't exist yet. Connections are cached by
// socket path so multiple ServiceGroups pointing at the same socket
// share a connection. grpc.NewClient is lazy — it does not block on a
// reachable sidecar, so a dial "succeeds" even if the sidecar isn't up
// yet; the first RPC surfaces an Unavailable error in that case.
func (a *Allocator) getOrDialSidecar(socket string) (*grpc.ClientConn, error) {
	if v, ok := a.sidecarConns.Load(socket); ok {
		return v.(*grpc.ClientConn), nil
	}
	conn, err := grpc.NewClient(
		"unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(newSidecarInstrumentation(socket)),
	)
	if err != nil {
		return nil, err
	}
	if existing, loaded := a.sidecarConns.LoadOrStore(socket, conn); loaded {
		// Another goroutine dialed concurrently; keep theirs, close ours.
		conn.Close()
		return existing.(*grpc.ClientConn), nil
	}
	return conn, nil
}

// closeAllSidecarConns closes every sidecar connection. Called on
// allocator shutdown.
func (a *Allocator) closeAllSidecarConns() {
	a.sidecarConns.Range(func(_, v interface{}) bool {
		v.(*grpc.ClientConn).Close()
		return true
	})
}

// parseGroups parses a slice of ServiceGroups and returns a map of
// the pools specified by those groups. We try to return any good
// pools so if a pool fails our validation it won't be in the output,
// but other valid pools will be. Therefore there might be fewer pools
// in the output than there are groups in the input.
func (a *Allocator) parseGroups(groups []*purelbv2.ServiceGroup) map[string]Pool {
	pools := map[string]Pool{}

Group:
	for _, group := range groups {
		pool, err := a.parsePool(group.Name, group.Spec)
		if err != nil {
			a.client.Errorf(group, "ParseFailed", "Failed to parse: %s", err)
			logging.Info(a.logger, "op", "parseGroups", "error", "parsing ServiceGroup", "group", group.Name, "msg", err)
			continue Group
		}

		// Check that the pool isn't already defined
		if pools[group.Name] != nil {
			a.client.Errorf(group, "ParseFailed", "Duplicate definition of pool %s", group.Name)
			logging.Info(a.logger, "op", "parseGroups", "error", "duplicate ServiceGroup", "group", group.Name)
			continue Group
		}

		// Check that this pool doesn't overlap with any of the previous
		// ones. Iterate in sorted order: with three or more overlapping
		// ServiceGroups a map walk names a different partner each time, so
		// the event text changes, the recorder cannot aggregate it, and a
		// fresh Event appears on every resync forever.
		for _, name := range slices.Sorted(maps.Keys(pools)) {
			r := pools[name]
			if pool.Overlaps(r) {
				a.client.Errorf(group, "ParseFailed", "Pool overlaps with already defined pool \"%s\"", name)
				logging.Info(a.logger, "op", "parseGroups", "error", "ServiceGroup overlaps", "group", group.Name, "overlaps", name)
				continue Group
			}
		}

		pools[group.Name] = pool
		a.client.Infof(group, "Parsed", "ServiceGroup parsed successfully")
	}

	return pools
}
