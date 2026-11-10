// Copyright 2020-2026 Acnodal Inc.
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
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/k8s"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// reEvalGroup is a single-pool local ServiceGroup; an empty range leaves
// that family out.
func reEvalGroup(name, v4, v6 string) *purelbv2.ServiceGroup {
	l := &purelbv2.ServiceGroupLocalSpec{}
	if v4 != "" {
		l.V4Pool = &purelbv2.AddressPool{Pool: v4, Subnet: v4}
	}
	if v6 != "" {
		l.V6Pool = &purelbv2.AddressPool{Pool: v6, Subnet: v6}
	}
	return &purelbv2.ServiceGroup{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: purelbv2.ServiceGroupSpec{Local: l}}
}

func reEvalController(t *testing.T, groups ...*purelbv2.ServiceGroup) *controller {
	t.Helper()
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	c := &controller{logger: l, ips: a, client: k}
	c.isDefault.Store(true)
	reEvalConfig(t, c, groups...)
	c.MarkSynced()
	return c
}

func reEvalConfig(t *testing.T, c *controller, groups ...*purelbv2.ServiceGroup) {
	t.Helper()
	require.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(&purelbv2.Config{DefaultAnnouncer: true, Groups: groups}))
}

func reEvalSvc(name, group string, families ...v1.IPFamily) *v1.Service {
	policy := v1.IPFamilyPolicySingleStack
	if len(families) == 2 {
		policy = v1.IPFamilyPolicyRequireDualStack
	}
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: name,
			Annotations: map[string]string{purelbv2.DesiredGroupAnnotation: group}},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, ClusterIP: "10.96.0.1",
			IPFamilies: families, IPFamilyPolicy: &policy},
	}
}

func ingressIPs(svc *v1.Service) []string {
	var out []string
	for _, in := range svc.Status.LoadBalancer.Ingress {
		out = append(out, in.IP)
	}
	return out
}

// A Service's addresses outside its group's changed pools move on
// re-evaluate, family by family; an address still inside stays. Without
// the annotation nothing moves.
func TestReEvaluateMovesOrphanedAddresses(t *testing.T) {
	v4, v6 := v1.IPv4Protocol, v1.IPv6Protocol
	for _, tc := range []struct {
		name             string
		families         []v1.IPFamily
		before, after    [2]string // the group's v4 and v6 ranges
		initial, settled []string
	}{
		{"IPv4", []v1.IPFamily{v4}, [2]string{"192.168.1.0/30", ""}, [2]string{"192.168.2.0/30", ""},
			[]string{"192.168.1.0"}, []string{"192.168.2.0"}},
		{"IPv6", []v1.IPFamily{v6}, [2]string{"", "2001:db8:1::/126"}, [2]string{"", "2001:db8:2::/126"},
			[]string{"2001:db8:1::1"}, []string{"2001:db8:2::1"}},
		{"dual-stack, IPv4 range changed", []v1.IPFamily{v4, v6},
			[2]string{"192.168.1.0/30", "2001:db8:1::/126"}, [2]string{"192.168.2.0/30", "2001:db8:1::/126"},
			[]string{"192.168.1.0", "2001:db8:1::1"}, []string{"2001:db8:1::1", "192.168.2.0"}},
		{"dual-stack, both changed", []v1.IPFamily{v4, v6},
			[2]string{"192.168.1.0/30", "2001:db8:1::/126"}, [2]string{"192.168.2.0/30", "2001:db8:2::/126"},
			[]string{"192.168.1.0", "2001:db8:1::1"}, []string{"192.168.2.0", "2001:db8:2::1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := reEvalController(t, reEvalGroup(defaultPoolName, tc.before[0], tc.before[1]))
			svc := reEvalSvc("svc", defaultPoolName, tc.families...)
			require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
			require.Equal(t, tc.initial, ingressIPs(svc))

			reEvalConfig(t, c, reEvalGroup(defaultPoolName, tc.after[0], tc.after[1]))
			require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
			assert.Equal(t, tc.initial, ingressIPs(svc), "no annotation: kept")

			svc.Annotations[purelbv2.ReEvaluateAnnotation] = "true"
			require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
			assert.Equal(t, tc.settled, ingressIPs(svc), "re-evaluated: in the group's pools")
			assert.Equal(t, purelbv2.Brand, svc.Annotations[purelbv2.BrandAnnotation])
			assert.Equal(t, defaultPoolName, svc.Annotations[purelbv2.PoolAnnotation])
			_, again := svc.Annotations[purelbv2.ReEvaluateAnnotation]
			assert.False(t, again)
		})
	}
}

// A pinned address is the user's to change: kept, though outside the
// pool.
func TestReEvaluateKeepsPinnedAddress(t *testing.T) {
	c := reEvalController(t, reEvalGroup(defaultPoolName, "192.168.1.0/30", ""))
	svc := reEvalSvc("svc", defaultPoolName, v1.IPv4Protocol)
	svc.Annotations[purelbv2.DesiredAddressAnnotation] = "192.168.1.2"
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	require.Equal(t, []string{"192.168.1.2"}, ingressIPs(svc))

	reEvalConfig(t, c, reEvalGroup(defaultPoolName, "192.168.2.0/30", ""))
	svc.Annotations[purelbv2.ReEvaluateAnnotation] = "true"
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, []string{"192.168.1.2"}, ingressIPs(svc))
}

// A shared address moves for the re-evaluated Service only; the other
// keeps it until it is re-evaluated too, then both share again.
func TestReEvaluateSharedAddress(t *testing.T) {
	c := reEvalController(t, reEvalGroup(defaultPoolName, "192.168.1.0/30", ""))
	s1 := reEvalSvc("s1", defaultPoolName, v1.IPv4Protocol)
	s2 := reEvalSvc("s2", defaultPoolName, v1.IPv4Protocol)
	for _, s := range []*v1.Service{s1, s2} {
		s.Annotations[purelbv2.SharingAnnotation] = "k"
		s.Spec.Ports = []v1.ServicePort{{Port: map[string]int32{"s1": 80, "s2": 443}[s.Name], Protocol: v1.ProtocolTCP}}
		require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(s, nil))
	}
	require.Equal(t, ingressIPs(s1), ingressIPs(s2), "shared")

	reEvalConfig(t, c, reEvalGroup(defaultPoolName, "192.168.2.0/30", ""))
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(s2, nil)) // the rebuilt pool learns s2's address
	s1.Annotations[purelbv2.ReEvaluateAnnotation] = "true"
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(s1, nil))
	assert.Equal(t, []string{"192.168.2.0"}, ingressIPs(s1))
	assert.Equal(t, []string{"192.168.1.0"}, ingressIPs(s2), "not re-evaluated: kept")

	s2.Annotations[purelbv2.ReEvaluateAnnotation] = "true"
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(s2, nil))
	assert.Equal(t, ingressIPs(s1), ingressIPs(s2), "shared again")
}

// A Service that names another group moves there, and its address in the
// old group is free again, even when the pools were just rebuilt and the
// informer cache still shows the Service with its old address.
func TestReEvaluateMovesToAnotherGroup(t *testing.T) {
	a, b := reEvalGroup("a", "192.168.1.0/30", ""), reEvalGroup("b", "192.168.2.0/30", "")
	c := reEvalController(t, a, b)
	svc := reEvalSvc("svc", "a", v1.IPv4Protocol)
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	require.Equal(t, []string{"192.168.1.0"}, ingressIPs(svc))
	cached := *svc.DeepCopy()
	c.ips.SetListServices(func() []v1.Service { return []v1.Service{cached} })
	reEvalConfig(t, c, a, b) // pools rebuilt: the next allocation replays the cache

	svc.Annotations[purelbv2.DesiredGroupAnnotation] = "b"
	svc.Annotations[purelbv2.ReEvaluateAnnotation] = "true"
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, []string{"192.168.2.0"}, ingressIPs(svc))

	other := reEvalSvc("other", "a", v1.IPv4Protocol)
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(other, nil))
	assert.Equal(t, []string{"192.168.1.0"}, ingressIPs(other), "the old address is free")
}

// A partial move: the group's IPv4 range moved to another group, its IPv6
// range didn't. The old IPv4 address, now in the other group's pool, is
// freed there, even with the pools just rebuilt and the informer cache
// still showing it: a replay after the release would record it again.
func TestReEvaluateFreesAddressInAnotherGroupsPool(t *testing.T) {
	c := reEvalController(t, reEvalGroup("a", "192.168.1.0/30", "2001:db8:1::/126"))
	svc := reEvalSvc("svc", "a", v1.IPv4Protocol, v1.IPv6Protocol)
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	require.Equal(t, []string{"192.168.1.0", "2001:db8:1::1"}, ingressIPs(svc))
	cached := *svc.DeepCopy()
	c.ips.SetListServices(func() []v1.Service { return []v1.Service{cached} })
	reEvalConfig(t, c, reEvalGroup("a", "192.168.3.0/30", "2001:db8:1::/126"), reEvalGroup("b", "192.168.1.0/30", ""))

	svc.Annotations[purelbv2.ReEvaluateAnnotation] = "true"
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, []string{"2001:db8:1::1", "192.168.3.0"}, ingressIPs(svc))

	other := reEvalSvc("other", "b", v1.IPv4Protocol)
	require.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(other, nil))
	assert.Equal(t, []string{"192.168.1.0"}, ingressIPs(other), "the old address is free in b")
}
