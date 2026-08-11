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
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestIPFamilyFromIP(t *testing.T) {
	tests := []struct {
		name string
		ip   net.IP
		want v1.IPFamily
	}{
		{"dotted quad", net.ParseIP("192.168.1.1"), v1.IPv4Protocol},
		{"ipv6", net.ParseIP("2001:db8::1"), v1.IPv6Protocol},
		// net.ParseIP returns a 16-byte IPv4-in-IPv6 representation for
		// dotted-quad input, so To4() must be what decides the family --
		// len(ip) == net.IPv4len would classify these as v6.
		{"ipv4-mapped ipv6", net.ParseIP("::ffff:192.0.2.1"), v1.IPv4Protocol},
		{"ipv4 4-byte form", net.IPv4(10, 0, 0, 1).To4(), v1.IPv4Protocol},
		// nil.To4() is nil, so nil falls through to the v6 branch. Pinning
		// the current answer: callers guard with net.ParseIP != nil, so
		// this is unreachable in practice rather than deliberate.
		{"nil", nil, v1.IPv6Protocol},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ipFamilyFromIP(tc.ip))
		})
	}
}

// TestAnalyzeIPFamilyTransition covers the sole decision point for
// dual-stack <-> single-stack transitions. Its excessIPs output is what
// feeds ReleaseIPs, so a mistake here either strands addresses or frees
// ones the Service still holds.
func TestAnalyzeIPFamilyTransition(t *testing.T) {
	tests := []struct {
		name        string
		families    []v1.IPFamily
		ingress     []string
		wantMissing []v1.IPFamily
		wantExcess  []string
		wantKeep    []string
	}{
		{
			// No ipFamilies at all defaults to IPv4 rather than "none
			// requested", which would release every address on a Service
			// whose spec omitted the field.
			name:        "no families no ingress defaults to v4",
			wantMissing: []v1.IPFamily{v1.IPv4Protocol},
		},
		{
			name:     "v4 only steady state",
			families: []v1.IPFamily{v1.IPv4Protocol},
			ingress:  []string{"192.168.1.1"},
			wantKeep: []string{"192.168.1.1"},
		},
		{
			name:     "v6 only steady state",
			families: []v1.IPFamily{v1.IPv6Protocol},
			ingress:  []string{"2001:db8::1"},
			wantKeep: []string{"2001:db8::1"},
		},
		{
			name:       "dual to v4 releases the v6",
			families:   []v1.IPFamily{v1.IPv4Protocol},
			ingress:    []string{"192.168.1.1", "2001:db8::1"},
			wantExcess: []string{"2001:db8::1"},
			wantKeep:   []string{"192.168.1.1"},
		},
		{
			// The IPv6 half of the same rule: dropping to v6-only must
			// release the v4, not silently keep both.
			name:       "dual to v6 releases the v4",
			families:   []v1.IPFamily{v1.IPv6Protocol},
			ingress:    []string{"192.168.1.1", "2001:db8::1"},
			wantExcess: []string{"192.168.1.1"},
			wantKeep:   []string{"2001:db8::1"},
		},
		{
			name:        "v4 to dual asks for a v6",
			families:    []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
			ingress:     []string{"192.168.1.1"},
			wantMissing: []v1.IPFamily{v1.IPv6Protocol},
			wantKeep:    []string{"192.168.1.1"},
		},
		{
			name:        "v6 to dual asks for a v4",
			families:    []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
			ingress:     []string{"2001:db8::1"},
			wantMissing: []v1.IPFamily{v1.IPv4Protocol},
			wantKeep:    []string{"2001:db8::1"},
		},
		{
			name:        "dual requested with no ingress asks for both",
			families:    []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
			wantMissing: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
		},
		{
			// An unparseable ingress entry is skipped entirely: neither
			// kept nor released. It therefore leaves the Service holding a
			// junk address forever, and the family it should have supplied
			// is reported missing. Pinning the current silent-drop.
			name:        "unparseable ingress is neither kept nor released",
			families:    []v1.IPFamily{v1.IPv4Protocol},
			ingress:     []string{"not-an-ip"},
			wantMissing: []v1.IPFamily{v1.IPv4Protocol},
		},
		{
			name:     "unparseable alongside a good address",
			families: []v1.IPFamily{v1.IPv4Protocol},
			ingress:  []string{"not-an-ip", "192.168.1.1"},
			wantKeep: []string{"192.168.1.1"},
		},
		{
			// Duplicate entries collapse in the requestedFamilies set, so
			// a repeated family must not produce a duplicate "missing".
			name:        "duplicate family in spec",
			families:    []v1.IPFamily{v1.IPv4Protocol, v1.IPv4Protocol},
			wantMissing: []v1.IPFamily{v1.IPv4Protocol},
		},
		{
			name:        "two addresses of the same unrequested family",
			families:    []v1.IPFamily{v1.IPv4Protocol},
			ingress:     []string{"2001:db8::1", "2001:db8::2"},
			wantExcess:  []string{"2001:db8::1", "2001:db8::2"},
			wantMissing: []v1.IPFamily{v1.IPv4Protocol},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &v1.Service{}
			svc.Spec.IPFamilies = tc.families
			for _, ip := range tc.ingress {
				svc.Status.LoadBalancer.Ingress = append(
					svc.Status.LoadBalancer.Ingress, v1.LoadBalancerIngress{IP: ip})
			}

			missing, excess, keep := analyzeIPFamilyTransition(svc)

			// missingFamilies is built by ranging a map, so its order is
			// not deterministic -- ElementsMatch, never Equal.
			assert.ElementsMatch(t, tc.wantMissing, missing, "missingFamilies")
			assert.Equal(t, tc.wantExcess, excess, "excessIPs")
			assert.Equal(t, tc.wantKeep, keep, "keepIPs")
		})
	}
}
