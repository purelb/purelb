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
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/go-kit/log"
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
