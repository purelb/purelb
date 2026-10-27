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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func route(dst string, link int, gw string, mod ...func(*netlink.Route)) netlink.Route {
	r := netlink.Route{Table: unix.RT_TABLE_MAIN, Type: unix.RTN_UNICAST, LinkIndex: link}
	if dst != "" {
		_, n, _ := net.ParseCIDR(dst)
		r.Dst = n
	}
	if gw != "" {
		r.Gw = net.ParseIP(gw)
	}
	for _, m := range mod {
		m(&r)
	}
	return r
}

func table(t int) func(*netlink.Route) { return func(r *netlink.Route) { r.Table = t } }
func typ(t int) func(*netlink.Route)   { return func(r *netlink.Route) { r.Type = t } }

// The cases measured on the Tier 1 and Tier 2 CNIs (RESULTS, "Pod ranges
// from kernel routes plus Node podCIDRs").
func TestClusterRoutes(t *testing.T) {
	const (
		lo = iota + 1
		eth0
		eth1
		flannel
		cni0
		kubeLB0
		cali
	)
	names := map[int]string{lo: "lo", eth0: "eth0", eth1: "eth1", flannel: "flannel.1", cni0: "cni0",
		kubeLB0: "kube-lb0", cali: "cali1234"}
	nodes := map[netip.Addr]bool{netip.MustParseAddr("172.30.250.101"): true, netip.MustParseAddr("2001:db8:250::101"): true}
	routes := []netlink.Route{
		route("", eth0, "172.30.250.1"),                // default: eth0 is an uplink
		route("::/0", eth0, "2001:db8:250::1"),         // and IPv6
		route("", eth1, "10.9.0.1", table(100)),        // a default in another table: eth1 too
		route("172.30.250.0/24", eth0, ""),             // the node subnet, on an uplink: no
		route("10.9.0.0/24", eth1, ""),                 // likewise
		route("172.24.5.0/24", cni0, ""),               // flannel: this node's pods, on the bridge
		route("172.24.3.0/24", flannel, "172.24.3.0"),  // flannel VXLAN: another node's
		route("fd04:0:0:3::/64", flannel, ""),          // and IPv6
		route("172.24.1.0/24", eth0, "172.30.250.101"), // DirectRouting / Calico BGP: via a node
		route("fd04:0:0:1::/64", eth0, "2001:db8:250::101"),
		route("10.62.5.0/26", eth0, "172.30.250.1"),                                       // via the router: not a cluster network
		route("fd62:0:0:1::/122", lo, "", typ(unix.RTN_BLACKHOLE)),                        // Calico's IPv6 block: on lo
		route("10.62.7.0/26", 0, "", typ(unix.RTN_BLACKHOLE)),                             // its IPv4 block
		route("10.62.7.5/32", cali, ""),                                                   // a Calico pod
		route("192.0.2.10/32", kubeLB0, ""),                                               // a VIP: no
		route("172.24.5.1/32", cni0, "", table(unix.RT_TABLE_LOCAL), typ(unix.RTN_LOCAL)), // local table: no
		route("10.250.0.0/24", flannel, "", table(PoolTableBase)),                         // egress's own table: no
		route("fe80::/64", cni0, ""),                                                      // link-local: no
		route("ff00::/8", cni0, "", typ(unix.RTN_MULTICAST)),                              // multicast: no
		route("10.66.0.0/16", 0, "", typ(unix.RTN_THROW)),                                 // throw: no
		route("10.77.0.0/16", 0, "", func(r *netlink.Route) { // multipath, one nexthop off the uplinks
			r.MultiPath = []*netlink.NexthopInfo{{LinkIndex: eth0, Gw: net.ParseIP("172.30.250.1")}, {LinkIndex: flannel}}
		}),
	}
	got := clusterRoutes(routes, names, nodes)
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	assert.Equal(t, []string{"10.62.7.0/26", "10.62.7.5/32", "10.77.0.0/16", "172.24.1.0/24", "172.24.3.0/24",
		"172.24.5.0/24", "fd04:0:0:1::/64", "fd04:0:0:3::/64", "fd62:0:0:1::/122"}, s)
}
