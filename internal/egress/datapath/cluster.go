// Copyright 2026 Acnodal Inc.
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

package datapath

import (
	"net"
	"net/netip"
	"slices"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// FixedClusterNetworks are in every node's cluster networks: link-local,
// multicast and the limited broadcast never leave the cluster as egress.
var FixedClusterNetworks = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
}

// ClusterRoutes are the pod ranges this node's routes show (design 6.3),
// both families; nodes are every Node's InternalIPs and ExternalIPs.
func ClusterRoutes(h *netlink.Handle, nodes map[netip.Addr]bool) ([]netip.Prefix, error) {
	var routes []netlink.Route
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		// Table unspecified with the table filter: every table.
		rs, err := h.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return nil, err
		}
		routes = append(routes, rs...)
	}
	links, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	return clusterRoutes(routes, names, nodes), nil
}

// clusterRoutes applies the rule of design 6.3 to routes. A route's prefix
// counts when it is in any table but local (and egress's own), its type is
// unicast, blackhole, unreachable or prohibit, it is no default, link-local
// or multicast route, and either its type is one of the last three (on any
// device: Calico's IPv6 block blackhole is on lo), or a device it uses
// isn't an uplink, lo or kube-lb0, or a gateway it uses is a Node's
// address. An uplink is any device with a unicast default route in any
// table, either family.
func clusterRoutes(routes []netlink.Route, names map[int]string, nodes map[netip.Addr]bool) []netip.Prefix {
	uplinks := map[int]bool{}
	for _, r := range routes {
		if r.Type == unix.RTN_UNICAST && !ownTable(r.Table) && defaultDst(r) {
			for _, idx := range devices(r) {
				uplinks[idx] = true
			}
		}
	}
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	for _, r := range routes {
		if r.Table == unix.RT_TABLE_LOCAL || ownTable(r.Table) || defaultDst(r) {
			continue
		}
		p, ok := prefixOf(r.Dst)
		if !ok || p.Addr().IsLinkLocalUnicast() || p.Addr().IsMulticast() {
			continue
		}
		counts := false
		switch r.Type {
		case unix.RTN_BLACKHOLE, unix.RTN_UNREACHABLE, unix.RTN_PROHIBIT:
			counts = true
		case unix.RTN_UNICAST:
			for _, idx := range devices(r) {
				if name := names[idx]; !uplinks[idx] && name != "lo" && name != "kube-lb0" && name != TunnelName {
					counts = true
				}
			}
			for _, gw := range gateways(r) {
				counts = counts || nodes[gw]
			}
		}
		if counts && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return out
}

// ownTable reports whether a routing table is egress's own.
func ownTable(t int) bool {
	return t == TunnelTable || t == ProhibitTable || (t >= PoolTableBase && t < PoolTableBase+MaxPoolInterfaces)
}

// defaultDst reports whether a route is a default route; netlink gives some
// without a destination.
func defaultDst(r netlink.Route) bool {
	if r.Dst == nil {
		return true
	}
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	bits, _ := n.Mask.Size()
	return netip.PrefixFrom(a.Unmap(), bits).Masked(), true
}

// devices are the interfaces a route uses, its nexthops' included.
func devices(r netlink.Route) []int {
	var out []int
	if r.LinkIndex != 0 {
		out = append(out, r.LinkIndex)
	}
	for _, nh := range r.MultiPath {
		out = append(out, nh.LinkIndex)
	}
	return out
}

// gateways are a route's gateways, its nexthops' included.
func gateways(r netlink.Route) []netip.Addr {
	var out []netip.Addr
	add := func(ip net.IP) {
		if a, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, a.Unmap())
		}
	}
	add(r.Gw)
	if v, ok := r.Via.(*netlink.Via); ok {
		add(v.Addr)
	}
	for _, nh := range r.MultiPath {
		add(nh.Gw)
		if v, ok := nh.Via.(*netlink.Via); ok {
			add(v.Addr)
		}
	}
	return out
}
