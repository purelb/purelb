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
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/egress/datapath"
)

func pc(seen, tagged, forwarded uint64) datapath.ProbeCount {
	return datapath.ProbeCount{Seen: seen, Tagged: tagged, Forwarded: forwarded}
}

// What the probe's counts show (design 6.1), in the families probed.
func TestStageOf(t *testing.T) {
	both := map[string]bool{"4": true, "6": true}
	v4 := map[string]bool{"4": true}
	for _, tc := range []struct {
		name   string
		counts map[string]datapath.ProbeCount
		fams   map[string]bool
		stage  string
		like   string
	}{
		{"tagged and forwarded", map[string]datapath.ProbeCount{"4": pc(1, 1, 1), "6": pc(2, 2, 2)}, both, cnisock.StageMark, "IPv4 1/1/1, IPv6 2/2/2"},
		{"untagged", map[string]datapath.ProbeCount{"4": pc(1, 0, 1), "6": pc(1, 1, 1)}, both, cnisock.StageSourceIP, "IPv4 pod traffic reached classify without"},
		{"some untagged", map[string]datapath.ProbeCount{"4": pc(2, 1, 2)}, v4, cnisock.StageSourceIP, "IPv4"},
		{"not forwarded", map[string]datapath.ProbeCount{"4": pc(1, 1, 1), "6": pc(0, 0, 0)}, both, cnisock.StageBypassed, "IPv6 pod traffic never reached the forward hook"},
		{"seen, not forwarded", map[string]datapath.ProbeCount{"4": pc(1, 1, 0)}, v4, cnisock.StageBypassed, "IPv4 1/1/0"},
		{"an unprobed family ignored", map[string]datapath.ProbeCount{"4": pc(1, 1, 1)}, v4, cnisock.StageMark, "IPv4 1/1/1"},
	} {
		r := stageOf(tc.counts, tc.fams)
		assert.Equal(t, tc.stage, r.stage, tc.name)
		assert.Contains(t, r.detail, tc.like, tc.name)
	}
}

// Who is asked to probe: a selected pod with a pod-set and no hold, whose
// ADD is ready, while the node has no stage; one sandbox at a time, until
// it is overdue; the same sandbox again.
func TestAskProbe(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := New(log.NewNopLogger(), testNode, "purelb-system")
	a.now = func() time.Time { return now }
	ready := &cniVerdict{result: cnisock.ResultReady, final: true}
	sel := podState{selected: true, podSet: 3}
	for _, tc := range []struct {
		name string
		st   podState
		v    *cniVerdict
	}{
		{"no verdict", sel, nil},
		{"not final", sel, &cniVerdict{result: cnisock.ResultPass}},
		{"held", sel, &cniVerdict{result: cnisock.ResultHeld, final: true}},
		{"not selected", podState{}, ready},
		{"no pod-set", podState{selected: true}, ready},
		{"hold", podState{selected: true, podSet: 3, hold: true}, ready},
	} {
		assert.Equal(t, tc.v, a.askProbe("c0", tc.st, tc.v), tc.name)
		assert.Nil(t, a.prober.Load(), tc.name)
	}
	v := a.askProbe("c1", sel, ready)
	require.True(t, v.probe)
	assert.False(t, ready.probe, "the verdict is copied, not changed")
	assert.False(t, a.askProbe("c2", sel, ready).probe, "c1 is probing")
	assert.True(t, a.askProbe("c1", sel, ready).probe, "c1 again: its answer may have been lost")
	now = now.Add(probeTimeout)
	assert.True(t, a.askProbe("c2", sel, ready).probe, "c1 is overdue")
	a.stage.Store(&stageResult{stage: cnisock.StageMark})
	now = now.Add(probeTimeout)
	assert.False(t, a.askProbe("c3", sel, ready).probe, "the node has its stage")
}

func TestProbeReport(t *testing.T) {
	a := New(log.NewNopLogger(), testNode, "purelb-system")
	req := cnisock.Request{Op: cnisock.OpProbe, ContainerID: "c1", Namespace: "voice", Name: "a"}
	ips := []netip.Addr{netip.MustParseAddr("10.1.0.5"), netip.MustParseAddr("fd04::5")}
	assert.Contains(t, a.probeReport(req, ips).Error, "asked no stage probe")
	a.prober.Store(&probeTicket{container: "c1", at: time.Now()})
	assert.Contains(t, a.probeReport(req, nil).Error, "names no pod address")

	defer func(d time.Duration) { probeSettle = d }(probeSettle)
	probeSettle = 500 * time.Millisecond
	// report answers ready at once and settles the ticket in the
	// background; settled takes the result on the queue goroutine.
	report := func(ips []netip.Addr) (*probeTicket, cnisock.Response, time.Duration) {
		tk := &probeTicket{container: "c1", at: time.Now()}
		a.prober.Store(tk)
		start := time.Now()
		resp := a.probeReport(req, ips)
		assert.Less(t, time.Since(start), probeSettle/2, "the plugin isn't kept waiting")
		require.Eventually(t, func() bool { return tk.result.Load() != nil || tk.failed.Load() }, 5*time.Second, 5*time.Millisecond)
		return tk, resp, time.Since(start)
	}

	// The counters can't be read for probeSettle: reported, and the
	// next pod asked.
	a.probeCounts = func() (map[string]datapath.ProbeCount, error) { return nil, errors.New("no table") }
	tk, resp, took := report(ips)
	assert.GreaterOrEqual(t, took, probeSettle, "retried meanwhile")
	assert.Equal(t, cnisock.ResultReady, resp.Result)
	assert.True(t, tk.failed.Load())
	a.settled()
	assert.Nil(t, a.prober.Load())
	assert.Nil(t, a.stage.Load())

	// Counted only after a while: read until forwarded in each family.
	var reads atomic.Int32
	a.probeCounts = func() (map[string]datapath.ProbeCount, error) {
		if reads.Add(1) < 3 {
			return map[string]datapath.ProbeCount{"4": pc(1, 1, 1)}, nil
		}
		return map[string]datapath.ProbeCount{"4": pc(1, 1, 1), "6": pc(1, 1, 1)}, nil
	}
	tk, resp, _ = report(ips)
	assert.Equal(t, cnisock.ResultReady, resp.Result)
	assert.Equal(t, int32(3), reads.Load())
	assert.Nil(t, a.stage.Load(), "until the queue goroutine takes it")
	// A second report of the same ticket starts no second watch.
	assert.Equal(t, cnisock.ResultReady, a.probeReport(req, ips).Result)
	time.Sleep(3 * probePoll)
	assert.Equal(t, int32(3), reads.Load())
	a.settled()
	assert.Equal(t, cnisock.StageMark, a.stage.Load().stage)
	assert.Nil(t, a.prober.Load())
	assert.Contains(t, a.probeReport(req, ips).Error, "asked no stage probe", "once per start")

	// Never forwarded: given up after probeSettle, bypassed.
	a.stage.Store(nil)
	a.probeCounts = func() (map[string]datapath.ProbeCount, error) { return map[string]datapath.ProbeCount{}, nil }
	tk, resp, took = report(ips[:1])
	assert.Equal(t, cnisock.ResultReady, resp.Result)
	assert.GreaterOrEqual(t, took, probeSettle)
	assert.Equal(t, cnisock.StageBypassed, tk.result.Load().stage)

	// A session stopped meanwhile drops it with its ticket.
	a.prober.Store(nil)
	a.settled()
	assert.Nil(t, a.stage.Load())
}
