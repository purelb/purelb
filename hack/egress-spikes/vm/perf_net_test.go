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
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/egress-spikes/vm/perf"
)

// Spike 10, netns measurements. Everything runs in one 2-vCPU QEMU VM over
// veth: the numbers are relative (one configuration against another on the
// same kernel and VM), not hardware throughput. CPU is the VM-wide busy
// time from /proc/stat, so it includes the sender and the receiver.

const window = 2 * time.Second

type fam46 struct {
	name                   string
	v6                     bool
	pod, server, nodeUp    string
	egress, bg, bgDst, bgV string
}

var perfFams = []fam46{
	{"ipv4", false, "10.244.1.5", "172.30.10.1", "172.30.10.2", "172.30.10.110", "10.245.1.5", "172.31.0.0/16", "172.30.10.1"},
	{"ipv6", true, "fd44:1::5", "fdaa::1", "fdaa::2", "fdaa::110", "fd45:1::5", "fdbb::/64", "fdaa::1"},
}

func TestPerfNet(t *testing.T) {
	perf.Require(t)
	t.Logf("SPIKE10 MEM start%s", perf.MemLine())
	for _, s := range []struct {
		name string
		fn   func(*testing.T)
	}{{"nft", perfNft}, {"tunnel", perfTunnel}, {"newflow", perfNewFlow}, {"conntrack", perfConntrack}, {"rules", perfRules}, {"install", perfInstall}} {
		t.Run(s.name, s.fn)
		t.Logf("SPIKE10 MEM after %s%s", s.name, perf.MemLine())
	}
}

// ---- loads -----------------------------------------------------------------

func linkRx(t *testing.T, n *node, dev string) uint64 {
	l, err := n.nl.LinkByName(dev)
	require.NoError(t, err)
	return l.Attrs().Statistics.RxPackets
}

// streamRuns: perf.Runs TCP transfers from -> to (dir 'U') or to -> from
// ('D'), each for window. Returns Gbit/s and busy CPUs per Gbit/s.
func streamRuns(t *testing.T, from *node, hp string, dir byte) (gbps, cpg []float64) {
	for range perf.Runs {
		g, cores, err := perf.Stream(func() (net.Conn, error) { return dialTCP(from, "", hp, 3*time.Second) }, dir, window)
		require.NoError(t, err, "stream %c to %s", dir, hp)
		gbps = append(gbps, g)
		cpg = append(cpg, cores/g)
	}
	return gbps, cpg
}

// serveStream starts perf.Serve on hp inside n.
func serveStream(t *testing.T, n *node, hp string) {
	var ln net.Listener
	require.NoError(t, n.in(func() error {
		var err error
		ln, err = net.Listen("tcp", hp)
		return err
	}))
	t.Cleanup(func() { ln.Close() })
	perf.Serve(ln)
}

// ppsRuns: one established UDP flow (the server answers its first packet,
// so conntrack sees both directions), then perf.Runs windows of 64-byte
// frames sent with sendmmsg (after two unrecorded warm-up windows); packets
// are counted at the server's link and dropped there.
// Returns Mpps and busy-CPU ns per packet.
func ppsRuns(t *testing.T, from, srv *node, server string, port int) (mpps, nsPkt []float64) {
	hp := jh(server, port)
	sc := listenUDP(t, srv, hp)
	ra, err := net.ResolveUDPAddr("udp", hp)
	require.NoError(t, err)
	var c *net.UDPConn
	require.NoError(t, from.in(func() error {
		var err error
		c, err = net.DialUDP("udp", nil, ra)
		return err
	}))
	defer c.Close()
	_, err = c.Write([]byte("hi"))
	require.NoError(t, err)
	_ = sc.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 64)
	_, peer, err := sc.ReadFromUDP(b)
	require.NoError(t, err, "udp: first packet did not reach the server")
	_, err = sc.WriteToUDP([]byte("ok"), peer)
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(b)
	require.NoError(t, err, "udp: the server's answer did not reach the client")
	// From here the server's link drops every packet at tcx ingress (after
	// the link counts it), so the server's stack is not in the measurement.
	drop, err := perf.Dropper()
	require.NoError(t, err)
	defer drop.Close()
	idx := ifindex(t, srv, "eth0")
	var l link.Link
	require.NoError(t, srv.in(func() error {
		var err error
		l, err = link.AttachTCX(link.TCXOptions{Program: drop, Interface: idx, Attach: ebpf.AttachTCXIngress})
		return err
	}))
	defer l.Close()
	size := 18 // 60-byte IPv4 frame, 80-byte IPv6 frame
	// Background load before the windows (should be ~0 busy CPUs).
	i0 := perf.ReadCPU()
	time.Sleep(time.Second)
	t.Logf("SPIKE10 IDLE before pps to %s: busy CPUs %.3f", hp, perf.Cores(i0, perf.ReadCPU()))
	// Runs -2 and -1 warm up and are not recorded.
	for run := -2; run < perf.Runs; run++ {
		rx0 := linkRx(t, srv, "eth0")
		c0 := perf.ReadCPU()
		start := time.Now()
		sent, err := perf.Blast(c, size, window)
		el := time.Since(start)
		c1 := perf.ReadCPU()
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		rx := linkRx(t, srv, "eth0") - rx0
		require.NotZero(t, rx, "no packets reached the server (sent %d)", sent)
		if run < 0 {
			continue
		}
		mpps = append(mpps, float64(rx)/el.Seconds()/1e6)
		nsPkt = append(nsPkt, perf.Cores(c0, c1)*el.Seconds()*1e9/float64(rx))
	}
	return mpps, nsPkt
}

func logStream(t *testing.T, test, cfg, dir string, gbps, cpg []float64) {
	t.Logf("SPIKE10 NET %s | %s | %s | Gbit/s %s", test, cfg, dir, perf.MaxMed(gbps))
	t.Logf("SPIKE10 NET %s | %s | %s | CPUs per Gbit/s %s", test, cfg, dir, perf.MaxMed(cpg))
}

func logPPS(t *testing.T, test, cfg string, mpps, ns []float64) {
	t.Logf("SPIKE10 NET %s | %s | udp 64B | Mpps %s", test, cfg, perf.MaxMed(mpps))
	t.Logf("SPIKE10 NET %s | %s | udp 64B | CPU ns/pkt %s", test, cfg, perf.MinMed(ns))
}

// ---- nft cost per established flow ------------------------------------------

// pnTopo is natTopo's addressing (client -- router -- server) for the modes
// that are not egress: "plain" (forwarding only, no conntrack) and "cni"
// (conntrack and a CNI-style SNAT of pod traffic to the node address at
// srcnat priority, as flannel's masquerade does).
func pnTopo(t *testing.T, mode string) (cli, rtr, srv *node) {
	if mode == "egress" {
		return natTopo(t)
	}
	cli, rtr, srv = newNode(t, "client"), newNode(t, "router"), newNode(t, "server")
	veth(t, cli, "eth0", rtr, "vpod")
	veth(t, rtr, "up0", srv, "eth0")
	addr(t, cli, "eth0", "10.244.1.5/24")
	addr(t, cli, "eth0", "fd44:1::5/64")
	addr(t, rtr, "vpod", "10.244.1.1/24")
	addr(t, rtr, "vpod", "fd44:1::1/64")
	addr(t, rtr, "up0", "172.30.10.2/24")
	addr(t, rtr, "up0", "fdaa::2/64")
	addr(t, srv, "eth0", "172.30.10.1/24")
	addr(t, srv, "eth0", "fdaa::1/64")
	defaultRoute(t, cli, "10.244.1.1")
	defaultRoute(t, cli, "fd44:1::1")
	rtr.sysctl(t, "net.ipv4.ip_forward", "1")
	rtr.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	rtr.sysctl(t, "net.ipv4.conf.all.rp_filter", "1")
	if mode == "plain" {
		defaultRoute(t, srv, "172.30.10.2")
		defaultRoute(t, srv, "fdaa::2")
		return
	}
	c, err := nftables.New(nftables.WithNetNSFd(int(rtr.h)))
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "perf_cni"})
	post := c.AddChain(&nftables.Chain{Name: "post", Table: tbl, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityRef(100)})
	for _, a := range []string{"172.30.10.2", "fdaa::2"} {
		ip := netip.MustParseAddr(a)
		fam, nfp := uint32(unix.NFPROTO_IPV4), byte(unix.NFPROTO_IPV4)
		if ip.Is6() {
			fam, nfp = unix.NFPROTO_IPV6, unix.NFPROTO_IPV6
		}
		c.AddRule(&nftables.Rule{Table: tbl, Chain: post, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{nfp}},
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("vpod")},
			&expr.Immediate{Register: 1, Data: ip.AsSlice()},
			&expr.NAT{Type: expr.NATTypeSourceNAT, Family: fam, RegAddrMin: 1, RegAddrMax: 1},
		}})
	}
	require.NoError(t, c.Flush())
	return
}

func ifname(s string) []byte {
	b := make([]byte, 16)
	copy(b, s)
	return b
}

func perfNft(t *testing.T) {
	for _, mode := range []string{"plain", "cni", "egress"} {
		cli, _, srv := pnTopo(t, mode)
		desc := map[string]string{
			"plain":  "plain forwarding, no conntrack",
			"cni":    "conntrack + CNI-style SNAT to the node address",
			"egress": "egress table (spike 7: new-flow mark, ct-mark restore vmap, per-slot SNAT) + 1 fwmark rule",
		}[mode]
		for _, f := range perfFams {
			cfg := fmt.Sprintf("%s (%s) %s", mode, desc, f.name)
			hp := jh(f.server, 8000)
			serveStream(t, srv, hp)
			for _, dir := range []byte{'U', 'D'} {
				g, c := streamRuns(t, cli, hp, dir)
				logStream(t, "nft", cfg, map[byte]string{'U': "tcp pod->server", 'D': "tcp server->pod"}[dir], g, c)
			}
			m, ns := ppsRuns(t, cli, srv, f.server, 9100)
			logPPS(t, "nft", cfg, m, ns)
		}
	}
}

// ---- tunnel throughput -------------------------------------------------------

// tunTopo builds spike 1's pod -- nodeA == nodeB -- server, in one of:
//
//	plain:  routed, no egress at all
//	direct: nodeA holds the egress address on its uplink and SNATs the
//	        pod's flows to it (pl_pod_tag, spike 7 nft, a slot rule): the
//	        direct path, with the same number of hops as the tunnel
//	l3, l2: spike 1's tunnel through nodeB (L3 with the Ethernet push, or
//	        L2), every program attached as in runTunnel
//
// All links have MTU mtu; purelb-eg0 has mtu less the Geneve overhead, and
// in the tunnel modes the pod's link has the tunnel's MTU (in place of route
// mtu and MSS clamping), so TCP segments fit.
func tunTopo(t *testing.T, mode string, inner6, under6 bool, mtu int) (pod, srv *node, server string) {
	pod, a, b, srv := newNode(t, "pod"), newNode(t, "nodeA"), newNode(t, "nodeB"), newNode(t, "server")
	veth(t, pod, "eth0", a, "vpod")
	veth(t, a, "ul", b, "ul")
	veth(t, b, "pool", srv, "eth0")
	over := 0
	if mode == "l3" || mode == "l2" {
		over = 36
		if under6 {
			over = 56
		}
		if mode == "l2" {
			over += 14
		}
	}
	setMTU := func(n *node, dev string, m int) {
		l, err := n.nl.LinkByName(dev)
		require.NoError(t, err)
		require.NoError(t, n.nl.LinkSetMTU(l, m))
	}
	for _, x := range []struct {
		n   *node
		dev string
		m   int
	}{{pod, "eth0", mtu - over}, {a, "vpod", mtu - over}, {a, "ul", mtu}, {b, "ul", mtu}, {b, "pool", mtu}, {srv, "eth0", mtu}} {
		setMTU(x.n, x.dev, x.m)
	}
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
	addr(t, srv, "eth0", "172.30.10.1/24")
	addr(t, srv, "eth0", "fdaa::1/64")
	defaultRoute(t, srv, "172.30.10.2")
	defaultRoute(t, srv, "fdaa::2")
	for _, n := range []*node{a, b} {
		n.sysctl(t, "net.ipv4.ip_forward", "1")
		n.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	}
	server = "172.30.10.1"
	if inner6 {
		server = "fdaa::1"
	}
	slotRule := func(gw4, gw6 string, dev int) {
		for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			r := netlink.NewRule()
			r.Family, r.Priority, r.Table = fam, 50, 100
			r.Mark, r.Mask = slot, ptr(uint32(0xff))
			require.NoError(t, a.nl.RuleAdd(r))
			dst, gw := "0.0.0.0/0", gw4
			if fam == netlink.FAMILY_V6 {
				dst, gw = "::/0", gw6
			}
			_, d, _ := net.ParseCIDR(dst)
			rt := &netlink.Route{Dst: d, LinkIndex: dev, Table: 100}
			if gw != "" {
				rt.Gw = net.ParseIP(gw)
			}
			route(t, a, rt)
		}
	}
	podTag := func(o *coreObjects) {
		require.NoError(t, o.PodDevs.Put(uint32(ifindex(t, a, "vpod")), corePodDev{Slot: slot,
			V4: ip16("10.244.1.5")[0], V6: ip16("fd44:1::5")}))
		attach(t, a, "vpod", o.PlPodTag, ebpf.AttachTCXIngress)
	}

	switch mode {
	case "plain":
		defaultRoute(t, a, "10.0.0.2")
		defaultRoute(t, a, "fd00::2")
		for _, r := range []struct{ dst, gw string }{{"10.244.1.0/24", "10.0.0.1"}, {"fd44:1::/64", "fd00::1"}} {
			_, d, _ := net.ParseCIDR(r.dst)
			route(t, b, &netlink.Route{Dst: d, Gw: net.ParseIP(r.gw)})
		}
		return pod, srv, server
	case "direct":
		addr(t, a, "ul", "10.0.0.110/32")
		addr(t, a, "ul", "fd00::110/128")
		a.sysctl(t, "net.ipv4.conf.all.rp_filter", "1")
		defaultRoute(t, a, "10.0.0.2")
		defaultRoute(t, a, "fd00::2")
		slotRule("10.0.0.2", "fd00::2", ifindex(t, a, "ul"))
		// The restored slot must not send de-NATed replies back out: the
		// pod networks are thrown back to main (the design's cluster throws).
		for _, th := range []string{"10.244.0.0/16", "fd44::/16"} {
			_, d, _ := net.ParseCIDR(th)
			route(t, a, &netlink.Route{Dst: d, Type: unix_RTN_THROW, Table: 100})
		}
		nftEgress(t, a, []snatSlot{
			{slot: slot, addr: "10.0.0.110", portLo: sliceLo, portHi: sliceHi},
			{slot: slot, addr: "fd00::110", portLo: sliceLo, portHi: sliceHi},
		})
		podTag(newCore(t))
		return pod, srv, server
	}

	l2 := mode == "l2"
	addr(t, b, "pool", "172.30.10.110/32")
	addr(t, b, "pool", "fdaa::110/128")
	for _, n := range []*node{a, b} {
		g := &netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: "purelb-eg0", MTU: mtu - over},
			FlowBased: true, Dport: tunPort, InnerProtoInherit: !l2}
		require.NoError(t, n.nl.LinkAdd(g), "%s: create purelb-eg0", n.name)
		if l2 {
			l, err := n.nl.LinkByName("purelb-eg0")
			require.NoError(t, err)
			require.NoError(t, n.nl.LinkSetARPOff(l))
		}
		up(t, n, "purelb-eg0")
	}
	underA, underB := "10.0.0.1", "10.0.0.2"
	if under6 {
		underA, underB = "fd00::1", "fd00::2"
	}
	slotRule("", "", ifindex(t, a, "purelb-eg0"))
	for _, th := range []string{"10.0.0.0/24", "fd00::/64"} {
		_, d, _ := net.ParseCIDR(th)
		route(t, a, &netlink.Route{Dst: d, Type: unix_RTN_THROW, Table: 100})
	}
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
	push := b2u8(!l2)
	require.NoError(t, objA.Cfg.Put(uint32(0), coreConfig{SelfNode: nodeAID,
		TunIfindex: uint32(ifindex(t, a, "purelb-eg0")), L2Mode: uint32(b2u8(l2)), PushEth: uint32(push), TunMac: tunMAC(a)}))
	require.NoError(t, objB.Cfg.Put(uint32(0), coreConfig{SelfNode: nodeBID,
		TunIfindex: uint32(ifindex(t, b, "purelb-eg0")), PoolIfindex: uint32(ifindex(t, b, "pool")),
		L2Mode: uint32(b2u8(l2)), PushEth: uint32(push), TunMac: tunMAC(b)}))
	pr := func(s string) corePeer { return corePeer{Addr: ip16(s), V6: b2u8(netip.MustParseAddr(s).Is6())} }
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
	podTag(objA)
	attach(t, a, "purelb-eg0", objA.PlSteer, ebpf.AttachTCXEgress)
	attach(t, a, "purelb-eg0", objA.PlTunIn, ebpf.AttachTCXIngress)
	attach(t, b, "pool", objB.PlAnnounce, ebpf.AttachTCXIngress)
	attach(t, b, "purelb-eg0", objB.PlTunIn, ebpf.AttachTCXIngress)
	return pod, srv, server
}

func perfTunnel(t *testing.T) {
	type cfg struct {
		mode   string
		under6 bool
	}
	cfgs := []cfg{{"plain", false}, {"direct", false}, {"l3", false}, {"l3", true}, {"l2", false}, {"l2", true}}
	for _, mtu := range []int{1500, 9000} {
		for _, inner6 := range []bool{false, true} {
			for _, c := range cfgs {
				name := fmt.Sprintf("mtu%d/inner%s/%s", mtu, yes(inner6, "6", "4"), c.mode)
				if c.mode == "l3" || c.mode == "l2" {
					name += "/underlay" + yes(c.under6, "6", "4")
				}
				t.Run(name, func(t *testing.T) {
					pod, srv, server := tunTopo(t, c.mode, inner6, c.under6, mtu)
					hp := jh(server, 8000)
					serveStream(t, srv, hp)
					for _, dir := range []byte{'U', 'D'} {
						g, cp := streamRuns(t, pod, hp, dir)
						logStream(t, "tunnel", name, map[byte]string{'U': "tcp pod->server", 'D': "tcp server->pod"}[dir], g, cp)
						t.Logf("SPIKE10 MEM after %s %c%s slabtop:%s", name, dir, perf.MemLine(), perf.SlabTop(2))
					}
				})
				t.Logf("SPIKE10 MEM after %s%s slabtop:%s", name, perf.MemLine(), perf.SlabTop(4))
			}
		}
	}
}

// ---- new flows until NAT failure ----------------------------------------------

// rawSender opens an IPPROTO_RAW socket inside n.
func rawSender(t *testing.T, n *node, v6 bool) (send func(pkt []byte, dst netip.Addr) error, done func()) {
	var fd int
	require.NoError(t, n.in(func() error {
		af := unix.AF_INET
		if v6 {
			af = unix.AF_INET6
		}
		var err error
		fd, err = unix.Socket(af, unix.SOCK_RAW, unix.IPPROTO_RAW)
		return err
	}))
	return func(pkt []byte, dst netip.Addr) error {
		for {
			err := unix.Sendto(fd, pkt, 0, sockaddr(dst, 0))
			if err == unix.ENOBUFS || err == unix.EAGAIN {
				time.Sleep(time.Millisecond)
				continue
			}
			return err
		}
	}, func() { unix.Close(fd) }
}

func udpPkt(src, dst netip.Addr, sport, dport uint16) []byte {
	l4 := udpDatagram(src, dst, sport, dport, []byte("flow"))
	if src.Is4() {
		return ipv4Packets(src, dst, 0, unix.IPPROTO_UDP, l4, 1500)[0]
	}
	return ipv6Packets(src, dst, nil, nil, false, 0, nil, nil, unix.IPPROTO_UDP, l4, 1500)[0]
}

func ctIn(t *testing.T, n *node) (stats map[string]uint64, count int) {
	require.NoError(t, n.in(func() error {
		var err error
		if stats, err = perf.CTStats(); err != nil {
			return err
		}
		count, err = perf.CTCount()
		return err
	}))
	return stats, count
}

func ctDelta(a, b map[string]uint64) string {
	if b["unavailable"] == 1 {
		return " unavailable (no /proc/net/stat/nf_conntrack on this kernel)"
	}
	var keys []string
	for k := range b {
		if b[k] != a[k] && k != "entries" && k != "searched" && k != "found" && k != "search_restart" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var s strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&s, " %s+%d", k, b[k]-a[k])
	}
	return s.String()
}

// perfNewFlow sends new UDP flows (one packet each, distinct source ports)
// from the pod to one server address and port through the router, from a
// raw socket as fast as it can: "egress" SNATs them into one 4,096-port
// slice, "cni" to the node address with the whole port range (the rate
// baseline). Five runs, the conntrack table flushed before each: flows
// 1-3,584 (the slice up to 87.5%) and flows 3,585-8,192 (past the slice's
// end). Then one pass checks nf_conntrack_count after every flow for the
// first flow that got no entry. The rates include the sender, which shares
// the CPU with the router's softirq processing.
func perfNewFlow(t *testing.T) {
	for _, mode := range []string{"cni", "egress"} {
		for _, f := range perfFams {
			cli, rtr, srv := pnTopo(t, mode)
			rtr.sysctl(t, "net.netfilter.nf_conntrack_udp_timeout", "600")
			ctnl, err := netlink.NewHandleAt(rtr.h, unix.NETLINK_NETFILTER)
			require.NoError(t, err)
			defer ctnl.Close()
			var cntf *os.File
			require.NoError(t, rtr.in(func() error {
				var err error
				cntf, err = os.Open("/proc/sys/net/netfilter/nf_conntrack_count")
				return err
			}))
			defer cntf.Close()
			count := func() int {
				b := make([]byte, 32)
				n, _ := cntf.ReadAt(b, 0)
				var c int
				fmt.Sscanf(string(b[:n]), "%d", &c)
				return c
			}
			sc := listenUDP(t, srv, jh(f.server, 9200))
			rc, err := sc.SyscallConn()
			require.NoError(t, err)
			_ = rc.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 64<<20) })
			seen := map[uint16]bool{}
			got := make(chan struct{})
			go func() {
				defer close(got)
				b := make([]byte, 64)
				for {
					_ = sc.SetReadDeadline(time.Now().Add(3 * time.Second))
					_, from, err := sc.ReadFromUDPAddrPort(b)
					if err != nil {
						return
					}
					seen[from.Port()] = true
				}
			}()
			send, done := rawSender(t, cli, f.v6)
			src, dst := netip.MustParseAddr(f.pod), netip.MustParseAddr(f.server)
			const total, below = 8192, 3584
			pkts := make([][]byte, total)
			for i := range pkts {
				pkts[i] = udpPkt(src, dst, uint16(1024+i), 9200)
			}
			st0, _ := ctIn(t, rtr)
			var r1, r2 []float64
			var mapped []int
			for range perf.Runs {
				require.NoError(t, ctnl.ConntrackTableFlush(netlink.ConntrackTable))
				time.Sleep(200 * time.Millisecond)
				c0 := count()
				s := time.Now()
				for j := 0; j < below; j++ {
					require.NoError(t, send(pkts[j], dst))
				}
				r1 = append(r1, float64(below)/time.Since(s).Seconds())
				s = time.Now()
				for j := below; j < total; j++ {
					require.NoError(t, send(pkts[j], dst))
				}
				r2 = append(r2, float64(total-below)/time.Since(s).Seconds())
				time.Sleep(100 * time.Millisecond)
				mapped = append(mapped, count()-c0)
			}
			require.NoError(t, ctnl.ConntrackTableFlush(netlink.ConntrackTable))
			time.Sleep(200 * time.Millisecond)
			first, c := -1, count()
			for j := 0; j < total; j++ {
				require.NoError(t, send(pkts[j], dst))
				n := count()
				if n == c && first < 0 {
					first = j + 1
				}
				c = n
			}
			done()
			<-got
			st1, _ := ctIn(t, rtr)
			inSl := 0
			for p := range seen {
				if mode == "cni" || inSlice(p) {
					inSl++
				}
			}
			t.Logf("SPIKE10 NATFILL nft %s %s flows 1-%d (slice up to 87.5%%) | flows/s %s", mode, f.name, below, perf.MaxMed(r1))
			t.Logf("SPIKE10 NATFILL nft %s %s flows %d-%d (past a 4096-port slice) | flows/s %s", mode, f.name, below+1, total, perf.MaxMed(r2))
			t.Logf("SPIKE10 NATFILL nft %s %s RESULT ct entries per run %v of %d flows; first flow with no entry: %d; distinct source ports at the server %d (in slice: %d); ct stat deltas:%s",
				mode, f.name, mapped, total, first, len(seen), inSl, ctDelta(st0, st1))
		}
	}
}

// ---- conntrack growth and dump time ---------------------------------------------

// perfConntrack fills the router's conntrack table (natTopo, the egress
// table) with half egress flows (from the pod, ct mark = slot 3, SNATed
// into the slice) and half other flows (from a second namespace, ct mark
// 0), to 10,000 and then 100,000 entries per family, and times a full
// ctnetlink dump (counted, and parsed as `conntrack -L` would), and a dump
// with the kernel-side mark filter (ct mark 3 / 0xff).
func perfConntrack(t *testing.T) {
	require.NoError(t, os.WriteFile("/proc/sys/net/netfilter/nf_conntrack_max", []byte("262144"), 0o644))
	b, _ := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_buckets")
	t.Logf("SPIKE10 CT nf_conntrack_max=262144 nf_conntrack_buckets=%s", strings.TrimSpace(string(b)))
	cli, rtr, _ := natTopo(t)
	bg := newNode(t, "bg")
	veth(t, bg, "eth0", rtr, "vbg")
	addr(t, bg, "eth0", "10.245.1.5/24")
	addr(t, bg, "eth0", "fd45:1::5/64")
	addr(t, rtr, "vbg", "10.245.1.1/24")
	addr(t, rtr, "vbg", "fd45:1::1/64")
	defaultRoute(t, bg, "10.245.1.1")
	defaultRoute(t, bg, "fd45:1::1")
	rtr.sysctl(t, "net.netfilter.nf_conntrack_udp_timeout", "3600")
	ctnl, err := netlink.NewHandleAt(rtr.h, unix.NETLINK_NETFILTER)
	require.NoError(t, err)
	defer ctnl.Close()
	for _, f := range perfFams {
		// natTopo blackholes the server's host route in main, so the
		// gateway is given on-link.
		_, d, _ := net.ParseCIDR(f.bgDst)
		route(t, rtr, &netlink.Route{Dst: d, Gw: net.ParseIP(f.bgV), LinkIndex: ifindex(t, rtr, "up0"), Flags: int(netlink.FLAG_ONLINK)})
		sendE, doneE := rawSender(t, cli, f.v6)
		sendB, doneB := rawSender(t, bg, f.v6)
		pod, srv, bgs := netip.MustParseAddr(f.pod), netip.MustParseAddr(f.server), netip.MustParseAddr(f.bg)
		bgDst := func(i int) netip.Addr {
			if f.v6 {
				a := netip.MustParseAddr("fdbb::").As16()
				a[12], a[13], a[14], a[15] = byte(i>>24), byte(i>>16), byte(i>>8), byte(i)+1
				return netip.AddrFrom16(a)
			}
			return netip.AddrFrom4([4]byte{172, 31, byte(i >> 8), byte(i)})
		}
		time.Sleep(time.Second) // let earlier tests' conntrack objects be freed (RCU)
		_, c0 := ctIn(t, rtr)
		prevCnt, prevSlab, prevMem := c0, 0, perf.MemInfo("Slab")
		if a, _, err := perf.Slab("nf_conntrack"); err == nil {
			prevSlab = a
		}
		have := 0
		for _, target := range []int{10_000, 100_000} {
			half := target / 2
			for i := have / 2; i < half; i++ {
				// Egress: at most 2,000 flows per server port, well inside
				// one 4,096-port slice per (server, port).
				require.NoError(t, sendE(udpPkt(pod, srv, uint16(30000+i%2000), uint16(10000+i/2000)), srv))
				require.NoError(t, sendB(udpPkt(bgs, bgDst(i), 40000, 9), bgDst(i)))
			}
			have = target
			time.Sleep(200 * time.Millisecond)
			_, cnt := ctIn(t, rtr)
			slab, objsize := 0, 0
			if a, o, err := perf.Slab("nf_conntrack"); err == nil {
				slab, objsize = a, o
			}
			mem := perf.MemInfo("Slab")
			t.Logf("SPIKE10 CT %s flows-sent=%d ct-entries=%d (+%d) slab nf_conntrack active=%d (+%d) objsize=%d; meminfo Slab +%d kB (%.0f B per new entry)",
				f.name, target, cnt-c0, cnt-prevCnt, slab, slab-prevSlab, objsize, mem-prevMem, float64(mem-prevMem)*1024/float64(max(cnt-prevCnt, 1)))
			prevCnt, prevSlab, prevMem = cnt, slab, mem
			var full, filt, parsed []float64
			var nFull, nFilt, nParsed int
			af := uint8(unix.AF_INET)
			if f.v6 {
				af = unix.AF_INET6
			}
			for range perf.Runs {
				require.NoError(t, rtr.in(func() error {
					var d time.Duration
					var err error
					if nFull, d, err = perf.CTDump(af, 0, 0); err != nil {
						return err
					}
					full = append(full, float64(d.Microseconds())/1000)
					if nFilt, d, err = perf.CTDump(af, slot, 0xff); err != nil {
						return err
					}
					filt = append(filt, float64(d.Microseconds())/1000)
					s := time.Now()
					fl, err := ctnl.ConntrackTableList(netlink.ConntrackTable, netlink.InetFamily(af))
					parsed = append(parsed, float64(time.Since(s).Microseconds())/1000)
					nParsed = len(fl)
					return err
				}))
			}
			t.Logf("SPIKE10 CT %s entries=%d dump-all count=%d ms %s", f.name, cnt-c0, nFull, perf.MinMed(full))
			t.Logf("SPIKE10 CT %s entries=%d dump-mark-filter(ct mark 3/0xff) count=%d ms %s", f.name, cnt-c0, nFilt, perf.MinMed(filt))
			t.Logf("SPIKE10 CT %s entries=%d dump-all-parsed(vishvananda ConntrackTableList) count=%d ms %s", f.name, cnt-c0, nParsed, perf.MinMed(parsed))
		}
		doneE()
		doneB()
		s := time.Now()
		require.NoError(t, ctnl.ConntrackTableFlush(netlink.ConntrackTable))
		t.Logf("SPIKE10 CT %s flush of %d entries: %.1f ms", f.name, have, float64(time.Since(s).Microseconds())/1000)
		time.Sleep(500 * time.Millisecond)
	}
}

// ---- routing rules ---------------------------------------------------------------

// perfRules adds the design's per-slot rules ("pref 90 fwmark N/0xff lookup
// <target table>") to a router, 0, 1, 16, 64 and 127 of them per family,
// and measures (a) bpf_fib_lookup of a packet that is not egress (mark 0:
// it walks every slot rule, then main) with the guard's test-run method,
// and (b) forwarded 64-byte UDP packets per second and CPU per packet.
func perfRules(t *testing.T) {
	cli, rtr, srv := pnTopo(t, "plain")
	for _, tb := range []int{7700, 7701, 7702} {
		for _, gw := range []string{"172.30.10.1", "fdaa::1"} {
			dst := "0.0.0.0/0"
			if strings.Contains(gw, ":") {
				dst = "::/0"
			}
			_, d, _ := net.ParseCIDR(dst)
			route(t, rtr, &netlink.Route{Dst: d, Gw: net.ParseIP(gw), Table: tb})
		}
	}
	// A resolved next hop, so every lookup returns success.
	sl, err := srv.nl.LinkByName("eth0")
	require.NoError(t, err)
	for _, ip := range []string{"172.30.10.1", "fdaa::1"} {
		require.NoError(t, rtr.nl.NeighSet(&netlink.Neigh{LinkIndex: ifindex(t, rtr, "up0"), State: netlink.NUD_PERMANENT,
			IP: net.ParseIP(ip), HardwareAddr: sl.Attrs().HardwareAddr}))
	}
	in := ifindex(t, rtr, "vpod")
	have := 0
	for _, n := range []int{0, 1, 16, 64, 127} {
		for i := have + 1; i <= n; i++ {
			for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
				r := netlink.NewRule()
				r.Family, r.Priority, r.Table = fam, 90, 7700+i%3
				r.Mark, r.Mask = uint32(i), ptr(uint32(0xff))
				require.NoError(t, rtr.nl.RuleAdd(r))
			}
		}
		have = n
		for _, f := range perfFams {
			src, dst := netip.MustParseAddr(f.pod).AsSlice(), netip.MustParseAddr(f.server).AsSlice()
			p, err := perf.FibProg(in, src, dst)
			require.NoError(t, err)
			pkt := perf.Eth(make([]byte, 40))
			pkt[14] = 0x45
			var vals []float64
			var ret uint32
			require.NoError(t, rtr.in(func() error {
				var err error
				vals, ret, err = perf.Repeat(p, pkt, nil, 1_000_000, perf.Runs)
				return err
			}))
			p.Close()
			t.Logf("SPIKE10 RULES %s rules=%d bpf_fib_lookup(mark 0) ret=%d | ns/lookup %s", f.name, n, ret, perf.MinMed(vals))
		}
		if n == 1 {
			continue
		}
		for _, f := range perfFams {
			m, ns := ppsRuns(t, cli, srv, f.server, 9300+n)
			logPPS(t, "rules", fmt.Sprintf("%d slot rules, forwarded, %s", n, f.name), m, ns)
		}
	}
}

// ---- install times (restart convergence inputs) ------------------------------------

func perfInstall(t *testing.T) {
	n := newNode(t, "install")
	s := time.Now()
	for i := 1; i <= 127; i++ {
		for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			r := netlink.NewRule()
			r.Family, r.Priority, r.Table = fam, 90, 7700+i%3
			r.Mark, r.Mask = uint32(i), ptr(uint32(0xff))
			require.NoError(t, n.nl.RuleAdd(r))
		}
	}
	t.Logf("SPIKE10 INSTALL 254 fwmark rules (127 x 2 families), one netlink request each: %.1f ms", float64(time.Since(s).Microseconds())/1000)
	o := newCore(t)
	for _, count := range []int{10, 1000, 10000, 65536} {
		var ks []coreSliceKey
		var vs []uint32
		for i := 0; i < count; i++ {
			ks = append(ks, coreSliceKey{Addr: ip16(fmt.Sprintf("10.%d.%d.1", 100+i/4096, (i/16)%256)), Family: 2, Index: uint16(i % 16)})
			vs = append(vs, 1)
		}
		s = time.Now()
		for i := range ks {
			require.NoError(t, o.Slices.Put(ks[i], vs[i]))
		}
		put := time.Since(s)
		for i := range ks {
			require.NoError(t, o.Slices.Delete(ks[i]))
		}
		s = time.Now()
		_, err := o.Slices.BatchUpdate(ks, vs, nil)
		require.NoError(t, err)
		t.Logf("SPIKE10 INSTALL slices map, %d entries: one update each %.2f ms, BatchUpdate %.2f ms",
			count, float64(put.Microseconds())/1000, float64(time.Since(s).Microseconds())/1000)
		for i := range ks {
			require.NoError(t, o.Slices.Delete(ks[i]))
		}
	}
}

// TestPerfNftOneTransaction installs the spike-7 egress table (nftEgress:
// one nft transaction, google/nftables v0.3.0 with its default socket
// buffers) with 16, 32, 64 and 127 slots for one family, and 127 slots for
// both, each in a fresh namespace, and logs the time of each that loads.
// It is EXPECTED TO FAIL: in the 2026-10-20 trials 127 slots for one family
// failed with "netlink receive: recvmsg: no buffer space available" and 127
// for both families with "netlink send-messages: sendmsg: message too long".
// vm-perf.sh runs it on its own line and requires one of those failures.
func TestPerfNftOneTransaction(t *testing.T) {
	perf.Require(t)
	for _, c := range []struct {
		n    uint32
		fams []string
	}{{16, []string{"ipv4"}}, {32, []string{"ipv4"}}, {64, []string{"ipv4"}}, {127, []string{"ipv4"}}, {127, []string{"ipv6"}}, {127, []string{"ipv4", "ipv6"}}} {
		name := fmt.Sprintf("%d-slots-%s", c.n, strings.Join(c.fams, "+"))
		t.Run(name, func(t *testing.T) {
			n := newNode(t, "install")
			var slots []snatSlot
			for s := uint32(1); s <= c.n; s++ {
				for _, f := range c.fams {
					a := fmt.Sprintf("172.30.10.%d", s)
					if f == "ipv6" {
						a = fmt.Sprintf("fdaa::%x", s)
					}
					slots = append(slots, snatSlot{slot: s, addr: a, portLo: sliceLo, portHi: sliceHi})
				}
			}
			s := time.Now()
			nftEgress(t, n, slots)
			t.Logf("SPIKE10 INSTALL nft spike-7 table %s (%d SNAT rules), one transaction: %.1f ms", name, len(slots), float64(time.Since(s).Microseconds())/1000)
		})
	}
}
