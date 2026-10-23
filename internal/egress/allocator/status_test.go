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
	"errors"
	"strings"
	"testing"
	"time"

	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// events returns and clears the recorded events.
func (g *gatingFixture) events() []recorded {
	return g.r.recorder.(*recorder).take()
}

func hasEvent(evs []recorded, reason, contains string) bool {
	for _, e := range evs {
		if e.reason == reason && strings.Contains(e.message, contains) {
			return true
		}
	}
	return false
}

func condition(t *testing.T, e *purelbv2.Egress, typ string) metav1.Condition {
	t.Helper()
	c := apimeta.FindStatusCondition(e.Status.Conditions, typ)
	require.NotNil(t, c, "condition %s", typ)
	return *c
}

// The policy's status reports its selection, each address with its
// slices and capacity, and the conditions, Ready among them.
func TestStatusPublished(t *testing.T) {
	sel := map[string]string{"app": "a"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), pod("voice", "a-1", "n1", sel), pod("voice", "a-2", "n1", sel),
		egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	g.reconcile(t, "egress/voice/sip")
	e := g.egress(t, "voice", "sip")
	assert.Equal(t, int32(2), e.Status.SelectedPods)
	require.Len(t, e.Status.Addresses, 1)
	a := e.Status.Addresses[0]
	assert.Equal(t, "192.168.10.10", a.Address)
	assert.Equal(t, []purelbv2.EgressSliceStatus{{Node: "n1", Ports: "4096-8191", State: sliceActive}}, a.Slices)
	assert.Equal(t, &purelbv2.EgressCapacity{SlicesTotal: 15, Active: 1, Free: 14}, a.Capacity)
	for _, typ := range append(published, condReady) {
		assert.Equal(t, metav1.ConditionTrue, condition(t, e, typ).Status, typ)
	}
	assert.Equal(t, 1.0, ptu.ToFloat64(slicesGauge.WithLabelValues("192.168.10.10", "active")))
	assert.InDelta(t, 1.0/15, ptu.ToFloat64(sliceUtilization.WithLabelValues("192.168.10.10")), 1e-9)

	evs := g.events()
	assert.True(t, hasEvent(evs, "AddressAllocated", "192.168.10.10"), "%v", evs)
	assert.True(t, hasEvent(evs, "SliceActivated", "on n1"), "%v", evs)
	assert.True(t, hasEvent(evs, "PodSelected", "egress/voice/sip"), "%v", evs)
}

// A Redacted group's status hides nodes and slices, and its node-bearing
// events go to the EgressEndpoints in the PureLB namespace.
func TestRedacted(t *testing.T) {
	sel := map[string]string{"app": "a"}
	sg := group("default")
	sg.Spec.Egress.Tenancy = &purelbv2.EgressTenancy{StatusDetail: "Redacted"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{sg}, namespace("voice", nil),
		node("n1", true), pod("voice", "a-1", "n1", sel), egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	g.reconcile(t, "egress/voice/sip")
	a := g.egress(t, "voice", "sip").Status.Addresses[0]
	assert.Empty(t, a.Slices)
	assert.Empty(t, a.Announcers)
	assert.NotNil(t, a.Capacity)
	evs := g.events()
	require.True(t, hasEvent(evs, "SliceActivated", "on n1"))
	for _, e := range evs {
		switch e.reason {
		case "SliceActivated":
			assert.IsType(t, &purelbv2.EgressEndpoints{}, e.obj, "node-bearing: to the EgressEndpoints")
		case "AddressAllocated":
			assert.IsType(t, &purelbv2.Egress{}, e.obj, "no node: to the Egress")
		}
	}
}

// Two live nodes announcing a SingleAnnouncer address for two lease
// durations is SplitBrain.
func TestSplitBrain(t *testing.T) {
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), node("n2", true), liveLease("n1", t0), liveLease("n2", t0), egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	ep := g.ep(t)
	for _, n := range []string{"n1", "n2"} {
		g.status(t, n, purelbv2.EgressNodeStateStatus{Announcing: []purelbv2.EgressAnnouncing{
			{Endpoints: ep.Name, Address: ep.Spec.Address, Generation: ep.Generation}}})
	}
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, []string{"n1", "n2"}, g.ep(t).Status.ObservedAnnouncers)
	assert.Equal(t, metav1.ConditionTrue, condition(t, g.egress(t, "voice", "sip"), condAnnounced).Status, "not yet")

	g.clock = t0.Add(2 * 10 * time.Second)
	g.renew(t, "n1")
	g.renew(t, "n2")
	g.reconcile(t, "egress/voice/sip")
	c := condition(t, g.egress(t, "voice", "sip"), condAnnounced)
	assert.Equal(t, reasonSplitBrain, c.Reason)
	assert.True(t, hasEvent(g.events(), reasonSplitBrain, "192.168.10.10"))
}

// A higher-ranked policy taking every destination of a pod outranks the
// lower one for that pod; one taking only some destinations leaves it
// partially outranked.
func TestOutranking(t *testing.T) {
	sel := map[string]string{"app": "a"}
	narrow := func(s *purelbv2.EgressSpec) {
		s.Destinations = purelbv2.EgressDestinations{Include: []purelbv2.EgressDestination{{CIDRs: []string{"203.0.113.0/24"}}}}
	}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), pod("voice", "a-1", "n1", sel),
		egressObj("voice", "high", func(s *purelbv2.EgressSpec) { s.Priority = 100 }),
		egressObj("voice", "low", narrow),
		egressObj("voice", "lowwide", func(s *purelbv2.EgressSpec) {
			s.Destinations.Include = append(s.Destinations.Include, purelbv2.EgressDestination{CIDRs: []string{"::/0"}})
		}))
	g.reconcile(t, "egress/voice/high")

	g.reconcile(t, "egress/voice/low")
	low := g.egress(t, "voice", "low")
	assert.Equal(t, reasonOutrankedBy, condition(t, low, condPolicyEffective).Reason)
	assert.Contains(t, condition(t, low, condPolicyEffective).Message, "egress/voice/high")
	assert.Equal(t, []purelbv2.EgressExcludedPod{{Pod: "a-1", Namespace: "voice", Reason: excludedOutranked}}, low.Status.ExcludedPods)

	// high has no IPv6 destination: lowwide keeps the pod for ::/0.
	g.reconcile(t, "egress/voice/lowwide")
	lw := g.egress(t, "voice", "lowwide")
	assert.Equal(t, reasonPartiallyOutranked, condition(t, lw, condPolicyEffective).Reason)
	assert.Equal(t, int32(1), lw.Status.SelectedPods)

	assert.Equal(t, metav1.ConditionTrue, condition(t, g.egress(t, "voice", "high"), condPolicyEffective).Status)
}

func TestDestinationsCover(t *testing.T) {
	d := func(cidrs ...string) purelbv2.EgressDestinations {
		return purelbv2.EgressDestinations{Include: []purelbv2.EgressDestination{{CIDRs: cidrs}}}
	}
	withPorts := func(dst purelbv2.EgressDestinations, ports ...purelbv2.EgressPortRange) purelbv2.EgressDestinations {
		dst.Include[0].Ports = ports
		return dst
	}
	assert.True(t, destinationsCover(d("0.0.0.0/0"), d("10.0.0.0/8")))
	assert.False(t, destinationsCover(d("10.0.0.0/8"), d("0.0.0.0/0")))
	assert.False(t, destinationsCover(d("0.0.0.0/0"), d("::/0")), "families don't cover each other")
	hi := d("0.0.0.0/0")
	hi.Exclude = d("10.0.0.0/8").Include
	assert.False(t, destinationsCover(hi, d("192.0.2.0/24")), "an exclude may hand flows back")
	assert.True(t, destinationsCover(withPorts(d("0.0.0.0/0"), purelbv2.EgressPortRange{Port: 1, EndPort: ptr(int32(1024))}),
		withPorts(d("10.0.0.0/8"), purelbv2.EgressPortRange{Port: 443})))
	assert.False(t, destinationsCover(withPorts(d("0.0.0.0/0"), purelbv2.EgressPortRange{Port: 443}), d("10.0.0.0/8")),
		"some ports don't cover every port")
	assert.False(t, destinationsCover(withPorts(d("0.0.0.0/0"), purelbv2.EgressPortRange{Protocol: "UDP", Port: 443}),
		withPorts(d("10.0.0.0/8"), purelbv2.EgressPortRange{Port: 443})), "TCP isn't UDP")

	assert.True(t, destinationsOverlap(d("10.0.0.0/8"), d("10.1.0.0/16")))
	assert.False(t, destinationsOverlap(d("10.0.0.0/8"), d("192.0.2.0/24")))
}

// A status already submitted isn't written again while the informer
// catches up; a failed write is retried.
func TestStatusWrittenOnce(t *testing.T) {
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil), egressObj("voice", "sip", nil))
	fail := true
	patches := 0
	g.purelb.PrependReactor("patch", "egresses", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "status" {
			return false, nil, nil
		}
		patches++
		if fail {
			fail = false
			return true, nil, errors.New("injected")
		}
		return false, nil, nil
	})
	require.NoError(t, g.r.Sync("egress/voice/sip"))
	g.settle(t)
	require.Equal(t, 1, patches, "the first write fails")
	require.NoError(t, g.r.Sync("egress/voice/sip"))
	require.NoError(t, g.r.Sync("egress/voice/sip")) // before the informer sees it
	g.settle(t)
	assert.Equal(t, 2, patches, "retried once, not rewritten")
	assert.NotEmpty(t, g.egress(t, "voice", "sip").Status.Conditions)
}
