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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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
