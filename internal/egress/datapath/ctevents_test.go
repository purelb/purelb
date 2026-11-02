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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func be16b(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be64b(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// tuple is a CTA_TUPLE_* attribute: addresses, protocol and ports (an
// ICMP echo's id for ICMP).
func tuple(typ int, src, dst string, proto uint8, sport, dport uint16) *nl.RtAttr {
	t := nl.NewRtAttr(typ|unix.NLA_F_NESTED, nil)
	s, d := netip.MustParseAddr(src), netip.MustParseAddr(dst)
	ip := t.AddRtAttr(ctaTupleIP|unix.NLA_F_NESTED, nil)
	if s.Is4() {
		ip.AddRtAttr(ctaIPv4Src, s.AsSlice())
		ip.AddRtAttr(ctaIPv4Dst, d.AsSlice())
	} else {
		ip.AddRtAttr(ctaIPv6Src, s.AsSlice())
		ip.AddRtAttr(ctaIPv6Dst, d.AsSlice())
	}
	p := t.AddRtAttr(ctaTupleProto|unix.NLA_F_NESTED, nil)
	p.AddRtAttr(ctaProtoNum, []byte{proto})
	switch proto {
	case unix.IPPROTO_ICMP:
		p.AddRtAttr(ctaProtoICMPID, be16b(sport))
	case unix.IPPROTO_ICMPV6:
		p.AddRtAttr(ctaProtoICMPv6ID, be16b(sport))
	default:
		p.AddRtAttr(ctaProtoSrcPort, be16b(sport))
		p.AddRtAttr(ctaProtoDstPort, be16b(dport))
	}
	return t
}

func TestParseCtEvent(t *testing.T) {
	var b []byte
	for _, a := range []*nl.RtAttr{
		tuple(nl.CTA_TUPLE_ORIG, "10.244.1.5", "203.0.113.7", unix.IPPROTO_TCP, 40000, 443),
		tuple(nl.CTA_TUPLE_REPLY, "203.0.113.7", "192.0.2.1", unix.IPPROTO_TCP, 443, 20480),
		nl.NewRtAttr(nl.CTA_MARK, binary.BigEndian.AppendUint32(nil, 0x104)),
	} {
		b = append(b, a.Serialize()...)
	}
	var e CtEvent
	require.True(t, parseCtEvent(b, &e))
	assert.Equal(t, CtEvent{Proto: unix.IPPROTO_TCP, Src: netip.MustParseAddrPort("10.244.1.5:40000"),
		Dst: netip.MustParseAddrPort("203.0.113.7:443"), Nat: netip.MustParseAddrPort("192.0.2.1:20480"), Mark: 0x104}, e)

	// ICMPv6 echo: the id in place of the ports; counters (both
	// directions) and timestamps where the kernel keeps them.
	b = nil
	counters := func(typ int, p, n uint64) *nl.RtAttr {
		c := nl.NewRtAttr(typ|unix.NLA_F_NESTED, nil)
		c.AddRtAttr(ctaCountersPkts, be64b(p))
		c.AddRtAttr(ctaCountersBytes, be64b(n))
		return c
	}
	ts := nl.NewRtAttr(ctaTimestamp|unix.NLA_F_NESTED, nil)
	ts.AddRtAttr(ctaTimestampStart, be64b(1_000_000_000))
	ts.AddRtAttr(ctaTimestampStop, be64b(3_500_000_000))
	for _, a := range []*nl.RtAttr{
		tuple(nl.CTA_TUPLE_ORIG, "fd04::5", "2001:db8::1", unix.IPPROTO_ICMPV6, 77, 0),
		tuple(nl.CTA_TUPLE_REPLY, "2001:db8::1", "fd10::10", unix.IPPROTO_ICMPV6, 8193, 0),
		counters(ctaCountersOrig, 3, 312), counters(ctaCountersReply, 3, 312), ts,
	} {
		b = append(b, a.Serialize()...)
	}
	e = CtEvent{}
	require.True(t, parseCtEvent(b, &e))
	assert.Equal(t, netip.MustParseAddrPort("[fd04::5]:77"), e.Src)
	assert.Equal(t, netip.MustParseAddrPort("[fd10::10]:8193"), e.Nat, "the translated echo id")
	assert.True(t, e.Counted)
	assert.Equal(t, uint64(6), e.Packets)
	assert.Equal(t, uint64(624), e.Bytes)
	assert.Equal(t, 2500*time.Millisecond, e.Stop.Sub(e.Start))

	assert.False(t, parseCtEvent(nl.NewRtAttr(nl.CTA_MARK, binary.BigEndian.AppendUint32(nil, 1)).Serialize(), &CtEvent{}),
		"no original tuple")
	noAddrs := nl.NewRtAttr(nl.CTA_TUPLE_ORIG|unix.NLA_F_NESTED, nil)
	noAddrs.AddRtAttr(ctaTupleProto|unix.NLA_F_NESTED, nil).AddRtAttr(ctaProtoNum, []byte{unix.IPPROTO_TCP})
	assert.False(t, parseCtEvent(noAddrs.Serialize(), &CtEvent{}), "an original tuple without addresses")
}

// A subscribed socket gets a translated flow's NEW and DESTROY events, in
// both families.
func TestNetnsCtEvents(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	var ev *CtEvents
	require.NoError(t, h.in(func() (err error) { ev, err = OpenCtEvents(200 * time.Millisecond); return err }))
	defer ev.Close()
	flow := func(src, dst, nat string, sport uint16, mark uint32) *netlink.ConntrackFlow {
		s, d, n := net.ParseIP(src), net.ParseIP(dst), net.ParseIP(nat)
		fam := uint8(unix.AF_INET6)
		if s.To4() != nil {
			// The library sends a 16-byte IPv4 address as it is.
			fam, s, d, n = unix.AF_INET, s.To4(), d.To4(), n.To4()
		}
		return &netlink.ConntrackFlow{FamilyType: fam,
			Forward: netlink.IPTuple{SrcIP: s, DstIP: d, Protocol: unix.IPPROTO_UDP, SrcPort: sport, DstPort: 53},
			Reverse: netlink.IPTuple{SrcIP: d, DstIP: n, Protocol: unix.IPPROTO_UDP, SrcPort: 53, DstPort: 20480 + sport%10},
			Mark:    mark, TimeOut: 300}
	}
	f4 := flow("10.244.1.5", "203.0.113.9", "192.0.2.1", 1000, 4)
	f6 := flow("fd04::5", "2001:db8:9::9", "fd10::10", 1001, 6)
	require.NoError(t, h.in(func() error {
		for _, f := range []*netlink.ConntrackFlow{f4, f6} {
			if err := netlink.ConntrackCreate(netlink.ConntrackTable, netlink.InetFamily(f.FamilyType), f); err != nil {
				return err
			}
		}
		return netlink.ConntrackTableFlush(netlink.ConntrackTable)
	}))
	var got []CtEvent
	for end := time.Now().Add(3 * time.Second); len(got) < 4 && time.Now().Before(end); {
		es, err := ev.Read()
		require.NoError(t, err)
		got = append(got, es...)
	}
	want := func(isNew bool, src, dst, nat string, mark uint32) CtEvent {
		return CtEvent{New: isNew, Proto: unix.IPPROTO_UDP, Src: netip.MustParseAddrPort(src), Dst: netip.MustParseAddrPort(dst),
			Nat: netip.MustParseAddrPort(nat), Mark: mark}
	}
	assert.ElementsMatch(t, []CtEvent{
		want(true, "10.244.1.5:1000", "203.0.113.9:53", "192.0.2.1:20480", 4),
		want(true, "[fd04::5]:1001", "[2001:db8:9::9]:53", "[fd10::10]:20481", 6),
		want(false, "10.244.1.5:1000", "203.0.113.9:53", "192.0.2.1:20480", 4),
		want(false, "[fd04::5]:1001", "[2001:db8:9::9]:53", "[fd10::10]:20481", 6),
	}, got)
}
