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
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	stageSlot    = 3
	stagePodSet  = 7
	stageIfindex = 42
	stageFirst   = 20480
	stageLast    = 24575
)

// stageH loads the programs as an eBPF-stage pod node with one selected pod,
// slot stageSlot on both egress addresses, tunnel target, every destination
// but the cluster's and a policy per family named by families.
type stageH struct {
	*harness
	pod   map[bool]netip.Addr
	stage PodStage
}

func newStageH(t *testing.T) *stageH {
	t.Helper()
	h := &harness{t: t, o: load(t)}
	h.config(Config{EBPFStage: true, TunPort: 6082})
	s := &stageH{harness: h, pod: map[bool]netip.Addr{false: pod4, true: pod6}}
	require.NoError(t, h.o.SetPodDev(stageIfindex, PodDev{V4: pod4, V6: pod6, PodSet: stagePodSet}))
	require.NoError(t, h.o.SyncCluster([]netip.Prefix{cluster4, cluster6}))
	all := []Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}}
	s.stage = PodStage{
		PodSets: map[uint8][]string{stagePodSet: {"p"}},
		Policies: map[string]Policy{"p": {
			V4: &PolicyFamily{Include: all, Slot: stageSlot, Address: egress4},
			V6: &PolicyFamily{Include: all, Slot: stageSlot + 1, Address: egress6}}},
		Slots: map[uint8]Slot{
			stageSlot:     {Addr: egress4, First: stageFirst, Last: stageLast},
			stageSlot + 1: {Addr: egress6, First: stageFirst, Last: stageLast}},
	}
	s.sync()
	return s
}

func (s *stageH) sync() {
	s.t.Helper()
	require.NoError(s.t, s.o.SyncEBPFStage(s.stage, map[uint8]SlotRoute{
		stageSlot: {Target: TargetTunnel}, stageSlot + 1: {Target: TargetTunnel}}))
}

// out runs pkt through pl_pod_out_eth as a packet from the pod's device.
func (s *stageH) out(pkt []byte) (uint32, []byte, uint32) {
	s.t.Helper()
	var in, ctx skbCtx
	binary.NativeEndian.PutUint32(in[36:], stageIfindex)
	binary.NativeEndian.PutUint32(in[40:], 1)
	data := make([]byte, len(pkt)+256)
	ret, err := s.o.PlPodOutEth.Run(&ebpf.RunOptions{Data: pkt, DataOut: data, Context: in[:], ContextOut: ctx[:]})
	require.NoError(s.t, err)
	return ret, data, ctx.mark()
}

func tcpSeg(sport, dport uint16, flags uint8) []byte {
	b := ports(sport, dport)
	b[12] = 5 << 4
	b[13] = flags
	return b
}

const (
	flagSYN = 0x02
	flagACK = 0x10
	flagFIN = 0x01
)

func (s *stageH) srcOf(f family, data []byte) (netip.Addr, uint16) {
	if f.v6 {
		return netip.AddrFrom16([16]byte(data[14+8 : 14+24])), binary.BigEndian.Uint16(data[14+40:])
	}
	return netip.AddrFrom4([4]byte(data[14+12 : 14+16])), binary.BigEndian.Uint16(data[14+20:])
}

// pkt is a packet of family f from the pod to the server.
func (s *stageH) pkt(f family, proto uint8, payload []byte) []byte {
	return packet(f, s.pod[f.v6], f.server, proto, payload)
}

// A selected pod's flows take the slot's address and a port in its slice, go
// into the tunnel with the slot in the mark, and keep that port.
func TestPodOutNAT(t *testing.T) {
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			s := newStageH(t)
			slot := uint32(stageSlot)
			if f.v6 {
				slot++
			}
			ret, data, mark := s.out(s.pkt(f, protoTCP, tcpSeg(40000, 443, flagSYN)))
			require.Equal(t, uint32(tcxRedirect), ret)
			assert.Equal(t, slot, mark, "the slot, for pl_steer")
			src, port := s.srcOf(f, data)
			assert.Equal(t, f.egress, src)
			assert.GreaterOrEqual(t, port, uint16(stageFirst))
			assert.LessOrEqual(t, port, uint16(stageLast))
			assert.Equal(t, uint64(1), s.count("nat_new", f.v6))

			// The same flow keeps its port, past its SYN too.
			_, data, _ = s.out(s.pkt(f, protoTCP, tcpSeg(40000, 443, flagACK)))
			_, again := s.srcOf(f, data)
			assert.Equal(t, port, again)
			assert.Equal(t, uint64(1), s.count("nat_new", f.v6))
			assert.Equal(t, uint64(2), s.count("nat_out", f.v6))

			// UDP and echo too; the echo's identifier is what moves.
			ret, data, _ = s.out(s.pkt(f, protoUDP, ports(40001, 53)))
			require.Equal(t, uint32(tcxRedirect), ret)
			src, port = s.srcOf(f, data)
			assert.Equal(t, f.egress, src)
			assert.GreaterOrEqual(t, port, uint16(stageFirst))
			typ := uint8(8)
			if f.v6 {
				typ = 128
			}
			ret, data, _ = s.out(s.pkt(f, icmpProto(f), echo(typ, 77)))
			require.Equal(t, uint32(tcxRedirect), ret)
			id := binary.BigEndian.Uint16(data[14+20+4:])
			if f.v6 {
				id = binary.BigEndian.Uint16(data[14+40+4:])
			}
			assert.GreaterOrEqual(t, id, uint16(stageFirst))
			assert.LessOrEqual(t, id, uint16(stageLast))
		})
	}
}

// What isn't a new flow passes untouched (user decision 2026-11-04).
func TestPodOutNotNew(t *testing.T) {
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			s := newStageH(t)
			// TCP past its SYN with no flow: a reply to something coming in.
			for _, fl := range []uint8{flagSYN | flagACK, flagACK, flagACK | flagFIN} {
				ret, _, _ := s.out(s.pkt(f, protoTCP, tcpSeg(8080, 40000, fl)))
				assert.Equal(t, tcxNext, ret, "flags %#x", fl)
			}
			assert.Equal(t, uint64(3), s.count("nat_midstream", f.v6))
			assert.Zero(t, s.count("nat_new", f.v6))
			// ICMP echo replies and errors.
			typ, unreach := uint8(0), uint8(3)
			if f.v6 {
				typ, unreach = 129, 1
			}
			ret, _, _ := s.out(s.pkt(f, icmpProto(f), echo(typ, 77)))
			assert.Equal(t, tcxNext, ret, "echo reply")
			quote := packet(f, f.server, s.pod[f.v6], protoUDP, ports(53, 40001))[14:]
			ret, _, _ = s.out(s.pkt(f, icmpProto(f), append(echo(unreach, 0), quote...)))
			assert.Equal(t, tcxNext, ret, "error")
			// Another pod's address, or a packet round again: not the pod's.
			ret, _, _ = s.out(packet(f, f.egress, f.server, protoUDP, ports(20481, 53)))
			assert.Equal(t, tcxNext, ret, "not from the pod's address")
			// The cluster networks.
			dst := netip.MustParseAddr("10.96.0.5")
			if f.v6 {
				dst = netip.MustParseAddr("fd05::5")
			}
			ret, _, _ = s.out(packet(f, s.pod[f.v6], dst, protoTCP, tcpSeg(40000, 443, flagSYN)))
			assert.Equal(t, tcxNext, ret, "cluster")
			assert.Zero(t, s.count("nat_new", f.v6))
		})
	}
}

// A device that isn't a selected pod's costs a lookup and passes.
func TestPodOutUnselected(t *testing.T) {
	s := newStageH(t)
	require.NoError(t, s.o.SetPodDev(stageIfindex, PodDev{V4: pod4, V6: pod6}))
	f := families[0]
	before := s.total()
	ret, _, _ := s.out(s.pkt(f, protoTCP, tcpSeg(40000, 443, flagSYN)))
	assert.Equal(t, tcxNext, ret)
	assert.Equal(t, before, s.total(), "uncounted")
	require.NoError(t, s.o.DeletePodDev(stageIfindex))
	ret, _, _ = s.out(s.pkt(f, protoTCP, tcpSeg(40000, 443, flagSYN)))
	assert.Equal(t, tcxNext, ret, "no device entry at all")
}

// The netkit path: no ingress device, the pod found by source address.
func TestPodOutBySource(t *testing.T) {
	s := newStageH(t)
	s.config(Config{EBPFStage: true, TunPort: 6082, PodBySrc: true})
	var ctx skbCtx
	var in skbCtx
	f := families[0]
	binary.NativeEndian.PutUint32(in[40:], 1)
	pkt := s.pkt(f, protoTCP, tcpSeg(40000, 443, flagSYN))
	data := make([]byte, len(pkt)+256)
	ret, err := s.o.PlPodOutEth.Run(&ebpf.RunOptions{Data: pkt, DataOut: data, Context: in[:], ContextOut: ctx[:]})
	require.NoError(t, err)
	assert.Equal(t, uint32(tcxRedirect), ret)
	src, _ := s.srcOf(f, data)
	assert.Equal(t, f.egress, src)
	// Without the option, no ingress device means not a pod.
	s.config(Config{EBPFStage: true, TunPort: 6082})
	ret, err = s.o.PlPodOutEth.Run(&ebpf.RunOptions{Data: s.pkt(f, protoTCP, tcpSeg(40001, 443, flagSYN)), DataOut: data, Context: in[:], ContextOut: ctx[:]})
	require.NoError(t, err)
	assert.Equal(t, tcxNext, ret)
}

// Policies in rank order: an excluded destination falls to the next; ports
// limit a destination; the longest prefix carries the ports of every
// destination holding it. As design 6.3's chains.
func TestPodOutClassify(t *testing.T) {
	f := families[0]
	pfx := func(s ...string) (out []netip.Prefix) {
		for _, x := range s {
			out = append(out, netip.MustParsePrefix(x))
		}
		return out
	}
	s := newStageH(t)
	s.stage.PodSets[stagePodSet] = []string{"first", "second"}
	s.stage.Policies = map[string]Policy{
		"first": {V4: &PolicyFamily{Slot: stageSlot, Address: egress4,
			Include: []Destination{
				{Prefixes: pfx("198.51.100.0/24"), Ports: []PortRange{{Proto: protoTCP, First: 443, Last: 443}}},
				{Prefixes: pfx("198.51.0.0/16"), Ports: []PortRange{{Proto: protoTCP, First: 80, Last: 81}}},
				{Prefixes: pfx("203.0.113.0/24")},
				// A network with every port, holding one limited to a port.
				{Prefixes: pfx("172.16.0.0/12")},
				{Prefixes: pfx("172.16.5.0/24"), Ports: []PortRange{{Proto: protoTCP, First: 25, Last: 25}}},
			},
			Exclude: []Destination{{Prefixes: pfx("203.0.113.128/25")}}}},
		"second": {V4: &PolicyFamily{Slot: stageSlot + 2, Address: egress4,
			Include: []Destination{{Prefixes: pfx("0.0.0.0/0")}},
			Exclude: []Destination{{Prefixes: pfx("192.0.2.0/24")}}}},
	}
	s.stage.Slots = map[uint8]Slot{
		stageSlot:     {Addr: egress4, First: stageFirst, Last: stageLast},
		stageSlot + 2: {Addr: egress4, First: 28672, Last: 32767}}
	require.NoError(t, s.o.SyncEBPFStage(s.stage, map[uint8]SlotRoute{
		stageSlot: {Target: TargetTunnel}, stageSlot + 2: {Target: TargetTunnel}}))
	for i, c := range []struct {
		dst   string
		dport uint16
		want  uint32 // the slot in the mark; 0: not egress
	}{
		{"198.51.100.7", 443, stageSlot},    // the /24's port
		{"198.51.100.7", 80, stageSlot},     // the /16's, which holds the /24 too
		{"198.51.100.7", 81, stageSlot},     // the range's end
		{"198.51.100.7", 82, stageSlot + 2}, // no port of first: the next policy
		{"198.51.7.7", 443, stageSlot + 2},  // the /24's port, outside it
		{"203.0.113.7", 9, stageSlot},       // no ports: every port
		{"203.0.113.200", 9, stageSlot + 2}, // excluded from first
		{"192.0.2.9", 9, 0},                 // excluded from second, and no third
		{"172.16.5.9", 9, stageSlot},        // the limited /24 sits in a network with every port
	} {
		dst := netip.MustParseAddr(c.dst)
		pkt := packet(f, pod4, dst, protoTCP, tcpSeg(30000+uint16(i), c.dport, flagSYN))
		ret, data, mark := s.out(pkt)
		if c.want == 0 {
			assert.Equal(t, tcxNext, ret, "%s:%d", c.dst, c.dport)
			continue
		}
		require.Equal(t, uint32(tcxRedirect), ret, "%s:%d", c.dst, c.dport)
		assert.Equal(t, c.want, mark, "%s:%d", c.dst, c.dport)
		src, _ := s.srcOf(f, data)
		assert.Equal(t, egress4, src)
	}
}

// With no slot, or a protocol a slice can't carry: FailClosed drops and
// FailOpen passes, counted against the slot. SCTP is one of those on a slice
// (user decision 2026-11-03); a whole address carries everything, address
// only, keeping the port.
func TestPodOutProtocols(t *testing.T) {
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			s := newStageH(t)
			slot := uint8(stageSlot)
			if f.v6 {
				slot++
			}
			pol := s.stage.Policies["p"]
			pf := pol.V4
			if f.v6 {
				pf = pol.V6
			}
			ret, _, _ := s.out(s.pkt(f, protoGRE, make([]byte, 8)))
			assert.Equal(t, uint32(tcxDrop), ret, "port-less, FailClosed")
			ret, _, _ = s.out(s.pkt(f, 132, ports(40000, 9)))
			assert.Equal(t, uint32(tcxDrop), ret, "SCTP, FailClosed")
			counts, err := s.o.PortlessCounts()
			require.NoError(t, err)
			assert.Equal(t, uint64(2), counts[slot])

			pf.FailOpen = true
			s.sync()
			ret, _, _ = s.out(s.pkt(f, protoGRE, make([]byte, 8)))
			assert.Equal(t, tcxNext, ret, "port-less, FailOpen")

			pf.FailOpen, pf.Slot = false, 0
			s.sync()
			ret, _, _ = s.out(s.pkt(f, protoTCP, tcpSeg(40002, 443, flagSYN)))
			assert.Equal(t, uint32(tcxDrop), ret, "no slot, FailClosed")
			pf.FailOpen = true
			s.sync()
			ret, _, _ = s.out(s.pkt(f, protoTCP, tcpSeg(40003, 443, flagSYN)))
			assert.Equal(t, tcxNext, ret, "no slot, FailOpen")

			// A whole address.
			pf.FailOpen, pf.Slot, pf.Whole = false, slot, true
			sl := s.stage.Slots[slot]
			sl.First, sl.Last = 0, 0
			s.stage.Slots[slot] = sl
			s.sync()
			ret, data, _ := s.out(s.pkt(f, protoTCP, tcpSeg(40004, 443, flagSYN)))
			require.Equal(t, uint32(tcxRedirect), ret)
			src, port := s.srcOf(f, data)
			assert.Equal(t, f.egress, src)
			assert.Equal(t, uint16(40004), port, "a whole address keeps the port")
			ret, data, _ = s.out(s.pkt(f, 132, ports(40005, 9)))
			require.Equal(t, uint32(tcxRedirect), ret, "SCTP on a whole address")
			src, port = s.srcOf(f, data)
			assert.Equal(t, f.egress, src)
			assert.Equal(t, uint16(40005), port)
			ret, data, _ = s.out(s.pkt(f, protoGRE, make([]byte, 8)))
			require.Equal(t, uint32(tcxRedirect), ret, "port-less on a whole address")
			src, _ = s.srcOf(f, data)
			assert.Equal(t, f.egress, src)
		})
	}
}

// A reserved port (D19) keeps its port, ahead of the slice; another pod's or
// another protocol's doesn't.
func TestPodOutReserved(t *testing.T) {
	for _, f := range families {
		s := newStageH(t)
		slot := uint8(stageSlot)
		if f.v6 {
			slot++
		}
		sl := s.stage.Slots[slot]
		sl.Reserved = []ReservedPort{{Pod: s.pod[f.v6], Proto: protoUDP, Port: 5060}}
		s.stage.Slots[slot] = sl
		s.sync()
		_, data, _ := s.out(s.pkt(f, protoUDP, ports(5060, 5060)))
		_, port := s.srcOf(f, data)
		assert.Equal(t, uint16(5060), port, f.name)
		_, data, _ = s.out(s.pkt(f, protoTCP, tcpSeg(5060, 5060, flagSYN)))
		_, port = s.srcOf(f, data)
		assert.GreaterOrEqual(t, port, uint16(stageFirst), f.name)
	}
}

// A slice fills to its budget's level (spike 10: first failure at 85% or
// more with 64 probes), then a new flow is dropped and counted.
func TestPodOutExhaustion(t *testing.T) {
	s := newStageH(t)
	f := families[0]
	sl := s.stage.Slots[stageSlot]
	sl.First, sl.Last = 20480, 20480+255
	s.stage.Slots[stageSlot] = sl
	s.sync()
	first := 0
	for i := range 512 {
		ret, _, _ := s.out(s.pkt(f, protoUDP, ports(uint16(30000+i), 53)))
		if ret == uint32(tcxDrop) && first == 0 {
			first = i + 1
		}
	}
	require.NotZero(t, first, "256 ports can't hold 512 flows")
	assert.Greater(t, first, 256*85/100, "the first failure, at 85%% or more")
	assert.NotZero(t, s.count("nat_insert_failed", false))
	flows, err := s.o.NatFlows()
	require.NoError(t, err)
	assert.Equal(t, 256, len(flows), "every port mapped")
}

// Expiry by protocol and state; a flow's deletion takes both entries.
func TestNatUpkeep(t *testing.T) {
	s := newStageH(t)
	f := families[0]
	s.out(s.pkt(f, protoUDP, ports(40000, 53)))
	s.out(s.pkt(f, protoTCP, tcpSeg(40001, 443, flagSYN)))
	flows, err := s.o.NatFlows()
	require.NoError(t, err)
	require.Len(t, flows, 2)
	for _, fl := range flows {
		assert.Equal(t, pod4, fl.Pod)
		assert.Equal(t, uint8(stageSlot), fl.Slot)
	}
	now, err := MonotonicNow()
	require.NoError(t, err)
	n, err := s.o.NatGC(now)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing idle")
	n, err = s.o.NatGC(now + natUDPNew + time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the unanswered UDP flow; the TCP SYN has longer")
	n, err = s.o.NatGC(now + natTCPSyn + time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, entries(t, s.o), "both entries of both flows")

	s.out(s.pkt(f, protoTCP, tcpSeg(40002, 443, flagSYN)))
	flows, err = s.o.NatFlows()
	require.NoError(t, err)
	require.Len(t, flows, 1)
	require.NoError(t, s.o.NatDelete(flows[0]))
	assert.Zero(t, entries(t, s.o))
}

func entries(t *testing.T, o *Objects) int {
	t.Helper()
	var k egressNatKey
	var v egressNatVal
	n := 0
	it := o.Nat.Iterate()
	for it.Next(&k, &v) {
		n++
	}
	require.NoError(t, it.Err())
	return n
}

// A rewrite removes what is stale and keeps the ids of what stayed.
func TestSyncEBPFStage(t *testing.T) {
	s := newStageH(t)
	slots, err := s.o.RecoverPodSlots()
	require.NoError(t, err)
	assert.Equal(t, map[uint8]SlotNAT{stageSlot: {Addr: egress4, First: stageFirst, Last: stageLast},
		stageSlot + 1: {Addr: egress6, First: stageFirst, Last: stageLast}}, slots)
	require.NoError(t, s.o.ClearEBPFStage())
	for _, m := range []*ebpf.Map{s.o.PodSlots, s.o.PodSetPolicies, s.o.PodPolicies, s.o.PodDest4, s.o.PodDest6, s.o.PodDestPorts, s.o.PodReserved} {
		var k, v []byte
		it := m.Iterate()
		assert.False(t, it.Next(&k, &v), "%v is empty", m)
	}
	// With no pod-set, nothing classifies.
	ret, _, _ := s.out(s.pkt(families[0], protoTCP, tcpSeg(40000, 443, flagSYN)))
	assert.Equal(t, tcxNext, ret)
	// A slice that isn't a power of two can't be written.
	s.stage.Slots[stageSlot] = Slot{Addr: egress4, First: 20480, Last: 20482}
	assert.Error(t, s.o.SyncEBPFStage(s.stage, nil))
}

func TestPortBlocks(t *testing.T) {
	cover := func(first, last uint16) {
		seen := map[uint32]bool{}
		for _, b := range portBlocks(first, last) {
			size := uint32(1) << (16 - b.bits)
			assert.Zero(t, uint32(b.first)&(size-1), "aligned")
			for p := uint32(b.first); p < uint32(b.first)+size; p++ {
				assert.False(t, seen[p])
				seen[p] = true
			}
		}
		assert.Equal(t, int(last)-int(first)+1, len(seen))
		assert.True(t, seen[uint32(first)] && seen[uint32(last)])
	}
	cover(443, 443)
	cover(80, 81)
	cover(1, 65535)
	cover(0, 65535)
	cover(1024, 2047)
	cover(1023, 2048)
	assert.Len(t, portBlocks(0, 65535), 1)
}

// pl_pod_tag's part on an eBPF-stage node: a pod's UDP to a node's tunnel port
// is dropped (D20, resolution 15), and its SYN leaving the cluster counted.
func TestPodTagStage(t *testing.T) {
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			s := newStageH(t)
			nodeAddr := otherV4
			if f.v6 {
				nodeAddr = otherV6
			}
			require.NoError(t, s.o.SetNode(other, Node{V4: otherV4, V6: otherV6, MAC: otherMAC}))
			require.NoError(t, s.o.SetPodDev(podIfindex, PodDev{V4: pod4, V6: pod6, PodSet: stagePodSet}))
			tag := func(pkt []byte) uint32 {
				var in, ctx skbCtx
				binary.NativeEndian.PutUint32(in[40:], podIfindex)
				data := make([]byte, len(pkt)+256)
				ret, err := s.o.PlPodTag.Run(&ebpf.RunOptions{Data: pkt, DataOut: data, Context: in[:], ContextOut: ctx[:]})
				require.NoError(t, err)
				return ret
			}
			assert.Equal(t, uint32(tcxDrop), tag(packet(f, s.pod[f.v6], nodeAddr, protoUDP, ports(40000, 6082))), "to a node's tunnel port")
			assert.Equal(t, uint64(1), s.count("tunnel_port", f.v6))
			assert.Equal(t, tcxNext, tag(packet(f, s.pod[f.v6], f.server, protoUDP, ports(40000, 6082))), "not a node")
			assert.Equal(t, tcxNext, tag(packet(f, s.pod[f.v6], nodeAddr, protoUDP, ports(40000, 53))), "another port")
			assert.Equal(t, tcxNext, tag(s.pkt(f, protoTCP, tcpSeg(40000, 443, flagSYN))))
			assert.Equal(t, uint64(1), s.count("pod_syn", f.v6))
			assert.Equal(t, tcxNext, tag(s.pkt(f, protoTCP, tcpSeg(40000, 443, flagACK))))
			assert.Equal(t, uint64(1), s.count("pod_syn", f.v6), "only a SYN")
		})
	}
}

// ---- end to end, in namespaces ---------------------------------------------------

// stageTopo is a pod, its node and a server, direct: pl_pod_tag on the pod's
// device, pl_pod_out and pl_announce on the node's uplink, as on an eBPF-stage
// node. The node forwards with no nft and no conntrack.
type stageTopo struct {
	pod, node, srv *host
	o              *Objects
	egress, server netip.Addr
	podAddr        netip.Addr
	cluster        netip.Addr
	inner6         bool
}

func newStageTopo(t *testing.T, v6 bool, mtu uint16, mss uint16) *stageTopo {
	t.Helper()
	pod, nd, srv := newHost(t, "pod"), newHost(t, "node"), newHost(t, "server")
	wire(t, pod, "eth0", nd, "vpod")
	wire(t, nd, "ul", srv, "eth0")
	pod.addr(t, "eth0", "10.244.1.5/24")
	pod.addr(t, "eth0", "fd44:1::5/64")
	pod.route(t, &netlink.Route{Gw: net.ParseIP("10.244.1.1")})
	pod.route(t, &netlink.Route{Gw: net.ParseIP("fd44:1::1")})
	nd.addr(t, "vpod", "10.244.1.1/24")
	nd.addr(t, "vpod", "fd44:1::1/64")
	nd.addr(t, "ul", "172.30.10.2/24")
	nd.addr(t, "ul", "fdaa::2/64")
	nd.addr(t, "ul", "172.30.10.110/32")
	nd.addr(t, "ul", "fdaa::110/128")
	nd.route(t, &netlink.Route{Dst: cidr("10.96.7.1/32"), Gw: net.ParseIP("172.30.10.1")})
	nd.route(t, &netlink.Route{Dst: cidr("fd05::1/128"), Gw: net.ParseIP("fdaa::1")})
	srv.addr(t, "eth0", "172.30.10.1/24")
	srv.addr(t, "eth0", "fdaa::1/64")
	srv.addr(t, "eth0", "10.96.7.1/32")
	srv.addr(t, "eth0", "fd05::1/128")
	srv.route(t, &netlink.Route{Dst: cidr("10.244.1.0/24"), Gw: net.ParseIP("172.30.10.2")})
	srv.route(t, &netlink.Route{Dst: cidr("fd44:1::/64"), Gw: net.ParseIP("fdaa::2")})
	nd.sysctl(t, "net.ipv4.ip_forward", "1")
	nd.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	nd.sysctl(t, "net.ipv4.conf.all.rp_filter", "0")

	o := load(t)
	self := NodeID("node")
	require.NoError(t, o.SetConfig(Config{Self: self, TunIfindex: 1, L2: false, Underlay4: true,
		Self4: netip.MustParseAddr("172.30.10.2"), EBPFStage: true, TunPort: 6082}))
	eg4, eg6 := netip.MustParseAddr("172.30.10.110"), netip.MustParseAddr("fdaa::110")
	for _, e := range []netip.Addr{eg4, eg6} {
		require.NoError(t, o.SetAddress(e, Address{Held: true, SliceSize: 4096, Base: 1024, Table: 254}))
		require.NoError(t, o.SetSlice(e, sliceIndex, self))
	}
	podL := pod.link(t, "eth0")
	var mac [6]byte
	copy(mac[:], podL.Attrs().HardwareAddr)
	podIP4, podIP6 := netip.MustParseAddr("10.244.1.5"), netip.MustParseAddr("fd44:1::5")
	require.NoError(t, o.SetPodDev(nd.link(t, "vpod").Attrs().Index, PodDev{V4: podIP4, V6: podIP6, PodSet: stagePodSet, MAC: mac}))
	require.NoError(t, o.SyncCluster([]netip.Prefix{netip.MustParsePrefix("10.96.7.0/24"), netip.MustParsePrefix("fd05::/64")}))
	all := []Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}}
	st := PodStage{
		PodSets: map[uint8][]string{stagePodSet: {"p"}},
		Policies: map[string]Policy{"p": {
			V4: &PolicyFamily{Include: all, Slot: stageSlot, Address: eg4},
			V6: &PolicyFamily{Include: all, Slot: stageSlot + 1, Address: eg6}}},
		Slots: map[uint8]Slot{
			stageSlot:     {Addr: eg4, First: stageFirst, Last: stageLast, MSS: mss},
			stageSlot + 1: {Addr: eg6, First: stageFirst, Last: stageLast, MSS: mss}},
	}
	require.NoError(t, o.SyncEBPFStage(st, map[uint8]SlotRoute{
		stageSlot:     {Target: TargetDirect, Table: 254, MTU: mtu},
		stageSlot + 1: {Target: TargetDirect, Table: 254, MTU: mtu}}))
	nd.attach(t, "vpod", o.PlPodTag, ebpf.AttachTCXIngress)
	nd.attach(t, "ul", o.PlPodOutEth, ebpf.AttachTCXEgress)
	nd.attach(t, "ul", o.PlAnnounceEth, ebpf.AttachTCXIngress)

	s := &stageTopo{pod: pod, node: nd, srv: srv, o: o, inner6: v6, egress: eg4, server: netip.MustParseAddr("172.30.10.1"),
		podAddr: podIP4, cluster: netip.MustParseAddr("10.96.7.1")}
	if v6 {
		s.egress, s.server, s.podAddr, s.cluster = eg6, netip.MustParseAddr("fdaa::1"), podIP6, netip.MustParseAddr("fd05::1")
	}
	return s
}

func (s *stageTopo) counts() map[string][2]uint64 {
	c, _ := s.o.Counts()
	return nonzero(c)
}

// tcpEcho starts a TCP server that says who it sees and echoes, and hands each
// connection's TCP_MAXSEG back through mss.
func tcpEcho(t *testing.T, h *host, addr string, mss chan int) {
	t.Helper()
	var ln net.Listener
	require.NoError(t, h.in(func() error {
		var err error
		ln, err = net.Listen("tcp", addr)
		return err
	}))
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if mss != nil {
					raw, _ := c.(*net.TCPConn).SyscallConn()
					_ = raw.Control(func(fd uintptr) {
						v, _ := unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG)
						select {
						case mss <- v:
						default:
						}
					})
				}
				fmt.Fprintf(c, "%s\n", c.RemoteAddr())
				buf := make([]byte, 65536)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
}

// udpEcho answers each datagram with the source it saw, then its bytes.
func udpEcho(t *testing.T, h *host, addr string) {
	t.Helper()
	var pc net.PacketConn
	require.NoError(t, h.in(func() error {
		var err error
		pc, err = net.ListenPacket("udp", addr)
		return err
	}))
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			out := append([]byte(from.String()+"\n"), buf[:n]...)
			_, _ = pc.WriteTo(out, from)
		}
	}()
}

func seenInSlice(t *testing.T, seen string, want netip.Addr) {
	t.Helper()
	ap, err := netip.ParseAddrPort(strings.TrimSpace(seen))
	require.NoError(t, err, "%q", seen)
	assert.Equal(t, want, ap.Addr().Unmap(), "the source the far end sees")
	assert.GreaterOrEqual(t, ap.Port(), uint16(stageFirst))
	assert.LessOrEqual(t, ap.Port(), uint16(stageLast))
}

func TestNetnsEBPFStage(t *testing.T) {
	netnsOrSkip(t)
	for _, v6 := range []bool{false, true} {
		name := "IPv4"
		if v6 {
			name = "IPv6"
		}
		t.Run(name, func(t *testing.T) { podStageCase(t, v6) })
	}
}

func podStageCase(t *testing.T, v6 bool) {
	s := newStageTopo(t, v6, 0, 1000)
	mss := make(chan int, 4)
	hp := func(a netip.Addr, port int) string { return net.JoinHostPort(a.String(), fmt.Sprint(port)) }
	tcpEcho(t, s.srv, hp(s.server, 8080), mss)
	tcpEcho(t, s.srv, hp(s.cluster, 8081), nil)
	udpEcho(t, s.srv, hp(s.server, 8053))
	fam := 0
	if v6 {
		fam = 1
	}

	// TCP: the egress address and a slice port, data both ways, in GSO-sized
	// writes, both ends clamped.
	require.NoError(t, s.pod.in(func() error {
		c, err := net.DialTimeout("tcp", hp(s.server, 8080), 5*time.Second)
		if err != nil {
			return fmt.Errorf("%w; %v", err, s.counts())
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		r := bufio.NewReader(c)
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		seenInSlice(t, line, s.egress)
		raw, _ := c.(*net.TCPConn).SyscallConn()
		_ = raw.Control(func(fd uintptr) {
			v, _ := unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG)
			assert.LessOrEqual(t, v, 1000, "the server's MSS, clamped on the SYN-ACK")
		})
		const total = 2 << 20
		errc := make(chan error, 1)
		go func() {
			_, err := c.Write(make([]byte, total))
			errc <- err
		}()
		got := 0
		buf := make([]byte, 65536)
		for got < total {
			n, err := r.Read(buf)
			if err != nil {
				return fmt.Errorf("after %d bytes: %w; %v", got, err, s.counts())
			}
			got += n
		}
		return <-errc
	}))
	assert.LessOrEqual(t, <-mss, 1000, "the pod's MSS, clamped on the SYN")

	// UDP, and fragments both ways: 4000 bytes over a 1500-byte link.
	require.NoError(t, s.pod.in(func() error {
		c, err := net.DialTimeout("udp", hp(s.server, 8053), time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		for _, size := range []int{100, 4000} {
			if _, err := c.Write(make([]byte, size)); err != nil {
				return err
			}
			buf := make([]byte, 65536)
			n, err := c.Read(buf)
			if err != nil {
				return fmt.Errorf("%d bytes: %w; %v", size, err, s.counts())
			}
			line, _, _ := strings.Cut(string(buf[:n]), "\n")
			seenInSlice(t, line, s.egress)
			assert.Equal(t, size, n-len(line)-1, "whole")
		}
		return nil
	}))
	assert.Zero(t, s.counts()["frag_untracked"][fam], "every later fragment followed its first")

	// An ICMP error comes back through its quote: a closed port refuses.
	require.NoError(t, s.pod.in(func() error {
		c, err := net.Dial("udp", hp(s.server, 9))
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		_, err = c.Read(make([]byte, 16))
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("want connection refused, got %v; %v", err, s.counts())
		}
		return nil
	}))
	assert.NotZero(t, s.counts()["nat_icmp_error"][fam])

	// ICMP echo: the identifier moves into the slice and back.
	require.NoError(t, s.pod.in(func() error {
		netw, typ, want := "ip4:icmp", byte(8), byte(0)
		if v6 {
			netw, typ, want = "ip6:ipv6-icmp", 128, 129
		}
		pc, err := net.ListenPacket(netw, "")
		if err != nil {
			return err
		}
		defer pc.Close()
		msg := []byte{typ, 0, 0, 0, 0x12, 0x34, 0, 1, 'p', 'l'}
		if !v6 {
			cs := checksum(msg)
			msg[2], msg[3] = byte(cs>>8), byte(cs)
		}
		if _, err := pc.WriteTo(msg, &net.IPAddr{IP: s.server.AsSlice()}); err != nil {
			return err
		}
		_ = pc.SetDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 1500)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return fmt.Errorf("echo: %w; %v", err, s.counts())
			}
			if n >= 8 && buf[0] == want && buf[4] == 0x12 && buf[5] == 0x34 {
				return nil
			}
		}
	}))

	// A cluster destination isn't egress: the far end sees the pod.
	var seen string
	require.NoError(t, s.pod.in(func() error {
		c, err := net.DialTimeout("tcp", hp(s.cluster, 8081), 5*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		seen, err = bufio.NewReader(c).ReadString('\n')
		return err
	}))
	ap, err := netip.ParseAddrPort(strings.TrimSpace(seen))
	require.NoError(t, err)
	assert.Equal(t, s.podAddr, ap.Addr().Unmap())

	// Flows into the pod: TCP needs no record, UDP takes pl_pod_inbound's. The
	// pod's replies keep its address.
	cg, err := link.AttachCgroup(link.CgroupOptions{Path: "/sys/fs/cgroup", Attach: ebpf.AttachCGroupInetIngress, Program: s.o.PlPodInbound})
	if err != nil {
		skipOrFailNetns(t, fmt.Sprintf("attaching to the root cgroup: %v", err))
		return
	}
	defer cg.Close()
	tcpEcho(t, s.pod, hp(s.podAddr, 7070), nil)
	udpEcho(t, s.pod, hp(s.podAddr, 7053))
	require.NoError(t, s.srv.in(func() error {
		c, err := net.DialTimeout("tcp", hp(s.podAddr, 7070), 5*time.Second)
		if err != nil {
			return fmt.Errorf("inbound TCP: %w; %v", err, s.counts())
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
			return fmt.Errorf("inbound TCP: %w; %v", err, s.counts())
		}
		return nil
	}))
	assert.NotZero(t, s.counts()["nat_midstream"][fam])
	flows, err := s.o.NatFlows()
	require.NoError(t, err)
	before := len(flows)
	require.NoError(t, s.srv.in(func() error {
		c, err := net.Dial("udp", hp(s.podAddr, 7053))
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		// A connected socket only hears the address it sent to: a translated
		// reply never arrives.
		if _, err := c.Read(make([]byte, 128)); err != nil {
			return fmt.Errorf("inbound UDP: %w; %v", err, s.counts())
		}
		return nil
	}))
	c := s.counts()
	assert.NotZero(t, c["inbound_recorded"][fam])
	assert.NotZero(t, c["inbound_reply"][fam])
	flows, err = s.o.NatFlows()
	require.NoError(t, err)
	assert.Len(t, flows, before, "no NAT flow for it")
}

// An oversize packet comes back to the pod as an error, and its path MTU falls.
func TestNetnsEBPFStagePTB(t *testing.T) {
	netnsOrSkip(t)
	for _, v6 := range []bool{false, true} {
		name := "IPv4"
		if v6 {
			name = "IPv6"
		}
		t.Run(name, func(t *testing.T) {
			s := newStageTopo(t, v6, 1300, 0)
			udpEcho(t, s.srv, net.JoinHostPort(s.server.String(), "8053"))
			fam := 0
			if v6 {
				fam = 1
			}
			require.NoError(t, s.pod.in(func() error {
				c, err := net.Dial("udp", net.JoinHostPort(s.server.String(), "8053"))
				if err != nil {
					return err
				}
				defer c.Close()
				raw, _ := c.(*net.UDPConn).SyscallConn()
				opt, optMTU, lvl := unix.IP_MTU_DISCOVER, unix.IP_MTU, unix.IPPROTO_IP
				val := unix.IP_PMTUDISC_DO
				if v6 {
					opt, optMTU, lvl = unix.IPV6_MTU_DISCOVER, unix.IPV6_MTU, unix.IPPROTO_IPV6
					val = unix.IPV6_PMTUDISC_DO
				}
				var serr error
				_ = raw.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), lvl, opt, val) })
				if serr != nil {
					return serr
				}
				_, _ = c.Write(make([]byte, 1400))
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					var mtu int
					_ = raw.Control(func(fd uintptr) { mtu, serr = unix.GetsockoptInt(int(fd), lvl, optMTU) })
					if serr == nil && mtu == 1300 {
						return nil
					}
					// Each failed write clears the socket's pending error.
					_, _ = c.Write(make([]byte, 1400))
					time.Sleep(50 * time.Millisecond)
				}
				return fmt.Errorf("the path MTU never fell to 1300; %v", s.counts())
			}))
			assert.NotZero(t, s.counts()["ptb_sent"][fam])
			// What fits still leaves translated.
			require.NoError(t, s.pod.in(func() error {
				c, err := net.Dial("udp", net.JoinHostPort(s.server.String(), "8053"))
				if err != nil {
					return err
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := c.Write(make([]byte, 1000)); err != nil {
					return err
				}
				buf := make([]byte, 4096)
				n, err := c.Read(buf)
				if err != nil {
					return err
				}
				line, _, _ := strings.Cut(string(buf[:n]), "\n")
				seenInSlice(t, line, s.egress)
				return nil
			}))
		})
	}
}

// The tunnel path: a pod on A leaves through B, which holds the address, and
// replies come back through the tunnel and into the pod past the stack.
func TestNetnsEBPFStageTunnel(t *testing.T) {
	netnsOrSkip(t)
	for _, inner6 := range []bool{false, true} {
		for _, under6 := range []bool{false, true} {
			for _, l2 := range []bool{false, true} {
				c := tunCase{inner6, under6, l2}
				t.Run(c.String(), func(t *testing.T) { stageTunnelCase(t, c) })
			}
		}
	}
}

func stageTunnelCase(t *testing.T, c tunCase) {
	nodeA, nodeB := NodeID("node-a"), NodeID("node-b")
	pod, a, b, srv := newHost(t, "pod"), newHost(t, "nodeA"), newHost(t, "nodeB"), newHost(t, "server")
	wire(t, pod, "eth0", a, "vpod")
	wire(t, a, "ul", b, "ul")
	wire(t, b, "pool", srv, "eth0")
	eg4, eg6 := netip.MustParseAddr("172.30.10.110"), netip.MustParseAddr("fdaa::110")
	server, egress := netip.MustParseAddr("172.30.10.1"), eg4
	if c.inner6 {
		server, egress = netip.MustParseAddr("fdaa::1"), eg6
	}
	pod.addr(t, "eth0", "10.244.1.5/24")
	pod.addr(t, "eth0", "fd44:1::5/64")
	pod.route(t, &netlink.Route{Gw: net.ParseIP("10.244.1.1")})
	pod.route(t, &netlink.Route{Gw: net.ParseIP("fd44:1::1")})
	a.addr(t, "vpod", "10.244.1.1/24")
	a.addr(t, "vpod", "fd44:1::1/64")
	a.addr(t, "ul", "10.0.0.1/24")
	a.addr(t, "ul", "fd00::1/64")
	// The node's own route out, whatever the destination: the stage leaves
	// from NIC egress, so a pod's packet has to be routed to one.
	a.route(t, &netlink.Route{Gw: net.ParseIP("10.0.0.2")})
	a.route(t, &netlink.Route{Gw: net.ParseIP("fd00::2")})
	b.addr(t, "ul", "10.0.0.2/24")
	b.addr(t, "ul", "fd00::2/64")
	b.addr(t, "pool", "172.30.10.2/24")
	b.addr(t, "pool", "fdaa::2/64")
	b.addr(t, "pool", "172.30.10.110/32")
	b.addr(t, "pool", "fdaa::110/128")
	srv.addr(t, "eth0", "172.30.10.1/24")
	srv.addr(t, "eth0", "fdaa::1/64")
	for _, h := range []*host{a, b} {
		h.sysctl(t, "net.ipv4.ip_forward", "1")
		h.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
		h.sysctl(t, "net.ipv4.conf.all.rp_filter", "0")
	}
	mode := ModeL3
	if c.l2 {
		mode = ModeL2
	}
	var tunA, tunB netlink.Link
	for _, x := range []struct {
		h  *host
		id uint64
		l  *netlink.Link
	}{{a, nodeA, &tunA}, {b, nodeB, &tunB}} {
		got, l, err := EnsureTunnel(x.h.nl, mode, 6082, TunnelMAC(x.id))
		require.NoError(t, err)
		require.Equal(t, mode, got)
		*x.l = l
	}
	b.sysctl(t, "net.ipv4.conf.purelb-eg0.rp_filter", "0")
	b.route(t, &netlink.Route{Dst: cidr("172.30.10.0/24"), LinkIndex: b.link(t, "pool").Attrs().Index, Table: 7900})
	b.route(t, &netlink.Route{Dst: cidr("fdaa::/64"), LinkIndex: b.link(t, "pool").Attrs().Index, Table: 7900})

	objA, objB := load(t), load(t)
	underA, underB := Node{V4: netip.MustParseAddr("10.0.0.1")}, Node{V4: netip.MustParseAddr("10.0.0.2")}
	if c.under6 {
		underA, underB = Node{V6: netip.MustParseAddr("fd00::1")}, Node{V6: netip.MustParseAddr("fd00::2")}
	}
	underA.MAC, underB.MAC = TunnelMAC(nodeA), TunnelMAC(nodeB)
	require.NoError(t, objA.SetConfig(Config{Self: nodeA, TunIfindex: tunA.Attrs().Index, L2: c.l2, TunMAC: TunnelMAC(nodeA),
		Underlay4: !c.under6, Underlay6: c.under6, Self4: underA.V4, Self6: underA.V6, EBPFStage: true, TunPort: 6082}))
	require.NoError(t, objB.SetConfig(Config{Self: nodeB, TunIfindex: tunB.Attrs().Index, L2: c.l2, TunMAC: TunnelMAC(nodeB),
		Underlay4: !c.under6, Underlay6: c.under6, Self4: underB.V4, Self6: underB.V6}))
	require.NoError(t, objA.SetNode(nodeB, underB))
	require.NoError(t, objB.SetNode(nodeA, underA))
	require.NoError(t, objB.SetPoolInterface(b.link(t, "pool").Attrs().Index, 0))
	for _, e := range []netip.Addr{eg4, eg6} {
		require.NoError(t, objA.SetAddress(e, Address{Held: false, SliceSize: 4096, Base: 1024}))
		require.NoError(t, objB.SetAddress(e, Address{Held: true, SliceSize: 4096, Base: 1024, Table: 7900}))
		require.NoError(t, objA.SetSlice(e, sliceIndex, nodeA))
		require.NoError(t, objB.SetSlice(e, sliceIndex, nodeA))
	}
	all := []Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}}
	require.NoError(t, objA.SyncEBPFStage(PodStage{
		PodSets: map[uint8][]string{stagePodSet: {"p"}},
		Policies: map[string]Policy{"p": {
			V4: &PolicyFamily{Include: all, Slot: stageSlot, Address: eg4},
			V6: &PolicyFamily{Include: all, Slot: stageSlot + 1, Address: eg6}}},
		Slots: map[uint8]Slot{
			// The clamp the agent sets from the tunnel's path MTU (D13): an
			// IPv6 outer packet isn't fragmented.
			stageSlot:     {Addr: eg4, First: stageFirst, Last: stageLast, MSS: 1300},
			stageSlot + 1: {Addr: eg6, First: stageFirst, Last: stageLast, MSS: 1300}},
	}, map[uint8]SlotRoute{stageSlot: {Target: TargetTunnel}, stageSlot + 1: {Target: TargetTunnel}}))
	require.NoError(t, objA.SetSteer(stageSlot, []uint64{nodeB}))
	require.NoError(t, objA.SetSteer(stageSlot+1, []uint64{nodeB}))
	var mac [6]byte
	copy(mac[:], pod.link(t, "eth0").Attrs().HardwareAddr)
	require.NoError(t, objA.SetPodDev(a.link(t, "vpod").Attrs().Index, PodDev{V4: netip.MustParseAddr("10.244.1.5"),
		V6: netip.MustParseAddr("fd44:1::5"), PodSet: stagePodSet, MAC: mac}))
	a.attach(t, "vpod", objA.PlPodTag, ebpf.AttachTCXIngress)
	a.attach(t, "ul", objA.PlPodOutEth, ebpf.AttachTCXEgress)
	a.attach(t, TunnelName, objA.PlSteer, ebpf.AttachTCXEgress)
	a.attach(t, TunnelName, objA.PlAnnounceTun, ebpf.AttachTCXIngress)
	b.attach(t, "pool", objB.PlAnnounceEth, ebpf.AttachTCXIngress)
	b.attach(t, TunnelName, objB.PlAnnounceTun, ebpf.AttachTCXIngress)

	hp := net.JoinHostPort(server.String(), "8080")
	tcpEcho(t, srv, hp, nil)
	diag := func() string {
		ca, _ := objA.Counts()
		cb, _ := objB.Counts()
		return fmt.Sprintf("A: %v B: %v", nonzero(ca), nonzero(cb))
	}
	require.NoError(t, pod.in(func() error {
		conn, err := net.DialTimeout("tcp", hp, 5*time.Second)
		if err != nil {
			return fmt.Errorf("%w; %s", err, diag())
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("%w; %s", err, diag())
		}
		seenInSlice(t, line, egress)
		// A bulk transfer: GSO through the tunnel and back.
		const total = 1 << 20
		go func() { _, _ = conn.Write(make([]byte, total)) }()
		buf := make([]byte, 65536)
		for got := 0; got < total; {
			n, err := r.Read(buf)
			if err != nil {
				return fmt.Errorf("after %d: %w; %s", got, err, diag())
			}
			got += n
		}
		return nil
	}))
	ca, _ := objA.Counts()
	cb, _ := objB.Counts()
	fam := 0
	if c.inner6 {
		fam = 1
	}
	assert.NotZero(t, ca["nat_out"][fam], "A translated")
	assert.NotZero(t, ca["steered"][fam], "A's pl_steer keyed it")
	assert.NotZero(t, cb["transit"][fam], "B forwarded it")
	assert.NotZero(t, cb["to_tunnel"][fam], "B tunnelled the replies")
	assert.NotZero(t, ca["nat_reply"][fam], "A took them back, into the pod")
}

// A netkit pod: pl_pod_tag at the peer hook, replies by bpf_redirect_peer to
// the primary, the pod found by source address when no ingress device names
// it.
func TestNetnsEBPFStageNetkit(t *testing.T) {
	netnsOrSkip(t)
	for _, v6 := range []bool{false, true} {
		name := "IPv4"
		if v6 {
			name = "IPv6"
		}
		t.Run(name, func(t *testing.T) {
			pod, nd, srv := newHost(t, "pod"), newHost(t, "node"), newHost(t, "server")
			nk := &netlink.Netkit{LinkAttrs: netlink.LinkAttrs{Name: "nk0"}, Mode: netlink.NETKIT_MODE_L3,
				Policy: netlink.NETKIT_POLICY_FORWARD, PeerPolicy: netlink.NETKIT_POLICY_FORWARD}
			peer := netlink.NewLinkAttrs()
			peer.Name = "eth0"
			peer.Namespace = netlink.NsFd(int(pod.ns))
			nk.SetPeerAttrs(&peer)
			if err := nd.nl.LinkAdd(nk); err != nil {
				t.Skipf("no netkit on this kernel (it needs 6.7): %v", err)
			}
			host := nd.link(t, "nk0")
			require.NoError(t, nd.nl.LinkSetUp(host))
			require.NoError(t, pod.nl.LinkSetUp(pod.link(t, "eth0")))
			wire(t, nd, "ul", srv, "eth0")
			pod.addr(t, "eth0", "10.244.1.5/32")
			pod.addr(t, "eth0", "fd44:1::5/128")
			pEth := pod.link(t, "eth0").Attrs().Index
			pod.route(t, &netlink.Route{Dst: cidr("0.0.0.0/0"), LinkIndex: pEth, Scope: netlink.SCOPE_LINK})
			pod.route(t, &netlink.Route{Dst: cidr("::/0"), LinkIndex: pEth})
			nd.addr(t, "ul", "172.30.10.2/24")
			nd.addr(t, "ul", "fdaa::2/64")
			nd.addr(t, "ul", "172.30.10.110/32")
			nd.addr(t, "ul", "fdaa::110/128")
			nd.route(t, &netlink.Route{Dst: cidr("10.244.1.5/32"), LinkIndex: host.Attrs().Index, Scope: netlink.SCOPE_LINK})
			nd.route(t, &netlink.Route{Dst: cidr("fd44:1::5/128"), LinkIndex: host.Attrs().Index})
			srv.addr(t, "eth0", "172.30.10.1/24")
			srv.addr(t, "eth0", "fdaa::1/64")
			nd.sysctl(t, "net.ipv4.ip_forward", "1")
			nd.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
			nd.sysctl(t, "net.ipv4.conf.all.rp_filter", "0")

			o := load(t)
			self := NodeID("node")
			eg4, eg6 := netip.MustParseAddr("172.30.10.110"), netip.MustParseAddr("fdaa::110")
			require.NoError(t, o.SetConfig(Config{Self: self, TunIfindex: 1, Underlay4: true,
				Self4: netip.MustParseAddr("172.30.10.2"), EBPFStage: true, PodBySrc: true, TunPort: 6082}))
			for _, e := range []netip.Addr{eg4, eg6} {
				require.NoError(t, o.SetAddress(e, Address{Held: true, SliceSize: 4096, Base: 1024, Table: 254}))
				require.NoError(t, o.SetSlice(e, sliceIndex, self))
			}
			require.NoError(t, o.SetPodDev(host.Attrs().Index, PodDev{V4: netip.MustParseAddr("10.244.1.5"),
				V6: netip.MustParseAddr("fd44:1::5"), PodSet: stagePodSet}))
			all := []Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}}
			require.NoError(t, o.SyncEBPFStage(PodStage{
				PodSets: map[uint8][]string{stagePodSet: {"p"}},
				Policies: map[string]Policy{"p": {
					V4: &PolicyFamily{Include: all, Slot: stageSlot, Address: eg4},
					V6: &PolicyFamily{Include: all, Slot: stageSlot + 1, Address: eg6}}},
				Slots: map[uint8]Slot{
					stageSlot:     {Addr: eg4, First: stageFirst, Last: stageLast},
					stageSlot + 1: {Addr: eg6, First: stageFirst, Last: stageLast}},
			}, map[uint8]SlotRoute{stageSlot: {Target: TargetDirect, Table: 254}, stageSlot + 1: {Target: TargetDirect, Table: 254}}))
			var kl link.Link
			require.NoError(t, nd.in(func() error {
				var err error
				kl, err = link.AttachNetkit(link.NetkitOptions{Interface: host.Attrs().Index, Program: o.PlPodTag,
					Attach: ebpf.AttachNetkitPeer, Anchor: link.Head()})
				return err
			}))
			t.Cleanup(func() { _ = kl.Close() })
			nd.attach(t, "ul", o.PlPodOutEth, ebpf.AttachTCXEgress)
			nd.attach(t, "ul", o.PlAnnounceEth, ebpf.AttachTCXIngress)

			server, egress := "172.30.10.1", eg4
			if v6 {
				server, egress = "fdaa::1", eg6
			}
			hp := net.JoinHostPort(server, "8080")
			tcpEcho(t, srv, hp, nil)
			counts := func() map[string][2]uint64 {
				c, _ := o.Counts()
				return nonzero(c)
			}
			require.NoError(t, pod.in(func() error {
				c, err := net.DialTimeout("tcp", hp, 5*time.Second)
				if err != nil {
					return fmt.Errorf("%w; %v", err, counts())
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return fmt.Errorf("%w; %v", err, counts())
				}
				seenInSlice(t, line, egress)
				return nil
			}))
			// A spoofed source dies at the peer hook, ahead of everything.
			fam := 0
			if v6 {
				fam = 1
			}
			spoof, sp := "10.244.1.99/32", "10.244.1.99"
			if v6 {
				spoof, sp = "fd44:1::99/128", "fd44:1::99"
			}
			pod.addr(t, "eth0", spoof)
			require.NoError(t, pod.in(func() error {
				d := net.Dialer{Timeout: 500 * time.Millisecond, LocalAddr: &net.TCPAddr{IP: net.ParseIP(sp)}}
				if c, err := d.Dial("tcp", hp); err == nil {
					c.Close()
					return errors.New("a spoofed source got through")
				}
				return nil
			}))
			assert.NotZero(t, counts()["source_check"][fam])
		})
	}
}

// What the eBPF stage costs per packet, the guard's way (BPF_PROG_TEST_RUN, the
// best of five), against spike 10's budgets. Logged; the budgets are checked by
// hand on a reference machine, as the guard's (shared runners are noisy).
func TestEBPFStageCost(t *testing.T) {
	s := newStageH(t)
	f := families[0]
	// A repeated run sees the packet it already rewrote, so each path is timed
	// one run at a time, the median of many, less the harness's own cost (what
	// the cheapest path takes the same way). Benchmark runs take no context:
	// the pod's device here is index 0.
	once := func(pkt func(i int) []byte) time.Duration {
		var ds []time.Duration
		for i := range 2000 {
			_, d, err := s.o.PlPodOutEth.Benchmark(pkt(i), 1, nil)
			require.NoError(t, err)
			ds = append(ds, d)
		}
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	established := s.pkt(f, protoTCP, tcpSeg(40000, 443, flagACK))
	base := once(func(int) []byte { return established })
	t.Logf("pl_pod_out, not from a pod (the baseline, one run each): %v", base)
	require.NoError(t, s.o.SetPodDev(0, PodDev{V4: pod4, V6: pod6}))
	t.Logf("pl_pod_out, an unselected pod: +%v", once(func(int) []byte { return established })-base)
	require.NoError(t, s.o.SetPodDev(0, PodDev{V4: pod4, V6: pod6, PodSet: stagePodSet}))
	_, _, err := s.o.PlPodOutEth.Benchmark(s.pkt(f, protoTCP, tcpSeg(40000, 443, flagSYN)), 1, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), s.count("nat_new", false))
	before := s.count("nat_out", false)
	t.Logf("pl_pod_out, established TCP: +%v", once(func(int) []byte { return established })-base)
	require.Equal(t, before+2000, s.count("nat_out", false), "the path timed is the flow's")
	require.Equal(t, uint64(1), s.count("nat_new", false))
	t.Logf("pl_pod_out, new flow (1 policy, 2,000 flows): +%v",
		once(func(i int) []byte { return s.pkt(f, protoTCP, tcpSeg(uint16(10000+i), 443, flagSYN)) })-base)
	require.Equal(t, uint64(2001), s.count("nat_new", false))
}

// An address a newer pod took keeps its entry when the old pod's device goes.
func TestPodSrcReusedAddress(t *testing.T) {
	o := load(t)
	require.NoError(t, o.SetPodDev(3, PodDev{V4: pod4, V6: pod6}))
	require.NoError(t, o.SetPodDev(4, PodDev{V4: pod4}))
	require.NoError(t, o.DeletePodDev(3))
	var ifx uint32
	k4, k6 := addrKey(pod4), addrKey(pod6)
	require.NoError(t, o.PodSrc.Lookup(&k4, &ifx), "the newer pod's")
	assert.Equal(t, uint32(4), ifx)
	assert.Error(t, o.PodSrc.Lookup(&k6, &ifx), "the old pod's alone goes")
}
