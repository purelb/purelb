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

// Helpers for the Phase 0 gap tests (gaps_test.go): kernel counters, raw
// ICMP echo, AF_PACKET capture, neighbour state, and nft building blocks.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// snmp returns the node's IP, ICMP and ICMPv6 counters, keyed like
// "IcmpInDestUnreachs" and "Icmp6InPktTooBigs". It reads
// /proc/thread-self/net, because /proc/self/net follows the process's main
// thread, not the thread n.in switched into the namespace.
func snmp(t *testing.T, n *node) map[string]int64 {
	t.Helper()
	m := map[string]int64{}
	require.NoError(t, n.in(func() error {
		for _, f := range []string{"/proc/thread-self/net/snmp", "/proc/thread-self/net/netstat"} {
			raw, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			lines := strings.Split(string(raw), "\n")
			for i := 0; i+1 < len(lines); i++ {
				h, v := strings.Fields(lines[i]), strings.Fields(lines[i+1])
				if len(h) < 2 || len(h) != len(v) || h[0] != v[0] {
					continue
				}
				p := strings.TrimSuffix(h[0], ":")
				for j := 1; j < len(h); j++ {
					x, _ := strconv.ParseInt(v[j], 10, 64)
					m[p+h[j]] = x
				}
				i++
			}
		}
		raw, err := os.ReadFile("/proc/thread-self/net/snmp6")
		if err != nil {
			return err
		}
		for _, l := range strings.Split(string(raw), "\n") {
			if f := strings.Fields(l); len(f) == 2 {
				x, _ := strconv.ParseInt(f[1], 10, 64)
				m[f[0]] = x
			}
		}
		return nil
	}), "%s: read counters", n.name)
	return m
}

// deltas formats after-before for the named counters (all counters that
// changed when keys is empty), skipping those that did not change.
func deltas(before, after map[string]int64, keys ...string) string {
	if len(keys) == 0 {
		for k := range after {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	var b strings.Builder
	for _, k := range keys {
		if d := after[k] - before[k]; d != 0 {
			fmt.Fprintf(&b, " %s=%+d", k, d)
		}
	}
	if b.Len() == 0 {
		return " none"
	}
	return b.String()
}

func rcvTimeout(fd int, d time.Duration) error {
	tv := unix.NsecToTimeval(d.Nanoseconds())
	return unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

func sockaddr(a netip.Addr, port int) unix.Sockaddr {
	if a.Is4() {
		return &unix.SockaddrInet4{Port: port, Addr: a.As4()}
	}
	return &unix.SockaddrInet6{Port: port, Addr: a.As16()}
}

func csum16(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// rawICMP opens a raw ICMP (or ICMPv6) socket inside n.
func rawICMP(n *node, v6 bool) (int, error) {
	var fd int
	err := n.in(func() error {
		var err error
		if v6 {
			fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_RAW, unix.IPPROTO_ICMPV6)
		} else {
			fd, err = unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_ICMP)
		}
		return err
	})
	return fd, err
}

// echo sends one echo request with the given identifier and sequence from
// inside n to dst (from src when not empty), and waits for the echo reply
// with that sequence and payload. It returns the identifier in the reply.
func echo(n *node, src, dst string, id, seq uint16, timeout time.Duration) (uint16, error) {
	d := netip.MustParseAddr(dst)
	v6 := d.Is6()
	fd, err := rawICMP(n, v6)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	if src != "" {
		if err := unix.Bind(fd, sockaddr(netip.MustParseAddr(src), 0)); err != nil {
			return 0, fmt.Errorf("bind %s: %w", src, err)
		}
	}
	if err := rcvTimeout(fd, 100*time.Millisecond); err != nil {
		return 0, err
	}
	payload := []byte(fmt.Sprintf("purelb-gap-%05d", seq))
	req, rep := byte(8), byte(0)
	if v6 {
		req, rep = 128, 129
	}
	pkt := append([]byte{req, 0, 0, 0, byte(id >> 8), byte(id), byte(seq >> 8), byte(seq)}, payload...)
	if !v6 { // the kernel computes the ICMPv6 checksum of raw ICMPv6 sockets
		binary.BigEndian.PutUint16(pkt[2:], csum16(pkt))
	}
	if err := unix.Sendto(fd, pkt, 0, sockaddr(d, 0)); err != nil {
		return 0, fmt.Errorf("send: %w", err)
	}
	buf := make([]byte, 2048)
	for end := time.Now().Add(timeout); time.Now().Before(end); {
		nr, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil || nr < 1 {
			continue
		}
		msg := buf[:nr]
		if !v6 {
			msg = msg[int(msg[0]&0xf)*4:]
		}
		if len(msg) >= 8 && msg[0] == rep && binary.BigEndian.Uint16(msg[6:]) == seq && bytes.Equal(msg[8:], payload) {
			return binary.BigEndian.Uint16(msg[4:]), nil
		}
	}
	return 0, fmt.Errorf("no echo reply within %s", timeout)
}

type echoSeen struct {
	src     netip.Addr
	id, seq uint16
	csumOK  bool // the ICMP checksum is valid on the bytes
}

func (e echoSeen) String() string {
	return fmt.Sprintf("{src=%s id=%d seq=%d csum=%s}", e.src, e.id, e.seq, yesno(e.csumOK, "valid", "BAD"))
}

// sniffEcho records the echo requests received on dev inside n, from the
// bytes on the device (AF_PACKET), until the returned function is called.
func sniffEcho(t *testing.T, n *node, dev string) func() []echoSeen {
	t.Helper()
	idx := ifindex(t, n, dev)
	var fd int
	require.NoError(t, n.in(func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_ALL)))
		if err != nil {
			return err
		}
		return unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: idx})
	}), "%s: capture on %s", n.name, dev)
	require.NoError(t, rcvTimeout(fd, 100*time.Millisecond))
	var stop atomic.Bool
	out := make(chan []echoSeen, 1)
	go func() {
		var seen []echoSeen
		buf := make([]byte, 2048)
		for !stop.Load() {
			nr, from, err := unix.Recvfrom(fd, buf, 0)
			if err != nil || nr < 28 {
				continue
			}
			if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
				continue
			}
			d := buf[:nr]
			switch d[0] >> 4 {
			case 4:
				ihl := int(d[0]&0xf) * 4
				if d[9] != unix.IPPROTO_ICMP || len(d) < ihl+8 || d[ihl] != 8 {
					continue
				}
				m := d[ihl:]
				seen = append(seen, echoSeen{src: netip.AddrFrom4([4]byte(d[12:16])),
					id: binary.BigEndian.Uint16(m[4:]), seq: binary.BigEndian.Uint16(m[6:]), csumOK: csum16(m) == 0})
			case 6:
				if len(d) < 48 || d[6] != unix.IPPROTO_ICMPV6 || d[40] != 128 {
					continue
				}
				m := d[40:]
				l := len(m)
				ps := append(append([]byte{}, d[8:40]...), byte(l>>24), byte(l>>16), byte(l>>8), byte(l), 0, 0, 0, unix.IPPROTO_ICMPV6)
				seen = append(seen, echoSeen{src: netip.AddrFrom16([16]byte(d[8:24])),
					id: binary.BigEndian.Uint16(m[4:]), seq: binary.BigEndian.Uint16(m[6:]), csumOK: csum16(append(ps, m...)) == 0})
			}
		}
		unix.Close(fd)
		out <- seen
	}()
	return func() []echoSeen { stop.Store(true); return <-out }
}

// capPkt is one inner IP packet captured on a device.
type capPkt struct {
	v6     bool
	frag   bool   // carries a fragment (IPv4 MF/offset, IPv6 Fragment Header)
	id     uint32 // IPv4 ID or IPv6 Fragment Header ID
	offset int
	more   bool
	proto  byte
	sport  uint16 // UDP source port, when the packet holds the UDP header
	length int
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// capture records every packet received (not sent) on dev inside n, from
// its network header, until the returned function is called.
func capture(t *testing.T, n *node, dev string) func() []capPkt {
	t.Helper()
	idx := ifindex(t, n, dev)
	var fd int
	require.NoError(t, n.in(func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_ALL)))
		if err != nil {
			return err
		}
		return unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: idx})
	}), "%s: capture on %s", n.name, dev)
	require.NoError(t, rcvTimeout(fd, 100*time.Millisecond))
	var stop atomic.Bool
	out := make(chan []capPkt, 1)
	go func() {
		var pkts []capPkt
		buf := make([]byte, 65536)
		for !stop.Load() {
			nr, from, err := unix.Recvfrom(fd, buf, 0)
			if err != nil || nr < 20 {
				continue
			}
			if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
				continue
			}
			if p, ok := parseIP(buf[:nr]); ok {
				pkts = append(pkts, p)
			}
		}
		unix.Close(fd)
		out <- pkts
	}()
	return func() []capPkt { stop.Store(true); return <-out }
}

func parseIP(d []byte) (capPkt, bool) {
	p := capPkt{length: len(d)}
	switch d[0] >> 4 {
	case 4:
		ihl := int(d[0]&0xf) * 4
		fo := binary.BigEndian.Uint16(d[6:8])
		p.id = uint32(binary.BigEndian.Uint16(d[4:6]))
		p.more = fo&0x2000 != 0
		p.offset = int(fo&0x1fff) * 8
		p.frag = p.more || p.offset > 0
		p.proto = d[9]
		if p.offset == 0 && p.proto == unix.IPPROTO_UDP && len(d) >= ihl+8 {
			p.sport = binary.BigEndian.Uint16(d[ihl:])
		}
		return p, true
	case 6:
		if len(d) < 40 {
			return p, false
		}
		p.v6 = true
		p.proto = d[6]
		l4 := 40
		if d[6] == unix.IPPROTO_FRAGMENT && len(d) >= 48 {
			fo := binary.BigEndian.Uint16(d[42:44])
			p.frag, p.proto = true, d[40]
			p.offset, p.more = int(fo&0xfff8), fo&1 != 0
			p.id = binary.BigEndian.Uint32(d[44:48])
			l4 = 48
		}
		if p.offset == 0 && p.proto == unix.IPPROTO_UDP && len(d) >= l4+8 {
			p.sport = binary.BigEndian.Uint16(d[l4:])
		}
		return p, true
	}
	return p, false
}

// neigh returns the state and link-layer address of ip in n's neighbour
// table on dev (state 0 when absent).
func neigh(t *testing.T, n *node, dev, ip string) (int, net.HardwareAddr) {
	t.Helper()
	a := net.ParseIP(ip)
	fam := netlink.FAMILY_V4
	if a.To4() == nil {
		fam = netlink.FAMILY_V6
	}
	l, err := n.nl.LinkByName(dev)
	require.NoError(t, err)
	list, err := n.nl.NeighList(l.Attrs().Index, fam)
	require.NoError(t, err)
	for _, e := range list {
		if e.IP.Equal(a) {
			return e.State, e.HardwareAddr
		}
	}
	return 0, nil
}

// forceStale puts an existing neighbour entry into STALE, so the next
// packet sent to it starts neighbour unreachability detection (DELAY,
// then a unicast PROBE).
func forceStale(t *testing.T, n *node, dev, ip string) error {
	t.Helper()
	st, mac := neigh(t, n, dev, ip)
	if st&(netlink.NUD_REACHABLE|netlink.NUD_STALE|netlink.NUD_DELAY|netlink.NUD_PROBE) == 0 || mac == nil {
		return fmt.Errorf("no valid neighbour entry for %s (%s)", ip, nudName(st))
	}
	a := net.ParseIP(ip)
	fam := netlink.FAMILY_V4
	if a.To4() == nil {
		fam = netlink.FAMILY_V6
	}
	return n.nl.NeighSet(&netlink.Neigh{LinkIndex: ifindex(t, n, dev), Family: fam,
		State: netlink.NUD_STALE, IP: a, HardwareAddr: mac})
}

// watchNeigh samples the neighbour state for d and returns the sequence of
// distinct states seen and the last one.
func watchNeigh(t *testing.T, n *node, dev, ip string, d time.Duration) ([]string, int) {
	t.Helper()
	var seq []string
	last := -1
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		st, _ := neigh(t, n, dev, ip)
		if st != last {
			seq = append(seq, nudName(st))
			last = st
		}
	}
	return seq, last
}

func nudName(s int) string {
	names := map[int]string{0: "NONE", 0x01: "INCOMPLETE", 0x02: "REACHABLE", 0x04: "STALE", 0x08: "DELAY",
		0x10: "PROBE", 0x20: "FAILED", 0x40: "NOARP", 0x80: "PERMANENT"}
	if s, ok := names[s]; ok {
		return s
	}
	return fmt.Sprintf("0x%x", s)
}

// fastNUD shortens neighbour timers on dev so NUD probes run within a
// second (delay 1 s, retransmit 300 ms), for both families.
func fastNUD(t *testing.T, n *node, dev string) {
	t.Helper()
	for _, f := range []string{"ipv4", "ipv6"} {
		n.sysctl(t, "net."+f+".neigh."+dev+".delay_first_probe_time", "1")
		n.sysctl(t, "net."+f+".neigh."+dev+".retrans_time_ms", "300")
	}
}

// addrDeprecated adds an IPv6 address with preferred lifetime 0, as the
// announcer places IPv6 VIPs and egress addresses.
func addrDeprecated(t *testing.T, n *node, dev, cidr string) {
	t.Helper()
	l, err := n.nl.LinkByName(dev)
	require.NoError(t, err)
	a, err := netlink.ParseAddr(cidr)
	require.NoError(t, err)
	a.Flags |= 0x02 // IFA_F_NODAD
	a.PreferedLft, a.ValidLft = 0, 0xffffffff
	require.NoError(t, n.nl.AddrAdd(l, a), "%s: %s %s", n.name, dev, cidr)
}

// ---- nft building blocks ---------------------------------------------------

func cat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func mNfproto(p byte) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{p}}}
}

// mCtState matches when any of bits is set in ct state.
func mCtState(bits uint32) []expr.Any {
	return []expr.Any{&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(bits), Xor: le32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: le32(0)}}
}

// mCtReply is "ct direction reply".
func mCtReply() []expr.Any {
	return []expr.Any{&expr.Ct{Key: expr.CtKeyDIRECTION, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{1}}} // IP_CT_DIR_REPLY
}

// mNDType is "icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert }".
func mNDType(types *nftables.Set) []expr.Any {
	return cat(mNfproto(unix.NFPROTO_IPV6), []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Lookup{SourceRegister: 1, SetName: types.Name, SetID: types.ID},
	})
}

// mHop255 is "ip6 hoplimit 255".
func mHop255() []expr.Any {
	return []expr.Any{&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 7, Len: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{255}}}
}

// mAddrIn is "ip[6] saddr|daddr @set".
func mAddrIn(v6, src bool, set *nftables.Set) []expr.Any {
	p, off, ln := byte(unix.NFPROTO_IPV4), uint32(16), uint32(4)
	if src {
		off = 12
	}
	if v6 {
		p, off, ln = unix.NFPROTO_IPV6, 24, 16
		if src {
			off = 8
		}
	}
	return cat(mNfproto(p), []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: off, Len: ln},
		&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID},
	})
}

func mIifname(name string) []expr.Any {
	b := make([]byte, 16)
	copy(b, name)
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b}}
}

func counted(v *expr.Verdict) []expr.Any {
	if v == nil {
		return []expr.Any{&expr.Counter{}}
	}
	return []expr.Any{&expr.Counter{}, v}
}

var (
	vAccept = &expr.Verdict{Kind: expr.VerdictAccept}
	vDrop   = &expr.Verdict{Kind: expr.VerdictDrop}
)

// egressSets adds the egress4 and egress6 sets to tbl.
func egressSets(t *testing.T, c *nftables.Conn, tbl *nftables.Table, eg4, eg6 []string) (s4, s6 *nftables.Set) {
	t.Helper()
	s4 = &nftables.Set{Table: tbl, Name: "egress4", KeyType: nftables.TypeIPAddr}
	s6 = &nftables.Set{Table: tbl, Name: "egress6", KeyType: nftables.TypeIP6Addr}
	el := func(as []string) []nftables.SetElement {
		var e []nftables.SetElement
		for _, a := range as {
			e = append(e, nftables.SetElement{Key: netip.MustParseAddr(a).AsSlice()})
		}
		return e
	}
	require.NoError(t, c.AddSet(s4, el(eg4)))
	require.NoError(t, c.AddSet(s6, el(eg6)))
	return s4, s6
}

// ndTypes adds the anonymous set { nd-neighbor-solicit, nd-neighbor-advert }.
// The kernel binds an anonymous set to exactly one rule, so every rule
// needs its own.
func ndTypes(t *testing.T, c *nftables.Conn, tbl *nftables.Table) *nftables.Set {
	t.Helper()
	nd := &nftables.Set{Table: tbl, Anonymous: true, Constant: true, KeyType: nftables.TypeICMP6Type}
	require.NoError(t, c.AddSet(nd, []nftables.SetElement{{Key: []byte{135}}, {Key: []byte{136}}}))
	return nd
}

// ctOn activates conntrack in n's namespace (a table with one ct rule), as
// kube-proxy and the CNI do on a real node, so flows are tracked from their
// first packet.
func ctOn(t *testing.T, n *node) {
	t.Helper()
	c, err := nftables.New(nftables.WithNetNSFd(int(n.h)))
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "purelb_gap_ct"})
	ch := c.AddChain(&nftables.Chain{Name: "ct", Table: tbl, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityRef(-300)})
	c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: cat(mCtState(expr.CtStateBitESTABLISHED), counted(nil))})
	require.NoError(t, c.Flush(), "%s: conntrack on", n.name)
}

// nftCounters returns each counter in the chain by its rule's label
// (UserData), and the counters formatted in rule order.
func nftCounters(t *testing.T, n *node, table, chain string) (map[string]uint64, string) {
	t.Helper()
	c, err := nftables.New(nftables.WithNetNSFd(int(n.h)))
	require.NoError(t, err)
	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: table}
	rules, err := c.GetRules(tbl, &nftables.Chain{Name: chain, Table: tbl})
	require.NoError(t, err)
	m := map[string]uint64{}
	var b strings.Builder
	for _, r := range rules {
		for _, e := range r.Exprs {
			if ctr, ok := e.(*expr.Counter); ok {
				m[string(r.UserData)] = ctr.Packets
				fmt.Fprintf(&b, " %s=%d", r.UserData, ctr.Packets)
			}
		}
	}
	return m, b.String()
}

// ---- sockets ---------------------------------------------------------------

func jh(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// listenTCP serves hp inside n, running handle on each connection.
func listenTCP(t *testing.T, n *node, hp string, handle func(net.Conn)) {
	t.Helper()
	var ln net.Listener
	lc := net.ListenConfig{KeepAlive: -1} // no keepalive probes: they would pick up flows mid-test
	require.NoError(t, n.in(func() error {
		var err error
		ln, err = lc.Listen(context.Background(), "tcp", hp)
		return err
	}), "%s: listen tcp %s", n.name, hp)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
}

// listenUDP opens a UDP socket bound to hp inside n.
func listenUDP(t *testing.T, n *node, hp string) *net.UDPConn {
	t.Helper()
	ua, err := net.ResolveUDPAddr("udp", hp)
	require.NoError(t, err)
	var c *net.UDPConn
	require.NoError(t, n.in(func() error {
		var err error
		c, err = net.ListenUDP("udp", ua)
		return err
	}), "%s: listen udp %s", n.name, hp)
	t.Cleanup(func() { c.Close() })
	return c
}

// dialTCP connects from inside n, from local (any address when empty).
func dialTCP(n *node, local, hp string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout, KeepAlive: -1}
	if local != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(local)}
	}
	var c net.Conn
	err := n.in(func() error {
		var err error
		c, err = d.Dial("tcp", hp)
		return err
	})
	return c, err
}

// udpAsk sends msg from inside n (from local when not empty) to hp and
// returns the first reply, or an error after timeout.
func udpAsk(n *node, local, hp, msg string, timeout time.Duration) (string, error) {
	la := &net.UDPAddr{}
	if local != "" {
		la.IP = net.ParseIP(local)
	}
	ra, err := net.ResolveUDPAddr("udp", hp)
	if err != nil {
		return "", err
	}
	var c *net.UDPConn
	if err := n.in(func() error {
		var err error
		c, err = net.ListenUDP("udp", la)
		return err
	}); err != nil {
		return "", err
	}
	defer c.Close()
	if _, err := c.WriteToUDP([]byte(msg), ra); err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 2048)
	nr, _, err := c.ReadFromUDP(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:nr]), nil
}

// expectLine waits for want on ch, discarding other lines.
func expectLine(ch <-chan string, want string, timeout time.Duration) bool {
	tm := time.After(timeout)
	for {
		select {
		case l := <-ch:
			if l == want {
				return true
			}
		case <-tm:
			return false
		}
	}
}

// ndProbes counts, on dev inside n, the address-resolution probes n sends
// for target and the answers it receives, until the returned function is
// called: ARP requests and replies for IPv4, NS and NA for IPv6. unicast
// counts the probes sent to a unicast link-layer address (NUD probes of a
// known neighbour, as opposed to broadcast or multicast resolution).
func ndProbes(t *testing.T, n *node, dev, target string) func() (sent, unicast, answered int) {
	t.Helper()
	idx := ifindex(t, n, dev)
	tg := netip.MustParseAddr(target)
	var fd int
	require.NoError(t, n.in(func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
		if err != nil {
			return err
		}
		return unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: idx})
	}), "%s: capture on %s", n.name, dev)
	require.NoError(t, rcvTimeout(fd, 100*time.Millisecond))
	var stop atomic.Bool
	out := make(chan [3]int, 1)
	go func() {
		var c [3]int
		buf := make([]byte, 2048)
		for !stop.Load() {
			nr, from, err := unix.Recvfrom(fd, buf, 0)
			if err != nil || nr < 14 {
				continue
			}
			ll, ok := from.(*unix.SockaddrLinklayer)
			if !ok {
				continue
			}
			outgoing := ll.Pkttype == unix.PACKET_OUTGOING
			d := buf[:nr]
			ucast := d[0]&1 == 0
			et := binary.BigEndian.Uint16(d[12:14])
			p := d[14:]
			switch {
			case et == unix.ETH_P_ARP && tg.Is4() && len(p) >= 28:
				op := binary.BigEndian.Uint16(p[6:8])
				spa, tpa := netip.AddrFrom4([4]byte(p[14:18])), netip.AddrFrom4([4]byte(p[24:28]))
				if outgoing && op == 1 && tpa == tg {
					c[0]++
					if ucast {
						c[1]++
					}
				} else if !outgoing && op == 2 && spa == tg {
					c[2]++
				}
			case et == unix.ETH_P_IPV6 && tg.Is6() && len(p) >= 64 && p[6] == unix.IPPROTO_ICMPV6:
				if netip.AddrFrom16([16]byte(p[48:64])) != tg {
					continue
				}
				if outgoing && p[40] == 135 {
					c[0]++
					if ucast {
						c[1]++
					}
				} else if !outgoing && p[40] == 136 {
					c[2]++
				}
			}
		}
		unix.Close(fd)
		out <- c
	}()
	return func() (int, int, int) {
		stop.Store(true)
		c := <-out
		return c[0], c[1], c[2]
	}
}

// csumSeen is one captured UDP packet to a port: its length, whether its
// UDP checksum is valid on the bytes, and the AF_PACKET status flags
// (TP_STATUS_CSUMNOTREADY 0x8: checksum left to offload, the bytes hold
// only the pseudo-header sum; TP_STATUS_CSUM_VALID 0x80).
type csumSeen struct {
	n      int
	valid  bool
	status uint32
}

func (c csumSeen) String() string {
	return fmt.Sprintf("%dB:%s/status=0x%x", c.n, yesno(c.valid, "valid", "BAD"), c.status)
}

func yesno(b bool, y, n string) string {
	if b {
		return y
	}
	return n
}

// udpCsums captures the unfragmented UDP packets to dport received on dev
// inside n and checks their UDP checksums, until the returned function is
// called.
func udpCsums(t *testing.T, n *node, dev string, dport uint16) func() []csumSeen {
	t.Helper()
	idx := ifindex(t, n, dev)
	var fd int
	require.NoError(t, n.in(func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_ALL)))
		if err != nil {
			return err
		}
		if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_AUXDATA, 1); err != nil {
			return err
		}
		return unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: idx})
	}), "%s: capture on %s", n.name, dev)
	require.NoError(t, rcvTimeout(fd, 100*time.Millisecond))
	var stop atomic.Bool
	out := make(chan []csumSeen, 1)
	go func() {
		var seen []csumSeen
		buf, oob := make([]byte, 65536), make([]byte, 256)
		for !stop.Load() {
			nr, noob, _, from, err := unix.Recvmsg(fd, buf, oob, 0)
			if err != nil || nr < 28 {
				continue
			}
			if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
				continue
			}
			var status uint32
			if cms, err := unix.ParseSocketControlMessage(oob[:noob]); err == nil {
				for _, cm := range cms {
					if cm.Header.Level == unix.SOL_PACKET && cm.Header.Type == unix.PACKET_AUXDATA && len(cm.Data) >= 4 {
						status = binary.LittleEndian.Uint32(cm.Data)
					}
				}
			}
			d := buf[:nr]
			var pseudo, udp []byte
			switch d[0] >> 4 {
			case 4:
				ihl := int(d[0]&0xf) * 4
				if d[9] != unix.IPPROTO_UDP || binary.BigEndian.Uint16(d[6:8])&0x3fff != 0 || len(d) < ihl+8 {
					continue
				}
				udp = d[ihl:]
				pseudo = append(append(append([]byte{}, d[12:20]...), 0, unix.IPPROTO_UDP), byte(len(udp)>>8), byte(len(udp)))
			case 6:
				if d[6] != unix.IPPROTO_UDP || len(d) < 48 {
					continue
				}
				udp = d[40:]
				l := len(udp)
				pseudo = append(append([]byte{}, d[8:40]...), byte(l>>24), byte(l>>16), byte(l>>8), byte(l), 0, 0, 0, unix.IPPROTO_UDP)
			default:
				continue
			}
			if binary.BigEndian.Uint16(udp[2:4]) != dport {
				continue
			}
			seen = append(seen, csumSeen{n: nr, valid: csum16(append(pseudo, udp...)) == 0, status: status})
		}
		unix.Close(fd)
		out <- seen
	}()
	return func() []csumSeen { stop.Store(true); return <-out }
}

// ---- crafted packets (reordered fragments, IPv6 extension headers, Geneve) --

// sum16 adds b to a ones-complement accumulator.
func sum16(s uint32, b []byte) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	return s
}

func fold16(s uint32) uint16 {
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// udpDatagram returns a UDP header and payload with a valid checksum.
func udpDatagram(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	u := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(u[0:], sport)
	binary.BigEndian.PutUint16(u[2:], dport)
	binary.BigEndian.PutUint16(u[4:], uint16(len(u)))
	copy(u[8:], payload)
	var s uint32
	s = sum16(s, src.AsSlice())
	s = sum16(s, dst.AsSlice())
	s += unix.IPPROTO_UDP + uint32(len(u))
	cs := fold16(sum16(s, u))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], cs)
	return u
}

// ipv4Packets carries the transport bytes l4 in IPv4 packets with TTL 64,
// fragmented when fragData (a multiple of 8) is smaller than l4. Header
// checksums are valid.
func ipv4Packets(src, dst netip.Addr, id uint16, proto byte, l4 []byte, fragData int) [][]byte {
	var out [][]byte
	for off := 0; off < len(l4) || off == 0; off += fragData {
		end := min(off+fragData, len(l4))
		p := make([]byte, 20+end-off)
		p[0], p[8], p[9] = 0x45, 64, proto
		binary.BigEndian.PutUint16(p[2:], uint16(len(p)))
		binary.BigEndian.PutUint16(p[4:], id)
		fo := uint16(off / 8)
		if end < len(l4) {
			fo |= 0x2000 // MF
		}
		binary.BigEndian.PutUint16(p[6:], fo)
		copy(p[12:], src.AsSlice())
		copy(p[16:], dst.AsSlice())
		binary.BigEndian.PutUint16(p[10:], fold16(sum16(0, p[:20])))
		copy(p[20:], l4[off:end])
		out = append(out, p)
		if end == len(l4) {
			break
		}
	}
	return out
}

// padOpt is an 8-byte Hop-by-Hop or Destination Options header (one PadN
// option); its first byte, the next header, is filled in by ipv6Packets.
func padOpt() []byte { return []byte{0, 0, 1, 4, 0, 0, 0, 0} }

// ipv6Packets carries l4 (protocol proto) in IPv6 packets with hop limit 64.
// pre are extension headers of the unfragmentable part (types preT), post
// those of the fragmentable part (types postT), chained in order. With frag,
// a Fragment Header (ID id) splits the fragmentable part into fragData-byte
// pieces (a multiple of 8); without it, one packet.
func ipv6Packets(src, dst netip.Addr, preT []byte, pre [][]byte, frag bool, id uint32,
	postT []byte, post [][]byte, proto byte, l4 []byte, fragData int) [][]byte {
	chain := func(types []byte, hdrs [][]byte, next byte) []byte {
		var b []byte
		for i, h := range hdrs {
			h = append([]byte{}, h...)
			h[0] = next
			if i+1 < len(types) {
				h[0] = types[i+1]
			}
			b = append(b, h...)
		}
		return b
	}
	fragPart := append(chain(postT, post, proto), l4...)
	firstFrag := proto
	if len(postT) > 0 {
		firstFrag = postT[0]
	}
	afterPre := firstFrag
	if frag {
		afterPre = unix.IPPROTO_FRAGMENT
	}
	unfrag := chain(preT, pre, afterPre)
	first := afterPre
	if len(preT) > 0 {
		first = preT[0]
	}
	hdr := func(plen int) []byte {
		h := make([]byte, 40)
		h[0], h[6], h[7] = 0x60, first, 64
		binary.BigEndian.PutUint16(h[4:], uint16(plen))
		copy(h[8:], src.AsSlice())
		copy(h[24:], dst.AsSlice())
		return h
	}
	if !frag {
		body := append(append([]byte{}, unfrag...), fragPart...)
		return [][]byte{append(hdr(len(body)), body...)}
	}
	var out [][]byte
	for off := 0; off < len(fragPart); off += fragData {
		end := min(off+fragData, len(fragPart))
		fh := make([]byte, 8)
		fh[0] = firstFrag
		fo := uint16(off)
		if end < len(fragPart) {
			fo |= 1 // M
		}
		binary.BigEndian.PutUint16(fh[2:], fo)
		binary.BigEndian.PutUint32(fh[4:], id)
		body := append(append(append([]byte{}, unfrag...), fh...), fragPart[off:end]...)
		out = append(out, append(hdr(len(body)), body...))
	}
	return out
}

// sendRawIP sends complete IP packets (pkts[i] for i in order) from inside
// n through an IPPROTO_RAW socket (header included; the kernel routes them
// and, for IPv4, fills the header checksum).
func sendRawIP(n *node, dst string, pkts [][]byte, order []int) error {
	d := netip.MustParseAddr(dst)
	return n.in(func() error {
		af := unix.AF_INET
		if d.Is6() {
			af = unix.AF_INET6
		}
		fd, err := unix.Socket(af, unix.SOCK_RAW, unix.IPPROTO_RAW)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		for _, i := range order {
			if err := unix.Sendto(fd, pkts[i], 0, sockaddr(d, 0)); err != nil {
				return fmt.Errorf("raw send of packet %d: %w", i, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	})
}

// sendGeneve sends inner IP packets (pkts[i] for i in order) from inside n,
// from the underlay address from to the underlay address to, as Geneve on
// tunPort with VNI 1, as purelb-eg0 on a peer node would: L3 mode carries
// the IP packet with its EtherType as the protocol, L2 mode an Ethernet
// frame (Transparent Ethernet Bridging) addressed to dmac.
func sendGeneve(n *node, from, to string, l2 bool, dmac net.HardwareAddr, pkts [][]byte, order []int) error {
	return n.in(func() error {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(from)})
		if err != nil {
			return err
		}
		defer c.Close()
		for _, i := range order {
			p := pkts[i]
			et := uint16(unix.ETH_P_IP)
			if p[0]>>4 == 6 {
				et = unix.ETH_P_IPV6
			}
			g := []byte{0, 0, 0, 0, 0, 0, vni, 0}
			if l2 {
				binary.BigEndian.PutUint16(g[2:], 0x6558)
				eth := make([]byte, 14)
				copy(eth, dmac)
				copy(eth[6:], []byte{0x02, 0, 0, 0, 0, 0x99})
				binary.BigEndian.PutUint16(eth[12:], et)
				g = append(g, eth...)
			} else {
				binary.BigEndian.PutUint16(g[2:], et)
			}
			if _, err := c.WriteToUDP(append(g, p...), &net.UDPAddr{IP: net.ParseIP(to), Port: tunPort}); err != nil {
				return fmt.Errorf("geneve send of packet %d: %w", i, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	})
}

// icmpErrSeen is one ICMP or ICMPv6 error received on a device.
type icmpErrSeen struct {
	src, dst     netip.Addr
	typ, code    byte
	mtu          uint32 // frag-needed: next-hop MTU; Packet Too Big: MTU
	quoted       int    // bytes of the invoking packet quoted
	quotedDst    netip.Addr
	quotedProto  byte
	length       int // the whole IP packet
	ipCsum, csum bool
}

func (e icmpErrSeen) String() string {
	return fmt.Sprintf("{%s->%s type=%d code=%d mtu=%d len=%d quoted=%dB(dst %s proto %d) iphdr-csum=%s icmp-csum=%s}",
		e.src, e.dst, e.typ, e.code, e.mtu, e.length, e.quoted, e.quotedDst, e.quotedProto,
		yesno(e.ipCsum, "valid", "BAD"), yesno(e.csum, "valid", "BAD"))
}

// sniffICMPErr records the ICMP destination-unreachable and ICMPv6 Packet
// Too Big messages received on dev inside n, until the returned function is
// called, with their checksums checked on the bytes.
func sniffICMPErr(t *testing.T, n *node, dev string) func() []icmpErrSeen {
	t.Helper()
	idx := ifindex(t, n, dev)
	var fd int
	require.NoError(t, n.in(func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_ALL)))
		if err != nil {
			return err
		}
		return unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: idx})
	}), "%s: capture on %s", n.name, dev)
	require.NoError(t, rcvTimeout(fd, 100*time.Millisecond))
	var stop atomic.Bool
	out := make(chan []icmpErrSeen, 1)
	go func() {
		var seen []icmpErrSeen
		buf := make([]byte, 65536)
		for !stop.Load() {
			nr, from, err := unix.Recvfrom(fd, buf, 0)
			if err != nil || nr < 28 {
				continue
			}
			if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
				continue
			}
			d := buf[:nr]
			switch d[0] >> 4 {
			case 4:
				ihl := int(d[0]&0xf) * 4
				if d[9] != unix.IPPROTO_ICMP || len(d) < ihl+8 || d[ihl] != 3 {
					continue
				}
				m := d[ihl:]
				e := icmpErrSeen{src: netip.AddrFrom4([4]byte(d[12:16])), dst: netip.AddrFrom4([4]byte(d[16:20])),
					typ: m[0], code: m[1], mtu: uint32(binary.BigEndian.Uint16(m[6:])), quoted: len(m) - 8, length: len(d),
					ipCsum: fold16(sum16(0, d[:ihl])) == 0, csum: fold16(sum16(0, m)) == 0}
				if len(m) >= 28 {
					e.quotedDst, e.quotedProto = netip.AddrFrom4([4]byte(m[24:28])), m[17]
				}
				seen = append(seen, e)
			case 6:
				if len(d) < 48 || d[6] != unix.IPPROTO_ICMPV6 || d[40] != 2 {
					continue
				}
				m := d[40:]
				l := len(m)
				ps := append(append([]byte{}, d[8:40]...), byte(l>>24), byte(l>>16), byte(l>>8), byte(l), 0, 0, 0, unix.IPPROTO_ICMPV6)
				e := icmpErrSeen{src: netip.AddrFrom16([16]byte(d[8:24])), dst: netip.AddrFrom16([16]byte(d[24:40])),
					typ: m[0], code: m[1], mtu: binary.BigEndian.Uint32(m[4:]), quoted: l - 8, length: len(d),
					ipCsum: true, csum: fold16(sum16(0, append(ps, m...))) == 0}
				if len(m) >= 48 {
					e.quotedDst, e.quotedProto = netip.AddrFrom16([16]byte(m[32:48])), m[14]
				}
				seen = append(seen, e)
			}
		}
		unix.Close(fd)
		out <- seen
	}()
	return func() []icmpErrSeen { stop.Store(true); return <-out }
}
