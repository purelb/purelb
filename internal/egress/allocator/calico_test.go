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
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	listers "purelb.io/pkg/generated/listers/purelb/v2"
)

func calicoObj(kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": calicoGroupVersion, "kind": kind,
		"metadata": map[string]interface{}{"name": name}, "spec": spec,
	}}
}

func TestReadCalico(t *testing.T) {
	disc := func(served bool) *fakediscovery.FakeDiscovery {
		d := kubefake.NewSimpleClientset().Discovery().(*fakediscovery.FakeDiscovery)
		if served {
			d.Fake.Resources = []*metav1.APIResourceList{{GroupVersion: calicoGroupVersion,
				APIResources: []metav1.APIResource{{Name: "ippools"}, {Name: "felixconfigurations"}}}}
		}
		d.Fake = &clienttesting.Fake{Resources: d.Fake.Resources}
		return d
	}
	dyn := func(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
		return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			calicoPoolsGVR: "IPPoolList", calicoFelixGVR: "FelixConfigurationList"}, objs...)
	}
	on := calicoObj("FelixConfiguration", "default", map[string]interface{}{"bpfEnabled": true})
	off := calicoObj("FelixConfiguration", "default", map[string]interface{}{"bpfEnabled": false})
	p1 := calicoObj("IPPool", "a", map[string]interface{}{"cidr": "10.62.0.0/16", "natOutgoing": true})
	p2 := calicoObj("IPPool", "b", map[string]interface{}{"cidr": "fd62::/56"})

	got, err := readCalico(disc(false), dyn())
	require.NoError(t, err)
	assert.Nil(t, got, "not Calico")
	got, err = readCalico(disc(true), dyn(off, p1))
	require.NoError(t, err)
	assert.Nil(t, got, "Calico, not eBPF: every pool reaches netfilter")
	got, err = readCalico(disc(true), dyn(p1))
	require.NoError(t, err)
	assert.Nil(t, got, "no default configuration: Felix's default isn't eBPF")

	got, err = readCalico(disc(true), dyn(on, p1, p2))
	require.NoError(t, err)
	require.NotNil(t, got)
	nat, ok := got.natOutgoing(netip.MustParseAddr("10.62.1.1"))
	assert.True(t, nat && ok)
	nat, ok = got.natOutgoing(netip.MustParseAddr("fd62::1"))
	assert.True(t, !nat && ok, "natOutgoing unset is off")
	_, ok = got.natOutgoing(netip.MustParseAddr("192.0.2.1"))
	assert.False(t, ok, "outside every pool")
}

// A node's selected pods must agree with each other and with the node's stage
// (resolution 20); nothing is reported off Calico's eBPF dataplane.
func TestNatConflicts(t *testing.T) {
	r := &Reconciler{}
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	r.pods = pods
	add := func(name, ip string) {
		require.NoError(t, pods.Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: ip}}}}))
	}
	add("on", "10.1.0.1")
	add("off", "10.2.0.1")
	add("none", "192.0.2.1")
	states := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	r.nodeStates = listers.NewEgressNodeStateLister(states)
	stage := func(node, s string) {
		require.NoError(t, states.Add(&purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: node},
			Status: purelbv2.EgressNodeStateStatus{Capability: &purelbv2.EgressCapability{PodNodeStage: s}}}))
	}
	stage("netfilter", "Netfilter")
	stage("ebpf", "EBPF")
	m := func(names ...string) (out []member) {
		for _, n := range names {
			out = append(out, member{namespace: "ns", name: n})
		}
		return out
	}
	rn := &run{sel: selection{nodes: map[string][]member{
		"mixed": m("on", "off"), "unknown-on": m("on"), "unknown-off": m("off"),
		"netfilter": m("on"), "ebpf": m("off"),
		"netfilter-off": m("off"), "ebpf-on": m("on"), "outside": m("none"),
	}}}
	stage("netfilter-off", "Netfilter")
	stage("ebpf-on", "EBPF")
	assert.Empty(t, r.natConflicts(rn), "not Calico eBPF")
	r.calico.Store(&calicoState{bpf: true, pools: []calicoPool{
		{netip.MustParsePrefix("10.1.0.0/16"), true}, {netip.MustParsePrefix("10.2.0.0/16"), false}}})
	assert.Equal(t, map[string]bool{"mixed": true, "netfilter-off": true, "ebpf-on": true}, r.natConflicts(rn))
}

func TestNatOutgoingLongestPool(t *testing.T) {
	c := &calicoState{bpf: true, pools: []calicoPool{
		{netip.MustParsePrefix("10.0.0.0/8"), true},
		{netip.MustParsePrefix("10.1.0.0/16"), false},
	}}
	nat, ok := c.natOutgoing(netip.MustParseAddr("10.1.2.3"))
	assert.True(t, ok)
	assert.False(t, nat)
	nat, _ = c.natOutgoing(netip.MustParseAddr("10.2.2.3"))
	assert.True(t, nat)
}
