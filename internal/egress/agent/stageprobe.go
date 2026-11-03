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

// probeWait bounds reading the counters after the plugin's report: the
// datagrams cross the pod's device and the stack asynchronously.
const probeWait, probePoll = time.Second, 50 * time.Millisecond

// stageResult is what this session's probe found.
type stageResult struct {
	stage  string // cnisock.Stage*
	detail string
}

// probeTicket is the sandbox asked to probe, and when.
type probeTicket struct {
	container string
	at        time.Time
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

// probeReport reads where the probe the plugin sent from ips was seen, on
// the socket's goroutine, and records the node's stage.
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
	var counts map[string]datapath.ProbeCount
	var err error
	for deadline := time.Now().Add(probeWait); ; time.Sleep(probePoll) {
		if counts, err = a.probeCounts(); err == nil && forwarded(counts, fams) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		a.prober.CompareAndSwap(t, nil)
		return a.cniAnswer(req, &cniVerdict{err: "reading the probe's counters: " + err.Error()})
	}
	r := stageOf(counts, fams)
	a.stage.Store(r)
	a.prober.CompareAndSwap(t, nil)
	logging.Info(a.logger, "op", "egressStageProbe", "node", a.node, "pod", req.Namespace+"/"+req.Name, "stage", r.stage,
		"detail", r.detail, "msg", "stage probe")
	a.add(keyPods, false) // the classification
	a.add(keyNode, false) // the capability and conditions
	return a.cniAnswer(req, &cniVerdict{result: cnisock.ResultReady, stage: r.stage})
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
