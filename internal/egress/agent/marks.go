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
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var (
	markMoves = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "mark_moves_total",
		Help: "Moves of an unset markField to another mark byte, away from one the node's other rules use (D18).",
	})
	markConflict = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "mark_conflict",
		Help: "1 while the node's other netfilter rules use egress's mark byte (MarkConflict), else 0.",
	})
	forwardBlocked = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "forward_blocked",
		Help: "1 while the node's pods' new egress flows are marked but none reaches its SNAT (ForwardBlocked), else 0.",
	}, []string{"family"})
)

func init() {
	prometheus.MustRegister(markMoves, markConflict, forwardBlocked)
}

// markCandidates are the bytes an unset markField may use, in order: the
// default, then the bytes the most CNIs leave free, kube-proxy's last
// (resolution 22).
var markCandidates = []uint8{0, 16, 24, 8}

// markReread bounds how often the foreign rules are read: on a changed
// nf_tables generation (legacy iptables has none), at most this often.
// Half the reconcile interval, so each periodic reconcile reads them: a
// change is seen within one.
const markReread = reconcileInterval / 2

// forwardWindow is one ForwardBlocked sample, at least this long: each
// periodic reconcile ends one. forwardWindows in a row with flows marked
// and none translated report it (user, 2026-10-31).
const (
	forwardWindow  = reconcileInterval / 2
	forwardWindows = 2
)

// markRead is the foreign rules as last read: nf_tables' at generation
// gen, and legacy iptables'.
type markRead struct {
	at          time.Time
	gen         uint32
	nft, legacy []datapath.MarkUse
}

// fwdWindow is a family's slot flow counts when its window began, in the
// table whose counters they are, and how many windows in a row had flows
// marked and none translated.
type fwdWindow struct {
	table              *datapath.Table
	start              time.Time
	marked, translated uint64
	blocked            int
}

// stepForward moves w on to f, counted now in table: a new table's
// counters start a new window (the count of blocked windows stays); a
// window ended is judged, and true returned.
func stepForward(w *fwdWindow, table *datapath.Table, now time.Time, f datapath.SlotFlows) bool {
	switch {
	case w.table != table:
		*w = fwdWindow{table: table, start: now, marked: f.Marked, translated: f.Translated, blocked: w.blocked}
	case now.Sub(w.start) >= forwardWindow:
		if f.Marked > w.marked && f.Translated == w.translated {
			w.blocked++
		} else {
			w.blocked = 0
		}
		w.start, w.marked, w.translated = now, f.Marked, f.Translated
		return true
	}
	return false
}

// fieldName is the markField naming the byte at shift.
func fieldName(shift uint8) string {
	return fmt.Sprintf("0x%08x", uint32(0xff)<<shift)
}

// chooseByte is the mark byte to use (D18) and the rules conflicting with
// it: an explicit markField (spec) is never moved; an unset one keeps cur
// while it is free, else takes the first free candidate, else keeps cur.
func chooseByte(uses []datapath.MarkUse, spec string, cur uint8, tag bool) (uint8, []datapath.MarkUse) {
	conflicts := func(shift uint8) []datapath.MarkUse {
		var out []datapath.MarkUse
		for _, u := range uses {
			if u.Conflicts(shift, tag) {
				out = append(out, u)
			}
		}
		return out
	}
	if spec != "" {
		shift := markShift(purelbv2.LBNodeAgentEgressSpec{MarkField: spec})
		return shift, conflicts(shift)
	}
	c := conflicts(cur)
	if len(c) == 0 {
		return cur, nil
	}
	for _, s := range markCandidates {
		if len(conflicts(s)) == 0 {
			return s, nil
		}
	}
	return cur, c
}

// conflictMessage names the rules using the byte at shift and the bits
// each uses there.
func conflictMessage(shift uint8, uses []datapath.MarkUse) string {
	b := uint32(0xff) << shift
	var parts []string
	for _, u := range uses {
		var how []string
		for _, x := range []struct {
			what string
			bits uint32
		}{{"matches mark", u.Mark}, {"sets mark", u.MarkSet}, {"matches ct mark", u.Ct}, {"sets ct mark", u.CtSet}} {
			if x.bits&b != 0 {
				how = append(how, fmt.Sprintf("%s %#x", x.what, x.bits&b))
			}
		}
		parts = append(parts, u.Where+" ("+strings.Join(how, ", ")+")")
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)
	return fmt.Sprintf("markField %s is used by other rules: %s", fieldName(shift), strings.Join(parts, "; "))
}

// readMarks returns the foreign rules' mark uses, read again once
// markReread has passed: legacy iptables' each time, nf_tables' when its
// generation moved. Datapath goroutine only.
func (a *Agent) readMarks(st *dpState) ([]datapath.MarkUse, error) {
	now := a.now()
	r := st.marks
	if r == nil || now.Sub(r.at) >= markReread {
		gen, err := datapath.RulesetGen()
		if err != nil {
			return nil, err
		}
		legacy, err := datapath.LegacyMarks()
		if err != nil {
			return nil, err
		}
		next := &markRead{at: now, gen: gen, legacy: legacy}
		if r != nil && gen == r.gen {
			next.nft = r.nft
		} else if next.nft, err = datapath.ForeignMarks(st.nft); err != nil {
			return nil, err
		}
		st.marks, r = next, next
		logging.Debug(a.logger, "op", "egressMarks", "node", a.node, "nft", len(r.nft), "legacy", len(r.legacy),
			"generation", gen, "msg", "read the mark uses of the node's other netfilter rules")
	}
	return append(slices.Clone(r.nft), r.legacy...), nil
}

// syncMarkByte chooses the mark byte (D18) once the pod-node state is
// planned (until then the one an earlier agent pinned stays, with its
// table), and reports a conflict. A move resets the flows marked in the
// old byte (resetMoved). Datapath goroutine only.
func (a *Agent) syncMarkByte(st *dpState, w dpWant) {
	if !w.podsKnown {
		return
	}
	if st.nft == nil {
		c, err := datapath.NftConn()
		if err != nil {
			logging.Info(a.logger, "op", "egressMarks", "node", a.node, "error", err, "msg", "reading the netfilter rules; retrying")
			return
		}
		st.nft = c
	}
	uses, err := a.readMarks(st)
	if err != nil {
		// The byte in use stays, and so does what was reported.
		logging.Info(a.logger, "op", "egressMarks", "node", a.node, "error", err, "msg", "reading the netfilter rules; retrying")
		return
	}
	tag := w.stage != nil && !w.stage.SourceIP
	shift, conflicts := chooseByte(uses, w.spec.MarkField, st.shift, tag)
	if shift != st.shift {
		if w.spec.MarkField == "" {
			markMoves.Inc()
			logging.Info(a.logger, "op", "egressMarks", "node", a.node, "from", fieldName(st.shift), "to", fieldName(shift),
				"reason", conflictMessage(st.shift, slices.DeleteFunc(slices.Clone(uses),
					func(u datapath.MarkUse) bool { return !u.Conflicts(st.shift, tag) })),
				"msg", "moved egress's mark byte away from one other rules use; its flows are reset")
		} else {
			logging.Info(a.logger, "op", "egressMarks", "node", a.node, "from", fieldName(st.shift), "to", fieldName(shift),
				"msg", "markField changed; egress's flows are reset")
		}
		from := st.shift
		st.movedFrom = &from
		st.shift = shift
	}
	msg := ""
	if len(conflicts) > 0 {
		msg = conflictMessage(shift, conflicts)
	}
	if msg != st.markConflict {
		if msg != "" {
			logging.Info(a.logger, "op", "egressMarks", "node", a.node, "markField", fieldName(shift), "conflict", msg,
				"msg", "MarkConflict: other netfilter rules use egress's mark byte")
		} else if st.markConflict != "" {
			logging.Info(a.logger, "op", "egressMarks", "node", a.node, "markField", fieldName(shift),
				"msg", "MarkConflict cleared: no other rule uses egress's mark byte")
		}
		st.markConflict = msg
	}
	markConflict.Set(b2f(msg != ""))
}

// resetMoved deletes, once the mark byte moved, the conntrack entries of
// this node's pods carrying a slot value in the old byte: their packets
// would no longer get their slot back (D18). Entries of other sources in
// that byte are another user's and stay. Kept for the next apply on a
// failure. Datapath goroutine only.
func (a *Agent) resetMoved(st *dpState) {
	if st.movedFrom == nil {
		return
	}
	var errs []error
	n := 0
	for v := uint8(1); v <= slotBudget; v++ {
		flows, err := datapath.CtFlows(*st.movedFrom, v)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range flows {
			if !st.podIPs[f.Src] {
				continue
			}
			if err := datapath.CtDelete(f); err != nil {
				errs = append(errs, err)
				continue
			}
			n++
		}
	}
	if n > 0 {
		ctDeleted.WithLabelValues("markMoved").Add(float64(n))
		logging.Info(a.logger, "op", "egressMarks", "node", a.node, "flows", n, "from", fieldName(*st.movedFrom),
			"msg", "reset egress's flows marked in the old mark byte: deleted their conntrack entries")
	}
	if err := errors.Join(errs...); err != nil {
		logging.Info(a.logger, "op", "egressMarks", "node", a.node, "error", err,
			"msg", "deleting the flows of the old mark byte; retrying")
		return
	}
	st.movedFrom = nil
}

// syncForward samples the slots' flow counters (D25): a family whose new
// flows were marked, none translated, for forwardWindows windows in a row
// is reported ForwardBlocked; a window with one translated, or none
// marked, clears it. Slots routed to prohibit are left out: theirs are
// dropped by design. Datapath goroutine only.
func (a *Agent) syncForward(st *dpState) {
	if st.table == nil || st.table.Pods == nil {
		st.fwd = [2]fwdWindow{}
		a.setForwardBlocked(st, nil)
		return
	}
	counts, err := datapath.ForwardCounts(st.nft, *st.table.Pods)
	if err != nil {
		logging.Info(a.logger, "op", "egressForward", "node", a.node, "error", err, "msg", "reading the slots' flow counters; retrying")
		return
	}
	var sum [2]datapath.SlotFlows
	for id, f := range counts {
		if st.prohibited[id] {
			continue
		}
		i := 0
		if st.table.Pods.Slots[id].Addr.Is6() {
			i = 1
		}
		sum[i].Marked += f.Marked
		sum[i].Translated += f.Translated
	}
	now := a.now()
	var blocked []string
	for i, fam := range []string{"IPv4", "IPv6"} {
		w := &st.fwd[i]
		prev := *w
		if stepForward(w, st.table, now, sum[i]) {
			logging.Debug(a.logger, "op", "egressForward", "node", a.node, "family", fam, "marked", sum[i].Marked-prev.marked,
				"translated", sum[i].Translated-prev.translated, "blocked", w.blocked, "msg", "egress flow window")
		}
		if w.blocked >= forwardWindows {
			blocked = append(blocked, fam)
		}
	}
	a.setForwardBlocked(st, blocked)
}

// setForwardBlocked records the families reported ForwardBlocked.
func (a *Agent) setForwardBlocked(st *dpState, blocked []string) {
	if !slices.Equal(blocked, st.forwardBlocked) {
		if len(blocked) > 0 {
			logging.Info(a.logger, "op", "egressForward", "node", a.node, "families", blocked,
				"msg", "ForwardBlocked: pods' new egress flows are marked but none reaches its SNAT")
		} else {
			logging.Info(a.logger, "op", "egressForward", "node", a.node, "msg", "ForwardBlocked cleared")
		}
		st.forwardBlocked = blocked
	}
	for _, fam := range []string{"IPv4", "IPv6"} {
		forwardBlocked.WithLabelValues(fam).Set(b2f(slices.Contains(blocked, fam)))
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
