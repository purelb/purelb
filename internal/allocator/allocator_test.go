// Copyright 2017 Google Inc.
// Copyright 2020 Acnodal Inc.
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
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/go-kit/kit/log"
	"github.com/google/go-cmp/cmp"
	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv1 "purelb.io/pkg/apis/v1"
)

var (
	allocatorTestLogger = log.NewNopLogger()
)

func TestNotifyExisting(t *testing.T) {
	alloc := New(allocatorTestLogger)
	alloc.pools = map[string]Pool{
		"default": mustLocalPool(t, "192.168.1.2/31"),
	}
	ip1 := net.ParseIP("192.168.1.2")
	ip2 := net.ParseIP("192.168.1.3")

	svc1 := service("svc1", ports("tcp/80"), "")
	svc2 := service("svc2", ports("tcp/80"), "")

	// Tell the allocator that ip1 is in use
	svc1.Annotations[purelbv1.PoolAnnotation] = "default"
	addIngress(localPoolTestLogger, &svc1, ip1)
	assert.Nil(t, alloc.NotifyExisting(&svc1), "Notify failed")

	// Allocate an address to svc2 - it should get ip2 since ip1 is in
	// use by svc1
	_, err := alloc.AllocateAnyIP(&svc2)
	assert.Nil(t, err, "Allocating an address failed")
	assert.Equal(t, ip2.String(), svc2.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")
}

func TestAssignment(t *testing.T) {
	alloc := New(allocatorTestLogger)
	alloc.pools = map[string]Pool{
		"test0": mustLocalPool(t, "1.2.3.4/31"),
		"test1": mustLocalPool(t, "1000::4/127"),
		"test2": mustLocalPool(t, "1.2.4.0/24"),
		"test3": mustLocalPool(t, "1000::4:0/120"),
	}

	tests := []struct {
		desc       string
		svc        string
		ip         string
		ports      []v1.ServicePort
		sharingKey string
		wantErr    bool
	}{
		{
			desc: "assign s1",
			svc:  "s1",
			ip:   "1.2.3.4",
		},
		{
			desc: "s1 idempotent reassign",
			svc:  "s1",
			ip:   "1.2.3.4",
		},
		{
			desc:    "s2 can't grab s1's IP",
			svc:     "s2",
			ip:      "1.2.3.4",
			wantErr: true,
		},
		{
			desc: "s2 can get the other IP",
			svc:  "s2",
			ip:   "1.2.3.5",
		},
		{
			desc:    "s1 now can't grab s2's IP",
			svc:     "s1",
			ip:      "1.2.3.5",
			wantErr: true,
		},
		{
			desc: "s1 frees its IP",
			svc:  "s1",
			ip:   "",
		},
		{
			desc: "s2 can grab s1's former IP",
			svc:  "s2",
			ip:   "1.2.3.4",
		},
		{
			desc: "s1 can now grab s2's former IP",
			svc:  "s1",
			ip:   "1.2.3.5",
		},
		{
			desc: "s3 can grab another IP in that pool",
			svc:  "s3",
			ip:   "1.2.4.254",
		},
		{
			desc:       "s4 takes an IP, with sharing",
			svc:        "s4",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "sharing",
		},
		{
			desc:       "s4 changes its sharing key in place",
			svc:        "s4",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can't share with s4 (port conflict)",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong sharing key)",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			wantErr:    true,
		},
		{
			desc:       "s3 takes the same IP as s4",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can change its ports while keeping the same IP",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("udp/53"),
			sharingKey: "share",
		},
		{
			desc: "s4 takes s3's former IP",
			svc:  "s4",
			ip:   "1.2.4.254",
		},

		// IPv6 tests (same as ipv4 but with ipv6 addresses)
		{
			desc: "ipv6 assign s1",
			svc:  "s1",
			ip:   "1000::4",
		},
		{
			desc: "s1 idempotent reassign",
			svc:  "s1",
			ip:   "1000::4",
		},
		{
			desc:    "s2 can't grab s1's IP",
			svc:     "s2",
			ip:      "1000::4",
			wantErr: true,
		},
		{
			desc: "s2 can get the other IP",
			svc:  "s2",
			ip:   "1000::4:5",
		},
		{
			desc:    "s1 now can't grab s2's IP",
			svc:     "s1",
			ip:      "1000::4:5",
			wantErr: true,
		},
		{
			desc: "s1 frees its IP",
			svc:  "s1",
			ip:   "",
		},
		{
			desc: "s2 can grab s1's former IP",
			svc:  "s2",
			ip:   "1000::4",
		},
		{
			desc: "s1 can now grab s2's former IP",
			svc:  "s1",
			ip:   "1000::4:5",
		},
		{
			desc: "s3 can grab another IP in that pool",
			svc:  "s3",
			ip:   "1000::4:ff",
		},
		{
			desc:       "s4 takes an IP, with sharing",
			svc:        "s4",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "sharing",
		},
		{
			desc:       "s4 changes its sharing key in place",
			svc:        "s4",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can't share with s4 (port conflict)",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong sharing key)",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			wantErr:    true,
		},
		{
			desc:       "s3 takes the same IP as s4",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can change its ports while keeping the same IP",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("udp/53"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can't change its sharing key while keeping the same IP",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			wantErr:    true,
		},
		{
			desc: "s4 takes s3's former IP",
			svc:  "s4",
			ip:   "1000::4:ff",
		},
	}

	for _, test := range tests {
		service := service(test.svc, test.ports, test.sharingKey)
		if test.ip == "" {
			alloc.Unassign(namespacedName(&service))
			continue
		}
		ip := net.ParseIP(test.ip)
		if ip == nil {
			t.Fatalf("invalid IP %q in test %q", test.ip, test.desc)
		}
		service.Spec.LoadBalancerIP = test.ip
		_, err := alloc.allocateSpecificIP(&service)
		if test.wantErr {
			if err == nil {
				t.Errorf("%q should have caused an error, but did not", test.desc)
			}
		} else {
			if err != nil {
				t.Errorf("%q: Assign(%q, %q): %s", test.desc, test.svc, test.ip, err)
			}
		}
	}
}

func TestPoolAllocation(t *testing.T) {
	alloc := New(allocatorTestLogger)
	// This test only allocates from the "test" and "testV6" pools, so
	// it will run out of IPs quickly even though there are tons
	// available in other pools.
	alloc.pools = map[string]Pool{
		"not_this_one": mustLocalPool(t, "192.168.0.0/16"),
		"test":         mustLocalPool(t, "1.2.3.4/30"),
		"testV6":       mustLocalPool(t, "1000::/126"),
		"test2":        mustLocalPool(t, "10.20.30.0/24"),
	}

	validIP4s := map[string]bool{
		"1.2.3.4": true,
		"1.2.3.5": true,
		"1.2.3.6": true,
		"1.2.3.7": true,
	}
	validIP6s := map[string]bool{
		"1000::":  true,
		"1000::1": true,
		"1000::2": true,
		"1000::3": true,
	}

	tests := []struct {
		desc       string
		svc        string
		ports      []v1.ServicePort
		sharingKey string
		unassign   bool
		wantErr    bool
		isIPv6     bool
	}{
		{
			desc: "s1 gets an IP",
			svc:  "s1",
		},
		{
			desc: "s2 gets an IP",
			svc:  "s2",
		},
		{
			desc: "s3 gets an IP",
			svc:  "s3",
		},
		{
			desc: "s4 gets an IP",
			svc:  "s4",
		},
		{
			desc:    "s5 can't get an IP",
			svc:     "s5",
			wantErr: true,
		},
		{
			desc:    "s6 can't get an IP",
			svc:     "s6",
			wantErr: true,
		},
		{
			desc:     "s1 releases its IP",
			svc:      "s1",
			unassign: true,
		},
		{
			desc: "s5 can now grab s1's former IP",
			svc:  "s5",
		},
		{
			desc:    "s6 still can't get an IP",
			svc:     "s6",
			wantErr: true,
		},
		{
			desc:     "s5 unassigns in prep for enabling IP sharing",
			svc:      "s5",
			unassign: true,
		},
		{
			desc:       "s5 enables IP sharing",
			svc:        "s5",
			ports:      ports("tcp/80"),
			sharingKey: "share",
		},
		{
			desc:       "s6 can get an IP now, with sharing",
			svc:        "s6",
			ports:      ports("tcp/443"),
			sharingKey: "share",
		},

		// Clear old ipv4 addresses
		{
			desc:     "s1 clear old ipv4 address",
			svc:      "s1",
			unassign: true,
		},
		{
			desc:     "s2 clear old ipv4 address",
			svc:      "s2",
			unassign: true,
		},
		{
			desc:     "s3 clear old ipv4 address",
			svc:      "s3",
			unassign: true,
		},
		{
			desc:     "s4 clear old ipv4 address",
			svc:      "s4",
			unassign: true,
		},
		{
			desc:     "s5 clear old ipv4 address",
			svc:      "s5",
			unassign: true,
		},
		{
			desc:     "s6 clear old ipv4 address",
			svc:      "s6",
			unassign: true,
		},

		// IPv6 tests.
		{
			desc:   "s1 gets an IP6",
			svc:    "s1",
			isIPv6: true,
		},
		{
			desc:   "s2 gets an IP6",
			svc:    "s2",
			isIPv6: true,
		},
		{
			desc:   "s3 gets an IP6",
			svc:    "s3",
			isIPv6: true,
		},
		{
			desc:   "s4 gets an IP6",
			svc:    "s4",
			isIPv6: true,
		},
		{
			desc:    "s5 can't get an IP6",
			svc:     "s5",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:    "s6 can't get an IP6",
			svc:     "s6",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:     "s1 releases its IP6",
			svc:      "s1",
			unassign: true,
		},
		{
			desc:   "s5 can now grab s1's former IP6",
			svc:    "s5",
			isIPv6: true,
		},
		{
			desc:    "s6 still can't get an IP6",
			svc:     "s6",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:     "s5 unassigns in prep for enabling IP6 sharing",
			svc:      "s5",
			unassign: true,
		},
		{
			desc:       "s5 enables IP6 sharing",
			svc:        "s5",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			isIPv6:     true,
		},
		{
			desc:       "s6 can get an IP6 now, with sharing",
			svc:        "s6",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			isIPv6:     true,
		},

		// Test the "should-not-happen" case where an svc already has a IP from the wrong family
		{
			desc:     "s1 clear",
			svc:      "s1",
			unassign: true,
		},
		{
			desc: "s1 get an IPv4",
			svc:  "s1",
		},
		{
			desc:    "s1 get an IPv6",
			svc:     "s1",
			isIPv6:  true,
			wantErr: true,
		},
	}

	for _, test := range tests {
		service := service(test.svc, test.ports, test.sharingKey)
		if test.unassign {
			alloc.Unassign(namespacedName(&service))
			continue
		}
		pool := "test"
		if test.isIPv6 {
			pool = "testV6"
		}
		err := alloc.allocateFromPool(&service, pool)
		if test.wantErr {
			if err == nil {
				t.Errorf("%s: should have caused an error, but did not", test.desc)

			}
			continue
		}
		if err != nil {
			t.Errorf("%s: AllocateFromPool(%q, \"test\"): %s", test.desc, test.svc, err)
		}
		validIPs := validIP4s
		if test.isIPv6 {
			validIPs = validIP6s
		}
		ip := service.Status.LoadBalancer.Ingress[0].IP
		if !validIPs[ip] {
			t.Errorf("%s: allocated unexpected IP %q", test.desc, ip)
		}
	}

	alloc.Unassign("unit/s5")
	service := service("s5", []v1.ServicePort{}, "")
	if err := alloc.allocateFromPool(&service, "nonexistentpool"); err == nil {
		t.Error("Allocating from non-existent pool succeeded")
	}
}

func TestAllocateAnyIP(t *testing.T) {
	var svc v1.Service

	alloc := New(allocatorTestLogger)
	alloc.pools = map[string]Pool{
		// Start suite with no "default" pool
		"test1V6": mustLocalPool(t, "1000::4/127"),
	}

	// Allocate specific IP succeeds
	svc = service("t1", ports("tcp/80"), "")
	svc.Spec.LoadBalancerIP = "1000::4"
	pool, err := alloc.AllocateAnyIP(&svc)
	assert.Nil(t, err, "specific IP allocation failed")
	assert.Equal(t, "test1V6", pool, "IP allocated from wrong pool")
	assert.Equal(t, "1000::4", svc.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")

	// Allocate specific IP fails if IP and pool disagree
	svc = service("t2", ports("tcp/80"), "")
	svc.Spec.LoadBalancerIP = "1000::4"
	svc.ObjectMeta.Annotations[purelbv1.DesiredGroupAnnotation] = "not test1V6"
	_, err = alloc.AllocateAnyIP(&svc)
	assert.Error(t, err, "specific IP allocation should have failed")

	// Allocate from specific pool succeeds
	svc = service("t3", ports("tcp/80"), "")
	svc.ObjectMeta.Annotations[purelbv1.DesiredGroupAnnotation] = "test1V6"
	pool, err = alloc.AllocateAnyIP(&svc)
	assert.Nil(t, err, "specific IP allocation failed")
	assert.Equal(t, "test1V6", pool, "IP allocated from wrong pool")
	assert.Equal(t, "1000::5", svc.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")

	// Pool is empty so allocation fails
	svc = service("t4", ports("tcp/80"), "")
	svc.ObjectMeta.Annotations[purelbv1.DesiredGroupAnnotation] = "test1V6"
	_, err = alloc.AllocateAnyIP(&svc)
	assert.Error(t, err, "allocation from exhausted pool should have failed")

	// There's no "default" pool so allocation fails if the pool isn't
	// specified
	svc = service("t5", ports("tcp/80"), "")
	_, err = alloc.AllocateAnyIP(&svc)
	assert.Error(t, err, "default pool IP allocation should have failed")

	// Add a "default" pool
	alloc.pools[defaultPoolName] = mustLocalPool(t, "1.2.3.4/30")

	// Now that there's a "default" pool, allocation succeeds
	svc = service("t6", ports("tcp/80"), "")
	pool, err = alloc.AllocateAnyIP(&svc)
	assert.Nil(t, err, "default pool IP allocation failed")
	assert.Equal(t, defaultPoolName, pool, "IP allocated from wrong pool")
	assert.Equal(t, "1.2.3.4", svc.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")
}

func TestPoolMetrics(t *testing.T) {
	alloc := New(allocatorTestLogger)
	alloc.SetClient(&testK8S{t: t})
	alloc.SetPools([]*purelbv1.ServiceGroup{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test",
			},
			Spec: purelbv1.ServiceGroupSpec{
				Local: &purelbv1.ServiceGroupLocalSpec{
					Subnet: "1.2.3.4/30",
					Pool:   "1.2.3.4/30",
				},
			},
		},
	})

	tests := []struct {
		desc       string
		svc        string
		ip         string
		ports      []v1.ServicePort
		sharingKey string
		ipsInUse   float64
	}{
		{
			desc:     "assign s1",
			svc:      "s1",
			ip:       "1.2.3.4",
			ipsInUse: 1,
		},
		{
			desc:     "assign s2",
			svc:      "s2",
			ip:       "1.2.3.5",
			ipsInUse: 2,
		},
		{
			desc:     "unassign s1",
			svc:      "s1",
			ipsInUse: 1,
		},
		{
			desc:     "unassign s2",
			svc:      "s2",
			ipsInUse: 0,
		},
		{
			desc:       "assign s1 shared",
			svc:        "s1",
			ip:         "1.2.3.4",
			sharingKey: "key",
			ports:      ports("tcp/80"),
			ipsInUse:   1,
		},
		{
			desc:       "assign s2 shared",
			svc:        "s2",
			ip:         "1.2.3.4",
			sharingKey: "key",
			ports:      ports("tcp/443"),
			ipsInUse:   1,
		},
		{
			desc:       "assign s3 shared",
			svc:        "s3",
			ip:         "1.2.3.4",
			sharingKey: "key",
			ports:      ports("tcp/23"),
			ipsInUse:   1,
		},
		{
			desc:     "unassign s1 shared",
			svc:      "s1",
			ports:    ports("tcp/80"),
			ipsInUse: 1,
		},
		{
			desc:     "unassign s2 shared",
			svc:      "s2",
			ports:    ports("tcp/443"),
			ipsInUse: 1,
		},
		{
			desc:     "unassign s3 shared",
			svc:      "s3",
			ports:    ports("tcp/23"),
			ipsInUse: 0,
		},
	}

	// The "test" pool contains one range: 1.2.3.4/30
	value := ptu.ToFloat64(poolCapacity.WithLabelValues("test"))
	if int(value) != 4 {
		t.Errorf("stats.poolCapacity invalid %f. Expected 4", value)
	}

	for _, test := range tests {
		service := service(test.svc, test.ports, test.sharingKey)
		if test.ip == "" {
			alloc.Unassign(namespacedName(&service))
			value := ptu.ToFloat64(poolActive.WithLabelValues("test"))
			if value != test.ipsInUse {
				t.Errorf("%v; in-use %v. Expected %v", test.desc, value, test.ipsInUse)
			}
			continue
		}

		service.Spec.LoadBalancerIP = test.ip
		_, err := alloc.AllocateAnyIP(&service)
		if err != nil {
			t.Errorf("%q: Assign(%q, %q): %v", test.desc, test.svc, test.ip, err)
		}
		value := ptu.ToFloat64(poolActive.WithLabelValues("test"))
		if value != test.ipsInUse {
			t.Errorf("%v; in-use %v. Expected %v", test.desc, value, test.ipsInUse)
		}
	}
}

// TestSpecificAddress tests allocations when a specific address is
// requested
func TestSpecificAddress(t *testing.T) {
	alloc := New(allocatorTestLogger)
	alloc.SetClient(&testK8S{t: t})

	groups := []*purelbv1.ServiceGroup{
		{ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
			Spec: purelbv1.ServiceGroupSpec{
				Local: &purelbv1.ServiceGroupLocalSpec{
					Subnet: "1.2.3.0/31",
					Pool:   "1.2.3.0/31",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alternate"},
			Spec: purelbv1.ServiceGroupSpec{
				Local: &purelbv1.ServiceGroupLocalSpec{
					Subnet: "3.2.1.0/31",
					Pool:   "3.2.1.0/31",
				},
			},
		},
	}

	if alloc.SetPools(groups) != nil {
		t.Fatal("SetConfig failed")
	}

	// Fail to allocate a specific address that's not in the default
	// pool
	svc1 := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				purelbv1.DesiredGroupAnnotation: defaultPoolName,
			},
		},
		Spec: v1.ServiceSpec{
			LoadBalancerIP: "1.2.3.8",
		},
	}
	_, err := alloc.AllocateAnyIP(svc1)
	assert.Error(t, err, "address allocated but shouldn't be")

	// Allocate a specific address in the default pool
	svc1.Spec.LoadBalancerIP = "1.2.3.0"
	pool, err := alloc.AllocateAnyIP(svc1)
	assert.Nil(t, err, "error allocating address")
	assert.Equal(t, defaultPoolName, pool, "incorrect pool chosen")
	assert.Equal(t, svc1.Spec.LoadBalancerIP, svc1.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")

}

// TestSharingSimple tests address sharing with no address or pool
// specified. Addresses should come from the "default" pool.
func TestSharingSimple(t *testing.T) {
	const sharing = "sharing-is-caring"
	spec := v1.ServiceSpec{}

	alloc := New(allocatorTestLogger)
	alloc.SetClient(&testK8S{t: t})

	groups := []*purelbv1.ServiceGroup{
		{ObjectMeta: metav1.ObjectMeta{Name: defaultPoolName},
			Spec: purelbv1.ServiceGroupSpec{
				Local: &purelbv1.ServiceGroupLocalSpec{
					Subnet: "1.2.3.0/31",
					Pool:   "1.2.3.0/31",
				},
			},
		},
	}

	if alloc.SetPools(groups) != nil {
		t.Fatal("SetConfig failed")
	}

	svc1 := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc1",
			Annotations: map[string]string{
				purelbv1.DesiredGroupAnnotation: defaultPoolName,
				purelbv1.SharingAnnotation:      sharing,
			},
		},
		Spec: spec,
	}
	pool, err := alloc.AllocateAnyIP(svc1)
	assert.Nil(t, err, "error allocating address")
	assert.Equal(t, defaultPoolName, pool, "incorrect pool chosen")
	assert.Equal(t, "1.2.3.0", svc1.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")

	// Mismatched SharingAnnotation so different address
	svc2 := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc2",
			Annotations: map[string]string{
				purelbv1.DesiredGroupAnnotation: defaultPoolName,
				purelbv1.SharingAnnotation:      "i-really-dont-care-do-u",
			},
		},
		Spec: spec,
	}
	pool, err = alloc.AllocateAnyIP(svc2)
	assert.Nil(t, err, "error allocating address")
	assert.Equal(t, defaultPoolName, pool, "incorrect pool chosen")
	assert.Equal(t, "1.2.3.1", svc2.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")

	// Matching SharingAnnotation so same address as svc1
	svc3 := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc3",
			Annotations: map[string]string{
				purelbv1.DesiredGroupAnnotation: defaultPoolName,
				purelbv1.SharingAnnotation:      sharing,
			},
		},
		Spec: spec,
	}
	pool, err = alloc.AllocateAnyIP(svc3)
	assert.Nil(t, err, "error allocating address")
	assert.Equal(t, defaultPoolName, pool, "incorrect pool chosen")
	assert.Equal(t, "1.2.3.0", svc3.Status.LoadBalancer.Ingress[0].IP, "IP wasn't assigned to service ingress")
}

func TestParseGroups(t *testing.T) {
	tests := []struct {
		desc string
		raw  []*purelbv1.ServiceGroup
		want map[string]Pool
	}{
		{desc: "empty config",
			raw:  []*purelbv1.ServiceGroup{},
			want: map[string]Pool{},
		},

		{desc: "config using all features",
			raw: []*purelbv1.ServiceGroup{
				localServiceGroup("pool1", "10.20.0.0/16"),
				localServiceGroup("pool2", "30.0.0.0/8"),
				localServiceGroup("pool3", "40.0.0.0/25"),
				localServiceGroup("pool4", "2001:db8::/126"),
			},
			want: map[string]Pool{
				"pool1": mustLocalPool(t, "10.20.0.0/16"),
				"pool2": mustLocalPool(t, "30.0.0.0/8"),
				"pool3": mustLocalPool(t, "40.0.0.0/25"),
				"pool4": mustLocalPool(t, "2001:db8::/126"),
			},
		},

		{desc: "invalid CIDR",
			raw: []*purelbv1.ServiceGroup{
				localServiceGroup("pool1", "100.200.300.400/24"),
			},
			want: map[string]Pool{},
		},

		{desc: "invalid CIDR prefix length",
			raw: []*purelbv1.ServiceGroup{
				localServiceGroup("pool1", "1.2.3.0/33"),
			},
			want: map[string]Pool{},
		},

		{desc: "duplicate group name",
			raw: []*purelbv1.ServiceGroup{
				localServiceGroup("pool1", "10.20.0.0/16"),
				localServiceGroup("pool1", "30.0.0.0/8"),
			},
			want: map[string]Pool{
				"pool1": mustLocalPool(t, "10.20.0.0/16"),
			},
		},

		{desc: "duplicate CIDRs",
			raw: []*purelbv1.ServiceGroup{
				localServiceGroup("pool1", "10.0.0.0/8"),
				localServiceGroup("pool2", "10.0.0.0/8"),
			},
			want: map[string]Pool{
				"pool1": mustLocalPool(t, "10.0.0.0/8"),
			},
		},

		{desc: "overlapping CIDRs",
			raw: []*purelbv1.ServiceGroup{
				localServiceGroup("pool1", "10.0.0.0/8"),
				localServiceGroup("pool2", "10.0.0.0/16"),
			},
			want: map[string]Pool{
				"pool1": mustLocalPool(t, "10.0.0.0/8"),
			},
		},
	}

	k := &testK8S{t: t}
	alloc := New(log.NewNopLogger())
	alloc.client = k

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			got := alloc.parseGroups(test.raw)
			if diff := cmp.Diff(test.want, got, purelbv1.IPRangeComparer, cmp.AllowUnexported(LocalPool{})); diff != "" {
				t.Errorf("%q: parse returned wrong result (-want, +got)\n%s", test.desc, diff)
			}
		})
	}
}

// Some helpers

func ports(ports ...string) []v1.ServicePort {
	var ret []v1.ServicePort
	for _, s := range ports {
		fs := strings.Split(s, "/")
		p, err := strconv.Atoi(fs[1])
		if err != nil {
			panic("bad port in test")
		}
		proto := v1.ProtocolTCP
		if fs[0] == "udp" {
			proto = v1.ProtocolUDP
		}
		ret = append(ret, v1.ServicePort{Protocol: proto, Port: int32(p)})
	}
	return ret
}

func localServiceGroup(name string, pool string) *purelbv1.ServiceGroup {
	return serviceGroup(name, purelbv1.ServiceGroupSpec{
		Local: &purelbv1.ServiceGroupLocalSpec{Pool: pool, Subnet: pool},
	})
}

func serviceGroup(name string, spec purelbv1.ServiceGroupSpec) *purelbv1.ServiceGroup {
	return &purelbv1.ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: spec,
	}
}

func service(name string, ports []v1.ServicePort, sharingKey string) v1.Service {
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "unit",
			Name:        name,
			Annotations: map[string]string{},
		},
		Spec: v1.ServiceSpec{Ports: ports},
	}

	if sharingKey != "" {
		service.Annotations[purelbv1.SharingAnnotation] = sharingKey
	}

	return service
}
