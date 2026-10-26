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
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func (g *gatingFixture) egress(t *testing.T, ns, name string) *purelbv2.Egress {
	t.Helper()
	e, err := g.purelb.PurelbV2().Egresses(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return e
}

func (g *gatingFixture) updateEgress(t *testing.T, e *purelbv2.Egress) {
	t.Helper()
	_, err := g.purelb.PurelbV2().Egresses(e.Namespace).Update(context.Background(), e, metav1.UpdateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
}

func byState(eps []purelbv2.EgressEndpoints) map[string]string {
	out := map[string]string{}
	for _, ep := range eps {
		out[ep.Spec.Address] = ep.Spec.State
	}
	return out
}

// Deleting a policy drains its slices, turns its address into a
// tombstone, then lets the policy go; the address stays out of the pool,
// across a rebuild, until the quarantine ends.
func TestDeleteToTombstone(t *testing.T) {
	sel := map[string]string{"app": "a"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), pod("voice", "a-1", "n1", sel), egressObj("voice", "sip", nil), egressObj("voice", "next", nil))
	g.reconcile(t, "egress/voice/sip")
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, []string{egress.Finalizer}, g.egress(t, "voice", "sip").Finalizers)
	require.Equal(t, map[string]string{"n1": sliceActive}, states(g.ep(t)))

	e := g.egress(t, "voice", "sip")
	e.DeletionTimestamp = &metav1.Time{Time: t0}
	g.updateEgress(t, e)
	g.reconcile(t, "egress/voice/sip")
	ep := g.ep(t)
	assert.Equal(t, map[string]string{"n1": sliceDraining}, states(ep))
	assert.Zero(t, reserves(ep), "reserves go at once")
	assert.Empty(t, g.members(t))
	assert.Equal(t, []string{egress.Finalizer}, g.egress(t, "voice", "sip").Finalizers, "kept while the address is Active")

	g.status(t, "n1", purelbv2.EgressNodeStateStatus{Drained: []purelbv2.EgressDrained{{Endpoints: ep.Name, Index: ep.Spec.Slices[0].Index}}})
	g.reconcile(t, "egress/voice/sip")
	assert.Empty(t, g.ep(t).Spec.Slices)
	g.reconcile(t, "egress/voice/sip")
	ep = g.ep(t)
	assert.Equal(t, stateQuarantined, ep.Spec.State)
	assert.Equal(t, t0.Add(time.Hour), ep.Spec.Until.Time)
	assert.True(t, g.q.has(tombstonePrefix+ep.Name), "the tombstone is enqueued")
	g.reconcile(t, "egress/voice/sip")
	assert.Empty(t, g.egress(t, "voice", "sip").Finalizers, "released once nothing is Active")

	// The tombstone holds the address, across a rebuild.
	require.NoError(t, g.r.alloc.SetPools([]*purelbv2.ServiceGroup{group("default")}))
	g.reconcile(t, "egress/voice/next")
	assert.Equal(t, map[string]string{"192.168.10.10": stateQuarantined, "192.168.10.11": stateActive}, byState(g.endpoints(t)))

	// Quarantine ends: released and deleted.
	require.NoError(t, g.r.Sync(tombstonePrefix+ep.Name))
	g.settle(t)
	assert.Len(t, g.endpoints(t), 2, "not yet")
	g.clock = t0.Add(time.Hour)
	require.NoError(t, g.r.Sync(tombstonePrefix+ep.Name))
	g.settle(t)
	assert.Equal(t, map[string]string{"192.168.10.11": stateActive}, byState(g.endpoints(t)))
	assert.Equal(t, 1, g.inUse(t, "default"))
}

// A group that stops allowing egress withdraws the policy's addresses and
// then clears its finalizer.
func TestDisallowedWithdraws(t *testing.T) {
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	require.Equal(t, []string{egress.Finalizer}, g.egress(t, "voice", "sip").Finalizers)

	off := group("default")
	off.Spec.Egress.Allowed = false
	require.NoError(t, g.r.alloc.SetPools([]*purelbv2.ServiceGroup{off}))
	for i := 0; i < 3; i++ {
		g.reconcile(t, "egress/voice/sip")
	}
	assert.Equal(t, stateQuarantined, g.ep(t).Spec.State)
	assert.Empty(t, g.egress(t, "voice", "sip").Finalizers)
}

// A PerPod address survives its pod's restart and is released when its
// ordinal is scaled away (D27). Its pod's node holds the whole address as
// slice 0, which drains before the address is retired.
func TestPerPodOrdinals(t *testing.T) {
	sel := map[string]string{"app": "a"}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "db"}, Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(2))}}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("db", nil), node("n1", true), sts,
		stsPod("db", "db-0", "n1", sel), stsPod("db", "db-1", "n1", sel),
		egressObj("db", "perpod", func(s *purelbv2.EgressSpec) { s.Affinity = &purelbv2.EgressAffinity{Mode: "PerPod"} }))
	g.reconcile(t, "egress/db/perpod")
	g.reconcile(t, "egress/db/perpod")
	require.Equal(t, map[string]string{"192.168.10.10": stateActive, "192.168.10.11": stateActive}, byState(g.endpoints(t)))
	db1 := endpointsOf(t, g.endpoints(t), "192.168.10.11")
	require.Equal(t, []purelbv2.EgressSlice{{Index: perPodIndex, Node: "n1", State: sliceActive}}, db1.Spec.Slices,
		"the pod's node holds the whole address")

	require.NoError(t, g.kube.CoreV1().Pods("db").Delete(context.Background(), "db-1", metav1.DeleteOptions{}))
	time.Sleep(100 * time.Millisecond)
	g.reconcile(t, "egress/db/perpod")
	assert.Equal(t, map[string]string{"192.168.10.10": stateActive, "192.168.10.11": stateActive}, byState(g.endpoints(t)), "restarting")
	assert.Equal(t, map[string]string{"n1": sliceDraining}, states(endpointsOf(t, g.endpoints(t), "192.168.10.11")),
		"the gone pod's node drains")
	g.status(t, "n1", purelbv2.EgressNodeStateStatus{Drained: []purelbv2.EgressDrained{{
		Endpoints: db1.Name, Index: perPodIndex}}})
	g.reconcile(t, "egress/db/perpod")
	g.reconcile(t, "egress/db/perpod")
	assert.Empty(t, endpointsOf(t, g.endpoints(t), "192.168.10.11").Spec.Slices)
	assert.Equal(t, map[string]string{"192.168.10.10": stateActive, "192.168.10.11": stateActive}, byState(g.endpoints(t)),
		"drained, and still kept for the restarting pod")

	sts.Spec.Replicas = ptr(int32(1))
	_, err := g.kube.AppsV1().StatefulSets("db").Update(context.Background(), sts, metav1.UpdateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	g.reconcile(t, "egress/db/perpod")
	assert.Equal(t, map[string]string{"192.168.10.10": stateActive, "192.168.10.11": stateQuarantined}, byState(g.endpoints(t)), "scaled down")
}

// A PerPod pod that moves: its old node's slice drains, and the new node
// gets the address only once that slice is freed.
func TestPerPodMove(t *testing.T) {
	sel := map[string]string{"app": "a"}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "db"}, Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(1))}}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("db", nil), node("n1", true), node("n2", true), sts,
		stsPod("db", "db-0", "n1", sel),
		egressObj("db", "perpod", func(s *purelbv2.EgressSpec) { s.Affinity = &purelbv2.EgressAffinity{Mode: "PerPod"} }))
	g.reconcile(t, "egress/db/perpod")
	g.reconcile(t, "egress/db/perpod")
	require.Equal(t, map[string]string{"n1": sliceActive}, states(g.ep(t)))

	require.NoError(t, g.kube.CoreV1().Pods("db").Delete(context.Background(), "db-0", metav1.DeleteOptions{}))
	_, err := g.kube.CoreV1().Pods("db").Create(context.Background(), stsPod("db", "db-0", "n2", sel), metav1.CreateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	g.reconcile(t, "egress/db/perpod")
	assert.Equal(t, map[string]string{"n1": sliceDraining}, states(g.ep(t)), "never on two nodes at once")
	g.reconcile(t, "egress/db/perpod")
	assert.Equal(t, map[string]string{"n1": sliceDraining}, states(g.ep(t)), "until the old node drains")

	g.status(t, "n1", purelbv2.EgressNodeStateStatus{Drained: []purelbv2.EgressDrained{{Endpoints: g.ep(t).Name, Index: perPodIndex}}})
	g.reconcile(t, "egress/db/perpod")
	assert.Equal(t, map[string]string{"n2": slicePending}, states(g.ep(t)))
	g.reconcile(t, "egress/db/perpod")
	assert.Equal(t, map[string]string{"n2": sliceActive}, states(g.ep(t)))
}

// A changed pin is allocated first; the old address is retired.
func TestPinChange(t *testing.T) {
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		egressObj("voice", "sip", func(s *purelbv2.EgressSpec) { s.Addresses = []string{"192.168.10.15"} }))
	g.reconcile(t, "egress/voice/sip")
	e := g.egress(t, "voice", "sip")
	e.Spec.Addresses = []string{"192.168.10.16"}
	g.updateEgress(t, e)
	g.reconcile(t, "egress/voice/sip")
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, map[string]string{"192.168.10.15": stateQuarantined, "192.168.10.16": stateActive}, byState(g.endpoints(t)))
}

// A policy gone without its finalizer has its addresses retired.
func TestOrphanRetired(t *testing.T) {
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	require.NoError(t, g.purelb.PurelbV2().Egresses("voice").Delete(context.Background(), "sip", metav1.DeleteOptions{}))
	time.Sleep(100 * time.Millisecond)
	g.reconcile(t, "egress/voice/sip")
	g.reconcile(t, "egress/voice/sip")
	assert.Equal(t, stateQuarantined, g.ep(t).Spec.State)
}

// endpointsOf is the EgressEndpoints of addr among eps.
func endpointsOf(t *testing.T, eps []purelbv2.EgressEndpoints, addr string) *purelbv2.EgressEndpoints {
	t.Helper()
	for i := range eps {
		if eps[i].Spec.Address == addr {
			return &eps[i]
		}
	}
	t.Fatalf("no EgressEndpoints for %s", addr)
	return nil
}
