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

package spikes

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"purelb.io/egress-spikes/vm/perf"
)

// Spike 10: BPF_PROG_TEST_RUN cost of every prototype program, per path
// and family, with the address guard's method (TestBPFNonVIPBudget: 1M
// iterations on one skb, five runs). Paths that rewrite the packet cannot
// be repeated on one skb, so they use perf.Batched (a bounded number of
// iterations per fresh skb) or perf.SingleShot (one iteration per fresh
// skb, less the test-run fixed cost). Run under CAP_BPF + CAP_NET_ADMIN
// only, so the verifier's speculation hardening is in the measured code
// exactly as the agent would load it.
//
// Test-run limits (recorded per case): the skb is on the loopback device;
// it carries no tunnel metadata, so transit's two bpf_skb_get_tunnel_key
// calls both fail and the sender is looked up as the all-zero peer (the
// test maps that peer to the sending node); redirects are returned, not
// performed. L3-mode purelb-eg0 packets start at the IP header: test-run
// always parses an Ethernet header first, so those cases use source
// addresses whose bytes 12-13 of the packet read as the EtherType
// (8.0.x.x for IPv4; 2001:db8:86dd:: for IPv6).

const (
	tcxNext     = 0xFFFFFFFF
	tcxDrop     = 2
	tcxRedirect = 7
	benchRepeat = 1_000_000
	singleCalls = 20_000
)

// stat indices (egress_core.c enum); the fragment and PTB ones are in
// gaps_test.go.
const (
	stSteered = iota
	_
	stToTunnel
	stOwnerLocal
	stNoOwner
	stTransit
	stTransitForged
	_
	stReplyLocal
	stReplyNotOwner
	_
	_
	stPodTagged
	stPodSpoof
)

func a4(s string) [4]byte   { return netip.MustParseAddr(s).As4() }
func a16(s string) [16]byte { return netip.MustParseAddr(s).As16() }

// ipPkt builds a raw IP packet of the family of dst.
func ipPkt(src, dst string, proto uint8, sport, dport uint16, payload, frag int, id uint32, hbh bool, ttl uint8) []byte {
	l4 := perf.L4(proto, sport, dport, payload)
	if frag == perf.Later {
		l4 = make([]byte, 64) // no L4 header in a later fragment
	}
	if netip.MustParseAddr(dst).Is4() {
		return perf.IPv4(a4(src), a4(dst), proto, l4, ttl, frag, uint16(id))
	}
	return perf.IPv6(a16(src), a16(dst), proto, l4, ttl, hbh, frag, id)
}

type benchCase struct {
	prog, fam, path string
	p               *ebpf.Program
	method          string // repeat, batched, single
	data, ctx       []byte
	setup           func()
	ret             uint32
	stat            int // counter that must move, -1: none
}

func TestPerfBPF(t *testing.T) {
	perf.Require(t)
	o := newCore(t)
	triv, err := perf.Trivial()
	require.NoError(t, err)
	defer triv.Close()

	const self, peer = 1, 2
	cfg := func(tunIf, l2, push uint32) {
		require.NoError(t, o.Cfg.Put(uint32(0), coreConfig{SelfNode: self, TunIfindex: tunIf, PoolIfindex: 1,
			L2Mode: l2, PushEth: push, TunMac: [6]uint8{2, 0, 0, 0, 0, 1}, TunMtu: 1400}))
	}
	pr := func(s string) corePeer { return corePeer{Addr: ip16(s), V6: b2u8(netip.MustParseAddr(s).Is6())} }
	// Egress addresses: the two under test (and the L3-mode test-run
	// addresses), plus 60 more with 16 slices each, so the hash maps are
	// not trivially small.
	egress := []string{"172.30.10.110", "fdaa::110", "8.0.10.110", "2001:db8:86dd::110"}
	for i := 0; i < 30; i++ {
		egress = append(egress, fmt.Sprintf("172.30.11.%d", i+1), fmt.Sprintf("fdab::%d", i+1))
	}
	for _, e := range egress {
		require.NoError(t, o.EgressAddrs.Put(coreAddrKey{Addr: ip16(e), Family: family(e)},
			coreAddrCfg{Base: base, Shift: shift, Announced: 1}))
		for idx := uint16(1); idx < 16; idx++ {
			owner := uint32(10 + idx)
			switch idx {
			case 5:
				owner = self // 20480-24575
			case 6:
				owner = peer // 24576-28671
			case 7:
				owner = 0 // 28672-32767: no owner
			}
			require.NoError(t, o.Slices.Put(coreSliceKey{Addr: ip16(e), Family: family(e), Index: idx}, owner))
		}
	}
	setPeer := func(u string) { require.NoError(t, o.Nodes.Put(uint32(peer), pr(u))) }
	require.NoError(t, o.Nodes.Put(uint32(self), pr("10.0.0.1")))
	// Test-run skbs carry no tunnel metadata: the transit sender lookup
	// sees the all-zero peer, mapped here to the peer node.
	require.NoError(t, o.NodeByAddr.Put(corePeer{}, uint32(peer)))
	steer := func(v6 bool) {
		var st coreSteer
		st.N = 2
		if v6 {
			st.Peers[0], st.Peers[1] = pr("fd00::2"), pr("fd00::3")
		} else {
			st.Peers[0], st.Peers[1] = pr("10.0.0.2"), pr("10.0.0.3")
		}
		require.NoError(t, o.SteerSlots.Put(uint32(slot), st))
	}
	podDev := func(slotv uint32) {
		require.NoError(t, o.PodDevs.Put(uint32(1), corePodDev{Slot: slotv, V4: ip16("10.244.1.5")[0], V6: ip16("fd44:1::5")}))
	}

	type fam struct {
		name, client, other, server, eg, pod, srvT, egT string
	}
	fams := []fam{
		{"ipv4", "198.51.100.7", "192.0.2.99", "172.30.10.1", "172.30.10.110", "10.244.1.5", "8.0.10.1", "8.0.10.110"},
		{"ipv6", "2001:db8:1::7", "2001:db8::99", "fdaa::1", "fdaa::110", "fd44:1::5", "2001:db8:86dd::1", "2001:db8:86dd::110"},
	}
	none := func() {}
	upl := func() { cfg(1000, 0, 0); setPeer("10.0.0.2") }
	upl6 := func() { cfg(1000, 0, 0); setPeer("fd00::2") }
	tunL2 := func() { cfg(1, 1, 0) }
	tunL3 := func() { cfg(1, 0, 1) }

	var cases []benchCase
	for _, f := range fams {
		tcp := func(src, dst string, sp, dp uint16) []byte {
			return perf.Eth(ipPkt(src, dst, unix.IPPROTO_TCP, sp, dp, 0, perf.Whole, 0, false, 64))
		}
		udp := func(src, dst string, sp, dp uint16, payload, frag int, id uint32, hbh bool) []byte {
			return perf.Eth(ipPkt(src, dst, unix.IPPROTO_UDP, sp, dp, payload, frag, id, hbh, 64))
		}
		miss := tcp(f.client, f.other, 40000, 443)
		c := func(prog string, p *ebpf.Program, path, method string, data, ctx []byte, setup func(), ret uint32, st int) {
			cases = append(cases, benchCase{prog, f.name, path, p, method, data, ctx, setup, ret, st})
		}
		for _, pa := range []struct {
			name string
			p    *ebpf.Program
		}{{"pl_announce", o.PlAnnounce}, {"pl_announce_all", o.PlAnnounceAll}} {
			c(pa.name, pa.p, "uplink miss (not an egress address)", "repeat", miss, nil, upl, tcxNext, -1)
			c(pa.name, pa.p, "owner-local reply", "repeat", tcp(f.server, f.eg, 443, 20500), nil, upl, tcxNext, stOwnerLocal)
			c(pa.name, pa.p, "no-owner drop", "repeat", tcp(f.server, f.eg, 443, 28700), nil, upl, tcxDrop, stNoOwner)
			c(pa.name, pa.p, "reply to tunnel, IPv4 underlay", "repeat", tcp(f.server, f.eg, 443, 24600), nil, upl, tcxRedirect, stToTunnel)
			c(pa.name, pa.p, "reply to tunnel, IPv6 underlay", "repeat", tcp(f.server, f.eg, 443, 24600), nil, upl6, tcxRedirect, stToTunnel)
		}
		p := o.PlAnnounce
		if f.name == "ipv6" {
			c("pl_announce", p, "reply to tunnel, Hop-by-Hop header (walk)", "repeat", udp(f.server, f.eg, 53, 24600, 32, perf.Whole, 0, true), nil, upl, tcxRedirect, stToTunnel)
		}
		c("pl_announce", p, "first fragment to tunnel (frags update)", "repeat", udp(f.server, f.eg, 53, 24600, 1400, perf.First, 77, false), nil, upl, tcxRedirect, stFragReplyFirst)
		c("pl_announce", p, "later fragment to tunnel (frags hit)", "repeat", udp(f.server, f.eg, 53, 24600, 0, perf.Later, 77, false), nil, upl, tcxRedirect, stFragReplyFollowed)
		c("pl_announce", p, "later fragment, no first (drop)", "repeat", udp(f.server, f.eg, 53, 24600, 0, perf.Later, 78, false), nil, upl, tcxDrop, stFragReplyMissing)
		ptbStat := stPtb4Sent
		if f.name == "ipv6" {
			ptbStat = stPtb6Sent
		}
		big := udp(f.server, f.eg, 53, 24600, 1500-28-20*b2i(f.name == "ipv6"), perf.Whole, 0, false)
		c("pl_announce", p, "oversize reply, IPv6 underlay: PTB synthesised", "single", big, nil, upl6, tcxRedirect, ptbStat)
		// Method check: repeatable paths measured single-shot too.
		c("pl_announce", p, "uplink miss (not an egress address)", "single", miss, nil, upl, tcxNext, -1)
		c("pl_announce", p, "reply to tunnel, IPv4 underlay", "single", tcp(f.server, f.eg, 443, 24600), nil, upl, tcxRedirect, stToTunnel)

		// purelb-eg0 ingress.
		for _, pa := range []struct {
			name string
			p    *ebpf.Program
		}{{"pl_tun_in", o.PlTunIn}, {"pl_announce_all", o.PlAnnounceAll}} {
			c(pa.name, pa.p, "reply-local, L2", "repeat", tcp(f.server, f.eg, 443, 20500), nil, tunL2, tcxNext, stReplyLocal)
		}
		t2 := o.PlTunIn
		c("pl_tun_in", t2, "reply for another node's slice, L2 (drop)", "repeat", tcp(f.server, f.eg, 443, 24600), nil, tunL2, tcxDrop, stReplyNotOwner)
		c("pl_tun_in", t2, "reply-local, L3", "repeat", ipPkt(f.srvT, f.egT, unix.IPPROTO_TCP, 443, 20500, 0, perf.Whole, 0, false, 64), nil, tunL3, tcxNext, stReplyLocal)
		c("pl_tun_in", t2, "transit, L2 (200 iterations per skb, TTL 255)", "batched",
			perf.Eth(ipPkt(f.eg, f.server, unix.IPPROTO_TCP, 24600, 443, 0, perf.Whole, 0, false, 255)), nil, tunL2, tcxRedirect, stTransit)
		c("pl_tun_in", t2, "transit, L3 + Ethernet push", "single",
			ipPkt(f.egT, f.srvT, unix.IPPROTO_TCP, 24600, 443, 0, perf.Whole, 0, false, 64), nil, tunL3, tcxRedirect, stTransit)
		c("pl_tun_in", t2, "transit from another node's slice, L2 (drop)", "repeat",
			perf.Eth(ipPkt(f.eg, f.server, unix.IPPROTO_TCP, 20500, 443, 0, perf.Whole, 0, false, 64)), nil, tunL2, tcxDrop, stTransitForged)

		// purelb-eg0 egress.
		pod := tcp(f.pod, f.server, 40000, 443)
		c("pl_steer", o.PlSteer, "slot -> tunnel key, IPv4 underlay (2 announcers)", "single", pod, perf.SkbCtx(slot, 0, 0), func() { steer(false) }, tcxNext, stSteered)
		c("pl_steer", o.PlSteer, "slot -> tunnel key, IPv6 underlay (2 announcers)", "single", pod, perf.SkbCtx(slot, 0, 0), func() { steer(true) }, tcxNext, stSteered)
		c("pl_steer", o.PlSteer, "relayed reply (reply flag: pass)", "repeat", pod, perf.SkbCtx(0x80, 0, 0), none, tcxNext, -1)

		// Pod device ingress.
		c("pl_pod_tag", o.PlPodTag, "device not managed (no pod_dev)", "repeat", pod, nil, func() { _ = o.PodDevs.Delete(uint32(1)) }, tcxNext, -1)
		c("pl_pod_tag", o.PlPodTag, "unselected pod (lookup + source check)", "repeat", pod, nil, func() { podDev(0) }, tcxNext, stPodTagged)
		c("pl_pod_tag", o.PlPodTag, "selected pod (+ mark write)", "repeat", pod, nil, func() { podDev(slot) }, tcxNext, stPodTagged)
		c("pl_pod_tag", o.PlPodTag, "spoofed source (drop)", "repeat", tcp(f.server, f.client, 40000, 443), nil, func() { podDev(slot) }, tcxDrop, stPodSpoof)
		c("pl_pod_tag", o.PlPodTag, "selected pod (+ mark write)", "single", pod, nil, func() { podDev(slot) }, tcxNext, stPodTagged)
	}

	// The frags entry the "later fragment" cases follow.
	upl()
	for _, f := range fams {
		_, _, err := perf.TestRun(o.PlAnnounce, perf.Eth(ipPkt(f.server, f.eg, unix.IPPROTO_UDP, 53, 24600, 1400, perf.First, 77, false, 64)), nil, 1, nil)
		require.NoError(t, err)
	}

	for _, c := range cases {
		c.setup()
		before := uint64(0)
		if c.stat >= 0 {
			before = stat(t, o.Stats, uint32(c.stat))
		}
		var vals []float64
		var ret uint32
		extra := ""
		switch c.method {
		case "repeat":
			vals, ret, err = perf.Repeat(c.p, c.data, c.ctx, benchRepeat, perf.Runs)
		case "batched":
			for range perf.Runs {
				var v float64
				v, ret, err = perf.Batched(c.p, c.data, c.ctx, 200, 5000)
				if err != nil {
					break
				}
				vals = append(vals, v)
			}
		case "single":
			var fixed []float64
			for range perf.Runs {
				var v, z float64
				v, z, ret, err = perf.SingleShot(c.p, triv, func(int) []byte { return c.data }, c.ctx, singleCalls)
				if err != nil {
					break
				}
				vals = append(vals, v)
				fixed = append(fixed, z)
			}
			extra = fmt.Sprintf(" (test-run fixed cost subtracted: median %.0f ns)", perf.Median(fixed))
		}
		require.NoError(t, err, "%s %s %s", c.prog, c.fam, c.path)
		ok := ret == c.ret
		moved := uint64(0)
		if c.stat >= 0 {
			moved = stat(t, o.Stats, uint32(c.stat)) - before
			ok = ok && moved > 0
		}
		if !ok {
			t.Errorf("SPIKE10 BPF WRONG-PATH %s %s %s: ret=%#x want %#x, counter %d moved %d;%s",
				c.prog, c.fam, c.path, ret, c.ret, c.stat, moved, stats(t, o.Stats))
			continue
		}
		t.Logf("SPIKE10 BPF %s | %s | %s | %s | ns/pkt %s%s", c.prog, c.fam, c.path, c.method, perf.MinMed(vals), extra)
	}

	// Map memory as the kernel accounts it (fdinfo memlock), with the
	// contents above.
	for _, m := range []struct {
		name string
		m    *ebpf.Map
	}{{"steer_slots", o.SteerSlots}, {"egress_addrs", o.EgressAddrs}, {"slices", o.Slices}, {"nodes", o.Nodes},
		{"frags", o.Frags}, {"node_by_addr", o.NodeByAddr}, {"pod_devs", o.PodDevs}, {"cfg", o.Cfg},
		{"scratch", o.Scratch}, {"stats", o.Stats}} {
		ml, err := perf.Memlock(m.m.FD())
		require.NoError(t, err)
		info, _ := m.m.Info()
		t.Logf("SPIKE10 MAP core %s type=%s max_entries=%d key=%d value=%d memlock=%d", m.name, info.Type, info.MaxEntries, info.KeySize, info.ValueSize, ml)
	}
	// slices at its 65,536-entry maximum.
	n := len(egress) * 15
	for i := 0; n < 65536; i++ {
		e := fmt.Sprintf("10.%d.%d.1", 100+i/256, i%256)
		for idx := uint16(0); idx < 16 && n < 65536; idx++ {
			require.NoError(t, o.Slices.Put(coreSliceKey{Addr: ip16(e), Family: 2, Index: idx}, uint32(1)))
			n++
		}
	}
	ml, err := perf.Memlock(o.Slices.FD())
	require.NoError(t, err)
	t.Logf("SPIKE10 MAP core slices-full entries=65536 memlock=%d", ml)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestPerfMapMemory logs each core map's kernel memory accounting (fdinfo
// memlock) empty and, for the hash maps, filled to max_entries: the BPF
// part of the agent's memory budget.
func TestPerfMapMemory(t *testing.T) {
	perf.Require(t)
	o := newCore(t)
	fill := map[string]func() error{
		"slices": func() error {
			var ks []coreSliceKey
			var vs []uint32
			for i := 0; i < 65536; i++ {
				ks = append(ks, coreSliceKey{Addr: [4]uint32{uint32(i / 16)}, Family: 2, Index: uint16(i % 16)})
				vs = append(vs, 1)
			}
			_, err := o.Slices.BatchUpdate(ks, vs, nil)
			return err
		},
		"egress_addrs": func() error {
			var ks []coreAddrKey
			var vs []coreAddrCfg
			for i := 0; i < 1024; i++ {
				ks = append(ks, coreAddrKey{Addr: [4]uint32{uint32(i + 1)}, Family: 2})
				vs = append(vs, coreAddrCfg{Base: base, Shift: shift})
			}
			_, err := o.EgressAddrs.BatchUpdate(ks, vs, nil)
			return err
		},
		"pod_devs": func() error {
			var ks []uint32
			var vs []corePodDev
			for i := 0; i < 4096; i++ {
				ks = append(ks, uint32(i+1))
				vs = append(vs, corePodDev{Slot: 3})
			}
			_, err := o.PodDevs.BatchUpdate(ks, vs, nil)
			return err
		},
		"node_by_addr": func() error {
			var ks []corePeer
			var vs []uint32
			for i := 0; i < 256; i++ {
				ks = append(ks, corePeer{Addr: [4]uint32{uint32(i + 1)}})
				vs = append(vs, uint32(i))
			}
			_, err := o.NodeByAddr.BatchUpdate(ks, vs, nil)
			return err
		},
	}
	for _, m := range []struct {
		name string
		m    *ebpf.Map
	}{{"steer_slots", o.SteerSlots}, {"egress_addrs", o.EgressAddrs}, {"slices", o.Slices}, {"nodes", o.Nodes},
		{"frags", o.Frags}, {"node_by_addr", o.NodeByAddr}, {"pod_devs", o.PodDevs}, {"cfg", o.Cfg},
		{"scratch", o.Scratch}, {"stats", o.Stats}} {
		empty, err := perf.Memlock(m.m.FD())
		require.NoError(t, err)
		info, _ := m.m.Info()
		full := "same (fixed size)"
		if f, ok := fill[m.name]; ok {
			require.NoError(t, f(), m.name)
			ml, err := perf.Memlock(m.m.FD())
			require.NoError(t, err)
			full = fmt.Sprintf("%d", ml)
		}
		t.Logf("SPIKE10 MAPMEM core %s type=%s max_entries=%d key=%d value=%d memlock-empty=%d memlock-full=%s",
			m.name, info.Type, info.MaxEntries, info.KeySize, info.ValueSize, empty, full)
	}
}
