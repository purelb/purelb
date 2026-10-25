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

package local

import (
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egresshook"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// placeSandbox locks the test to a thread in a new network namespace with
// a pool interface, eth1 (192.0.2.1/24, 2001:db8:1::1/64, the default
// routes), and the dummy interface; it skips without CAP_SYS_ADMIN unless
// PURELB_NETNS_TESTS=required. The announcer's netlink calls run on this
// thread, so they stay in the namespace; GARP is off, since its sequence
// runs on another thread.
func placeSandbox(t *testing.T) (*announcer, netlink.Link, netlink.Link) {
	t.Helper()
	runtime.LockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	ns, err := netns.New()
	if err != nil {
		_ = orig.Close()
		runtime.UnlockOSThread()
		if os.Getenv("PURELB_NETNS_TESTS") == "required" {
			t.Fatalf("creating a network namespace: %v", err)
		}
		t.Skipf("creating a network namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = netns.Set(orig)
		_ = ns.Close()
		_ = orig.Close()
		runtime.UnlockOSThread()
	})
	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1"}}))
	eth1, err := netlink.LinkByName("eth1")
	require.NoError(t, err)
	for _, a := range []string{"192.0.2.1/24", "2001:db8:1::1/64"} {
		addr, err := netlink.ParseAddr(a)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, netlink.AddrAdd(eth1, addr))
	}
	require.NoError(t, netlink.LinkSetUp(eth1))
	for _, gw := range []string{"192.0.2.254", "2001:db8:1::fe"} {
		require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth1.Attrs().Index, Gw: net.ParseIP(gw)}))
	}
	dummy, err := addDummyInterface("kube-lb0")
	require.NoError(t, err)
	a := &announcer{logger: log.NewNopLogger(), myNode: "n1"}
	a.cfg.Store(&announcerConfig{spec: &purelbv2.LBNodeAgentLocalSpec{DummyInterface: "kube-lb0"}, dummyInt: dummy})
	return a, eth1, dummy
}

// addrOn is ip's address on link, nil if it has none.
func addrOn(t *testing.T, link netlink.Link, ip string) *netlink.Addr {
	t.Helper()
	list, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	require.NoError(t, err)
	for i := range list {
		if list[i].IP.Equal(net.ParseIP(ip)) {
			return &list[i]
		}
	}
	return nil
}

// Egress's addresses go through the VIP path (D31): a local one on the
// interface a Service's would use, with the local lifetimes, IPv6
// deprecated and DAD as asked; a routed one on the dummy interface with
// its pool's prefix. Each is renewed and counted under its own key, so a
// Service's withdrawal can't take it, and its own withdrawal leaves an
// address a Service announces.
func TestNetnsPlaceEgress(t *testing.T) {
	a, eth1, dummy := placeSandbox(t)

	idx, name, err := a.EgressInterface(net.ParseIP("192.0.2.10"))
	require.NoError(t, err)
	assert.Equal(t, eth1.Attrs().Index, idx)
	assert.Equal(t, "eth1", name)
	_, _, err = a.EgressInterface(net.ParseIP("198.51.100.5"))
	assert.Error(t, err, "no local interface for it")

	require.NoError(t, a.PlaceEgress("egress/u4", net.ParseIP("192.0.2.10"), egresshook.EgressPlacement{}))
	v4 := addrOn(t, eth1, "192.0.2.10")
	require.NotNil(t, v4)
	ones, _ := v4.Mask.Size()
	assert.Equal(t, 24, ones)
	assert.Equal(t, 300, v4.ValidLft, "the local lifetime, renewed")
	assert.NotZero(t, v4.Flags&unix.IFA_F_NOPREFIXROUTE)
	_, renewing := a.addressRenewals.Load("egress/u4:192.0.2.10")
	assert.True(t, renewing)

	require.NoError(t, a.PlaceEgress("egress/u6", net.ParseIP("2001:db8:1::10"), egresshook.EgressPlacement{SkipDAD: true}))
	v6 := addrOn(t, eth1, "2001:db8:1::10")
	require.NotNil(t, v6)
	assert.NotZero(t, v6.Flags&unix.IFA_F_DEPRECATED, "never a source")
	assert.NotZero(t, v6.Flags&unix.IFA_F_NODAD)
	assert.Zero(t, v6.Flags&unix.IFA_F_TENTATIVE)

	require.NoError(t, a.PlaceEgress("egress/r4", net.ParseIP("198.51.100.5"),
		egresshook.EgressPlacement{Routed: true, Subnet: "198.51.100.0/24", Aggregation: "/32"}))
	r4 := addrOn(t, dummy, "198.51.100.5")
	require.NotNil(t, r4, "host-route mode")
	ones, _ = r4.Mask.Size()
	assert.Equal(t, 32, ones)
	require.NoError(t, a.PlaceEgress("egress/r6", net.ParseIP("2001:db8:9::5"),
		egresshook.EgressPlacement{Routed: true, Subnet: "2001:db8:9::/64", Aggregation: "default"}))
	r6 := addrOn(t, dummy, "2001:db8:9::5")
	require.NotNil(t, r6, "aggregate mode")
	ones, _ = r6.Mask.Size()
	assert.Equal(t, 64, ones)
	assert.NotZero(t, r6.Flags&unix.IFA_F_DEPRECATED)

	// A Service announcing the address too keeps it.
	a.announced.Store(renewalKey("app/web", "192.0.2.10"), struct{}{})
	require.NoError(t, a.WithdrawEgress("egress/u4", net.ParseIP("192.0.2.10")))
	assert.NotNil(t, addrOn(t, eth1, "192.0.2.10"))
	_, renewing = a.addressRenewals.Load("egress/u4:192.0.2.10")
	assert.False(t, renewing)
	a.announced.Delete(renewalKey("app/web", "192.0.2.10"))

	for key, ip := range map[string]string{"egress/u4": "192.0.2.10", "egress/u6": "2001:db8:1::10",
		"egress/r4": "198.51.100.5", "egress/r6": "2001:db8:9::5"} {
		require.NoError(t, a.WithdrawEgress(key, net.ParseIP(ip)))
		assert.Nil(t, addrOn(t, eth1, ip), ip)
		assert.Nil(t, addrOn(t, dummy, ip), ip)
	}
	n := 0
	a.addressRenewals.Range(func(any, any) bool { n++; return true })
	assert.Zero(t, n, "no renewal left")
}
