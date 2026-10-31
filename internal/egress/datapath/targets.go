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
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// ReplyTable is the reply table (design 6.3): a reply the tunnel brought
// carries the reply flag 0x80, which routes its reverse-path check here,
// back into the tunnel.
const ReplyTable = 7800

// The RPDB preferences egress owns, both families (design 6.11): replies
// first, then the slots, all ahead of the CNI's rules and main (D18).
const (
	PrefReply  = 80
	PrefDirect = 81
	PrefSlot   = 90
	prefFirst  = 80
	prefLast   = 99
)

// Targets is what the target tables hold (design 6.3): throws for the
// cluster networks and every peer's underlay address, so a pod's traffic
// to the cluster, a reply on its way to a pod and the tunnel's outer
// packets all leave them for main; then each table's default: into the
// tunnel (TunnelTable, with the path MTU per family, locked for IPv6, so
// an oversize packet from a pod gets frag-needed or Packet Too Big,
// design 6.13), prohibit (ProhibitTable), and back into the tunnel
// (ReplyTable). Tunnel 0 leaves the tables empty.
type Targets struct {
	Throws     []netip.Prefix
	Tunnel     int
	MTU4, MTU6 int
}

// SyncTargets makes the target tables exactly t, both families.
func SyncTargets(h *netlink.Handle, t Targets) error {
	var errs []error
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, table := range []int{TunnelTable, ProhibitTable, ReplyTable} {
			var want []netlink.Route
			if t.Tunnel != 0 {
				want = targetRoutes(t, fam, table)
			}
			errs = append(errs, syncTable(h, fam, table, want))
		}
	}
	return errors.Join(errs...)
}

// targetRoutes are table's routes in family fam.
func targetRoutes(t Targets, fam, table int) []netlink.Route {
	var out []netlink.Route
	seen := map[netip.Prefix]bool{}
	for _, p := range t.Throws {
		if p.Addr().Is4() != (fam == netlink.FAMILY_V4) || seen[p.Masked()] {
			continue
		}
		seen[p.Masked()] = true
		out = append(out, netlink.Route{Dst: ipNet(p.Masked()), Table: table, Type: unix.RTN_THROW, Family: fam})
	}
	def := netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	if fam == netlink.FAMILY_V6 {
		def = netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	}
	r := netlink.Route{Dst: ipNet(def), Table: table, Type: unix.RTN_UNICAST, Family: fam, LinkIndex: t.Tunnel}
	switch table {
	case TunnelTable:
		r.MTU = t.MTU4
		if fam == netlink.FAMILY_V6 {
			r.MTU, r.MTULock = t.MTU6, t.MTU6 != 0
		}
	case ProhibitTable:
		r.Type, r.LinkIndex = unix.RTN_PROHIBIT, 0
	}
	return append(out, r)
}

// syncTable makes table's routes in fam exactly want: what differs is
// removed, what is missing added.
func syncTable(h *netlink.Handle, fam, table int, want []netlink.Route) error {
	have, err := h.RouteListFiltered(fam, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, r := range want {
		keep[targetKey(r)] = true
	}
	var errs []error
	present := map[string]bool{}
	for _, r := range have {
		k := targetKey(r)
		if keep[k] && !present[k] {
			present[k] = true
			continue
		}
		if err := h.RouteDel(&r); err != nil && !errors.Is(err, unix.ESRCH) {
			errs = append(errs, fmt.Errorf("table %d: deleting %s: %w", table, r, err))
		}
	}
	for i := range want {
		if present[targetKey(want[i])] {
			continue
		}
		if err := h.RouteAdd(&want[i]); err != nil && !errors.Is(err, unix.EEXIST) {
			errs = append(errs, fmt.Errorf("table %d: adding %s: %w", table, want[i], err))
		}
	}
	return errors.Join(errs...)
}

// targetKey identifies a target route by what SyncTargets sets. The
// kernel puts IPv6 throw and prohibit routes on lo: only a unicast
// route's device counts.
func targetKey(r netlink.Route) string {
	dst := "default"
	if r.Dst != nil {
		if ones, _ := r.Dst.Mask.Size(); ones != 0 {
			dst = r.Dst.String()
		}
	}
	dev := 0
	if r.Type == unix.RTN_UNICAST {
		dev = r.LinkIndex
	}
	return fmt.Sprintf("%d %d %s dev=%d mtu=%d lock=%v", r.Family, r.Type, dst, dev, r.MTU, r.MTULock)
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

// MarkRule routes the packets whose mark field is Mark through Table, at
// preference Pref.
type MarkRule struct {
	Pref  int
	Mark  uint8
	Table int
}

// SyncRules makes the RPDB rules at egress's preferences exactly want, in
// both families, each matching its mark in the mark field at shift
// (D18). Rules there that the agent did not compute are deleted (design
// 6.11). The wanted ones are added first, so a slot moving to another
// table is never without a rule.
func SyncRules(h *netlink.Handle, shift uint8, want []MarkRule) error {
	mask := uint32(0xff) << shift
	var errs []error
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		have, err := h.RuleList(fam)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		keep := map[string]bool{}
		for _, w := range want {
			keep[ruleKey(fam, w.Pref, uint32(w.Mark)<<shift, mask, w.Table)] = true
		}
		present := map[string]bool{}
		var stale []netlink.Rule
		for _, r := range have {
			if r.Priority < prefFirst || r.Priority > prefLast {
				continue
			}
			m := uint32(0)
			if r.Mask != nil {
				m = *r.Mask
			}
			k := ruleKey(fam, r.Priority, r.Mark, m, r.Table)
			if keep[k] && !present[k] && !r.Invert && r.Src == nil && r.Dst == nil && r.IifName == "" && r.OifName == "" {
				present[k] = true
				continue
			}
			stale = append(stale, r)
		}
		for _, w := range want {
			if present[ruleKey(fam, w.Pref, uint32(w.Mark)<<shift, mask, w.Table)] {
				continue
			}
			r := netlink.NewRule()
			r.Family, r.Priority, r.Table, r.Mark, r.Mask = fam, w.Pref, w.Table, uint32(w.Mark)<<shift, &mask
			if err := h.RuleAdd(r); err != nil && !errors.Is(err, unix.EEXIST) {
				errs = append(errs, fmt.Errorf("adding rule %s: %w", r, err))
			}
		}
		for i := range stale {
			if err := h.RuleDel(&stale[i]); err != nil && !errors.Is(err, unix.ENOENT) {
				errs = append(errs, fmt.Errorf("deleting rule %s: %w", stale[i], err))
			}
		}
	}
	return errors.Join(errs...)
}

func ruleKey(fam, pref int, mark, mask uint32, table int) string {
	return fmt.Sprintf("%d %d %#x/%#x %d", fam, pref, mark, mask, table)
}

// ipv4DevconfSrcVmark is IPV4_DEVCONF_SRC_VMARK (linux/ip.h).
const ipv4DevconfSrcVmark = 24

// EnsureSrcValidMark turns on IPv4 src_valid_mark on ifname (design
// 15.2): strict rp_filter then checks a reply's source with its mark,
// which routes the check through the reply tables (design 6.3). It is
// written through rtnetlink (IFLA_INET_CONF), since /proc/sys is
// read-only in the agent's container, and only when it is off. It
// reports whether it wrote it.
func EnsureSrcValidMark(ifindex int, ifname string) (bool, error) {
	b, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + ifname + "/src_valid_mark")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(b)) != "0" {
		return false, nil
	}
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(ifindex)
	req.AddData(msg)
	spec := nl.NewRtAttr(unix.IFLA_AF_SPEC, nil)
	conf := spec.AddRtAttr(unix.AF_INET, nil).AddRtAttr(unix.IFLA_INET_CONF, nil)
	conf.AddRtAttr(ipv4DevconfSrcVmark, nl.Uint32Attr(1))
	req.AddData(spec)
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return false, fmt.Errorf("setting src_valid_mark on %s: %w", ifname, err)
	}
	return true, nil
}

// SyncSteer makes the steer map hold want's slots (D3): those it names
// are written; with prune, the others are removed. Applying want without
// pruning first, then the rules, then pruning, keeps every slot a rule
// sends into the tunnel steered throughout.
func (o *Objects) SyncSteer(want map[uint8][]uint64, prune bool) error {
	have := map[uint32]egressHolders{}
	var k uint32
	var v egressHolders
	it := o.Steer.Iterate()
	for it.Next(&k, &v) {
		have[k] = v
	}
	if err := it.Err(); err != nil {
		return err
	}
	var errs []error
	for slot, holders := range want {
		cur, ok := have[uint32(slot)]
		if ok && cur.N <= MaxHolders && slices.Equal(cur.Holders[:cur.N], holders) {
			continue
		}
		errs = append(errs, o.SetSteer(slot, holders))
	}
	if prune {
		for slot := range have {
			if _, ok := want[uint8(slot)]; !ok {
				errs = append(errs, ignoreNotExist(o.Steer.Delete(slot)))
			}
		}
	}
	return errors.Join(errs...)
}

// DefaultRouteMTU is the smallest MTU of the main table's default routes
// in the family, each its own MTU or its interface's; 0 with none.
func DefaultRouteMTU(h *netlink.Handle, v6 bool) int {
	fam := netlink.FAMILY_V4
	if v6 {
		fam = netlink.FAMILY_V6
	}
	routes, err := h.RouteListFiltered(fam, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return 0
	}
	mtu := 0
	for _, r := range routes {
		if r.Dst != nil {
			if ones, _ := r.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		links := []int{r.LinkIndex}
		if len(r.MultiPath) > 0 {
			links = nil
			for _, nh := range r.MultiPath {
				links = append(links, nh.LinkIndex)
			}
		}
		for _, ifindex := range links {
			m := r.MTU
			if m == 0 {
				if l, err := h.LinkByIndex(ifindex); err == nil {
					m = l.Attrs().MTU
				}
			}
			if m > 0 && (mtu == 0 || m < mtu) {
				mtu = m
			}
		}
	}
	return mtu
}
