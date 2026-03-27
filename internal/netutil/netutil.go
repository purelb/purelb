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

// Package netutil provides shared network utility functions for
// interacting with the Linux netlink subsystem.
package netutil

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// IPv6BadFlags contains IPv6 address flags that indicate an address
// should NOT be used:
//   - IFA_F_DADFAILED (0x08): Duplicate address detection failed
//   - IFA_F_DEPRECATED (0x20): Address is deprecated, don't use for new connections
//   - IFA_F_TENTATIVE (0x40): DAD not complete, address not yet usable
const IPv6BadFlags = 0x08 | 0x20 | 0x40

// IsDefaultRoute returns true if the route is a default route.
// Handles both netlink v1.1.0 (Dst == nil) and v1.3.1+ (Dst = 0.0.0.0/0 or ::/0).
func IsDefaultRoute(r netlink.Route) bool {
	if r.Dst == nil {
		return true
	}
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}

// DefaultInterface finds the interface with the default route for the
// given address family (nl.FAMILY_V4 or nl.FAMILY_V6). If multiple
// default routes exist, the one with the lowest metric is preferred.
func DefaultInterface(family int) (netlink.Link, error) {
	routes, err := netlink.RouteList(nil, family)
	if err != nil && !errors.Is(err, netlink.ErrDumpInterrupted) {
		return nil, fmt.Errorf("failed to list routes: %w", err)
	}

	var defaultIdx int
	var defaultMetric int
	first := true

	for _, r := range routes {
		if !IsDefaultRoute(r) {
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
