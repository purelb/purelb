// Copyright 2020 Acnodal Inc.
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

package election

import (
	"net"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNetworkAddress(t *testing.T) {
	tests := []struct {
		name     string
		input    string // CIDR notation
		expected string
	}{
		{
			name:     "IPv4 host in /24",
			input:    "192.168.1.50/24",
			expected: "192.168.1.0/24",
		},
		{
			name:     "IPv4 already network address",
			input:    "192.168.1.0/24",
			expected: "192.168.1.0/24",
		},
		{
			name:     "IPv4 /16 subnet",
			input:    "10.0.5.100/16",
			expected: "10.0.0.0/16",
		},
		{
			name:     "IPv4 /32 single host",
			input:    "192.168.1.1/32",
			expected: "192.168.1.1/32",
		},
		{
			name:     "IPv6 host in /64",
			input:    "fd53:9ef0:8683::50/64",
			expected: "fd53:9ef0:8683::/64",
		},
		{
			name:     "IPv6 /120 subnet",
			input:    "fd53:9ef0:8683::ab/120",
			expected: "fd53:9ef0:8683::/120",
		},
		{
			name:     "IPv6 /128 single host",
			input:    "2001:db8::1/128",
			expected: "2001:db8::1/128",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ipnet, err := net.ParseCIDR(tt.input)
			assert.NoError(t, err)

			result := networkAddress(ipnet)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatSubnetsAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		subnets  []string
		expected string
	}{
		{
			name:     "empty slice",
			subnets:  []string{},
			expected: "",
		},
		{
			name:     "single subnet",
			subnets:  []string{"192.168.1.0/24"},
			expected: "192.168.1.0/24",
		},
		{
			name:     "multiple subnets",
			subnets:  []string{"192.168.1.0/24", "10.0.0.0/16"},
			expected: "192.168.1.0/24,10.0.0.0/16",
		},
		{
			name:     "mixed IPv4 and IPv6",
			subnets:  []string{"192.168.1.0/24", "fd53:9ef0:8683::/64"},
			expected: "192.168.1.0/24,fd53:9ef0:8683::/64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatSubnetsAnnotation(tt.subnets)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseSubnetsAnnotation(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		expected   []string
	}{
		{
			name:       "empty string",
			annotation: "",
			expected:   []string{},
		},
		{
			name:       "single subnet",
			annotation: "192.168.1.0/24",
			expected:   []string{"192.168.1.0/24"},
		},
		{
			name:       "multiple subnets",
			annotation: "192.168.1.0/24,10.0.0.0/16",
			expected:   []string{"192.168.1.0/24", "10.0.0.0/16"},
		},
		{
			name:       "mixed IPv4 and IPv6",
			annotation: "192.168.1.0/24,fd53:9ef0:8683::/64",
			expected:   []string{"192.168.1.0/24", "fd53:9ef0:8683::/64"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseSubnetsAnnotation(tt.annotation)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	// Test that format->parse is a round trip
	original := []string{"192.168.1.0/24", "10.0.0.0/8", "fd53:9ef0:8683::/64"}

	formatted := FormatSubnetsAnnotation(original)
	parsed := ParseSubnetsAnnotation(formatted)

	assert.Equal(t, original, parsed)
}

func TestSubnetContainsIP(t *testing.T) {
	tests := []struct {
		name     string
		subnets  []string
		ip       string
		expected []string
	}{
		{
			name:     "IP in single subnet",
			subnets:  []string{"192.168.1.0/24"},
			ip:       "192.168.1.100",
			expected: []string{"192.168.1.0/24"},
		},
		{
			name:     "IP not in any subnet",
			subnets:  []string{"192.168.1.0/24", "10.0.0.0/8"},
			ip:       "172.16.0.1",
			expected: nil,
		},
		{
			name:     "IP in multiple overlapping subnets",
			subnets:  []string{"10.0.0.0/8", "10.0.1.0/24"},
			ip:       "10.0.1.50",
			expected: []string{"10.0.0.0/8", "10.0.1.0/24"},
		},
		{
			name:     "IPv6 in subnet",
			subnets:  []string{"fd53:9ef0:8683::/64"},
			ip:       "fd53:9ef0:8683::100",
			expected: []string{"fd53:9ef0:8683::/64"},
		},
		{
			name:     "IPv6 not in subnet",
			subnets:  []string{"fd53:9ef0:8683::/64"},
			ip:       "fd53:9ef0:8684::100",
			expected: nil,
		},
		{
			name:     "empty subnets list",
			subnets:  []string{},
			ip:       "192.168.1.100",
			expected: nil,
		},
		{
			name:     "invalid subnet in list is skipped",
			subnets:  []string{"invalid", "192.168.1.0/24"},
			ip:       "192.168.1.100",
			expected: []string{"192.168.1.0/24"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			assert.NotNil(t, ip, "failed to parse test IP")

			result := SubnetContainsIP(tt.subnets, ip)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestGetLocalSubnets_LoopbackOnly tests that GetLocalSubnets works
// with the loopback interface, which exists on all Linux systems.
// This is an integration test that requires actual netlink access.
func TestGetLocalSubnets_LoopbackOnly(t *testing.T) {
	// The loopback interface should always exist and have 127.0.0.0/8
	subnets, err := GetLocalSubnets([]string{"lo"}, false, nil)
	assert.NoError(t, err)

	// Should contain the loopback subnet
	assert.True(t, slices.Contains(subnets, "127.0.0.0/8"),
		"expected 127.0.0.0/8 in subnets, got: %v", subnets)
}

// TestGetLocalSubnets_NonexistentInterface verifies that a non-existent
// interface is gracefully skipped (not an error).
func TestGetLocalSubnets_NonexistentInterface(t *testing.T) {
	// A non-existent interface should be skipped, not cause an error
	subnets, err := GetLocalSubnets([]string{"nonexistent-interface-xyz"}, false, nil)
	assert.NoError(t, err)
	assert.Empty(t, subnets)
}

// TestGetLocalSubnets_IncludeDefault tests that includeDefault=true
// adds subnets from the default interface. This test may be skipped
// if there's no default route (e.g., in a container without network).
func TestGetLocalSubnets_IncludeDefault(t *testing.T) {
	subnets, err := GetLocalSubnets([]string{}, true, nil)
	if err != nil {
		// May fail in environments without a default route
		t.Skipf("Skipping: no default route available: %v", err)
	}

	// Should have at least one subnet from the default interface
	assert.NotEmpty(t, subnets, "expected at least one subnet from default interface")
}

// TestGetLocalSubnets_Deduplication verifies that specifying the same
// interface multiple times doesn't result in duplicate subnets.
func TestGetLocalSubnets_Deduplication(t *testing.T) {
	// List loopback twice - should still only get unique subnets
	subnets, err := GetLocalSubnets([]string{"lo", "lo"}, false, nil)
	assert.NoError(t, err)

	// Check for duplicates
	seen := make(map[string]bool)
	for _, s := range subnets {
		assert.False(t, seen[s], "duplicate subnet found: %s", s)
		seen[s] = true
	}
}

// TestGetLocalSubnets_Sorted verifies that results are sorted
// for deterministic output.
func TestGetLocalSubnets_Sorted(t *testing.T) {
	subnets, err := GetLocalSubnets([]string{"lo"}, true, nil)
	if err != nil {
		t.Skipf("Skipping: %v", err)
	}

	// Verify sorted order
	for i := 1; i < len(subnets); i++ {
		assert.True(t, subnets[i-1] <= subnets[i],
			"subnets not sorted: %s should come before %s",
			subnets[i-1], subnets[i])
	}
}
