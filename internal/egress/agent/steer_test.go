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
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/egresshook"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	listers "purelb.io/pkg/generated/listers/purelb/v2"
)

// steerElector answers the steering view: live nodes, and a winner per
// preferred set, recording for each preferred set whether it was asked
// with an eligibility test.
type steerElector struct {
	live     []string
	winner   func(preferred []string) string
	eligible map[string]bool
}

func (e *steerElector) WinnerAmong(string, func(string) bool, []string) string { return "" }
func (e *steerElector) SteeringWinner(_ string, eligible func(string) bool, preferred []string) string {
	if e.eligible == nil {
		e.eligible = map[string]bool{}
	}
	e.eligible[strings.Join(preferred, ",")] = eligible != nil
	return e.winner(preferred)
}
func (e *steerElector) SteeringLive() []string       { return e.live }
func (e *steerElector) LastRenewal() time.Time       { return time.Time{} }
func (e *steerElector) LeaseDuration() time.Duration { return 10 * time.Second }

func steerAgent(t *testing.T, e Elector, eas ...*purelbv2.EgressAddress) (*Agent, *session, *int) {
	t.Helper()
	a := New(log.NewNopLogger(), "a", "purelb-system")
	a.SetElection(e)
	kicks := 0
	f := func(string, bool) { kicks++ }
	a.enqueue.Store(&f)
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, ea := range eas {
		require.NoError(t, idx.Add(ea))
	}
	edge := withLabel("edge", "true")
	s := &session{addresses: listers.NewEgressAddressLister(idx).EgressAddresses("purelb-system"),
		nodes: nodeIndexer(t, map[string]map[string]string{"a": edge, "b": edge, "c": capable, "d": {"edge": "true"}})}
	a.config.Store(&delivery{groups: map[string]*purelbv2.ServiceGroup{
		"edge": group(&metav1.LabelSelector{MatchLabels: map[string]string{"edge": "true"}})}})
	return a, s, &kicks
}

func steerAddress(name, addr, poolType string, shared bool, preferred ...string) *purelbv2.EgressAddress {
	ea := &purelbv2.EgressAddress{ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: name},
		Spec: purelbv2.EgressAddressSpec{State: "Active", Address: addr, ServiceGroup: "edge", PoolType: poolType,
			PreferredNodes: preferred, PoolMTU: 1400}}
	if shared {
		ea.Spec.SharedWith = &purelbv2.EgressServiceRef{Name: "web"}
	}
	return ea
}

// Each slot steers to its address's holders in the steering view (§5.5):
// a local address's election winner; a routed one's live advertisers;
// a shared one's nodes as its Service's LB path places it. Only
// egress-capable nodes hold; d isn't.
func TestSteering(t *testing.T) {
	local, routed := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	sharedLocal, sharedRemote := netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("2001:db8::4")
	e := &steerElector{live: []string{"a", "b", "c", "d"}, winner: func(p []string) string {
		if len(p) > 0 {
			return p[0]
		}
		return "c"
	}}
	a, s, _ := steerAgent(t, e,
		steerAddress("l", "192.0.2.1", "Local", false, "b"),
		steerAddress("r", "192.0.2.2", "Routed", false),
		steerAddress("sl", "192.0.2.3", "Local", true),
		steerAddress("sr", "2001:db8::4", "Routed", true))
	a.ServicePlacement("app/web", sharedLocal.AsSlice(), &egresshook.ServicePlacement{Preferred: []string{"d"}})
	a.ServicePlacement("app/web", net.IP(sharedRemote.AsSlice()), &egresshook.ServicePlacement{Remote: true, ETPLocal: true,
		EndpointNodes: []string{"b", "d"}})
	a.ServicePlacement("app/web2", net.IP(sharedRemote.AsSlice()), &egresshook.ServicePlacement{Remote: true, ETPLocal: true,
		EndpointNodes: []string{"c"}})
	stage := &datapath.PodStage{Slots: map[uint8]datapath.Slot{1: {Addr: local}, 2: {Addr: routed}, 3: {Addr: sharedLocal},
		4: {Addr: sharedRemote}, 5: {Addr: netip.MustParseAddr("192.0.2.9")}}}

	got := a.steering(s, stage)
	assert.Equal(t, dpSteer{addr: local, holders: []string{"b"}, mtu: 1400}, got[1], "the winner, preferring the preferred nodes")
	assert.Equal(t, dpSteer{addr: routed, holders: []string{"a", "b"}, routed: true, mtu: 1400}, got[2],
		"the live selector nodes; c isn't selected, d isn't capable")
	assert.Empty(t, got[3].holders, "the Service's election picks d, which can't hold an egress address")
	assert.Equal(t, map[string]bool{"b": true, "d": false}, e.eligible,
		"the dedicated address's election has egress eligibility, the Service's none")
	assert.Equal(t, []string{"b", "c"}, got[4].holders, "both Services' endpoint nodes, capable ones")
	assert.Equal(t, dpSteer{addr: netip.MustParseAddr("192.0.2.9")}, got[5], "no EgressAddress: no holder")

	// The Service's election moves; and the routed advertisers' leases.
	a.ServicePlacement("app/web", sharedLocal.AsSlice(), &egresshook.ServicePlacement{})
	e.live = []string{"a"}
	got = a.steering(s, stage)
	assert.Equal(t, []string{"c"}, got[3].holders)
	assert.Equal(t, []string{"a"}, got[2].holders)
	assert.Empty(t, got[4].holders, "no live endpoint node")
}

// A placement report kicks the datapath only when it changes; nil drops
// it.
func TestServicePlacementReports(t *testing.T) {
	a, _, kicks := steerAgent(t, &steerElector{})
	ip := net.ParseIP("192.0.2.3")
	p := &egresshook.ServicePlacement{Preferred: []string{"b"}}
	a.ServicePlacement("app/web", ip, p)
	a.ServicePlacement("app/web", ip, &egresshook.ServicePlacement{Preferred: []string{"b"}})
	assert.Equal(t, 1, *kicks, "a repeat changes nothing")
	p.Preferred[0] = "x"
	assert.Equal(t, []string{"b"}, a.placements[netip.MustParseAddr("192.0.2.3")]["app/web"].Preferred, "kept as a copy")
	a.ServicePlacement("app/web", ip, &egresshook.ServicePlacement{Remote: true})
	assert.Equal(t, 2, *kicks)
	a.ServicePlacement("app/web", ip, nil)
	a.ServicePlacement("app/web", ip, nil)
	assert.Equal(t, 3, *kicks)
	assert.Empty(t, a.placements)
}

// A FailOpen policy's slot with no holder is left out, so its flows take
// the CNI's path (design 11.2); a FailClosed one keeps its slot, which
// routes to prohibit. The plan itself is untouched.
func TestSteeredStage(t *testing.T) {
	stage := &datapath.PodStage{Policies: map[string]datapath.Policy{
		"open":   {V4: &datapath.PolicyFamily{Slot: 1, FailOpen: true}, V6: &datapath.PolicyFamily{Slot: 2, FailOpen: true}},
		"closed": {V4: &datapath.PolicyFamily{Slot: 3}},
	}}
	steer := map[uint8]dpSteer{1: {}, 2: {holders: []string{"b"}}, 3: {}}
	got := steeredStage(stage, steer)
	assert.Zero(t, got.Policies["open"].V4.Slot, "FailOpen, no holder")
	assert.Equal(t, uint8(2), got.Policies["open"].V6.Slot, "FailOpen, a holder")
	assert.Equal(t, uint8(3), got.Policies["closed"].V4.Slot, "FailClosed keeps it")
	assert.Equal(t, uint8(1), stage.Policies["open"].V4.Slot, "the plan is untouched")
	assert.Nil(t, steeredStage(nil, steer))
}

// Steering goes to the holders but this node, those with an underlay; a
// routed address skips unreachable ones while one remains (resolution
// 16), a local one keeps its elected owner.
func TestSteerTo(t *testing.T) {
	self := node("a", "10.0.0.1", "fd00::1")
	nodes := map[string]dpNode{"a": self, "b": node("b", "10.0.0.2", "fd00::2"), "c": node("c", "10.0.0.3", "")}
	id := datapath.NodeID
	v4, v6 := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")
	lost := map[netip.Addr]bool{netip.MustParseAddr("10.0.0.2"): true}

	routed := dpSteer{addr: v4, holders: []string{"a", "b", "c", "x"}, routed: true}
	assert.Equal(t, []uint64{id("c")}, steerTo(routed, "a", self, nodes, lost), "b unreachable by IPv4; x has no underlay")
	routed.addr = v6
	assert.Equal(t, []uint64{id("b"), id("c")}, steerTo(routed, "a", self, nodes, lost),
		"IPv6 to b goes over its IPv6 underlay, which answers")
	routed.addr, routed.holders = v4, []string{"b"}
	assert.Equal(t, []uint64{id("b")}, steerTo(routed, "a", self, nodes, lost), "the last one is kept")
	assert.Equal(t, []uint64{id("b")}, steerTo(dpSteer{addr: v4, holders: []string{"b"}}, "a", self, nodes, lost),
		"a local address's owner, unreachable or not")
	assert.Empty(t, steerTo(dpSteer{addr: v4, holders: []string{"a"}}, "a", self, nodes, lost), "only this node")

	assert.Equal(t, netip.MustParseAddr("10.0.0.3"), outerAddr(self, nodes["c"], true), "no IPv6 underlay at c")
	assert.Equal(t, netip.MustParseAddr("fd00::2"), outerAddr(node("a", "", "fd00::1"), nodes["b"], false))
}

// A slot's path MTU is the tunnel's in its family, capped by its pool's.
func TestSlotMTU(t *testing.T) {
	v4, v6 := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")
	assert.Equal(t, 1464, slotMTU(v4, 1464, 1444, 0))
	assert.Equal(t, 1400, slotMTU(v4, 1464, 1444, 1400))
	assert.Equal(t, 1444, slotMTU(v6, 1464, 1444, 1500))
	assert.Equal(t, 1400, slotMTU(v6, 1464, 0, 1400), "no tunnel peer")
	assert.Zero(t, slotMTU(v6, 1464, 0, 0))
}
