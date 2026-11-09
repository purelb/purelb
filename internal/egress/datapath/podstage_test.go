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
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egress/cnisock"
)

func pfx(s ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range s {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

// TestIntervals merges prefixes and port ranges into interval elements.
func TestIntervals(t *testing.T) {
	keys := func(es []nftables.SetElement) []string {
		var out []string
		for _, e := range es {
			a, _ := netip.AddrFromSlice(e.Key)
			s := a.String()
			if len(e.Key) == 2 {
				s = strconv.Itoa(int(e.Key[0])<<8 | int(e.Key[1]))
			}
			if e.IntervalEnd {
				s = ")" + s
			}
			out = append(out, s)
		}
		return out
	}
	v4, v6 := nftFamilies[0], nftFamilies[1]
	got := intervals(pfx("10.0.0.0/16", "10.0.5.0/24", "10.1.0.0/16", "192.0.2.7/32", "fd00::/8", "255.255.255.255/32"), v4)
	assert.Equal(t, []string{"10.0.0.0", ")10.2.0.0", "192.0.2.7", ")192.0.2.8", "255.255.255.255"}, keys(got),
		"contained and adjacent merged; the last address has no end; the other family left out")
	assert.Equal(t, []string{"fe80::", ")fec0::", "ff00::"}, keys(intervals(pfx("ff00::/8", "fe80::/10", "10.0.0.0/8"), v6)))
	assert.Equal(t, []string{"0.0.0.0"}, keys(intervals(pfx("0.0.0.0/0"), v4)))
	ports := portIntervals([]PortRange{{First: 80, Last: 80}, {First: 8000, Last: 8100}, {First: 81, Last: 90}, {First: 65000, Last: 65535}})
	assert.Equal(t, []string{"80", ")91", "8000", ")8101", "65000"}, keys(ports))
}

// whoUDP answers each datagram to addr:port in h with the source it came
// from. Bound to the address, so the answer comes from it: with two IPv6
// addresses on one link, source selection may pick the other.
func whoUDP(t *testing.T, h *host, addr string, port int) {
	t.Helper()
	var pc net.PacketConn
	require.NoError(t, h.in(func() error {
		var err error
		pc, err = net.ListenPacket("udp", net.JoinHostPort(addr, strconv.Itoa(port)))
		return err
	}))
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		b := make([]byte, 64)
		for {
			_, from, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo([]byte(from.String()), from)
		}
	}()
}

// askUDP sends to dst:port from h, from sport when it isn't 0, and returns
// the source the server saw: "" when nothing came back. It retries for up
// to wait, for a fresh veth's neighbour discovery.
func askUDP(t *testing.T, h *host, dst string, port, sport int, wait time.Duration) string {
	t.Helper()
	var seen string
	require.NoError(t, h.in(func() error {
		d := net.Dialer{LocalAddr: &net.UDPAddr{Port: sport}}
		c, err := d.Dial("udp", net.JoinHostPort(dst, strconv.Itoa(port)))
		if err != nil {
			return err
		}
		defer c.Close()
		b := make([]byte, 64)
		for end := time.Now().Add(wait); ; {
			if _, err := c.Write([]byte("?")); err != nil {
				return err
			}
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if n, err := c.Read(b); err == nil {
				seen = string(b[:n])
				return nil
			}
			if time.Now().After(end) {
				return nil
			}
		}
	}))
	return seen
}

// whoTCP answers each connection on port in h with its peer and the MSS
// negotiated.
func whoTCP(t *testing.T, h *host, port int) {
	t.Helper()
	var ln net.Listener
	require.NoError(t, h.in(func() error {
		var err error
		ln, err = net.Listen("tcp", ":"+strconv.Itoa(port))
		return err
	}))
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mss := 0
			if rc, err := c.(*net.TCPConn).SyscallConn(); err == nil {
				_ = rc.Control(func(fd uintptr) { mss, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG) })
			}
			_, _ = fmt.Fprintf(c, "%s %d\n", c.RemoteAddr(), mss)
			_ = c.Close()
		}
	}()
}

// askTCP connects from h, advertising mss when it isn't 0, and returns
// the server's answer: peer and MSS.
func askTCP(t *testing.T, h *host, dst string, port, mss int) (string, int) {
	t.Helper()
	var peer string
	var seen int
	require.NoError(t, h.in(func() error {
		d := net.Dialer{Timeout: time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
			if mss == 0 {
				return nil
			}
			var err error
			_ = rc.Control(func(fd uintptr) { err = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG, mss) })
			return err
		}}
		c, err := d.Dial("tcp", net.JoinHostPort(dst, strconv.Itoa(port)))
		if err != nil {
			return err
		}
		defer c.Close()
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			return err
		}
		f := strings.Fields(line)
		peer = f[0]
		seen, err = strconv.Atoi(f[1])
		return err
	}))
	return peer, seen
}

// rawListen records the sources of protocol 253 packets reaching h.
func rawListen(t *testing.T, h *host, v6 bool) <-chan string {
	t.Helper()
	network := "ip4:253"
	if v6 {
		network = "ip6:253"
	}
	var pc net.PacketConn
	require.NoError(t, h.in(func() error {
		var err error
		pc, err = net.ListenPacket(network, "")
		return err
	}))
	t.Cleanup(func() { _ = pc.Close() })
	ch := make(chan string, 64)
	go func() {
		b := make([]byte, 64)
		for {
			_, from, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			ch <- from.String()
		}
	}()
	return ch
}

// rawSend sends a protocol 253 packet (no ports: D4) from h to dst.
func rawSend(t *testing.T, h *host, dst string) {
	t.Helper()
	network := "ip4:253"
	if strings.Contains(dst, ":") {
		network = "ip6:253"
	}
	require.NoError(t, h.in(func() error {
		c, err := net.Dial(network, dst)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Write([]byte("portless"))
		return err
	}))
}

// rawFrom is the source of the next packet on ch, "" if none comes.
func rawFrom(ch <-chan string) string {
	select {
	case s := <-ch:
		return s
	case <-time.After(300 * time.Millisecond):
		return ""
	}
}

// inSlice reports whether addr:port has addr want and a port in the
// slice [first, last].
func inSlice(seen, want string, first, last int) bool {
	ap, err := netip.ParseAddrPort(seen)
	if err != nil || ap.Addr().String() != want {
		return false
	}
	return int(ap.Port()) >= first && int(ap.Port()) <= last
}

// ctMarks are the conntrack marks of h's flows from src, by destination
// address and port.
func ctMarks(t *testing.T, h *host, src string) map[string]uint32 {
	t.Helper()
	out := map[string]uint32{}
	fam := netlink.InetFamily(unix.AF_INET)
	if strings.Contains(src, ":") {
		fam = unix.AF_INET6
	}
	require.NoError(t, h.in(func() error {
		flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, fam)
		if err != nil {
			return err
		}
		for _, f := range flows {
			if f.Forward.SrcIP.String() == src {
				out[net.JoinHostPort(f.Forward.DstIP.String(), strconv.Itoa(int(f.Forward.DstPort)))] = f.Mark
			}
		}
		return nil
	}))
	return out
}

// markProbe counts, at n's forward hook, the packets from src to UDP
// port: all of them, and those whose mark has field value v (field mask
// under shift), as routing sees them.
func markProbe(t *testing.T, x *stageNet, src string, port uint16, v uint32, shift uint8) func() (all, marked uint64) {
	t.Helper()
	// A fresh table: an earlier probe's counters are gone.
	tbl := x.nft.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "probe"})
	x.nft.DelTable(tbl)
	x.nft.AddTable(tbl)
	ch := x.nft.AddChain(&nftables.Chain{Name: "fwd", Table: tbl, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityRef(100)})
	f := familyOf(netip.MustParseAddr(src))
	match := cat(f.match(), f.saddr(1), []expr.Any{&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: netip.MustParseAddr(src).AsSlice()}},
		udpPort(port))
	for _, name := range []string{"all", "marked"} {
		x.nft.AddObj(&nftables.NamedObj{Table: tbl, Name: name, Type: nftables.ObjTypeCounter, Obj: &expr.Counter{}})
		e := match
		if name == "marked" {
			e = cat(match, markField{shift: shift}.load(), []expr.Any{&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne32(v << shift)}})
		}
		x.nft.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: cat(e, []expr.Any{&expr.Objref{Type: int(nftables.ObjTypeCounter), Name: name}})})
	}
	require.NoError(t, x.nft.Flush())
	t.Cleanup(func() {
		x.nft.DelTable(tbl)
		_ = x.nft.Flush()
	})
	return func() (uint64, uint64) {
		objs, err := x.nft.GetNamedObjects(tbl)
		require.NoError(t, err)
		n := map[string]uint64{}
		for _, o := range objs {
			n[o.(*nftables.NamedObj).Name] = o.(*nftables.NamedObj).Obj.(*expr.Counter).Packets
		}
		return n["all"], n["marked"]
	}
}

// sendUDP sends n datagrams on one flow from h to dst:port.
func sendUDP(t *testing.T, h *host, dst string, port, n int) {
	t.Helper()
	require.NoError(t, h.in(func() error {
		c, err := net.Dial("udp", net.JoinHostPort(dst, strconv.Itoa(port)))
		if err != nil {
			return err
		}
		defer c.Close()
		for i := 0; i < n; i++ {
			if _, err := c.Write([]byte("x")); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}))
}

// stageNet is a node n between two pods, p (selected) and q (unselected),
// each on its own veth, and a server s; s routes the egress addresses and
// pods back through n. s also has a cluster address, 10.0.2.3 / fd02::3.
type stageNet struct {
	p, q, r, n, s *host
	nft           *nftables.Conn
}

func newStageNet(t *testing.T) *stageNet {
	t.Helper()
	x := &stageNet{p: newHost(t, "p"), q: newHost(t, "q"), r: newHost(t, "r"), n: newHost(t, "n"), s: newHost(t, "s")}
	wire(t, x.p, "p0", x.n, "np")
	wire(t, x.q, "q0", x.n, "nq")
	wire(t, x.r, "r0", x.n, "nr")
	wire(t, x.s, "s0", x.n, "ns")
	for _, a := range []struct {
		h         *host
		dev, cidr string
	}{
		{x.p, "p0", "10.0.1.5/24"}, {x.p, "p0", "fd01::5/64"}, {x.n, "np", "10.0.1.1/24"}, {x.n, "np", "fd01::1/64"},
		{x.q, "q0", "10.0.3.5/24"}, {x.q, "q0", "fd03::5/64"}, {x.n, "nq", "10.0.3.1/24"}, {x.n, "nq", "fd03::1/64"},
		{x.r, "r0", "10.0.4.5/24"}, {x.r, "r0", "fd04::5/64"}, {x.n, "nr", "10.0.4.1/24"}, {x.n, "nr", "fd04::1/64"},
		{x.s, "s0", "10.0.2.2/24"}, {x.s, "s0", "fd02::2/64"}, {x.n, "ns", "10.0.2.1/24"}, {x.n, "ns", "fd02::1/64"},
		{x.s, "s0", "10.0.2.3/24"}, {x.s, "s0", "fd02::3/64"},
	} {
		a.h.addr(t, a.dev, a.cidr)
	}
	for _, c := range []struct {
		h      *host
		v4, v6 string
	}{{x.p, "10.0.1.1", "fd01::1"}, {x.q, "10.0.3.1", "fd03::1"}, {x.r, "10.0.4.1", "fd04::1"}, {x.s, "10.0.2.1", "fd02::1"}} {
		c.h.route(t, &netlink.Route{Dst: cidr("0.0.0.0/0"), Gw: net.ParseIP(c.v4)})
		c.h.route(t, &netlink.Route{Dst: cidr("::/0"), Gw: net.ParseIP(c.v6)})
	}
	x.n.sysctl(t, "net.ipv4.ip_forward", "1")
	x.n.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	nft, err := NftConn(nftables.WithNetNSFd(int(x.n.ns)))
	require.NoError(t, err)
	x.nft = nft
	return x
}

// testStage: pod p (10.0.1.5) is pod-set 10, policy a, a slice of
// 10.9.9.9 / fd09::9 for 10.0.2.0/24 / fd02::/64 except UDP 7000 to
// 10.0.2.2 / fd02::2, with its UDP 5060 reserved; pod r (10.0.4.5) is
// pod-set 11, PerPod policy b, the whole of 10.9.9.10 / fd09::10 for every
// destination.
func testStage() PodStage {
	return PodStage{SourceIP: true,
		Cluster: pfx("10.0.2.3/32", "fd02::3/128", "10.0.1.0/24", "fd01::/64", "10.0.3.0/24", "fd03::/64", "10.0.4.0/24", "fd04::/64"),
		Pods: map[netip.Addr]uint8{netip.MustParseAddr("10.0.1.5"): 10, netip.MustParseAddr("fd01::5"): 10,
			netip.MustParseAddr("10.0.4.5"): 11, netip.MustParseAddr("fd04::5"): 11},
		PodSets: map[uint8][]string{10: {"a"}, 11: {"b"}},
		Policies: map[string]Policy{
			"a": {
				V4: &PolicyFamily{Include: []Destination{{Prefixes: pfx("10.0.2.0/24")}},
					Exclude: []Destination{{Prefixes: pfx("10.0.2.2/32"), Ports: []PortRange{{Proto: unix.IPPROTO_UDP, First: 7000, Last: 7000}}}},
					Slot:    1, Address: netip.MustParseAddr("10.9.9.9")},
				V6: &PolicyFamily{Include: []Destination{{Prefixes: pfx("fd02::/64")}},
					Exclude: []Destination{{Prefixes: pfx("fd02::2/128"), Ports: []PortRange{{Proto: unix.IPPROTO_UDP, First: 7000, Last: 7000}}}},
					Slot:    2, Address: netip.MustParseAddr("fd09::9")},
			},
			"b": {
				V4: &PolicyFamily{Include: []Destination{{Prefixes: pfx("0.0.0.0/0")}}, Slot: 3, Whole: true, Address: netip.MustParseAddr("10.9.9.10")},
				V6: &PolicyFamily{Include: []Destination{{Prefixes: pfx("::/0")}}, Slot: 4, Whole: true, Address: netip.MustParseAddr("fd09::10")},
			},
		},
		Slots: map[uint8]Slot{
			1: {Addr: netip.MustParseAddr("10.9.9.9"), First: 20480, Last: 24575, MSS: 1300,
				Reserved: []ReservedPort{{Pod: netip.MustParseAddr("10.0.1.5"), Proto: unix.IPPROTO_UDP, Port: 5060}}},
			2: {Addr: netip.MustParseAddr("fd09::9"), First: 20480, Last: 24575, MSS: 1280},
			3: {Addr: netip.MustParseAddr("10.9.9.10")},
			4: {Addr: netip.MustParseAddr("fd09::10")},
		},
	}
}

// TestNetnsPodStage checks the pod-node stage with real flows, both
// families: classification per (pod, destination), the slice SNAT, a
// reserved port, PerPod, the port-less rule, FailClosed and FailOpen, the
// MSS clamp, and the mark variant.
func TestNetnsPodStage(t *testing.T) {
	netnsOrSkip(t)
	x := newStageNet(t)
	for _, addr := range []string{"10.0.2.2", "10.0.2.3", "fd02::2", "fd02::3"} {
		for _, port := range []int{7000, 7001} {
			whoUDP(t, x.s, addr, port)
		}
	}
	whoTCP(t, x.s, 8000)
	// The mark probes' flows: one answered, so its later packets are
	// established and get their slot from the ct mark; one not.
	whoUDP(t, x.s, "10.0.2.2", 7003)
	udpListen(t, x.s, 7004)
	raw4, raw6 := rawListen(t, x.s, false), rawListen(t, x.s, true)
	for _, f := range []struct{ name, s, p string }{{"IPv4", "10.0.2.2", "10.0.1.5"}, {"IPv6", "fd02::2", "fd01::5"}} {
		seen := askUDP(t, x.p, f.s, 7001, 0, 5*time.Second)
		require.True(t, strings.HasPrefix(seen, f.p) || strings.HasPrefix(seen, "["+f.p), "%s baseline: %s", f.name, seen)
	}

	stage := testStage()
	tbl := Table{Hardening: Hardening{TunnelPort: 6082}, Pods: &stage}
	require.NoError(t, SyncTable(x.nft, tbl))
	ok, err := TableIntact(x.nft, tbl)
	require.NoError(t, err)
	assert.True(t, ok, "intact as written")
	// Each slot's SNAT reads back, as a restarted agent recovers its id.
	back, err := TableSlots(x.nft)
	require.NoError(t, err)
	assert.Equal(t, map[uint8]SlotNAT{1: {netip.MustParseAddr("10.9.9.9"), 20480, 24575},
		2: {netip.MustParseAddr("fd09::9"), 20480, 24575}, 3: {Addr: netip.MustParseAddr("10.9.9.10")},
		4: {Addr: netip.MustParseAddr("fd09::10")}}, back, "the slice rule, not the reserved port's")

	for _, f := range []struct {
		name, s, cluster, p, q, r, eg, whole string
		raw                                  <-chan string
		slot                                 uint32
	}{
		{"IPv4", "10.0.2.2", "10.0.2.3", "10.0.1.5", "10.0.3.5", "10.0.4.5", "10.9.9.9", "10.9.9.10", raw4, 1},
		{"IPv6", "fd02::2", "fd02::3", "fd01::5", "fd03::5", "fd04::5", "fd09::9", "fd09::10", raw6, 2},
	} {
		t.Run(f.name, func(t *testing.T) {
			src := func(seen string) string {
				ap, err := netip.ParseAddrPort(seen)
				if err != nil {
					return seen
				}
				return ap.Addr().String()
			}
			// p's flows to its policy's destinations leave in its slice.
			seen := askUDP(t, x.p, f.s, 7001, 0, time.Second)
			assert.True(t, inSlice(seen, f.eg, 20480, 24575), "a policy destination: %s", seen)
			seen, mss := askTCP(t, x.p, f.s, 8000, 0)
			assert.True(t, inSlice(seen, f.eg, 20480, 24575), "TCP: %s", seen)
			assert.LessOrEqual(t, mss, int(stage.Slots[uint8(f.slot)].MSS), "the SYN clamped")
			_, mss = askTCP(t, x.p, f.s, 8000, 1000)
			assert.LessOrEqual(t, mss, 1000, "a smaller MSS is never raised (the kernel's rule)")
			assert.Greater(t, mss, 900, "a smaller MSS is kept")
			// Excluded and cluster destinations, and the unselected pod,
			// leave from the pod's address.
			assert.Equal(t, f.p, src(askUDP(t, x.p, f.s, 7000, 0, time.Second)), "an excluded destination")
			assert.Equal(t, f.p, src(askUDP(t, x.p, f.cluster, 7001, 0, time.Second)), "a cluster destination")
			assert.Equal(t, f.q, src(askUDP(t, x.q, f.s, 7001, 0, 5*time.Second)), "an unselected pod")
			marks := ctMarks(t, x.n, f.p)
			assert.Equal(t, f.slot, marks[net.JoinHostPort(f.s, "7001")], "the slot in the ct mark")
			assert.Equal(t, uint32(noEgress), marks[net.JoinHostPort(f.s, "7000")], "an excluded flow is cached not egress")
			assert.Equal(t, uint32(noEgress), marks[net.JoinHostPort(f.cluster, "7001")], "so is a cluster one")
			// PerPod: the whole address, the pod's port, any protocol.
			assert.Equal(t, net.JoinHostPort(f.whole, "7777"), askUDP(t, x.r, f.s, 7001, 7777, 5*time.Second),
				"PerPod keeps the port")
			rawSend(t, x.r, f.s)
			assert.Equal(t, f.whole, rawFrom(f.raw), "PerPod carries a port-less protocol")
			// A port-less protocol on a slice is dropped and counted.
			rawSend(t, x.p, f.s)
			assert.Empty(t, rawFrom(f.raw), "port-less on a slice: FailClosed")
			drops, err := PortlessDrops(x.nft, stage)
			require.NoError(t, err)
			assert.Equal(t, uint64(1), drops[netip.MustParseAddr(f.eg)])
		})
	}
	seen := askUDP(t, x.p, "10.0.2.2", 7001, 5060, time.Second)
	assert.Equal(t, "10.9.9.9:5060", seen, "the reserved port")

	// Routing sees the slot on every packet of a flow: the first from
	// classification, the rest, established by the answers, restored from
	// the ct mark (D18).
	probe := markProbe(t, x, "10.0.1.5", 7003, 1, 0)
	sendUDP(t, x.p, "10.0.2.2", 7003, 5)
	all, marked := probe()
	assert.Equal(t, uint64(5), all)
	assert.Equal(t, all, marked, "every packet carries slot 1")
	assert.Equal(t, uint32(1), ctMarks(t, x.n, "10.0.1.5")["10.0.2.2:7003"], "answered: established")

	// The intact check sees a verdict changed under the same key.
	tb := &nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable}
	pods4, err := x.nft.GetSetByName(tb, "pods4")
	require.NoError(t, err)
	key := netip.MustParseAddr("10.0.1.5").AsSlice()
	require.NoError(t, x.nft.SetDeleteElements(pods4, []nftables.SetElement{{Key: key}}))
	require.NoError(t, x.nft.SetAddElements(pods4, []nftables.SetElement{{Key: key,
		VerdictData: &expr.Verdict{Kind: expr.VerdictGoto, Chain: "ps_11"}}}))
	require.NoError(t, x.nft.Flush())
	ok, err = TableIntact(x.nft, tbl)
	require.NoError(t, err)
	assert.False(t, ok, "a pod's pod-set changed")

	// FailOpen: a port-less flow leaves from the pod's address, still
	// counted; with no slot, every flow does. FailClosed with no slot: none.
	stage = testStage()
	stage.Policies["a"].V4.FailOpen = true
	require.NoError(t, SyncTable(x.nft, Table{Pods: &stage}))
	rawSend(t, x.p, "10.0.2.2")
	assert.Equal(t, "10.0.1.5", rawFrom(raw4), "port-less, FailOpen")
	drops, err := PortlessDrops(x.nft, stage)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), drops[netip.MustParseAddr("10.9.9.9")], "counted after the table was replaced")
	stage.Policies["a"].V4.Slot = 0
	require.NoError(t, SyncTable(x.nft, Table{Pods: &stage}))
	assert.Equal(t, "10.0.1.5", strings.Split(askUDP(t, x.p, "10.0.2.2", 7001, 0, time.Second), ":")[0], "no slot, FailOpen")
	stage.Policies["a"].V4.FailOpen = false
	require.NoError(t, SyncTable(x.nft, Table{Pods: &stage}))
	assert.Empty(t, askUDP(t, x.p, "10.0.2.2", 7001, 0, time.Second), "no slot, FailClosed")

	// The mark variant, in the second byte: p's packets carry its pod-set
	// id, as pl_pod_tag writes it; q's carry nothing.
	require.NoError(t, x.n.in(func() error { return netlink.ConntrackTableFlush(netlink.ConntrackTable) }))
	stage = testStage()
	stage.SourceIP, stage.MarkShift = false, 8
	require.NoError(t, SyncTable(x.nft, Table{Pods: &stage}))
	x.n.attach(t, "np", markProg(t, 10<<8), ebpf.AttachTCXIngress)
	assert.True(t, inSlice(askUDP(t, x.p, "10.0.2.2", 7001, 0, time.Second), "10.9.9.9", 20480, 24575), "tagged")
	assert.Equal(t, "10.0.3.5", strings.Split(askUDP(t, x.q, "10.0.2.2", 7001, 0, time.Second), ":")[0], "untagged")
	assert.Equal(t, uint32(1<<8), ctMarks(t, x.n, "10.0.1.5")["10.0.2.2:7001"]&0xff00, "the slot in the ct mark's second byte")
	// A pod-set id never reaches routing: to a cluster destination the
	// tagged packets go with the field cleared.
	probe = markProbe(t, x, "10.0.1.5", 7004, 0, 8)
	sendUDP(t, x.p, "10.0.2.3", 7004, 3)
	all, marked = probe()
	assert.Equal(t, uint64(3), all)
	assert.Equal(t, all, marked, "the field is cleared")

	// Intact no longer once a map element is gone.
	snat, err := x.nft.GetSetByName(tb, "snat")
	require.NoError(t, err)
	require.NoError(t, x.nft.SetDeleteElements(snat, []nftables.SetElement{{Key: ne32(1 << 8)}}))
	require.NoError(t, x.nft.Flush())
	ok, err = TableIntact(x.nft, Table{Pods: &stage})
	require.NoError(t, err)
	assert.False(t, ok, "a map element deleted")
}

// TestNetnsPodStageHelper runs FTP's helper on a slot (D23): the PORT
// command reaches the server rewritten to the egress address. Port 2121:
// an nft-assigned helper runs on the port its rule names, and binding 21
// needs CAP_NET_BIND_SERVICE.
// A rewrite keeps the counts of the counters it still has, in both families;
// SyncTable starts them from zero, for a new session; a rewrite with no table
// to read from writes it.
func TestNetnsRewriteKeepsCounters(t *testing.T) {
	netnsOrSkip(t)
	x := newStageNet(t)
	for _, addr := range []string{"10.0.2.2", "fd02::2"} {
		whoUDP(t, x.s, addr, 7001)
	}
	stage := testStage()
	tbl := Table{Hardening: Hardening{TunnelPort: 6082}, Pods: &stage}
	require.NoError(t, SyncTable(x.nft, tbl))
	slots := map[uint8]netip.Addr{1: netip.MustParseAddr("10.9.9.9"), 2: netip.MustParseAddr("fd09::9")}
	probe := func() {
		sendUDP(t, x.p, cnisock.ProbeV4.String(), cnisock.ProbePort, 3)
		sendUDP(t, x.p, cnisock.ProbeV6.String(), cnisock.ProbePort, 3)
	}
	// A datagram can wait for its first hop's neighbour.
	seen := func(f string, want uint64) uint64 {
		var n uint64
		for end := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			c, err := ProbeCounts(x.nft)
			require.NoError(t, err)
			if n = c[f].Seen; n == want || time.Now().After(end) {
				return n
			}
		}
	}
	probe()
	require.EqualValues(t, 3, seen("4", 3))
	require.EqualValues(t, 3, seen("6", 3))
	require.NotEmpty(t, askUDP(t, x.p, "10.0.2.2", 7001, 0, 5*time.Second))
	require.NotEmpty(t, askUDP(t, x.p, "fd02::2", 7001, 0, 5*time.Second))
	traffic, err := SlotTraffic(x.nft, slots)
	require.NoError(t, err)
	for _, addr := range slots {
		require.NotZero(t, traffic[addr].Packets[TrafficOutPod], addr)
		require.NotZero(t, traffic[addr].Bytes[TrafficOutPod], addr)
	}

	// A different table, as when a pod comes or goes, and counting goes on.
	tbl.Hardening.TunnelPort = 6083
	require.NoError(t, RewriteTable(x.nft, tbl))
	ok, err := TableIntact(x.nft, tbl)
	require.NoError(t, err)
	assert.True(t, ok, "intact as rewritten")
	kept, err := SlotTraffic(x.nft, slots)
	require.NoError(t, err)
	assert.Equal(t, traffic, kept, "traffic counts kept")
	assert.EqualValues(t, 3, seen("4", 3))
	assert.EqualValues(t, 3, seen("6", 3))
	probe()
	assert.EqualValues(t, 6, seen("4", 6), "counting goes on")
	assert.EqualValues(t, 6, seen("6", 6), "counting goes on")

	// A new session's first write.
	require.NoError(t, SyncTable(x.nft, tbl))
	c, err := ProbeCounts(x.nft)
	require.NoError(t, err)
	assert.Zero(t, c["4"].Seen)
	assert.Zero(t, c["6"].Seen)

	// The table gone from under the agent: written, with nothing to keep.
	x.nft.DelTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable})
	require.NoError(t, x.nft.Flush())
	require.NoError(t, RewriteTable(x.nft, tbl))
	ok, err = TableIntact(x.nft, tbl)
	require.NoError(t, err)
	assert.True(t, ok, "restored")
}

// Every packet that passes while the table is being rewritten is counted:
// the stage probe judges a node by exact counts, and a rewrite at a node's
// boot can land among its datagrams.
func TestNetnsRewriteCountsThroughout(t *testing.T) {
	netnsOrSkip(t)
	x := newStageNet(t)
	stage := testStage()
	tbl := Table{Hardening: Hardening{TunnelPort: 6082}, Pods: &stage}
	require.NoError(t, SyncTable(x.nft, tbl))
	seen := func() (uint64, uint64) {
		c, err := ProbeCounts(x.nft)
		require.NoError(t, err)
		return c["4"].Seen, c["6"].Seen
	}
	settle := func(want uint64) (v4, v6 uint64) {
		for end := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if v4, v6 = seen(); v4 == want && v6 == want || time.Now().After(end) {
				return v4, v6
			}
		}
	}
	// The first hop's neighbours resolved, so none of the counted wait.
	sendUDP(t, x.p, cnisock.ProbeV4.String(), cnisock.ProbePort, 1)
	sendUDP(t, x.p, cnisock.ProbeV6.String(), cnisock.ProbePort, 1)
	v4, v6 := settle(1)
	require.EqualValues(t, 1, v4)
	require.EqualValues(t, 1, v6)

	// Rewrites on a connection of their own, as the agent's datapath
	// goroutine does while the probe's reader has another.
	rw, err := NftConn(nftables.WithNetNSFd(int(x.n.ns)))
	require.NoError(t, err)
	// A fixed number of rewrites, whatever the machine's speed, with the pod
	// sending until they are done.
	const rewrites = 50
	done := make(chan error, 1)
	go func() {
		t2 := tbl
		for i := range rewrites {
			t2.Hardening.TunnelPort = 6082 + uint16(i%2) // a table that changes
			if err := RewriteTable(rw, t2); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var sent uint64
	var rerr error
	require.NoError(t, x.p.in(func() error {
		var conns []net.Conn
		for _, dst := range []string{cnisock.ProbeV4.String(), cnisock.ProbeV6.String()} {
			c, err := net.Dial("udp", net.JoinHostPort(dst, strconv.Itoa(cnisock.ProbePort)))
			if err != nil {
				return err
			}
			defer c.Close()
			conns = append(conns, c)
		}
		for {
			select {
			case rerr = <-done:
				return nil
			default:
			}
			for _, c := range conns {
				if _, err := c.Write([]byte("x")); err != nil {
					return err
				}
			}
			sent++
			time.Sleep(500 * time.Microsecond)
		}
	}))
	require.NoError(t, rerr)
	v4, v6 = settle(sent + 1)
	t.Logf("%d rewrites; counted IPv4 %d, IPv6 %d of %d", rewrites, v4-1, v6-1, sent)
	assert.Equal(t, sent+1, v4, "IPv4 datagrams counted once each")
	assert.Equal(t, sent+1, v6, "IPv6 datagrams counted once each")

	// The pod stage gone, as when its last selected pod leaves: its chains
	// and counters go; back, its counters start again from zero.
	hard := Table{Hardening: tbl.Hardening}
	require.NoError(t, RewriteTable(x.nft, hard))
	ok, err := TableIntact(x.nft, hard)
	require.NoError(t, err)
	assert.True(t, ok, "intact without the pod stage")
	objs, err := x.nft.GetNamedObjects(&nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable})
	require.NoError(t, err)
	assert.Empty(t, objs, "the pod stage's counters gone")
	require.NoError(t, RewriteTable(x.nft, tbl))
	ok, err = TableIntact(x.nft, tbl)
	require.NoError(t, err)
	assert.True(t, ok, "intact with the pod stage again")
	v4, v6 = seen()
	assert.Zero(t, v4)
	assert.Zero(t, v6)
}

func TestNetnsPodStageHelper(t *testing.T) {
	netnsOrSkip(t)
	x := newStageNet(t)
	ftp := Helper{Name: "ftp", Proto: unix.IPPROTO_TCP, Port: 2121}
	if why := HelperUsable(x.nft, "/sys/module", ftp, false); why != "" {
		if os.Getenv("PURELB_NETNS_TESTS") == "required" && os.Getenv("EGRESS_HELPER_TESTS") == "required" {
			t.Fatalf("FTP helper: %s", why)
		}
		t.Skipf("FTP helper: %s", why)
	}
	assert.Equal(t, "the kernel's H.323 NAT is IPv4 only",
		HelperUsable(x.nft, "/sys/module", Helper{Name: "Q.931", Proto: unix.IPPROTO_TCP, Port: 1720}, true))
	assert.Contains(t, HelperUsable(x.nft, t.TempDir(), ftp, false), "nf_nat_ftp is not loaded")

	var ln net.Listener
	require.NoError(t, x.s.in(func() error {
		var err error
		ln, err = net.Listen("tcp", ":2121")
		return err
	}))
	t.Cleanup(func() { _ = ln.Close() })
	// The server reports each connection's PORT command.
	lines := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					break
				}
				if strings.HasPrefix(line, "PORT") {
					lines <- strings.TrimSpace(line)
				}
			}
			_ = c.Close()
		}
	}()
	// The FTP helper parses a direction only from just after a line it
	// saw end, never its first line: as FTP does, USER comes first, and
	// PORT in a segment of its own.
	port := func() string {
		require.NoError(t, x.p.in(func() error {
			c, err := net.DialTimeout("tcp", "10.0.2.2:2121", 5*time.Second)
			if err != nil {
				return err
			}
			defer c.Close()
			if _, err := fmt.Fprintf(c, "USER egress\r\n"); err != nil {
				return err
			}
			time.Sleep(100 * time.Millisecond)
			_, err = fmt.Fprintf(c, "PORT 10,0,1,5,78,32\r\n")
			return err
		}))
		select {
		case l := <-lines:
			return l
		case <-time.After(2 * time.Second):
			return ""
		}
	}

	stage := testStage()
	require.NoError(t, SyncTable(x.nft, Table{Pods: &stage}))
	assert.Equal(t, "PORT 10,0,1,5,78,32", port(), "no helper: the payload is untouched")
	s := stage.Slots[1]
	s.Helpers = []Helper{ftp}
	stage.Slots[1] = s
	// Added by a rewrite, as the agent does, and rewritten again with it:
	// a helper object is deleted and added by the same name in one
	// transaction.
	for i := range 2 {
		require.NoError(t, RewriteTable(x.nft, Table{Pods: &stage}))
		ok, err := TableIntact(x.nft, Table{Pods: &stage})
		require.NoError(t, err)
		assert.True(t, ok, "intact with a helper object, rewrite %d", i)
		assert.True(t, strings.HasPrefix(port(), "PORT 10,9,9,9,"), "the helper rewrote the PORT command, rewrite %d", i)
	}
}
