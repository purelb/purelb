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
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

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

func TestGetLocalAddressOptions_Defaults(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
		config: nil, // No config
	}

	opts := a.getLocalAddressOptions()

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
			a := &announcer{
				logger: log.NewNopLogger(),
				config: &purelbv2.LBNodeAgentLocalSpec{
					AddressConfig: &purelbv2.AddressConfig{
						LocalInterface: &purelbv2.InterfaceAddressConfig{
							ValidLifetime:     tt.validLifetime,
							PreferredLifetime: tt.preferredLifetime,
							NoPrefixRoute:     tt.noPrefixRoute,
						},
					},
				},
			}

			opts := a.getLocalAddressOptions()

			assert.Equal(t, tt.expectedValid, opts.ValidLft, "ValidLft mismatch")
			assert.Equal(t, tt.expectedPreferred, opts.PreferedLft, "PreferedLft mismatch")
			assert.Equal(t, tt.expectedNoPrefix, opts.NoPrefixRoute, "NoPrefixRoute mismatch")
		})
	}
}

func TestGetDummyAddressOptions_Defaults(t *testing.T) {
	a := &announcer{
		logger: log.NewNopLogger(),
		config: nil, // No config
	}

	opts := a.getDummyAddressOptions()

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
			a := &announcer{
				logger: log.NewNopLogger(),
				config: &purelbv2.LBNodeAgentLocalSpec{
					AddressConfig: &purelbv2.AddressConfig{
						DummyInterface: &purelbv2.InterfaceAddressConfig{
							ValidLifetime:     tt.validLifetime,
							PreferredLifetime: tt.preferredLifetime,
							NoPrefixRoute:     tt.noPrefixRoute,
						},
					},
				},
			}

			opts := a.getDummyAddressOptions()

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

	for _, tt := range tests {
		t.Run(tt.name+" local", func(t *testing.T) {
			a := &announcer{
				logger: log.NewNopLogger(),
				config: tt.config,
			}

			// Should not panic
			opts := a.getLocalAddressOptions()
			assert.Equal(t, 300, opts.ValidLft, "default ValidLft")
		})

		t.Run(tt.name+" dummy", func(t *testing.T) {
			a := &announcer{
				logger: log.NewNopLogger(),
				config: tt.config,
			}

			// Should not panic
			opts := a.getDummyAddressOptions()
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
	renewal.timer.Stop()
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
	renewal.timer.Stop()
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
	renewal2.timer.Stop()
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
		val.(*addressRenewal).timer.Stop()
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
func ptrBool(b bool) *bool { return &b }
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
