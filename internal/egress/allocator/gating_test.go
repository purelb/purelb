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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// gatingFixture is a fixture whose fake API server keeps
// metadata.generation, on a controllable clock.
type gatingFixture struct {
	*fixture
	clock time.Time
}

func newGatingFixture(t *testing.T, groups []*purelbv2.ServiceGroup, objs ...runtime.Object) *gatingFixture {
	t.Helper()
	f := newFixture(t, groups, objs...)
	g := &gatingFixture{fixture: f, clock: t0}
	f.r.now = func() time.Time { return g.clock }
	// The fake tracker doesn't keep generations: bump on a spec change.
	f.purelb.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch a := a.(type) {
		case k8stesting.CreateAction:
			if m, ok := a.GetObject().(metav1.Object); ok {
				m.SetGeneration(1)
			}
		case k8stesting.UpdateAction:
			old, err := f.purelb.Tracker().Get(a.GetResource(), a.GetNamespace(), a.GetObject().(metav1.Object).GetName())
			if err != nil {
				return false, nil, nil
			}
			m := a.GetObject().(metav1.Object)
			m.SetGeneration(old.(metav1.Object).GetGeneration())
			if !apiequality.Semantic.DeepEqual(specOf(old), specOf(a.GetObject())) {
				m.SetGeneration(m.GetGeneration() + 1)
			}
		}
		return false, nil, nil
	})
	return g
}

func specOf(o runtime.Object) interface{} {
	switch o := o.(type) {
	case *purelbv2.EgressEndpoints:
		return o.Spec
	case *purelbv2.EgressNodeState:
		return o.Spec
	case *purelbv2.EgressMembers:
		return o.Spec
	}
	return nil
}

// capable is a node's status reporting it egress-capable (D29).
var capable = purelbv2.EgressNodeStateStatus{Capability: &purelbv2.EgressCapability{EgressCapable: true}}

func liveLease(node string, renew time.Time) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: purelbv2.LeasePrefix + node},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr(node), LeaseDurationSeconds: ptr(int32(10)),
			RenewTime: &metav1.MicroTime{Time: renew}},
	}
}

func (g *gatingFixture) ep(t *testing.T) *purelbv2.EgressEndpoints {
	t.Helper()
	eps := g.endpoints(t)
	require.Len(t, eps, 1)
	return &eps[0]
}

// status writes a node's EgressNodeState status.
func (g *gatingFixture) status(t *testing.T, node string, st purelbv2.EgressNodeStateStatus) {
	t.Helper()
	c := g.purelb.PurelbV2().EgressNodeStates()
	ns, err := c.Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		ns, err = c.Create(context.Background(), &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: node}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	ns.Status = st
	_, err = c.Update(context.Background(), ns, metav1.UpdateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
}

// renew renews node's Lease at the fixture's clock.
func (g *gatingFixture) renew(t *testing.T, node string) {
	t.Helper()
	_, err := g.kube.CoordinationV1().Leases(testNS).Update(context.Background(), liveLease(node, g.clock), metav1.UpdateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
}

func (g *gatingFixture) syncNode(t *testing.T, node string) *purelbv2.EgressNodeState {
	t.Helper()
	require.NoError(t, g.r.Sync(nodePrefix+node))
	g.settle(t)
	ns, err := g.purelb.PurelbV2().EgressNodeStates().Get(context.Background(), node, metav1.GetOptions{})
	require.NoError(t, err)
	return ns
}

func states(ep *purelbv2.EgressEndpoints) map[string]string {
	out := map[string]string{}
	for _, s := range ep.Spec.Slices {
		if s.Node != "" {
			out[s.Node] = s.State
		}
	}
	return out
}

func reserves(ep *purelbv2.EgressEndpoints) int {
	n := 0
	for _, s := range ep.Spec.Slices {
		if s.Node == "" {
			n++
		}
	}
	return n
}

// A pod node's slice is Pending until the address's announcers ack the
// generation that publishes it, then Active, in the EgressEndpoints and
// the node's EgressNodeState spec.
func TestSliceActivation(t *testing.T) {
	sel := map[string]string{"app": "a"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), node("n2", true), pod("voice", "a-1", "n1", sel), liveLease("n2", t0),
		egressObj("voice", "sip", nil))
	g.status(t, "n1", capable)
	g.reconcile(t, "egress/voice/sip")
	ep := g.ep(t)
	assert.Equal(t, map[string]string{"n1": slicePending}, states(ep))
	assert.Equal(t, defaultReserveSlices, reserves(ep))
	ns := g.syncNode(t, "n1")
	assert.Equal(t, []purelbv2.EgressNodeSlice{{Endpoints: ep.Name, Address: "192.168.10.10", Index: 1, State: slicePending}}, ns.Spec.Slices)

	// n2 announces the address but hasn't installed this generation.
	g.status(t, "n2", purelbv2.EgressNodeStateStatus{Announcing: []purelbv2.EgressAnnouncing{
		{Endpoints: ep.Name, Address: ep.Spec.Address, Generation: ep.Generation - 1}}})
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, map[string]string{"n1": slicePending}, states(g.ep(t)), "gated")

	g.status(t, "n2", purelbv2.EgressNodeStateStatus{Announcing: []purelbv2.EgressAnnouncing{
		{Endpoints: ep.Name, Address: ep.Spec.Address, Generation: ep.Generation}}})
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, map[string]string{"n1": sliceActive}, states(g.ep(t)), "acked")
	assert.Equal(t, sliceActive, g.syncNode(t, "n1").Spec.Slices[0].State)
}

// An announcer behind for six lease durations is evicted to
// excludedNodes (NodesLagging) and stops gating; it rejoins once caught
// up.
func TestLagEviction(t *testing.T) {
	sel := map[string]string{"app": "a"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), node("n2", true), pod("voice", "a-1", "n1", sel), liveLease("n2", t0),
		egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	ep := g.ep(t)
	g.status(t, "n2", purelbv2.EgressNodeStateStatus{Announcing: []purelbv2.EgressAnnouncing{
		{Endpoints: ep.Name, Address: ep.Spec.Address, Generation: 0}}})

	g.reconcile(t, "egress/voice/sip")
	assert.Empty(t, g.ep(t).Spec.ExcludedNodes)

	g.clock = t0.Add(59 * time.Second)
	g.renew(t, "n2")
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, slicePending, states(g.ep(t))["n1"], "not yet")

	g.clock = t0.Add(60 * time.Second)
	g.renew(t, "n2")
	assert.Equal(t, reasonNodesLagging, allocatedEndpoints(t, g.fixture, "egress/voice/sip")[condDatapathReady].reason)
	ep = g.ep(t)
	assert.Equal(t, []string{"n2"}, ep.Spec.ExcludedNodes)
	assert.Equal(t, sliceActive, states(ep)["n1"], "no longer gated by n2")

	g.status(t, "n2", purelbv2.EgressNodeStateStatus{Announcing: []purelbv2.EgressAnnouncing{
		{Endpoints: ep.Name, Address: ep.Spec.Address, Generation: ep.Generation}}})
	g.reconcile(t, "egress/voice/sip")
	assert.Empty(t, g.ep(t).Spec.ExcludedNodes, "rejoined")
}

// A claim takes the lowest reserve, already Active, and the reserve is
// replenished in the same generation.
func TestClaimTakesReserve(t *testing.T) {
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	ep := g.ep(t)
	require.Equal(t, defaultReserveSlices, reserves(ep))
	g.status(t, "n1", purelbv2.EgressNodeStateStatus{Claims: []purelbv2.EgressClaim{{Endpoints: ep.Name}}})
	assert.True(t, g.q.has("egress/voice/sip"), "a claim enqueues the owner")

	g.reconcile(t, "egress/voice/sip")
	ep = g.ep(t)
	assert.Equal(t, map[string]string{"n1": sliceActive}, states(ep))
	assert.Equal(t, int32(1), ep.Spec.Slices[0].Index, "the lowest reserve")
	assert.Equal(t, defaultReserveSlices, reserves(ep), "replenished")
}

// A node without selected pods drains; the drain ends at the node's
// drained report or the five-minute maximum; a gone node's slice is freed.
func TestDrain(t *testing.T) {
	sel := map[string]string{"app": "a"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), node("n2", true), node("n3", true),
		pod("voice", "a-1", "n1", sel), pod("voice", "a-2", "n2", sel), pod("voice", "a-3", "n3", sel),
		egressObj("voice", "sip", func(s *purelbv2.EgressSpec) {}))
	g.reconcile(t, "egress/voice/sip")
	g.reconcile(t, "egress/voice/sip")
	ep := g.ep(t)
	require.Equal(t, map[string]string{"n1": sliceActive, "n2": sliceActive, "n3": sliceActive}, states(ep))

	for _, p := range []string{"a-1", "a-2"} {
		require.NoError(t, g.kube.CoreV1().Pods("voice").Delete(context.Background(), p, metav1.DeleteOptions{}))
	}
	time.Sleep(100 * time.Millisecond)
	g.reconcile(t, "egress/voice/sip")
	ep = g.ep(t)
	assert.Equal(t, map[string]string{"n1": sliceDraining, "n2": sliceDraining, "n3": sliceActive}, states(ep))

	var n1 int32
	for _, s := range ep.Spec.Slices {
		if s.Node == "n1" {
			n1 = s.Index
		}
	}
	g.status(t, "n1", purelbv2.EgressNodeStateStatus{Drained: []purelbv2.EgressDrained{{Endpoints: ep.Name, Index: n1}}})
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, map[string]string{"n2": sliceDraining, "n3": sliceActive}, states(g.ep(t)), "n1 drained")

	g.clock = t0.Add(drainMax)
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, map[string]string{"n3": sliceActive}, states(g.ep(t)), "n2 reached the maximum")

	require.NoError(t, g.kube.CoreV1().Nodes().Delete(context.Background(), "n3", metav1.DeleteOptions{}))
	time.Sleep(100 * time.Millisecond)
	g.reconcile(t, "egress/voice/sip")
	assert.Empty(t, states(g.ep(t)), "n3 is gone")
}

// A node with no slot left gets no slice (SlotsExhausted);
// reservedClusterSlots holds slots back from namespaced Egresses only.
func TestSlotBudget(t *testing.T) {
	sel := map[string]string{"app": "a"}
	agent := &purelbv2.LBNodeAgent{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "default"},
		Spec: purelbv2.LBNodeAgentSpec{Egress: &purelbv2.LBNodeAgentEgressSpec{ReservedClusterSlots: slotBudget}}}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), pod("voice", "a-1", "n1", sel), agent, egressObj("voice", "sip", nil))
	assert.Equal(t, reasonSlotsExhausted, allocatedEndpoints(t, g.fixture, "egress/voice/sip")[condDatapathReady].reason)
	assert.Empty(t, states(g.ep(t)))
}

// A reserved port goes to the one selected pod its selector picks, and
// its block holds no slice; one matching two pods goes to no pod.
func TestReservedPorts(t *testing.T) {
	sel := map[string]string{"app": "a"}
	reg := pod("voice", "registrar-0", "n1", map[string]string{"app": "a", "role": "registrar"})
	reserve := func(selector map[string]string) func(*purelbv2.EgressSpec) {
		return func(s *purelbv2.EgressSpec) {
			s.Ports = &purelbv2.EgressPorts{Reserved: []purelbv2.EgressReservedPort{
				{Protocol: "UDP", Port: 5060, PodSelector: metav1.LabelSelector{MatchLabels: selector}}}}
		}
	}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), node("n2", true), reg, pod("voice", "a-2", "n2", sel),
		egressObj("voice", "one", reserve(map[string]string{"role": "registrar"})),
		egressObj("voice", "two", func(s *purelbv2.EgressSpec) {
			reserve(sel)(s)
			s.Priority = 10 // not outranked by "one"
		}))

	g.reconcile(t, "egress/voice/one")
	ep := g.ep(t)
	assert.Equal(t, []purelbv2.EgressReservedAssignment{{Protocol: "UDP", Port: 5060, Node: "n1", Pod: "registrar-0"}}, ep.Spec.Reserved)
	for _, s := range ep.Spec.Slices {
		assert.False(t, covers(ep.Spec.SliceSize, s.Index, []int32{5060}), "slice %d holds the reserved port", s.Index)
	}

	conds := allocatedEndpoints(t, g.fixture, "egress/voice/two")
	assert.Equal(t, reasonReservedPortAmbiguous, conds[condPortCapacity].reason)
}

// allocatedEndpoints runs a policy's pipeline through syncEndpoints and
// returns its conditions.
func allocatedEndpoints(t *testing.T, f *fixture, key string) conditions {
	t.Helper()
	ref, err := parsePolicyKey(key)
	require.NoError(t, err)
	run := &run{conds: conditions{}}
	run.p, _, _ = f.r.load(ref)
	f.r.scope(run)
	f.r.selectPods(run)
	f.r.syncEndpoints(run, f.r.allocate(run))
	f.settle(t)
	return run.conds
}

// The allocator creates a node's EgressNodeState, owned by its Node, and
// keeps the egress-capable label to the node's report (D29).
func TestNodeStateAndLabel(t *testing.T) {
	sel := map[string]string{"app": "a"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), pod("voice", "a-1", "n1", sel), egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	ns := g.syncNode(t, "n1")
	assert.Equal(t, "Node", ns.OwnerReferences[0].Kind)
	assert.Equal(t, "n1", ns.OwnerReferences[0].Name)
	n, err := g.kube.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, n.Labels, purelbv2.EgressCapableLabel, "nothing reported: not capable")

	g.status(t, "n1", capable)
	g.syncNode(t, "n1")
	n, err = g.kube.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", n.Labels[purelbv2.EgressCapableLabel])
}
