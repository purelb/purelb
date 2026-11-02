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
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// The ctnetlink attributes and groups the flow log reads
// (linux/netfilter/nfnetlink_conntrack.h, nfnetlink.h).
const (
	ctaTupleProto     = 2  // CTA_TUPLE_PROTO
	ctaIPv4Dst        = 2  // CTA_IP_V4_DST
	ctaIPv6Dst        = 4  // CTA_IP_V6_DST
	ctaProtoNum       = 1  // CTA_PROTO_NUM
	ctaProtoSrcPort   = 2  // CTA_PROTO_SRC_PORT
	ctaProtoDstPort   = 3  // CTA_PROTO_DST_PORT
	ctaProtoICMPID    = 4  // CTA_PROTO_ICMP_ID
	ctaProtoICMPv6ID  = 7  // CTA_PROTO_ICMPV6_ID
	ctaCountersOrig   = 9  // CTA_COUNTERS_ORIG
	ctaCountersReply  = 10 // CTA_COUNTERS_REPLY
	ctaCountersPkts   = 1  // CTA_COUNTERS_PACKETS
	ctaCountersBytes  = 2  // CTA_COUNTERS_BYTES
	ctaTimestamp      = 20 // CTA_TIMESTAMP
	ctaTimestampStart = 1  // CTA_TIMESTAMP_START
	ctaTimestampStop  = 2  // CTA_TIMESTAMP_STOP
	nfnlGroupCtNew    = 1  // NFNLGRP_CONNTRACK_NEW
	nfnlGroupCtDel    = 3  // NFNLGRP_CONNTRACK_DESTROY
)

// ctEventBuffer is the event socket's receive buffer.
const ctEventBuffer = 4 << 20

// CtEvent is one conntrack NEW or DESTROY event: the original direction's
// endpoints and the reply direction's destination, which is the original
// source as translated (an ICMP echo's id in place of a port). Packets,
// Bytes and the times are there only where the kernel keeps them
// (nf_conntrack_acct, nf_conntrack_timestamp).
type CtEvent struct {
	New           bool
	Proto         uint8
	Src, Dst, Nat netip.AddrPort
	Mark          uint32
	Counted       bool
	Packets       uint64
	Bytes         uint64
	Start, Stop   time.Time
}

// ErrCtOverflow is a read that found the kernel dropped events: the
// receive buffer overflowed, by how many isn't told.
var ErrCtOverflow = errors.New("conntrack events were lost: the receive buffer overflowed")

// CtEvents is a socket subscribed to conntrack NEW and DESTROY events,
// both families, in the namespace of the thread that opened it.
type CtEvents struct {
	fd  int
	buf []byte
}

// OpenCtEvents subscribes; a read waits up to wait for an event.
func OpenCtEvents(wait time.Duration) (*CtEvents, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return nil, fmt.Errorf("opening a conntrack event socket: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, ctEventBuffer); err != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, ctEventBuffer)
	}
	tv := unix.NsecToTimeval(wait.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("setting the conntrack event socket's timeout: %w", err)
	}
	groups := uint32(1<<(nfnlGroupCtNew-1) | 1<<(nfnlGroupCtDel-1))
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("subscribing to conntrack events: %w", err)
	}
	return &CtEvents{fd: fd, buf: make([]byte, 1<<16)}, nil
}

// Close closes the socket.
func (c *CtEvents) Close() error { return unix.Close(c.fd) }

// Read returns the events of one receive: none when the wait passed,
// ErrCtOverflow when events were lost (the socket goes on).
func (c *CtEvents) Read() ([]CtEvent, error) {
	n, _, err := unix.Recvfrom(c.fd, c.buf, 0)
	switch {
	case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
		return nil, nil
	case errors.Is(err, unix.ENOBUFS):
		return nil, ErrCtOverflow
	case err != nil:
		return nil, fmt.Errorf("reading conntrack events: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(c.buf[:n])
	if err != nil {
		return nil, fmt.Errorf("parsing conntrack events: %w", err)
	}
	var out []CtEvent
	for _, m := range msgs {
		if m.Header.Type>>8 != unix.NFNL_SUBSYS_CTNETLINK {
			continue
		}
		var e CtEvent
		switch m.Header.Type & 0xff {
		case nl.IPCTNL_MSG_CT_NEW:
			e.New = true
		case nl.IPCTNL_MSG_CT_DELETE:
		default:
			continue
		}
		if len(m.Data) < 4 || !parseCtEvent(m.Data[4:], &e) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// parseCtEvent fills e from an event's attributes; false without an
// original tuple.
func parseCtEvent(attrs []byte, e *CtEvent) bool {
	as, err := nl.ParseRouteAttr(attrs)
	if err != nil {
		return false
	}
	found := false
	for _, a := range as {
		switch a.Attr.Type & nl.NLA_TYPE_MASK {
		case nl.CTA_TUPLE_ORIG:
			e.Proto, e.Src, e.Dst = parseTuple(a.Value)
			found = e.Src.IsValid()
		case nl.CTA_TUPLE_REPLY:
			p, src, dst := parseTuple(a.Value)
			if p == unix.IPPROTO_ICMP || p == unix.IPPROTO_ICMPV6 {
				// The reply's echo id is the translated one.
				dst = netip.AddrPortFrom(dst.Addr(), src.Port())
			}
			e.Nat = dst
		case nl.CTA_MARK:
			if len(a.Value) == 4 {
				e.Mark = binary.BigEndian.Uint32(a.Value)
			}
		case ctaCountersOrig, ctaCountersReply:
			if p, b, ok := parseCounters(a.Value); ok {
				e.Counted = true
				e.Packets += p
				e.Bytes += b
			}
		case ctaTimestamp:
			e.Start, e.Stop = parseTimestamps(a.Value)
		}
	}
	return found
}

// parseTuple is a tuple's protocol and its source and destination, each
// with its port (or ICMP echo id).
func parseTuple(b []byte) (uint8, netip.AddrPort, netip.AddrPort) {
	as, err := nl.ParseRouteAttr(b)
	if err != nil {
		return 0, netip.AddrPort{}, netip.AddrPort{}
	}
	var src, dst netip.Addr
	var proto uint8
	var sport, dport uint16
	for _, a := range as {
		inner, err := nl.ParseRouteAttr(a.Value)
		if err != nil {
			continue
		}
		switch a.Attr.Type & nl.NLA_TYPE_MASK {
		case ctaTupleIP:
			for _, x := range inner {
				addr, _ := netip.AddrFromSlice(x.Value)
				switch x.Attr.Type & nl.NLA_TYPE_MASK {
				case ctaIPv4Src, ctaIPv6Src:
					src = addr
				case ctaIPv4Dst, ctaIPv6Dst:
					dst = addr
				}
			}
		case ctaTupleProto:
			for _, x := range inner {
				switch t := x.Attr.Type & nl.NLA_TYPE_MASK; {
				case t == ctaProtoNum && len(x.Value) == 1:
					proto = x.Value[0]
				case t == ctaProtoSrcPort && len(x.Value) == 2:
					sport = binary.BigEndian.Uint16(x.Value)
				case t == ctaProtoDstPort && len(x.Value) == 2:
					dport = binary.BigEndian.Uint16(x.Value)
				case (t == ctaProtoICMPID || t == ctaProtoICMPv6ID) && len(x.Value) == 2:
					sport = binary.BigEndian.Uint16(x.Value)
				}
			}
		}
	}
	return proto, netip.AddrPortFrom(src, sport), netip.AddrPortFrom(dst, dport)
}

func parseCounters(b []byte) (uint64, uint64, bool) {
	as, err := nl.ParseRouteAttr(b)
	if err != nil {
		return 0, 0, false
	}
	var p, n uint64
	for _, a := range as {
		if len(a.Value) != 8 {
			continue
		}
		switch a.Attr.Type & nl.NLA_TYPE_MASK {
		case ctaCountersPkts:
			p = binary.BigEndian.Uint64(a.Value)
		case ctaCountersBytes:
			n = binary.BigEndian.Uint64(a.Value)
		}
	}
	return p, n, true
}

func parseTimestamps(b []byte) (time.Time, time.Time) {
	as, err := nl.ParseRouteAttr(b)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	var start, stop time.Time
	for _, a := range as {
		if len(a.Value) != 8 {
			continue
		}
		t := time.Unix(0, int64(binary.BigEndian.Uint64(a.Value)))
		switch a.Attr.Type & nl.NLA_TYPE_MASK {
		case ctaTimestampStart:
			start = t
		case ctaTimestampStop:
			stop = t
		}
	}
	return start, stop
}
