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

package addrguard

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The BPF tests load the real program and run crafted packets through it
// with BPF_PROG_TEST_RUN. They need CAP_BPF and CAP_NET_ADMIN, so they skip
// under `make check` (non-root). The bpf-test CI job runs them with exactly
// the pod's capability set and ADDRGUARD_BPF_TESTS=required, which turns a
// skip into a failure so a misconfigured job can't pass with no coverage.

const (
	tcxNext = 0xFFFFFFFF // TCX_NEXT (-1)
	tcxDrop = 2          // TCX_DROP
	xdpDrop = 1          // XDP_DROP
	xdpPass = 2          // XDP_PASS

	protoICMP   = 1
	protoTCP    = 6
	protoUDP    = 17
	protoGRE    = 47
	protoAH     = 51
	protoICMPv6 = 58
	protoSCTP   = 132
)

var (
	vip4    = netip.MustParseAddr("192.0.2.10")
	vip6    = netip.MustParseAddr("2001:db8::10")
	other4  = netip.MustParseAddr("192.0.2.99")
	other6  = netip.MustParseAddr("2001:db8::99")
	client4 = netip.MustParseAddr("198.51.100.7")
	client6 = netip.MustParseAddr("2001:db8:1::7")
)

type harness struct {
	t    *testing.T
	objs addrguardObjects
	ncpu int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ncpu: ebpf.MustPossibleCPU()}
	if err := loadAddrguardObjects(&h.objs, nil); err != nil {
		if errors.Is(err, syscall.EPERM) {
			if os.Getenv("ADDRGUARD_BPF_TESTS") == "required" {
				t.Fatalf("BPF tests required but not permitted: %v", err)
			}
			t.Skipf("needs CAP_BPF and CAP_NET_ADMIN: %v", err)
		}
		t.Fatalf("loading the address guard program: %+v", err)
	}
	t.Cleanup(func() { _ = h.objs.Close() })
	return h
}

func (h *harness) addVIP(a netip.Addr) {
	h.t.Helper()
	zero := make([]addrguardVipCounters, h.ncpu)
	var err error
	if a.Is4() {
		err = h.objs.AgVip4.Put(vip4Key(a), zero)
	} else {
		err = h.objs.AgVip6.Put(vip6Key(a), zero)
	}
	require.NoError(h.t, err)
}

func (h *harness) allowPort(a netip.Addr, proto uint8, port uint16) {
	h.t.Helper()
	require.NoError(h.t, h.objs.AgPorts.Put(portKey(a, proto, port), uint8(1)))
}

func (h *harness) allowProto(p uint32) {
	h.t.Helper()
	require.NoError(h.t, h.objs.AgProtos.Put(p, uint8(1)))
}

func (h *harness) setMode(m uint32) {
	h.t.Helper()
	require.NoError(h.t, h.objs.AgConfig.Put(uint32(0), addrguardGuardConfig{Mode: m}))
}

// reason sums one ag_reasons slot across CPUs.
func (h *harness) reason(action, reason, fam uint32) uint64 {
	h.t.Helper()
	var per []uint64
	require.NoError(h.t, h.objs.AgReasons.Lookup(reasonIndex(action, reason, fam), &per))
	var n uint64
	for _, v := range per {
		n += v
	}
	return n
}

// totalReasons is the sum of every ag_reasons slot: the number of packets
// that matched a guarded address.
func (h *harness) totalReasons() uint64 {
	var n uint64
	for a := uint32(0); a < actMax; a++ {
		for r := uint32(0); r < reasonMax; r++ {
			for f := uint32(0); f < 2; f++ {
				n += h.reason(a, r, f)
			}
		}
	}
	return n
}

func (h *harness) vipCounters(a netip.Addr) addrguardVipCounters {
	h.t.Helper()
	var per []addrguardVipCounters
	var err error
	if a.Is4() {
		err = h.objs.AgVip4.Lookup(vip4Key(a), &per)
	} else {
		err = h.objs.AgVip6.Lookup(vip6Key(a), &per)
	}
	require.NoError(h.t, err)
	var c addrguardVipCounters
	for _, v := range per {
		c.Drop += v.Drop
		c.WouldDrop += v.WouldDrop
	}
	return c
}

// verdict runs pkt through both entry points and reports whether each
// passed it. Every case is checked on both hooks: they share the parser but
// reach it through different header-access helpers and offsets.
func (h *harness) verdict(pkt []byte) (tcxPassed, xdpPassed bool) {
	h.t.Helper()
	rt, err := h.objs.AgTcx.Run(&ebpf.RunOptions{Data: pkt})
	require.NoError(h.t, err)
	rx, err := h.objs.AgXdp.Run(&ebpf.RunOptions{Data: pkt})
	require.NoError(h.t, err)
	require.Contains(h.t, []uint32{tcxNext, tcxDrop}, rt, "unexpected tcx return")
	require.Contains(h.t, []uint32{xdpPass, xdpDrop}, rx, "unexpected xdp return")
	return rt == tcxNext, rx == xdpPass
}

func (h *harness) assertPass(pkt []byte, msg string) {
	h.t.Helper()
	tcx, xdp := h.verdict(pkt)
	assert.True(h.t, tcx, "tcx should pass: %s", msg)
	assert.True(h.t, xdp, "xdp should pass: %s", msg)
}

func (h *harness) assertDrop(pkt []byte, msg string) {
	h.t.Helper()
	tcx, xdp := h.verdict(pkt)
	assert.False(h.t, tcx, "tcx should drop: %s", msg)
	assert.False(h.t, xdp, "xdp should drop: %s", msg)
}

// --- packet construction ---------------------------------------------

// frame wraps an L3 packet in Ethernet, with one 802.1Q tag per entry in
// vlans (outermost first).
func frame(etherType uint16, vlans []uint16, l3 []byte) []byte {
	b := []byte{0x02, 0, 0, 0, 0, 1, 0x02, 0, 0, 0, 0, 2}
	for _, vid := range vlans {
		b = binary.BigEndian.AppendUint16(b, 0x8100)
		b = binary.BigEndian.AppendUint16(b, vid)
	}
	b = binary.BigEndian.AppendUint16(b, etherType)
	return append(b, l3...)
}

type v4opts struct {
	ihl       int    // header length in 32-bit words; 0 means 5
	fragOff   uint16 // fragment offset in 8-octet units
	moreFrags bool
}

func ipv4(src, dst netip.Addr, proto uint8, o v4opts, payload []byte) []byte {
	ihl := o.ihl
	if ihl == 0 {
		ihl = 5
	}
	hdrLen := ihl * 4
	if hdrLen < 20 {
		hdrLen = 20 // a bogus IHL still occupies a real header's bytes
	}
	b := make([]byte, hdrLen)
	b[0] = 0x40 | byte(ihl&0x0f)
	binary.BigEndian.PutUint16(b[2:], uint16(hdrLen+len(payload)))
	ff := o.fragOff & 0x1fff
	if o.moreFrags {
		ff |= 0x2000
	}
	binary.BigEndian.PutUint16(b[6:], ff)
	b[8] = 64
	b[9] = proto
	s, d := src.As4(), dst.As4()
	copy(b[12:], s[:])
	copy(b[16:], d[:])
	for i := 20; i < hdrLen; i++ {
		b[i] = 1 // NOP options
	}
	return append(b, payload...)
}

func ipv6(src, dst netip.Addr, next uint8, payload []byte) []byte {
	b := make([]byte, 40)
	b[0] = 0x60
	binary.BigEndian.PutUint16(b[4:], uint16(len(payload)))
	b[6] = next
	b[7] = 64
	s, d := src.As16(), dst.As16()
	copy(b[8:], s[:])
	copy(b[24:], d[:])
	return append(b, payload...)
}

// extHdr is a generic IPv6 extension header (hop-by-hop, routing, or
// destination options) of 8 octets.
func extHdr(next uint8) []byte { return []byte{next, 0, 1, 4, 0, 0, 0, 0} }

// fragHdr is an IPv6 fragment header.
func fragHdr(next uint8, offset uint16, more bool) []byte {
	v := offset << 3
	if more {
		v |= 1
	}
	b := []byte{next, 0, 0, 0, 0, 0, 0, 42}
	binary.BigEndian.PutUint16(b[2:], v)
	return b
}

// l4 is a TCP/UDP/SCTP-style header: source port, destination port, then
// padding to a plausible header length.
func l4(dport uint16) []byte {
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b[0:], 40000)
	binary.BigEndian.PutUint16(b[2:], dport)
	return b
}

func icmpMsg(typ, code uint8) []byte { return []byte{typ, code, 0, 0, 0, 0, 0, 0} }

func tcp4(dst netip.Addr, dport uint16) []byte {
	return frame(0x0800, nil, ipv4(client4, dst, protoTCP, v4opts{}, l4(dport)))
}

func tcp6(dst netip.Addr, dport uint16) []byte {
	return frame(0x86dd, nil, ipv6(client6, dst, protoTCP, l4(dport)))
}

// guarded returns a harness guarding vip4 and vip6 with TCP/80 allowed.
func guarded(t *testing.T) *harness {
	h := newHarness(t)
	h.addVIP(vip4)
	h.addVIP(vip6)
	h.allowPort(vip4, protoTCP, 80)
	h.allowPort(vip6, protoTCP, 80)
	return h
}

// --- tests -----------------------------------------------------------

func TestBPFNonVIPPassesUncounted(t *testing.T) {
	h := guarded(t)
	h.assertPass(tcp4(other4, 22), "IPv4 to an unguarded address")
	h.assertPass(tcp6(other6, 22), "IPv6 to an unguarded address")
	h.assertPass(frame(0x0806, nil, make([]byte, 28)), "ARP")
	// ::ffff:192.0.2.10 is an IPv6 address; it must not alias the IPv4 VIP.
	h.assertPass(tcp6(netip.AddrFrom16(vip4.As16()), 22), "IPv4-mapped IPv6 address")
	assert.Zero(t, h.totalReasons(), "unguarded traffic must not be counted")
}

func TestBPFServicePorts(t *testing.T) {
	h := guarded(t)
	h.allowPort(vip4, protoUDP, 53)
	h.allowPort(vip6, protoUDP, 53)
	h.allowPort(vip4, protoSCTP, 3868)
	h.allowPort(vip6, protoSCTP, 3868)

	for _, c := range []struct {
		name  string
		v4    bool
		proto uint8
		port  uint16
		pass  bool
	}{
		{"tcp allowed", true, protoTCP, 80, true},
		{"tcp other port", true, protoTCP, 22, false},
		{"udp allowed", true, protoUDP, 53, true},
		{"udp port allowed only for tcp", true, protoUDP, 80, false},
		{"sctp allowed", true, protoSCTP, 3868, true},
		{"sctp denied", true, protoSCTP, 22, false},
	} {
		for _, fam := range []string{"v4", "v6"} {
			var pkt []byte
			if fam == "v4" {
				pkt = frame(0x0800, nil, ipv4(client4, vip4, c.proto, v4opts{}, l4(c.port)))
			} else {
				pkt = frame(0x86dd, nil, ipv6(client6, vip6, c.proto, l4(c.port)))
			}
			if c.pass {
				h.assertPass(pkt, c.name+" "+fam)
			} else {
				h.assertDrop(pkt, c.name+" "+fam)
			}
		}
	}
	assert.Equal(t, uint64(6), h.reason(actDrop, reasonPortDenied, famV4)) // 3 cases x 2 hooks
	assert.Equal(t, uint64(6), h.reason(actDrop, reasonPortDenied, famV6))
	assert.Equal(t, uint64(6), h.reason(actPass, reasonPortAllowed, famV4))
	assert.Equal(t, uint64(6), h.vipCounters(vip4).Drop)
	assert.Equal(t, uint64(6), h.vipCounters(vip6).Drop)
}

func TestBPFIPv4Header(t *testing.T) {
	h := guarded(t)
	opt := func(ihl int, port uint16) []byte {
		return frame(0x0800, nil, ipv4(client4, vip4, protoTCP, v4opts{ihl: ihl}, l4(port)))
	}
	h.assertPass(opt(15, 80), "maximum options, allowed port found past them")
	h.assertDrop(opt(15, 22), "maximum options, denied port found past them")
	h.assertDrop(opt(4, 80), "IHL below 5")
	assert.Equal(t, uint64(2), h.reason(actDrop, reasonMalformed, famV4))
}

func TestBPFIPv6ExtensionHeaders(t *testing.T) {
	h := guarded(t)
	// chain builds n extension headers (hop-by-hop first, then
	// destination options) in front of a TCP header.
	chain := func(n int, port uint16) []byte {
		next := uint8(protoTCP)
		var ext []byte
		for i := n - 1; i >= 0; i-- {
			ext = append(extHdr(next), ext...)
			next = 60
			if i == 1 {
				next = 0
			}
		}
		if n == 1 {
			next = 0
		}
		return frame(0x86dd, nil, ipv6(client6, vip6, next, append(ext, l4(port)...)))
	}
	h.assertPass(chain(8, 80), "8 extension headers, allowed port")
	h.assertDrop(chain(8, 22), "8 extension headers, denied port")
	h.assertDrop(chain(9, 80), "9 extension headers exceeds the walk")
	assert.Equal(t, uint64(2), h.reason(actDrop, reasonMalformed, famV6))
}

func TestBPFFragments(t *testing.T) {
	h := guarded(t)
	// IPv4
	h.assertPass(frame(0x0800, nil, ipv4(client4, vip4, protoTCP, v4opts{fragOff: 100}, []byte{1, 2})),
		"IPv4 non-first fragment")
	h.assertPass(frame(0x0800, nil, ipv4(client4, vip4, protoTCP, v4opts{moreFrags: true}, l4(80))),
		"IPv4 first fragment, allowed port")
	h.assertDrop(frame(0x0800, nil, ipv4(client4, vip4, protoTCP, v4opts{moreFrags: true}, l4(22))),
		"IPv4 first fragment, denied port")
	h.assertDrop(frame(0x0800, nil, ipv4(client4, vip4, protoTCP, v4opts{moreFrags: true}, []byte{1, 2})),
		"IPv4 first fragment too short for ports")
	// IPv6
	h.assertPass(frame(0x86dd, nil, ipv6(client6, vip6, 44, append(fragHdr(protoTCP, 100, false), 1, 2))),
		"IPv6 non-first fragment")
	h.assertPass(frame(0x86dd, nil, ipv6(client6, vip6, 44, append(fragHdr(protoTCP, 0, true), l4(80)...))),
		"IPv6 first fragment, allowed port")
	h.assertDrop(frame(0x86dd, nil, ipv6(client6, vip6, 44, append(fragHdr(protoTCP, 0, true), l4(22)...))),
		"IPv6 first fragment, denied port")
	h.assertDrop(frame(0x86dd, nil, ipv6(client6, vip6, 44, append(fragHdr(protoTCP, 0, true), 1, 2))),
		"IPv6 first fragment too short for ports")
	assert.Equal(t, uint64(2), h.reason(actPass, reasonFragment, famV4))
	assert.Equal(t, uint64(2), h.reason(actPass, reasonFragment, famV6))
}

func TestBPFICMPAllowList(t *testing.T) {
	h := guarded(t)
	icmp4 := func(typ, code uint8) []byte {
		return frame(0x0800, nil, ipv4(client4, vip4, protoICMP, v4opts{}, icmpMsg(typ, code)))
	}
	icmp6 := func(typ uint8) []byte {
		return frame(0x86dd, nil, ipv6(client6, vip6, protoICMPv6, icmpMsg(typ, 0)))
	}
	h.assertPass(icmp4(3, 4), "IPv4 fragmentation needed (PMTUD)")
	h.assertPass(icmp6(2), "IPv6 packet too big (PMTUD)")
	for _, typ := range []uint8{0, 3, 8, 11, 12} {
		h.assertPass(icmp4(typ, 0), fmt.Sprintf("ICMP type %d", typ))
	}
	for _, typ := range []uint8{1, 2, 3, 4, 128, 129, 135, 136} {
		h.assertPass(icmp6(typ), fmt.Sprintf("ICMPv6 type %d", typ))
	}
	for _, typ := range []uint8{5, 9, 10, 13, 14, 17} { // redirect, router adv/sol, timestamp, mask
		h.assertDrop(icmp4(typ, 0), fmt.Sprintf("ICMP type %d", typ))
	}
	for _, typ := range []uint8{133, 134, 137, 138, 139} { // router sol/adv, redirect, renumbering, node info
		h.assertDrop(icmp6(typ), fmt.Sprintf("ICMPv6 type %d", typ))
	}
	assert.Equal(t, uint64(12), h.reason(actDrop, reasonICMPDenied, famV4))
	assert.Equal(t, uint64(10), h.reason(actDrop, reasonICMPDenied, famV6))
}

func TestBPFNonPortProtocols(t *testing.T) {
	h := guarded(t)
	gre4 := frame(0x0800, nil, ipv4(client4, vip4, protoGRE, v4opts{}, make([]byte, 8)))
	gre6 := frame(0x86dd, nil, ipv6(client6, vip6, protoGRE, make([]byte, 8)))
	ah4 := frame(0x0800, nil, ipv4(client4, vip4, protoAH, v4opts{}, make([]byte, 24)))
	ah6 := frame(0x86dd, nil, ipv6(client6, vip6, protoAH, make([]byte, 24)))
	h.assertDrop(gre4, "GRE not allowed")
	h.assertDrop(gre6, "GRE not allowed")
	h.assertDrop(ah4, "AH not allowed")
	h.assertDrop(ah6, "AH not allowed: a plain protocol in both families")
	h.allowProto(protoGRE)
	h.allowProto(protoAH)
	h.assertPass(gre4, "GRE allowed")
	h.assertPass(gre6, "GRE allowed")
	h.assertPass(ah4, "AH allowed")
	h.assertPass(ah6, "AH allowed")
}

func TestBPFVLANTags(t *testing.T) {
	h := guarded(t)
	for n, vlans := range [][]uint16{nil, {100}, {200, 300}} {
		denied := frame(0x0800, vlans, ipv4(client4, vip4, protoTCP, v4opts{}, l4(22)))
		allowed := frame(0x0800, vlans, ipv4(client4, vip4, protoTCP, v4opts{}, l4(80)))
		h.assertDrop(denied, fmt.Sprintf("%d tags, denied port", n))
		h.assertPass(allowed, fmt.Sprintf("%d tags, allowed port", n))
		denied6 := frame(0x86dd, vlans, ipv6(client6, vip6, protoTCP, l4(22)))
		h.assertDrop(denied6, fmt.Sprintf("%d tags, IPv6 denied port", n))
	}
	before := h.totalReasons()
	h.assertPass(frame(0x0800, []uint16{1, 2, 3}, ipv4(client4, vip4, protoTCP, v4opts{}, l4(22))),
		"more than 2 tags passes (documented)")
	assert.Equal(t, before, h.totalReasons(), "a frame we don't parse is not counted")
}

func TestBPFMonitorMode(t *testing.T) {
	h := guarded(t)
	h.setMode(modeMonitor)
	h.assertPass(tcp4(vip4, 22), "monitor passes a denied port")
	h.assertPass(tcp6(vip6, 22), "monitor passes a denied port")
	assert.Equal(t, uint64(2), h.reason(actWouldDrop, reasonPortDenied, famV4))
	assert.Equal(t, uint64(0), h.reason(actDrop, reasonPortDenied, famV4))
	assert.Equal(t, uint64(2), h.vipCounters(vip4).WouldDrop)
	assert.Equal(t, uint64(0), h.vipCounters(vip4).Drop)

	h.setMode(modeEnforce)
	h.assertDrop(tcp4(vip4, 22), "back to enforce")
}

// TestBPFNonVIPBudget is VP gate G3: traffic not addressed to a guarded
// address pays at most 50ns per packet. Run under the bpf-test job; the
// number is logged either way.
func TestBPFNonVIPBudget(t *testing.T) {
	h := guarded(t)
	const budget = 50 * time.Nanosecond
	for _, c := range []struct {
		name string
		pkt  []byte
		prog *ebpf.Program
	}{
		{"tcx ipv4", tcp4(other4, 443), h.objs.AgTcx},
		{"tcx ipv6", tcp6(other6, 443), h.objs.AgTcx},
		{"xdp ipv4", tcp4(other4, 443), h.objs.AgXdp},
		{"xdp ipv6", tcp6(other6, 443), h.objs.AgXdp},
	} {
		// The best of five: shared CI runners are noisy, and the minimum
		// is the program's cost with the least interference.
		per := time.Duration(1<<63 - 1)
		for range 5 {
			_, d, err := c.prog.Benchmark(c.pkt, 1_000_000, nil)
			require.NoError(t, err)
			per = min(per, d)
		}
		t.Logf("%s non-VIP: %v/packet", c.name, per)
		assert.LessOrEqual(t, per, budget, "%s non-VIP path over budget", c.name)
	}
}
