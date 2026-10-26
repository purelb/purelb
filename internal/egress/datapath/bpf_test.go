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
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tcx return codes.
const (
	tcxNext     = uint32(0xffffffff) // TCX_NEXT (-1)
	tcxDrop     = 2
	tcxRedirect = 7
)

const (
	protoICMP   = 1
	protoTCP    = 6
	protoUDP    = 17
	protoGRE    = 47
	protoICMPv6 = 58
	tunIfindex  = 99
	sliceIndex  = 5     // 20480-24575 at slice size 4096
	slicePort   = 20481 // in it
)

var (
	self, other = NodeID("node-self"), NodeID("node-other")
	server4     = netip.MustParseAddr("198.51.100.7")
	server6     = netip.MustParseAddr("2001:db8:ffff::7")
	egress4     = netip.MustParseAddr("192.0.2.10")
	egress6     = netip.MustParseAddr("2001:db8::10")
	otherV4     = netip.MustParseAddr("10.0.0.2")
	otherV6     = netip.MustParseAddr("fd00::2")
	otherMAC    = net.HardwareAddr{0x02, 0, 0x0a, 0, 0, 2}
)

// family is one address family's test addresses.
type family struct {
	name           string
	egress, server netip.Addr
	v6             bool
}

var families = []family{{"IPv4", egress4, server4, false}, {"IPv6", egress6, server6, true}}

type harness struct {
	t *testing.T
	o *Objects
}

// newHarness loads the programs with this node, one other node (both
// underlay families) and both egress addresses held, slice sliceIndex
// owned by other.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, o: load(t)}
	h.config(Config{})
	require.NoError(t, h.o.SetNode(other, Node{V4: otherV4, V6: otherV6, MAC: otherMAC}))
	for _, f := range families {
		require.NoError(t, h.o.SetAddress(f.egress, Address{Held: true, SliceSize: 4096, Base: 1024, Table: 7900}))
		require.NoError(t, h.o.SetSlice(f.egress, sliceIndex, other))
	}
	return h
}

// config writes c over the defaults: this node, both underlay families.
func (h *harness) config(c Config) {
	h.t.Helper()
	c.Self, c.TunIfindex = self, tunIfindex
	if !c.Underlay4 && !c.Underlay6 {
		c.Underlay4, c.Underlay6 = true, true
	}
	require.NoError(h.t, h.o.SetConfig(c))
}

// skbCtx is the head of struct __sk_buff: len, pkt_type, mark, ...,
// ifindex at 40. The rest of the 192 bytes stays zero.
type skbCtx [192]byte

func (c *skbCtx) mark() uint32 { return binary.NativeEndian.Uint32(c[8:]) }

// run runs prog on pkt, arriving on ifindex 1 (lo), and returns its
// verdict, the packet as it left, and its mark.
func (h *harness) run(prog *ebpf.Program, pkt []byte) (uint32, []byte, uint32) {
	h.t.Helper()
	var in, out skbCtx
	binary.NativeEndian.PutUint32(in[40:], 1)
	data := make([]byte, len(pkt)+256)
	ret, err := prog.Run(&ebpf.RunOptions{Data: pkt, DataOut: data, Context: in[:], ContextOut: out[:]})
	require.NoError(h.t, err)
	return ret, data, out.mark()
}

// count is reason's count for the family so far.
func (h *harness) count(reason string, v6 bool) uint64 {
	h.t.Helper()
	c, err := h.o.Counts()
	require.NoError(h.t, err)
	fam := 0
	if v6 {
		fam = 1
	}
	return c[reason][fam]
}

func (h *harness) total() uint64 {
	h.t.Helper()
	c, err := h.o.Counts()
	require.NoError(h.t, err)
	var n uint64
	for _, v := range c {
		n += v[0] + v[1]
	}
	return n
}

// expect runs pkt through pl_announce_eth and checks the verdict and the
// reason counted.
func (h *harness) expect(f family, pkt []byte, want uint32, reason string) ([]byte, uint32) {
	h.t.Helper()
	before := h.count(reason, f.v6)
	ret, out, mark := h.run(h.o.PlAnnounceEth, pkt)
	assert.Equal(h.t, want, ret, "verdict (%s)", reason)
	assert.Equal(h.t, before+1, h.count(reason, f.v6), "counted as %s", reason)
	return out, mark
}

// --- packets ------------------------------------------------------------

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
	fragOff   uint16
	moreFrags bool
	id        uint16
}

func ipv4(src, dst netip.Addr, proto uint8, o v4opts, payload []byte) []byte {
	b := make([]byte, 20)
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:], uint16(20+len(payload)))
	binary.BigEndian.PutUint16(b[4:], o.id)
	ff := o.fragOff & 0x1fff
	if o.moreFrags {
		ff |= 0x2000
	}
	binary.BigEndian.PutUint16(b[6:], ff)
	b[8], b[9] = 64, proto
	s, d := src.As4(), dst.As4()
	copy(b[12:], s[:])
	copy(b[16:], d[:])
	return append(b, payload...)
}

func ipv6(src, dst netip.Addr, next uint8, payload []byte) []byte {
	b := make([]byte, 40)
	b[0] = 0x60
	binary.BigEndian.PutUint16(b[4:], uint16(len(payload)))
	b[6], b[7] = next, 64
	s, d := src.As16(), dst.As16()
	copy(b[8:], s[:])
	copy(b[24:], d[:])
	return append(b, payload...)
}

func ports(sport, dport uint16) []byte {
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b[0:], sport)
	binary.BigEndian.PutUint16(b[2:], dport)
	return b
}

func echo(typ uint8, id uint16) []byte {
	b := []byte{typ, 0, 0, 0, 0, 0, 0, 1}
	binary.BigEndian.PutUint16(b[4:], id)
	return b
}

// packet is an IP packet of f from src to dst, framed.
func packet(f family, src, dst netip.Addr, proto uint8, payload []byte) []byte {
	if f.v6 {
		return frame(0x86dd, nil, ipv6(src, dst, proto, payload))
	}
	return frame(0x0800, nil, ipv4(src, dst, proto, v4opts{}, payload))
}

// reply is a UDP reply from the server to the egress address's port.
func reply(f family, port uint16) []byte {
	return packet(f, f.server, f.egress, protoUDP, ports(443, port))
}

func icmpProto(f family) uint8 {
	if f.v6 {
		return protoICMPv6
	}
	return protoICMP
}

// --- tests --------------------------------------------------------------

// Traffic not addressed to an egress address passes, uncounted: the fast
// path.
func TestNotEgressPassesUncounted(t *testing.T) {
	h := newHarness(t)
	for _, f := range families {
		ret, _, _ := h.run(h.o.PlAnnounceEth, packet(f, f.server, f.server, protoUDP, ports(1, slicePort)))
		assert.Equal(t, tcxNext, ret, f.name)
	}
	ret, _, _ := h.run(h.o.PlAnnounceEth, frame(0x0806, nil, make([]byte, 28))) // ARP
	assert.Equal(t, tcxNext, ret)
	assert.Zero(t, h.total())
}

// A reply whose slice another node owns goes to it through the tunnel; one
// whose slice this node owns goes on to the stack with the direct-path
// mark of the pool interface it arrived on.
func TestReplySteering(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.o.SetPoolInterface(1, 3))
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			_, mark := h.expect(f, reply(f, slicePort), tcxRedirect, "to_tunnel")
			assert.Zero(t, mark, "through the tunnel: no mark")

			require.NoError(t, h.o.SetSlice(f.egress, sliceIndex+1, self))
			_, mark = h.expect(f, reply(f, (sliceIndex+1)*4096+7), tcxNext, "owner_local")
			assert.Equal(t, uint32(0x80+1+3), mark, "direct-path flag 0x81 + n")
		})
	}
}

// A slice with no owner, an unallocated slice and a port below the slices
// find no owner; an owner missing from nodes is dropped too.
func TestNoOwner(t *testing.T) {
	h := newHarness(t)
	for _, f := range families {
		require.NoError(t, h.o.SetSlice(f.egress, 7, 0)) // a reserve: owner 0
		h.expect(f, reply(f, 7*4096), tcxDrop, "no_owner")
		h.expect(f, reply(f, 9*4096), tcxDrop, "no_owner")
		h.expect(f, reply(f, 80), tcxDrop, "no_owner")
		require.NoError(t, h.o.SetSlice(f.egress, 8, NodeID("node-gone")))
		h.expect(f, reply(f, 8*4096), tcxDrop, "no_node")
	}
}

// An egress address this node doesn't hold is dropped, whatever the port.
func TestNotAnnouncer(t *testing.T) {
	h := newHarness(t)
	for _, f := range families {
		require.NoError(t, h.o.SetAddress(f.egress, Address{Held: false, SliceSize: 4096, Base: 1024}))
		h.expect(f, reply(f, slicePort), tcxDrop, "not_announcer")
	}
}

// A shared address's Service port goes to kube-proxy; a reserved port to
// its node, ahead of the slice; a PerPod address to its owner, any
// protocol.
func TestServiceReservedPerPod(t *testing.T) {
	h := newHarness(t)
	for _, f := range families {
		require.NoError(t, h.o.SetServicePort(f.egress, protoUDP, 5060))
		h.expect(f, reply(f, 5060), tcxNext, "service_port")
		h.expect(f, packet(f, f.server, f.egress, protoTCP, ports(1, 5060)), tcxDrop, "no_owner") // UDP only

		require.NoError(t, h.o.SetReserved(f.egress, protoUDP, slicePort, self))
		h.expect(f, reply(f, slicePort), tcxNext, "owner_local")
		require.NoError(t, h.o.DeleteReserved(f.egress, protoUDP, slicePort))

		require.NoError(t, h.o.SetAddress(f.egress, Address{Held: true, WholeOwner: other, SliceSize: 4096, Base: 1024}))
		h.expect(f, packet(f, f.server, f.egress, protoGRE, make([]byte, 8)), tcxRedirect, "to_tunnel")
		h.expect(f, reply(f, 80), tcxRedirect, "to_tunnel")
	}
}

// ICMP: echo replies by identifier, errors by the quoted packet's source
// port, neighbour discovery never touched.
func TestICMP(t *testing.T) {
	h := newHarness(t)
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			reply := uint8(0)
			unreach := uint8(3)
			if f.v6 {
				reply, unreach = 129, 1
			}
			h.expect(f, packet(f, f.server, f.egress, icmpProto(f), echo(reply, slicePort)), tcxRedirect, "to_tunnel")
			h.expect(f, packet(f, f.server, f.egress, icmpProto(f), echo(reply, 80)), tcxDrop, "no_owner")

			// An error quoting the egress address's own UDP packet from the
			// slice port.
			var quoted []byte
			if f.v6 {
				quoted = ipv6(f.egress, f.server, protoUDP, ports(slicePort, 53))
			} else {
				quoted = ipv4(f.egress, f.server, protoUDP, v4opts{}, ports(slicePort, 53))
			}
			msg := append([]byte{unreach, 0, 0, 0, 0, 0, 0, 0}, quoted...)
			h.expect(f, packet(f, f.server, f.egress, icmpProto(f), msg), tcxRedirect, "to_tunnel")
		})
	}
	// NS and NA (135, 136) to the address are neighbour discovery: passed,
	// uncounted, before any lookup.
	f := families[1]
	before := h.total()
	for _, typ := range []uint8{133, 134, 135, 136, 137} {
		ret, _, _ := h.run(h.o.PlAnnounceEth, packet(f, f.server, f.egress, protoICMPv6, []byte{typ, 0, 0, 0, 0, 0, 0, 0}))
		assert.Equal(t, tcxNext, ret, "ND type %d", typ)
	}
	assert.Equal(t, before, h.total())
}

// A first fragment's owner, from its port, carries to the later fragments
// of the datagram; a later fragment with no first is dropped.
func TestFragments(t *testing.T) {
	h := newHarness(t)
	f := families[0]
	first := frame(0x0800, nil, ipv4(f.server, f.egress, protoUDP, v4opts{moreFrags: true, id: 7}, ports(443, slicePort)))
	later := frame(0x0800, nil, ipv4(f.server, f.egress, protoUDP, v4opts{fragOff: 185, id: 7}, make([]byte, 64)))
	orphan := frame(0x0800, nil, ipv4(f.server, f.egress, protoUDP, v4opts{fragOff: 185, id: 8}, make([]byte, 64)))
	h.expect(f, first, tcxRedirect, "to_tunnel")
	h.expect(f, later, tcxRedirect, "to_tunnel")
	h.expect(f, orphan, tcxDrop, "frag_missing")

	// IPv6: a Fragment Header, then the UDP header in the first fragment.
	f = families[1]
	frag := func(off uint16, more bool, id byte, payload []byte) []byte {
		v := off << 3
		if more {
			v |= 1
		}
		fh := []byte{protoUDP, 0, byte(v >> 8), byte(v), 0, 0, 0, id}
		return frame(0x86dd, nil, ipv6(f.server, f.egress, 44, append(fh, payload...)))
	}
	h.expect(f, frag(0, true, 9, ports(443, slicePort)), tcxRedirect, "to_tunnel")
	h.expect(f, frag(100, false, 9, make([]byte, 64)), tcxRedirect, "to_tunnel")
	h.expect(f, frag(100, false, 10, make([]byte, 64)), tcxDrop, "frag_missing")
}

// An in-band VLAN tag is parsed and removed before the tunnel; in L2 mode
// the inner frame is addressed to the owner's tunnel MAC.
func TestVLANAndL2(t *testing.T) {
	h := newHarness(t)
	f := families[0]
	tagged := frame(0x0800, []uint16{42}, ipv4(f.server, f.egress, protoUDP, v4opts{}, ports(443, slicePort)))
	out, _ := h.expect(f, tagged, tcxRedirect, "to_tunnel")
	assert.Equal(t, []byte{0x08, 0x00, 0x45}, out[12:15], "the tag is gone")

	h.config(Config{L2: true})
	out, _ = h.expect(f, reply(f, slicePort), tcxRedirect, "to_tunnel")
	assert.Equal(t, []byte(otherMAC), out[0:6], "addressed to the owner's tunnel MAC")
}

// The mark goes into the configured byte, leaving the others alone.
func TestMarkByte(t *testing.T) {
	h := newHarness(t)
	h.config(Config{MarkShift: 8})
	require.NoError(t, h.o.SetPoolInterface(1, 0))
	f := families[0]
	require.NoError(t, h.o.SetSlice(f.egress, sliceIndex, self))
	_, mark := h.expect(f, reply(f, slicePort), tcxNext, "owner_local")
	assert.Equal(t, uint32(0x81)<<8, mark)
}

// A reply too big for an IPv6 underlay is answered with ICMPv6 Packet Too
// Big (frag-needed for IPv4), back out the device it came in on.
func TestPacketTooBig(t *testing.T) {
	h := newHarness(t)
	h.config(Config{Underlay6: true, TunMTU: 1400})
	require.NoError(t, h.o.SetNode(other, Node{V6: otherV6}))
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			big := packet(f, f.server, f.egress, protoUDP, append(ports(443, slicePort), make([]byte, 1450)...))
			out, _ := h.expect(f, big, tcxRedirect, "ptb_sent")
			assert.Equal(t, big[6:12], out[0:6], "back to the sender's MAC")
			if f.v6 {
				assert.Equal(t, byte(protoICMPv6), out[14+6])
				icmp := out[54:]
				assert.Equal(t, byte(2), icmp[0], "Packet Too Big")
				assert.Equal(t, uint32(1400), binary.BigEndian.Uint32(icmp[4:]))
				assert.Equal(t, uint16(8+1232), binary.BigEndian.Uint16(out[18:]))
				assert.Zero(t, checksum6(out[22:38], out[38:54], icmp[:8+1232]), "ICMPv6 checksum")
			} else {
				assert.Equal(t, byte(protoICMP), out[14+9])
				icmp := out[34:]
				assert.Equal(t, []byte{3, 4}, icmp[0:2], "fragmentation needed")
				assert.Equal(t, uint16(1400), binary.BigEndian.Uint16(icmp[6:]))
				assert.Zero(t, checksum(out[14:34]), "IPv4 header checksum")
				assert.Zero(t, checksum(icmp[:8+548]), "ICMP checksum")
			}
			// Small enough: tunnelled as usual.
			h.expect(f, reply(f, slicePort), tcxRedirect, "to_tunnel")
		})
	}
}

// pl_announce_tun drops a packet that didn't come through the tunnel from
// a node.
func TestTunnelForged(t *testing.T) {
	h := newHarness(t)
	f := families[0]
	before := h.count("forged_tunnel", false)
	ret, _, _ := h.run(h.o.PlAnnounceTun, reply(f, slicePort))
	assert.Equal(t, uint32(tcxDrop), ret)
	assert.Equal(t, before+1, h.count("forged_tunnel", false))
}

// NodeID is stable and never 0.
func TestNodeID(t *testing.T) {
	assert.Equal(t, NodeID("node-a"), NodeID("node-a"))
	assert.NotEqual(t, NodeID("node-a"), NodeID("node-b"))
	assert.NotZero(t, NodeID(""))
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

func checksum6(src, dst, payload []byte) uint16 {
	ps := append(append(append([]byte{}, src...), dst...), 0, 0, byte(len(payload)>>8), byte(len(payload)), 0, 0, 0, protoICMPv6)
	return checksum(append(ps, payload...))
}
