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
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// routeDeletes counts the routes deleted in h while fn runs.
func routeDeletes(t *testing.T, h *host, fn func()) int {
	t.Helper()
	ch := make(chan netlink.RouteUpdate, 256)
	done := make(chan struct{})
	require.NoError(t, netlink.RouteSubscribeWithOptions(ch, done, netlink.RouteSubscribeOptions{Namespace: &h.ns}))
	fn()
	// Events after fn are its own; a marker route ends them.
	marker := &netlink.Route{Dst: cidr("192.0.2.123/32"), Type: unix.RTN_BLACKHOLE, Table: 99}
	h.route(t, marker)
	n := 0
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-ch:
			if u.Table == 99 {
				require.NoError(t, h.nl.RouteDel(marker))
				close(done)
				return n
			}
			if u.Type == unix.RTM_DELROUTE {
				n++
			}
		case <-deadline:
			t.Fatal("no marker route event")
		}
	}
}

// targetDump lists a table's routes as "dst type dev mtu[ lock]".
func targetDump(t *testing.T, h *host, table int) []string {
	t.Helper()
	var out []string
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := h.nl.RouteListFiltered(fam, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		require.NoError(t, err)
		for _, r := range routes {
			dst := "default"
			if r.Dst != nil && !isDefault(r) {
				dst = r.Dst.String()
			}
			if fam == netlink.FAMILY_V6 && dst == "default" {
				dst = "default6"
			}
			typ := map[int]string{unix.RTN_UNICAST: "unicast", unix.RTN_THROW: "throw", unix.RTN_PROHIBIT: "prohibit"}[r.Type]
			s := fmt.Sprintf("%s %s", dst, typ)
			if r.LinkIndex != 0 && r.Type == unix.RTN_UNICAST {
				s += fmt.Sprintf(" dev%d", r.LinkIndex)
			}
			if r.MTU != 0 {
				s += fmt.Sprintf(" mtu%d", r.MTU)
			}
			if r.MTULock {
				s += " lock"
			}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// SyncTargets makes the three target tables exactly what Targets says,
// both families, rewriting nothing that already matches.
func TestNetnsSyncTargets(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	require.NoError(t, h.nl.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "tun"}}))
	require.NoError(t, h.nl.LinkSetUp(h.link(t, "tun")))
	tun := h.link(t, "tun").Attrs().Index
	// Something an earlier agent, or anyone, left in a target table.
	h.route(t, &netlink.Route{Dst: cidr("203.0.113.0/24"), LinkIndex: tun, Table: TunnelTable})

	tg := Targets{Tunnel: tun, MTU4: 1400, MTU6: 1380, Throws: []netip.Prefix{
		netip.MustParsePrefix("10.0.0.2/32"), netip.MustParsePrefix("fd00::2/128"),
		netip.MustParsePrefix("10.244.0.0/16"), netip.MustParsePrefix("10.244.0.0/16"), netip.MustParsePrefix("fd44::/48")}}
	require.NoError(t, SyncTargets(h.nl, tg))
	throws := []string{"10.0.0.2/32 throw", "10.244.0.0/16 throw", "fd00::2/128 throw", "fd44::/48 throw"}
	dev := fmt.Sprintf(" dev%d", tun)
	assert.ElementsMatch(t, append([]string{"default unicast" + dev + " mtu1400", "default6 unicast" + dev + " mtu1380 lock"}, throws...),
		targetDump(t, h, TunnelTable), "IPv6's MTU is locked, so a pod gets Packet Too Big")
	assert.ElementsMatch(t, append([]string{"default prohibit", "default6 prohibit"}, throws...), targetDump(t, h, ProhibitTable))
	assert.ElementsMatch(t, append([]string{"default unicast" + dev, "default6 unicast" + dev}, throws...), targetDump(t, h, ReplyTable))

	assert.Zero(t, routeDeletes(t, h, func() { require.NoError(t, SyncTargets(h.nl, tg)) }), "an unchanged sync deletes nothing")

	tg.MTU4, tg.Throws = 1300, tg.Throws[2:3]
	require.NoError(t, SyncTargets(h.nl, tg))
	assert.ElementsMatch(t, []string{"default unicast" + dev + " mtu1300", "default6 unicast" + dev + " mtu1380 lock", "10.244.0.0/16 throw"},
		targetDump(t, h, TunnelTable))

	require.NoError(t, SyncTargets(h.nl, Targets{}))
	for _, table := range []int{TunnelTable, ProhibitTable, ReplyTable} {
		assert.Empty(t, targetDump(t, h, table), "table %d", table)
	}
}

// ruleDump lists the rules at preferences 70-109 as "fam pref mark/mask table".
func ruleDump(t *testing.T, h *host) []string {
	t.Helper()
	var out []string
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := h.nl.RuleList(fam)
		require.NoError(t, err)
		for _, r := range rules {
			if r.Priority < 70 || r.Priority > 109 {
				continue
			}
			mask := uint32(0)
			if r.Mask != nil {
				mask = *r.Mask
			}
			out = append(out, fmt.Sprintf("%d %d %#x/%#x %d", fam, r.Priority, r.Mark, mask, r.Table))
		}
	}
	sort.Strings(out)
	return out
}

// SyncRules owns preferences 80-99: what it computes is there, in both
// families, at the mark field's byte; anything else there goes; rules
// elsewhere stay.
func TestNetnsSyncRules(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	mask := uint32(0xff00)
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, r := range []struct {
			pref, table int
			mark        uint32
		}{{85, 100, 0x500}, {100, 100, 0x500}} {
			nr := netlink.NewRule()
			nr.Family, nr.Priority, nr.Table, nr.Mark, nr.Mask = fam, r.pref, r.table, r.mark, &mask
			require.NoError(t, h.nl.RuleAdd(nr))
		}
	}
	want := []MarkRule{{PrefReply, 0x80, ReplyTable}, {PrefDirect, 0x81, PoolTable(0)},
		{PrefSlot, 3, TunnelTable}, {PrefSlot, 4, ProhibitTable}}
	require.NoError(t, SyncRules(h.nl, 8, want))
	expect := func(slot3 int) []string {
		var out []string
		for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			out = append(out,
				fmt.Sprintf("%d 80 0x8000/0xff00 %d", fam, ReplyTable),
				fmt.Sprintf("%d 81 0x8100/0xff00 %d", fam, PoolTable(0)),
				fmt.Sprintf("%d 90 0x300/0xff00 %d", fam, slot3),
				fmt.Sprintf("%d 90 0x400/0xff00 %d", fam, ProhibitTable),
				fmt.Sprintf("%d 100 0x500/0xff00 100", fam))
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, expect(TunnelTable), ruleDump(t, h), "pref 85 was not computed: gone; pref 100 isn't egress's")

	// Slot 3's holder is now this node: its rule moves to the pool table.
	want[2].Table = PoolTable(0)
	require.NoError(t, SyncRules(h.nl, 8, want))
	assert.Equal(t, expect(PoolTable(0)), ruleDump(t, h))

	require.NoError(t, SyncRules(h.nl, 0, nil))
	assert.Equal(t, []string{"10 100 0x500/0xff00 100", "2 100 0x500/0xff00 100"}, ruleDump(t, h))
}

// EnsureSrcValidMark turns src_valid_mark on through rtnetlink, once.
func TestNetnsSrcValidMark(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	require.NoError(t, h.nl.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth9"}}))
	idx := h.link(t, "eth9").Attrs().Index
	read := func() string {
		var v string
		require.NoError(t, h.in(func() error {
			b, err := os.ReadFile("/proc/sys/net/ipv4/conf/eth9/src_valid_mark")
			v = strings.TrimSpace(string(b))
			return err
		}))
		return v
	}
	require.Equal(t, "0", read())
	for _, wantSet := range []bool{true, false} {
		require.NoError(t, h.in(func() error {
			set, err := EnsureSrcValidMark(idx, "eth9")
			if err == nil && set != wantSet {
				err = fmt.Errorf("set %v, want %v", set, wantSet)
			}
			return err
		}))
		assert.Equal(t, "1", read())
	}
}

// SyncSteer writes the slots it's given and, pruning, removes the others.
func TestSyncSteer(t *testing.T) {
	o := load(t)
	get := func(slot uint32) []uint64 {
		var v egressHolders
		if err := o.Steer.Lookup(slot, &v); err != nil {
			return nil
		}
		return append([]uint64(nil), v.Holders[:v.N]...)
	}
	require.NoError(t, o.SyncSteer(map[uint8][]uint64{3: {1, 2}, 4: {5}}, true))
	require.NoError(t, o.SyncSteer(map[uint8][]uint64{3: {2}, 6: {7}}, false))
	assert.Equal(t, []uint64{2}, get(3))
	assert.Equal(t, []uint64{5}, get(4), "not pruned yet")
	assert.Equal(t, []uint64{7}, get(6))
	require.NoError(t, o.SyncSteer(map[uint8][]uint64{3: {2}, 6: {7}}, true))
	assert.Nil(t, get(4), "pruned")
	assert.Equal(t, []uint64{2}, get(3))
}
