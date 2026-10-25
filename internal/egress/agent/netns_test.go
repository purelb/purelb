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

package agent

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/go-kit/log"
	"github.com/google/nftables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// The netns tests run the agent's datapath apply in a network namespace
// with a pin directory in bpffs. They need CAP_SYS_ADMIN (for the
// namespace; the apply itself runs without it, as the agent does),
// CAP_NET_ADMIN, CAP_BPF and the geneve module;
// PURELB_NETNS_TESTS=required turns a skip into a failure.
func skipOrFailNetns(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PURELB_NETNS_TESTS") == "required" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// sandbox locks the test to a thread in a new network namespace with an
// underlay interface (eth0: 10.0.0.1/24, fd00::1/64, MTU 1500), and
// returns a pin directory in bpffs.
func sandbox(t *testing.T) string {
	t.Helper()
	runtime.LockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	ns, err := netns.New()
	if err != nil {
		_ = orig.Close()
		runtime.UnlockOSThread()
		skipOrFailNetns(t, "creating a network namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = netns.Set(orig)
		_ = ns.Close()
		_ = orig.Close()
		runtime.UnlockOSThread()
	})
	if !bpfpin.Usable("/sys/fs/bpf") {
		skipOrFailNetns(t, "no bpffs at /sys/fs/bpf")
	}
	dir, err := os.MkdirTemp("/sys/fs/bpf", "egress-agent-test-")
	if err != nil {
		skipOrFailNetns(t, "creating a pin directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	lo, err := netlink.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(lo))
	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", MTU: 1500}}))
	eth0, err := netlink.LinkByName("eth0")
	require.NoError(t, err)
	for _, a := range []string{"10.0.0.1/24", "fd00::1/64"} {
		addr, err := netlink.ParseAddr(a)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, netlink.AddrAdd(eth0, addr))
	}
	require.NoError(t, netlink.LinkSetUp(eth0))
	return dir
}

// asAgent runs fn on this thread without CAP_SYS_ADMIN, the agent's
// capabilities (CAP_SYS_ADMIN would also count as CAP_PERFMON to the
// verifier). Capabilities are per thread, and the test is locked to its
// thread by sandbox.
func asAgent(t *testing.T, fn func()) {
	t.Helper()
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	require.NoError(t, unix.Capget(&hdr, &data[0]))
	saved := data
	data[0].Effective &^= 1 << unix.CAP_SYS_ADMIN
	require.NoError(t, unix.Capset(&hdr, &data[0]))
	defer func() { require.NoError(t, unix.Capset(&hdr, &saved[0])) }()
	fn()
}

func newNetnsAgent(dir string) *Agent {
	a := New(log.NewNopLogger(), "node-a", "purelb-system")
	a.dp.pinRoot = dir
	return a
}

func want(nodes ...dpNode) dpWant {
	return dpWant{self: "node-a", nodes: append([]dpNode{node("node-a", "10.0.0.1", "fd00::1")}, nodes...)}
}

func node(name, v4, v6 string) dpNode {
	n := dpNode{name: name}
	n.v4, _ = netip.ParseAddr(v4)
	n.v6, _ = netip.ParseAddr(v6)
	return n
}

func apply(t *testing.T, a *Agent, w dpWant) *purelbv2.EgressCapability {
	t.Helper()
	var c *purelbv2.EgressCapability
	var err error
	asAgent(t, func() { c, err = a.applyDatapath(w) })
	require.NoError(t, err)
	require.True(t, c.EgressCapable)
	return c
}

// close stands in for the agent exiting: its descriptors go, its pins
// stay.
func closeAgent(a *Agent) {
	for _, l := range a.dp.links {
		_ = l.Close()
	}
	_ = a.dp.objs.Close()
	a.dp.nl.Close()
}

func progID(t *testing.T, p *ebpf.Program) ebpf.ProgramID {
	t.Helper()
	info, err := p.Info()
	require.NoError(t, err)
	id, ok := info.ID()
	require.True(t, ok)
	return id
}

func chain(t *testing.T, ifindex int, hook ebpf.AttachType) []ebpf.ProgramID {
	t.Helper()
	res, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: hook})
	require.NoError(t, err)
	var ids []ebpf.ProgramID
	for _, p := range res.Programs {
		ids = append(ids, p.ID)
	}
	return ids
}

// pinnedLink is the ID and interface of the link pinned at path.
func pinnedLink(t *testing.T, path string) (link.ID, int) {
	t.Helper()
	l, err := link.LoadPinnedLink(path, nil)
	require.NoError(t, err)
	defer l.Close()
	info, err := l.Info()
	require.NoError(t, err)
	return info.ID, int(info.TCX().Ifindex)
}

func nodeIDs(t *testing.T, a *Agent) []uint64 {
	t.Helper()
	var ids []uint64
	var k uint64
	var v []byte
	it := a.dp.objs.Nodes.Iterate()
	for it.Next(&k, &v) {
		ids = append(ids, k)
	}
	require.NoError(t, it.Err())
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func ids(names ...string) []uint64 {
	var ids []uint64
	for _, n := range names {
		ids = append(ids, datapath.NodeID(n))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// setElems is the members of the hardening sets named.
func setElems(t *testing.T, names ...string) []string {
	t.Helper()
	c, err := nftables.New()
	require.NoError(t, err)
	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: datapath.NftTable}
	var out []string
	for _, n := range names {
		set, err := c.GetSetByName(tbl, n)
		require.NoError(t, err, n)
		elems, err := c.GetSetElements(set)
		require.NoError(t, err, n)
		for _, e := range elems {
			a, _ := netip.AddrFromSlice(e.Key)
			out = append(out, a.String())
		}
	}
	sort.Strings(out)
	return out
}

// dummy adds an up dummy interface with addrs and returns its ifindex.
func dummy(t *testing.T, name string, addrs ...string) int {
	t.Helper()
	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}))
	l, err := netlink.LinkByName(name)
	require.NoError(t, err)
	for _, a := range addrs {
		addr, err := netlink.ParseAddr(a)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, netlink.AddrAdd(l, addr))
	}
	require.NoError(t, netlink.LinkSetUp(l))
	return l.Attrs().Index
}

func tableDsts(t *testing.T, table int) []string {
	t.Helper()
	var dsts []string
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(fam, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		require.NoError(t, err)
		for _, r := range routes {
			d := "default"
			if r.Dst != nil {
				if ones, _ := r.Dst.Mask.Size(); ones != 0 {
					d = r.Dst.String()
				}
			}
			dsts = append(dsts, d)
		}
	}
	sort.Strings(dsts)
	return dsts
}

func throws(t *testing.T, table int) []string {
	t.Helper()
	var dsts []string
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(fam, &netlink.Route{Table: table, Type: unix.RTN_THROW},
			netlink.RT_FILTER_TABLE|netlink.RT_FILTER_TYPE)
		require.NoError(t, err)
		for _, r := range routes {
			dsts = append(dsts, r.Dst.String())
		}
	}
	sort.Strings(dsts)
	return dsts
}

// TestNetnsApplyDatapath runs the agent's datapath apply as the agent does:
// a first apply, a repeat that changes nothing, a node leaving, the tunnel
// device deleted under it, and an agent restart that takes over its pins
// with a node gone meanwhile.
func TestNetnsApplyDatapath(t *testing.T) {
	dir := sandbox(t)
	announce := filepath.Join(dir, "pl_announce-"+datapath.TunnelName)
	steer := filepath.Join(dir, "pl_steer-"+datapath.TunnelName)

	// First apply: the tunnel, every other node, both programs, pinned.
	a := newNetnsAgent(dir)
	c := apply(t, a, want(node("node-b", "10.0.0.2", "fd00::2"), node("node-c", "10.0.0.3", "")))
	assert.Equal(t, datapath.ModeL3, c.TunnelEncapsulation, "Auto resolves to L3 where the kernel supports it")
	assert.Equal(t, dir, a.dp.pinDir)
	tun, err := netlink.LinkByName(datapath.TunnelName)
	require.NoError(t, err)
	g, ok := tun.(*netlink.Geneve)
	require.True(t, ok)
	assert.True(t, g.FlowBased)
	assert.Equal(t, uint16(6082), g.Dport)
	assert.Equal(t, []ebpf.ProgramID{progID(t, a.dp.objs.PlAnnounceTun)}, chain(t, tun.Attrs().Index, ebpf.AttachTCXIngress))
	assert.Equal(t, []ebpf.ProgramID{progID(t, a.dp.objs.PlSteer)}, chain(t, tun.Attrs().Index, ebpf.AttachTCXEgress))
	announceID, ifindex := pinnedLink(t, announce)
	assert.Equal(t, tun.Attrs().Index, ifindex)
	steerID, _ := pinnedLink(t, steer)
	assert.FileExists(t, filepath.Join(dir, "nodes"), "state maps are pinned")
	assert.Equal(t, ids("node-b", "node-c"), nodeIDs(t, a), "every node but this one")
	allThrows := []string{"10.0.0.2/32", "10.0.0.3/32", "fd00::2/128"}
	assert.Equal(t, allThrows, throws(t, datapath.TunnelTable))
	assert.Equal(t, allThrows, throws(t, datapath.ProhibitTable))
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "fd00::1", "fd00::2"}, setElems(t, "nodes4", "nodes6"),
		"the hardening chains admit every node's underlay, this one's included")

	// The same state again changes nothing.
	apply(t, a, want(node("node-b", "10.0.0.2", "fd00::2"), node("node-c", "10.0.0.3", "")))
	tun2, err := netlink.LinkByName(datapath.TunnelName)
	require.NoError(t, err)
	assert.Equal(t, tun.Attrs().Index, tun2.Attrs().Index, "the tunnel is kept")
	id, _ := pinnedLink(t, announce)
	assert.Equal(t, announceID, id, "the link is kept")
	assert.Len(t, chain(t, tun.Attrs().Index, ebpf.AttachTCXIngress), 1)
	assert.Len(t, chain(t, tun.Attrs().Index, ebpf.AttachTCXEgress), 1)

	// A node leaves: its entry and throw routes go.
	apply(t, a, want(node("node-b", "10.0.0.2", "fd00::2")))
	assert.Equal(t, ids("node-b"), nodeIDs(t, a))
	assert.Equal(t, []string{"10.0.0.2/32", "fd00::2/128"}, throws(t, datapath.TunnelTable))
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2", "fd00::1", "fd00::2"}, setElems(t, "nodes4", "nodes6"))

	// The tunnel device deleted under the agent: a new one, programs
	// attached to it, the stale pins replaced.
	require.NoError(t, netlink.LinkDel(tun))
	apply(t, a, want(node("node-b", "10.0.0.2", "fd00::2"), node("node-c", "10.0.0.3", "")))
	tun, err = netlink.LinkByName(datapath.TunnelName)
	require.NoError(t, err)
	assert.NotEqual(t, tun2.Attrs().Index, tun.Attrs().Index)
	assert.Equal(t, []ebpf.ProgramID{progID(t, a.dp.objs.PlAnnounceTun)}, chain(t, tun.Attrs().Index, ebpf.AttachTCXIngress))
	assert.Equal(t, []ebpf.ProgramID{progID(t, a.dp.objs.PlSteer)}, chain(t, tun.Attrs().Index, ebpf.AttachTCXEgress))
	announceID, ifindex = pinnedLink(t, announce)
	assert.Equal(t, tun.Attrs().Index, ifindex)
	steerID, ifindex = pinnedLink(t, steer)
	assert.Equal(t, tun.Attrs().Index, ifindex)

	// The agent exits; node-c leaves while it is down; the next agent
	// takes over the tunnel and the pinned links in place, with its own
	// programs, and drops node-c from the pinned node map.
	closeAgent(a)
	assert.Len(t, chain(t, tun.Attrs().Index, ebpf.AttachTCXIngress), 1, "pinned links outlive the agent")
	b := newNetnsAgent(dir)
	apply(t, b, want(node("node-b", "10.0.0.2", "fd00::2")))
	tun2, err = netlink.LinkByName(datapath.TunnelName)
	require.NoError(t, err)
	assert.Equal(t, tun.Attrs().Index, tun2.Attrs().Index, "the tunnel is kept across the restart")
	id, _ = pinnedLink(t, announce)
	assert.Equal(t, announceID, id, "pl_announce's link is taken over, not replaced")
	id, _ = pinnedLink(t, steer)
	assert.Equal(t, steerID, id, "pl_steer's link is taken over, not replaced")
	assert.Equal(t, []ebpf.ProgramID{progID(t, b.dp.objs.PlAnnounceTun)}, chain(t, tun.Attrs().Index, ebpf.AttachTCXIngress))
	assert.Equal(t, []ebpf.ProgramID{progID(t, b.dp.objs.PlSteer)}, chain(t, tun.Attrs().Index, ebpf.AttachTCXEgress))
	assert.Equal(t, ids("node-b"), nodeIDs(t, b), "a node that left while the agent was down is dropped")
	assert.Equal(t, []string{"10.0.0.2/32", "fd00::2/128"}, throws(t, datapath.TunnelTable))
	closeAgent(b)
}

// TestNetnsApplyDatapathL2 forces L2: the device carries this node's
// tunnel MAC and does no ARP.
func TestNetnsApplyDatapathL2(t *testing.T) {
	dir := sandbox(t)
	a := newNetnsAgent(dir)
	w := want(node("node-b", "10.0.0.2", "fd00::2"))
	w.spec.TunnelEncapsulation = datapath.ModeL2
	c := apply(t, a, w)
	defer closeAgent(a)
	assert.Equal(t, datapath.ModeL2, c.TunnelEncapsulation)
	tun, err := netlink.LinkByName(datapath.TunnelName)
	require.NoError(t, err)
	assert.Equal(t, datapath.TunnelMAC(datapath.NodeID("node-a")).String(), tun.Attrs().HardwareAddr.String())
	assert.NotZero(t, tun.Attrs().RawFlags&unix.IFF_NOARP)
	assert.False(t, tun.(*netlink.Geneve).InnerProtoInherit)
}

// TestNetnsApplyDatapathPools numbers pool interfaces, mirrors their
// tables and reports what each lacks; a restarted agent keeps the numbers
// its predecessor gave them.
func TestNetnsApplyDatapathPools(t *testing.T) {
	dir := sandbox(t)
	pool0 := dummy(t, "pool0", "192.0.2.1/24", "2001:db8:1::1/64")
	pool1 := dummy(t, "pool1", "198.51.100.1/24")
	require.NoError(t, netlink.RouteAdd(&netlink.Route{Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Gw: net.ParseIP("192.0.2.254"), LinkIndex: pool0}))

	a := newNetnsAgent(dir)
	w := want(node("node-b", "10.0.0.2", "fd00::2"))
	w.pools = map[int]dpPool{pool0: {name: "pool0", v4: true, v6: true}}
	apply(t, a, w)
	assert.Equal(t, map[int]uint32{pool0: 0}, a.dp.pools)
	recovered, err := a.dp.objs.RecoverPoolInterfaces()
	require.NoError(t, err)
	assert.Equal(t, map[int]uint32{pool0: 0}, recovered, "pool_ifs names it")
	assert.Equal(t, []string{"192.0.2.0/24", "2001:db8:1::/64", "default", "fe80::/64"}, tableDsts(t, datapath.PoolTable(0)))
	assert.Equal(t, poolReport{noGateway6: true}, a.dp.reports[pool0], "no IPv6 default, arp_announce 0")
	assert.Equal(t, map[int]bool{pool0: true}, a.poolSet(), "the route watcher follows it")

	w.pools[pool1] = dpPool{name: "pool1", v4: true}
	apply(t, a, w)
	assert.Equal(t, map[int]uint32{pool0: 0, pool1: 1}, a.dp.pools)
	assert.Equal(t, poolReport{noGateway4: true}, a.dp.reports[pool1])

	// Restarted with pool0 gone: pool1 keeps table 7901, pool0's is emptied.
	closeAgent(a)
	b := newNetnsAgent(dir)
	defer closeAgent(b)
	delete(w.pools, pool0)
	apply(t, b, w)
	assert.Equal(t, map[int]uint32{pool1: 1}, b.dp.pools)
	recovered, err = b.dp.objs.RecoverPoolInterfaces()
	require.NoError(t, err)
	assert.Equal(t, map[int]uint32{pool1: 1}, recovered)
	assert.Empty(t, tableDsts(t, datapath.PoolTable(0)))
	assert.Equal(t, []string{"198.51.100.0/24", "fe80::/64"}, tableDsts(t, datapath.PoolTable(1)))
}

// runWatcher runs a watcher on a thread in the test's namespace until the
// test ends, and returns a function reporting whether it kicked within a
// wait. It requires the first kick, made on subscribing.
func runWatcher(t *testing.T, run func(stop <-chan struct{}, kick func())) func(time.Duration) bool {
	t.Helper()
	ns, err := netns.Get()
	require.NoError(t, err)
	kicks := make(chan struct{}, 16)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); _ = ns.Close() })
	go func() {
		runtime.LockOSThread() // discarded with the goroutine
		if netns.Set(ns) != nil {
			return
		}
		run(stop, func() {
			select {
			case kicks <- struct{}{}:
			default:
			}
		})
	}()
	kicked := func(wait time.Duration) bool {
		select {
		case <-kicks:
			return true
		case <-time.After(wait):
			return false
		}
	}
	require.True(t, kicked(2*time.Second), "once on subscribing")
	return kicked
}

// settle drains the kicks one change made.
func settle(kicked func(time.Duration) bool) {
	for kicked(100 * time.Millisecond) {
	}
}

// TestNetnsWatchRoutes wakes the datapath for main-table routes out of a
// pool interface and for routes deleted from the agent's own tables, and
// for nothing else.
func TestNetnsWatchRoutes(t *testing.T) {
	sandbox(t)
	pool0 := dummy(t, "pool0", "192.0.2.1/24", "2001:db8:1::1/64")
	eth0, err := netlink.LinkByName("eth0")
	require.NoError(t, err)
	kicked := runWatcher(t, func(stop <-chan struct{}, kick func()) {
		watchRoutes(log.NewNopLogger(), stop, func() map[int]bool { return map[int]bool{pool0: true} }, kick)
	})

	for _, r := range []*netlink.Route{
		{Dst: cidrNet("203.0.113.0/24"), Gw: net.ParseIP("192.0.2.254"), LinkIndex: pool0},
		{Dst: cidrNet("2001:db8:9::/64"), Gw: net.ParseIP("2001:db8:1::fe"), LinkIndex: pool0},
	} {
		require.NoError(t, netlink.RouteAdd(r))
		assert.True(t, kicked(2*time.Second), "a main-table route out of a pool interface: %s", r.Dst)
		settle(kicked)
	}
	own := []*netlink.Route{
		{Dst: cidrNet("203.0.113.0/24"), Gw: net.ParseIP("192.0.2.254"), LinkIndex: pool0, Table: datapath.PoolTable(0)},
		{Dst: cidrNet("10.0.0.9/32"), Type: unix.RTN_THROW, Table: datapath.TunnelTable},
		{Dst: cidrNet("fd00::9/128"), Type: unix.RTN_THROW, Table: datapath.ProhibitTable},
	}
	for _, r := range append([]*netlink.Route{
		{Dst: cidrNet("203.0.113.0/24"), LinkIndex: eth0.Attrs().Index, Table: 100},
		{Dst: cidrNet("198.18.0.0/24"), Gw: net.ParseIP("10.0.0.254"), LinkIndex: eth0.Attrs().Index},
	}, own...) {
		require.NoError(t, netlink.RouteAdd(r))
		assert.False(t, kicked(300*time.Millisecond), "added outside main, or not a pool interface: %v", r)
	}
	for _, r := range own {
		require.NoError(t, netlink.RouteDel(r))
		assert.True(t, kicked(2*time.Second), "deleted from the agent's own table %d", r.Table)
		settle(kicked)
	}
}

// TestNetnsWatchTunnel wakes the datapath when the tunnel device goes
// down or away, and for no other device.
func TestNetnsWatchTunnel(t *testing.T) {
	sandbox(t)
	tun := dummy(t, datapath.TunnelName)
	other := dummy(t, "other0")
	kicked := runWatcher(t, func(stop <-chan struct{}, kick func()) { watchTunnel(log.NewNopLogger(), stop, kick) })

	o, err := netlink.LinkByIndex(other)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetDown(o))
	require.NoError(t, netlink.LinkDel(o))
	assert.False(t, kicked(300*time.Millisecond), "another device")
	l, err := netlink.LinkByIndex(tun)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetDown(l))
	assert.True(t, kicked(2*time.Second), "the tunnel set down")
	settle(kicked)
	require.NoError(t, netlink.LinkDel(l))
	assert.True(t, kicked(2*time.Second), "the tunnel deleted")
}

// TestNetnsWatchNft wakes the datapath when something in the hardening
// table is deleted, and not for other tables.
func TestNetnsWatchNft(t *testing.T) {
	sandbox(t)
	c, err := nftables.New()
	require.NoError(t, err)
	require.NoError(t, datapath.SyncHardening(c, datapath.Hardening{TunnelPort: 6082}))
	kicked := runWatcher(t, func(stop <-chan struct{}, kick func()) { watchNft(log.NewNopLogger(), stop, kick) })

	other := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "other"})
	c.AddChain(&nftables.Chain{Name: "c", Table: other})
	require.NoError(t, c.Flush())
	c.DelTable(other)
	require.NoError(t, c.Flush())
	assert.False(t, kicked(300*time.Millisecond), "another table")

	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: datapath.NftTable}
	ch, err := c.ListChain(tbl, "tun_in")
	require.NoError(t, err)
	rules, err := c.GetRules(tbl, ch)
	require.NoError(t, err)
	require.NoError(t, c.DelRule(rules[0]))
	require.NoError(t, c.Flush())
	assert.True(t, kicked(2*time.Second), "a rule deleted")
	settle(kicked)
	c.DelTable(tbl)
	require.NoError(t, c.Flush())
	assert.True(t, kicked(2*time.Second), "the table deleted (a flushed ruleset)")
}

// TestNetnsApplyDatapathHeals restores what something else removed: the
// hardening table, an element of it, a detached link.
func TestNetnsApplyDatapathHeals(t *testing.T) {
	dir := sandbox(t)
	a := newNetnsAgent(dir)
	defer closeAgent(a)
	w := want(node("node-b", "10.0.0.2", "fd00::2"))
	apply(t, a, w)
	nodes := setElems(t, "nodes4", "nodes6")
	c, err := nftables.New()
	require.NoError(t, err)
	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: datapath.NftTable}

	c.DelTable(tbl)
	require.NoError(t, c.Flush())
	apply(t, a, w)
	assert.Equal(t, nodes, setElems(t, "nodes4", "nodes6"), "the table is back")

	set, err := c.GetSetByName(tbl, "nodes4")
	require.NoError(t, err)
	require.NoError(t, c.SetDeleteElements(set, []nftables.SetElement{{Key: netip.MustParseAddr("10.0.0.2").AsSlice()}}))
	require.NoError(t, c.Flush())
	apply(t, a, w)
	assert.Equal(t, nodes, setElems(t, "nodes4", "nodes6"), "the element is back")

	tun, err := netlink.LinkByName(datapath.TunnelName)
	require.NoError(t, err)
	l, err := link.LoadPinnedLink(filepath.Join(dir, "pl_steer-"+datapath.TunnelName), nil)
	require.NoError(t, err)
	require.NoError(t, l.Unpin())
	require.NoError(t, l.Detach())
	_ = l.Close()
	assert.Empty(t, chain(t, tun.Attrs().Index, ebpf.AttachTCXEgress), "detached outside the agent")
	apply(t, a, w)
	assert.Equal(t, []ebpf.ProgramID{progID(t, a.dp.objs.PlSteer)}, chain(t, tun.Attrs().Index, ebpf.AttachTCXEgress),
		"attached again")
	assert.FileExists(t, filepath.Join(dir, "pl_steer-"+datapath.TunnelName), "and pinned again")
}

func cidrNet(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// TestNetnsApplyDatapathAddresses installs the address entries a holder
// builds, changes and removes them, and prunes, after a restart, the ones
// that went away while the agent was down.
func TestNetnsApplyDatapathAddresses(t *testing.T) {
	dir := sandbox(t)
	a4, a6 := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")
	entries := func(held bool, slices map[uint16]uint64) *datapath.AddressEntries {
		e := &datapath.AddressEntries{Slices: slices, Reserved: map[datapath.PortKey]uint64{}, Services: map[datapath.PortKey]bool{}}
		if held {
			e.Address = datapath.Address{Held: true, SliceSize: 4096, Base: 1024, Table: 254}
			e.Reserved[datapath.PortKey{Proto: 17, Port: 5060}] = datapath.NodeID("node-b")
			e.Services[datapath.PortKey{Proto: 6, Port: 443}] = true
		}
		return e
	}
	recovered := func(a *Agent) map[netip.Addr]*datapath.AddressEntries {
		got, err := a.dp.objs.RecoverAddresses()
		require.NoError(t, err)
		return got
	}

	a := newNetnsAgent(dir)
	w := want(node("node-b", "10.0.0.2", "fd00::2"))
	w.hold.addrs = map[netip.Addr]*datapath.AddressEntries{
		a4: entries(true, map[uint16]uint64{9: datapath.NodeID("node-b"), 10: datapath.NodeID("node-a")}),
		a6: entries(false, map[uint16]uint64{}),
	}
	apply(t, a, w)
	assert.Equal(t, w.hold.addrs, recovered(a))

	// A slice freed, another moved, the IPv6 address now held too.
	w.hold.addrs[a4] = entries(true, map[uint16]uint64{10: datapath.NodeID("node-b")})
	w.hold.addrs[a6] = entries(true, map[uint16]uint64{12: datapath.NodeID("node-a")})
	apply(t, a, w)
	assert.Equal(t, w.hold.addrs, recovered(a))

	// The agent exits; the IPv4 address goes while it is down.
	closeAgent(a)
	b := newNetnsAgent(dir)
	defer closeAgent(b)
	delete(w.hold.addrs, a4)
	apply(t, b, w)
	assert.Equal(t, w.hold.addrs, recovered(b), "an address gone while the agent was down is pruned")
}

// pl_announce is on the uplinks only while the node holds an egress
// address: without the guard the default-route interfaces and the pool
// interfaces; with it, the guard's interfaces, right behind it. Links to
// interfaces no longer wanted go with their pins, and a restart takes the
// pinned ones over in place.
func TestNetnsApplyDatapathUplinks(t *testing.T) {
	dir := sandbox(t)
	eth0, err := netlink.LinkByName("eth0")
	require.NoError(t, err)
	for _, gw := range []string{"10.0.0.254", "fd00::fe"} {
		require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth0.Attrs().Index, Gw: net.ParseIP(gw)}))
	}
	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1"}}))
	eth1, err := netlink.LinkByName("eth1")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(eth1))
	e0, e1 := eth0.Attrs().Index, eth1.Attrs().Index
	a4 := netip.MustParseAddr("192.0.2.10")
	held := &datapath.AddressEntries{Address: datapath.Address{Held: true, SliceSize: 4096, Base: 1024, Table: 254},
		Slices: map[uint16]uint64{}, Reserved: map[datapath.PortKey]uint64{}, Services: map[datapath.PortKey]bool{}}
	pinned := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}

	a := newNetnsAgent(dir)
	w := want(node("node-b", "10.0.0.2", "fd00::2"))
	w.hold.addrs = map[netip.Addr]*datapath.AddressEntries{a4: {}}
	apply(t, a, w)
	assert.Empty(t, chain(t, e0, ebpf.AttachTCXIngress), "nothing held: no uplink")
	assert.Empty(t, chain(t, e1, ebpf.AttachTCXIngress))

	w.hold.addrs[a4] = held
	w.pools = map[int]dpPool{e1: {name: "eth1", v4: true}}
	apply(t, a, w)
	pl := progID(t, a.dp.objs.PlAnnounceEth)
	assert.Equal(t, []ebpf.ProgramID{pl}, chain(t, e0, ebpf.AttachTCXIngress), "the default-route interface")
	assert.Equal(t, []ebpf.ProgramID{pl}, chain(t, e1, ebpf.AttachTCXIngress), "the pool interface")
	assert.True(t, pinned("pl_announce-eth0"))
	assert.True(t, pinned("pl_announce-eth1"))

	// The guard on eth0 only: pl_announce follows its set, behind it.
	guard, err := ebpf.NewProgram(&ebpf.ProgramSpec{Type: ebpf.SchedCLS, License: "Apache-2.0",
		Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, -1), asm.Return()}})
	require.NoError(t, err)
	defer guard.Close()
	gl, err := link.AttachTCX(link.TCXOptions{Program: guard, Interface: e0, Attach: ebpf.AttachTCXIngress, Anchor: link.Head()})
	require.NoError(t, err)
	defer gl.Close()
	w.guardLinks, w.guardProgs = []int{e0}, []ebpf.ProgramID{9999999, progID(t, guard)}
	apply(t, a, w)
	assert.Equal(t, []ebpf.ProgramID{progID(t, guard), pl}, chain(t, e0, ebpf.AttachTCXIngress))
	assert.Empty(t, chain(t, e1, ebpf.AttachTCXIngress), "not one of the guard's")
	assert.False(t, pinned("pl_announce-eth1"))

	// A restart takes the pinned link over in place.
	closeAgent(a)
	b := newNetnsAgent(dir)
	defer closeAgent(b)
	apply(t, b, w)
	assert.Equal(t, []ebpf.ProgramID{progID(t, guard), progID(t, b.dp.objs.PlAnnounceEth)}, chain(t, e0, ebpf.AttachTCXIngress))

	// Holding nothing: off the uplinks, pins gone; the tunnel's stays.
	w.hold.addrs[a4] = &datapath.AddressEntries{}
	apply(t, b, w)
	assert.Equal(t, []ebpf.ProgramID{progID(t, guard)}, chain(t, e0, ebpf.AttachTCXIngress))
	assert.False(t, pinned("pl_announce-eth0"))
	assert.True(t, pinned("pl_announce-"+datapath.TunnelName))
}
