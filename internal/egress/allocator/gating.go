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

package allocator

import (
	"sort"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/egress"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

const (
	// lagLeases is the lag-eviction timeout in lease durations: longer
	// than a partitioned routed advertiser's withdrawal (3), so it
	// withdraws before it is evicted (§8.8; user, 2026-10-23).
	lagLeases = 6

	// drainMax is the longest a slice stays Draining (§5.6).
	drainMax = 5 * time.Minute

	// defaultLeaseDuration applies to a Lease without a duration.
	defaultLeaseDuration = 10 * time.Second
)

// Informer index names.
const (
	// idxAnnouncing indexes EgressNodeStates by the EgressEndpoints their
	// status.announcing lists.
	idxAnnouncing = "announcing"
	// idxClaims indexes EgressNodeStates by the EgressEndpoints they claim.
	idxClaims = "claims"
	// idxSliceNode indexes EgressEndpoints by the nodes their slices name.
	idxSliceNode = "sliceNode"
)

func announcingIndex(obj interface{}) ([]string, error) {
	ns, ok := obj.(*purelbv2.EgressNodeState)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(ns.Status.Announcing))
	for _, a := range ns.Status.Announcing {
		out = append(out, a.Endpoints)
	}
	return out, nil
}

func claimsIndex(obj interface{}) ([]string, error) {
	ns, ok := obj.(*purelbv2.EgressNodeState)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(ns.Status.Claims))
	for _, c := range ns.Status.Claims {
		out = append(out, c.Endpoints)
	}
	return out, nil
}

func sliceNodeIndex(obj interface{}) ([]string, error) {
	ep, ok := obj.(*purelbv2.EgressEndpoints)
	if !ok {
		return nil, nil
	}
	return sliceNodes(ep), nil
}

// sliceNodes are the nodes an EgressEndpoints' slices name.
func sliceNodes(ep *purelbv2.EgressEndpoints) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ep.Spec.Slices {
		if s.Node != "" && !seen[s.Node] {
			seen[s.Node] = true
			out = append(out, s.Node)
		}
	}
	return out
}

// lease is node's election Lease.
func (r *Reconciler) lease(node string) *coordinationv1.Lease {
	obj, ok, _ := r.leases.GetByKey(r.namespace + "/" + purelbv2.LeasePrefix + node)
	if !ok {
		return nil
	}
	return obj.(*coordinationv1.Lease)
}

func leaseDuration(l *coordinationv1.Lease) time.Duration {
	if l != nil && l.Spec.LeaseDurationSeconds != nil && *l.Spec.LeaseDurationSeconds > 0 {
		return time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}
	return defaultLeaseDuration
}

// leaseLive reports whether node's Lease is unexpired.
func (r *Reconciler) leaseLive(node string) bool {
	l := r.lease(node)
	if l == nil || l.Spec.RenewTime == nil {
		return false
	}
	return r.now().Before(l.Spec.RenewTime.Add(leaseDuration(l)))
}

// announcers are the nodes reporting an EgressEndpoints in
// status.announcing, with the generation each has installed.
func (r *Reconciler) announcers(ee string) map[string]int64 {
	out := map[string]int64{}
	objs, _ := r.nodeStateIdx.ByIndex(idxAnnouncing, ee)
	for _, o := range objs {
		ns := o.(*purelbv2.EgressNodeState)
		for _, a := range ns.Status.Announcing {
			if a.Endpoints == ee {
				out[ns.Name] = a.Generation
			}
		}
	}
	return out
}

// claimants are the nodes claiming a slice of an EgressEndpoints.
func (r *Reconciler) claimants(ee string) []string {
	objs, _ := r.nodeStateIdx.ByIndex(idxClaims, ee)
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(*purelbv2.EgressNodeState).Name)
	}
	return out
}

// gate reports whether every gating node has acked cur's generation, so
// its slices may change (§5.7), and the excludedNodes to publish. Gating
// nodes are the nodes reporting the address announced with a live Lease,
// less the excluded. One behind for lagLeases of its Lease duration is
// evicted (NodesLagging); an excluded node rejoins once it has caught up
// or stops reporting the address.
func (r *Reconciler) gate(run *run, cur *purelbv2.EgressEndpoints, owner string) (bool, []string) {
	now := r.now()
	ann := r.announcers(cur.Name)
	prev := r.behind[cur.Name]
	behind := map[string]time.Time{}
	excluded := map[string]bool{}
	for _, n := range cur.Spec.ExcludedNodes {
		if gen, ok := ann[n]; ok && gen < cur.Generation {
			excluded[n] = true
		}
	}
	acked := true
	for n, gen := range ann {
		if gen >= cur.Generation || excluded[n] || !r.leaseLive(n) {
			continue
		}
		since, ok := prev[n]
		if !ok {
			since = now
		}
		behind[n] = since
		timeout := lagLeases * leaseDuration(r.lease(n))
		if now.Sub(since) >= timeout {
			excluded[n] = true
			logging.Info(r.logger, "op", "egressGate", "endpoints", cur.Name, "node", n,
				"msg", "node evicted from gating for ack lag", "acked", gen, "generation", cur.Generation)
			continue
		}
		acked = false
		r.requeueAfter(timeout-now.Sub(since), owner)
	}
	if len(behind) > 0 {
		r.behind[cur.Name] = behind
	} else {
		delete(r.behind, cur.Name)
	}
	out := make([]string, 0, len(excluded))
	for n := range excluded {
		out = append(out, n)
	}
	sort.Strings(out)
	if len(out) > 0 {
		run.conds.fail(condDatapathReady, reasonNodesLagging, "%s: nodes evicted from gating for ack lag: %v", cur.Spec.Address, out)
		return acked, out
	}
	return acked, nil
}

// freedSlices are cur's slices to free: Draining slices their node
// reported drained or that reached drainMax, and every slice of a node
// that no longer exists.
func (r *Reconciler) freedSlices(cur *purelbv2.EgressEndpoints, owner string) map[int32]bool {
	now := r.now()
	prev := r.drainSince[cur.Name]
	since := map[int32]time.Time{}
	freed := map[int32]bool{}
	for _, s := range cur.Spec.Slices {
		if s.Node == "" {
			continue
		}
		if _, ok, _ := r.nodes.GetByKey(s.Node); !ok {
			freed[s.Index] = true
			continue
		}
		if s.State != sliceDraining {
			continue
		}
		if r.drained(s.Node, cur.Name, s.Index) {
			freed[s.Index] = true
			continue
		}
		t, ok := prev[s.Index]
		if !ok {
			t = now
		}
		if now.Sub(t) >= drainMax {
			freed[s.Index] = true
			logging.Info(r.logger, "op", "egressDrain", "endpoints", cur.Name, "node", s.Node, "index", s.Index,
				"msg", "slice reached the drain maximum; freed")
			continue
		}
		since[s.Index] = t
		r.requeueAfter(drainMax-now.Sub(t), owner)
	}
	if len(since) > 0 {
		r.drainSince[cur.Name] = since
	} else {
		delete(r.drainSince, cur.Name)
	}
	return freed
}

// drained reports whether node lists slice index of ee in status.drained.
func (r *Reconciler) drained(node, ee string, index int32) bool {
	ns, err := r.nodeStates.Get(node)
	if err != nil {
		return false
	}
	for _, d := range ns.Status.Drained {
		if d.Endpoints == ee && d.Index == index {
			return true
		}
	}
	return false
}

// slotsUsed is how many slices node holds across every address.
func (r *Reconciler) slotsUsed(node string) int {
	objs, _ := r.endpointsIdx.ByIndex(idxSliceNode, node)
	n := 0
	for _, o := range objs {
		for _, s := range o.(*purelbv2.EgressEndpoints).Spec.Slices {
			if s.Node == node {
				n++
			}
		}
	}
	return n
}

// slotLimit is how many slices a policy may hold on node: the slot
// budget, less the node's reservedClusterSlots for a namespaced Egress
// (§5.10).
func (r *Reconciler) slotLimit(node string, kind string) int {
	if kind == egress.KindClusterEgress {
		return slotBudget
	}
	obj, ok, _ := r.nodes.GetByKey(node)
	if !ok {
		return slotBudget
	}
	agents, _ := r.agents.List(labels.Everything())
	for _, a := range purelbv2.AgentsForNode(agents, obj.(*corev1.Node).Labels) {
		if a.Spec.Egress != nil {
			return slotBudget - int(a.Spec.Egress.ReservedClusterSlots)
		}
	}
	return slotBudget
}

// requeueAfter enqueues key after d, once per deadline: a later request
// for the same key isn't scheduled while an earlier one is pending.
func (r *Reconciler) requeueAfter(d time.Duration, key string) {
	at := r.now().Add(d)
	if t, ok := r.timers[key]; ok && t.After(r.now()) && !t.After(at) {
		return
	}
	r.timers[key] = at
	time.AfterFunc(d, func() { r.enqueue(key, false) })
}
