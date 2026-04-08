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
	"fmt"
	"sync"
	"testing"

	"purelb.io/internal/k8s"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"

	"github.com/go-kit/log"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func diffService(a, b *v1.Service) string {
	return cmp.Diff(a, b)
}

func statusAssigned(ip string) v1.ServiceStatus {
	ipModeVIP := v1.LoadBalancerIPModeVIP
	return v1.ServiceStatus{
		LoadBalancer: v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{
					IP:     ip,
					IPMode: &ipModeVIP,
				},
			},
		},
	}
}

// testK8S implements service by recording what the controller wants
// to do to k8s.
type testK8S struct {
	loggedWarning bool
	t             *testing.T
}

func (s *testK8S) Infof(_ runtime.Object, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Info event %q: %s", evtType, fmt.Sprintf(msg, args...))
}

func (s *testK8S) Errorf(_ runtime.Object, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Warning event %q: %s", evtType, fmt.Sprintf(msg, args...))
	s.loggedWarning = true
}

func (s *testK8S) ForceSync() {}

func (s *testK8S) reset() {
	s.loggedWarning = false
}

func TestControllerConfig(t *testing.T) {
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	c := &controller{
		logger: l,
		ips:    a,
		client: k,
	}

	// Create service that would need an IP allocation

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			Annotations: map[string]string{
				purelbv2.DesiredGroupAnnotation: defaultPoolName,
			},
		},
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	assert.Equal(t, k8s.SyncStateError, c.SetBalancer(svc, nil), "SetBalancer should have failed")
	assert.False(t, k.loggedWarning, "SetBalancer with no configuration logged an error")

	// Set an empty config. Balancer should still not do anything to
	// our unallocated service, and return an error to force a
	// retry after sync is complete.
	wantSvc := svc.DeepCopy()
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(&purelbv2.Config{}), "SetConfig with empty config failed")
	assert.Equal(t, k8s.SyncStateError, c.SetBalancer(svc, nil), "SetBalancer did not fail")

	assert.Empty(t, diffService(wantSvc, svc), "unsynced SetBalancer mutated service")
	assert.False(t, k.loggedWarning, "unsynced SetBalancer logged an error")

	// Set a config with some IPs. Still no allocation, not synced.
	cfg := &purelbv2.Config{
		DefaultAnnouncer: true,
		Groups: []*purelbv2.ServiceGroup{
			{ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pool: &purelbv2.AddressPool{
							Pool:   "1.2.3.0/24",
							Subnet: "1.2.3.0/24",
						},
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg), "SetConfig failed")
	wantSvc = svc.DeepCopy()
	assert.Equal(t, k8s.SyncStateError, c.SetBalancer(svc, nil), "SetBalancer did not fail")

	assert.Empty(t, diffService(wantSvc, svc), "unsynced SetBalancer mutated service")
	assert.False(t, k.loggedWarning, "unsynced SetBalancer logged an error")

	// Mark synced. Finally, we can allocate.
	c.MarkSynced()

	wantSvc = svc.DeepCopy()
	wantSvc.Status = statusAssigned("1.2.3.0")
	wantSvc.ObjectMeta = metav1.ObjectMeta{
		Name: "test",
		Annotations: map[string]string{
			purelbv2.DesiredGroupAnnotation: defaultPoolName,
			purelbv2.BrandAnnotation:        purelbv2.Brand,
			purelbv2.PoolAnnotation:         defaultPoolName,
			purelbv2.PoolTypeAnnotation:     purelbv2.PoolTypeLocal,
		},
	}

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil), "SetBalancer failed")

	assert.Empty(t, diffService(wantSvc, svc), "SetBalancer produced unexpected mutation")

	// Deleting the config also makes PureLB sad.
	assert.Equal(t, k8s.SyncStateError, c.SetConfig(nil), "SetConfig that deletes the config was accepted")
}

// ============================================================================
// Multi-pool SetBalancer tests
// ============================================================================

// newMultiPoolController creates a controller with a multi-pool ServiceGroup
// and active subnets configured. Ready to call SetBalancer.
func newMultiPoolController(t *testing.T, v4pools []purelbv2.AddressPool, v6pools []purelbv2.AddressPool, activeSubnets []string) (*controller, *testK8S) {
	t.Helper()
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	a.SetActiveSubnets(mockActiveSubnets(activeSubnets), "purelb")

	c := &controller{
		logger:    l,
		ips:       a,
		client:    k,
	}
	c.isDefault.Store(true)

	cfg := &purelbv2.Config{
		DefaultAnnouncer: true,
		Groups: []*purelbv2.ServiceGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pools:   v4pools,
						V6Pools:   v6pools,
						MultiPool: true,
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg))
	c.MarkSynced()
	return c, k
}

// newMixedController creates a controller with both a multi-pool and a
// non-multi-pool ServiceGroup.
func newMixedController(t *testing.T) (*controller, *testK8S) {
	t.Helper()
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	a.SetActiveSubnets(mockActiveSubnets([]string{"192.168.1.0/24", "192.168.2.0/24"}), "purelb")

	c := &controller{
		logger:    l,
		ips:       a,
		client:    k,
	}
	c.isDefault.Store(true)

	cfg := &purelbv2.Config{
		DefaultAnnouncer: true,
		Groups: []*purelbv2.ServiceGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "multi"},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pools: []purelbv2.AddressPool{
							{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
							{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
						},
						MultiPool: true,
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pool: &purelbv2.AddressPool{
							Pool:   "10.0.0.0/24",
							Subnet: "10.0.0.0/24",
						},
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg))
	c.MarkSynced()
	return c, k
}

func multiPoolSvc(name string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      name,
			Annotations: map[string]string{
				purelbv2.DesiredGroupAnnotation: defaultPoolName,
			},
		},
		Spec: v1.ServiceSpec{
			Type:       "LoadBalancer",
			ClusterIP:  "10.96.0.1",
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
		},
	}
}

func TestMultiPoolSetBalancerAllocates(t *testing.T) {
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc := multiPoolSvc("svc1")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "should get 2 IPs from 2 active subnets")
	assert.Equal(t, purelbv2.Brand, svc.Annotations[purelbv2.BrandAnnotation])
	assert.Equal(t, defaultPoolName, svc.Annotations[purelbv2.PoolAnnotation])
}

func TestMultiPoolLoopPrevention(t *testing.T) {
	// This is the critical test: once a multi-pool service has IPs and is
	// branded, subsequent SetBalancer calls must NOT re-allocate. Without
	// this guard, each allocation triggers a service update which re-triggers
	// SetBalancer, creating an infinite loop.
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc := multiPoolSvc("svc1")

	// First call: allocates
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress))
	firstIPs := make([]string, len(svc.Status.LoadBalancer.Ingress))
	for i, ing := range svc.Status.LoadBalancer.Ingress {
		firstIPs[i] = ing.IP
	}

	// Second call (simulating the update event from the first allocation):
	// must return success WITHOUT changing ingress
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "ingress count must not change")
	for i, ing := range svc.Status.LoadBalancer.Ingress {
		assert.Equal(t, firstIPs[i], ing.IP, "IP must not change on re-entry")
	}

	// Third call: still stable
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "must remain stable on repeated calls")
}

func TestMultiPoolNoBrandFallsThrough(t *testing.T) {
	// A multi-pool service with ingress but NOT branded by PureLB should
	// fall through to allocation (e.g., another controller set the IPs).
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc := multiPoolSvc("svc1")
	// Pre-set some ingress without brand annotation
	ipModeVIP := v1.LoadBalancerIPModeVIP
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{
		{IP: "10.10.10.10", IPMode: &ipModeVIP},
	}

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	// Should have allocated new IPs (the old non-pool IP gets replaced by Unassign+Allocate)
	assert.Equal(t, purelbv2.Brand, svc.Annotations[purelbv2.BrandAnnotation])
}

func TestMultiPoolDualStack(t *testing.T) {
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
		},
		[]purelbv2.AddressPool{
			{Pool: "fd00:1::1/128", Subnet: "fd00:1::/64"},
			{Pool: "fd00:2::1/128", Subnet: "fd00:2::/64"},
		},
		[]string{"192.168.1.0/24", "192.168.2.0/24", "fd00:1::/64", "fd00:2::/64"},
	)

	svc := multiPoolSvc("svc1")
	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 4, len(svc.Status.LoadBalancer.Ingress), "dual-stack should get 4 IPs: 2 v4 + 2 v6")
}

func TestMultiPoolPartialSubnets(t *testing.T) {
	// Only one of two subnets is active — should get 1 IP, not 2
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
		}, nil,
		[]string{"192.168.1.0/24"}, // only subnet 1 active
	)

	svc := multiPoolSvc("svc1")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 1, len(svc.Status.LoadBalancer.Ingress), "should get 1 IP for 1 active subnet")
}

func TestMultiPoolAnnotationOverrideOnSG(t *testing.T) {
	// ServiceGroup has multiPool: true, but service annotation says "false"
	// → should get single IP via normal allocation path
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc := multiPoolSvc("svc1")
	svc.Annotations[purelbv2.MultiPoolAnnotation] = "false"

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 1, len(svc.Status.LoadBalancer.Ingress), "annotation false should override SG multiPool")
}

func TestMultiPoolAnnotationEnablesOnNonMultiSG(t *testing.T) {
	// ServiceGroup does NOT have multiPool, but annotation says "true"
	c, _ := newMixedController(t)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "svc1",
			Annotations: map[string]string{
				purelbv2.DesiredGroupAnnotation: "multi",
				purelbv2.MultiPoolAnnotation:    "true",
			},
		},
		Spec: v1.ServiceSpec{
			Type:       "LoadBalancer",
			ClusterIP:  "10.96.0.1",
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
		},
	}
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "annotation true should enable multi-pool")
}

func TestMultiPoolDeleteRecycles(t *testing.T) {
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/32", Subnet: "192.168.1.0/24"}, // 1 IP each
			{Pool: "192.168.2.0/32", Subnet: "192.168.2.0/24"},
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc1 := multiPoolSvc("svc1")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc1, nil))
	assert.Equal(t, 2, len(svc1.Status.LoadBalancer.Ingress))

	// Ranges are now exhausted — second service should get allocation failure
	svc2 := multiPoolSvc("svc2")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc2, nil))
	assert.Empty(t, svc2.Status.LoadBalancer.Ingress, "pool exhausted, no IPs")

	// Delete first service — should free both IPs
	assert.Equal(t, k8s.SyncStateReprocessAll, c.DeleteBalancer(namespacedName(svc1)))

	// Now second service should succeed
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc2, nil))
	assert.Equal(t, 2, len(svc2.Status.LoadBalancer.Ingress), "recycled IPs should be available")
}

func TestMultiPoolRejectsSharingAtController(t *testing.T) {
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
		}, nil,
		[]string{"192.168.1.0/24"},
	)

	svc := multiPoolSvc("svc1")
	svc.Annotations[purelbv2.SharingAnnotation] = "share-me"

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	// Allocation should fail — ingress stays empty
	assert.Empty(t, svc.Status.LoadBalancer.Ingress, "multi-pool + sharing should be rejected")
}

func TestNonMultiPoolUnaffected(t *testing.T) {
	// A non-multi-pool service on the default pool should still get exactly 1 IP,
	// even when a multi-pool SG exists.
	c, _ := newMixedController(t)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "svc1",
			Annotations: map[string]string{
				purelbv2.DesiredGroupAnnotation: defaultPoolName,
			},
		},
		Spec: v1.ServiceSpec{
			Type:       "LoadBalancer",
			ClusterIP:  "10.96.0.1",
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
		},
	}
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 1, len(svc.Status.LoadBalancer.Ingress), "non-multi-pool should get exactly 1 IP")
}

func TestMultiPoolTypeChange(t *testing.T) {
	// Service starts as LoadBalancer with multi-pool IPs, then gets changed
	// to ClusterIP. Should release all IPs.
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc := multiPoolSvc("svc1")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress))

	// Change type to ClusterIP
	svc.Spec.Type = "ClusterIP"
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Empty(t, svc.Status.LoadBalancer.Ingress, "type change should release all IPs")
	assert.Empty(t, svc.Annotations[purelbv2.PoolAnnotation], "pool annotation should be removed")
}

func TestMultiPoolNoClusterIP(t *testing.T) {
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
		}, nil,
		[]string{"192.168.1.0/24"},
	)

	svc := multiPoolSvc("svc1")
	svc.Spec.ClusterIP = "" // no ClusterIP yet

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Empty(t, svc.Status.LoadBalancer.Ingress, "no ClusterIP → no allocation")
}

func TestMultiPoolUnknownPoolName(t *testing.T) {
	// Service references a pool that doesn't exist. isMultiPoolService
	// should return false, and the service follows the normal single-IP path.
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
		}, nil,
		[]string{"192.168.1.0/24"},
	)

	svc := multiPoolSvc("svc1")
	svc.Annotations[purelbv2.DesiredGroupAnnotation] = "nonexistent-pool"
	// No multi-pool annotation, so it falls through to pool lookup → pool not found → false

	// Should fail allocation (pool doesn't exist) but not panic
	result := c.SetBalancer(svc, nil)
	assert.Equal(t, k8s.SyncStateSuccess, result)
	assert.Empty(t, svc.Status.LoadBalancer.Ingress, "unknown pool should fail allocation")
}

func TestMultiPoolLBClassFiltering(t *testing.T) {
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
		}, nil,
		[]string{"192.168.1.0/24"},
	)

	otherClass := "other.io/lb"
	svc := multiPoolSvc("svc1")
	svc.Spec.LoadBalancerClass = &otherClass

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Empty(t, svc.Status.LoadBalancer.Ingress, "wrong LB class should be ignored")
	assert.Empty(t, svc.Annotations[purelbv2.BrandAnnotation], "should not be branded")
}

func TestMultiPoolMultipleServices(t *testing.T) {
	// Two services from the same multi-pool SG should each get their own IPs
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"}, // 2 IPs
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"}, // 2 IPs
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc1 := multiPoolSvc("svc1")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc1, nil))
	assert.Equal(t, 2, len(svc1.Status.LoadBalancer.Ingress))

	svc2 := multiPoolSvc("svc2")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc2, nil))
	assert.Equal(t, 2, len(svc2.Status.LoadBalancer.Ingress))

	// Verify no IP overlap
	svc1IPs := map[string]bool{}
	for _, ing := range svc1.Status.LoadBalancer.Ingress {
		svc1IPs[ing.IP] = true
	}
	for _, ing := range svc2.Status.LoadBalancer.Ingress {
		assert.False(t, svc1IPs[ing.IP], "services must not share IPs: %s", ing.IP)
	}
}

func TestMultiPoolConfigReprocess(t *testing.T) {
	// After a config change (SetConfig), existing multi-pool services should
	// survive re-processing via the loop prevention guard.
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	a.SetActiveSubnets(mockActiveSubnets([]string{"192.168.1.0/24", "192.168.2.0/24"}), "purelb")

	c := &controller{
		logger:    l,
		ips:       a,
		client:    k,
	}
	c.isDefault.Store(true)

	cfg := &purelbv2.Config{
		DefaultAnnouncer: true,
		Groups: []*purelbv2.ServiceGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pools: []purelbv2.AddressPool{
							{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
							{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
						},
						MultiPool: true,
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg))
	c.MarkSynced()

	svc := multiPoolSvc("svc1")
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress))
	originalIPs := make([]string, len(svc.Status.LoadBalancer.Ingress))
	for i, ing := range svc.Status.LoadBalancer.Ingress {
		originalIPs[i] = ing.IP
	}

	// Simulate config reprocess (same config reapplied)
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg))

	// Re-process the service — should keep existing IPs via loop prevention
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "should keep IPs after config reprocess")
	for i, ing := range svc.Status.LoadBalancer.Ingress {
		assert.Equal(t, originalIPs[i], ing.IP, "IPs should be unchanged after reprocess")
	}
}

func TestMultiPoolExplicitLBClass(t *testing.T) {
	// When PureLB is NOT the default announcer but the service explicitly
	// sets LoadBalancerClass to PureLB's class, it should still allocate.
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	a.SetActiveSubnets(mockActiveSubnets([]string{"192.168.1.0/24", "192.168.2.0/24"}), "purelb")

	c := &controller{
		logger:    l,
		ips:       a,
		client:    k,
		// isDefault defaults to false (not the default announcer)
	}

	cfg := &purelbv2.Config{
		DefaultAnnouncer: false,
		Groups: []*purelbv2.ServiceGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pools: []purelbv2.AddressPool{
							{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
							{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
						},
						MultiPool: true,
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg))
	c.MarkSynced()

	lbClass := purelbv2.ServiceLBClass
	svc := multiPoolSvc("svc1")
	svc.Spec.LoadBalancerClass = &lbClass

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "explicit PureLB LBClass should allocate even when not default")
}

func TestMultiPoolNotDefaultAnnouncer(t *testing.T) {
	// When PureLB is not the default announcer and service has no LBClass,
	// multi-pool or not, the service should be ignored.
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	a.SetActiveSubnets(mockActiveSubnets([]string{"192.168.1.0/24"}), "purelb")

	c := &controller{
		logger:    l,
		ips:       a,
		client:    k,
		// isDefault defaults to false (not the default announcer)
	}

	cfg := &purelbv2.Config{
		DefaultAnnouncer: false,
		Groups: []*purelbv2.ServiceGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pools: []purelbv2.AddressPool{
							{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
						},
						MultiPool: true,
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg))
	c.MarkSynced()

	svc := multiPoolSvc("svc1")
	// No LoadBalancerClass set
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Empty(t, svc.Status.LoadBalancer.Ingress, "not-default announcer should ignore service without LBClass")
}

func TestDeleteRecyclesIP(t *testing.T) {
	l := log.NewNopLogger()
	k := &testK8S{t: t}
	a := New(l)
	a.client = k
	c := &controller{
		logger: l,
		ips:    a,
		client: k,
	}

	cfg := &purelbv2.Config{
		DefaultAnnouncer: true,
		Groups: []*purelbv2.ServiceGroup{
			{ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
				Spec: purelbv2.ServiceGroupSpec{
					Local: &purelbv2.ServiceGroupLocalSpec{
						V4Pool: &purelbv2.AddressPool{
							Pool:   "1.2.3.0/32",
							Subnet: "1.2.3.0/24",
						},
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg), "SetConfig failed")
	c.MarkSynced()

	svc1 := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "test",
			Annotations: map[string]string{
				purelbv2.DesiredGroupAnnotation: defaultPoolName,
			},
		},
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc1, nil), "SetBalancer svc1 failed")
	assert.NotEmpty(t, svc1.Status.LoadBalancer.Ingress, "svc1 didn't get an IP")
	assert.Equal(t, "1.2.3.0", svc1.Status.LoadBalancer.Ingress[0].IP, "svc1 got the wrong IP")
	k.reset()

	// Second service should converge correctly, but not allocate an
	// IP because we have none left.
	svc2 := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "test2",
			Annotations: map[string]string{
				purelbv2.DesiredGroupAnnotation: defaultPoolName,
			},
		},
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc2, nil), "SetBalancer svc2 failed")
	assert.Empty(t, svc2.Status.LoadBalancer.Ingress, "svc2 didn't get an IP")
	k.reset()

	// Deleting the first LB should tell us to reprocess all services.
	assert.Equal(t, k8s.SyncStateReprocessAll, c.DeleteBalancer(namespacedName(svc1)), "DeleteBalancer didn't tell us to reprocess all balancers")

	// Setting svc2 should now allocate correctly.
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc2, nil), "SetBalancer svc2 failed")
	assert.NotEmpty(t, svc2.Status.LoadBalancer.Ingress, "svc2 didn't get an IP")
	assert.Equal(t, "1.2.3.0", svc2.Status.LoadBalancer.Ingress[0].IP, "svc2 got the wrong IP")
}

func TestIncrementalMultiPoolViaSetBalancer(t *testing.T) {
	// Start with 2 active subnets, 3 pool ranges
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
			{Pool: "192.168.2.0/31", Subnet: "192.168.2.0/24"},
			{Pool: "192.168.3.0/31", Subnet: "192.168.3.0/24"},
		}, nil,
		[]string{"192.168.1.0/24", "192.168.2.0/24"},
	)

	svc := multiPoolSvc("svc1")

	// First allocation: 2 IPs (subnets 1 and 2 active)
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "initial: 2 IPs")

	// Second call with same subnets: stable, no new IPs
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 2, len(svc.Status.LoadBalancer.Ingress), "stable: still 2 IPs")

	// Now subnet 3 becomes active (simulates new node joining + config update)
	c.ips.SetActiveSubnets(mockActiveSubnets([]string{"192.168.1.0/24", "192.168.2.0/24", "192.168.3.0/24"}), "purelb")

	// Next SetBalancer should pick up the 3rd IP
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))
	assert.Equal(t, 3, len(svc.Status.LoadBalancer.Ingress), "incremental: should now have 3 IPs")
}

func TestReEvaluateAnnotationCleared(t *testing.T) {
	c, _ := newMultiPoolController(t,
		[]purelbv2.AddressPool{
			{Pool: "192.168.1.0/31", Subnet: "192.168.1.0/24"},
		}, nil,
		[]string{"192.168.1.0/24"},
	)

	svc := multiPoolSvc("svc1")
	svc.Annotations[purelbv2.ReEvaluateAnnotation] = "true"

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer(svc, nil))

	// Annotation should be cleared after processing
	_, hasReEval := svc.Annotations[purelbv2.ReEvaluateAnnotation]
	assert.False(t, hasReEval, "re-evaluate annotation should be cleared after processing")

	// Service should still get allocated normally
	assert.NotEmpty(t, svc.Status.LoadBalancer.Ingress, "service should be allocated")
}

// TestConcurrentSetPoolsAndSetBalancer exercises the race between G1
// (SetBalancer/DeleteBalancer) and G2 (SetConfig/SetPools) to verify
// that atomic.Pointer prevents data races. Run with -race to detect.
func TestConcurrentSetPoolsAndSetBalancer(t *testing.T) {
	l := log.NewNopLogger()
	a := New(l)
	k := &testK8S{t: t}
	a.SetClient(k)

	c := &controller{
		logger: l,
		ips:    a,
		client: k,
		synced: true,
	}
	c.isDefault.Store(true)

	// Two ServiceGroup configs to alternate between
	cfg1 := &purelbv2.Config{
		DefaultAnnouncer: true,
		Groups: []*purelbv2.ServiceGroup{
			localServiceGroup("default", "10.0.0.0/28"),
		},
	}
	cfg2 := &purelbv2.Config{
		DefaultAnnouncer: true,
		Groups: []*purelbv2.ServiceGroup{
			localServiceGroup("default", "10.0.1.0/28"),
		},
	}

	// Initial config
	c.SetConfig(cfg1)

	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(2)

	// G2: rapidly swap pool configs
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				c.SetConfig(cfg2)
			} else {
				c.SetConfig(cfg1)
			}
		}
	}()

	// G1: rapidly process services (SetBalancer + DeleteBalancer)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			svc := service(fmt.Sprintf("svc-%d", i), ports("tcp/80"), "")
			c.SetBalancer(&svc, nil)
			c.DeleteBalancer(fmt.Sprintf("default/svc-%d", i))
		}
	}()

	wg.Wait()
}
