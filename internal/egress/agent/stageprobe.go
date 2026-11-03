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
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/nftables"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
)

// The stage probe (design 6.1; user decision 2026-11-02): at the first
// ADD after the agent starts of a selected pod pl_pod_tag tags, the
// plugin sends a UDP datagram per pod family from the pod's network
// namespace, and the agent reads where its table saw it. Until then the
// node classifies by source address.

// probeTimeout is how long a sandbox asked to probe has to report; then
// the next one is asked (a plugin too old to probe never reports).
const probeTimeout = 30 * time.Second

// probeSettle bounds watching the counters after the plugin's report: a
// datagram can wait seconds for its first hop's neighbour (a node's first
// pod after boot, IPv6: found on prox-purelb2 2026-11-03). The plugin is
// answered at once and the counters are watched in the background (user
// decision 2026-11-03).
var probeSettle = 10 * time.Second

const probePoll = 50 * time.Millisecond

// stageResult is what this session's probe found.
type stageResult struct {
	stage  string // cnisock.Stage*
	detail string
}

// probeTicket is the sandbox asked to probe, and when; result is what its
// report's counters showed, for the queue goroutine to take (settled).
type probeTicket struct {
	container string
	at        time.Time
	reported  atomic.Bool
	result    atomic.Pointer[stageResult]
	failed    atomic.Bool
}

// askProbe adds the probe request to v, an ADD's final ready verdict for
// the pod st, when the node has no stage yet and no sandbox is probing:
// the pod must have a pod-set id (it is selected, and pl_pod_tag tags it)
// and no hold (which would drop the probe). Queue goroutine only.
func (a *Agent) askProbe(container string, st podState, v *cniVerdict) *cniVerdict {
	if v == nil || !v.final || v.result != cnisock.ResultReady || st.podSet == 0 || st.hold ||
		a.stage.Load() != nil {
		return v
	}
	// The sandbox asked may ask again (its ADD retried, the answer lost);
	// another waits until the one asked is overdue.
	now := a.now()
	if t := a.prober.Load(); t != nil && t.container != container && now.Sub(t.at) < probeTimeout {
		return v
	}
	a.prober.Store(&probeTicket{container: container, at: now})
	asked := *v
	asked.probe = true
	return &asked
}

// probeReport takes the plugin's report that it sent the probe from ips,
// on the socket's goroutine, and watches the counters in the background.
func (a *Agent) probeReport(req cnisock.Request, ips []netip.Addr) cnisock.Response {
	t := a.prober.Load()
	if t == nil || t.container != req.ContainerID {
		return a.cniAnswer(req, &cniVerdict{err: "the agent asked no stage probe of this sandbox"})
	}
	fams := map[string]bool{}
	for _, ip := range ips {
		fams[map[bool]string{true: "4", false: "6"}[ip.Is4()]] = true
	}
	if len(fams) == 0 {
		return a.cniAnswer(req, &cniVerdict{err: "the probe report names no pod address"})
	}
	if t.reported.CompareAndSwap(false, true) {
		go a.settleProbe(t, req.Namespace+"/"+req.Name, fams)
	}
	return a.cniAnswer(req, &cniVerdict{result: cnisock.ResultReady})
}

// settleProbe reads the probe's counters until every family probed is
// forwarded, or for probeSettle, and leaves the stage they show in t.
func (a *Agent) settleProbe(t *probeTicket, pod string, fams map[string]bool) {
	var counts map[string]datapath.ProbeCount
	var err error
	for deadline := time.Now().Add(probeSettle); ; time.Sleep(probePoll) {
		if counts, err = a.probeCounts(); err == nil && forwarded(counts, fams) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		logging.Info(a.logger, "op", "egressStageProbe", "node", a.node, "pod", pod, "error", err,
			"msg", "reading the stage probe's counters; the next selected pod probes again")
		t.failed.Store(true)
	} else {
		r := stageOf(counts, fams)
		logging.Info(a.logger, "op", "egressStageProbe", "node", a.node, "pod", pod, "stage", r.stage,
			"detail", r.detail, "msg", "stage probe")
		t.result.Store(r)
	}
	a.add(keyPods, false) // the classification, and the capability and conditions
}

// settled takes the stage the asked sandbox's probe found, once its
// counters settled. Queue goroutine only, as stopSession, so a probe
// settling after its session stopped is dropped with its ticket.
func (a *Agent) settled() {
	t := a.prober.Load()
	if t == nil {
		return
	}
	if r := t.result.Load(); r != nil {
		a.stage.Store(r)
		a.prober.Store(nil)
	} else if t.failed.Load() {
		a.prober.Store(nil)
	}
}

func forwarded(counts map[string]datapath.ProbeCount, fams map[string]bool) bool {
	for f := range fams {
		if counts[f].Forwarded == 0 {
			return false
		}
	}
	return true
}

// stageOf is the stage the probe's counts show in the families probed:
// Mark when every probe datagram reached classify with pl_pod_tag's mark
// and was forwarded, SourceIP when one lost its mark, Bypassed when one
// was never forwarded.
func stageOf(counts map[string]datapath.ProbeCount, fams map[string]bool) *stageResult {
	var bypassed, untagged, seen []string
	for _, f := range []string{"4", "6"} {
		if !fams[f] {
			continue
		}
		c, name := counts[f], "IPv"+f
		seen = append(seen, fmt.Sprintf("%s %d/%d/%d", name, c.Seen, c.Tagged, c.Forwarded))
		switch {
		case c.Forwarded == 0:
			bypassed = append(bypassed, name)
		case c.Tagged == 0 || c.Tagged < c.Seen:
			untagged = append(untagged, name)
		}
	}
	detail := "seen/tagged/forwarded: " + strings.Join(seen, ", ")
	switch {
	case len(bypassed) > 0:
		return &stageResult{stage: cnisock.StageBypassed, detail: strings.Join(bypassed, ", ") +
			" pod traffic never reached the forward hook (" + detail + ")"}
	case len(untagged) > 0:
		return &stageResult{stage: cnisock.StageSourceIP, detail: strings.Join(untagged, ", ") +
			" pod traffic reached classify without pl_pod_tag's mark (" + detail + ")"}
	}
	return &stageResult{stage: cnisock.StageMark, detail: detail}
}

// readProbeCounts reads the probe's counters on a connection of its own.
func readProbeCounts() (map[string]datapath.ProbeCount, error) {
	c, err := nftables.New()
	if err != nil {
		return nil, err
	}
	return datapath.ProbeCounts(c)
}
