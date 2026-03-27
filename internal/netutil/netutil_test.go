package netutil

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vishvananda/netlink"
)

func TestIsDefaultRoute(t *testing.T) {
	tests := []struct {
		name     string
		route    netlink.Route
		expected bool
	}{
		{
			name:     "nil Dst (netlink v1.1.0 style)",
			route:    netlink.Route{Dst: nil},
			expected: true,
		},
		{
			name: "IPv4 default 0.0.0.0/0 (netlink v1.3.1 style)",
			route: netlink.Route{Dst: &net.IPNet{
				IP:   net.IPv4zero,
				Mask: net.CIDRMask(0, 32),
			}},
			expected: true,
		},
		{
			name: "IPv6 default ::/0 (netlink v1.3.1 style)",
			route: netlink.Route{Dst: &net.IPNet{
				IP:   net.IPv6zero,
				Mask: net.CIDRMask(0, 128),
			}},
			expected: true,
		},
		{
			name: "IPv4 non-default 192.168.1.0/24",
			route: netlink.Route{Dst: &net.IPNet{
				IP:   net.ParseIP("192.168.1.0"),
				Mask: net.CIDRMask(24, 32),
			}},
			expected: false,
		},
		{
			name: "IPv6 non-default ::/64",
			route: netlink.Route{Dst: &net.IPNet{
				IP:   net.IPv6zero,
				Mask: net.CIDRMask(64, 128),
			}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsDefaultRoute(tt.route)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLinkNotFoundErrorMatch(t *testing.T) {
	// Verify that errors.As works with netlink.LinkNotFoundError
	// in our pinned v1.3.1. Get a real error from the library by
	// looking up a non-existent interface.
	_, err := netlink.LinkByName("nonexistent-test-interface-xyz")
	assert.Error(t, err)

	var lnfErr netlink.LinkNotFoundError
	assert.True(t, errors.As(err, &lnfErr),
		"errors.As should match LinkNotFoundError with value-type target")
}

func TestDefaultInterface(t *testing.T) {
	// Integration test — requires real netlink access.
	// Skip gracefully in CI environments without a default route.
	link, err := DefaultInterface(2) // nl.FAMILY_V4
	if err != nil {
		t.Skipf("Skipping: no default route available: %v", err)
	}
	assert.NotNil(t, link)
	assert.NotEmpty(t, link.Attrs().Name)
}
