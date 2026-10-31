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
	"cmp"
	"errors"
	"maps"
	"net/netip"
	"slices"

	"github.com/google/nftables"
	"github.com/prometheus/client_golang/prometheus"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var ctDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "conntrack_deleted_total",
	Help: "Egress flows whose conntrack entries the agent deleted: those left on a slot it retired " +
		"(its drain reached the maximum, or it was removed), and a deleted pod's (§5.6).",
}, []string{"reason"})

func init() {
	prometheus.MustRegister(ctDeleted)
}

// ctInUse reports whether conntrack may still carry mark field value id
// (D18): until the datapath has deleted a retired value's flows, it may.
func (a *Agent) ctInUse(id uint8) bool {
	f := a.ctFlushed.Load()
	return f == nil || !(*f)[id]
}

// ackable is the spec generation gen, which the holdings were computed
// from, when pn was planned from it too and gave every slot an id: once
// installed, the spec is in place whole (D6). 0 otherwise.
func ackable(gen int64, pn *podNode) int64 {
	if gen != pn.generation || len(pn.short) > 0 {
		return 0
	}
	return gen
}

// drainWant is this node's Draining slices for the datapath to count:
// those with a slot by its id; those without one have no flows of their
// own (a retired id's are deleted), so they are drained already.
func drainWant(spec purelbv2.EgressNodeStateSpec, pn *podNode) (map[uint8]purelbv2.EgressDrained, []purelbv2.EgressDrained) {
	byID := map[uint8]purelbv2.EgressDrained{}
	var none []purelbv2.EgressDrained
	for _, s := range spec.Slices {
		if s.State != "Draining" {
			continue
		}
		d := purelbv2.EgressDrained{Endpoints: s.Endpoints, Index: s.Index}
		if id, ok := pn.slotIDs[slotKey{endpoints: s.Endpoints, index: s.Index}]; ok {
			byID[id] = d
		} else {
			none = append(none, d)
		}
	}
	return byID, none
}

// syncConntrack keeps conntrack in step with the slots (D18, §5.6): a
// retired mark value's flows, left when its slot's drain reached the
// maximum or it was removed, are deleted (the flows are reset) and
// counted, so the value can be used again; a deleted pod's flows are
// deleted from every slot; each Draining slot's flows are counted, and
// one with none is reported drained. A failure is logged, and the
// periodic reconcile tries again. Datapath goroutine only, once the
// pod-node state is planned.
func (a *Agent) syncConntrack(st *dpState, w dpWant) {
	shift := markShift(w.spec)
	var errs []error
	flushed := map[uint8]bool{}
	for _, id := range w.retired {
		flows, err := datapath.CtFlows(shift, id)
		if err == nil && len(flows) > 0 {
			if err = datapath.CtFlush(shift, id); err == nil {
				ctDeleted.WithLabelValues("slotRetired").Add(float64(len(flows)))
				logging.Info(a.logger, "op", "egressConntrack", "node", a.node, "slot", id, "flows", len(flows),
					"msg", "reset the flows left on a retired slot")
			}
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		flushed[id] = true
	}

	// A pod gone from the node: its flows of every slot (its address may
	// be given to a new pod). One that is a pod's again is left.
	now := map[netip.Addr]bool{}
	for _, p := range w.pods {
		for _, ip := range []netip.Addr{p.v4, p.v6} {
			if ip.IsValid() {
				now[ip] = true
			}
		}
	}
	for ip := range st.podIPs {
		if !now[ip] {
			st.gone[ip] = true
		}
	}
	maps.DeleteFunc(st.gone, func(ip netip.Addr, _ bool) bool { return now[ip] })
	st.podIPs = now
	if len(st.gone) > 0 && w.stage != nil {
		n, failed := 0, false
		for _, id := range slices.Sorted(maps.Keys(w.stage.Slots)) {
			flows, err := datapath.CtFlows(shift, id)
			if err != nil {
				errs, failed = append(errs, err), true
				continue
			}
			for _, f := range flows {
				if !st.gone[f.Src] {
					continue
				}
				if err := datapath.CtDelete(f); err != nil {
					errs, failed = append(errs, err), true
					continue
				}
				n++
			}
		}
		if n > 0 {
			ctDeleted.WithLabelValues("podDeleted").Add(float64(n))
			logging.Info(a.logger, "op", "egressConntrack", "node", a.node, "pods", len(st.gone), "flows", n,
				"msg", "deleted the flows of pods gone from this node")
		}
		if !failed {
			clear(st.gone)
		}
	}

	drained := slices.Clone(w.drainedNoSlot)
	for _, id := range slices.Sorted(maps.Keys(w.draining)) {
		flows, err := datapath.CtFlows(shift, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		logging.Debug(a.logger, "op", "egressConntrack", "node", a.node, "slot", id, "endpoints", w.draining[id].Endpoints,
			"index", w.draining[id].Index, "flows", len(flows), "msg", "draining slot")
		if len(flows) == 0 {
			drained = append(drained, w.draining[id])
		}
	}
	slices.SortFunc(drained, func(x, y purelbv2.EgressDrained) int {
		return cmp.Or(cmp.Compare(x.Endpoints, y.Endpoints), cmp.Compare(x.Index, y.Index))
	})
	if old := a.drained.Load(); old == nil || !slices.Equal(*old, drained) {
		a.drained.Store(&drained)
		a.add(keyNode, false)
	}
	if old := a.ctFlushed.Load(); old == nil || !maps.Equal(*old, flushed) {
		a.ctFlushed.Store(&flushed)
		if len(flushed) > 0 {
			a.add(keyPods, false) // the values flushed may be used again
		}
	}
	if err := errors.Join(errs...); err != nil {
		logging.Info(a.logger, "op", "egressConntrack", "node", a.node, "error", err,
			"msg", "syncing conntrack with the slots; retrying")
	}
}

// unflushLive drops from the values reported flushed any w uses: one
// used again may carry flows again (datapath goroutine).
func (a *Agent) unflushLive(w dpWant) {
	f := a.ctFlushed.Load()
	if f == nil || w.stage == nil {
		return
	}
	next := maps.Clone(*f)
	maps.DeleteFunc(next, func(id uint8, _ bool) bool { _, live := w.stage.Slots[id]; return live })
	if len(next) != len(*f) {
		a.ctFlushed.Store(&next)
	}
}

// seedSlots gives prev the slot ids an earlier agent's table holds (D12),
// matched to in's slices by address and ports, so its flows' conntrack
// marks keep naming their slots. One matching none stays a slot no longer
// wanted, retired while conntrack may carry it.
func seedSlots(prev *podNode, in pnInput, recovered map[uint8]datapath.SlotNAT) {
	prev.slotIDs, prev.slots = map[slotKey]uint8{}, map[uint8]slot{}
	byNAT := map[datapath.SlotNAT]slotKey{}
	for _, s := range in.spec.Slices {
		addr, err := netip.ParseAddr(s.Address)
		if err != nil {
			continue
		}
		n := datapath.SlotNAT{Addr: addr}
		if s.Identity == "" {
			n.First, n.Last = uint16(s.Index*s.SliceSize), uint16((s.Index+1)*s.SliceSize-1)
		}
		byNAT[n] = slotKey{endpoints: s.Endpoints, index: s.Index}
	}
	for _, id := range slices.Sorted(maps.Keys(recovered)) {
		if _, ok := prev.sets[id]; ok {
			continue
		}
		if k, ok := byNAT[recovered[id]]; ok {
			if _, dup := prev.slotIDs[k]; !dup {
				prev.slotIDs[k] = id
			}
		}
		prev.slots[id] = slot{}
	}
}

// tableSlots reads the slots an earlier agent left in the nft table.
func tableSlots() (map[uint8]datapath.SlotNAT, error) {
	c, err := nftables.New()
	if err != nil {
		return nil, err
	}
	return datapath.TableSlots(c)
}
