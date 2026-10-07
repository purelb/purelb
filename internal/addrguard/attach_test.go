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
	"net/netip"
	"os"
	"runtime"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
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

	// A hook change replaces the attachment.
	a.spec.XDP = false
	a.reconcileLinks(true)
	assert.Equal(t, hookTCX, hookOf("agv0"))

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

	// Disabled: everything detached and the special entries removed.
	a.spec = nil
	a.reconcileLinks(false)
	a.reconcileSpecial()
	assert.Empty(t, a.attached)
	assert.Empty(t, a.special)
	assert.Error(t, h.objs.AgVip4.Lookup(vip4Key(bcast), &per))
}
