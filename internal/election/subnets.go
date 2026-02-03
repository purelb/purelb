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
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/go-kit/log"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"

	"purelb.io/internal/logging"
)

// SubnetsAnnotation is the annotation key used on leases to store
// the node's local subnets.
const SubnetsAnnotation = "purelb.io/subnets"

// IPv6 address flags that indicate an address should NOT be used:
// - IFA_F_DADFAILED (0x08): Duplicate address detection failed
// - IFA_F_DEPRECATED (0x20): Address is deprecated, don't use for new connections
// - IFA_F_TENTATIVE (0x40): DAD not complete, address not yet usable
const ipv6BadFlags = 0x08 | 0x20 | 0x40

// GetLocalSubnets returns all subnets from the specified interfaces.
// If includeDefault is true, the interface with the default route is
// also included. The returned subnets are in CIDR notation (e.g.,
// "192.168.1.0/24") and are deduplicated and sorted for deterministic output.
//
// For IPv6 addresses, addresses with bad flags (DADFAILED, DEPRECATED,
// TENTATIVE) are filtered out.
//
// If logger is non-nil, detailed logging is provided at info and debug levels.
func GetLocalSubnets(interfaces []string, includeDefault bool, logger log.Logger) ([]string, error) {
	subnets := make(map[string]struct{})

	// Collect subnets from explicitly listed interfaces
	for _, ifName := range interfaces {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			// Interface doesn't exist - skip it rather than fail
			// This handles cases where an interface is configured but not yet present
			if logger != nil {
				logging.Debug(logger, "op", "getLocalSubnets", "interface", ifName,
					"msg", "interface not found, skipping")
			}
			continue
		}

		if logger != nil {
			logging.Debug(logger, "op", "getLocalSubnets", "interface", ifName,
				"msg", "scanning interface for subnets")
		}

		if err := collectSubnetsFromLink(link, subnets, logger); err != nil {
			return nil, fmt.Errorf("failed to get subnets from %s: %w", ifName, err)
		}
	}

	// Include default interface if requested
	if includeDefault {
		// Try IPv4 default first
		if link, err := defaultInterface(nl.FAMILY_V4); err == nil {
			ifName := link.Attrs().Name
			if logger != nil {
				logging.Debug(logger, "op", "getLocalSubnets", "interface", ifName,
					"family", "IPv4", "msg", "found default interface")
			}
			if err := collectSubnetsFromLink(link, subnets, logger); err != nil {
				return nil, fmt.Errorf("failed to get subnets from default IPv4 interface: %w", err)
			}
		} else if logger != nil {
			logging.Debug(logger, "op", "getLocalSubnets", "family", "IPv4",
				"msg", "no default route found")
		}

		// Also try IPv6 default (may be different interface)
		if link, err := defaultInterface(nl.FAMILY_V6); err == nil {
			ifName := link.Attrs().Name
			if logger != nil {
				logging.Debug(logger, "op", "getLocalSubnets", "interface", ifName,
					"family", "IPv6", "msg", "found default interface")
			}
			if err := collectSubnetsFromLink(link, subnets, logger); err != nil {
				return nil, fmt.Errorf("failed to get subnets from default IPv6 interface: %w", err)
			}
		} else if logger != nil {
			logging.Debug(logger, "op", "getLocalSubnets", "family", "IPv6",
				"msg", "no default route found")
		}
	}

	// Convert map to sorted slice for deterministic output
	result := make([]string, 0, len(subnets))
	for subnet := range subnets {
		result = append(result, subnet)
	}
	sort.Strings(result)

	// Log the final result at info level
	if logger != nil {
		logging.Info(logger, "op", "getLocalSubnets", "subnets", FormatSubnetsAnnotation(result),
			"count", len(result), "msg", "subnet detection complete")
	}

	return result, nil
}

// collectSubnetsFromLink extracts all valid subnets from a network interface
// and adds them to the provided map. The map is used for deduplication.
func collectSubnetsFromLink(link netlink.Link, subnets map[string]struct{}, logger log.Logger) error {
	ifName := link.Attrs().Name

	// Collect IPv4 subnets
	addrsV4, err := netlink.AddrList(link, nl.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("failed to list IPv4 addresses: %w", err)
	}
	for _, addr := range addrsV4 {
		if addr.IPNet != nil {
			subnet := networkAddress(addr.IPNet)
			subnets[subnet] = struct{}{}
			if logger != nil {
				logging.Debug(logger, "op", "collectSubnets", "interface", ifName,
					"address", addr.IPNet.String(), "subnet", subnet,
					"family", "IPv4", "msg", "found subnet")
			}
		}
	}

	// Collect IPv6 subnets (filtering out bad addresses)
	addrsV6, err := netlink.AddrList(link, nl.FAMILY_V6)
	if err != nil {
		return fmt.Errorf("failed to list IPv6 addresses: %w", err)
	}
	for _, addr := range addrsV6 {
		// Skip addresses with bad flags
		if (addr.Flags & ipv6BadFlags) != 0 {
			if logger != nil {
				logging.Debug(logger, "op", "collectSubnets", "interface", ifName,
					"address", addr.IPNet.String(), "flags", fmt.Sprintf("0x%x", addr.Flags),
					"family", "IPv6", "msg", "skipping address with bad flags (DADFAILED/DEPRECATED/TENTATIVE)")
			}
			continue
		}
		// Skip link-local addresses (fe80::/10)
		if addr.IPNet != nil && addr.IPNet.IP.IsLinkLocalUnicast() {
			if logger != nil {
				logging.Debug(logger, "op", "collectSubnets", "interface", ifName,
					"address", addr.IPNet.String(),
					"family", "IPv6", "msg", "skipping link-local address")
			}
			continue
		}
		if addr.IPNet != nil {
			subnet := networkAddress(addr.IPNet)
			subnets[subnet] = struct{}{}
			if logger != nil {
				logging.Debug(logger, "op", "collectSubnets", "interface", ifName,
					"address", addr.IPNet.String(), "subnet", subnet,
					"family", "IPv6", "msg", "found subnet")
			}
		}
	}

	return nil
}

// networkAddress returns the network address (subnet) for an IP/mask combination.
// For example, 192.168.1.50/24 returns "192.168.1.0/24".
func networkAddress(ipnet *net.IPNet) string {
	// Mask the IP to get the network address
	network := ipnet.IP.Mask(ipnet.Mask)
	// Reconstruct as CIDR
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprintf("%s/%d", network.String(), ones)
}

// defaultInterface finds the interface with the default route for the
// given address family (nl.FAMILY_V4 or nl.FAMILY_V6).
func defaultInterface(family int) (netlink.Link, error) {
	routes, err := netlink.RouteList(nil, family)
	if err != nil {
		return nil, fmt.Errorf("failed to list routes: %w", err)
	}

	var defaultIdx int
	var defaultMetric int
	first := true

	for _, r := range routes {
		// Default route has nil destination
		if r.Dst != nil {
			continue
		}
		// Take first default route, or one with lower metric
		if first || r.Priority < defaultMetric {
			defaultIdx = r.LinkIndex
			defaultMetric = r.Priority
			first = false
		}
	}

	if first {
		return nil, fmt.Errorf("no default route found for family %d", family)
	}

	return netlink.LinkByIndex(defaultIdx)
}

// FormatSubnetsAnnotation formats a slice of subnets into the annotation
// value format (comma-separated).
func FormatSubnetsAnnotation(subnets []string) string {
	return strings.Join(subnets, ",")
}

// ParseSubnetsAnnotation parses the annotation value back into a slice
// of subnet strings. Returns an empty slice for empty input.
func ParseSubnetsAnnotation(annotation string) []string {
	if annotation == "" {
		return []string{}
	}
	return strings.Split(annotation, ",")
}

// SubnetContainsIP checks if any of the given subnets contains the IP address.
// Returns the matching subnet(s) as a slice.
func SubnetContainsIP(subnets []string, ip net.IP) []string {
	var matches []string
	for _, subnet := range subnets {
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil {
			continue
		}
		if ipnet.Contains(ip) {
			matches = append(matches, subnet)
		}
	}
	return matches
}
