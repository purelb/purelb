// Copyright 2020-2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// nsGroup builds a ServiceGroup with one v4 range and the namespace-binding
// fields under test.
func nsGroup(name, cidr string, namespaces []string, enforce, isDefault bool) *purelbv2.ServiceGroup {
	return &purelbv2.ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: purelbv2.ServiceGroupSpec{
			Namespaces:        namespaces,
			EnforceNamespaces: enforce,
			NamespaceDefault:  isDefault,
			Local: &purelbv2.ServiceGroupLocalSpec{
				V4Pools: []purelbv2.AddressPool{{Pool: cidr, Subnet: "192.168.0.0/16"}},
			},
		},
	}
}

func nsState(t *testing.T, groups ...*purelbv2.ServiceGroup) *Allocator {
	t.Helper()
	alloc := New(allocatorTestLogger)
	alloc.SetClient(&testK8S{t: t})
	require.NoError(t, alloc.SetPools(groups))
	return alloc
}

func svcIn(namespace, name, group string) *v1.Service {
	s := service(name, ports("tcp/80"), "")
	s.Namespace = namespace
	if group != "" {
		s.Annotations[purelbv2.DesiredGroupAnnotation] = group
	}
	return &s
}

// TestResolveFallsBackToDefault is the zero-behaviour-change guard: with no
// spec.namespaces anywhere, every Service resolves exactly as it did before
// this feature existed.
func TestResolveFallsBackToDefault(t *testing.T) {
	alloc := nsState(t, nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false))
	st := alloc.Snapshot()

	pool, via, err := st.resolve(svcIn("anywhere", "s", ""))
	require.NoError(t, err)
	assert.Equal(t, defaultPoolName, pool.String())
	assert.Equal(t, viaDefault, via)
}

// TestResolveNamespaceDefaulting: a listed namespace gets its ServiceGroup
// with no annotation, and -- with enforcement off -- an annotation still
// overrides in both directions.
func TestResolveNamespaceDefaulting(t *testing.T) {
	alloc := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("tenant-a-pool", "192.168.2.0/30", []string{"tenant-a"}, false, false),
	)
	st := alloc.Snapshot()

	pool, via, err := st.resolve(svcIn("tenant-a", "s", ""))
	require.NoError(t, err)
	assert.Equal(t, "tenant-a-pool", pool.String())
	assert.Equal(t, viaNamespace, via)

	// Not enforcing, so the annotation may still take a bound Service out...
	pool, _, err = st.resolve(svcIn("tenant-a", "s", defaultPoolName))
	require.NoError(t, err, "defaulting mode must not fence a bound namespace in")
	assert.Equal(t, defaultPoolName, pool.String())

	// ...and may bring an unbound Service in.
	pool, _, err = st.resolve(svcIn("tenant-b", "s", "tenant-a-pool"))
	require.NoError(t, err, "defaulting mode must not fence outsiders out")
	assert.Equal(t, "tenant-a-pool", pool.String())
}

// TestResolveEnforcementFencesBothDirections: with enforcement on, an
// outsider cannot get in (exclusion) and a bound Service cannot get out
// (confinement).
func TestResolveEnforcementFencesBothDirections(t *testing.T) {
	alloc := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("tenant-a-pool", "192.168.2.0/30", []string{"tenant-a"}, true, false),
	)
	st := alloc.Snapshot()

	_, _, err := st.resolve(svcIn("tenant-b", "s", "tenant-a-pool"))
	assert.ErrorIs(t, err, ErrNamespaceDenied, "exclusion: outsider must not reach the group")

	_, _, err = st.resolve(svcIn("tenant-a", "s", defaultPoolName))
	assert.ErrorIs(t, err, ErrNamespaceDenied, "confinement: bound Service must not leave")

	// An unbound namespace is unaffected by someone else's enforcement.
	pool, _, err := st.resolve(svcIn("tenant-b", "s", ""))
	require.NoError(t, err)
	assert.Equal(t, defaultPoolName, pool.String())
}

// TestMixedEnforceFencesBothGroups is the half-open-fence regression guard.
// Exclusion derives from namespace confinement, not from the target group's
// own flag -- so enforcing on a tenant's L2 pool must also fence its BGP
// pool, which does not set the flag. A future "fix" reading the target's own
// EnforceNamespaces has to break this test on purpose.
func TestMixedEnforceFencesBothGroups(t *testing.T) {
	alloc := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("tenant-a-l2", "192.168.2.0/30", []string{"tenant-a"}, true, true),
		nsGroup("tenant-a-bgp", "192.168.3.0/30", []string{"tenant-a"}, false, false),
	)
	st := alloc.Snapshot()

	_, _, err := st.resolve(svcIn("tenant-b", "s", "tenant-a-bgp"))
	assert.ErrorIs(t, err, ErrNamespaceDenied,
		"the non-enforcing group of a confined namespace must still refuse outsiders")

	// The tenant itself still reaches both of its own ServiceGroups.
	pool, _, err := st.resolve(svcIn("tenant-a", "s", "tenant-a-bgp"))
	require.NoError(t, err, "an eligible non-default group must stay reachable by annotation")
	assert.Equal(t, "tenant-a-bgp", pool.String())
}

// TestMultipleEligibleGroupsL2AndBGP: a ServiceGroup is exactly one of
// local/remote/external, so an L2 + BGP tenant needs two. The unannotated
// Service gets the marked default; an annotation reaches the other.
func TestMultipleEligibleGroupsL2AndBGP(t *testing.T) {
	alloc := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("tenant-a-l2", "192.168.2.0/30", []string{"tenant-a"}, true, true),
		nsGroup("tenant-a-bgp", "192.168.3.0/30", []string{"tenant-a"}, true, false),
	)
	st := alloc.Snapshot()

	pool, via, err := st.resolve(svcIn("tenant-a", "s", ""))
	require.NoError(t, err)
	assert.Equal(t, "tenant-a-l2", pool.String(), "the marked default wins")
	assert.Equal(t, viaNamespace, via)

	pool, _, err = st.resolve(svcIn("tenant-a", "s", "tenant-a-bgp"))
	require.NoError(t, err)
	assert.Equal(t, "tenant-a-bgp", pool.String())
}

// TestAmbiguousDefaultIsModeDependent: with two eligible groups and none
// marked, an enforcing namespace denies (falling back would breach the
// boundary it declared) while a non-enforcing one degrades to "default" --
// so an additive, non-enforcing config can never take a namespace down.
func TestAmbiguousDefaultIsModeDependent(t *testing.T) {
	enforcing := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("a-one", "192.168.2.0/30", []string{"tenant-a"}, true, false),
		nsGroup("a-two", "192.168.3.0/30", []string{"tenant-a"}, true, false),
	).Snapshot()
	_, _, err := enforcing.resolve(svcIn("tenant-a", "s", ""))
	assert.ErrorIs(t, err, ErrAmbiguousDefault)

	relaxed := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("a-one", "192.168.2.0/30", []string{"tenant-a"}, false, false),
		nsGroup("a-two", "192.168.3.0/30", []string{"tenant-a"}, false, false),
	).Snapshot()
	pool, via, err := relaxed.resolve(svcIn("tenant-a", "s", ""))
	require.NoError(t, err, "a non-enforcing config must not break")
	assert.Equal(t, defaultPoolName, pool.String())
	assert.Equal(t, viaAmbiguous, via, "provenance must record that this was a fallback, not a choice")
}

// TestDuplicateNamespaceEntryIsNotAmbiguity: +listType=set is deliberately
// not used, so the API server accepts `namespaces: [a, a]`. Without a dedupe
// one ServiceGroup would appear twice in eligible, look like two groups
// competing, and deny its own namespace.
func TestDuplicateNamespaceEntryIsNotAmbiguity(t *testing.T) {
	alloc := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("tenant-a-pool", "192.168.2.0/30", []string{"tenant-a", "tenant-a"}, true, false),
	)
	pool, via, err := alloc.Snapshot().resolve(svcIn("tenant-a", "s", ""))
	require.NoError(t, err, "a repeated namespace entry must not read as two competing groups")
	assert.Equal(t, "tenant-a-pool", pool.String())
	assert.Equal(t, viaNamespace, via)
}

// TestResolveUnknownPoolMessageUnchanged pins the wording operators grep for.
func TestResolveUnknownPoolMessageUnchanged(t *testing.T) {
	alloc := nsState(t, nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false))
	_, _, err := alloc.Snapshot().resolve(svcIn("ns", "s", "nonexistent-pool"))
	assert.ErrorIs(t, err, ErrUnknownPool)
	assert.Contains(t, err.Error(), `unknown pool "nonexistent-pool"`)
}

// TestPinnedAddressRespectsBoundary: an address is a way of naming a
// ServiceGroup, so pinning must not bypass the boundary that naming it would
// hit. kube-vip ships exactly this gap.
func TestPinnedAddressRespectsBoundary(t *testing.T) {
	alloc := nsState(t,
		nsGroup(defaultPoolName, "192.168.1.0/30", nil, false, false),
		nsGroup("tenant-a-pool", "192.168.2.0/30", []string{"tenant-a"}, true, false),
	)
	st := alloc.Snapshot()

	// tenant-b pins an address that lives inside tenant-a's fenced pool.
	svc := svcIn("tenant-b", "thief", "")
	svc.Annotations[purelbv2.DesiredAddressAnnotation] = "192.168.2.1"
	_, err := alloc.allocateSpecificIP(st, svc)
	assert.ErrorIs(t, err, ErrNamespaceDenied, "pinning must not bypass exclusion")
	assert.Empty(t, svc.Status.LoadBalancer.Ingress)

	// And the pre-flight must run before anything is released: a Service
	// that already held an address keeps it when the request is refused.
	held := svcIn("tenant-b", "incumbent", "")
	require.NoError(t, alloc.Allocate(st, held))
	require.Len(t, held.Status.LoadBalancer.Ingress, 1)
	before := held.Status.LoadBalancer.Ingress[0].IP

	held.Annotations[purelbv2.DesiredAddressAnnotation] = "192.168.2.1"
	_, err = alloc.allocateSpecificIP(st, held)
	assert.ErrorIs(t, err, ErrNamespaceDenied)
	require.Len(t, held.Status.LoadBalancer.Ingress, 1,
		"the refusal must happen before Unassign, or the Service is left released but still advertised")
	assert.Equal(t, before, held.Status.LoadBalancer.Ingress[0].IP)
}

// TestBoundNamespacesInStatus: the field must actually change when the spec
// does. statusEqual is a hand-maintained field list, so a status field it
// does not compare is written once and then frozen -- the confirmation
// signal would show the old list while the allocator acted on the new one.
func TestBoundNamespacesInStatus(t *testing.T) {
	alloc := New(allocatorTestLogger)
	alloc.SetClient(&testK8S{t: t})
	writer := &fakeStatusWriter{}
	alloc.SetServiceGroupStatusWriter(writer)

	g := nsGroup("tenant-a-pool", "192.168.2.0/30", []string{"tenant-a"}, false, false)
	require.NoError(t, alloc.SetPools([]*purelbv2.ServiceGroup{g}))
	require.NoError(t, alloc.PublishAllSGStatus(context.Background()))
	require.NotEmpty(t, writer.updates)
	assert.Equal(t, []string{"tenant-a"}, writer.updates[len(writer.updates)-1].Status.BoundNamespaces)

	// Change only spec.namespaces -- no other status field moves.
	g2 := nsGroup("tenant-a-pool", "192.168.2.0/30", []string{"tenant-a", "tenant-a-staging"}, false, false)
	require.NoError(t, alloc.SetPools([]*purelbv2.ServiceGroup{g2}))
	require.NoError(t, alloc.PublishAllSGStatus(context.Background()))
	assert.Equal(t, []string{"tenant-a", "tenant-a-staging"},
		writer.updates[len(writer.updates)-1].Status.BoundNamespaces,
		"statusEqual must not elide a pure spec.namespaces edit")
}

// TestAllocationFailureReason locks in the metric taxonomy. The sentinels
// exist so failures can be classified without string matching; before this
// they were defined and never consumed, so a namespace boundary refusing a
// tenant's Services produced no metric at all.
func TestAllocationFailureReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"namespace denied", fmt.Errorf("%w: nope", ErrNamespaceDenied), "namespace_denied"},
		{"ambiguous default", fmt.Errorf("%w: nope", ErrAmbiguousDefault), "ambiguous_namespace_default"},
		{"no usable pool", fmt.Errorf("%w: nope", ErrNoUsablePool), "no_usable_pool"},
		{"unknown pool", fmt.Errorf("%w %q", ErrUnknownPool, "x"), "unknown_pool"},
		{"unclassified", errors.New("pool exhausted"), "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, allocationFailureReason(tt.err))
		})
	}
}

// TestIsRetryableAllocationErr covers the requeue decision. Getting this
// wrong in either direction is costly: retrying a permanent failure spins
// the goroutine that allocates every address in the cluster, and not
// retrying a transient one leaves a correct Service pending indefinitely.
func TestIsRetryableAllocationErr(t *testing.T) {
	retryable := []struct {
		name string
		err  error
	}{
		{"sidecar down", status.Error(codes.Unavailable, "connection refused")},
		{"sidecar too slow", status.Error(codes.DeadlineExceeded, "timeout")},
		{"sidecar out of capacity right now", status.Error(codes.ResourceExhausted, "busy")},
		{"sidecar aborted", status.Error(codes.Aborted, "conflict")},
		{"our context timed out", context.DeadlineExceeded},
		{"our context cancelled", context.Canceled},
	}
	for _, tt := range retryable {
		t.Run("retry/"+tt.name, func(t *testing.T) {
			assert.True(t, isRetryableAllocationErr(tt.err))
		})
	}

	permanent := []struct {
		name string
		err  error
	}{
		{"namespace boundary", fmt.Errorf("%w: nope", ErrNamespaceDenied)},
		{"ambiguous default", fmt.Errorf("%w: nope", ErrAmbiguousDefault)},
		{"unknown pool", fmt.Errorf("%w %q", ErrUnknownPool, "x")},
		{"pool exhausted", errors.New("no available IPs")},
		{"sharing conflict", errors.New("sharing key does not match")},
		{"sidecar rejected the request", status.Error(codes.InvalidArgument, "bad pool")},
		{"sidecar has no such pool", status.Error(codes.NotFound, "no pool")},
	}
	for _, tt := range permanent {
		t.Run("no-retry/"+tt.name, func(t *testing.T) {
			assert.False(t, isRetryableAllocationErr(tt.err))
		})
	}
}
