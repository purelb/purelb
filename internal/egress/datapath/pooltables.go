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
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The pool-interface tables (design 6.9): pool interface n forwards
// transit for its local-pool addresses in table PoolTableBase + n, a
// mirror of the main table's routes out of it, so pools on different
// NICs, VLANs or gateways never mix. Routed addresses forward in the
// main table, the routing the node's BGP sessions use.
const (
	PoolTableBase     = 7900
	MaxPoolInterfaces = 64
)

// PoolTable is pool interface n's routing table.
func PoolTable(n uint32) int {
	return PoolTableBase + int(n)
}

// PoolStatus is what one sync found about a pool interface.
type PoolStatus struct {
	// Gateway4 and Gateway6: the main table has a default route out of
	// the interface in that family (IPv6: an RA-learned one counts).
	Gateway4, Gateway6 bool
}

// SyncPoolTables makes the tables 7900-7963 exactly the mirrors of the
// pool interfaces in pools (ifindex: n): each holds the main table's
// routes out of its interface, both families; every other table in the
// range is emptied. It reports each interface's gateways.
func SyncPoolTables(h *netlink.Handle, pools map[int]uint32) (map[int]PoolStatus, error) {
	status := map[int]PoolStatus{}
	byTable := map[int]int{}
	for ifindex, n := range pools {
		if n >= MaxPoolInterfaces {
			return nil, fmt.Errorf("pool interface number %d is over %d", n, MaxPoolInterfaces-1)
		}
		byTable[PoolTable(n)] = ifindex
		status[ifindex] = PoolStatus{}
	}
	var errs []error
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		all, err := h.RouteListFiltered(fam, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		want := map[int][]netlink.Route{}
		have := map[int][]netlink.Route{}
		for _, r := range all {
			switch {
			case r.Table == unix.RT_TABLE_MAIN:
				for table, ifindex := range byTable {
					m, ok := mirror(r, ifindex, table)
					if !ok {
						continue
					}
					want[table] = append(want[table], m)
					if m.Dst == nil || isDefault(m) {
						s := status[ifindex]
						if fam == netlink.FAMILY_V4 {
							s.Gateway4 = true
						} else {
							s.Gateway6 = true
						}
						status[ifindex] = s
					}
				}
			case r.Table >= PoolTableBase && r.Table < PoolTableBase+MaxPoolInterfaces:
				have[r.Table] = append(have[r.Table], r)
			}
		}
		for table, routes := range have {
			keep := map[string]bool{}
			for _, r := range want[table] {
				keep[routeKey(r)] = true
			}
			for _, r := range routes {
				if keep[routeKey(r)] {
					continue
				}
				if err := h.RouteDel(&r); err != nil && !errors.Is(err, unix.ESRCH) {
					errs = append(errs, fmt.Errorf("table %d: deleting %s: %w", table, r, err))
				}
			}
		}
		for table, routes := range want {
			present := map[string]bool{}
			for _, r := range have[table] {
				present[routeKey(r)] = true
			}
			// On-link routes first, so the gateways after them resolve in
			// the table itself.
			sort.SliceStable(routes, func(i, j int) bool { return routes[i].Gw == nil && routes[j].Gw != nil })
			for i := range routes {
				if present[routeKey(routes[i])] {
					continue
				}
				if err := h.RouteReplace(&routes[i]); err != nil {
					errs = append(errs, fmt.Errorf("table %d: adding %s: %w", table, routes[i], err))
				}
			}
		}
	}
	return status, errors.Join(errs...)
}

// mirror is r copied into table when it leaves through ifindex (for a
// multipath route, its hops that do).
func mirror(r netlink.Route, ifindex, table int) (netlink.Route, bool) {
	if r.Type != unix.RTN_UNICAST {
		return netlink.Route{}, false
	}
	m := netlink.Route{Dst: r.Dst, Src: r.Src, Gw: r.Gw, Scope: r.Scope, Protocol: r.Protocol, Priority: r.Priority,
		Flags: r.Flags, MTU: r.MTU, Type: r.Type, Table: table, Family: r.Family}
	if len(r.MultiPath) == 0 {
		if r.LinkIndex != ifindex {
			return netlink.Route{}, false
		}
		m.LinkIndex = ifindex
		return m, true
	}
	for _, nh := range r.MultiPath {
		if nh.LinkIndex == ifindex {
			m.MultiPath = append(m.MultiPath, nh)
		}
	}
	return m, len(m.MultiPath) > 0
}

func isDefault(r netlink.Route) bool {
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}

// routeKey identifies a route by what a mirror copies.
func routeKey(r netlink.Route) string {
	dst := "default"
	if r.Dst != nil && !isDefault(r) {
		dst = r.Dst.String()
	}
	hops := []string{fmt.Sprintf("%d/%s", r.LinkIndex, r.Gw)}
	for _, nh := range r.MultiPath {
		hops = append(hops, fmt.Sprintf("%d/%s", nh.LinkIndex, nh.Gw))
	}
	return fmt.Sprintf("%d %s %s src=%s prio=%d mtu=%d", r.Family, dst, strings.Join(hops, ","), r.Src, r.Priority, r.MTU)
}

// ArpAnnounce is the arp_announce in effect on an interface: the larger
// of its own and "all" (design 6.9: 2 is required, validated not set).
func ArpAnnounce(ifname string) (int, error) {
	v := 0
	for _, dev := range []string{"all", ifname} {
		b, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + dev + "/arp_announce")
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return 0, err
		}
		v = max(v, n)
	}
	return v, nil
}

// RecoverPoolInterfaces returns the pool interface numbering the maps
// hold, as an earlier agent left it in the pinned maps, so the tables
// addresses point at keep their numbers across a restart.
func (o *Objects) RecoverPoolInterfaces() (map[int]uint32, error) {
	pools := map[int]uint32{}
	var ifindex uint32
	var v egressPoolIf
	it := o.PoolIfs.Iterate()
	for it.Next(&ifindex, &v) {
		pools[int(ifindex)] = v.N
	}
	return pools, it.Err()
}
