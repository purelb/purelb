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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// addrEndpoints is an EgressEndpoints of group "default" with slices.
func addrEndpoints(name, state, identity string, slices ...purelbv2.EgressSlice) *purelbv2.EgressEndpoints {
	return &purelbv2.EgressEndpoints{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, UID: types.UID("uid-" + name),
			Labels: map[string]string{"purelb.io/egress-name": "sip"}},
		Spec: purelbv2.EgressEndpointsSpec{State: state, Address: "192.168.10.10", Family: "IPv4", ServiceGroup: "default",
			Pool: "public", PoolType: "Local", Placement: "SingleAnnouncer", Identity: identity, SliceSize: 4096, Slices: slices},
	}
}

func (f *fixture) address(t *testing.T, name string) *purelbv2.EgressAddress {
	t.Helper()
	a, err := f.purelb.PurelbV2().EgressAddresses(testNS).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return a
}

func (f *fixture) addressWrites() int {
	n := 0
	for _, a := range f.purelb.Actions() {
		if a.GetResource().Resource == "egressaddresses" && (a.GetVerb() == "create" || a.GetVerb() == "patch") {
			n++
		}
	}
	return n
}

var someSlices = []purelbv2.EgressSlice{
	{Index: 1, Node: "n1", State: "Active"},
	{Index: 2, Node: "n2", State: "Draining"},
	{Index: 3, Node: "n3", State: "Pending"},
	{Index: 4, Node: "", State: "Active"},
}

// The EgressAddress carries the address without its slices, is owned by
// its EgressEndpoints, and is rewritten only when it changes.
func TestEgressAddress(t *testing.T) {
	ep := addrEndpoints("e-voice.sip-1", "Active", "", someSlices...)
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default")}, ep)
	f.reconcile(t, addressPrefix+ep.Name)

	a := f.address(t, ep.Name)
	assert.Equal(t, purelbv2.EgressAddressSpec{State: "Active", Address: "192.168.10.10", Family: "IPv4", ServiceGroup: "default",
		Pool: "public", PoolType: "Local", Placement: "SingleAnnouncer"}, a.Spec, "no preferred nodes without preferPodNodes")
	assert.Equal(t, []metav1.OwnerReference{{APIVersion: "purelb.io/v2", Kind: "EgressEndpoints", Name: ep.Name, UID: ep.UID}}, a.OwnerReferences)
	assert.Equal(t, ep.Labels, a.Labels)
	assert.Equal(t, 1, f.addressWrites())

	// A slice change alone doesn't touch it.
	ep.Spec.Slices = append(ep.Spec.Slices, purelbv2.EgressSlice{Index: 5, Node: "n4", State: "Active"})
	_, err := f.purelb.PurelbV2().EgressEndpointses(testNS).Update(context.Background(), ep, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.settle(t)
	f.reconcile(t, addressPrefix+ep.Name)
	assert.Equal(t, 1, f.addressWrites(), "a slice change is not an address change")

	// Quarantine is.
	ep.Spec.State = "Quarantined"
	_, err = f.purelb.PurelbV2().EgressEndpointses(testNS).Update(context.Background(), ep, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.settle(t)
	f.reconcile(t, addressPrefix+ep.Name)
	assert.Equal(t, "Quarantined", f.address(t, ep.Name).Spec.State)
	assert.Equal(t, 2, f.addressWrites())
}

// With preferPodNodes the election prefers the nodes holding an Active or
// Draining slice; a PerPod address prefers its pod's node (§5.5).
func TestEgressAddressPreferredNodes(t *testing.T) {
	g := group("default")
	g.Spec.Egress.PreferPodNodes = true
	hash := addrEndpoints("e-voice.sip-1", "Active", "", someSlices...)
	perPod := addrEndpoints("e-voice.db-2", "Active", "db-0", purelbv2.EgressSlice{Index: 0, Node: "n7", State: "Pending"})
	gone := addrEndpoints("e-voice.old-3", "Quarantined", "", someSlices...)
	f := newFixture(t, []*purelbv2.ServiceGroup{g}, hash, perPod, gone)
	for _, ep := range []string{hash.Name, perPod.Name, gone.Name} {
		f.reconcile(t, addressPrefix+ep)
	}
	assert.Equal(t, []string{"n1", "n2"}, f.address(t, hash.Name).Spec.PreferredNodes)
	assert.Equal(t, []string{"n7"}, f.address(t, perPod.Name).Spec.PreferredNodes, "a PerPod address's node, Pending included")
	assert.Empty(t, f.address(t, gone.Name).Spec.PreferredNodes, "a tombstone prefers no node")

	// PerPod prefers its pod node even without preferPodNodes.
	f2 := newFixture(t, []*purelbv2.ServiceGroup{group("default")}, perPod.DeepCopy())
	f2.reconcile(t, addressPrefix+perPod.Name)
	assert.Equal(t, []string{"n7"}, f2.address(t, perPod.Name).Spec.PreferredNodes)
}

// Every EgressEndpoints event queues its address, and ForceSync covers
// every address.
func TestEgressAddressKeys(t *testing.T) {
	ep := addrEndpoints("e-voice.sip-1", "Active", "")
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default")}, ep)
	assert.Contains(t, f.r.Keys(), addressPrefix+ep.Name)
	assert.True(t, f.q.has(addressPrefix+ep.Name))
}
