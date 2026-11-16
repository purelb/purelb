// Copyright 2020-2026 Acnodal Inc.
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
	"errors"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-kit/log"
	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// syncCounter counts ForceSync calls, from any goroutine.
type syncCounter struct {
	nopServiceEvent
	n atomic.Int32
}

func (s *syncCounter) ForceSync() { s.n.Add(1) }

// dadSequence answers lookDAD from a list, repeating the last state.
func dadSequence(states ...dadState) func(netlink.Link, net.IP) dadState {
	var i atomic.Int32
	return func(netlink.Link, net.IP) dadState {
		n := int(i.Add(1)) - 1
		return states[min(n, len(states)-1)]
	}
}

// The unsolicited NAs wait for DAD to pass, and passing resets a backoff.
func TestWatchDADAnnouncesAfterPass(t *testing.T) {
	ip := net.ParseIP("2001:db8::10")
	a := &announcer{logger: log.NewNopLogger(), client: &syncCounter{}}
	looks := 0
	seq := dadSequence(dadTentative, dadTentative, dadTentative, dadPassed)
	a.dadState = func(l netlink.Link, ip net.IP) dadState { looks++; return seq(l, ip) }
	a.dadFailed.Store(ip.String(), dadRetry{backoff: dadRetryMin})
	var sent atomic.Int32
	a.watchDAD(nil, ip, renewalKey("ns/svc", ip.String()), func() { sent.Add(1) })
	require.Eventually(t, func() bool { return sent.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 4, looks, "announced only once DAD passed")
	_, failed := a.dadFailed.Load(ip.String())
	assert.False(t, failed, "passing resets the backoff")
	assert.Zero(t, a.client.(*syncCounter).n.Load())
}

// A failure is recorded with a doubling backoff, capped, and every
// Service is synced again (to withdraw it) and again at the retry. Gone
// after this node withdrew it is not a failure.
func TestWatchDADFailure(t *testing.T) {
	ip := net.ParseIP("2001:db8::11")
	key := renewalKey("ns/svc", ip.String())
	for _, tc := range []struct {
		name    string
		states  []dadState
		ours    bool
		failure bool
	}{
		{"deleted by the kernel while announced", []dadState{dadTentative, dadGone}, true, true},
		{"permanent, flagged failed", []dadState{dadTentative, dadFailedState}, true, true},
		{"withdrawn here", []dadState{dadTentative, dadGone}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &syncCounter{}
			a := &announcer{logger: log.NewNopLogger(), client: sc}
			if tc.ours {
				a.announced.Store(key, struct{}{})
			}
			before := ptu.ToFloat64(dadFailures)
			a.dadState = dadSequence(tc.states...)
			var sent atomic.Int32
			a.watchDAD(nil, ip, key, func() { sent.Add(1) })
			require.Eventually(t, func() bool {
				_, busy := a.dadWatching.Load(ip.String())
				return !busy
			}, 5*time.Second, 10*time.Millisecond)
			assert.Zero(t, sent.Load(), "never announced")
			v, failed := a.dadFailed.Load(ip.String())
			assert.Equal(t, tc.failure, failed)
			if !tc.failure {
				assert.Equal(t, before, ptu.ToFloat64(dadFailures))
				assert.Zero(t, sc.n.Load())
				return
			}
			assert.Equal(t, dadRetryMin, v.(dadRetry).backoff)
			assert.Equal(t, before+1, ptu.ToFloat64(dadFailures))
			assert.Equal(t, int32(1), sc.n.Load(), "synced at once to withdraw it")
			waiting, left := a.inDADBackoff(ip)
			assert.True(t, waiting)
			assert.InDelta(t, dadRetryMin.Seconds(), left.Seconds(), 1)

			want := dadRetryMin
			for range 12 {
				want = min(want*2, dadRetryMax)
				a.failDAD(ip)
				v, _ := a.dadFailed.Load(ip.String())
				assert.Equal(t, want, v.(dadRetry).backoff)
			}
		})
	}
}

// An election move is the common failure: the new winner adds the
// address while the old one still holds it for a few hundred
// milliseconds. The retry has to come soon after, or the address has no
// holder until it does.
func TestDADRetrySoonAfterAMove(t *testing.T) {
	ip := net.ParseIP("2001:db8::13")
	sc := &syncCounter{}
	a := &announcer{logger: log.NewNopLogger(), client: sc}
	start := time.Now()
	a.failDAD(ip)
	require.Equal(t, int32(1), sc.n.Load(), "synced at once to withdraw it")
	require.Eventually(t, func() bool { return sc.n.Load() == 2 }, 3*time.Second, 10*time.Millisecond,
		"synced again for the retry")
	waiting, _ := a.inDADBackoff(ip)
	assert.False(t, waiting, "the retry finds the backoff over")
	assert.Less(t, time.Since(start), 2*time.Second)
}

// One watcher per address: a Service sharing it doesn't start another.
func TestWatchDADOnePerAddress(t *testing.T) {
	ip := net.ParseIP("2001:db8::12")
	a := &announcer{logger: log.NewNopLogger(), client: &syncCounter{}}
	release := make(chan struct{})
	a.dadState = func(netlink.Link, net.IP) dadState {
		select {
		case <-release:
			return dadPassed
		default:
			return dadTentative
		}
	}
	var sent atomic.Int32
	a.watchDAD(nil, ip, "a", func() { sent.Add(1) })
	a.watchDAD(nil, ip, "b", func() { sent.Add(1) })
	close(release)
	require.Eventually(t, func() bool { return sent.Load() >= 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(3 * dadPoll)
	assert.Equal(t, int32(1), sent.Load())
}

// eventRecorder records events with their Service.
type eventRecorder struct {
	recordingServiceEvent
	syncs atomic.Int32
}

func (e *eventRecorder) ForceSync() { e.syncs.Add(1) }

// While an address waits for its DAD retry, announceLocal withdraws it,
// clears this node's slot and says so on the Service once; with DAD
// skipped the backoff doesn't apply.
func TestAnnounceLocalWithdrawsDuringDADBackoff(t *testing.T) {
	const ip, slotKey = "2001:db8::13", "purelb.io/announcing-IPv6"
	ev := &eventRecorder{}
	a := withdrawalTestAnnouncer(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"})
	a.client = ev
	a.election = &fakeElection{winner: "node-a"}
	a.dadFailed.Store(ip, dadRetry{backoff: dadRetryMin, until: time.Now().Add(dadRetryMin)})
	svc := lbSvc(ip)
	svc.Namespace, svc.Name = "default", "test-svc"
	svc.Annotations = map[string]string{slotKey: "node-a,eth0," + ip}
	announceForTest(a, "default/test-svc", ip)
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "purelb-nolink"}}
	lbIP := net.ParseIP(ip)
	ipNet := net.IPNet{IP: lbIP, Mask: net.CIDRMask(64, 128)}
	cfg := a.cfg.Load()

	require.NoError(t, a.announceLocal(cfg, svc, nil, link, lbIP, ipNet))
	assert.Empty(t, svc.Annotations[slotKey], "slot cleared")
	key := renewalKey("default/test-svc", ip)
	_, announced := a.announced.Load(key)
	assert.False(t, announced)
	_, renewing := a.addressRenewals.Load(key)
	assert.False(t, renewing, "renewal cancelled: it would add the address again")
	require.NoError(t, a.announceLocal(cfg, svc, nil, link, lbIP, ipNet))
	assert.Equal(t, []string{"DADFailed"}, ev.reasons(), "said once")

	// DAD skipped: the add is attempted (and fails: the link isn't real).
	svc.Annotations[purelbv2.SkipIPv6DADAnnotation] = "true"
	assert.Error(t, a.announceLocal(cfg, svc, nil, link, lbIP, ipNet))
	assert.Equal(t, []string{"DADFailed", "AnnouncementFailed"}, ev.reasons())
}

// On the kernel: a veth pair, one end owning an address, the other adding
// it with DAD. A finite-lifetime address that fails is deleted by the
// kernel; a permanent one keeps IFA_F_DADFAILED; both are recorded as
// failures and never announced. An address no one else has is announced
// once it leaves tentative. Needs CAP_NET_ADMIN.
func TestDADOnTheKernel(t *testing.T) {
	a1, b1 := dadVeth(t, "purelb-dada", "purelb-dadb")

	owner := func(ip string) {
		require.NoError(t, addNetworkWithOptions(net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(64, 128)}, b1,
			AddressOptions{SkipDAD: true}))
	}
	watch := func(ip string, opts AddressOptions) (*announcer, *atomic.Int32) {
		a := &announcer{logger: log.NewNopLogger(), client: &syncCounter{}}
		key := renewalKey("ns/svc", ip)
		a.announced.Store(key, struct{}{})
		require.NoError(t, addNetworkWithOptions(net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(64, 128)}, a1, opts))
		require.Equal(t, dadTentative, addrDADState(a1, net.ParseIP(ip)), "%s: tentative after the add", ip)
		var sent atomic.Int32
		a.watchDAD(a1, net.ParseIP(ip), key, func() { sent.Add(1) })
		require.Eventually(t, func() bool {
			_, busy := a.dadWatching.Load(ip)
			return !busy
		}, dadWait+time.Second, 50*time.Millisecond)
		return a, &sent
	}

	t.Run("finite lifetime, duplicate", func(t *testing.T) {
		owner("2001:db8:dad::1")
		a, sent := watch("2001:db8:dad::1", AddressOptions{ValidLft: 300, PreferedLft: 300})
		_, failed := a.dadFailed.Load("2001:db8:dad::1")
		assert.True(t, failed)
		assert.Zero(t, sent.Load())
		assert.Equal(t, dadGone, addrDADState(a1, net.ParseIP("2001:db8:dad::1")), "the kernel deleted it")
	})
	t.Run("permanent, duplicate", func(t *testing.T) {
		owner("2001:db8:dad::2")
		a, sent := watch("2001:db8:dad::2", AddressOptions{})
		_, failed := a.dadFailed.Load("2001:db8:dad::2")
		assert.True(t, failed)
		assert.Zero(t, sent.Load())
		assert.Equal(t, dadFailedState, addrDADState(a1, net.ParseIP("2001:db8:dad::2")))
	})
	t.Run("unique", func(t *testing.T) {
		a, sent := watch("2001:db8:dad::3", AddressOptions{ValidLft: 300, PreferedLft: 300})
		_, failed := a.dadFailed.Load("2001:db8:dad::3")
		assert.False(t, failed)
		assert.Equal(t, int32(1), sent.Load(), "announced after DAD")
		assert.Equal(t, dadPassed, addrDADState(a1, net.ParseIP("2001:db8:dad::3")))
	})
}

// dadVeth makes a veth pair, both ends up with DAD on, removed when the
// test ends. Skips without CAP_NET_ADMIN or with accept_dad off.
func dadVeth(t *testing.T, a, b string) (netlink.Link, netlink.Link) {
	t.Helper()
	la := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: a}, PeerName: b}
	if err := netlink.LinkAdd(la); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("needs CAP_NET_ADMIN: %v", err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(la) })
	a1, err := netlink.LinkByName(a)
	require.NoError(t, err)
	b1, err := netlink.LinkByName(b)
	require.NoError(t, err)
	for _, l := range []netlink.Link{a1, b1} {
		dad, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + l.Attrs().Name + "/accept_dad")
		require.NoError(t, err)
		if string(dad) == "0\n" {
			t.Skip("accept_dad is 0")
		}
		require.NoError(t, netlink.LinkSetUp(l))
	}
	require.Eventually(t, func() bool {
		l, err := netlink.LinkByName(a)
		return err == nil && l.Attrs().OperState == netlink.OperUp
	}, 10*time.Second, 50*time.Millisecond)
	return a1, b1
}

// On the kernel, through announceLocal: an address with DAD is not
// claimed (no slot, no AnnouncingLocal event, gauge not set) while it is
// tentative, nor ever when another host has it; once DAD passes every
// Service is synced again, and that sync claims it. Needs CAP_NET_ADMIN.
func TestAnnounceLocalClaimsOnlyAfterDAD(t *testing.T) {
	const slotKey = "purelb.io/announcing-IPv6"
	a1, b1 := dadVeth(t, "purelb-dsa", "purelb-dsb")

	claim := func(ip string) (*announcer, *eventRecorder, *v1.Service) {
		ev := &eventRecorder{}
		a := withdrawalTestAnnouncer(&purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"})
		a.client = ev
		a.election = &fakeElection{winner: "node-a"}
		svc := lbSvc(ip)
		svc.Namespace, svc.Name = "default", "dad-"+ip
		lbIP := net.ParseIP(ip)
		require.NoError(t, a.announceLocal(a.cfg.Load(), svc, nil, a1, lbIP,
			net.IPNet{IP: lbIP, Mask: net.CIDRMask(64, 128)}))
		t.Cleanup(func() { a.cancelRenewal("default/dad-"+ip, ip) })
		return a, ev, svc
	}
	unclaimed := func(t *testing.T, ev *eventRecorder, svc *v1.Service, ip string) {
		t.Helper()
		assert.Empty(t, svc.Annotations[slotKey], "no slot")
		assert.NotContains(t, ev.reasons(), "AnnouncingLocal")
		assert.Zero(t, ptu.ToFloat64(announcing.WithLabelValues("default/dad-"+ip, "node-a", ip)), "gauge not set")
	}

	t.Run("another host has it", func(t *testing.T) {
		const ip = "2001:db8:dad::21"
		require.NoError(t, addNetworkWithOptions(net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(64, 128)}, b1,
			AddressOptions{SkipDAD: true}))
		a, ev, svc := claim(ip)
		unclaimed(t, ev, svc, ip)
		require.Eventually(t, func() bool { _, f := a.dadFailed.Load(ip); return f }, dadWait+time.Second,
			50*time.Millisecond, "DAD failed")
		unclaimed(t, ev, svc, ip)
	})
	t.Run("unique", func(t *testing.T) {
		const ip = "2001:db8:dad::22"
		a, ev, svc := claim(ip)
		unclaimed(t, ev, svc, ip)
		require.Eventually(t, func() bool { return ev.syncs.Load() == 1 }, dadWait+time.Second,
			50*time.Millisecond, "DAD passed: synced again")
		lbIP := net.ParseIP(ip)
		require.NoError(t, a.announceLocal(a.cfg.Load(), svc, nil, a1, lbIP,
			net.IPNet{IP: lbIP, Mask: net.CIDRMask(64, 128)}))
		assert.Equal(t, "node-a,purelb-dsa,"+ip, svc.Annotations[slotKey])
		assert.Equal(t, []string{"AnnouncingLocal"}, ev.reasons())
		assert.Equal(t, 1.0, ptu.ToFloat64(announcing.WithLabelValues("default/dad-"+ip, "node-a", ip)))
	})
}
