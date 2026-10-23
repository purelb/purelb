// Copyright 2026 Acnodal Inc.
//
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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// dualGroup is a local group with one IPv4 and one IPv6 entry.
func dualGroup(name string) *purelbv2.ServiceGroup {
	g := group(name)
	g.Spec.Local.V6Pool = &purelbv2.AddressPool{Name: "public", Pool: "fd00:10::10-fd00:10::20", Subnet: "fd00:10::/64"}
	return g
}

func (f *fixture) endpoints(t *testing.T) []purelbv2.EgressEndpoints {
	t.Helper()
	l, err := f.purelb.PurelbV2().EgressEndpointses(testNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Spec.Address < l.Items[j].Spec.Address })
	return l.Items
}

func addressesOf(eps []purelbv2.EgressEndpoints) []string {
	var out []string
	for _, ep := range eps {
		s := ep.Spec.Address
		if ep.Spec.Identity != "" {
			s = ep.Spec.Identity + "=" + s
		}
		out = append(out, s)
	}
	return out
}

func (f *fixture) inUse(t *testing.T, group string) int {
	t.Helper()
	_, pool, ok := f.r.alloc.ServiceGroup(group)
	require.True(t, ok)
	return pool.InUse()
}

func allocated(t *testing.T, f *fixture, key string) conditions {
	t.Helper()
	ref, err := parsePolicyKey(key)
	require.NoError(t, err)
	run := &run{conds: conditions{}}
	run.p, _, _ = f.r.load(ref)
	f.r.scope(run)
	f.r.selectPods(run)
	f.r.allocate(run)
	return run.conds
}

// A Hash Egress gets one address per family the pool has (PreferDualStack),
// recorded in EgressEndpoints, and keeps them across reconciles.
func TestAllocateDualStack(t *testing.T) {
	f := newFixture(t, []*purelbv2.ServiceGroup{dualGroup("default")}, namespace("voice", nil),
		egressObj("voice", "sip", nil))
	f.reconcile(t, "egress/voice/sip")
	eps := f.endpoints(t)
	assert.Equal(t, []string{"192.168.10.10", "fd00:10::10"}, addressesOf(eps))
	assert.Equal(t, 2, f.inUse(t, "default"))
	ep := eps[0]
	assert.Equal(t, "IPv4", ep.Spec.Family)
	assert.Equal(t, "Local", ep.Spec.PoolType)
	assert.Equal(t, "SingleAnnouncer", ep.Spec.Placement)
	assert.Equal(t, "public", ep.Spec.Pool)
	assert.Equal(t, int32(defaultSliceSize), ep.Spec.SliceSize)
	assert.Equal(t, "Hash", ep.Spec.Policy.AffinityMode)
	assert.Equal(t, "default", ep.Labels[egress.LabelServiceGroup])
	assert.Equal(t, egress.PoolLabelValue("public"), ep.Labels[egress.LabelPool])

	f.reconcile(t, "egress/voice/sip")
	assert.Equal(t, []string{"192.168.10.10", "fd00:10::10"}, addressesOf(f.endpoints(t)), "stable")
	assert.Equal(t, 2, f.inUse(t, "default"))
}

// Pins inside the pool are used; one outside it isn't.
func TestAllocatePins(t *testing.T) {
	f := newFixture(t, []*purelbv2.ServiceGroup{dualGroup("default")}, namespace("voice", nil),
		egressObj("voice", "pinned", func(s *purelbv2.EgressSpec) { s.Addresses = []string{"192.168.10.15"} }),
		egressObj("voice", "outside", func(s *purelbv2.EgressSpec) { s.Addresses = []string{"10.9.9.9"} }))
	f.reconcile(t, "egress/voice/pinned")
	assert.Equal(t, []string{"192.168.10.15"}, addressesOf(f.endpoints(t)))
	assert.Equal(t, reasonPinUnavailable, allocated(t, f, "egress/voice/outside")[condAllocated].reason)
}

// The group bounds the allocation: an unknown pool entry, no entry on a
// multi-pool group, a family the pool lacks.
func TestAllocateFailures(t *testing.T) {
	multi := group("multi")
	multi.Spec.Local = &purelbv2.ServiceGroupLocalSpec{MultiPool: true, V4Pools: []purelbv2.AddressPool{
		{Pool: "192.168.20.10-192.168.20.20", Subnet: "192.168.20.0/24"}}}
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default"), multi}, namespace("t", nil),
		egressObj("t", "badpool", func(s *purelbv2.EgressSpec) { s.Pool = "nope" }),
		egressObj("t", "nopool", func(s *purelbv2.EgressSpec) { s.ServiceGroup = "multi" }),
		egressObj("t", "dual", func(s *purelbv2.EgressSpec) { s.IPFamilyPolicy = "RequireDualStack" }))
	assert.Equal(t, reasonPoolNotFound, allocated(t, f, "egress/t/badpool")[condAllocated].reason)
	assert.Equal(t, reasonPoolRequired, allocated(t, f, "egress/t/nopool")[condAllocated].reason)
	assert.Equal(t, reasonFamilyUnavailable, allocated(t, f, "egress/t/dual")[condAllocated].reason)
	assert.Equal(t, 0, f.inUse(t, "default"))
}

// PerPod gives each StatefulSet pod its own address, keyed by identity.
func TestAllocatePerPod(t *testing.T) {
	sel := map[string]string{"app": "a"}
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("db", nil), node("n1", true),
		stsPod("db", "db-0", "n1", sel), stsPod("db", "db-1", "n1", sel),
		egressObj("db", "perpod", func(s *purelbv2.EgressSpec) { s.Affinity = &purelbv2.EgressAffinity{Mode: "PerPod"} }))
	f.reconcile(t, "egress/db/perpod")
	assert.Equal(t, []string{"db-0=192.168.10.10", "db-1=192.168.10.11"}, addressesOf(f.endpoints(t)))
}

// A serviceRef Egress references the Service's address: a hold, so the
// Service's own release leaves it allocated.
func TestAllocateServiceRef(t *testing.T) {
	g := group("default")
	g.Spec.Egress.AllowSharedServiceAddress = true
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip-gw",
			Annotations: map[string]string{purelbv2.BrandAnnotation: purelbv2.Brand, purelbv2.PoolAnnotation: "default"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "sip"},
			Ports: []corev1.ServicePort{{Protocol: corev1.ProtocolUDP, Port: 5060}}},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "192.168.10.12"}}}},
	}
	eg := egressObj("voice", "sip", func(s *purelbv2.EgressSpec) {
		s.PodSelector = nil
		s.ServiceRef = &purelbv2.EgressServiceRef{Name: "sip-gw"}
	})
	f := newFixture(t, []*purelbv2.ServiceGroup{g}, namespace("voice", nil), svc, eg)
	f.reconcile(t, "egress/voice/sip")
	eps := f.endpoints(t)
	require.Len(t, eps, 1)
	assert.Equal(t, "192.168.10.12", eps[0].Spec.Address)
	assert.Equal(t, &purelbv2.EgressServiceRef{Name: "sip-gw"}, eps[0].Spec.SharedWith)
	assert.Equal(t, []purelbv2.EgressServicePort{{Protocol: "UDP", Port: 5060}}, eps[0].Spec.ServicePorts)
	assert.Equal(t, "Inherit", eps[0].Spec.Policy.AffinityMode)

	_, pool, _ := f.r.alloc.ServiceGroup("default")
	require.NoError(t, pool.Release(context.Background(), "voice/sip-gw"))
	assert.Equal(t, 1, pool.InUse(), "the reference hold keeps the address")
}

// After a pool rebuild the egress addresses are replayed, so a new
// policy never gets one of them.
func TestReplayAfterRebuild(t *testing.T) {
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		egressObj("voice", "first", nil), egressObj("voice", "second", nil))
	f.reconcile(t, "egress/voice/first")
	require.Equal(t, []string{"192.168.10.10"}, addressesOf(f.endpoints(t)))

	require.NoError(t, f.r.alloc.SetPools([]*purelbv2.ServiceGroup{group("default")})) // empty pools
	f.reconcile(t, "egress/voice/second")
	assert.Equal(t, []string{"192.168.10.10", "192.168.10.11"}, addressesOf(f.endpoints(t)))
	assert.Equal(t, 2, f.inUse(t, "default"))
}

// A remote group's addresses are routed: MultiAnnouncer.
func TestAllocateRouted(t *testing.T) {
	g := group("bgp")
	g.Spec.Local = nil
	g.Spec.Remote = &purelbv2.ServiceGroupRemoteSpec{V4Pool: &purelbv2.AddressPool{Pool: "10.241.1.0/24", Subnet: "10.241.1.0/24"}}
	f := newFixture(t, []*purelbv2.ServiceGroup{g}, namespace("t", nil),
		egressObj("t", "r", func(s *purelbv2.EgressSpec) { s.ServiceGroup = "bgp" }))
	f.reconcile(t, "egress/t/r")
	eps := f.endpoints(t)
	require.Len(t, eps, 1)
	assert.Equal(t, "Routed", eps[0].Spec.PoolType)
	assert.Equal(t, "MultiAnnouncer", eps[0].Spec.Placement)
	assert.Equal(t, "10.241.1.0/24", eps[0].Spec.Pool)
	assert.Equal(t, egress.PoolLabelValue("10.241.1.0/24"), eps[0].Labels[egress.LabelPool])
}
