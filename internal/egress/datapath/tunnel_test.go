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
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"purelb.io/internal/bpfpin"
)

// The tests here build hosts out of network namespaces. They need
// CAP_SYS_ADMIN, CAP_NET_ADMIN, CAP_NET_RAW, CAP_BPF and the geneve module;
// PURELB_NETNS_TESTS=required turns a skip into a failure.
func netnsOrSkip(t *testing.T) {
	t.Helper()
	runtime.LockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	h, err := netns.New()
	if err == nil {
		h.Close()
	}
	_ = netns.Set(orig)
	orig.Close()
	runtime.UnlockOSThread()
	if err != nil {
		if os.Getenv("PURELB_NETNS_TESTS") == "required" {
			t.Fatalf("netns tests required but namespace creation failed: %v", err)
		}
		t.Skipf("cannot create a network namespace: %v", err)
	}
}

// host is one network namespace standing in for a node.
type host struct {
	name string
	ns   netns.NsHandle
	nl   *netlink.Handle
}

func newHost(t *testing.T, name string) *host {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer orig.Close()
	ns, err := netns.New()
	require.NoError(t, err)
	require.NoError(t, netns.Set(orig))
	nl, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	require.NoError(t, err)
	h := &host{name: name, ns: ns, nl: nl}
	t.Cleanup(func() { nl.Close(); ns.Close() })
	lo, err := nl.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, nl.LinkSetUp(lo))
	return h
}

// in runs fn on a thread switched into the host's namespace.
func (h *host) in(fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // discarded with the goroutine, never unlocked
		if err := netns.Set(h.ns); err != nil {
			errc <- err
			return
		}
		errc <- fn()
	}()
	return <-errc
}

func (h *host) sysctl(t *testing.T, key, val string) {
	t.Helper()
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	require.NoError(t, h.in(func() error { return os.WriteFile(path, []byte(val), 0o644) }), "%s: %s", h.name, key)
}

func (h *host) link(t *testing.T, name string) netlink.Link {
	t.Helper()
	l, err := h.nl.LinkByName(name)
	require.NoError(t, err, "%s: %s", h.name, name)
	return l
}

func (h *host) addr(t *testing.T, dev, cidr string) {
	t.Helper()
	a, err := netlink.ParseAddr(cidr)
	require.NoError(t, err)
	a.Flags |= unix.IFA_F_NODAD
	require.NoError(t, h.nl.AddrAdd(h.link(t, dev), a), "%s: %s %s", h.name, dev, cidr)
}

func (h *host) route(t *testing.T, r *netlink.Route) {
	t.Helper()
	require.NoError(t, h.nl.RouteAdd(r), "%s: route %v", h.name, r)
}

func cidr(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func wire(t *testing.T, a *host, an string, b *host, bn string) {
	t.Helper()
	require.NoError(t, a.nl.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: an}, PeerName: bn,
		PeerNamespace: netlink.NsFd(int(b.ns))}))
	require.NoError(t, a.nl.LinkSetUp(a.link(t, an)))
	require.NoError(t, b.nl.LinkSetUp(b.link(t, bn)))
}

// attach puts prog at hook of dev in h, until the test ends.
func (h *host) attach(t *testing.T, dev string, prog *ebpf.Program, hook ebpf.AttachType) {
	t.Helper()
	idx := h.link(t, dev).Attrs().Index
	var l link.Link
	require.NoError(t, h.in(func() error {
		var err error
		l, err = AttachTCX("", "", idx, prog, hook, nil)
		return err
	}), "%s: attach to %s", h.name, dev)
	t.Cleanup(func() { _ = l.Close() })
}

// markProg stands in for the pod-node stage (Phase 5): it puts slot in the
// mark of every packet.
func markProg(t *testing.T, slot uint32) *ebpf.Program {
	t.Helper()
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{Type: ebpf.SchedCLS, License: "Apache-2.0",
		Instructions: asm.Instructions{
			asm.StoreImm(asm.R1, 8, int64(slot), asm.Word), // __sk_buff->mark
			asm.Mov.Imm(asm.R0, -1),                        // TCX_NEXT
			asm.Return(),
		}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

type tunCase struct{ inner6, under6, l2 bool }

func (c tunCase) String() string {
	pick := func(b bool, y, n string) string {
		if b {
			return y
		}
		return n
	}
	return fmt.Sprintf("inner%s/underlay%s/%s", pick(c.inner6, "6", "4"), pick(c.under6, "6", "4"), pick(c.l2, "L2", "L3"))
}

// TestNetnsTunnelEndToEnd drives the decided path with real traffic (D3):
//
//	pod --veth-- A (pod node) ==underlay== B (holder) --pool-- server
//
// A's traffic in slot 3 goes into purelb-eg0, where pl_steer keys it to B;
// B's pl_announce_tun forwards the transit out the pool interface by its
// table; the server's reply reaches B's pl_announce_eth, which tunnels it
// back to A, whose pl_announce_tun admits it for the pod. The pod sources
// from the egress address and a port in A's slice itself: the SNAT that
// does that for a real pod is Phase 5's.
func TestNetnsTunnelEndToEnd(t *testing.T) {
	netnsOrSkip(t)
	for _, inner6 := range []bool{false, true} {
		for _, under6 := range []bool{false, true} {
			for _, l2 := range []bool{false, true} {
				c := tunCase{inner6, under6, l2}
				t.Run(c.String(), func(t *testing.T) { tunnelCase(t, c) })
			}
		}
	}
}

func tunnelCase(t *testing.T, c tunCase) {
	const slot = 3
	nodeA, nodeB := NodeID("node-a"), NodeID("node-b")
	pod, a, b, srv := newHost(t, "pod"), newHost(t, "nodeA"), newHost(t, "nodeB"), newHost(t, "server")
	wire(t, pod, "eth0", a, "vpod")
	wire(t, a, "ul", b, "ul")
	wire(t, b, "pool", srv, "eth0")

	egress4, egress6 := "172.30.10.110", "fdaa::110"
	server, egress := "172.30.10.1", egress4
	if c.inner6 {
		server, egress = "fdaa::1", egress6
	}
	pod.addr(t, "eth0", "10.244.1.5/24")
	pod.addr(t, "eth0", "fd44:1::5/64")
	pod.addr(t, "eth0", egress4+"/32")
	pod.addr(t, "eth0", egress6+"/128")
	pod.route(t, &netlink.Route{Gw: net.ParseIP("10.244.1.1")})
	pod.route(t, &netlink.Route{Gw: net.ParseIP("fd44:1::1")})
	a.addr(t, "vpod", "10.244.1.1/24")
	a.addr(t, "vpod", "fd44:1::1/64")
	vpod := a.link(t, "vpod").Attrs().Index
	a.route(t, &netlink.Route{Dst: cidr(egress4 + "/32"), LinkIndex: vpod})
	a.route(t, &netlink.Route{Dst: cidr(egress6 + "/128"), LinkIndex: vpod})
	a.addr(t, "ul", "10.0.0.1/24")
	a.addr(t, "ul", "fd00::1/64")
	b.addr(t, "ul", "10.0.0.2/24")
	b.addr(t, "ul", "fd00::2/64")
	b.addr(t, "pool", "172.30.10.2/24")
	b.addr(t, "pool", "fdaa::2/64")
	b.addr(t, "pool", egress4+"/32") // B holds the egress address
	b.addr(t, "pool", egress6+"/128")
	srv.addr(t, "eth0", "172.30.10.1/24")
	srv.addr(t, "eth0", "fdaa::1/64")
	for _, h := range []*host{a, b} {
		h.sysctl(t, "net.ipv4.ip_forward", "1")
		h.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
		h.sysctl(t, "net.ipv4.conf.all.rp_filter", "0") // Phase 5 marks for strict rp_filter
	}

	// purelb-eg0 on both.
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
		require.NoError(t, err, "%s: %s", x.h.name, c)
		require.Equal(t, mode, got)
		*x.l = l
		x.h.sysctl(t, "net.ipv4.conf.purelb-eg0.rp_filter", "0")
	}

	// A: the slot rule into the tunnel table, its default to purelb-eg0
	// and the underlay throws.
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		r := netlink.NewRule()
		r.Family, r.Priority, r.Table, r.Mark = fam, 80, TunnelTable, slot
		r.Mask = func(v uint32) *uint32 { return &v }(0xff)
		require.NoError(t, a.nl.RuleAdd(r))
		dst := "0.0.0.0/0"
		if fam == netlink.FAMILY_V6 {
			dst = "::/0"
		}
		a.route(t, &netlink.Route{Dst: cidr(dst), LinkIndex: tunA.Attrs().Index, Table: TunnelTable})
	}
	require.NoError(t, SyncThrows(a.nl, []netip.Addr{netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("fd00::2")}))
	// B: the pool interface's table, as the route watcher mirrors it.
	b.route(t, &netlink.Route{Dst: cidr("172.30.10.0/24"), LinkIndex: b.link(t, "pool").Attrs().Index, Table: 7900})
	b.route(t, &netlink.Route{Dst: cidr("fdaa::/64"), LinkIndex: b.link(t, "pool").Attrs().Index, Table: 7900})

	objA, objB := load(t), load(t)
	underA, underB := Node{V4: netip.MustParseAddr("10.0.0.1")}, Node{V4: netip.MustParseAddr("10.0.0.2")}
	if c.under6 {
		underA, underB = Node{V6: netip.MustParseAddr("fd00::1")}, Node{V6: netip.MustParseAddr("fd00::2")}
	}
	underA.MAC, underB.MAC = TunnelMAC(nodeA), TunnelMAC(nodeB)
	require.NoError(t, objA.SetConfig(Config{Self: nodeA, TunIfindex: tunA.Attrs().Index, L2: c.l2,
		TunMAC: TunnelMAC(nodeA), Underlay4: !c.under6, Underlay6: c.under6}))
	require.NoError(t, objB.SetConfig(Config{Self: nodeB, TunIfindex: tunB.Attrs().Index, L2: c.l2,
		TunMAC: TunnelMAC(nodeB), Underlay4: !c.under6, Underlay6: c.under6}))
	require.NoError(t, objA.SetNode(nodeB, underB))
	require.NoError(t, objB.SetNode(nodeA, underA))
	require.NoError(t, objA.SetSteer(slot, []uint64{nodeB}))
	require.NoError(t, objB.SetPoolInterface(b.link(t, "pool").Attrs().Index, 0))
	for _, e := range []string{egress4, egress6} {
		ea := netip.MustParseAddr(e)
		require.NoError(t, objA.SetAddress(ea, Address{Held: false, SliceSize: 4096, Base: 1024}))
		require.NoError(t, objB.SetAddress(ea, Address{Held: true, SliceSize: 4096, Base: 1024, Table: 7900}))
		require.NoError(t, objA.SetSlice(ea, sliceIndex, nodeA))
		require.NoError(t, objB.SetSlice(ea, sliceIndex, nodeA))
	}
	a.attach(t, "vpod", markProg(t, slot), ebpf.AttachTCXIngress)
	a.attach(t, TunnelName, objA.PlSteer, ebpf.AttachTCXEgress)
	a.attach(t, TunnelName, objA.PlAnnounceTun, ebpf.AttachTCXIngress)
	b.attach(t, "pool", objB.PlAnnounceEth, ebpf.AttachTCXIngress)
	b.attach(t, TunnelName, objB.PlAnnounceTun, ebpf.AttachTCXIngress)

	// The server reports the source it sees.
	var ln net.Listener
	require.NoError(t, srv.in(func() error {
		var err error
		ln, err = net.Listen("tcp", net.JoinHostPort(server, "8080"))
		return err
	}))
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprintf(conn, "%s\n", conn.RemoteAddr())
			conn.Close()
		}
	}()

	var line string
	err := pod.in(func() error {
		d := net.Dialer{Timeout: 5 * time.Second,
			LocalAddr: &net.TCPAddr{IP: net.ParseIP(egress), Port: slicePort}}
		conn, err := d.Dial("tcp", net.JoinHostPort(server, "8080"))
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		line, err = bufio.NewReader(conn).ReadString('\n')
		return err
	})
	ca, _ := objA.Counts()
	cb, _ := objB.Counts()
	require.NoError(t, err, "%s: through the tunnel\n  A: %v\n  B: %v", c, nonzero(ca), nonzero(cb))
	seen, err := netip.ParseAddrPort(strings.TrimSpace(line))
	require.NoError(t, err)
	assert.Equal(t, egress, seen.Addr().String(), "%s: the server sees the egress address", c)
	assert.Equal(t, uint16(slicePort), seen.Port(), "%s: and the port in A's slice", c)
	fam := 0
	if c.inner6 {
		fam = 1
	}
	assert.NotZero(t, ca["steered"][fam], "A steered")
	assert.NotZero(t, cb["transit"][fam], "B forwarded the transit")
	assert.NotZero(t, cb["to_tunnel"][fam], "B tunnelled the replies")
	assert.NotZero(t, ca["reply_local"][fam], "A admitted the replies")

	// A probes B (design 6.8): a Geneve datagram from A's underlay address
	// to B's tunnel port; B reflects it, A records the reply.
	selfU, peerU := underA.V4, underB.V4
	if c.under6 {
		selfU, peerU = underA.V6, underB.V6
	}
	pkt := ProbePacket(mode, selfU, peerU, TunnelMAC(nodeA), TunnelMAC(nodeB))
	require.NoError(t, a.in(func() error {
		conn, err := net.DialUDP("udp", &net.UDPAddr{IP: selfU.AsSlice()}, &net.UDPAddr{IP: peerU.AsSlice(), Port: 6082})
		if err != nil {
			return err
		}
		defer conn.Close()
		_, err = conn.Write(pkt)
		return err
	}))
	ok := assert.Eventually(t, func() bool {
		seen, err := objA.ProbeReplies()
		return err == nil && seen[peerU] != 0
	}, 5*time.Second, 50*time.Millisecond, "%s: B answered A's probe", c)
	ca, _ = objA.Counts()
	cb, _ = objB.Counts()
	require.True(t, ok, "%s: A: %v\n  B: %v", c, nonzero(ca), nonzero(cb))
	ca, _ = objA.Counts()
	cb, _ = objB.Counts()
	pfam := 0
	if c.under6 {
		pfam = 1
	}
	assert.NotZero(t, cb["probe_reflected"][pfam], "B reflected the probe")
	assert.NotZero(t, ca["probe_reply"][pfam], "A recorded the reply")
}

func nonzero(c map[string][2]uint64) map[string][2]uint64 {
	out := map[string][2]uint64{}
	for k, v := range c {
		if v[0]+v[1] > 0 {
			out[k] = v
		}
	}
	return out
}

// An uplink without a link-layer header (pl_announce_l3): a reply written
// into a tun device goes through the tunnel to its owner.
func TestNetnsUplinkL3(t *testing.T) {
	netnsOrSkip(t)
	nodeA, nodeB := NodeID("node-a"), NodeID("node-b")
	a, b := newHost(t, "nodeA"), newHost(t, "nodeB")
	wire(t, a, "ul", b, "ul")
	a.addr(t, "ul", "10.0.0.1/24")
	b.addr(t, "ul", "10.0.0.2/24")
	_, tunA, err := EnsureTunnel(a.nl, ModeL3, 6082, nil)
	require.NoError(t, err)
	_, tunB, err := EnsureTunnel(b.nl, ModeL3, 6082, nil)
	require.NoError(t, err)

	var fd int
	require.NoError(t, b.in(func() error {
		var err error
		fd, err = unix.Open("/dev/net/tun", unix.O_RDWR, 0)
		if err != nil {
			return err
		}
		ifr, err := unix.NewIfreq("tun0")
		if err != nil {
			return err
		}
		ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
		return unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr)
	}))
	defer unix.Close(fd)
	require.NoError(t, b.nl.LinkSetUp(b.link(t, "tun0")))

	objA, objB := load(t), load(t)
	egress := netip.MustParseAddr("172.30.10.110")
	require.NoError(t, objA.SetConfig(Config{Self: nodeA, TunIfindex: tunA.Attrs().Index, Underlay4: true}))
	require.NoError(t, objB.SetConfig(Config{Self: nodeB, TunIfindex: tunB.Attrs().Index, Underlay4: true}))
	require.NoError(t, objA.SetNode(nodeB, Node{V4: netip.MustParseAddr("10.0.0.2")}))
	require.NoError(t, objB.SetNode(nodeA, Node{V4: netip.MustParseAddr("10.0.0.1")}))
	require.NoError(t, objA.SetAddress(egress, Address{SliceSize: 4096, Base: 1024}))
	require.NoError(t, objB.SetAddress(egress, Address{Held: true, SliceSize: 4096, Base: 1024}))
	require.NoError(t, objA.SetSlice(egress, sliceIndex, nodeA))
	require.NoError(t, objB.SetSlice(egress, sliceIndex, nodeA))
	b.attach(t, "tun0", objB.PlAnnounceL3, ebpf.AttachTCXIngress)
	a.attach(t, TunnelName, objA.PlAnnounceTun, ebpf.AttachTCXIngress)

	pkt := ipv4(netip.MustParseAddr("198.51.100.7"), egress, protoUDP, v4opts{}, ports(443, slicePort))
	require.Eventually(t, func() bool {
		_, _ = unix.Write(fd, pkt)
		c, _ := objA.Counts()
		return c["reply_local"][0] > 0
	}, 5*time.Second, 100*time.Millisecond, "the reply reaches A")
	c, _ := objB.Counts()
	assert.NotZero(t, c["to_tunnel"][0])
}

// EnsureTunnel creates purelb-eg0 in the mode asked for, keeps a matching
// device (so its programs stay attached), and replaces one that doesn't
// match.
func TestNetnsEnsureTunnel(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	mac := TunnelMAC(NodeID("node"))

	mode, l, err := EnsureTunnel(h.nl, ModeAuto, 6082, mac)
	require.NoError(t, err)
	assert.Equal(t, ModeL3, mode, "every supported kernel has innerprotoinherit")
	idx := l.Attrs().Index
	assert.NotZero(t, l.Attrs().Flags&net.FlagUp)

	_, l, err = EnsureTunnel(h.nl, ModeL3, 6082, mac)
	require.NoError(t, err)
	assert.Equal(t, idx, l.Attrs().Index, "a matching device is kept")

	mode, l, err = EnsureTunnel(h.nl, ModeL2, 6082, mac)
	require.NoError(t, err)
	assert.Equal(t, ModeL2, mode)
	assert.NotEqual(t, idx, l.Attrs().Index, "replaced")
	l = h.link(t, TunnelName)
	assert.Equal(t, mac.String(), l.Attrs().HardwareAddr.String())
	assert.NotZero(t, l.Attrs().RawFlags&unix.IFF_NOARP, "L2 is NOARP")

	_, l, err = EnsureTunnel(h.nl, ModeL2, 7000, mac)
	require.NoError(t, err)
	assert.Equal(t, uint16(7000), h.link(t, TunnelName).(*netlink.Geneve).Dport, "a port change replaces it")

	_, _, err = EnsureTunnel(h.nl, "L4", 6082, mac)
	assert.Error(t, err)
}

// SyncThrows keeps exactly one throw per underlay address in each target
// table, both families.
func TestNetnsSyncThrows(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	throws := func(table int) []string {
		var out []string
		for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			rs, err := h.nl.RouteListFiltered(fam, &netlink.Route{Table: table, Type: unix.RTN_THROW},
				netlink.RT_FILTER_TABLE|netlink.RT_FILTER_TYPE)
			require.NoError(t, err)
			for _, r := range rs {
				out = append(out, r.Dst.String())
			}
		}
		return out
	}
	addrs := func(s ...string) []netip.Addr {
		var out []netip.Addr
		for _, a := range s {
			out = append(out, netip.MustParseAddr(a))
		}
		return out
	}
	require.NoError(t, SyncThrows(h.nl, addrs("10.0.0.2", "10.0.0.3", "fd00::2")))
	for _, table := range []int{TunnelTable, ProhibitTable} {
		assert.ElementsMatch(t, []string{"10.0.0.2/32", "10.0.0.3/32", "fd00::2/128"}, throws(table))
	}
	require.NoError(t, SyncThrows(h.nl, addrs("10.0.0.3", "fd00::4")))
	assert.ElementsMatch(t, []string{"10.0.0.3/32", "fd00::4/128"}, throws(TunnelTable))
	require.NoError(t, SyncThrows(h.nl, nil))
	assert.Empty(t, throws(ProhibitTable))
}

// TunnelMTU is the smallest route MTU to an IPv6 peer less the tunnel's
// overhead.
func TestNetnsTunnelMTU(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	require.NoError(t, h.nl.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "ul", MTU: 9000}}))
	require.NoError(t, h.nl.LinkSetUp(h.link(t, "ul")))
	h.addr(t, "ul", "fd00::1/64")
	h.route(t, &netlink.Route{Dst: cidr("fd00:9::/64"), LinkIndex: h.link(t, "ul").Attrs().Index, MTU: 1500})
	peers := []netip.Addr{netip.MustParseAddr("fd00::2"), netip.MustParseAddr("fd00:9::2")}
	assert.Equal(t, 1500-56, TunnelMTU(h.nl, ModeL3, peers), "the 1500 route wins over the 9000 link")
	assert.Equal(t, 1500-70, TunnelMTU(h.nl, ModeL2, peers))
	assert.Equal(t, 9000-56, TunnelMTU(h.nl, ModeL3, peers[:1]))
	assert.Zero(t, TunnelMTU(h.nl, ModeL3, nil))
}

// AttachTCX anchors behind a given program and pins, and the next attach
// takes the pinned link over in place.
func TestNetnsAttachTCX(t *testing.T) {
	netnsOrSkip(t)
	if !bpfpin.Usable("/sys/fs/bpf") {
		skipOrFailNetns(t, "no bpffs at /sys/fs/bpf")
	}
	dir, err := os.MkdirTemp("/sys/fs/bpf", "datapath-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h := newHost(t, "node")
	require.NoError(t, h.nl.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "ul"}}))
	idx := h.link(t, "ul").Attrs().Index
	// The guard has an Ethernet and an L3 program; one is attached.
	guard, guardL3, other := markProg(t, 0), markProg(t, 2), markProg(t, 1)
	o := load(t)
	ids := func() []ebpf.ProgramID {
		var res *link.QueryResult
		require.NoError(t, h.in(func() error {
			var err error
			res, err = link.QueryPrograms(link.QueryOptions{Target: idx, Attach: ebpf.AttachTCXIngress})
			return err
		}))
		var out []ebpf.ProgramID
		for _, p := range res.Programs {
			out = append(out, p.ID)
		}
		return out
	}
	id := func(p *ebpf.Program) ebpf.ProgramID {
		info, err := p.Info()
		require.NoError(t, err)
		i, _ := info.ID()
		return i
	}
	var gl, ol, pl link.Link
	require.NoError(t, h.in(func() error {
		var err error
		if gl, err = link.AttachTCX(link.TCXOptions{Program: guard, Interface: idx, Attach: ebpf.AttachTCXIngress}); err != nil {
			return err
		}
		if ol, err = link.AttachTCX(link.TCXOptions{Program: other, Interface: idx, Attach: ebpf.AttachTCXIngress}); err != nil {
			return err
		}
		pl, err = AttachTCX(dir, "pl_announce-ul", idx, o.PlAnnounceEth, ebpf.AttachTCXIngress,
			[]ebpf.ProgramID{id(guardL3), id(guard)})
		return err
	}))
	defer gl.Close()
	defer ol.Close()
	assert.Equal(t, []ebpf.ProgramID{id(guard), id(o.PlAnnounceEth), id(other)}, ids(), "right behind the guard")
	require.NoError(t, pl.Close()) // the agent exits; the pin holds it

	o2 := load(t)
	require.NoError(t, h.in(func() error {
		var err error
		pl, err = AttachTCX(dir, "pl_announce-ul", idx, o2.PlAnnounceEth, ebpf.AttachTCXIngress,
			[]ebpf.ProgramID{id(guardL3), id(guard)})
		return err
	}))
	assert.Equal(t, []ebpf.ProgramID{id(guard), id(o2.PlAnnounceEth), id(other)}, ids(), "taken over in place")
	require.NoError(t, pl.Unpin())
	require.NoError(t, pl.Detach())
	require.NoError(t, pl.Close())
	_, err = os.Stat(filepath.Join(dir, "pl_announce-ul"))
	assert.True(t, os.IsNotExist(err))
}

func skipOrFailNetns(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("PURELB_NETNS_TESTS") == "required" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}
