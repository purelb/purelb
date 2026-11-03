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
	"bytes"
	"context"
	"encoding/binary"
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
	"github.com/go-kit/log"
	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/egress"
	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// podNetns makes a pod: a network namespace with eth0 holding addrs, its
// host side vethName in this one. It returns the host side's ifindex.
func podNetns(t *testing.T, vethName string, addrs ...string) int {
	t.Helper()
	ifindex, _ := podNetnsAt(t, vethName, addrs...)
	return ifindex
}

// podNetnsAt is podNetns, also returning the pod's namespace.
func podNetnsAt(t *testing.T, vethName string, addrs ...string) (int, netns.NsHandle) {
	t.Helper()
	here, err := netns.Get()
	require.NoError(t, err)
	defer here.Close()
	pod, err := netns.New()
	require.NoError(t, err)
	require.NoError(t, netns.Set(here))
	t.Cleanup(func() { _ = pod.Close() })
	require.NoError(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: vethName}, PeerName: "eth0",
		PeerNamespace: netlink.NsFd(pod)}))
	h, err := netlink.NewHandleAt(pod, unix.NETLINK_ROUTE)
	require.NoError(t, err)
	defer h.Close()
	eth0, err := h.LinkByName("eth0")
	require.NoError(t, err)
	for _, a := range addrs {
		addr, err := netlink.ParseAddr(a)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, h.AddrAdd(eth0, addr))
	}
	require.NoError(t, h.LinkSetUp(eth0))
	host, err := netlink.LinkByName(vethName)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(host))
	return host.Attrs().Index, pod
}

// podDial opens n TCP connections from the pod to addr, each a new flow;
// none needs an answer.
func podDial(t *testing.T, podNS netns.NsHandle, addr string, n int) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // discarded with the goroutine
		if err := netns.Set(podNS); err != nil {
			done <- err
			return
		}
		for range n {
			if c, err := net.DialTimeout("tcp4", addr, 50*time.Millisecond); err == nil {
				_ = c.Close()
			}
		}
		done <- nil
	}()
	require.NoError(t, <-done)
}

// foreignChain adds, in this thread's namespace, another program's table
// ip <name> with one base chain at hook and prio, holding a rule of exprs
// when given.
func foreignChain(t *testing.T, name string, hook *nftables.ChainHook, prio nftables.ChainPriority,
	policy nftables.ChainPolicy, exprs ...expr.Any) *nftables.Table {
	t.Helper()
	c, err := nftables.New()
	require.NoError(t, err)
	tb := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: name})
	ch := c.AddChain(&nftables.Chain{Name: "post", Table: tb, Type: nftables.ChainTypeFilter, Hooknum: hook,
		Priority: nftables.ChainPriorityRef(prio), Policy: &policy})
	if len(exprs) > 0 {
		c.AddRule(&nftables.Rule{Table: tb, Chain: ch, Exprs: exprs})
	}
	require.NoError(t, c.Flush())
	return tb
}

func dropForeign(t *testing.T, tb *nftables.Table) {
	t.Helper()
	c, err := nftables.New()
	require.NoError(t, err)
	c.DelTable(tb)
	require.NoError(t, c.Flush())
}

// The real apply tags a pod's host-side device: its pod_devs entry and
// pl_pod_tag, pinned; the cluster networks from the API and the routes; a
// restart keeps both until the pods are planned, then takes the link over
// in place; a pod gone loses both. Also ForwardBlocked (D25), the mark
// byte's move and conflict (D18), and the shutdown repoint (D7).
func TestNetnsApplyDatapathPods(t *testing.T) {
	dir := sandbox(t)
	eth0, err := netlink.LinkByName("eth0")
	require.NoError(t, err)
	require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth0.Attrs().Index, Gw: net.ParseIP("10.0.0.254")}))
	flannel := dummy(t, "flannel.1")
	require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: flannel, Dst: cidrNet("10.244.2.0/24")}))
	veth, podNS := podNetnsAt(t, "vpod0", "10.244.1.5/24", "fd04:0:0:1::5/64")
	pod4, pod6 := netip.MustParseAddr("10.244.1.5"), netip.MustParseAddr("fd04:0:0:1::5")
	pinned := func() bool {
		_, err := os.Stat(filepath.Join(dir, "pl_pod_tag-vpod0"))
		return err == nil
	}

	// The pod stage: pod-set 3, a slice of 192.0.2.1 with FTP's helper,
	// whose NAT module isn't loaded here.
	defer func(m string) { sysModule = m }(sysModule)
	sysModule = t.TempDir()
	eg, eg6 := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")
	stage := &datapath.PodStage{SourceIP: true, Pods: map[netip.Addr]uint8{pod4: 3, pod6: 3}, PodSets: map[uint8][]string{3: {"p0"}},
		Policies: map[string]datapath.Policy{"p0": {V4: &datapath.PolicyFamily{
			Include: []datapath.Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, Slot: 4, Address: eg}}},
		Slots: map[uint8]datapath.Slot{4: {Addr: eg, First: 20480, Last: 24575,
			Helpers: []datapath.Helper{{Name: "ftp", Proto: unix.IPPROTO_TCP, Port: 21}}},
			6: {Addr: eg6, First: 20480, Last: 24575}}}

	a := newNetnsAgent(dir)
	w := want(node("node-b", "10.0.0.2", "fd00::2"))
	w.cluster = []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")}
	w.podsKnown = true
	w.pods = []dpPod{{name: "voice/a", v4: pod4, v6: pod6, podSet: 3, hold: true},
		{name: "voice/unwired", v4: netip.MustParseAddr("10.244.1.9")}}
	w.stage = stage
	// Slot 4's address is held by node-b, whose pool interface's MTU is
	// 1400; slot 6's too, at 1200, below IPv6's minimum.
	w.steer = map[uint8]dpSteer{4: {addr: eg, holders: []string{"node-b"}, mtu: 1400},
		6: {addr: eg6, holders: []string{"node-b"}, mtu: 1200}}
	apply(t, a, w)
	// The table: the stage with the cluster networks, the slot's MSS from
	// its path MTU (the tunnel to node-b, a 1500-byte eth0 less L3 Geneve
	// over IPv4, capped at the pool's 1400) less 40, and no helper, which
	// is reported instead.
	written := a.dp.table.Pods
	require.NotNil(t, written)
	assert.Contains(t, written.Cluster, netip.MustParsePrefix("10.96.0.0/12"))
	assert.Contains(t, written.Cluster, netip.MustParsePrefix("10.244.2.0/24"))
	assert.Equal(t, uint16(1400-40), written.Slots[4].MSS)
	assert.Equal(t, uint16(1280-60), written.Slots[6].MSS, "IPv6's minimum, and reported")
	assert.Equal(t, "PathMTUTooSmall", a.dp.podFaults[eg6].Reason)
	// The slots go into the tunnel, steered to node-b; the tunnel route
	// carries the smallest of its slots' path MTUs per family (IPv6's
	// locked, at least 1280); replies come back by the reply rule, which
	// src_valid_mark lets strict rp_filter use.
	assert.Equal(t, []string{"80 0x80 7800", "90 0x4 7700", "90 0x6 7700", "80 0x80 7800", "90 0x4 7700", "90 0x6 7700"},
		egressRules(t))
	assert.Equal(t, []uint64{datapath.NodeID("node-b")}, steerOf(t, a, 4))
	assert.Subset(t, targetDefaults(t, datapath.TunnelTable), []string{"default mtu 1400", "default6 mtu 1280 lock"})
	assert.Contains(t, throws(t, datapath.TunnelTable), "10.0.0.2/32", "the underlay backstop")
	assert.Contains(t, throws(t, datapath.TunnelTable), "10.96.0.0/12", "the cluster networks")
	assert.Equal(t, "1", sysctlIn(t, "net/ipv4/conf/"+datapath.TunnelName+"/src_valid_mark"))
	assert.Empty(t, written.Slots[4].Helpers)
	assert.Equal(t, "HelpersUnsupported", a.dp.podFaults[eg].Reason)
	assert.NotEmpty(t, a.dp.podFaults[eg].Message, "why it can't run")
	assert.Len(t, stage.Slots[4].Helpers, 1, "the queue goroutine's plan is untouched")
	intact, err := datapath.TableIntact(a.dp.nft, *a.dp.table)
	require.NoError(t, err)
	assert.True(t, intact)
	tag := progID(t, a.dp.objs.PlPodTag)
	assert.Equal(t, []ebpf.ProgramID{tag}, chain(t, veth, ebpf.AttachTCXIngress))
	assert.True(t, pinned())
	devs, err := a.dp.objs.RecoverPodDevs()
	require.NoError(t, err)
	assert.Len(t, devs, 1, "a pod with no device yet has no entry")
	assert.Equal(t, uint8(3), devs[veth].PodSet)
	assert.True(t, devs[veth].Hold)
	// What a CNI ADD may count on (§8.1): both of the pod's addresses,
	// entry and program in place; not the pod with no device.
	held := datapath.PodDev{V4: pod4, V6: pod6, PodSet: 3, Hold: true}
	assert.Equal(t, map[netip.Addr]datapath.PodDev{pod4: held, pod6: held}, a.dp.tagged)
	cluster, err := a.dp.objs.ClusterNetworks()
	require.NoError(t, err)
	assert.Contains(t, cluster, netip.MustParsePrefix("10.96.0.0/12"), "from the API")
	assert.Contains(t, cluster, netip.MustParsePrefix("10.244.2.0/24"), "from the routes")
	assert.NotContains(t, cluster, netip.MustParsePrefix("10.0.0.0/24"), "the uplink's subnet")
	assert.Equal(t, map[netip.Addr]uint8{pod4: 3, pod6: 3}, recoveredPodSets(dir))

	// A restart: until the pods are planned the kernel keeps what it has;
	// then the pinned link is taken over in place.
	id, _ := pinnedLink(t, filepath.Join(dir, "pl_pod_tag-vpod0"))
	closeAgent(a)
	b := newNetnsAgent(dir)
	early := w
	early.pods, early.podsKnown = nil, false
	early.stage = nil
	apply(t, b, early)
	assert.Equal(t, []ebpf.ProgramID{tag}, chain(t, veth, ebpf.AttachTCXIngress),
		"the earlier agent's program, on the shared pinned maps")
	intact, err = datapath.TableIntact(b.dp.nft, *a.dp.table)
	require.NoError(t, err)
	assert.True(t, intact, "the earlier agent's table, pod stage and all")
	devs, err = b.dp.objs.RecoverPodDevs()
	require.NoError(t, err)
	assert.Len(t, devs, 1, "kept")
	apply(t, b, w)
	require.NotNil(t, b.dp.table.Pods, "rewritten once planned")
	again, _ := pinnedLink(t, filepath.Join(dir, "pl_pod_tag-vpod0"))
	assert.Equal(t, id, again, "the same link")
	assert.Equal(t, []ebpf.ProgramID{progID(t, b.dp.objs.PlPodTag)}, chain(t, veth, ebpf.AttachTCXIngress),
		"now running this agent's program")

	// A protocol without ports from the pod to the slice's destinations is
	// dropped and reported (D4): ProtocolUnsupported on the address.
	w.pods[0].hold = false
	apply(t, b, w)
	gw, err := netlink.ParseAddr("10.244.1.1/24")
	require.NoError(t, err)
	require.NoError(t, netlink.AddrAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Index: veth}}, gw))
	ph, err := netlink.NewHandleAt(podNS, unix.NETLINK_ROUTE)
	require.NoError(t, err)
	defer ph.Close()
	require.NoError(t, ph.RouteAdd(&netlink.Route{Gw: net.ParseIP("10.244.1.1")}))
	sent := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // discarded with the goroutine
		if err := netns.Set(podNS); err != nil {
			sent <- err
			return
		}
		c, err := net.Dial("ip4:253", "203.0.113.7")
		if err == nil {
			for i := 0; i < 3 && err == nil; i++ {
				_, err = c.Write([]byte("portless"))
			}
			_ = c.Close()
		}
		sent <- err
	}()
	require.NoError(t, <-sent)
	// Polled here, not in require.Eventually: its condition runs on another
	// goroutine, outside this thread's namespace, and the agent's nft calls
	// would reach the host's.
	for end := time.Now().Add(5 * time.Second); b.dp.podFaults[eg].Reason != "ProtocolUnsupported" && time.Now().Before(end); {
		time.Sleep(100 * time.Millisecond)
		apply(t, b, w)
	}
	require.Equal(t, "ProtocolUnsupported", b.dp.podFaults[eg].Reason)

	// The stage probe (design 6.1), sent from the pod as the plugin sends
	// it: seen ahead of classify with pl_pod_tag's mark and forwarded in
	// both families, then dropped, taking no slot. Forwarding on in both
	// (a new namespace takes IPv4's from the host's, which may be off,
	// and starts with IPv6's off), and IPv6 routes both ways.
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644))
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0o644))
	gw6, err := netlink.ParseAddr("fd04:0:0:1::1/64")
	require.NoError(t, err)
	gw6.Flags = unix.IFA_F_NODAD
	require.NoError(t, netlink.AddrAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Index: veth}}, gw6))
	require.NoError(t, ph.RouteAdd(&netlink.Route{Gw: net.ParseIP("fd04:0:0:1::1")}))
	require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth0.Attrs().Index, Gw: net.ParseIP("fd00::254")}))
	flows, err := datapath.ForwardCounts(b.dp.nft, *b.dp.table.Pods)
	require.NoError(t, err)
	txPackets := func() uint64 {
		l, err := netlink.LinkByName("eth0")
		require.NoError(t, err)
		return l.Attrs().Statistics.TxPackets
	}
	tx := txPackets()
	probed := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // discarded with the goroutine
		if err := netns.Set(podNS); err != nil {
			probed <- err
			return
		}
		for _, x := range [][2]netip.Addr{{pod4, cnisock.ProbeV4}, {pod6, cnisock.ProbeV6}} {
			c, err := net.DialUDP("udp", &net.UDPAddr{IP: x[0].AsSlice()}, &net.UDPAddr{IP: x[1].AsSlice(), Port: cnisock.ProbePort})
			if err == nil {
				_, err = c.Write([]byte("probe"))
				_ = c.Close()
			}
			if err != nil {
				probed <- err
				return
			}
		}
		probed <- nil
	}()
	require.NoError(t, <-probed)
	// The counters are watched on another goroutine, whose thread isn't
	// in this namespace: read them on a socket opened here.
	lasting, err := datapath.NftConn(nftables.AsLasting())
	require.NoError(t, err)
	defer lasting.CloseLasting()
	b.probeCounts = func() (map[string]datapath.ProbeCount, error) { return datapath.ProbeCounts(lasting) }
	tk := &probeTicket{container: "c-probe", at: time.Now()}
	b.prober.Store(tk)
	resp := b.probeReport(cnisock.Request{Op: cnisock.OpProbe, ContainerID: "c-probe", Namespace: "voice", Name: "a"},
		[]netip.Addr{pod4, pod6})
	require.Empty(t, resp.Error)
	for deadline := time.Now().Add(5 * time.Second); tk.result.Load() == nil && !tk.failed.Load() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	require.NotNil(t, tk.result.Load())
	assert.Equal(t, cnisock.StageMark, tk.result.Load().stage)
	counts, err := datapath.ProbeCounts(b.dp.nft)
	require.NoError(t, err)
	assert.Equal(t, map[string]datapath.ProbeCount{"4": {Seen: 1, Tagged: 1, Forwarded: 1}, "6": {Seen: 1, Tagged: 1, Forwarded: 1}}, counts)
	after, err := datapath.ForwardCounts(b.dp.nft, *b.dp.table.Pods)
	require.NoError(t, err)
	assert.Equal(t, flows, after, "no slot for the probe")
	assert.Equal(t, tx, txPackets(), "dropped: the probe never leaves the node")
	b.stage.Store(nil)

	faults := b.faultList(health(b.dp, w, nil))
	assert.Contains(t, faults, purelbv2.EgressAddressFault{Address: "192.0.2.1", Reason: "ProtocolUnsupported",
		Message: b.dp.podFaults[eg].Message})

	// ForwardBlocked (D25), over windows of the agent's clock: the pod's new
	// flows marked with slot 4 and reaching its SNAT, until a foreign
	// forward drop stops them for two windows in a row.
	clock := time.Now()
	b.now = func() time.Time { return clock }
	window := func() {
		podDial(t, podNS, "203.0.113.7:80", 3)
		clock = clock.Add(forwardWindow + time.Second)
		apply(t, b, w)
	}
	forwardCond := func(c *purelbv2.EgressCapability) *metav1.Condition {
		return apimeta.FindStatusCondition(b.nodeConditions(c, health(b.dp, w, nil), nil), condForwardPath)
	}
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv4/conf/all/forwarding", []byte("1"), 0o644))
	c4 := apply(t, b, w)
	window()
	window()
	assert.Empty(t, b.dp.forwardBlocked, "the flows reach their SNAT")
	fw := foreignChain(t, "fw", nftables.ChainHookForward, 0, nftables.ChainPolicyDrop)
	window()
	assert.Empty(t, b.dp.forwardBlocked, "one window isn't enough")
	window()
	assert.Equal(t, []string{"IPv4"}, b.dp.forwardBlocked)
	assert.Equal(t, 1.0, testutil.ToFloat64(forwardBlocked.WithLabelValues("IPv4")))
	assert.Equal(t, 0.0, testutil.ToFloat64(forwardBlocked.WithLabelValues("IPv6")))
	assert.Equal(t, "ForwardBlocked", forwardCond(c4).Reason)
	assert.Contains(t, forwardCond(c4).Message, "IPv4: ")
	dropForeign(t, fw)
	window()
	assert.Empty(t, b.dp.forwardBlocked, "one reached its SNAT")
	assert.Equal(t, 0.0, testutil.ToFloat64(forwardBlocked.WithLabelValues("IPv4")))
	assert.Equal(t, metav1.ConditionTrue, forwardCond(c4).Status)

	// The mark byte (D18): another rule setting a bit of an unset field's
	// byte after classify moves it to the next free one; rules, BPF config
	// and table follow, and the pod's flows marked in the old byte are
	// reset, another source's stay.
	cni := foreignChain(t, "cni", nftables.ChainHookPostrouting, 0, nftables.ChainPolicyAccept,
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(^uint32(1)), Xor: binaryutil.NativeEndian.PutUint32(1)},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true})
	require.NoError(t, netlink.ConntrackTableFlush(netlink.ConntrackTable))
	ctFlow(t, "10.244.1.5", 2000, 4)
	ctFlow(t, "10.0.0.99", 2001, 4)
	moves, moved := testutil.ToFloat64(markMoves), testutil.ToFloat64(ctDeleted.WithLabelValues("markMoved"))
	clock = clock.Add(markReread)
	capa := apply(t, b, w)
	assert.Equal(t, "0x00ff0000", capa.MarkField)
	assert.Equal(t, []string{"80 0x800000 7800", "90 0x40000 7700", "90 0x60000 7700",
		"80 0x800000 7800", "90 0x40000 7700", "90 0x60000 7700"}, egressRules(t))
	shift, err := b.dp.objs.MarkShift()
	require.NoError(t, err)
	assert.Equal(t, uint8(16), shift)
	assert.Equal(t, uint8(16), b.dp.table.Pods.MarkShift)
	assert.Equal(t, uint32(16), b.markByte.Load(), "the flow log reads the byte in use")
	assert.Equal(t, []string{"10.0.0.99"}, ctMarked(t, 0, 4))
	assert.Equal(t, 1.0, testutil.ToFloat64(markMoves)-moves)
	assert.Equal(t, 1.0, testutil.ToFloat64(ctDeleted.WithLabelValues("markMoved"))-moved)
	assert.Empty(t, b.dp.markConflict)
	assert.Nil(t, b.dp.movedFrom)
	// Set explicitly to that byte: never moved, MarkConflict instead.
	compat := func(c *purelbv2.EgressCapability) *metav1.Condition {
		// purelb-cni reporting the plugin chained.
		b.installer.Store(&cnisock.InstallerStatus{Written: b.now(), Chain: cnisock.ChainPresent})
		return apimeta.FindStatusCondition(b.nodeConditions(c, health(b.dp, w, nil), nil), condDatapathCompatible)
	}
	w.spec.MarkField = "0x000000ff"
	clock = clock.Add(markReread)
	capa = apply(t, b, w)
	assert.Equal(t, "0x000000ff", capa.MarkField)
	assert.Equal(t, "markField 0x000000ff is used by other rules: ip cni post (sets mark 0x1)", b.dp.markConflict)
	assert.Equal(t, 1.0, testutil.ToFloat64(markConflict))
	assert.Equal(t, "MarkConflict", compat(capa).Reason)
	dropForeign(t, cni)
	clock = clock.Add(markReread)
	capa = apply(t, b, w)
	assert.Empty(t, b.dp.markConflict)
	assert.Equal(t, 0.0, testutil.ToFloat64(markConflict))
	assert.Equal(t, metav1.ConditionTrue, compat(capa).Status)
	// Unset again: the byte in use stays while it is free.
	w.spec.MarkField = ""
	clock = clock.Add(markReread)
	assert.Equal(t, "0x000000ff", apply(t, b, w).MarkField)

	// Shutting down (D7): slot 4's routed address, held here too, goes
	// direct by main until the slot is repointed through the tunnel to the
	// advertiser that remains; with none it stays direct; slot 6, held
	// elsewhere, is left; the next apply takes the direct path back.
	w.hold.addrs = map[netip.Addr]*datapath.AddressEntries{eg: {
		Address: datapath.Address{Held: true, SliceSize: 4096, Table: mainTable}}}
	w.steer[4] = dpSteer{addr: eg, holders: []string{"node-a", "node-b"}, routed: true, mtu: 1400}
	apply(t, b, w)
	assert.Equal(t, []string{"80 0x80 7800", "90 0x4 254", "90 0x6 7700", "80 0x80 7800", "90 0x4 254", "90 0x6 7700"},
		egressRules(t))
	repointed := testutil.ToFloat64(repointedSlots)
	without := map[uint8]dpSteer{4: {addr: eg, holders: []string{"node-b"}, routed: true, mtu: 1400}, 6: w.steer[6]}
	asAgent(t, func() { require.NoError(t, b.repointSlots(without)) })
	assert.Equal(t, []string{"80 0x80 7800", "90 0x4 7700", "90 0x6 7700", "80 0x80 7800", "90 0x4 7700", "90 0x6 7700"},
		egressRules(t))
	assert.Equal(t, []uint64{datapath.NodeID("node-b")}, steerOf(t, b, 4))
	assert.Contains(t, throws(t, datapath.TunnelTable), "10.96.0.0/12", "the cluster networks stay out of it")
	assert.Equal(t, map[uint8][]string{4: {"node-b"}}, b.dp.repointed)
	assert.Equal(t, 1.0, testutil.ToFloat64(repointedSlots)-repointed)
	apply(t, b, w)
	asAgent(t, func() {
		require.NoError(t, b.repointSlots(map[uint8]dpSteer{4: {addr: eg, routed: true}, 6: w.steer[6]}))
	})
	assert.Equal(t, []string{"80 0x80 7800", "90 0x4 254", "90 0x6 7700", "80 0x80 7800", "90 0x4 254", "90 0x6 7700"},
		egressRules(t), "no other advertiser: direct")
	assert.Empty(t, b.dp.repointed)
	w.hold.addrs = nil
	w.steer[4] = dpSteer{addr: eg, holders: []string{"node-b"}, mtu: 1400}
	apply(t, b, w)
	assert.Equal(t, []string{"80 0x80 7800", "90 0x4 7700", "90 0x6 7700", "80 0x80 7800", "90 0x4 7700", "90 0x6 7700"},
		egressRules(t))

	// Slot 4's holder gone: FailClosed, its rule sends it to prohibit,
	// and its steering is pruned.
	w.steer = map[uint8]dpSteer{4: {addr: eg, mtu: 1400}}
	apply(t, b, w)
	assert.Equal(t, []string{"80 0x80 7800", "90 0x4 7799", "90 0x6 7799", "80 0x80 7800", "90 0x4 7799", "90 0x6 7799"},
		egressRules(t))
	assert.Nil(t, steerOf(t, b, 4))
	// Its flows are dropped by design, never reported ForwardBlocked.
	window()
	window()
	window()
	assert.Empty(t, b.dp.forwardBlocked)
	require.NoError(t, netlink.ConntrackTableFlush(netlink.ConntrackTable))

	// Conntrack (§5.6, D18): a retired mark value's flows are reset, after
	// which it may be used again; a Draining slot with flows isn't
	// drained, one without a slot is.
	ct := func(src string, sport uint16, mark uint32) { ctFlow(t, src, sport, mark) }
	marked := func(v uint8) []string { return ctMarked(t, 0, v) }
	ct("10.244.1.5", 1000, 4)
	ct("10.244.1.9", 1001, 4)
	ct("fd04:0:0:1::5", 1002, 6)
	ct("10.244.1.5", 1003, 9)
	ct("10.244.1.5", 1004, 0x109)
	reset := testutil.ToFloat64(ctDeleted.WithLabelValues("slotRetired"))
	w.retired = []uint8{9}
	w.draining = map[uint8]purelbv2.EgressDrained{6: {Endpoints: "e6", Index: 1}}
	w.drainedNoSlot = []purelbv2.EgressDrained{{Endpoints: "e0", Index: 2}}
	assert.True(t, b.ctInUse(9), "not known to be free")
	apply(t, b, w)
	assert.Empty(t, marked(9), "the retired value's flows")
	assert.Equal(t, 2.0, testutil.ToFloat64(ctDeleted.WithLabelValues("slotRetired"))-reset,
		"both: bits outside the field don't count")
	assert.False(t, b.ctInUse(9), "free to use again")
	assert.Equal(t, []purelbv2.EgressDrained{{Endpoints: "e0", Index: 2}}, *b.drained.Load(), "slot 6 has a flow")
	require.NoError(t, netlink.ConntrackTableFlush(netlink.ConntrackTable))
	ct("10.244.1.5", 1000, 4)
	ct("10.244.1.9", 1001, 4)
	apply(t, b, w)
	assert.Equal(t, []purelbv2.EgressDrained{{Endpoints: "e0", Index: 2}, {Endpoints: "e6", Index: 1}}, *b.drained.Load())
	// A value used again may carry flows again.
	w.retired, w.stage.Slots[9] = nil, datapath.Slot{Addr: eg, First: 4096, Last: 8191}
	apply(t, b, w)
	assert.True(t, b.ctInUse(9))
	delete(w.stage.Slots, 9)

	// A pod gone from the node: its flows go, the others' stay.
	gone := testutil.ToFloat64(ctDeleted.WithLabelValues("podDeleted"))
	w.pods = w.pods[:1]
	apply(t, b, w)
	assert.Equal(t, []string{"10.244.1.5"}, marked(4))
	assert.Equal(t, 1.0, testutil.ToFloat64(ctDeleted.WithLabelValues("podDeleted"))-gone)

	// The pod gone while no agent ran: the next deletes its flows too,
	// knowing its addresses from the pinned map. And entry, link and pin
	// go.
	ct("10.244.1.5", 1005, 4)
	closeAgent(b)
	c := newNetnsAgent(dir)
	w.pods = nil
	apply(t, c, w)
	assert.Empty(t, marked(4))
	assert.Empty(t, chain(t, veth, ebpf.AttachTCXIngress))
	assert.False(t, pinned())
	devs, err = c.dp.objs.RecoverPodDevs()
	require.NoError(t, err)
	assert.Empty(t, devs)

	// An agent start with no entry pinned for a FailClosed pod (it started
	// while no agent ran, as after a reboot): held before the table
	// classifies anything, then let go, its slot ready (design 8.1); not
	// the pod with an entry an earlier agent pinned (single-family, as the
	// held one: a missing family is no address). A restart finding its
	// entry pinned doesn't hold it again.
	var logs bytes.Buffer
	start := func() *Agent {
		closeAgent(c)
		logs.Reset()
		n := newNetnsAgent(dir)
		n.logger = logging.New(&logs, logging.LevelDebug)
		return n
	}
	open4 := netip.MustParseAddr("10.244.1.9")
	openVeth, _ := podNetnsAt(t, "vpod1", "10.244.1.9/24")
	open := dpPod{name: "voice/open", v4: open4, failClosed: true}
	c = start()
	w.pods = []dpPod{open}
	apply(t, c, w)
	c = start()
	w.pods = []dpPod{{name: "voice/a", v4: pod4, podSet: 3, failClosed: true}, open,
		{name: "voice/b", v4: netip.MustParseAddr("10.244.1.10")}}
	apply(t, c, w)
	assert.Contains(t, logs.String(), `"pods":["voice/a"]`, "FailClosed pods no earlier agent programmed only")
	heldAt, writtenAt := strings.Index(logs.String(), "holding FailClosed pods"), strings.Index(logs.String(), "egress table written")
	require.GreaterOrEqual(t, heldAt, 0, logs.String())
	require.GreaterOrEqual(t, writtenAt, 0, logs.String())
	assert.Less(t, heldAt, writtenAt, "held before the table")
	devs, err = c.dp.objs.RecoverPodDevs()
	require.NoError(t, err)
	assert.Equal(t, datapath.PodDev{V4: pod4, PodSet: 3}, devs[veth], "let go once classified")
	assert.Equal(t, datapath.PodDev{V4: open4}, devs[openVeth])
	c = start()
	apply(t, c, w)
	assert.NotContains(t, logs.String(), "holding FailClosed pods")

	// The pod's device deleted while the pod is still planned (its
	// sandbox torn down during termination, without the plugin's DEL):
	// its link goes, and a CNI ADD no longer counts it tagged.
	w.pods = []dpPod{{name: "voice/a", v4: pod4, v6: pod6, podSet: 3}}
	apply(t, c, w)
	require.Contains(t, c.dp.tagged, pod4)
	require.NoError(t, netlink.LinkDel(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Index: veth}}))
	apply(t, c, w)
	assert.NotContains(t, c.dp.links, "pl_pod_tag-vpod0")
	assert.Empty(t, c.dp.tagged)
	assert.False(t, pinned())
	closeAgent(c)
}

// The datapath's desired state carries this node's pods, planned, and the
// cluster networks the API shows: podCIDRs, node addresses, Service CIDRs,
// the API server, excludedNetworks, podNetworks and the fixed ones.
func TestDatapathGetsPodsAndCluster(t *testing.T) {
	const ep = "e-voice.sip-0a0b0c0d"
	ens := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode},
		Spec: purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{
			{Endpoints: ep, Address: "192.0.2.1", Index: 2, State: "Pending", SliceSize: 4096},
			{Endpoints: ep + "-db", Address: "192.0.2.2", Index: 0, State: "Active", SliceSize: 4096, Identity: "db-0"}},
			Reserved: []purelbv2.EgressNodeReserved{{Endpoints: ep, Address: "192.0.2.1", Protocol: "UDP", Port: 5060, Pod: "a-1"}}}}
	ea := &purelbv2.EgressAddress{ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: ep,
		Labels: map[string]string{egress.LabelKind: egress.KindEgress, egress.LabelNamespace: "voice", egress.LabelName: "sip"}},
		Spec: purelbv2.EgressAddressSpec{State: "Active", Address: "192.0.2.1", Family: "IPv4", ServiceGroup: "default",
			PoolType: "Local", Placement: "SingleAnnouncer"}}
	eg := &purelbv2.Egress{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip", UID: "uid-sip"},
		Spec: purelbv2.EgressSpec{PodSelector: &metav1.LabelSelector{MatchLabels: appA}}}
	n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.2.0/24", "fd04:0:0:2::/64"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.2"},
			{Type: corev1.NodeExternalIP, Address: "198.51.100.2"}, {Type: corev1.NodeHostName, Address: "n2"}}}}
	cidrs := &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"},
		Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12", "fd05::/112"}}}
	api := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes",
		Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.200"}}}}
	perPod := ea.DeepCopy()
	perPod.Name, perPod.Spec.Address = ep+"-db", "192.0.2.2"
	f := newFixture(t, true, testNodeObj, n2, cidrs, api, ens, ea, perPod, eg, pnPod("voice", "a-1", appA, "10.1.0.5", "fd01::5"))
	f.kube.Resources = append(f.kube.Resources, &metav1.APIResourceList{GroupVersion: networkingv1.SchemeGroupVersion.String(),
		APIResources: []metav1.APIResource{{Name: "servicecidrs"}}})
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true, ExcludedNetworks: []string{"203.0.113.0/24"},
		PodNetworks: []string{"fd99::/64"}}))

	var w dpWant
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
		default:
		}
		return w.podsKnown && len(w.pods) == 1
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "voice/a-1", w.pods[0].name)
	assert.Equal(t, netip.MustParseAddr("10.1.0.5"), w.pods[0].v4)
	assert.Equal(t, netip.MustParseAddr("fd01::5"), w.pods[0].v6)
	assert.NotZero(t, w.pods[0].podSet)
	assert.True(t, w.pods[0].hold, "FailClosed, its slot Pending")
	// The flow log names flows by the plan's pods and slots' policies.
	fv := f.a.flowView.Load()
	require.NotNil(t, fv)
	assert.Equal(t, "voice/a-1", fv.pods[netip.MustParseAddr("fd01::5")])
	require.NotEmpty(t, fv.slots)
	for _, s := range fv.slots {
		assert.Contains(t, s.policy, "voice/")
	}
	for _, p := range []string{"10.244.2.0/24", "fd04:0:0:2::/64", "10.0.0.1/32", "10.0.0.2/32", "198.51.100.2/32", "fd00::1/128",
		"10.96.0.0/12", "fd05::/112", "192.0.2.200/32", "203.0.113.0/24", "fd99::/64", "169.254.0.0/16", "ff00::/8"} {
		assert.Contains(t, w.cluster, netip.MustParsePrefix(p))
	}
	assert.True(t, w.nodeAddrs[netip.MustParseAddr("198.51.100.2")])
	// Another node holds the address (the election's "owner"): this node's
	// maps carry its own slice and reserved port, so the tunnel's replies
	// to its pods are admitted (design 6.5).
	self := datapath.NodeID(testNode)
	e := w.hold.addrs[netip.MustParseAddr("192.0.2.1")]
	require.NotNil(t, e)
	assert.False(t, e.Held)
	assert.Equal(t, datapath.Address{SliceSize: 4096, Base: 1024}, e.Address)
	assert.Equal(t, map[uint16]uint64{2: self}, e.Slices)
	assert.Equal(t, map[datapath.PortKey]uint64{{Proto: 17, Port: 5060}: self}, e.Reserved)
	e = w.hold.addrs[netip.MustParseAddr("192.0.2.2")]
	require.NotNil(t, e)
	assert.Equal(t, datapath.Address{WholeOwner: self, SliceSize: 4096, Base: 1024}, e.Address, "PerPod: every port and protocol")
}

// A restarted agent's first plan keeps the pod-set id the pinned maps give
// its pod's address.
func TestSeedPodSets(t *testing.T) {
	p := pnEgress("voice", "p", 100, nil)
	in := pnIn("Active", []*corev1.Pod{pnPod("voice", "a", appA, "10.1.0.5")}, p)
	assert.Equal(t, uint8(2), planPodNode(in, nil).pods["voice/a"].podSet, "slot 1, pod-set 2 when fresh")
	pn := planPodNode(in, seedPodSets(in, map[netip.Addr]uint8{netip.MustParseAddr("10.1.0.5"): 9}))
	assert.Equal(t, uint8(9), pn.pods["voice/a"].podSet)
	_, slotOn9 := pn.slots[9]
	assert.False(t, slotOn9, "the slot takes another id")
}

// A restarted agent's first plan keeps the slot ids its table holds,
// matched by address and ports, so conntrack's marks keep naming their
// slots; one matching no slice is retired until its flows are deleted.
func TestSeedSlots(t *testing.T) {
	p := pnEgress("voice", "p", 100, nil)
	pp := pnEgress("voice", "pp", 50, func(p *pnPolicy) { p.spec.Affinity = &purelbv2.EgressAffinity{Mode: "PerPod"} })
	in := pnIn("Active", []*corev1.Pod{pnPod("voice", "a", appA, "10.1.0.5")}, p, pp)
	in.spec.Slices[1].Identity, in.spec.Slices[1].Index = "a", 0
	in.inUse = func(uint8) bool { return true }
	prev := seedPodSets(in, nil)
	seedSlots(prev, in, map[uint8]datapath.SlotNAT{
		7: {Addr: netip.MustParseAddr("192.0.2.1"), First: 8192, Last: 12287}, // e-p's slice 2 of 4096
		4: {Addr: netip.MustParseAddr("192.0.2.2")},                           // e-pp's PerPod address
		5: {Addr: netip.MustParseAddr("192.0.2.1"), First: 4096, Last: 8191},  // a slice no longer here
	})
	pn := planPodNode(in, prev)
	assert.Equal(t, map[slotKey]uint8{{endpoints: "e-p", index: 2}: 7, {endpoints: "e-pp", index: 0}: 4}, pn.slotIDs)
	assert.Equal(t, map[uint8]bool{5: true}, pn.retired)
	assert.Equal(t, uint8(1), pn.pods["voice/a"].podSet, "the pod-set takes the lowest free id")
}

// A retired mark value is in use until the datapath reports its flows
// deleted (D18).
func TestCtInUse(t *testing.T) {
	a := New(log.NewNopLogger(), "a", "purelb-system")
	assert.True(t, a.ctInUse(5), "nothing known")
	a.ctFlushed.Store(&map[uint8]bool{5: true})
	assert.False(t, a.ctInUse(5))
	assert.True(t, a.ctInUse(6))
	a.unflushLive(dpWant{stage: &datapath.PodStage{Slots: map[uint8]datapath.Slot{5: {}}}})
	assert.True(t, a.ctInUse(5), "used again")
}

// A spec generation is acked only when the plan came from it too, and
// gave every slot an id.
func TestAckable(t *testing.T) {
	assert.Equal(t, int64(3), ackable(3, &podNode{generation: 3}))
	assert.Zero(t, ackable(4, &podNode{generation: 3}), "planned from another generation")
	assert.Zero(t, ackable(3, &podNode{generation: 3, short: []string{"e-x"}}), "a slot without an id")
}

// The datapath gets this node's Draining slices and the spec generation
// its plan and holdings came from; installed, the status acks it
// (observedGeneration) and lists the slices drained, only those Draining
// now (§5.6, D6).
func TestSpecAckAndDrained(t *testing.T) {
	const ep = "e-voice.sip-0a0b0c0d"
	ens := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode, Generation: 3},
		Spec: purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{
			{Endpoints: ep, Address: "192.0.2.1", Index: 2, State: "Active", SliceSize: 4096},
			{Endpoints: ep, Address: "192.0.2.1", Index: 3, State: "Draining", SliceSize: 4096},
			{Endpoints: "e-gone", Address: "192.0.2.9", Index: 0, State: "Draining", SliceSize: 4096}}}}
	ea := &purelbv2.EgressAddress{ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: ep,
		Labels: map[string]string{egress.LabelKind: egress.KindEgress, egress.LabelNamespace: "voice", egress.LabelName: "sip"}},
		Spec: purelbv2.EgressAddressSpec{State: "Active", Address: "192.0.2.1", Family: "IPv4", ServiceGroup: "default",
			PoolType: "Local", Placement: "SingleAnnouncer"}}
	eg := &purelbv2.Egress{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip", UID: "uid-sip"},
		Spec: purelbv2.EgressSpec{PodSelector: &metav1.LabelSelector{MatchLabels: appA}}}
	f := newFixture(t, true, testNodeObj, ens, ea, eg, pnPod("voice", "a-1", appA, "10.1.0.5"))
	// An earlier agent's table gave the Active slice slot 9.
	f.a.tableSlots = func() (map[uint8]datapath.SlotNAT, error) {
		return map[uint8]datapath.SlotNAT{9: {Addr: netip.MustParseAddr("192.0.2.1"), First: 8192, Last: 12287}}, nil
	}
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true}))

	var w dpWant
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
		default:
		}
		return w.specGen == 3
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, uint16(8192), w.stage.Slots[9].First, "the slot keeps the id the table gave it")
	require.Len(t, w.draining, 1)
	for id, d := range w.draining {
		assert.Equal(t, purelbv2.EgressDrained{Endpoints: ep, Index: 3}, d)
		assert.Contains(t, w.stage.Slots, id, "its slot")
	}
	assert.Equal(t, []purelbv2.EgressDrained{{Endpoints: "e-gone", Index: 0}}, w.drainedNoSlot, "no slot: no flows of its own")

	// What the datapath found drained, one of them no longer Draining.
	f.a.drained.Store(&[]purelbv2.EgressDrained{{Endpoints: ep, Index: 3}, {Endpoints: "e-gone", Index: 0}, {Endpoints: ep, Index: 2}})
	f.a.add(keyNode, false)
	var st purelbv2.EgressNodeStateStatus
	require.Eventually(t, func() bool {
		ns, err := f.purelb.PurelbV2().EgressNodeStates().Get(context.Background(), testNode, metav1.GetOptions{})
		if err == nil {
			st = ns.Status
		}
		return err == nil && st.ObservedGeneration == 3 && len(st.Drained) > 0
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []purelbv2.EgressDrained{{Endpoints: ep, Index: 3}, {Endpoints: "e-gone", Index: 0}}, st.Drained)

	// Slot 9's slice goes: its id is retired until the datapath reports
	// its flows deleted (D18).
	ens = f.nodeState()
	ens.Spec.Slices, ens.Generation = ens.Spec.Slices[1:], 4
	_, err := f.purelb.PurelbV2().EgressNodeStates().Update(context.Background(), ens, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
		default:
		}
		return w.specGen == 4
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []uint8{9}, w.retired)
	assert.NotContains(t, w.stage.Slots, uint8(9))
	f.a.ctFlushed.Store(&map[uint8]bool{9: true})
	f.a.add(keyPods, false)
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
		default:
		}
		return w.specGen == 4 && len(w.retired) == 0
	}, 5*time.Second, 10*time.Millisecond, "free once flushed")

	// A new generation that changes no slot (a reserved port for a pod
	// not here) is installed and acked all the same.
	ens = f.nodeState()
	ens.Spec.Reserved, ens.Generation = []purelbv2.EgressNodeReserved{{Endpoints: ep, Address: "192.0.2.1",
		Protocol: "TCP", Port: 99, Pod: "elsewhere"}}, 5
	_, err = f.purelb.PurelbV2().EgressNodeStates().Update(context.Background(), ens, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
		default:
		}
		return w.specGen == 5
	}, 5*time.Second, 10*time.Millisecond)
}

// steerOf is slot's holders in the steer map, nil without an entry.
func steerOf(t *testing.T, a *Agent, slot uint32) []uint64 {
	t.Helper()
	raw := make([]byte, a.dp.objs.Steer.ValueSize())
	if err := a.dp.objs.Steer.Lookup(slot, &raw); err != nil {
		return nil
	}
	n := binary.NativeEndian.Uint32(raw)
	var out []uint64
	for i := uint32(0); i < n; i++ {
		out = append(out, binary.NativeEndian.Uint64(raw[8+8*i:]))
	}
	return out
}

// targetDefaults lists a table's default routes as "default[6] mtu N[ lock]".
func targetDefaults(t *testing.T, table int) []string {
	t.Helper()
	var out []string
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(fam, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		require.NoError(t, err)
		for _, r := range routes {
			if r.Dst != nil {
				if ones, _ := r.Dst.Mask.Size(); ones != 0 {
					continue
				}
			}
			d := "default"
			if fam == netlink.FAMILY_V6 {
				d = "default6"
			}
			d += fmt.Sprintf(" mtu %d", r.MTU)
			if r.MTULock {
				d += " lock"
			}
			out = append(out, d)
		}
	}
	return out
}
