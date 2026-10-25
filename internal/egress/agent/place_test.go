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
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// recordApplies is a datapath hook that records each apply in the
// placer's call sequence, as "apply <addr> held=<bool>", failing it while
// fail is set.
func recordApplies(p *fakePlacer, addr netip.Addr, fail *atomic.Bool) func(dpWant) error {
	return func(w dpWant) error {
		held := w.hold.addrs[addr] != nil && w.hold.addrs[addr].Held
		if fail.Load() {
			p.calls <- fmt.Sprintf("apply %s held=%v failed", addr, held)
			return errors.New("datapath down")
		}
		p.calls <- fmt.Sprintf("apply %s held=%v", addr, held)
		return nil
	}
}

// await returns the placer calls up to and including the first that
// starts with prefix.
func (p *fakePlacer) await(t *testing.T, prefix string) []string {
	t.Helper()
	var seen []string
	for {
		c := p.next(t)
		seen = append(seen, c)
		if strings.HasPrefix(c, prefix) {
			return seen
		}
	}
}

// announcing is this node's status.announcing addresses.
func (f *fixture) announcing() []string {
	ens, err := f.purelb.PurelbV2().EgressNodeStates().Get(context.Background(), testNode, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range ens.Status.Announcing {
		out = append(out, a.Address)
	}
	return out
}

// A local address this node wins is placed only once the datapath has
// installed it held, and acked only once placed; one removed outside the
// agent is placed again; one failing DAD is left; losing the election
// withdraws it before the datapath clears its held flag (design 6.7).
func TestPlaceLocal(t *testing.T) {
	a1 := netip.MustParseAddr("192.0.2.10")
	w := &winners{}
	w.set(map[string]string{"192.0.2.10": testNode})
	g := &fakeGuard{calls: make(chan string, 100)}
	p := &fakePlacer{calls: make(chan string, 200)}
	var fail atomic.Bool
	fail.Store(true)
	ea := address("a1", "192.0.2.10", "Local", "ee1")
	ea.UID = "uid-a1"
	f := holdingWith(t, w, g, p, recordApplies(p, a1, &fail), testNodeObj, ea,
		endpoints("ee1", "192.0.2.10", "Local", 3, purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Active"}))

	assert.NotContains(t, strings.Join(p.await(t, "apply 192.0.2.10 held=true failed"), "\n"), "place")
	got := f.until(t, func(w dpWant) bool { return len(w.hold.ifaces) > 0 })
	assert.Equal(t, map[netip.Addr]int{a1: 7}, got.hold.ifaces, "the pool interface, for its table")
	assert.Equal(t, map[int]dpPool{7: {name: "eth1", v4: true}}, got.pools)
	time.Sleep(200 * time.Millisecond)
	for len(p.calls) > 0 {
		assert.NotContains(t, <-p.calls, "place", "not placed while its held flag isn't installed")
	}

	fail.Store(false)
	f.a.add(keyDatapath, false)
	placeA1 := "place egress/uid-a1 192.0.2.10 routed=false  skipDAD=false"
	seen := p.await(t, "place")
	assert.Equal(t, placeA1, last(seen))
	assert.Contains(t, seen, "apply 192.0.2.10 held=true", "the held flag first")
	require.Eventually(t, func() bool { return slices.Equal(f.announcing(), []string{"192.0.2.10"}) },
		5*time.Second, 20*time.Millisecond, "acked once placed")

	// Removed outside the agent (an interface flap): placed again.
	p.present.Delete(a1)
	f.a.add(keyDatapath, false)
	assert.Equal(t, placeA1, last(p.await(t, "place")))

	// Failing DAD is reported, not fought: no placing again.
	p.present.Store(a1, unix.IFA_F_DADFAILED)
	f.a.add(keyDatapath, false)
	assert.NotContains(t, strings.Join(p.await(t, "apply"), "\n"), "place")

	// Lost: withdrawn before the datapath stops holding it.
	w.set(map[string]string{"192.0.2.10": "n2"})
	f.a.add(keyDatapath, false)
	seen = p.await(t, "apply 192.0.2.10 held=false")
	require.Contains(t, seen, "withdraw egress/uid-a1 192.0.2.10")
	assert.Less(t, slices.Index(seen, "withdraw egress/uid-a1 192.0.2.10"), len(seen)-1)
	require.Eventually(t, func() bool { return len(f.announcing()) == 0 }, 5*time.Second, 20*time.Millisecond)
}

// An address held but not placed (no interface for it) is not announced,
// so it isn't acked: it gates nothing (D6).
func TestPlaceFailedNotAcked(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{"192.0.2.10": testNode})
	g := &fakeGuard{calls: make(chan string, 100)}
	p := &fakePlacer{calls: make(chan string, 200)}
	p.fail.Store(true)
	f := holdingWith(t, w, g, p, nil, testNodeObj, address("a1", "192.0.2.10", "Local", "ee1"),
		endpoints("ee1", "192.0.2.10", "Local", 3, purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Active"}))
	f.until(t, func(w dpWant) bool { return len(w.hold.acks) == 1 })
	p.await(t, "place failed")
	require.Eventually(t, func() bool {
		ens, err := f.purelb.PurelbV2().EgressNodeStates().Get(context.Background(), testNode, metav1.GetOptions{})
		return err == nil && ens.Status.Capability != nil
	}, 5*time.Second, 20*time.Millisecond)
	assert.Empty(t, f.announcing())

	p.fail.Store(false)
	f.a.add(keyDatapath, false)
	p.await(t, "place egress/")
	require.Eventually(t, func() bool { return len(f.announcing()) == 1 }, 5*time.Second, 20*time.Millisecond)
}

// A routed address goes on the dummy interface with its pool's prefix;
// the advertiser withdraws when its guard stands down and when its lease
// has gone unrenewed for 3 lease durations, at that moment without another
// event, and advertises again once neither holds (design 6.10, 8.8).
func TestPlaceRouted(t *testing.T) {
	r1 := netip.MustParseAddr("198.51.100.5")
	w := &winners{}
	w.set(map[string]string{})
	g := &fakeGuard{calls: make(chan string, 100)}
	p := &fakePlacer{calls: make(chan string, 200)}
	var fail atomic.Bool
	node := testNodeObj.DeepCopy()
	node.Labels = map[string]string{purelbv2.EgressCapableLabel: "true"}
	ea := address("r1", "198.51.100.5", "Routed", "")
	ea.UID = "uid-r1"
	f := holdingWith(t, w, g, p, recordApplies(p, r1, &fail), node, ea,
		endpoints("er1", "198.51.100.5", "Routed", 7, purelbv2.EgressSlice{Index: 8, Node: "n3", State: "Active"}))
	placeR1 := "place egress/uid-r1 198.51.100.5 routed=true 198.51.100.0/24/32 skipDAD=false"
	assert.Equal(t, placeR1, last(p.await(t, "place")))

	g.down.Store(true)
	f.a.add(keyDatapath, false)
	seen := p.await(t, "apply 198.51.100.5 held=false")
	assert.Contains(t, seen, "withdraw egress/uid-r1 198.51.100.5", "the guard stood down")
	g.down.Store(false)
	f.a.add(keyDatapath, false)
	assert.Equal(t, placeR1, last(p.await(t, "place")))

	// Unrenewed for just under 3 lease durations: the sync it arms
	// withdraws without another event.
	w.renewed.Store(time.Now().Add(-29800 * time.Millisecond).UnixNano())
	f.a.add(keyDatapath, false)
	p.await(t, "apply 198.51.100.5 held=true")
	seen = p.await(t, "apply 198.51.100.5 held=false")
	assert.Contains(t, seen, "withdraw egress/uid-r1 198.51.100.5", "partitioned")
	w.renewed.Store(0) // renewed
	f.a.add(keyDatapath, false)
	assert.Equal(t, placeR1, last(p.await(t, "place")))
}

// A dedicated address on the node that it no longer holds, placed before
// the agent restarted, is withdrawn; one it holds is placed again.
func TestPlaceStaleWithdrawn(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{"192.0.2.10": testNode, "192.0.2.11": "n2"})
	g := &fakeGuard{calls: make(chan string, 100)}
	p := &fakePlacer{calls: make(chan string, 200)}
	p.present.Store(netip.MustParseAddr("192.0.2.10"), 0)
	p.present.Store(netip.MustParseAddr("192.0.2.11"), 0)
	held, stale := address("a1", "192.0.2.10", "Local", "ee1"), address("a2", "192.0.2.11", "Local", "ee2")
	held.UID, stale.UID = "uid-a1", "uid-a2"
	holdingWith(t, w, g, p, nil, testNodeObj, held, stale,
		endpoints("ee1", "192.0.2.10", "Local", 3, purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Active"}))
	var calls []string
	for len(calls) < 2 {
		calls = append(calls, p.next(t))
	}
	assert.ElementsMatch(t, []string{"withdraw egress/uid-a2 192.0.2.11",
		"place egress/uid-a1 192.0.2.10 routed=false  skipDAD=false"}, calls)
}

// IPv6 DAD is skipped per the policy's skipIPv6DAD when set, else per the
// pool's (D31).
func TestPlaceSkipDAD(t *testing.T) {
	for _, tc := range []struct {
		name         string
		policy, pool *bool
		want         bool
	}{
		{name: "pool", pool: ptrTo(true), want: true},
		{name: "policy wins", policy: ptrTo(false), pool: ptrTo(true), want: false},
		{name: "policy", policy: ptrTo(true), want: true},
		{name: "neither", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &winners{}
			w.set(map[string]string{"2001:db8::10": testNode})
			ea := address("a6", "2001:db8::10", "Local", "ee6")
			ea.Spec.Family, ea.Spec.SkipIPv6DAD = "IPv6", tc.policy
			ee := endpoints("ee6", "2001:db8::10", "Local", 1, purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Active"})
			ee.Spec.Family = "IPv6"
			p := &fakePlacer{calls: make(chan string, 200)}
			f := holdingWith(t, w, &fakeGuard{calls: make(chan string, 100)}, p, nil, testNodeObj, ea, ee)
			if tc.pool != nil {
				cfg := agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true})
				cfg.Groups = []*purelbv2.ServiceGroup{{ObjectMeta: metav1.ObjectMeta{Name: "sg"},
					Spec: purelbv2.ServiceGroupSpec{Egress: &purelbv2.ServiceGroupEgressSpec{},
						Local: &purelbv2.ServiceGroupLocalSpec{SkipIPv6DAD: *tc.pool}}}}
				f.a.Configure(cfg)
			}
			deadline := time.After(5 * time.Second)
			for {
				select {
				case c := <-p.calls:
					if strings.HasSuffix(c, fmt.Sprintf("skipDAD=%v", tc.want)) && strings.HasPrefix(c, "place") {
						return
					}
				case <-deadline:
					t.Fatalf("not placed with skipDAD=%v", tc.want)
				}
			}
		})
	}
}

// Disabling egress withdraws what it placed.
func TestPlaceDisableWithdraws(t *testing.T) {
	w := &winners{}
	w.set(map[string]string{"192.0.2.10": testNode})
	ea := address("a1", "192.0.2.10", "Local", "ee1")
	ea.UID = "uid-a1"
	p := &fakePlacer{calls: make(chan string, 200)}
	f := holdingWith(t, w, &fakeGuard{calls: make(chan string, 100)}, p, nil, testNodeObj, ea,
		endpoints("ee1", "192.0.2.10", "Local", 3, purelbv2.EgressSlice{Index: 9, Node: "n2", State: "Active"}))
	p.await(t, "place")
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: false}))
	assert.Equal(t, "withdraw egress/uid-a1 192.0.2.10", last(p.await(t, "withdraw")))
}

func last(s []string) string { return s[len(s)-1] }

func ptrTo[T any](v T) *T { return &v }
