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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"purelb.io/internal/egress"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Announced reasons (§4.8).
const reasonSplitBrain = "SplitBrain"

// splitBrainLeases is how long two nodes may both report a
// SingleAnnouncer address before SplitBrain, in lease durations (D1).
const splitBrainLeases = 2

// published are the conditions the allocator reports; the agents' come
// with Phase 4. Ready is True when all of them are.
var published = []string{condAllocated, condContained, condAnnounced, condDatapathReady, condPortCapacity, condPolicyEffective}

var (
	slicesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "slices",
		Help: "Slices of an egress address by state (D28).",
	}, []string{"ip", "state"})
	sliceUtilization = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "slice_utilization_ratio",
		Help: "Share of an egress address's usable slices in use, draining included.",
	}, []string{"ip"})
	laggingNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "lagging_nodes",
		Help: "Nodes evicted from an egress policy's gating for ack lag (D6).",
	}, []string{"egress"})
)

func init() {
	prometheus.MustRegister(slicesGauge, sliceUtilization, laggingNodes)
}

// redacted reports whether the group hides nodes, slices and holders.
func redacted(sg *purelbv2.ServiceGroup) bool {
	return sg != nil && sg.Spec.Egress != nil && sg.Spec.Egress.Tenancy != nil && sg.Spec.Egress.Tenancy.StatusDetail == "Redacted"
}

// policyObject is the policy's API object, for events.
func (r *Reconciler) policyObject(ref egress.PolicyRef) runtime.Object {
	if ref.Kind == egress.KindClusterEgress {
		if c, err := r.clusterEgresses.Get(ref.Name); err == nil {
			return c
		}
		return nil
	}
	if e, err := r.egresses.Egresses(ref.Namespace).Get(ref.Name); err == nil {
		return e
	}
	return nil
}

// event records an event about a policy. A node-bearing event of a
// Redacted group goes to the address's EgressEndpoints, in the PureLB
// namespace, instead (D20).
func (r *Reconciler) event(ref egress.PolicyRef, ep *purelbv2.EgressEndpoints, sg *purelbv2.ServiceGroup, nodes bool,
	warning bool, reason, format string, args ...interface{}) {
	obj := r.policyObject(ref)
	if nodes && redacted(sg) {
		if ep == nil {
			return
		}
		obj = ep
	}
	if obj == nil {
		return
	}
	typ := corev1.EventTypeNormal
	if warning {
		typ = corev1.EventTypeWarning
	}
	r.recorder.Eventf(obj, typ, reason, format, args...)
}

// syncStatus publishes the policy's status (§4.8): selection, addresses
// with their slices and capacity, and the allocator's conditions.
func (r *Reconciler) syncStatus(run *run, addrs []address, eps map[string]*purelbv2.EgressEndpoints) {
	p := run.p
	st := purelbv2.EgressStatus{
		ObservedGeneration:    p.meta.Generation,
		SelectedPods:          run.sel.selected,
		ExcludedPods:          run.sel.excluded,
		ExcludedPodsTruncated: run.sel.truncated,
		Conditions:            slices.Clone(p.status.Conditions),
	}
	hide := redacted(run.group)
	lagging := 0
	for _, a := range addrs {
		ep, ok := eps[egress.EndpointsName(p.ref, a.ip)]
		if !ok {
			continue
		}
		as := purelbv2.EgressAddressStatus{Address: ep.Spec.Address, Family: ep.Spec.Family, Pool: ep.Spec.Pool, Placement: ep.Spec.Placement}
		if !hide {
			as.Announcers = ep.Status.ObservedAnnouncers
			for _, s := range ep.Spec.Slices {
				if s.Node != "" {
					lo := s.Index * ep.Spec.SliceSize
					as.Slices = append(as.Slices, purelbv2.EgressSliceStatus{Node: s.Node, State: s.State,
						Ports: fmt.Sprintf("%d-%d", lo, lo+ep.Spec.SliceSize-1)})
				}
			}
		}
		if a.identity == "" {
			as.Capacity = r.capacity(ep, hide)
		}
		lagging += len(ep.Spec.ExcludedNodes)
		st.Addresses = append(st.Addresses, as)
	}
	laggingNodes.WithLabelValues(p.ref.String()).Set(float64(lagging))

	now := metav1.NewTime(r.now())
	ready := true
	for _, typ := range published {
		c := metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "AsExpected", ObservedGeneration: p.meta.Generation, LastTransitionTime: now}
		if f, failed := run.conds[typ]; failed {
			c.Status, c.Reason, c.Message = metav1.ConditionFalse, f.reason, f.message
			ready = false
		}
		apimeta.SetStatusCondition(&st.Conditions, c)
	}
	rc := metav1.Condition{Type: condReady, Status: metav1.ConditionTrue, Reason: "AsExpected", ObservedGeneration: p.meta.Generation, LastTransitionTime: now}
	if !ready {
		rc.Status, rc.Reason, rc.Message = metav1.ConditionFalse, "NotReady", "a condition is False"
	}
	apimeta.SetStatusCondition(&st.Conditions, rc)

	key := p.ref.Kind + "/" + p.ref.Namespace + "/" + p.ref.Name + "/status"
	if apiequality.Semantic.DeepEqual(p.status, st) || r.statusWritten(key, st) {
		return
	}
	patch := statusPatch(st)
	r.write(p.ref.String(), key, false, func(ctx context.Context) error {
		var err error
		if p.cluster() {
			_, err = r.purelb.PurelbV2().ClusterEgresses().Patch(ctx, p.ref.Name, types.JSONPatchType, patch, metav1.PatchOptions{}, "status")
		} else {
			_, err = r.purelb.PurelbV2().Egresses(p.ref.Namespace).Patch(ctx, p.ref.Name, types.JSONPatchType, patch, metav1.PatchOptions{}, "status")
		}
		return err
	})
}

// statusWritten reports whether status (or a spec or label the allocator
// alone writes) is what was last submitted for key, and records it
// otherwise: the informer copy lags the allocator's own writes, so
// comparing with it alone rewrites the same value until it catches up. A
// failed write forgets key (writeDone), so it is retried.
func (r *Reconciler) statusWritten(key string, status interface{}) bool {
	if last, ok := r.lastStatus[key]; ok && apiequality.Semantic.DeepEqual(last, status) {
		return true
	}
	r.lastStatus[key] = status
	return false
}

// statusPatch is a JSON patch replacing an object's whole status, and
// nothing else: a status write never carries a stale copy of the rest.
func statusPatch(status interface{}) []byte {
	b, _ := json.Marshal([]map[string]interface{}{{"op": "add", "path": "/status", "value": status}})
	return b
}

// specPatch is a JSON patch replacing an object's whole spec.
func specPatch(spec interface{}) []byte {
	b, _ := json.Marshal([]map[string]interface{}{{"op": "add", "path": "/spec", "value": spec}})
	return b
}

// capacity counts an address's slices (D28) and sets its gauges, which a
// Redacted group doesn't export.
func (r *Reconciler) capacity(ep *purelbv2.EgressEndpoints, hide bool) *purelbv2.EgressCapacity {
	var ports []int32
	for _, sp := range ep.Spec.ServicePorts {
		ports = append(ports, sp.Port)
	}
	for _, rp := range ep.Spec.Reserved {
		ports = append(ports, rp.Port)
	}
	first, last := sliceRange(ep.Spec.SliceSize)
	c := &purelbv2.EgressCapacity{}
	for i := first; i <= last; i++ {
		if !covers(ep.Spec.SliceSize, i, ports) {
			c.SlicesTotal++
		}
	}
	for _, s := range ep.Spec.Slices {
		switch {
		case s.State == sliceDraining:
			c.Draining++
		case s.Node != "":
			c.Active++
		}
	}
	c.Free = c.SlicesTotal - c.Active - c.Draining
	if !hide {
		slicesGauge.WithLabelValues(ep.Spec.Address, "active").Set(float64(c.Active))
		slicesGauge.WithLabelValues(ep.Spec.Address, "draining").Set(float64(c.Draining))
		slicesGauge.WithLabelValues(ep.Spec.Address, "free").Set(float64(c.Free))
		if c.SlicesTotal > 0 {
			sliceUtilization.WithLabelValues(ep.Spec.Address).Set(float64(c.Active+c.Draining) / float64(c.SlicesTotal))
		}
	}
	return c
}

// forgetAddress removes a released address's series.
func forgetAddress(ip string) {
	for _, s := range []string{"active", "draining", "free"} {
		slicesGauge.DeleteLabelValues(ip, s)
	}
	sliceUtilization.DeleteLabelValues(ip)
}

// observeAnnouncers writes an address's observedAnnouncers, the nodes
// with a live Lease reporting it announced (never read by agents, D1),
// with events for joins and leaves, and reports SplitBrain when a
// SingleAnnouncer address has had two for two lease durations.
func (r *Reconciler) observeAnnouncers(run *run, ep *purelbv2.EgressEndpoints) {
	var obs []string
	longest := time.Duration(0)
	for n := range r.announcers(ep.Name) {
		if r.leaseLive(n) {
			obs = append(obs, n)
			longest = max(longest, leaseDuration(r.lease(n)))
		}
	}
	sort.Strings(obs)

	ref := run.p.ref
	if ep.Spec.Placement == "SingleAnnouncer" && len(obs) > 1 {
		since, ok := r.split[ep.Name]
		if !ok {
			since = r.now()
			r.split[ep.Name] = since
		}
		dwell := splitBrainLeases * longest
		if r.now().Sub(since) >= dwell {
			run.conds.fail(condAnnounced, reasonSplitBrain, "%s is announced by %d nodes", ep.Spec.Address, len(obs))
			if !r.splitReported[ep.Name] {
				r.splitReported[ep.Name] = true
				r.event(ref, ep, run.group, true, true, reasonSplitBrain, "%s is announced by %v", ep.Spec.Address, obs)
			}
		} else {
			r.requeueAfter(dwell-r.now().Sub(since), ref.String())
		}
	} else {
		delete(r.split, ep.Name)
		delete(r.splitReported, ep.Name)
	}

	old := ep.Status.ObservedAnnouncers
	key := "EgressEndpoints/" + ep.Name + "/status"
	if slices.Equal(old, obs) || r.statusWritten(key, obs) {
		return
	}
	for _, n := range obs {
		if !slices.Contains(old, n) {
			r.event(ref, ep, run.group, true, false, "AnnouncerJoined", "%s announced by %s", ep.Spec.Address, n)
		}
	}
	for _, n := range old {
		if !slices.Contains(obs, n) {
			r.event(ref, ep, run.group, true, false, "AnnouncerLeft", "%s no longer announced by %s", ep.Spec.Address, n)
		}
	}
	patch := statusPatch(purelbv2.EgressEndpointsStatus{ObservedAnnouncers: obs})
	client := r.purelb.PurelbV2().EgressEndpointses(r.namespace)
	name := ep.Name
	r.write(ref.String(), key, false, func(ctx context.Context) error {
		_, err := client.Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{}, "status")
		return err
	})
}

// sliceEvents records an address's slice transitions between two specs.
func (r *Reconciler) sliceEvents(run *run, ep *purelbv2.EgressEndpoints, from, to []purelbv2.EgressSlice) {
	was := map[int32]purelbv2.EgressSlice{}
	for _, s := range from {
		was[s.Index] = s
	}
	now := map[int32]bool{}
	for _, s := range to {
		now[s.Index] = true
		if s.Node == "" {
			continue
		}
		w, ok := was[s.Index]
		switch {
		case s.State == sliceActive && (!ok || w.State != sliceActive || w.Node != s.Node):
			r.event(run.p.ref, ep, run.group, true, false, "SliceActivated", "%s slice %d active on %s", ep.Spec.Address, s.Index, s.Node)
		case s.State == sliceDraining && ok && w.State != sliceDraining:
			r.event(run.p.ref, ep, run.group, true, false, "SliceDraining", "%s slice %d draining on %s", ep.Spec.Address, s.Index, s.Node)
		}
	}
	for _, w := range from {
		if !now[w.Index] && w.Node != "" && w.State != slicePending {
			r.event(run.p.ref, ep, run.group, true, false, "SliceFreed", "%s slice %d freed on %s", ep.Spec.Address, w.Index, w.Node)
		}
	}
}

// syncNodeLabel applies the D29 egress-capable label from the node's
// reported capability: present exactly when its EgressNodeState reports
// egressCapable. The allocator's node patches are limited to this label
// by a ValidatingAdmissionPolicy.
func (r *Reconciler) syncNodeLabel(node string) {
	obj, ok, _ := r.nodes.GetByKey(node)
	if !ok {
		return
	}
	n := obj.(*corev1.Node)
	capable := false
	if ns, err := r.nodeStates.Get(node); err == nil && ns.Status.Capability != nil {
		capable = ns.Status.Capability.EgressCapable
	}
	labelled := n.Labels[purelbv2.EgressCapableLabel] == "true"
	if capable == labelled || r.statusWritten("Node/"+node, capable) {
		return
	}
	var value interface{} // null removes the label
	if capable {
		value = "true"
	}
	patch, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{purelbv2.EgressCapableLabel: value}}})
	logging.Info(r.logger, "op", "egressNodeLabel", "node", node, "capable", capable)
	r.write(nodePrefix+node, "Node/"+node, false, func(ctx context.Context) error {
		_, err := r.kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
}
