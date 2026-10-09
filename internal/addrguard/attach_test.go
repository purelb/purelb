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

package addrguard

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// TestAttacherInNetns drives the attacher against the real kernel, inside a
// throwaway network namespace so the host's interfaces are never touched.
// Needs CAP_SYS_ADMIN as well as CAP_BPF and CAP_NET_ADMIN; the bpf-test CI
// job runs it in its own step. The attacher's methods are
// called directly on this (locked) thread, which is what makes the
// namespace apply to them.
func TestAttacherInNetns(t *testing.T) {
	h := newHarness(t) // skips (or fails when required) without privileges

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer orig.Close()
	ns, err := netns.New() // also switches this thread into it
	if err != nil {
		// Creating a namespace needs CAP_SYS_ADMIN, which the reduced
		// capability set of the main BPF run deliberately lacks; CI runs
		// this test in a second step with it.
		if os.Getenv("ADDRGUARD_NETNS_TESTS") == "required" {
			t.Fatalf("netns test required but namespace creation failed: %v", err)
		}
		t.Skipf("cannot create a network namespace: %v", err)
	}
	defer func() {
		require.NoError(t, netns.Set(orig))
		ns.Close()
	}()

	mk := func(l netlink.Link) netlink.Link {
		require.NoError(t, netlink.LinkAdd(l))
		got, err := netlink.LinkByName(l.Attrs().Name)
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetUp(got))
		return got
	}
	mk(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "agv0"}, PeerName: "agv1"})
	peer, _ := netlink.LinkByName("agv1")
	require.NoError(t, netlink.LinkSetUp(peer))
	mk(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "agd0"}})
	dummy := mk(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "agdum"}})
	// The kernel creates the IPv6 subnet-router anycast only with forwarding.
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv6/conf/agdum/forwarding", []byte("1"), 0o644))
	for _, cidr := range []string{"10.255.77.1/24", "fd77::1/64"} {
		addr, _ := netlink.ParseAddr(cidr)
		addr.Flags |= 0x02 // NODAD
		require.NoError(t, netlink.AddrAdd(dummy, addr))
	}

	g := newGuard(log.NewNopLogger(), "node1", true, &h.objs, nil)
	a := &attacher{
		g:           g,
		md:          &mapDataplane{objs: &h.objs},
		attached:    map[int]*attachment{},
		special:     map[netip.Addr]bool{},
		warnedLinks: map[string]bool{},
	}
	t.Cleanup(func() {
		for idx := range a.attached {
			a.detach(idx)
		}
	})
	hookOf := func(name string) string {
		for _, at := range a.attached {
			if at.name == name {
				return at.hook
			}
		}
		return ""
	}

	// No physical NICs here: the links are guarded because they are listed.
	a.spec = &Spec{XDP: true, Extra: []string{"agv0", "agd0"}, Dummy: "agdum"}
	a.reconcileLinks(true)
	assert.Equal(t, hookXDP, hookOf("agv0"), "veth supports native XDP")
	assert.Equal(t, hookTCX, hookOf("agd0"), "a dummy refuses native XDP: falls back to tcx")
	assert.Len(t, a.attached, 2)

	// A re-created link gets a new ifindex and must be re-attached.
	old, _ := netlink.LinkByName("agd0")
	require.NoError(t, netlink.LinkDel(old))
	mk(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "agd0"}})
	a.reconcileLinks(false)
	assert.Equal(t, hookTCX, hookOf("agd0"))
	now, _ := netlink.LinkByName("agd0")
	_, attachedNew := a.attached[now.Attrs().Index]
	assert.True(t, attachedNew, "attached to the new ifindex")
	_, attachedOld := a.attached[old.Attrs().Index]
	assert.False(t, attachedOld, "the old ifindex is forgotten")

	// A hook change replaces the attachment. agd0 asked for xdp and got
	// tcx; asked for tcx now, it keeps the tcx link it has -- attaching the
	// same program to the same tcx chain again fails (EEXIST).
	agd0Errors := func() float64 { return testutil.ToFloat64(attachErrors.WithLabelValues("agd0")) }
	errsBefore := agd0Errors()
	a.spec.XDP = false
	a.reconcileLinks(true)
	assert.Equal(t, hookTCX, hookOf("agv0"))
	assert.Equal(t, hookTCX, hookOf("agd0"))
	assert.Equal(t, errsBefore, agd0Errors(), "xdp->tcx on a tcx fallback is not an attach failure")

	// tcx -> xdp on a link without native XDP falls back to the tcx link
	// already there, with no failure; agv0 moves to xdp.
	a.spec.XDP = true
	a.reconcileLinks(true)
	assert.Equal(t, hookXDP, hookOf("agv0"))
	assert.Equal(t, hookTCX, hookOf("agd0"))
	assert.Equal(t, errsBefore, agd0Errors(), "tcx->xdp falling back to tcx is not an attach failure")

	// Something else detaches the guard -- the kernel says nothing: the
	// chain check notices and attaches it again, at both hooks.
	agv0Link, _ := netlink.LinkByName("agv0")
	agd0Link, _ := netlink.LinkByName("agd0")
	require.NoError(t, a.attached[agv0Link.Attrs().Index].link.Detach())
	require.NoError(t, a.attached[agd0Link.Attrs().Index].link.Detach())
	id, err := queryXDP(agv0Link.Attrs().Index)
	require.NoError(t, err)
	require.Zero(t, id, "detached")
	a.retry()
	assert.Equal(t, hookXDP, hookOf("agv0"))
	assert.Equal(t, hookTCX, hookOf("agd0"))
	id, err = queryXDP(agv0Link.Attrs().Index)
	require.NoError(t, err)
	assert.Equal(t, a.xdpID, id, "XDP attached again")
	ids, err := queryTCX(agd0Link.Attrs().Index)
	require.NoError(t, err)
	assert.Equal(t, 1, chainPosition(ids, a.tcxID), "tcx attached again, first")
	assert.Equal(t, errsBefore, agd0Errors())

	a.spec.XDP = false
	a.reconcileLinks(true)
	assert.Equal(t, hookTCX, hookOf("agv0"))
	assert.Equal(t, errsBefore, agd0Errors())

	// A failed config map write leaves the program on the previous mode:
	// under failurePolicy closed the node stands down until a retry works.
	a.spec.Closed = true
	a.setConfig = func(*Spec) error { return errors.New("injected") }
	a.apply(configUpdate{spec: a.spec})
	assert.True(t, g.StandingDown(), "config not written: standing down")
	a.setConfig = nil
	a.retry()
	assert.False(t, g.StandingDown(), "written on the retry: announcing again")
	a.spec.Closed = false
	a.watch.close()
	a.watch = nil

	// Something attached at the head later runs before the guard, and the
	// kernel's own chain query says so.
	agd0, _ := netlink.LinkByName("agd0")
	a.checkChain()
	assert.Equal(t, 1, a.chainPos[agd0.Attrs().Index], "the guard attaches first")
	var other addrguardObjects
	require.NoError(t, loadAddrguardObjects(&other, nil))
	defer other.Close()
	front, err := link.AttachTCX(link.TCXOptions{Interface: agd0.Attrs().Index, Program: other.AgTcx,
		Attach: ebpf.AttachTCXIngress, Anchor: link.Head()})
	require.NoError(t, err)
	a.checkChain()
	assert.Equal(t, 2, a.chainPos[agd0.Attrs().Index], "a program attached at the head runs first")
	assert.Equal(t, 2.0, testutil.ToFloat64(chainPositionVec.WithLabelValues("agd0")))
	require.NoError(t, front.Close())
	a.checkChain()
	assert.Equal(t, 1, a.chainPos[agd0.Attrs().Index], "first again once it goes")

	// Real frames through the kernel, which test runs can't produce: sent
	// from agx1 into agx0's tcx ingress through an AF_PACKET socket (this
	// needs CAP_NET_RAW). A frame larger than a page, with a vnet header
	// whose header length is 14, arrives with only the Ethernet header in
	// the skb's linear area -- the IP header is in a page fragment, as
	// virtio can deliver it. (A frame under a page is built linear, and a
	// veth that has had native XDP linearizes the first 86 bytes: hence a
	// fresh pair with a jumbo MTU.) Every frame gets a verdict from what
	// the guard read.
	mk(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "agx0", MTU: 9000}, PeerName: "agx1", PeerMTU: 9000})
	agx1, _ := netlink.LinkByName("agx1")
	require.NoError(t, netlink.LinkSetUp(agx1))
	a.spec.Extra = []string{"agv0", "agd0", "agx0"}
	a.reconcileLinks(false)
	require.Equal(t, hookTCX, hookOf("agx0"))
	h.addVIP(vip4)
	h.addVIP(vip6)
	h.allowPort(vip4, protoUDP, 80)
	h.allowPort(vip6, protoUDP, 80)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	require.NoError(t, err, "needs CAP_NET_RAW")
	defer unix.Close(fd)
	require.NoError(t, unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_VNET_HDR, 1))
	sa := &unix.SockaddrLinklayer{Ifindex: agx1.Attrs().Index, Halen: 6}
	send := func(pkt []byte, linear uint16) {
		vh := make([]byte, 10) // virtio_net_hdr: hdr_len at offset 2
		binary.LittleEndian.PutUint16(vh[2:], linear)
		require.NoError(t, unix.Sendto(fd, append(vh, pkt...), 0, sa))
	}
	// expect waits for counter to reach want: the frame is received in
	// softirq, after Sendto returns.
	expect := func(counter func() uint64, want uint64, what string) {
		t.Helper()
		assert.Eventually(t, func() bool { return counter() == want }, 2*time.Second, 10*time.Millisecond,
			"%s: got %d, want %d", what, counter(), want)
	}
	reason := func(act, r, fam uint32) func() uint64 { return func() uint64 { return h.reason(act, r, fam) } }
	big := make([]byte, 6000) // more than a page
	udp := func(port uint16) []byte { return append(l4(port)[:8], big...) }

	counted := h.totalReasons()
	send(frame(0x0800, nil, ipv4(client4, vip4, protoUDP, v4opts{}, udp(80))), 14)
	expect(reason(actPass, reasonPortAllowed, famV4), 1, "IPv4, IP header in a fragment, allowed port")
	send(frame(0x0800, nil, ipv4(client4, vip4, protoUDP, v4opts{}, udp(81))), 14)
	expect(reason(actDrop, reasonPortDenied, famV4), 1, "IPv4, IP header in a fragment, denied port")
	send(frame(0x86dd, nil, ipv6(client6, vip6, protoUDP, udp(80))), 14)
	expect(reason(actPass, reasonPortAllowed, famV6), 1, "IPv6, IP header in a fragment, allowed port")
	send(frame(0x86dd, nil, ipv6(client6, vip6, protoUDP, udp(81))), 14)
	expect(reason(actDrop, reasonPortDenied, famV6), 1, "IPv6, IP header in a fragment, denied port")
	send(frame(0x86dd, nil, ipv6(client6, vip6, protoICMPv6, append(icmpMsg(2, 0), big...))), 14)
	expect(reason(actPass, reasonICMP, famV6), 1, "IPv6 packet too big, header in a fragment")
	send(frame(0x0800, nil, ipv4(client4, other4, protoUDP, v4opts{}, udp(81))), 14)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, counted+5, h.totalReasons(), "a non-VIP frame is read and passed, uncounted")

	// QinQ: the kernel moves the outer tag to metadata before tcx, and the
	// inner one is parsed; a third tag is more than QinQ and is dropped.
	send(frame(0x0800, []uint16{100, 200}, ipv4(client4, vip4, protoUDP, v4opts{}, udp(81))), 0)
	expect(reason(actDrop, reasonPortDenied, famV4), 2, "QinQ, one tag in metadata and one in-band, parsed")
	send(frame(0x0800, []uint16{100, 200, 300}, ipv4(client4, vip4, protoUDP, v4opts{}, udp(80))), 0)
	expect(func() uint64 { return h.unread(unreadVLANDepth, false) }, 1, "three tags, dropped unread")

	// A frame too short for its IP header: dropped, never passed unread.
	send(frame(0x0800, nil, make([]byte, 10)), 0)
	expect(func() uint64 { return h.unread(unreadTruncated, false) }, 1, "IPv4 header cut short")
	send(frame(0x86dd, nil, make([]byte, 30)), 0)
	expect(func() uint64 { return h.unread(unreadTruncated, false) }, 2, "IPv6 header cut short")

	// A link with no link-layer header gets ag_tcx_l3, whose packets start
	// with the IP header: a tun device, written to directly.
	tunFd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	require.NoError(t, err)
	defer unix.Close(tunFd)
	var ifr [unix.IFNAMSIZ + 64]byte
	copy(ifr[:], "agt0")
	binary.NativeEndian.PutUint16(ifr[unix.IFNAMSIZ:], unix.IFF_TUN|unix.IFF_NO_PI)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(tunFd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&ifr[0])))
	require.Zero(t, errno)
	tun, err := netlink.LinkByName("agt0")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(tun))
	// And a link the guard can't parse (netlink monitor) is refused, and
	// reported unattached, rather than read wrongly.
	require.NoError(t, netlink.LinkAdd(&netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "agn0"}, LinkType: "nlmon"}))
	a.spec.Extra = []string{"agv0", "agd0", "agx0", "agt0", "agn0"}
	ok, why := a.reconcileLinks(false)
	assert.False(t, ok)
	assert.Contains(t, why, "agn0")
	assert.Equal(t, 1.0, testutil.ToFloat64(unattachedVec.WithLabelValues("agn0")))
	require.Contains(t, a.attached, tun.Attrs().Index)
	assert.True(t, a.attached[tun.Attrs().Index].l3, "tun gets the program for links without a link-layer header")
	_, err = unix.Write(tunFd, ipv4(client4, vip4, protoUDP, v4opts{}, l4(81)))
	require.NoError(t, err)
	expect(reason(actDrop, reasonPortDenied, famV4), 3, "IPv4 on tun, denied port")
	_, err = unix.Write(tunFd, ipv6(client6, vip6, protoUDP, l4(80)))
	require.NoError(t, err)
	expect(reason(actPass, reasonPortAllowed, famV6), 2, "IPv6 on tun, allowed port")
	a.spec.Extra = []string{"agv0", "agd0"}
	a.reconcileLinks(false)
	for _, name := range []string{"agn0", "agx0"} {
		l, _ := netlink.LinkByName(name)
		require.NoError(t, netlink.LinkDel(l))
	}

	// An interface the guard can't attach to is reported as unattached for
	// as long as it is, not just when an attempt fails: here, a second
	// attacher whose tcx program has been closed.
	unattached := func(name string) float64 { return testutil.ToFloat64(unattachedVec.WithLabelValues(name)) }
	assert.Zero(t, testutil.CollectAndCount(unattachedVec), "everything is attached")
	var broken addrguardObjects
	require.NoError(t, loadAddrguardObjects(&broken, nil))
	defer broken.Close()
	require.NoError(t, broken.AgTcx.Close())
	b := &attacher{g: newGuard(log.NewNopLogger(), "node1", true, &broken, nil), md: &mapDataplane{objs: &broken},
		attached: map[int]*attachment{}, special: map[netip.Addr]bool{}, warnedLinks: map[string]bool{},
		spec: &Spec{Extra: []string{"agd0"}, Dummy: "agdum"}}
	ok, why = b.reconcileLinks(false)
	assert.False(t, ok)
	assert.Contains(t, why, "agd0")
	assert.Equal(t, 1.0, unattached("agd0"))
	b.spec = nil
	b.reconcileLinks(false)
	assert.Zero(t, testutil.CollectAndCount(unattachedVec), "disabled: nothing should be attached")
	a.reconcileLinks(false)
	assert.Zero(t, testutil.CollectAndCount(unattachedVec), "the working attacher has everything attached")

	// Exclusion removes a link.
	a.spec.Exclude = []string{"agv0"}
	a.reconcileLinks(true)
	assert.Equal(t, "", hookOf("agv0"))
	assert.Equal(t, hookTCX, hookOf("agd0"))

	// The dummy's kernel-generated broadcast and anycast addresses are
	// guarded with no ports.
	a.reconcileSpecial()
	bcast, anycast := netip.MustParseAddr("10.255.77.255"), netip.MustParseAddr("fd77::")
	assert.True(t, a.special[bcast], "IPv4 subnet broadcast guarded")
	assert.True(t, a.special[anycast], "IPv6 subnet-router anycast guarded")
	var per []addrguardVipCounters
	require.NoError(t, h.objs.AgVip4.Lookup(vip4Key(bcast), &per))
	require.NoError(t, h.objs.AgVip6.Lookup(vip6Key(anycast), &per))

	// A VIP that already holds an address keeps its entry.
	vip := netip.MustParseAddr("10.255.77.200")
	require.NoError(t, a.md.putVIP(vip))
	inserted, err := a.md.putSpecial(vip)
	require.NoError(t, err)
	assert.False(t, inserted, "a special address never overwrites a VIP's entry")

	// Losing the subscription: rewatch replaces it.
	a.watch = newWatcher(g)
	before := a.watch
	a.rewatch()
	assert.NotNil(t, a.watch)
	assert.NotSame(t, before, a.watch)
	a.watch.close()
	a.watch = nil

	// A subscription that failed is retried on the tick.
	a.watch = &watcher{done: make(chan struct{}), failed: true}
	a.retry()
	require.NotNil(t, a.watch)
	assert.False(t, a.watch.failed, "resubscribed")
	a.watch.close()
	a.watch = nil

	// Closing a watcher whose channel filled up -- nobody reading, more
	// than 64 link events -- leaves no netlink goroutine blocked on it.
	settled := func() int { // goroutines from earlier steps may still be exiting
		for prev := -1; ; time.Sleep(100 * time.Millisecond) {
			if n := runtime.NumGoroutine(); n == prev {
				return n
			} else {
				prev = n
			}
		}
	}
	baseline := settled()
	w := newWatcher(g)
	for i := range 60 {
		name := fmt.Sprintf("agf%d", i)
		require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}))
		l, _ := netlink.LinkByName(name)
		require.NoError(t, netlink.LinkDel(l))
	}
	w.close()
	// netlink's goroutines notice the closed socket at their next message;
	// on a node one is never far off. Without the drain, the link
	// goroutine wakes only to block for good on its full channel.
	wake := mk(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "agwake"}})
	wakeAddr, _ := netlink.ParseAddr("10.255.78.1/24")
	require.NoError(t, netlink.AddrAdd(wake, wakeAddr))
	require.NoError(t, netlink.LinkDel(wake))
	// Polled here, not with assert.Eventually: it runs the condition in a
	// goroutine of its own, which NumGoroutine would count.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), baseline, "netlink's goroutines exit once the watcher is closed")

	// Disabled: everything detached and the special entries removed.
	a.spec = nil
	a.reconcileLinks(false)
	a.reconcileSpecial()
	assert.Empty(t, a.attached)
	assert.Empty(t, a.special)
	assert.Error(t, h.objs.AgVip4.Lookup(vip4Key(bcast), &per))
}

// The program for each kind of link. The types and encap names are the
// ones netlink reports for real links of each kind (observed with ip link
// add in a namespace), not this package's constants: Ethernet gets ag_tcx,
// links without a link-layer header ag_tcx_l3, anything else is refused.
func TestLinkLayer(t *testing.T) {
	for _, tc := range []struct {
		link netlink.Link
		l3   bool
		err  bool
	}{
		{&netlink.Device{LinkAttrs: netlink.LinkAttrs{EncapType: "ether"}}, false, false},
		{&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{EncapType: "ether"}}, false, false},
		{&netlink.Veth{LinkAttrs: netlink.LinkAttrs{EncapType: "ether"}}, false, false},
		{&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{EncapType: "ether"}}, false, false},
		{&netlink.Bond{LinkAttrs: netlink.LinkAttrs{EncapType: "ether"}}, false, false},
		{&netlink.Gretap{LinkAttrs: netlink.LinkAttrs{EncapType: "ether"}}, false, false},
		{&netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{EncapType: "ether"}, Mode: netlink.TUNTAP_MODE_TAP}, false, false},
		{&netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{EncapType: "none"}, Mode: netlink.TUNTAP_MODE_TUN}, true, false},
		{&netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{EncapType: "none"}}, true, false},
		{&netlink.Iptun{LinkAttrs: netlink.LinkAttrs{EncapType: "ipip"}}, true, false},
		{&netlink.Gretun{LinkAttrs: netlink.LinkAttrs{EncapType: "gre"}}, true, false},
		{&netlink.Sittun{LinkAttrs: netlink.LinkAttrs{EncapType: "sit"}}, true, false},
		{&netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{EncapType: "tunnel6"}}, true, false},
		{&netlink.Gretun{LinkAttrs: netlink.LinkAttrs{EncapType: "unknown823"}, Local: net.ParseIP("fd00::1")}, true, false},
		{&netlink.IPoIB{LinkAttrs: netlink.LinkAttrs{EncapType: "infiniband"}}, false, true},
		{&netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{EncapType: "unknown824"}, LinkType: "nlmon"}, false, true},
		{&netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{EncapType: "none"}, LinkType: "something-else"}, false, true},
	} {
		l3, err := linkLayer(tc.link)
		name := tc.link.Type() + "/" + tc.link.Attrs().EncapType
		if tc.err {
			assert.Error(t, err, name)
			continue
		}
		require.NoError(t, err, name)
		assert.Equal(t, tc.l3, l3, name)
	}
}
