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
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"purelb.io/internal/addrguard"
	"purelb.io/internal/egress"
	"purelb.io/internal/egress/datapath"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// winners is an Elector whose winners the test sets, by address.
type winners struct {
	m atomic.Pointer[map[string]string]
}

func (w *winners) set(m map[string]string) { w.m.Store(&m) }

func (w *winners) WinnerAmong(key string, _ func(string) bool, _ []string) string {
	return (*w.m.Load())[key]
}
func (w *winners) SteeringWinner(key string, _ func(string) bool, _ []string) string {
	return (*w.m.Load())[key]
}
func (w *winners) SteeringLive() []string { return nil }

// fakeGuard records UpdateEgress calls and refuses the addresses in fail.
type fakeGuard struct {
	calls chan string
	fail  map[netip.Addr]bool // set before the agent starts
}

func (g *fakeGuard) UpdateEgress(a netip.Addr, present bool, ports []addrguard.EgressPort, sl []addrguard.EgressSlice) error {
	g.calls <- fmt.Sprintf("%s %v ports=%v slices=%v", a, present, ports, sl)
	if g.fail[a] {
		return errors.New("map full")
	}
	return nil
}

func (g *fakeGuard) drain() []string {
	var out []string
	for {
		select {
		case c := <-g.calls:
			out = append(out, c)
		default:
			return out
		}
	}
}

const ns = "purelb-system"

func address(name, ip, poolType, ee string) *purelbv2.EgressAddress {
	a := &purelbv2.EgressAddress{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: purelbv2.EgressAddressSpec{State: "Active", Address: ip, Family: "IPv4", ServiceGroup: "sg",
			PoolType: poolType, Placement: "SingleAnnouncer"}}
	if ee != "" {
		a.OwnerReferences = []metav1.OwnerReference{{Kind: "EgressEndpoints", Name: ee}}
	}
	return a
}

func endpoints(name, ip, poolType string, gen int64, slices ...purelbv2.EgressSlice) *purelbv2.EgressEndpoints {
	return &purelbv2.EgressEndpoints{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: gen,
		Labels: map[string]string{egress.LabelServiceGroup: "sg"}},
		Spec: purelbv2.EgressEndpointsSpec{State: "Active", Address: ip, Family: "IPv4", ServiceGroup: "sg",
			PoolType: poolType, SliceSize: 4096, Slices: slices}}
}

// holding starts an egress-enabled agent on objs with the winners and guard
// given, and the ServiceGroup "sg".
func holding(t *testing.T, w *winners, g *fakeGuard, objs ...runtime.Object) *fixture {
	t.Helper()
	f := newFixture(t, true, objs...)
	f.a.SetElection(w)
	f.a.SetGuard(g)
	cfg := agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true})
	cfg.Groups = []*purelbv2.ServiceGroup{{ObjectMeta: metav1.ObjectMeta{Name: "sg"},
		Spec: purelbv2.ServiceGroupSpec{Egress: &purelbv2.ServiceGroupEgressSpec{}}}}
	f.a.Configure(cfg)
	return f
}

// until returns the first desired state that pred accepts.
func (f *fixture) until(t *testing.T, pred func(dpWant) bool) dpWant {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case w := <-f.wants:
			if pred(w) {
				return w
			}
		case <-deadline:
			t.Fatal("no desired state matched")
			return dpWant{}
		}
	}
}

// A local address this node wins is held once its EgressEndpoints is
// read: its slices, reserved ports and table go to the datapath and the
// guard, and the installed generation is acked; one another node wins is
// only known, guarded with nothing allowed. Losing the election drops the
// address and its ack.
func TestHoldLocal(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{"192.0.2.10": testNode, "192.0.2.11": "n2"})
	g := &fakeGuard{calls: make(chan string, 100)}
	ee := endpoints("ee1", "192.0.2.10", "Local", 3,
		purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Active"},
		purelbv2.EgressSlice{Index: 10, Node: testNode, State: "Pending"},
		purelbv2.EgressSlice{Index: 11, State: "Active"}) // an unassigned reserve
	ee.Spec.Reserved = []purelbv2.EgressReservedAssignment{{Protocol: "UDP", Port: 5060, Node: "n2"}}
	f := holding(t, w, g, testNodeObj, address("a1", "192.0.2.10", "Local", "ee1"), ee,
		address("a2", "192.0.2.11", "Local", "ee2"))

	got := f.until(t, func(w dpWant) bool { return len(w.hold.acks) > 0 })
	a1, a2 := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
	assert.Equal(t, &datapath.AddressEntries{
		Address:  datapath.Address{Held: true, SliceSize: 4096, Base: egress.FirstSlicePort},
		Slices:   map[uint16]uint64{9: datapath.NodeID("n2"), 10: datapath.NodeID(testNode)},
		Reserved: map[datapath.PortKey]uint64{{Proto: 17, Port: 5060}: datapath.NodeID("n2")},
		Services: map[datapath.PortKey]bool{},
	}, got.hold.addrs[a1])
	assert.Equal(t, &datapath.AddressEntries{}, got.hold.addrs[a2], "known, not held")
	assert.ElementsMatch(t, []netip.Addr{a1, a2}, got.hold.egress)
	assert.Equal(t, []purelbv2.EgressAnnouncing{{Endpoints: "ee1", Address: "192.0.2.10", Generation: 3}}, got.hold.acks)
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && len(ens.Status.Announcing) == 1 && ens.Status.Announcing[0].Generation == 3
	}, 5*time.Second, 20*time.Millisecond, "acked in status.announcing")
	calls := g.drain()
	assert.Contains(t, calls, "192.0.2.10 true ports=[{17 5060}] slices=[{36864 4096} {40960 4096}]")
	assert.Contains(t, calls, "192.0.2.11 true ports=[] slices=[]")

	// Another node wins it now: not held, no ack.
	w.set(map[string]string{"192.0.2.10": "n2", "192.0.2.11": "n2"})
	f.a.add(keyDatapath, false)
	got = f.until(t, func(w dpWant) bool { return len(w.hold.acks) == 0 })
	assert.Equal(t, &datapath.AddressEntries{}, got.hold.addrs[a1])
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && len(ens.Status.Announcing) == 0
	}, 5*time.Second, 20*time.Millisecond, "the ack withdrawn")
}

// A routed address is held by every advertiser (D7): an egress-capable
// node the group's selector picks, through the label-selector watch. The
// node becoming capable (its label) is acted on at once, not at the next
// periodic reconcile.
func TestHoldRouted(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{})
	g := &fakeGuard{calls: make(chan string, 100)}
	f := holding(t, w, g, testNodeObj.DeepCopy(), address("r1", "198.51.100.5", "Routed", ""),
		endpoints("er1", "198.51.100.5", "Routed", 7, purelbv2.EgressSlice{Index: 8, Node: "n3", State: "Active"}))
	got := f.until(t, func(w dpWant) bool { return len(w.hold.egress) == 1 })
	assert.Empty(t, got.hold.acks, "not egress-capable yet: no advertiser")

	node := testNodeObj.DeepCopy()
	node.Labels = map[string]string{purelbv2.EgressCapableLabel: "true"}
	_, err := f.kube.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	got = f.until(t, func(w dpWant) bool { return len(w.hold.acks) > 0 })
	e := got.hold.addrs[netip.MustParseAddr("198.51.100.5")]
	assert.True(t, e.Held)
	assert.Equal(t, uint32(254), e.Table, "routed transit in the main table")
	assert.Equal(t, []purelbv2.EgressAnnouncing{{Endpoints: "er1", Address: "198.51.100.5", Generation: 7}}, got.hold.acks)
}

// Turning egress off on a holder withdraws its acks with its capability,
// so the allocator stops gating on it (D6).
func TestDisableClearsAcks(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{"192.0.2.10": testNode})
	g := &fakeGuard{calls: make(chan string, 100)}
	f := holding(t, w, g, testNodeObj, address("a1", "192.0.2.10", "Local", "ee1"),
		endpoints("ee1", "192.0.2.10", "Local", 3, purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Active"}))
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && len(ens.Status.Announcing) == 1
	}, 5*time.Second, 20*time.Millisecond)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: false}))
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && !ens.Status.Capability.EgressCapable && len(ens.Status.Announcing) == 0
	}, 5*time.Second, 20*time.Millisecond)
	assert.Contains(t, g.drain(), "192.0.2.10 false ports=[] slices=[]", "out of the guard")
}

// A shared address is held where its Service is announced (Inherit), with
// the Service ports from its EgressEndpoints in the hardening sets.
func TestHoldShared(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{})
	g := &fakeGuard{calls: make(chan string, 100)}
	ea := address("s1", "192.0.2.20", "Local", "es1")
	ea.Spec.SharedWith = &purelbv2.EgressServiceRef{Name: "web"}
	ee := endpoints("es1", "192.0.2.20", "Local", 2)
	ee.Spec.ServicePorts = []purelbv2.EgressServicePort{{Protocol: "TCP", Port: 443}}
	f := newFixture(t, true, testNodeObj, ea, ee)
	f.a.SetElection(w)
	f.a.SetGuard(g)
	// Before the session: nothing else runs on the queue goroutine yet.
	f.a.ServiceAddress("app/web", net.ParseIP("192.0.2.20"), true)
	cfg := agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true})
	cfg.Groups = []*purelbv2.ServiceGroup{{ObjectMeta: metav1.ObjectMeta{Name: "sg"},
		Spec: purelbv2.ServiceGroupSpec{Egress: &purelbv2.ServiceGroupEgressSpec{}}}}
	f.a.Configure(cfg)

	got := f.until(t, func(w dpWant) bool { return len(w.hold.acks) > 0 })
	addr := netip.MustParseAddr("192.0.2.20")
	assert.True(t, got.hold.addrs[addr].Held)
	assert.Equal(t, []datapath.ServicePort{{Addr: addr, Proto: 6, Port: 443}}, got.hold.services)
}

// An address the guard refuses (its map full) is not acked, so its slices
// stay Pending (§6.10).
func TestHoldGuardRefuses(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{"192.0.2.10": testNode})
	a1 := netip.MustParseAddr("192.0.2.10")
	g := &fakeGuard{calls: make(chan string, 100), fail: map[netip.Addr]bool{a1: true}}
	f := holding(t, w, g, testNodeObj, address("a1", "192.0.2.10", "Local", "ee1"),
		endpoints("ee1", "192.0.2.10", "Local", 3, purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Pending"}))
	require.Eventually(t, func() bool {
		for _, c := range g.drain() {
			if c == "192.0.2.10 true ports=[] slices=[{36864 4096}]" {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the guard was asked")
	got := f.until(t, func(dpWant) bool { return true })
	assert.Empty(t, got.hold.acks)
	assert.False(t, got.hold.addrs[a1].Held)
	ens, _ := f.purelb.PurelbV2().EgressNodeStates().Get(context.Background(), testNode, metav1.GetOptions{})
	if ens != nil {
		assert.Empty(t, ens.Status.Announcing)
	}
}
