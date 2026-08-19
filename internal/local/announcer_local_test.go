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

package local

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/mdlayher/ndp"
	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func intPtr(i int) *int {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}

func TestRenewalKey(t *testing.T) {
	tests := []struct {
		name     string
		svcName  string
		ip       string
		expected string
	}{
		{
			name:     "basic key",
			svcName:  "default/my-service",
			ip:       "192.168.1.100",
			expected: "default/my-service:192.168.1.100",
		},
		{
			name:     "ipv6 address",
			svcName:  "kube-system/lb-svc",
			ip:       "2001:db8::1",
			expected: "kube-system/lb-svc:2001:db8::1",
		},
		{
			name:     "empty namespace",
			svcName:  "/service",
			ip:       "10.0.0.1",
			expected: "/service:10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := renewalKey(tt.svcName, tt.ip)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// nopServiceEvent is a k8s.ServiceEvent that discards events, for tests that
// exercise code paths which emit Kubernetes events.
type nopServiceEvent struct{}

func (nopServiceEvent) Infof(runtime.Object, string, string, ...interface{})  {}
func (nopServiceEvent) Errorf(runtime.Object, string, string, ...interface{}) {}
func (nopServiceEvent) ForceSync()                                            {}

func lbSvc(ips ...string) *v1.Service {
	svc := &v1.Service{}
	for _, ip := range ips {
		svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress,
			v1.LoadBalancerIngress{IP: ip})
	}
	return svc
}

func TestClaimAnnounceSlot(t *testing.T) {
	const key = "purelb.io/announcing-IPv4"

	t.Run("nil annotations does not panic and creates the slot", func(t *testing.T) {
		a := &announcer{myNode: "node-a", client: nopServiceEvent{}}
		svc := lbSvc("192.0.2.10")
		a.claimAnnounceSlot(svc, "eth0", net.ParseIP("192.0.2.10"))
		assert.Equal(t, "node-a,eth0,192.0.2.10", svc.Annotations[key])
	})

	t.Run("taking the slot from another node replaces it and counts a steal", func(t *testing.T) {
		before := ptu.ToFloat64(announceSlotSteals.WithLabelValues("node-b", "node-a"))
		a := &announcer{myNode: "node-a", client: nopServiceEvent{}}
		svc := lbSvc("192.0.2.10")
		svc.Annotations = map[string]string{key: "node-b,eth0,192.0.2.10"}
		a.claimAnnounceSlot(svc, "eth0", net.ParseIP("192.0.2.10"))
		assert.Equal(t, "node-a,eth0,192.0.2.10", svc.Annotations[key])
		assert.Equal(t, before+1, ptu.ToFloat64(announceSlotSteals.WithLabelValues("node-b", "node-a")))
	})

	t.Run("multi-pool keeps another node's slot for a different live IP", func(t *testing.T) {
		a := &announcer{myNode: "node-a", client: nopServiceEvent{}}
		svc := lbSvc("192.0.2.10", "192.0.2.11")
		svc.Annotations = map[string]string{key: "node-b,eth0,192.0.2.11"}
		a.claimAnnounceSlot(svc, "eth0", net.ParseIP("192.0.2.10"))
		assert.Equal(t, "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11", svc.Annotations[key])
	})

	t.Run("re-claiming an owned slot makes no change (idempotent)", func(t *testing.T) {
		a := &announcer{myNode: "node-a", client: nopServiceEvent{}}
		svc := lbSvc("192.0.2.10")
		svc.Annotations = map[string]string{key: "node-a,eth0,192.0.2.10"}
		a.claimAnnounceSlot(svc, "eth0", net.ParseIP("192.0.2.10"))
		assert.Equal(t, "node-a,eth0,192.0.2.10", svc.Annotations[key])
	})
}

func TestClearOwnAnnounceSlot(t *testing.T) {
	const key = "purelb.io/announcing-IPv4"

	t.Run("removes only this node's slot", func(t *testing.T) {
		a := &announcer{myNode: "node-a"}
		svc := lbSvc("192.0.2.10", "192.0.2.11")
		svc.Annotations = map[string]string{key: "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11"}
		a.clearOwnAnnounceSlot(svc, net.ParseIP("192.0.2.10"))
		assert.Equal(t, "node-b,eth0,192.0.2.11", svc.Annotations[key])
	})

	t.Run("deletes the key when our slot was the only one", func(t *testing.T) {
		a := &announcer{myNode: "node-a"}
		svc := lbSvc("192.0.2.10")
		svc.Annotations = map[string]string{key: "node-a,eth0,192.0.2.10"}
		a.clearOwnAnnounceSlot(svc, net.ParseIP("192.0.2.10"))
		_, present := svc.Annotations[key]
		assert.False(t, present, "empty annotation key should be deleted, not left blank")
	})

	t.Run("no slot for us is a no-op", func(t *testing.T) {
		a := &announcer{myNode: "node-a"}
		svc := lbSvc("192.0.2.10")
		svc.Annotations = map[string]string{key: "node-b,eth0,192.0.2.10"}
		a.clearOwnAnnounceSlot(svc, net.ParseIP("192.0.2.10"))
		assert.Equal(t, "node-b,eth0,192.0.2.10", svc.Annotations[key])
	})

	t.Run("nil annotations does not panic", func(t *testing.T) {
		a := &announcer{myNode: "node-a"}
		a.clearOwnAnnounceSlot(lbSvc("192.0.2.10"), net.ParseIP("192.0.2.10"))
	})
}

// TestOtherAnnouncerOf covers the reference count that decides whether a
// shared IP may be withdrawn. The bug this replaces counted entries in
// svcIngresses, which holds every LoadBalancer Service the node has seen
// whether or not it won the election, so two services sharing an IP each
// found the other and both declined to withdraw -- leaving the address on a
// node that had lost the election while the winner also announced it.
func TestOtherAnnouncerOf(t *testing.T) {
	const shared = "192.168.1.100"

	tests := []struct {
		name      string
		announced []string // keys already in a.announced
		addr      string
		wantOther string
		wantInUse bool
	}{
		{
			name:      "nothing announced",
			announced: nil,
			addr:      shared,
			wantInUse: false,
		},
		{
			name:      "only this service announced it, and it was already removed",
			announced: []string{},
			addr:      shared,
			wantInUse: false,
		},
		{
			name:      "another service on this node still announces it",
			announced: []string{"ns/other:" + shared},
			addr:      shared,
			wantOther: "ns/other",
			wantInUse: true,
		},
		{
			name:      "other service announces a different address",
			announced: []string{"ns/other:192.168.1.101"},
			addr:      shared,
			wantInUse: false,
		},
		{
			name:      "ipv6 shared address splits on the first colon",
			announced: []string{"ns/other:2001:db8::1"},
			addr:      "2001:db8::1",
			wantOther: "ns/other",
			wantInUse: true,
		},
		{
			name:      "ipv6 near-miss is not a match",
			announced: []string{"ns/other:2001:db8::11"},
			addr:      "2001:db8::1",
			wantInUse: false,
		},
		{
			name:      "address that is a suffix of another is not a match",
			announced: []string{"ns/other:110.0.0.1"},
			addr:      "10.0.0.1",
			wantInUse: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &announcer{}
			for _, k := range tt.announced {
				a.announced.Store(k, struct{}{})
			}

			other, inUse := a.otherAnnouncerOf(net.ParseIP(tt.addr))

			assert.Equal(t, tt.wantInUse, inUse)
			if tt.wantInUse {
				assert.Equal(t, tt.wantOther, other)
			}
		})
	}
}

// A Service this node knows about but never announced must not block
// withdrawal. This is the exact shape of the reproduced bug: both services
// sharing the IP are present in svcIngresses, but only the election winner
// holds an entry in announced.
func TestOtherAnnouncerOf_IgnoresUnannouncedServices(t *testing.T) {
	const shared = "192.168.1.100"

	a := &announcer{
		// Both services are known to this node...
		svcIngresses: map[string][]v1.LoadBalancerIngress{
			"ns/svc-a": {{IP: shared}},
			"ns/svc-b": {{IP: shared}},
		},
	}
	// ...but this node announced neither (it lost the election for both).

	other, inUse := a.otherAnnouncerOf(net.ParseIP(shared))
	assert.False(t, inUse, "a service this node never announced must not hold a reference")
	assert.Empty(t, other)
}

func TestGetLocalAddressOptions_Defaults(t *testing.T) {
	// nil snapshot == not configured; the helper must still yield defaults.
	opts := getLocalAddressOptions(nil)

	assert.Equal(t, 300, opts.ValidLft, "default ValidLft should be 300")
	assert.Equal(t, 300, opts.PreferedLft, "default PreferedLft should be 300")
	assert.True(t, opts.NoPrefixRoute, "default NoPrefixRoute should be true")
}

func TestGetLocalAddressOptions_WithConfig(t *testing.T) {
	tests := []struct {
		name              string
		validLifetime     *int
		preferredLifetime *int
		noPrefixRoute     *bool
		expectedValid     int
		expectedPreferred int
		expectedNoPrefix  bool
	}{
		{
			name:              "explicit values",
			validLifetime:     intPtr(600),
			preferredLifetime: intPtr(300),
			noPrefixRoute:     boolPtr(false),
			expectedValid:     600,
			expectedPreferred: 300,
			expectedNoPrefix:  false,
		},
		{
			name:              "permanent (zero lifetime)",
			validLifetime:     intPtr(0),
			preferredLifetime: intPtr(0),
			noPrefixRoute:     boolPtr(true),
			expectedValid:     0,
			expectedPreferred: 0,
			expectedNoPrefix:  true,
		},
		{
			name:              "minimum lifetime enforcement",
			validLifetime:     intPtr(30), // Below 60s minimum
			preferredLifetime: nil,
			noPrefixRoute:     nil,
			expectedValid:     60, // Should be clamped to 60
			expectedPreferred: 60, // Should match valid
			expectedNoPrefix:  true,
		},
		{
			name:              "preferred capped to valid",
			validLifetime:     intPtr(120),
			preferredLifetime: intPtr(300), // Greater than valid
			noPrefixRoute:     nil,
			expectedValid:     120,
			expectedPreferred: 120, // Should be capped to valid
			expectedNoPrefix:  true,
		},
		{
			name:              "only valid lifetime set",
			validLifetime:     intPtr(180),
			preferredLifetime: nil,
			noPrefixRoute:     nil,
			expectedValid:     180,
			expectedPreferred: 180, // Should default to valid
			expectedNoPrefix:  true,
		},
		{
			name:              "edge case: exactly 60s",
			validLifetime:     intPtr(60),
			preferredLifetime: intPtr(60),
			noPrefixRoute:     boolPtr(true),
			expectedValid:     60,
			expectedPreferred: 60,
			expectedNoPrefix:  true,
		},
		{
			name:              "edge case: 59s should clamp to 60s",
			validLifetime:     intPtr(59),
			preferredLifetime: nil,
			noPrefixRoute:     nil,
			expectedValid:     60,
			expectedPreferred: 60,
			expectedNoPrefix:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &announcerConfig{
				spec: &purelbv2.LBNodeAgentLocalSpec{
					AddressConfig: &purelbv2.AddressConfig{
						LocalInterface: &purelbv2.InterfaceAddressConfig{
							ValidLifetime:     tt.validLifetime,
							PreferredLifetime: tt.preferredLifetime,
							NoPrefixRoute:     tt.noPrefixRoute,
						},
					},
				},
			}

			opts := getLocalAddressOptions(c)

			assert.Equal(t, tt.expectedValid, opts.ValidLft, "ValidLft mismatch")
			assert.Equal(t, tt.expectedPreferred, opts.PreferedLft, "PreferedLft mismatch")
			assert.Equal(t, tt.expectedNoPrefix, opts.NoPrefixRoute, "NoPrefixRoute mismatch")
		})
	}
}

func TestGetDummyAddressOptions_Defaults(t *testing.T) {
	// nil snapshot == not configured; the helper must still yield defaults.
	opts := getDummyAddressOptions(nil)

	assert.Equal(t, 0, opts.ValidLft, "default ValidLft should be 0 (permanent)")
	assert.Equal(t, 0, opts.PreferedLft, "default PreferedLft should be 0 (permanent)")
	assert.False(t, opts.NoPrefixRoute, "default NoPrefixRoute should be false")
}

func TestGetDummyAddressOptions_WithConfig(t *testing.T) {
	tests := []struct {
		name              string
		validLifetime     *int
		preferredLifetime *int
		noPrefixRoute     *bool
		expectedValid     int
		expectedPreferred int
		expectedNoPrefix  bool
	}{
		{
			name:              "explicit finite values",
			validLifetime:     intPtr(300),
			preferredLifetime: intPtr(150),
			noPrefixRoute:     boolPtr(true),
			expectedValid:     300,
			expectedPreferred: 150,
			expectedNoPrefix:  true,
		},
		{
			name:              "minimum lifetime enforcement",
			validLifetime:     intPtr(10), // Below 60s minimum
			preferredLifetime: nil,
			noPrefixRoute:     nil,
			expectedValid:     60, // Should be clamped to 60
			expectedPreferred: 60, // Should match valid
			expectedNoPrefix:  false,
		},
		{
			name:              "preferred capped to valid",
			validLifetime:     intPtr(100),
			preferredLifetime: intPtr(200), // Greater than valid
			noPrefixRoute:     nil,
			expectedValid:     100,
			expectedPreferred: 100, // Should be capped to valid
			expectedNoPrefix:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &announcerConfig{
				spec: &purelbv2.LBNodeAgentLocalSpec{
					AddressConfig: &purelbv2.AddressConfig{
						DummyInterface: &purelbv2.InterfaceAddressConfig{
							ValidLifetime:     tt.validLifetime,
							PreferredLifetime: tt.preferredLifetime,
							NoPrefixRoute:     tt.noPrefixRoute,
						},
					},
				},
			}

			opts := getDummyAddressOptions(c)

			assert.Equal(t, tt.expectedValid, opts.ValidLft, "ValidLft mismatch")
			assert.Equal(t, tt.expectedPreferred, opts.PreferedLft, "PreferedLft mismatch")
			assert.Equal(t, tt.expectedNoPrefix, opts.NoPrefixRoute, "NoPrefixRoute mismatch")
		})
	}
}

func TestGetAddressOptions_NilConfigLevels(t *testing.T) {
	// Test various levels of nil config to ensure no panics

	tests := []struct {
		name   string
		config *purelbv2.LBNodeAgentLocalSpec
	}{
		{
			name:   "nil config",
			config: nil,
		},
		{
			name:   "nil AddressConfig",
			config: &purelbv2.LBNodeAgentLocalSpec{},
		},
		{
			name: "nil LocalInterface",
			config: &purelbv2.LBNodeAgentLocalSpec{
				AddressConfig: &purelbv2.AddressConfig{},
			},
		},
		{
			name: "nil DummyInterface",
			config: &purelbv2.LBNodeAgentLocalSpec{
				AddressConfig: &purelbv2.AddressConfig{},
			},
		},
	}

	// A nil spec never appears in a published snapshot -- "not configured"
	// is a nil *announcerConfig -- so that case maps to a nil snapshot.
	snapshot := func(spec *purelbv2.LBNodeAgentLocalSpec) *announcerConfig {
		if spec == nil {
			return nil
		}
		return &announcerConfig{spec: spec}
	}

	for _, tt := range tests {
		t.Run(tt.name+" local", func(t *testing.T) {
			// Should not panic
			opts := getLocalAddressOptions(snapshot(tt.config))
			assert.Equal(t, 300, opts.ValidLft, "default ValidLft")
		})

		t.Run(tt.name+" dummy", func(t *testing.T) {
			// Should not panic
			opts := getDummyAddressOptions(snapshot(tt.config))
			assert.Equal(t, 0, opts.ValidLft, "default ValidLft")
		})
	}
}

func TestScheduleRenewal_PermanentAddress(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
	}

	lbIPNet := net.IPNet{
		IP:   net.ParseIP("192.168.1.100"),
		Mask: net.CIDRMask(24, 32),
	}

	// Permanent address (ValidLft=0) should not schedule a renewal
	opts := AddressOptions{ValidLft: 0, PreferedLft: 0}
	a.scheduleRenewal("default/test-svc", lbIPNet, nil, opts)

	// Verify no renewal was scheduled
	key := renewalKey("default/test-svc", "192.168.1.100")
	_, exists := a.addressRenewals.Load(key)
	assert.False(t, exists, "permanent address should not have renewal scheduled")
}

func TestScheduleRenewal_FiniteLifetime(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
	}

	lbIPNet := net.IPNet{
		IP:   net.ParseIP("192.168.1.100"),
		Mask: net.CIDRMask(24, 32),
	}

	// Finite lifetime should schedule a renewal
	opts := AddressOptions{ValidLft: 300, PreferedLft: 300}
	a.scheduleRenewal("default/test-svc", lbIPNet, nil, opts)

	// Verify renewal was scheduled
	key := renewalKey("default/test-svc", "192.168.1.100")
	val, exists := a.addressRenewals.Load(key)
	assert.True(t, exists, "finite lifetime should have renewal scheduled")

	renewal := val.(*addressRenewal)
	assert.Equal(t, 150*time.Second, renewal.interval, "renewal interval should be 50% of lifetime")

	// Clean up timer
	renewal.stopTimer()
}

func TestScheduleRenewal_MinimumInterval(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
	}

	lbIPNet := net.IPNet{
		IP:   net.ParseIP("192.168.1.100"),
		Mask: net.CIDRMask(24, 32),
	}

	// Very short lifetime should still have minimum 30s interval
	opts := AddressOptions{ValidLft: 60, PreferedLft: 60}
	a.scheduleRenewal("default/test-svc", lbIPNet, nil, opts)

	key := renewalKey("default/test-svc", "192.168.1.100")
	val, exists := a.addressRenewals.Load(key)
	assert.True(t, exists, "renewal should be scheduled")

	renewal := val.(*addressRenewal)
	// 60/2 = 30s, which equals the minimum
	assert.Equal(t, 30*time.Second, renewal.interval, "renewal interval should be at minimum 30s")

	// Clean up timer
	renewal.stopTimer()
}

func TestScheduleRenewal_ReplacesExisting(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
	}

	lbIPNet := net.IPNet{
		IP:   net.ParseIP("192.168.1.100"),
		Mask: net.CIDRMask(24, 32),
	}

	// Schedule first renewal
	opts1 := AddressOptions{ValidLft: 300, PreferedLft: 300}
	a.scheduleRenewal("default/test-svc", lbIPNet, nil, opts1)

	key := renewalKey("default/test-svc", "192.168.1.100")
	val1, _ := a.addressRenewals.Load(key)
	renewal1 := val1.(*addressRenewal)
	interval1 := renewal1.interval

	// Schedule second renewal with different options - should replace
	opts2 := AddressOptions{ValidLft: 600, PreferedLft: 600}
	a.scheduleRenewal("default/test-svc", lbIPNet, nil, opts2)

	val2, exists := a.addressRenewals.Load(key)
	assert.True(t, exists, "renewal should still exist")

	renewal2 := val2.(*addressRenewal)
	assert.Equal(t, 300*time.Second, renewal2.interval, "new renewal should have updated interval")
	assert.NotEqual(t, interval1, renewal2.interval, "interval should be different after replacement")

	// Clean up timer
	renewal2.stopTimer()
}

func TestCancelRenewal(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
	}

	lbIPNet := net.IPNet{
		IP:   net.ParseIP("192.168.1.100"),
		Mask: net.CIDRMask(24, 32),
	}

	// Schedule a renewal
	opts := AddressOptions{ValidLft: 300, PreferedLft: 300}
	a.scheduleRenewal("default/test-svc", lbIPNet, nil, opts)

	key := renewalKey("default/test-svc", "192.168.1.100")
	_, exists := a.addressRenewals.Load(key)
	assert.True(t, exists, "renewal should be scheduled before cancel")

	// Cancel the renewal
	a.cancelRenewal("default/test-svc", "192.168.1.100")

	_, exists = a.addressRenewals.Load(key)
	assert.False(t, exists, "renewal should be removed after cancel")
}

func TestCancelRenewal_NonExistent(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
	}

	// Should not panic when canceling non-existent renewal
	a.cancelRenewal("default/nonexistent", "192.168.1.100")
}

func TestScheduleRenewal_ConcurrentAccess(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
	}

	var wg sync.WaitGroup
	numGoroutines := 10

	// Concurrently schedule and cancel renewals
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			ip := net.ParseIP("192.168.1.100")
			lbIPNet := net.IPNet{IP: ip, Mask: net.CIDRMask(24, 32)}
			opts := AddressOptions{ValidLft: 300, PreferedLft: 300}

			// Alternate between scheduling and canceling
			if idx%2 == 0 {
				a.scheduleRenewal("default/test-svc", lbIPNet, nil, opts)
			} else {
				a.cancelRenewal("default/test-svc", "192.168.1.100")
			}
		}(i)
	}

	wg.Wait()

	// Clean up any remaining timers
	a.addressRenewals.Range(func(key, val interface{}) bool {
		val.(*addressRenewal).stopTimer()
		return true
	})
}

func TestAddressOptions_Struct(t *testing.T) {
	// Test that AddressOptions struct works correctly
	opts := AddressOptions{
		ValidLft:      300,
		PreferedLft:   150,
		NoPrefixRoute: true,
	}

	assert.Equal(t, 300, opts.ValidLft)
	assert.Equal(t, 150, opts.PreferedLft)
	assert.True(t, opts.NoPrefixRoute)
	assert.False(t, opts.SkipDAD, "SkipDAD should default to false")
}

func TestAddressOptions_SkipDAD(t *testing.T) {
	opts := AddressOptions{
		ValidLft:      300,
		PreferedLft:   150,
		NoPrefixRoute: true,
		SkipDAD:       true,
	}

	assert.True(t, opts.SkipDAD, "SkipDAD should be true when explicitly set")
	assert.Equal(t, 300, opts.ValidLft, "other fields should be unaffected")
	assert.True(t, opts.NoPrefixRoute, "other fields should be unaffected")
}

func TestSkipDADFromServiceAnnotation(t *testing.T) {
	// This tests the annotation-reading pattern used in announceLocal():
	//   opts := a.getLocalAddressOptions()
	//   if svc.Annotations[purelbv2.SkipIPv6DADAnnotation] == "true" {
	//       opts.SkipDAD = true
	//   }

	tests := []struct {
		name        string
		annotations map[string]string
		wantSkipDAD bool
	}{
		{
			name:        "no annotation",
			annotations: map[string]string{},
			wantSkipDAD: false,
		},
		{
			name:        "annotation set to true",
			annotations: map[string]string{purelbv2.SkipIPv6DADAnnotation: "true"},
			wantSkipDAD: true,
		},
		{
			name:        "annotation set to false",
			annotations: map[string]string{purelbv2.SkipIPv6DADAnnotation: "false"},
			wantSkipDAD: false,
		},
		{
			name:        "annotation absent among other annotations",
			annotations: map[string]string{"purelb.io/allocated-by": "PureLB"},
			wantSkipDAD: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start with default options (as getLocalAddressOptions would return)
			opts := AddressOptions{
				ValidLft:      300,
				PreferedLft:   300,
				NoPrefixRoute: true,
			}

			// Apply the annotation-reading logic from announceLocal
			if tt.annotations[purelbv2.SkipIPv6DADAnnotation] == "true" {
				opts.SkipDAD = true
			}

			assert.Equal(t, tt.wantSkipDAD, opts.SkipDAD)
		})
	}
}

// =================================================================
// v0.17.0: opt-in app-affinity election (buildPreferredNodes)
// =================================================================

// ptrBool returns &b. Required because EndpointConditions fields are pointers.
func ptrBool(b bool) *bool    { return &b }
func ptrStr(s string) *string { return &s }

// makeSlice builds a single-slice EndpointSlice list for tests. Each
// entry is (nodeName, ready, serving). nil nodeName encodes "no
// NodeName" — the helper must skip those.
func makeSlice(entries ...struct {
	nodeName *string
	ready    bool
	serving  bool
}) []*discoveryv1.EndpointSlice {
	eps := make([]discoveryv1.Endpoint, 0, len(entries))
	for _, e := range entries {
		eps = append(eps, discoveryv1.Endpoint{
			NodeName: e.nodeName,
			Conditions: discoveryv1.EndpointConditions{
				Ready:   ptrBool(e.ready),
				Serving: ptrBool(e.serving),
			},
		})
	}
	return []*discoveryv1.EndpointSlice{{Endpoints: eps}}
}

func TestBuildPreferredNodes(t *testing.T) {
	withAnnot := func(annot, sharingKey string) *v1.Service {
		s := &v1.Service{}
		s.Annotations = map[string]string{}
		if annot != "" {
			s.Annotations[purelbv2.NodeAffinityAnnotation] = annot
		}
		if sharingKey != "" {
			s.Annotations[purelbv2.SharingAnnotation] = sharingKey
		}
		return s
	}

	type entry = struct {
		nodeName *string
		ready    bool
		serving  bool
	}

	t.Run("no annotation → returns nil", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot("", ""), makeSlice(entry{ptrStr("a"), true, true}))
		assert.Nil(t, got)
	})

	t.Run("unknown annotation value → returns nil", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot("topology-zone", ""), makeSlice(entry{ptrStr("a"), true, true}))
		assert.Nil(t, got, "future-mode annotation values must opt out (treat as not-set)")
	})

	t.Run("annotation + shared-IP → returns nil (sharing exclusion)", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, "shared-1"),
			makeSlice(entry{ptrStr("a"), true, true}))
		assert.Nil(t, got, "sharing exclusion must fire even with valid annotation")
	})

	t.Run("annotation set, two Ready endpoints on different nodes → both included", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, ""),
			makeSlice(entry{ptrStr("a"), true, true}, entry{ptrStr("b"), true, true}))
		assert.ElementsMatch(t, []string{"a", "b"}, got)
	})

	t.Run("nil NodeName endpoint → skipped", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, ""),
			makeSlice(entry{nil, true, true}, entry{ptrStr("b"), true, true}))
		assert.Equal(t, []string{"b"}, got)
	})

	t.Run("Ready=false, Serving=true → INCLUDED (graceful drain)", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, ""),
			makeSlice(entry{ptrStr("draining"), false, true}))
		assert.Equal(t, []string{"draining"}, got,
			"Serving=true endpoints (terminating but draining) must stay preferred")
	})

	t.Run("Ready=false, Serving=false → skipped", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, ""),
			makeSlice(entry{ptrStr("dead"), false, false}))
		assert.Empty(t, got)
	})

	t.Run("duplicate node across endpoints → deduplicated", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, ""),
			makeSlice(entry{ptrStr("a"), true, true}, entry{ptrStr("a"), true, true}))
		assert.Equal(t, []string{"a"}, got)
	})

	t.Run("nil slice in input list → tolerated (no panic, skipped)", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, ""),
			[]*discoveryv1.EndpointSlice{nil, makeSlice(entry{ptrStr("a"), true, true})[0]})
		assert.Equal(t, []string{"a"}, got)
	})

	t.Run("empty endpoint slice list → empty preferred (NOT nil)", func(t *testing.T) {
		got := buildPreferredNodes(withAnnot(purelbv2.NodeAffinityServiceEndpoints, ""),
			[]*discoveryv1.EndpointSlice{})
		// Returns nil from append-loop never running; this is treated as
		// "opted-in but no endpoints exist" — WinnerWithPreference handles
		// it via the !len(preferred)>0 branch, falling through to standard
		// election (no fallback metric since len(preferred)==0).
		assert.Empty(t, got)
	})
}

func TestCompileLocalInterface(t *testing.T) {
	t.Run("default yields no regex", func(t *testing.T) {
		re, err := compileLocalInterface(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"})
		assert.NoError(t, err)
		assert.Nil(t, re)
	})

	t.Run("empty treated as default", func(t *testing.T) {
		re, err := compileLocalInterface(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: ""})
		assert.NoError(t, err)
		assert.Nil(t, re)
	})

	t.Run("regex compiled", func(t *testing.T) {
		re, err := compileLocalInterface(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "^(eth1|eth2)$"})
		assert.NoError(t, err)
		if assert.NotNil(t, re) {
			assert.True(t, re.MatchString("eth1"))
			assert.False(t, re.MatchString("eth10"))
		}
	})

	t.Run("invalid regex errors", func(t *testing.T) {
		re, err := compileLocalInterface(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "["})
		assert.Error(t, err)
		assert.Nil(t, re)
	})
}

func TestFindLocal(t *testing.T) {
	t.Run("resolves loopback address", func(t *testing.T) {
		ipnet, link, err := findLocal(regexp.MustCompile("^lo$"), "", net.ParseIP("127.0.0.1"))
		assert.NoError(t, err)
		assert.Equal(t, "lo", link.Attrs().Name)
		assert.Equal(t, "127.0.0.1/8", ipnet.String())
	})

	t.Run("exclude removes the only match", func(t *testing.T) {
		_, _, err := findLocal(regexp.MustCompile("^lo$"), "lo", net.ParseIP("127.0.0.1"))
		assert.Error(t, err)
	})

	t.Run("no matching interface", func(t *testing.T) {
		_, _, err := findLocal(regexp.MustCompile("^purelb-nomatch$"), "", net.ParseIP("127.0.0.1"))
		assert.Error(t, err)
		assert.False(t, errors.Is(err, errIndeterminate))
	})
}

func TestTryExtraInterfaces(t *testing.T) {
	loV6 := func() bool {
		lo, err := net.InterfaceByName("lo")
		if err != nil {
			return false
		}
		addrs, _ := lo.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() == nil && ipnet.IP.IsLoopback() {
				return true
			}
		}
		return false
	}

	a := &announcer{logger: log.NewNopLogger()}
	// tryExtraInterfaces reads its spec from the snapshot it is handed, not
	// from announcer state, so each case just builds the snapshot it wants.
	cfgFor := func(spec *purelbv2.LBNodeAgentLocalSpec) *announcerConfig {
		return &announcerConfig{spec: spec}
	}

	t.Run("resolves IPv4 on explicit interface", func(t *testing.T) {
		ipnet, link, err := a.tryExtraInterfaces(
			cfgFor(&purelbv2.LBNodeAgentLocalSpec{Interfaces: []string{"lo"}}), net.ParseIP("127.0.0.1"))
		assert.NoError(t, err)
		assert.Equal(t, "lo", link.Attrs().Name)
		assert.Equal(t, "127.0.0.1/8", ipnet.String())
	})

	t.Run("resolves IPv6 on explicit interface", func(t *testing.T) {
		if !loV6() {
			t.Skip("loopback has no IPv6 address")
		}
		_, link, err := a.tryExtraInterfaces(
			cfgFor(&purelbv2.LBNodeAgentLocalSpec{Interfaces: []string{"lo"}}), net.ParseIP("::1"))
		assert.NoError(t, err)
		assert.Equal(t, "lo", link.Attrs().Name)
	})

	t.Run("dummy interface name skipped", func(t *testing.T) {
		_, _, err := a.tryExtraInterfaces(
			cfgFor(&purelbv2.LBNodeAgentLocalSpec{Interfaces: []string{"lo"}, DummyInterface: "lo"}),
			net.ParseIP("127.0.0.1"))
		assert.Error(t, err)
	})

	t.Run("missing interface skipped without error abort", func(t *testing.T) {
		_, link, err := a.tryExtraInterfaces(
			cfgFor(&purelbv2.LBNodeAgentLocalSpec{Interfaces: []string{"purelb-does-not-exist0", "lo"}}),
			net.ParseIP("127.0.0.1"))
		assert.NoError(t, err)
		assert.Equal(t, "lo", link.Attrs().Name)
	})

	t.Run("no candidate matches", func(t *testing.T) {
		_, _, err := a.tryExtraInterfaces(
			cfgFor(&purelbv2.LBNodeAgentLocalSpec{Interfaces: []string{"purelb-does-not-exist0"}}),
			net.ParseIP("127.0.0.1"))
		assert.Error(t, err)
		assert.False(t, errors.Is(err, errIndeterminate))
	})
}

// withdrawalTestAnnouncer builds an announcer that has "announced"
// TEST-NET addresses (192.0.2.0/24 is never on a real interface, so
// deleteAddr's scan is read-only and removes nothing) with a live
// renewal timer, ready to be driven into a withdrawal path.
// A nil config means "not configured" and is published as a nil snapshot,
// which is what nodeSelector deselection and config removal produce.
// Otherwise the snapshot is assembled exactly as SetConfig would, regex
// included, so these tests exercise the same shape the real path publishes.
func withdrawalTestAnnouncer(config *purelbv2.LBNodeAgentLocalSpec) *announcer {
	a := &announcer{
		logger:       log.NewNopLogger(),
		myNode:       "node-a",
		client:       nopServiceEvent{},
		svcIngresses: map[string][]v1.LoadBalancerIngress{},
	}
	if config != nil {
		regex, err := compileLocalInterface(config)
		if err != nil {
			panic(err) // test fixture: a bad regex here is a test bug
		}
		a.cfg.Store(&announcerConfig{
			spec:           config,
			groups:         map[string]*purelbv2.ServiceGroupLocalSpec{},
			remoteGroups:   map[string]*purelbv2.ServiceGroupRemoteSpec{},
			localNameRegex: regex,
		})
	}
	return a
}

// announceForTest simulates a prior successful announcement of ip for
// nsName: an announced-set entry plus a scheduled renewal timer.
func announceForTest(a *announcer, nsName, ip string) {
	a.announced.Store(renewalKey(nsName, ip), struct{}{})
	lbIP := net.ParseIP(ip)
	a.scheduleRenewal(nsName, net.IPNet{IP: lbIP, Mask: net.CIDRMask(24, 32)}, nil,
		AddressOptions{ValidLft: 300, PreferedLft: 300})
}

func TestSetBalancerWithdrawsOnNoLocalInterface(t *testing.T) {
	const slotKey = "purelb.io/announcing-IPv4"
	// withdrawalTestAnnouncer compiles LocalInterface into the snapshot, so
	// the regex no longer has to be poked in separately.
	a := withdrawalTestAnnouncer(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "^purelb-nomatch$"})

	svc := lbSvc("192.0.2.1")
	svc.Namespace = "default"
	svc.Name = "test-svc"
	svc.Annotations = map[string]string{slotKey: "node-a,eth0,192.0.2.1"}
	announceForTest(a, "default/test-svc", "192.0.2.1")

	assert.NoError(t, a.SetBalancer(svc, nil))

	key := renewalKey("default/test-svc", "192.0.2.1")
	_, announced := a.announced.Load(key)
	assert.False(t, announced, "announced entry must be removed")
	_, timerAlive := a.addressRenewals.Load(key)
	assert.False(t, timerAlive, "renewal timer must be cancelled")
	assert.Empty(t, svc.Annotations[slotKey], "announce slot must be cleared")
}

func TestSetBalancerWithdrawsOnNoConfig(t *testing.T) {
	const slotKey = "purelb.io/announcing-IPv4"
	a := withdrawalTestAnnouncer(nil) // nodeSelector deselection / config removal

	svc := lbSvc("192.0.2.1")
	svc.Namespace = "default"
	svc.Name = "test-svc"
	svc.Annotations = map[string]string{slotKey: "node-a,eth0,192.0.2.1"}
	announceForTest(a, "default/test-svc", "192.0.2.1")

	assert.NoError(t, a.SetBalancer(svc, nil))

	key := renewalKey("default/test-svc", "192.0.2.1")
	_, announced := a.announced.Load(key)
	assert.False(t, announced, "announced entry must be removed")
	_, timerAlive := a.addressRenewals.Load(key)
	assert.False(t, timerAlive, "renewal timer must be cancelled")
	assert.Empty(t, svc.Annotations[slotKey], "announce slot must be cleared")
}

// TestSetConfigStoresNilWhenNoAgentSelectsNode covers the single most
// dangerous line of the snapshot change. SetConfig used to blank the
// config on entry, which implicitly meant "no agent => announce nothing".
// Removing that blanking (it was the race window) means the no-agent path
// has to store nil explicitly; if it ever stops doing so, a node whose
// LBNodeAgent was deleted, or that a nodeSelector deselected, would keep
// announcing on its last config forever.
//
// Reachable without CAP_NET_ADMIN because it returns before
// addDummyInterface.
func TestSetConfigStoresNilWhenNoAgentSelectsNode(t *testing.T) {
	a := withdrawalTestAnnouncer(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "^eth0$"})
	assert.NotNil(t, a.cfg.Load(), "fixture should start configured")

	// No agent has a Local spec: deselected, or the CR was deleted.
	assert.NoError(t, a.SetConfig(&purelbv2.Config{Agents: nil}))
	assert.Nil(t, a.cfg.Load(), "no governing agent must publish a nil snapshot")
}

// TestSetConfigStoresNilOnInvalidRegex checks the other fail-safe path: a
// config the announcer rejects must not leave the previous one live, or
// the node would announce on config the operator believes was replaced.
func TestSetConfigStoresNilOnInvalidRegex(t *testing.T) {
	a := withdrawalTestAnnouncer(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "^eth0$"})
	assert.NotNil(t, a.cfg.Load(), "fixture should start configured")

	err := a.SetConfig(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
		{Spec: purelbv2.LBNodeAgentSpec{Local: &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "["}}},
	}})
	assert.Error(t, err, "an uncompilable localInterface must be rejected")
	assert.Nil(t, a.cfg.Load(), "a rejected config must publish a nil snapshot")
}

// TestSetBalancerConcurrentWithConfigSwap is the regression test for the
// crash that restarted every node agent on the test cluster: SetConfig
// (CR-controller goroutine) publishing config while SetBalancer
// (service-sync goroutine) was mid-announcement.
//
// Before the snapshot change this reproduced two distinct failures --
// a nil dereference of the spec pointer in tryExtraInterfaces, and
// "concurrent map iteration and map write" on groups/remoteGroups, the
// latter a runtime fatal error that no recover() can catch.
//
// The writer is the real SetConfig, not a bare a.cfg.Store: driving the
// production writer is what makes this a regression test rather than a
// test of the fixture. Verified by temporarily restoring the pre-fix
// unsynchronized field, which makes this fail with a DATA RACE whose
// stack is the one seen in the crash-looping pods --
// tryExtraInterfaces <- SetBalancer against SetConfig.
//
// SetConfig ends in addDummyInterface, which needs CAP_NET_ADMIN. Both
// outcomes are useful: unprivileged, it fails and publishes a nil
// snapshot (exercising the fail-safe path); privileged, it publishes a
// real one. The interface is named distinctively and removed on cleanup
// so a root test run leaves nothing behind.
//
// TEST-NET-1 addresses are never present on a real interface, so the
// netlink work the reader drives is read-only.
func TestSetBalancerConcurrentWithConfigSwap(t *testing.T) {
	const tmpDummy = "purelb-race-test0"

	a := withdrawalTestAnnouncer(&purelbv2.LBNodeAgentLocalSpec{
		LocalInterface: "^purelb-nomatch$",
		Interfaces:     []string{"purelb-does-not-exist0", "lo"},
	})

	t.Cleanup(func() {
		if link, err := netlink.LinkByName(tmpDummy); err == nil {
			_ = removeInterface(link)
		}
	})

	// Alternating deliveries: one that selects this node and one that
	// deselects it, so the writer exercises both the publish and the
	// store-nil paths the way a nodeSelector change does.
	configured := &purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
		{Spec: purelbv2.LBNodeAgentSpec{Local: &purelbv2.LBNodeAgentLocalSpec{
			LocalInterface: "^purelb-nomatch$",
			Interfaces:     []string{"purelb-does-not-exist0", "lo"},
			DummyInterface: tmpDummy,
		}}},
		{Spec: purelbv2.LBNodeAgentSpec{Local: nil}},
	}}
	deselected := &purelbv2.Config{Agents: nil}

	const iterations = 500
	stop := make(chan struct{})
	var writer sync.WaitGroup

	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Errors are expected without CAP_NET_ADMIN and are not the
			// point; the point is that the reader never observes a
			// half-applied config.
			if i%4 == 3 {
				_ = a.SetConfig(deselected)
				continue
			}
			_ = a.SetConfig(configured)
		}
	}()

	// Exactly one reader. SetBalancer writes svcIngresses, which is
	// deliberately unsynchronized because production only ever calls it
	// from the single service-sync goroutine; a second reader here would
	// manufacture a map race that cannot happen in the real agent and
	// would test the fixture rather than the fix.
	svc := lbSvc("192.0.2.1")
	svc.Namespace = "default"
	svc.Name = "test-svc"
	for i := 0; i < iterations; i++ {
		// Errors are expected and irrelevant: nothing on this host holds
		// a TEST-NET address. The assertion is that this neither panics
		// nor trips the race detector.
		_ = a.SetBalancer(svc, nil)
	}

	close(stop)
	writer.Wait()
}

// TestRenewalTimerConcurrentWithSchedule is the regression test for the
// addressRenewal.timer race: renewAddress re-arms the timer on the timer's
// own goroutine while scheduleRenewal Stops it on the service-sync
// goroutine, which happens whenever a Service is re-announced as its
// renewal fires.
//
// scheduleRenewal is the pairing that was wide open, because it Stopped the
// superseded timer without consulting cancelled. cancelRenewal and
// WithdrawAll set cancelled before Stop and renewAddress re-checks it, which
// narrows their window to a couple of instructions but does not close it --
// so timer is atomic rather than flag-gated, which fixes all three at once.
//
// Verified to catch the defect by reverting timer to a plain *time.Timer,
// which makes this fail with a DATA RACE pairing renewAddress's re-arm
// against scheduleRenewal's Stop.
//
// A real link is used so renewAddress gets past addNetworkWithOptions;
// unprivileged the call fails, which still falls through to the re-arm.
// TEST-NET-1 means nothing is configured on the host either way.
func TestRenewalTimerConcurrentWithSchedule(t *testing.T) {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Skip("no loopback interface")
	}
	a := &announcer{logger: log.NewNopLogger(), myNode: "node-a", client: nopServiceEvent{}}

	const nsName = "default/renew-race"
	ipnet := net.IPNet{IP: net.ParseIP("192.0.2.1"), Mask: net.CIDRMask(24, 32)}
	key := renewalKey(nsName, ipnet.IP.String())
	opts := AddressOptions{ValidLft: 300, PreferedLft: 300}

	for i := 0; i < 300; i++ {
		r := &addressRenewal{ipNet: ipnet, link: lo, opts: opts, interval: time.Hour}
		r.timer.Store(time.AfterFunc(time.Hour, func() {}))
		a.addressRenewals.Store(key, r)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // the fired renewal timer, re-arming itself
			defer wg.Done()
			a.renewAddress(key)
		}()
		// service-sync goroutine re-announcing the same address
		a.scheduleRenewal(nsName, ipnet, lo, opts)
		wg.Wait()
	}

	// Leave no timers behind for other tests in the package.
	a.addressRenewals.Range(func(k, v interface{}) bool {
		v.(*addressRenewal).cancelled.Store(true)
		v.(*addressRenewal).stopTimer()
		a.addressRenewals.Delete(k)
		return true
	})
}

// TestScheduleRenewalCancelsSupersededTimer covers the second half of the
// same fix. scheduleRenewal replaces a renewal wholesale, so the superseded
// one must be marked cancelled and not merely Stopped: an in-flight
// renewAddress re-checks that flag before re-arming, and without it a
// Stopped timer could arm itself again and go on firing untracked, with
// nothing left in the map to cancel it.
func TestScheduleRenewalCancelsSupersededTimer(t *testing.T) {
	a := &announcer{logger: log.NewNopLogger(), myNode: "node-a", client: nopServiceEvent{}}

	const nsName = "default/superseded"
	ipnet := net.IPNet{IP: net.ParseIP("192.0.2.2"), Mask: net.CIDRMask(24, 32)}
	key := renewalKey(nsName, ipnet.IP.String())
	opts := AddressOptions{ValidLft: 300, PreferedLft: 300}

	a.scheduleRenewal(nsName, ipnet, nil, opts)
	first, ok := a.addressRenewals.Load(key)
	assert.True(t, ok, "first renewal should be tracked")

	a.scheduleRenewal(nsName, ipnet, nil, opts)
	second, ok := a.addressRenewals.Load(key)
	assert.True(t, ok, "replacement renewal should be tracked")
	assert.NotSame(t, first, second, "scheduleRenewal should install a new renewal")

	assert.True(t, first.(*addressRenewal).cancelled.Load(),
		"superseded renewal must be marked cancelled, not just stopped")
	assert.False(t, second.(*addressRenewal).cancelled.Load(),
		"the live renewal must not be marked cancelled")

	a.cancelRenewal(nsName, ipnet.IP.String())
}

func TestWithdrawalSharedIPRefcount(t *testing.T) {
	a := withdrawalTestAnnouncer(nil)

	announceForTest(a, "default/svc-a", "192.0.2.1")
	announceForTest(a, "default/svc-b", "192.0.2.1")

	// Withdrawing one service leaves the other's state intact — the
	// shared address must not be removed while another announcer holds it.
	assert.NoError(t, a.deleteAddress("default/svc-a", "test", net.ParseIP("192.0.2.1")))
	_, aAnnounced := a.announced.Load(renewalKey("default/svc-a", "192.0.2.1"))
	assert.False(t, aAnnounced)
	_, bAnnounced := a.announced.Load(renewalKey("default/svc-b", "192.0.2.1"))
	assert.True(t, bAnnounced, "second service's announced entry must survive")
	_, bTimer := a.addressRenewals.Load(renewalKey("default/svc-b", "192.0.2.1"))
	assert.True(t, bTimer, "second service's renewal timer must survive")

	// Withdrawing the second reference releases the address.
	assert.NoError(t, a.deleteAddress("default/svc-b", "test", net.ParseIP("192.0.2.1")))
	_, bAnnounced = a.announced.Load(renewalKey("default/svc-b", "192.0.2.1"))
	assert.False(t, bAnnounced)
	_, bTimer = a.addressRenewals.Load(renewalKey("default/svc-b", "192.0.2.1"))
	assert.False(t, bTimer)
}

// TestBuildUnsolicitedNA pins the wire-level shape of the IPv6
// failover announcement without needing a raw socket: Override set
// (neighbors must replace the moved VIP's cache entry), Solicited and
// Router clear, target = the VIP, with a target link-layer address
// option. ndp.MarshalMessage round-trips through the real wire format.
func TestBuildUnsolicitedNA(t *testing.T) {
	hw := net.HardwareAddr{0x02, 0x00, 0x5e, 0x10, 0x00, 0x01}
	target := netip.MustParseAddr("2001:db8::10")

	na := buildUnsolicitedNA(target, hw)
	assert.True(t, na.Override)
	assert.False(t, na.Solicited)
	assert.False(t, na.Router)
	assert.Equal(t, target, na.TargetAddress)

	raw, err := ndp.MarshalMessage(na)
	assert.NoError(t, err)
	assert.Equal(t, byte(136), raw[0], "ICMPv6 type must be Neighbor Advertisement")

	parsed, err := ndp.ParseMessage(raw)
	assert.NoError(t, err)
	round, ok := parsed.(*ndp.NeighborAdvertisement)
	if assert.True(t, ok) {
		assert.Equal(t, target, round.TargetAddress)
		assert.True(t, round.Override)
		assert.False(t, round.Solicited)
		if assert.Len(t, round.Options, 1) {
			lla, ok := round.Options[0].(*ndp.LinkLayerAddress)
			if assert.True(t, ok) {
				assert.Equal(t, ndp.Target, lla.Direction)
				assert.Equal(t, hw, lla.Addr)
			}
		}
	}
}

func TestSendUnsolicitedNARejectsIPv4(t *testing.T) {
	assert.Error(t, sendUnsolicitedNA("lo", net.ParseIP("192.0.2.1")))
}

// fakeElection reproduces the one condition that mattered: the answer to
// "who owns this address" differs depending on which question you ask.
type fakeElection struct {
	winner     string // what plain Winner would have said
	preferred  string // what WinnerWithPreference says when preferred is non-empty
	sawPreferr []string
}

func (f *fakeElection) WinnerWithPreference(_ string, preferred []string) string {
	f.sawPreferr = preferred
	if len(preferred) > 0 {
		return f.preferred
	}
	return f.winner
}

func (f *fakeElection) MemberCount() int { return 3 }

// TestSendGARPSequenceHonoursAffinityPreference is the regression test for
// a bug the e2e migration found: affinity-placed addresses never sent a
// gratuitous ARP or an unsolicited Neighbour Advertisement.
//
// The announce path chooses the node with election.WinnerWithPreference,
// but verify-before-send used plain election.Winner. Those disagree
// exactly when a Service opts into NodeAffinityAnnotation, so the node
// that actually held the address decided it was "no longer winner" and
// skipped every packet. A VIP that moved then received nothing until the
// upstream ARP/ND caches aged out -- minutes of blackhole after a
// failover PureLB itself completed in seconds.
//
// The assertion is "the sequence did not return early", not "a frame
// reached the wire": there is no interface to send on under `go test`, so
// the send fails and counts an error. Sent-or-errored proves the
// ownership check passed; neither moving is the bug.
func TestSendGARPSequenceHonoursAffinityPreference(t *testing.T) {
	const me = "node-b"
	enabled := true
	count := 1

	for _, tc := range []struct {
		name      string
		preferred []string
	}{
		// The bug: preference names this node, plain Winner names another.
		{name: "affinity places it here", preferred: []string{me}},
		// The control: no preference, this node wins outright. This path
		// always worked, and must keep working.
		{name: "no affinity, wins outright", preferred: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeElection{winner: me, preferred: me}
			if len(tc.preferred) > 0 {
				fake.winner = "node-a" // plain Winner would say someone else
			}
			a := &announcer{myNode: me, logger: log.NewNopLogger(), election: fake}
			cfg := &announcerConfig{spec: &purelbv2.LBNodeAgentLocalSpec{
				GARPConfig: &purelbv2.GARPConfig{
					Enabled:      &enabled,
					Count:        &count,
					InitialDelay: "0s",
				},
			}}

			before := ptu.ToFloat64(garpSent) + ptu.ToFloat64(garpErrors)
			a.sendGARPSequence(cfg, net.ParseIP("192.168.1.100"),
				"purelb-test-nonexistent", tc.preferred)

			assert.Eventually(t, func() bool {
				return ptu.ToFloat64(garpSent)+ptu.ToFloat64(garpErrors) > before
			}, 5*time.Second, 20*time.Millisecond,
				"the sequence skipped every packet: verify-before-send asked a "+
					"different question than the announce path did")
			assert.Equal(t, tc.preferred, fake.sawPreferr,
				"verify-before-send must pass the same preferred set the "+
					"announce path used")
		})
	}
}

// recordingServiceEvent captures the events emitted on a Service so a test
// can assert what an operator would see in `kubectl describe svc`.
type recordingServiceEvent struct {
	mu     sync.Mutex
	events []string // "reason: message"
}

func (r *recordingServiceEvent) record(reason, format string, args ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, reason+": "+fmt.Sprintf(format, args...))
}

func (r *recordingServiceEvent) Infof(_ runtime.Object, reason, format string, args ...interface{}) {
	r.record(reason, format, args...)
}

func (r *recordingServiceEvent) Errorf(_ runtime.Object, reason, format string, args ...interface{}) {
	r.record(reason, format, args...)
}

func (r *recordingServiceEvent) ForceSync() {}

func (r *recordingServiceEvent) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, strings.SplitN(e, ":", 2)[0])
	}
	return out
}

// TestAggregationRequiresALeadingSlash pins the parsing rule that made an
// announcement fail silently on a real cluster.
//
// addVirtualInt builds the mask with net.ParseCIDR("0.0.0.0" + aggregation),
// so the value must carry the slash. The field's own doc comment says "an
// integer in the range 8-128" and the CRD validates nothing, so following the
// documentation produces "0.0.0.032", the announcement fails, and -- before
// the fix in this commit -- the Service still reported that it was being
// announced.
//
// The event ORDERING itself is asserted end to end in
// test/e2e/py/tests/test_remote.py, where a real announcement can actually be
// made and then observed to be absent; a unit test here could only check that
// the fake it was handed got called.
func TestAggregationRequiresALeadingSlash(t *testing.T) {
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kube-lb0-test"}}

	for _, tc := range []struct {
		name        string
		aggregation string
		wantErr     string
	}{
		{name: "no slash, as the doc comment describes", aggregation: "32",
			wantErr: "invalid CIDR address"},
		{name: "no slash, IPv6", aggregation: "128", wantErr: "invalid CIDR address"},
		{name: "garbage", aggregation: "/not-a-number", wantErr: "invalid CIDR address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := addVirtualInt(net.ParseIP("10.255.8.1"), link,
				"10.255.8.0/24", tc.aggregation, AddressOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestElectionLossesMetric verifies election_losses_total is callable without panic.
// Full loss scenarios tested via e2e suite (actual node failovers).
func TestElectionLossesMetric(t *testing.T) {
	before := ptu.ToFloat64(electionLosses)
	RecordElectionLoss()
	assert.Greater(t, ptu.ToFloat64(electionLosses), before,
		"election_losses_total should increment on call")
}
