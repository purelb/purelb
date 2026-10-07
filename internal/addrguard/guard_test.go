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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// fakeDataplane records map writes, in order, and can fail the Nth write.
type fakeDataplane struct {
	calls  []string
	failAt int // 1-based write number to fail; 0 = never
}

func (f *fakeDataplane) do(call string) error {
	f.calls = append(f.calls, call)
	if f.failAt == len(f.calls) {
		return errors.New("map full")
	}
	return nil
}

func (f *fakeDataplane) putVIP(a netip.Addr) error { return f.do("putVIP " + a.String()) }
func (f *fakeDataplane) delVIP(a netip.Addr) error { return f.do("delVIP " + a.String()) }
func (f *fakeDataplane) putPort(a netip.Addr, p portSpec) error {
	return f.do(fmt.Sprintf("putPort %s %d/%d", a, p.proto, p.port))
}
func (f *fakeDataplane) delPort(a netip.Addr, p portSpec) error {
	return f.do(fmt.Sprintf("delPort %s %d/%d", a, p.proto, p.port))
}

func testGuard() (*Guard, *fakeDataplane) {
	g := newGuard(log.NewNopLogger(), "node1", true, nil, nil)
	f := &fakeDataplane{}
	g.dp = f
	return g, f
}

func svc(name string, ips []string, ports ...v1.ServicePort) *v1.Service {
	s := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: ports},
	}
	for _, ip := range ips {
		s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, v1.LoadBalancerIngress{IP: ip})
	}
	return s
}

func port(proto v1.Protocol, p, nodePort int32) v1.ServicePort {
	return v1.ServicePort{Protocol: proto, Port: p, NodePort: nodePort}
}

func TestDesiredFor(t *testing.T) {
	s := desiredFor(svc("a", []string{"2001:db8::1", "192.0.2.1", "192.0.2.1"},
		port(v1.ProtocolTCP, 443, 30443),
		port(v1.ProtocolUDP, 53, 30053),
		port(v1.ProtocolSCTP, 3868, 0),
		port("", 80, 0), // protocol defaults to TCP
	))
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")}, s.vips,
		"both families, de-duplicated")
	assert.Equal(t, []portSpec{{6, 80}, {6, 443}, {17, 53}, {132, 3868}}, s.ports,
		"Service ports only: NodePorts are never allowed on the VIP")
}

func TestGuardWriteOrder(t *testing.T) {
	g, f := testGuard()
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0), port(v1.ProtocolTCP, 443, 0)), nil))
	require.Len(t, f.calls, 3)
	assert.Equal(t, "putVIP 192.0.2.1", f.calls[2], "the VIP key is published only after its ports")
	assert.ElementsMatch(t, []string{"putPort 192.0.2.1 6/80", "putPort 192.0.2.1 6/443"}, f.calls[:2])

	f.calls = nil
	require.NoError(t, g.DeleteBalancer("ns/a", "", nil))
	require.Len(t, f.calls, 3)
	assert.Equal(t, "delVIP 192.0.2.1", f.calls[0], "the VIP key goes before its ports")
	assert.ElementsMatch(t, []string{"delPort 192.0.2.1 6/80", "delPort 192.0.2.1 6/443"}, f.calls[1:])
}

func TestGuardUnchangedServiceIsFree(t *testing.T) {
	g, f := testGuard()
	s := svc("a", []string{"192.0.2.1", "2001:db8::1"}, port(v1.ProtocolTCP, 80, 0))
	require.NoError(t, g.SetBalancer(s, nil))
	f.calls = nil
	require.NoError(t, g.SetBalancer(s, nil))
	assert.Empty(t, f.calls, "SetBalancer runs on every EndpointSlice change; an unchanged Service must cost nothing")
}

func TestGuardPortChange(t *testing.T) {
	g, f := testGuard()
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	f.calls = nil
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 8080, 0)), nil))
	assert.Equal(t, []string{"putPort 192.0.2.1 6/8080", "delPort 192.0.2.1 6/80"}, f.calls,
		"the new port is allowed before the old one is removed")
}

func TestGuardSharedIP(t *testing.T) {
	g, f := testGuard()
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	require.NoError(t, g.SetBalancer(svc("b", []string{"192.0.2.1"}, port(v1.ProtocolUDP, 53, 0)), nil))
	f.calls = nil
	require.NoError(t, g.DeleteBalancer("ns/a", "", nil))
	assert.Equal(t, []string{"delPort 192.0.2.1 6/80"}, f.calls,
		"removing one sharer narrows the port set; the VIP stays guarded for the other")
	f.calls = nil
	require.NoError(t, g.DeleteBalancer("ns/b", "", nil))
	assert.Equal(t, []string{"delVIP 192.0.2.1", "delPort 192.0.2.1 17/53"}, f.calls)
}

func TestGuardIngressIPRemoved(t *testing.T) {
	g, f := testGuard()
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1", "2001:db8::1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	f.calls = nil
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	assert.Equal(t, []string{"delVIP 2001:db8::1", "delPort 2001:db8::1 6/80"}, f.calls)
}

func TestGuardPartialWriteFailsOpenAndRetries(t *testing.T) {
	g, f := testGuard()
	f.failAt = 2 // the second port write fails
	err := g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0), port(v1.ProtocolTCP, 443, 0)), nil)
	require.Error(t, err, "the error drives the existing SyncStateError retry")
	assert.NotContains(t, f.calls, "putVIP 192.0.2.1",
		"a VIP whose ports aren't all written must not be guarded: that would drop real Service ports")
	assert.Equal(t, int64(1), g.failedCount.Load())

	// The retry is the same Service again; it must not be short-circuited.
	f.calls, f.failAt = nil, 0
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0), port(v1.ProtocolTCP, 443, 0)), nil))
	assert.Contains(t, f.calls, "putVIP 192.0.2.1")
	assert.Equal(t, int64(0), g.failedCount.Load())
}

func TestGuardFailureAfterPublishUnguards(t *testing.T) {
	g, f := testGuard()
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	f.calls, f.failAt = nil, 1 // adding the new port fails
	require.Error(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0), port(v1.ProtocolTCP, 443, 0)), nil))
	assert.Equal(t, []string{"putPort 192.0.2.1 6/443", "delVIP 192.0.2.1"}, f.calls,
		"a published VIP that can't get its new port fails open rather than dropping it")
}

// gauge reads one unguarded_vips series from the collector.
func unguarded(t *testing.T, g *Guard, reason string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(collector{g}))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "purelb_address_guard_unguarded_vips" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelValue(m, "reason") == reason {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("no unguarded_vips{reason=%q}", reason)
	return 0
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func TestUnguardedVIPs(t *testing.T) {
	g, f := testGuard() // objs nil: the program isn't loaded
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1", "2001:db8::1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	assert.Equal(t, 0.0, unguarded(t, g, "not_loaded"), "not configured: nothing is claimed")

	g.configured.Store(true)
	assert.Equal(t, 2.0, unguarded(t, g, "not_loaded"), "configured but not loaded: every VIP is unguarded")

	f.calls, f.failAt = nil, 1
	require.Error(t, g.SetBalancer(svc("b", []string{"192.0.2.2"}, port(v1.ProtocolTCP, 80, 0)), nil))
	assert.Equal(t, 1.0, unguarded(t, g, "map_write_failed"))
}

func TestSpecFor(t *testing.T) {
	spec, _ := specFor(nil)
	assert.Nil(t, spec)

	agent := func(local *purelbv2.LBNodeAgentLocalSpec) *purelbv2.Config {
		return &purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{{Spec: purelbv2.LBNodeAgentSpec{Local: local}}}}
	}
	spec, _ = specFor(agent(&purelbv2.LBNodeAgentLocalSpec{}))
	assert.Nil(t, spec, "an agent without addressGuard means disabled")

	spec, got := specFor(agent(&purelbv2.LBNodeAgentLocalSpec{AddressGuard: &purelbv2.AddressGuardConfig{
		Mode: "monitor", Hook: "xdp", AllowedProtocols: []int32{47, 50},
		ExtraInterfaces: []string{"wg0"}, ExcludeInterfaces: []string{"eth2"},
	}}))
	require.NotNil(t, spec)
	assert.NotNil(t, got)
	assert.Equal(t, &Spec{Monitor: true, XDP: true, Closed: true, AllowedProtocols: []uint8{47, 50},
		Extra: []string{"wg0"}, Exclude: []string{"eth2"}, Dummy: "kube-lb0"}, spec)

	spec, _ = specFor(agent(&purelbv2.LBNodeAgentLocalSpec{DummyInterface: "lb9", AddressGuard: &purelbv2.AddressGuardConfig{}}))
	assert.Equal(t, &Spec{Closed: true, Dummy: "lb9"}, spec, "defaults: enforce, tcx, fail-closed")

	spec, _ = specFor(agent(&purelbv2.LBNodeAgentLocalSpec{AddressGuard: &purelbv2.AddressGuardConfig{FailurePolicy: "open"}}))
	assert.False(t, spec.Closed, "failurePolicy: open")
	spec, _ = specFor(agent(&purelbv2.LBNodeAgentLocalSpec{AddressGuard: &purelbv2.AddressGuardConfig{FailurePolicy: "closed"}}))
	assert.True(t, spec.Closed, "failurePolicy: closed")
}

func TestSetConfigNeverBlocks(t *testing.T) {
	g, _ := testGuard() // no attacher running: nothing drains cfgCh
	on := &purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{{Spec: purelbv2.LBNodeAgentSpec{Local: &purelbv2.LBNodeAgentLocalSpec{
		AddressGuard: &purelbv2.AddressGuardConfig{Mode: "monitor"},
	}}}}}
	require.NoError(t, g.SetConfig(on))
	require.NoError(t, g.SetConfig(&purelbv2.Config{}))
	require.NoError(t, g.SetConfig(on))
	u := <-g.cfgCh
	require.NotNil(t, u.spec, "the latest config wins")
	assert.True(t, u.spec.Monitor)
	assert.True(t, g.configured.Load())
}

// --- attach rule -----------------------------------------------------

func fakeLink(t string, idx int, name string, mutate ...func(*netlink.LinkAttrs)) netlink.Link {
	attrs := netlink.LinkAttrs{Index: idx, Name: name, EncapType: "ether"}
	for _, m := range mutate {
		m(&attrs)
	}
	switch t {
	case "device":
		return &netlink.Device{LinkAttrs: attrs}
	case "bond":
		return &netlink.Bond{LinkAttrs: attrs}
	case "vlan":
		return &netlink.Vlan{LinkAttrs: attrs}
	case "vxlan":
		return &netlink.Vxlan{LinkAttrs: attrs}
	case "bridge":
		return &netlink.Bridge{LinkAttrs: attrs}
	case "dummy":
		return &netlink.Dummy{LinkAttrs: attrs}
	case "veth":
		return &netlink.Veth{LinkAttrs: attrs}
	}
	return &netlink.GenericLink{LinkAttrs: attrs, LinkType: t}
}

func names(m map[int]netlink.Link) []string {
	var n []string
	for _, l := range m {
		n = append(n, l.Attrs().Name)
	}
	return n
}

func loopback(a *netlink.LinkAttrs) { a.Flags |= net.FlagLoopback; a.EncapType = "loopback" }

// The prox-purelb2 inventory, as `ip -d link` reported it on 2026-10-03.
func TestSelectLinksTestCluster(t *testing.T) {
	links := []netlink.Link{
		fakeLink("device", 1, "lo", loopback),
		fakeLink("device", 2, "eth1"),
		fakeLink("device", 3, "eth0"),
		fakeLink("vxlan", 4, "flannel.1"),
		fakeLink("vxlan", 5, "flannel-v6.1"),
		fakeLink("bridge", 6, "cni0"),
		fakeLink("dummy", 7, "kube-lb0"),
		fakeLink("veth", 8, "veth9f2c1a"),
	}
	sel, excluded := selectLinks(links, map[int]bool{2: true}, nil, nil)
	assert.ElementsMatch(t, []string{"eth0", "eth1"}, names(sel),
		"uplinks only: tunnels, CNI bridges, the dummy, veths and lo are reachable only by those who can already reach node IPs")
	assert.Empty(t, excluded)
}

func TestSelectLinksRules(t *testing.T) {
	bondSlave := func(a *netlink.LinkAttrs) { a.MasterIndex = 10 }
	vlanOnEth0 := func(a *netlink.LinkAttrs) { a.ParentIndex = 2 }
	links := []netlink.Link{
		fakeLink("device", 1, "lo", loopback),
		fakeLink("device", 2, "eth0"),
		fakeLink("device", 3, "eth1", bondSlave),
		fakeLink("device", 4, "eth2", bondSlave),
		fakeLink("bond", 10, "bond0"),
		fakeLink("vlan", 11, "eth0.100", vlanOnEth0),
		fakeLink("veth", 12, "eth9"), // a kind node's uplink is a veth
		fakeLink("wireguard", 13, "wg0", func(a *netlink.LinkAttrs) { a.EncapType = "none" }),
		fakeLink("ipoib", 14, "ib0"),
	}

	sel, _ := selectLinks(links, map[int]bool{11: true}, nil, nil)
	assert.ElementsMatch(t, []string{"eth0", "bond0"}, names(sel),
		"the bond replaces its slaves; a default-route VLAN on a guarded parent is covered by the parent")

	sel, _ = selectLinks(links, map[int]bool{12: true}, nil, nil)
	assert.Contains(t, names(sel), "eth9", "a default-route link is guarded whatever its kind")

	sel, excluded := selectLinks(links, map[int]bool{}, []string{"wg0", "ib0", "missing0", "lo"}, []string{"eth0", "absent9"})
	assert.ElementsMatch(t, []string{"bond0", "wg0", "ib0"}, names(sel),
		"extra adds (never lo; missing names skipped), exclude removes")
	assert.Equal(t, []string{"eth0"}, excluded, "only excluded names that exist are reported")
}

func TestWantHook(t *testing.T) {
	a := &attacher{spec: &Spec{XDP: true}}
	assert.Equal(t, hookXDP, a.wantHook(fakeLink("device", 2, "eth0")))
	assert.Equal(t, hookTCX, a.wantHook(fakeLink("wireguard", 3, "wg0", func(l *netlink.LinkAttrs) { l.EncapType = "none" })),
		"XDP would misread an L3 device's packets")
	a.spec.XDP = false
	assert.Equal(t, hookTCX, a.wantHook(fakeLink("device", 2, "eth0")))
}

func TestSpecialRouteEvent(t *testing.T) {
	ev := func(msg uint16, table, typ int) netlink.RouteUpdate {
		return netlink.RouteUpdate{Type: msg, Route: netlink.Route{Table: table, Type: typ}}
	}
	assert.True(t, specialRouteEvent(ev(unix.RTM_NEWROUTE, unix.RT_TABLE_LOCAL, unix.RTN_BROADCAST)))
	assert.True(t, specialRouteEvent(ev(unix.RTM_DELROUTE, unix.RT_TABLE_LOCAL, unix.RTN_ANYCAST)))
	assert.False(t, specialRouteEvent(ev(unix.RTM_NEWROUTE, unix.RT_TABLE_LOCAL, unix.RTN_LOCAL)), "a VIP's own local route")
	assert.False(t, specialRouteEvent(ev(unix.RTM_NEWROUTE, unix.RT_TABLE_MAIN, unix.RTN_UNICAST)))
}

var (
	closedCfg = &purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{{Spec: purelbv2.LBNodeAgentSpec{Local: &purelbv2.LBNodeAgentLocalSpec{
		AddressGuard: &purelbv2.AddressGuardConfig{}}}}}}
	openCfg = &purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{{Spec: purelbv2.LBNodeAgentSpec{Local: &purelbv2.LBNodeAgentLocalSpec{
		AddressGuard: &purelbv2.AddressGuardConfig{FailurePolicy: "open"}}}}}}
)

// hookLog records what the stand-down hook was called with.
func hookLog(g *Guard) *[]bool {
	got := &[]bool{}
	g.SetStandDownHook(func(down bool) { *got = append(*got, down) })
	return got
}

// takeConfig hands the attacher the update SetConfig queued, as run does.
func (a *attacher) takeConfig() { a.apply(<-a.g.cfgCh) }

func TestStandingDown(t *testing.T) {
	g, _ := testGuard()
	a := &attacher{g: g, warnedLinks: map[string]bool{}}
	assert.False(t, g.StandingDown(), "not configured")

	require.NoError(t, g.SetConfig(closedCfg))
	assert.True(t, g.StandingDown(), "at startup fail-closed stands down from the moment it is configured until it has attached")
	a.takeConfig() // not loaded
	assert.True(t, g.StandingDown(), "configured fail-closed, not running")
	a.publish(true, "")
	assert.False(t, g.StandingDown(), "attached")
	a.publish(false, "could not attach to eth1")
	assert.True(t, g.StandingDown(), "an interface lost its guard")

	require.NoError(t, g.SetConfig(openCfg))
	a.takeConfig()
	assert.False(t, g.StandingDown(), "fail-open never stands down")
	require.NoError(t, g.SetConfig(&purelbv2.Config{}))
	a.takeConfig()
	assert.False(t, g.StandingDown(), "unconfigured")
}

// Turning the guard on live used to stand the node down from SetConfig,
// before the attacher had tried, while the hook only heard what the
// attacher published -- it never heard of that stand-down, so Services
// withdrawn in it stayed withdrawn until the next informer resync. After the
// first apply, only the attacher decides, and it reports every change.
func TestLiveChangesStandDownOnlyIfTheAttacherFails(t *testing.T) {
	g, _ := testGuard()
	got := hookLog(g)
	a := &attacher{g: g, warnedLinks: map[string]bool{}}
	require.NoError(t, g.SetConfig(&purelbv2.Config{}))
	a.takeConfig() // started with the guard off

	require.NoError(t, g.SetConfig(closedCfg))
	assert.False(t, g.StandingDown(), "off -> on: VIPs stay announced while the attacher tries")
	a.publish(true, "") // attached
	assert.False(t, g.StandingDown())
	assert.Empty(t, *got, "nothing changed, nothing to re-sync")

	// open -> closed while the guard isn't working: the node stands down
	// only once the attacher has looked, and the hook hears both edges.
	require.NoError(t, g.SetConfig(openCfg))
	a.takeConfig() // not loaded: fail-open announces anyway
	require.NoError(t, g.SetConfig(closedCfg))
	assert.False(t, g.StandingDown())
	a.takeConfig()
	assert.True(t, g.StandingDown())
	a.publish(true, "")
	assert.Equal(t, []bool{true, false}, *got)
}

// At startup SetConfig stands a fail-closed node down before anything is
// attached; the hook must hear when that ends, though the attacher never
// published the stand-down itself.
func TestStartupStandDownEndReachesTheHook(t *testing.T) {
	g, _ := testGuard()
	got := hookLog(g)
	a := &attacher{g: g, warnedLinks: map[string]bool{}}

	require.NoError(t, g.SetConfig(closedCfg))
	assert.True(t, g.StandingDown())
	<-g.cfgCh
	a.spec = &Spec{Closed: true}
	a.publish(true, "") // attached
	assert.False(t, g.StandingDown())
	assert.Equal(t, []bool{false}, *got, "the end of the startup stand-down re-syncs Services")
}

func TestFailClosedKeepsAFailedVIPFiltered(t *testing.T) {
	g, f := testGuard()
	g.closed.Store(true)
	f.failAt = 2 // the second port write fails
	require.Error(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0), port(v1.ProtocolTCP, 443, 0)), nil))
	assert.Contains(t, f.calls, "putVIP 192.0.2.1",
		"fail-closed publishes the VIP even with a port missing: blocking a Service port until the retry beats exposing the host")
	assert.True(t, g.installedVIP[netip.MustParseAddr("192.0.2.1")])

	f.calls, f.failAt = nil, 1 // a later port add fails on a published VIP
	require.Error(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0), port(v1.ProtocolTCP, 8443, 0)), nil))
	assert.NotContains(t, f.calls, "delVIP 192.0.2.1", "fail-closed never unguards a VIP to get around a write failure")
}

func TestCollectorUnderFailClosed(t *testing.T) {
	g, _ := testGuard() // not loaded
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	require.NoError(t, g.SetConfig(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{{Spec: purelbv2.LBNodeAgentSpec{
		Local: &purelbv2.LBNodeAgentLocalSpec{AddressGuard: &purelbv2.AddressGuardConfig{}}}}}}))
	assert.Equal(t, 0.0, unguarded(t, g, "not_loaded"), "fail-closed stands down instead of exposing the VIP")

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(collector{g}))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var down float64 = -1
	for _, mf := range mfs {
		if mf.GetName() == "purelb_address_guard_standing_down" {
			down = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	assert.Equal(t, 1.0, down)
}

// publish drives the node agent's hook: standing down while the guard isn't
// working under fail-closed, back up once it is -- on changes only, since
// every call re-syncs every Service.
func TestPublishDrivesTheStandDownHook(t *testing.T) {
	g, _ := testGuard()
	got := hookLog(g)
	a := &attacher{g: g, spec: &Spec{Closed: true}, warnedLinks: map[string]bool{}}

	a.publish(false, "could not attach to eth1")
	a.publish(false, "could not attach to eth1")
	a.publish(true, "")
	a.publish(true, "")
	assert.Equal(t, []bool{true, false}, *got)
	assert.False(t, g.StandingDown())
}

// gauge reads an unlabelled gauge from g's collector.
func gauge(t *testing.T, g *Guard, name string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(collector{g}))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no %s", name)
	return 0
}

func TestEnabled(t *testing.T) {
	agent := func(name string, sel map[string]string, local *purelbv2.LBNodeAgentLocalSpec) *purelbv2.LBNodeAgent {
		a := &purelbv2.LBNodeAgent{ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: name},
			Spec: purelbv2.LBNodeAgentSpec{Local: local}}
		if sel != nil {
			a.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: sel}
		}
		return a
	}
	guarded := &purelbv2.LBNodeAgentLocalSpec{AddressGuard: &purelbv2.AddressGuardConfig{}}
	plain := &purelbv2.LBNodeAgentLocalSpec{}

	assert.False(t, Enabled(nil, nil), "no LBNodeAgent")
	assert.False(t, Enabled([]*purelbv2.LBNodeAgent{agent("default", nil, plain)}, nil), "no addressGuard")
	assert.True(t, Enabled([]*purelbv2.LBNodeAgent{agent("default", nil, guarded)}, nil))

	// A node-specific agent wins over the catch-all, as in every config
	// delivery: the guard is on for zone b only.
	agents := []*purelbv2.LBNodeAgent{agent("default", nil, guarded), agent("zone-a", map[string]string{"zone": "a"}, plain)}
	assert.False(t, Enabled(agents, map[string]string{"zone": "a"}))
	assert.True(t, Enabled(agents, map[string]string{"zone": "b"}))

	// An agent with only a nodeSelector is skipped for the local spec.
	agents = []*purelbv2.LBNodeAgent{agent("default", nil, guarded), agent("labels-only", map[string]string{"zone": "a"}, nil)}
	assert.True(t, Enabled(agents, map[string]string{"zone": "a"}))
}

// A guard that isn't configured at startup loads nothing into the kernel.
func TestNewAnnouncerLoadsOnlyWhenEnabled(t *testing.T) {
	calls := 0
	defer func(orig func() (*addrguardObjects, error)) { loadProgram = orig }(loadProgram)
	loadProgram = func() (*addrguardObjects, error) { calls++; return nil, errors.New("not in a unit test") }

	g := NewAnnouncer(log.NewNopLogger(), "node1", false)
	g.Shutdown()
	prometheus.Unregister(collector{g})
	assert.Zero(t, calls, "not configured at startup: the program is not loaded")
	assert.False(t, g.loadWanted)

	g = NewAnnouncer(log.NewNopLogger(), "node1", true)
	g.Shutdown()
	prometheus.Unregister(collector{g})
	assert.Equal(t, 1, calls, "configured at startup: loaded")
}

// Enabled after startup: nothing is loaded, so nothing is filtered until the
// agent restarts, and it says so. The node keeps announcing whatever the
// failure policy -- standing down would turn an LBNodeAgent edit into every
// node withdrawing at once.
func TestEnabledAfterStartupWaitsForARestart(t *testing.T) {
	g := newGuard(log.NewNopLogger(), "node1", false, nil, nil)
	g.dp = &fakeDataplane{}
	got := hookLog(g)
	a := &attacher{g: g, warnedLinks: map[string]bool{}}
	require.NoError(t, g.SetBalancer(svc("a", []string{"192.0.2.1", "2001:db8::1"}, port(v1.ProtocolTCP, 80, 0)), nil))
	require.NoError(t, g.SetConfig(&purelbv2.Config{}))
	a.takeConfig()
	assert.Zero(t, gauge(t, g, "purelb_address_guard_restart_required"))

	require.NoError(t, g.SetConfig(closedCfg))
	assert.False(t, g.StandingDown(), "SetConfig never stands down a guard that isn't loaded")
	a.takeConfig()
	assert.False(t, g.StandingDown(), "fail-closed, but not loaded by choice: keep announcing")
	assert.Empty(t, *got)
	assert.Equal(t, 1.0, gauge(t, g, "purelb_address_guard_restart_required"))
	assert.Equal(t, 2.0, unguarded(t, g, "restart_pending"), "every VIP is unfiltered until the restart")
	assert.Zero(t, unguarded(t, g, "not_loaded"), "not a load failure")

	require.NoError(t, g.SetConfig(&purelbv2.Config{}))
	a.takeConfig()
	assert.Zero(t, gauge(t, g, "purelb_address_guard_restart_required"), "back to what was loaded")
	assert.Zero(t, unguarded(t, g, "restart_pending"))

	// Enabled between the startup read and the first config delivery:
	// still not loaded, so still no stand-down.
	g = newGuard(log.NewNopLogger(), "node1", false, nil, nil)
	require.NoError(t, g.SetConfig(closedCfg))
	assert.False(t, g.StandingDown(), "the first config never stands down a guard that isn't loaded")
}

// Disabled after startup: it detaches at once, but the program stays loaded
// until the agent restarts, and it says so.
func TestDisabledAfterStartupNeedsARestartToUnload(t *testing.T) {
	g, _ := testGuard() // configured at startup
	a := &attacher{g: g, warnedLinks: map[string]bool{}}
	require.NoError(t, g.SetConfig(closedCfg))
	a.takeConfig()
	assert.Zero(t, gauge(t, g, "purelb_address_guard_restart_required"))

	require.NoError(t, g.SetConfig(&purelbv2.Config{}))
	a.takeConfig()
	assert.False(t, g.StandingDown())
	assert.Equal(t, 1.0, gauge(t, g, "purelb_address_guard_restart_required"))

	require.NoError(t, g.SetConfig(closedCfg))
	a.takeConfig()
	assert.Zero(t, gauge(t, g, "purelb_address_guard_restart_required"), "re-enabled: the loaded program is wanted again")
}

func TestChainPosition(t *testing.T) {
	assert.Equal(t, 1, chainPosition([]ebpf.ProgramID{7, 9}, 7))
	assert.Equal(t, 3, chainPosition([]ebpf.ProgramID{4, 9, 7}, 7))
	assert.Zero(t, chainPosition([]ebpf.ProgramID{4, 9}, 7), "not attached")
}

// checkChain reports the guard's place in each tcx chain, and says so when
// something runs in front of it: nothing else would notice.
func TestCheckChainReportsProgramsInFront(t *testing.T) {
	var logs strings.Builder
	g := newGuard(log.NewLogfmtLogger(&logs), "node1", true, &addrguardObjects{}, nil)
	a := &attacher{g: g, tcxID: 7, attached: map[int]*attachment{
		3: {name: "eth1-chain", hook: hookTCX},
		4: {name: "eth2-chain", hook: hookXDP},
	}}
	chain := []ebpf.ProgramID{7}
	defer func(orig func(int) ([]ebpf.ProgramID, error)) { queryTCX = orig }(queryTCX)
	queryTCX = func(ifindex int) ([]ebpf.ProgramID, error) {
		require.Equal(t, 3, ifindex, "XDP has one slot: no chain to query")
		return chain, nil
	}
	pos := func() float64 { return testutil.ToFloat64(chainPositionVec.WithLabelValues("eth1-chain")) }

	a.checkChain()
	assert.Equal(t, 1.0, pos())
	assert.NotContains(t, logs.String(), "guardNotFirst")

	chain = []ebpf.ProgramID{12, 9, 7}
	a.checkChain()
	assert.Equal(t, 3.0, pos())
	assert.Contains(t, logs.String(), "event=guardNotFirst interface=eth1-chain position=3 before=\"[12 9]\"")
	logs.Reset()
	a.checkChain()
	assert.Empty(t, logs.String(), "reported on a change, not on every check")

	chain = []ebpf.ProgramID{7, 12}
	a.checkChain()
	assert.Equal(t, 1.0, pos())
	assert.Contains(t, logs.String(), "event=guardFirstAgain interface=eth1-chain")

	a.attached[3].hook = hookXDP // switched to XDP: the series goes
	a.checkChain()
	assert.Zero(t, testutil.CollectAndCount(chainPositionVec, "purelb_address_guard_chain_position"))
}
