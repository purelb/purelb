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
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Spike 1: the decided tunnel, end to end, on this kernel.
//
//   pod --veth-- nodeA ==underlay== nodeB --pool-- server
//
// nodeA is a pod node that is not an announcer: pl_pod_tag marks the pod's
// traffic with slot 3, the slot rule routes it into purelb-eg0, nft SNATs it
// into nodeA's slice, and pl_steer puts the Geneve key on it. nodeB holds the
// egress address on its pool interface: pl_tun_in forwards the transit out
// of the pool interface, and pl_announce returns the server's reply to
// nodeA through the tunnel, where conntrack de-NATs it for the pod.

const (
	slot     = 3
	sliceLo  = 20480 // nodeA's slice: 20480>>12 = index 5 (aligned)
	sliceHi  = 24575
	base     = 1024
	shift    = 12
	nodeAID  = 1
	nodeBID  = 2
	vni      = 1
	tunPort  = 6082
	httpPort = 8080
)

type tunCase struct {
	inner6, under6, l2, pushEth bool
	combined                    bool // pl_announce_all in both roles, plus the negative cases
}

func (c tunCase) String() string {
	f := func(b bool, y, n string) string {
		if b {
			return y
		}
		return n
	}
	s := fmt.Sprintf("inner%s/underlay%s/%s", f(c.inner6, "6", "4"), f(c.under6, "6", "4"), f(c.l2, "L2", "L3"))
	if !c.l2 {
		s += f(c.pushEth, "+push-eth", "")
	}
	return s + f(c.combined, "/announce_all", "")
}

func TestSpike1Tunnel(t *testing.T) {
	requireVM(t)
	for _, inner6 := range []bool{false, true} {
		for _, under6 := range []bool{false, true} {
			for _, mode := range []tunCase{{l2: false, pushEth: false}, {l2: false, pushEth: true}, {l2: true}} {
				c := tunCase{inner6: inner6, under6: under6, l2: mode.l2, pushEth: mode.pushEth}
				t.Run(c.String(), func(t *testing.T) { runTunnel(t, c) })
			}
			// Item 25: the design's single pl_announce (pl_announce_all) in
			// both roles, in the two modes that carry traffic.
			for _, mode := range []tunCase{{l2: false, pushEth: true}, {l2: true}} {
				c := tunCase{inner6: inner6, under6: under6, l2: mode.l2, pushEth: mode.pushEth, combined: true}
				t.Run(c.String(), func(t *testing.T) { runTunnel(t, c) })
			}
		}
	}
}

func runTunnel(t *testing.T, c tunCase) {
	pod, a, b, srv := newNode(t, "pod"), newNode(t, "nodeA"), newNode(t, "nodeB"), newNode(t, "server")

	veth(t, pod, "eth0", a, "vpod")
	veth(t, a, "ul", b, "ul")
	veth(t, b, "pool", srv, "eth0")

	addr(t, pod, "eth0", "10.244.1.5/24")
	addr(t, pod, "eth0", "fd44:1::5/64")
	addr(t, a, "vpod", "10.244.1.1/24")
	addr(t, a, "vpod", "fd44:1::1/64")
	defaultRoute(t, pod, "10.244.1.1")
	defaultRoute(t, pod, "fd44:1::1")

	addr(t, a, "ul", "10.0.0.1/24")
	addr(t, a, "ul", "fd00::1/64")
	addr(t, b, "ul", "10.0.0.2/24")
	addr(t, b, "ul", "fd00::2/64")

	addr(t, b, "pool", "172.30.10.2/24")
	addr(t, b, "pool", "fdaa::2/64")
	addr(t, b, "pool", "172.30.10.110/32") // the egress address, placed on the pool interface
	addr(t, b, "pool", "fdaa::110/128")
	addr(t, srv, "eth0", "172.30.10.1/24")
	addr(t, srv, "eth0", "fdaa::1/64")

	for _, n := range []*node{a, b} {
		n.sysctl(t, "net.ipv4.ip_forward", "1")
		n.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	}

	// purelb-eg0 on both nodes: collect-metadata Geneve, L3 or L2.
	for _, n := range []*node{a, b} {
		g := &netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: "purelb-eg0", MTU: 1400},
			FlowBased: true, Dport: tunPort, InnerProtoInherit: !c.l2}
		require.NoError(t, n.nl.LinkAdd(g), "%s: create purelb-eg0 (%s)", n.name, c)
		if c.l2 {
			// L2 fallback (D3): no neighbour resolution across the tunnel.
			// Without NOARP the node ARPs for the remote destination over
			// purelb-eg0, and ARP frames carry no tunnel key.
			l, err := n.nl.LinkByName("purelb-eg0")
			require.NoError(t, err)
			require.NoError(t, n.nl.LinkSetARPOff(l))
		}
		up(t, n, "purelb-eg0")
	}

	egress := "172.30.10.110"
	server := "172.30.10.1"
	if c.inner6 {
		egress, server = "fdaa::110", "fdaa::1"
	}
	underA, underB := "10.0.0.1", "10.0.0.2"
	if c.under6 {
		underA, underB = "fd00::1", "fd00::2"
	}

	// nodeA: slot rule at pref < 100, its table, the underlay throw backstop.
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		r := netlink.NewRule()
		r.Family, r.Priority, r.Table = fam, 50, 100
		r.Mark, r.Mask = slot, ptr(uint32(0xff))
		require.NoError(t, a.nl.RuleAdd(r))
		dst := "0.0.0.0/0"
		throw := "10.0.0.0/24"
		if fam == netlink.FAMILY_V6 {
			dst, throw = "::/0", "fd00::/64"
		}
		_, d, _ := net.ParseCIDR(dst)
		_, th, _ := net.ParseCIDR(throw)
		route(t, a, &netlink.Route{Dst: d, LinkIndex: ifindex(t, a, "purelb-eg0"), Table: 100})
		route(t, a, &netlink.Route{Dst: th, Type: unix_RTN_THROW, Table: 100})
	}

	// Replies arrive on purelb-eg0 marked REPLY_FLAG (0x80) by pl_tun_in.
	// Strict rp_filter (set here deliberately) would drop them: the external
	// source has no route via purelb-eg0. src_valid_mark makes the reverse
	// lookup use the mark, the mark rule sends it to a table whose default
	// is purelb-eg0, and the pod CIDR is thrown back to main so the
	// de-NATed reply still routes to the pod. IPv6 has no rp_filter.
	a.sysctl(t, "net.ipv4.conf.all.rp_filter", "1")
	a.sysctl(t, "net.ipv4.conf.purelb-eg0.rp_filter", "1")
	a.sysctl(t, "net.ipv4.conf.purelb-eg0.src_valid_mark", "1")
	{
		r := netlink.NewRule()
		r.Family, r.Priority, r.Table = netlink.FAMILY_V4, 51, 200
		r.Mark, r.Mask = 0x80, ptr(uint32(0xff))
		require.NoError(t, a.nl.RuleAdd(r))
		_, any4, _ := net.ParseCIDR("0.0.0.0/0")
		_, pods, _ := net.ParseCIDR("10.244.0.0/16")
		route(t, a, &netlink.Route{Dst: any4, LinkIndex: ifindex(t, a, "purelb-eg0"), Table: 200})
		route(t, a, &netlink.Route{Dst: pods, Type: unix_RTN_THROW, Table: 200})
	}

	nftEgress(t, a, []snatSlot{
		{slot: slot, addr: "172.30.10.110", portLo: sliceLo, portHi: sliceHi},
		{slot: slot, addr: "fdaa::110", portLo: sliceLo, portHi: sliceHi},
	})

	objA, objB := newCore(t), newCore(t)
	tunMAC := func(n *node) [6]uint8 {
		l, err := n.nl.LinkByName("purelb-eg0")
		require.NoError(t, err)
		var m [6]uint8
		copy(m[:], l.Attrs().HardwareAddr)
		return m
	}
	pushEth := uint32(0)
	if c.pushEth {
		pushEth = 1
	}
	l2 := uint32(0)
	if c.l2 {
		l2 = 1
	}
	require.NoError(t, objA.Cfg.Put(uint32(0), coreConfig{SelfNode: nodeAID,
		TunIfindex: uint32(ifindex(t, a, "purelb-eg0")), L2Mode: l2, PushEth: pushEth, TunMac: tunMAC(a)}))
	require.NoError(t, objB.Cfg.Put(uint32(0), coreConfig{SelfNode: nodeBID,
		TunIfindex: uint32(ifindex(t, b, "purelb-eg0")), PoolIfindex: uint32(ifindex(t, b, "pool")),
		L2Mode: l2, PushEth: pushEth, TunMac: tunMAC(b)}))

	pr := func(s string) corePeer {
		return corePeer{Addr: ip16(s), V6: b2u8(netip.MustParseAddr(s).Is6())}
	}
	var st coreSteer
	st.N = 1
	st.Peers[0] = pr(underB)
	require.NoError(t, objA.SteerSlots.Put(uint32(slot), st))

	for _, e := range []string{"172.30.10.110", "fdaa::110"} {
		k := coreAddrKey{Addr: ip16(e), Family: family(e)}
		require.NoError(t, objA.EgressAddrs.Put(k, coreAddrCfg{Base: base, Shift: shift, Announced: 0}))
		require.NoError(t, objB.EgressAddrs.Put(k, coreAddrCfg{Base: base, Shift: shift, Announced: 1}))
		sk := coreSliceKey{Addr: ip16(e), Family: family(e), Index: sliceLo >> shift}
		require.NoError(t, objA.Slices.Put(sk, uint32(nodeAID)))
		require.NoError(t, objB.Slices.Put(sk, uint32(nodeAID)))
	}
	require.NoError(t, objB.Nodes.Put(uint32(nodeAID), pr(underA)))
	require.NoError(t, objB.NodeByAddr.Put(pr(underA), uint32(nodeAID)))
	require.NoError(t, objA.PodDevs.Put(uint32(ifindex(t, a, "vpod")), corePodDev{Slot: slot,
		V4: ip16("10.244.1.5")[0], V6: ip16("fd44:1::5")}))

	announce, tunIn, tunInA := objB.PlAnnounce, objB.PlTunIn, objA.PlTunIn
	if c.combined {
		announce, tunIn, tunInA = objB.PlAnnounceAll, objB.PlAnnounceAll, objA.PlAnnounceAll
		// Slice 6 (24576-28671) belongs to nodeB itself, slice 7
		// (28672-32767) to nobody (owner 0).
		for _, e := range []string{"172.30.10.110", "fdaa::110"} {
			for idx, owner := range map[uint16]uint32{6: nodeBID, 7: 0} {
				sk := coreSliceKey{Addr: ip16(e), Family: family(e), Index: idx}
				require.NoError(t, objA.Slices.Put(sk, owner))
				require.NoError(t, objB.Slices.Put(sk, owner))
			}
		}
	}
	attach(t, a, "vpod", objA.PlPodTag, ebpf.AttachTCXIngress)
	attach(t, a, "purelb-eg0", objA.PlSteer, ebpf.AttachTCXEgress)
	attach(t, a, "purelb-eg0", tunInA, ebpf.AttachTCXIngress)
	attach(t, b, "pool", announce, ebpf.AttachTCXIngress)
	attach(t, b, "purelb-eg0", tunIn, ebpf.AttachTCXIngress)

	// The server reports the source it sees, then echoes a stream.
	var ln net.Listener
	require.NoError(t, srv.in(func() error {
		var err error
		ln, err = net.Listen("tcp", net.JoinHostPort(server, strconv.Itoa(httpPort)))
		return err
	}))
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				fmt.Fprintf(conn, "%s\n", conn.RemoteAddr())
				_, _ = io.Copy(io.Discard, conn)
			}(conn)
		}
	}()

	var conn net.Conn
	err := pod.in(func() error {
		var err error
		conn, err = net.DialTimeout("tcp", net.JoinHostPort(server, strconv.Itoa(httpPort)), 5*time.Second)
		return err
	})
	if !c.l2 && !c.pushEth {
		// Expected: bpf_redirect_neigh refuses a packet with no MAC header,
		// which is what an L3 (innerprotoinherit) Geneve device delivers. L3
		// transit must add one first (bpf_skb_change_head).
		require.Error(t, err, "%s: plain L3 transit was expected to fail", c)
		require.NotZero(t, stat(t, objB.Stats, 5), "transit was accepted before the redirect")
		t.Logf("SPIKE1 EXPECTED-FAIL %s: bpf_redirect_neigh drops an L3 skb without a MAC header (transit=%d, server reached=no)",
			c, stat(t, objB.Stats, 5))
		return
	}
	if err != nil {
		t.Fatalf("%s: pod could not reach the server through the tunnel: %v\n  nodeA:%s\n  nodeB:%s\n  nodeA kernel:%s\n  server kernel:%s",
			c, err, stats(t, objA.Stats), stats(t, objB.Stats),
			kstats(t, a, "purelb-eg0", "vpod"), kstats(t, srv, "eth0"))
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err, "%s: reading the server's reply", c)
	seen, err := netip.ParseAddrPort(line[:len(line)-1])
	require.NoError(t, err)
	require.Equal(t, egress, seen.Addr().String(), "%s: server must see the egress address", c)
	require.True(t, seen.Port() >= sliceLo && seen.Port() <= sliceHi,
		"%s: source port %d must be in nodeA's slice %d-%d", c, seen.Port(), sliceLo, sliceHi)

	// Throughput through the tunnel (veth in a VM: relative numbers only).
	buf := make([]byte, 64<<10)
	start := time.Now()
	var sent int64
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for time.Since(start) < time.Second {
		n, err := conn.Write(buf)
		sent += int64(n)
		if err != nil {
			break
		}
	}
	mbps := float64(sent*8) / time.Since(start).Seconds() / 1e6

	require.NotZero(t, stat(t, objA.Stats, 12), "pod_tagged")
	require.NotZero(t, stat(t, objA.Stats, 0), "steered")
	require.NotZero(t, stat(t, objB.Stats, 5), "transit")
	require.NotZero(t, stat(t, objB.Stats, 2), "to_tunnel")
	require.NotZero(t, stat(t, objA.Stats, 8), "reply_local")
	require.Zero(t, stat(t, objA.Stats, 11), "pkt_type_other at nodeA tunnel ingress")
	t.Logf("SPIKE1 PASS %s: server saw %s; %.0f Mbit/s; nodeA:%s nodeB:%s",
		c, seen, mbps, stats(t, objA.Stats), stats(t, objB.Stats))
	if c.combined {
		announceAllCases(t, c, a, b, srv, objA, objB, egress, server, underA, underB)
	}
}

// announceAllCases (item 25) drives the rest of pl_announce_all's decisions
// with traffic, after runTunnel's pod-to-server flow (transit on nodeB's
// purelb-eg0, reply to the tunnel on nodeB's pool interface, reply
// admission on nodeA's purelb-eg0) passed:
//
//	uplink:     a reply to a slice nodeB owns passes to nodeB's stack; a
//	            reply to a slice nobody owns is dropped.
//	purelb-eg0: transit whose source port is in another node's slice, and
//	            transit from an underlay address that is not a node, are
//	            dropped (an in-slice injected datagram is the control); a
//	            reply for a slice nodeA does not own is dropped at nodeA (an
//	            injected reply for nodeA's slice is the control).
//
// Injected packets are Geneve datagrams sent from the peer's underlay
// address, as its purelb-eg0 would send them.
func announceAllCases(t *testing.T, c tunCase, a, b, srv *node, objA, objB *coreObjects,
	egress, server, underA, underB string) {
	const (
		srvPort    = 9400
		localPort  = 24600 // slice 6: nodeB
		nobodyPort = 28700 // slice 7: owner 0
	)
	eg, sv := netip.MustParseAddr(egress), netip.MustParseAddr(server)
	st := func(o *coreObjects, i uint32) uint64 { return stat(t, o.Stats, i) }
	res := func(what string, ok bool, detail string) {
		t.Logf("SPIKE1 RESULT announce_all %s %s: %s (%s)", c, what, yes(ok, "PASS", "FAIL"), detail)
	}
	srvU := listenUDP(t, srv, jh(server, srvPort))
	srvGot := make(chan string, 64)
	go func() {
		buf := make([]byte, 2048)
		for {
			nr, from, err := srvU.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			srvGot <- string(buf[:nr])
			_, _ = srvU.WriteToUDPAddrPort(buf[:nr], from) // echo
		}
	}()
	drain := func() {
		for len(srvGot) > 0 {
			<-srvGot
		}
	}

	// Uplink, owner is this node: nodeB's own socket on the egress address,
	// in nodeB's slice, gets the server's echo.
	l0 := st(objB, 3)
	var bc *net.UDPConn
	require.NoError(t, b.in(func() error {
		var err error
		bc, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(egress), Port: localPort})
		return err
	}))
	defer bc.Close()
	_, err := bc.WriteToUDP([]byte("owner-local"), &net.UDPAddr{IP: net.ParseIP(server), Port: srvPort})
	require.NoError(t, err)
	_ = bc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	nr, _, err := bc.ReadFromUDP(buf)
	ok := err == nil && string(buf[:nr]) == "owner-local"
	res("uplink owner-local", ok, fmt.Sprintf("nodeB socket on %s:%d got the echo=%v, owner_local +%d", egress, localPort, ok, st(objB, 3)-l0))
	require.True(t, ok, "uplink: a reply to nodeB's own slice must pass to its stack: %v", err)
	require.NotZero(t, st(objB, 3)-l0, "owner_local")
	drain()

	// Uplink, no owner: a listener on nodeB in the unowned slice gets nothing.
	var nb *net.UDPConn
	require.NoError(t, b.in(func() error {
		var err error
		nb, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(egress), Port: nobodyPort})
		return err
	}))
	defer nb.Close()
	n0 := st(objB, 4)
	_, err = srvU.WriteToUDP([]byte("nobody"), &net.UDPAddr{IP: net.ParseIP(egress), Port: nobodyPort})
	require.NoError(t, err)
	_ = nb.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err = nb.ReadFromUDP(buf)
	dropped := err != nil
	res("uplink no-owner", dropped && st(objB, 4) > n0, fmt.Sprintf("listener in slice 7 received=%v, no_owner +%d", !dropped, st(objB, 4)-n0))
	require.True(t, dropped, "uplink: a reply to an unowned slice must not reach the stack")
	require.NotZero(t, st(objB, 4)-n0, "no_owner")

	// inject sends one UDP datagram src:sport -> dst:dport from node `from`
	// (underlay address fromU) into node `to`'s tunnel (underlay toU).
	inject := func(from *node, fromU string, to *node, toU string, src, dst netip.Addr, sport, dport uint16, msg string) error {
		u := udpDatagram(src, dst, sport, dport, []byte(msg))
		var pkts [][]byte
		if src.Is6() {
			pkts = ipv6Packets(src, dst, nil, nil, false, 0, nil, nil, unix.IPPROTO_UDP, u, 0)
		} else {
			pkts = ipv4Packets(src, dst, 0x4242, unix.IPPROTO_UDP, u, 2048)
		}
		var dmac net.HardwareAddr
		if c.l2 {
			l, err := to.nl.LinkByName("purelb-eg0")
			require.NoError(t, err)
			dmac = l.Attrs().HardwareAddr
		}
		return sendGeneve(from, fromU, toU, c.l2, dmac, pkts, []int{0})
	}
	wait := func(want string) bool { return expectLine(srvGot, want, time.Second) }

	t0, f0 := st(objB, 5), st(objB, 6)
	// purelb-eg0 transit at nodeB, injected from nodeA's underlay address.
	require.NoError(t, inject(a, underA, b, underB, eg, sv, sliceLo+40, srvPort, "transit-ok"))
	okT := wait("transit-ok")
	res("tunnel transit control", okT, fmt.Sprintf("injected in-slice transit reached the server=%v, transit +%d", okT, st(objB, 5)-t0))
	require.True(t, okT, "an injected transit datagram in nodeA's slice must reach the server")
	require.NoError(t, inject(a, underA, b, underB, eg, sv, localPort+100, srvPort, "transit-forged"))
	forged := wait("transit-forged")
	res("tunnel transit forged", !forged, fmt.Sprintf("source port in nodeB's slice from nodeA: reached the server=%v, transit_forged +%d", forged, st(objB, 6)-f0))
	require.False(t, forged, "transit from outside the sender's slice must be dropped")
	require.Equal(t, uint64(1), st(objB, 6)-f0, "transit_forged")

	// Transit from an underlay address that is not a node.
	stranger := "10.0.0.9"
	cidr := "10.0.0.9/24"
	if c.under6 {
		stranger, cidr = "fd00::9", "fd00::9/64"
	}
	addr(t, a, "ul", cidr)
	s0 := st(objB, 14)
	require.NoError(t, inject(a, stranger, b, underB, eg, sv, sliceLo+41, srvPort, "transit-stranger"))
	strange := wait("transit-stranger")
	res("tunnel transit unknown sender", !strange, fmt.Sprintf("from %s: reached the server=%v, transit_no_sender +%d", stranger, strange, st(objB, 14)-s0))
	require.False(t, strange, "transit from a non-node underlay address must be dropped")
	require.Equal(t, uint64(1), st(objB, 14)-s0, "transit_no_sender")

	// purelb-eg0 reply admission at nodeA: injected from nodeB.
	r0, x0 := st(objA, 8), st(objA, 9)
	require.NoError(t, inject(b, underB, a, underA, sv, eg, srvPort, sliceLo+42, "reply-ok"))
	require.NoError(t, inject(b, underB, a, underA, sv, eg, srvPort, localPort+101, "reply-not-mine"))
	time.Sleep(300 * time.Millisecond)
	res("tunnel reply admission", st(objA, 8)-r0 == 1 && st(objA, 9)-x0 == 1,
		fmt.Sprintf("reply to nodeA's slice: reply_local +%d; reply to nodeB's slice: reply_not_owner +%d", st(objA, 8)-r0, st(objA, 9)-x0))
	require.Equal(t, uint64(1), st(objA, 8)-r0, "nodeA admits a reply for its own slice")
	require.Equal(t, uint64(1), st(objA, 9)-x0, "nodeA drops a reply for another node's slice")
}

func newCore(t *testing.T) *coreObjects {
	t.Helper()
	var o coreObjects
	require.NoError(t, loadCoreObjects(&o, nil))
	t.Cleanup(func() { o.Close() })
	return &o
}

func ptr[T any](v T) *T { return &v }

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

const unix_RTN_THROW = 9 // linux/rtnetlink.h RTN_THROW
