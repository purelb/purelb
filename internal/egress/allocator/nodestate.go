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
	"sort"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// nodePrefix keys a node's EgressNodeState spec on the queue.
const nodePrefix = "node:"

// syncNodeState writes a node's EgressNodeState spec: its slices of every
// address, as the EgressEndpoints publish them (D6). The node acks it in
// status.observedGeneration. A node with a claim outstanding is written on
// the priority lane (D-conc).
func (r *Reconciler) syncNodeState(node string) error {
	r.syncNodeLabel(node)
	objs, _ := r.endpointsIdx.ByIndex(idxSliceNode, node)
	var slices []purelbv2.EgressNodeSlice
	for _, o := range objs {
		ep := o.(*purelbv2.EgressEndpoints)
		for _, s := range ep.Spec.Slices {
			if s.Node == node {
				slices = append(slices, purelbv2.EgressNodeSlice{Endpoints: ep.Name, Address: ep.Spec.Address, Index: s.Index, State: s.State})
			}
		}
	}
	sort.Slice(slices, func(i, j int) bool {
		if slices[i].Endpoints != slices[j].Endpoints {
			return slices[i].Endpoints < slices[j].Endpoints
		}
		return slices[i].Index < slices[j].Index
	})
	spec := purelbv2.EgressNodeStateSpec{Slices: slices}
	client := r.purelb.PurelbV2().EgressNodeStates()
	key := nodePrefix + node

	cur, err := r.nodeStates.Get(node)
	if err != nil {
		if len(slices) == 0 {
			return nil
		}
		obj, ok, _ := r.nodes.GetByKey(node)
		if !ok {
			return nil // a gone node's slices are freed by its addresses
		}
		n := obj.(*corev1.Node)
		want := &purelbv2.EgressNodeState{
			ObjectMeta: metav1.ObjectMeta{Name: node, OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Node", Name: n.Name, UID: n.UID}}},
			Spec: spec,
		}
		r.write(key, "EgressNodeState/"+node, true, func(ctx context.Context) error {
			_, err := client.Create(ctx, want, metav1.CreateOptions{})
			return err
		})
		return nil
	}
	if apiequality.Semantic.DeepEqual(cur.Spec, spec) || r.statusWritten("EgressNodeState/"+node, spec) {
		return nil
	}
	// A patch of /spec alone: the allocator is the spec's only writer and
	// builds it from the EgressEndpoints, so a resourceVersion check would
	// only collide with the agent's own status writes.
	patch := specPatch(spec)
	r.write(key, "EgressNodeState/"+node, len(cur.Status.Claims) > 0, func(ctx context.Context) error {
		_, err := client.Patch(ctx, node, types.JSONPatchType, patch, metav1.PatchOptions{})
		return err
	})
	return nil
}

// nodeKeys are the queue keys of every node with an EgressNodeState or a
// slice, for ForceSync.
func (r *Reconciler) nodeKeys() []string {
	seen := map[string]bool{}
	nss, _ := r.nodeStates.List(labels.Everything())
	for _, ns := range nss {
		seen[ns.Name] = true
	}
	for _, n := range r.endpointsIdx.ListIndexFuncValues(idxSliceNode) {
		seen[n] = true
	}
	keys := make([]string, 0, len(seen))
	for n := range seen {
		keys = append(keys, nodePrefix+n)
	}
	sort.Strings(keys)
	return keys
}

// handleEndpoints reconciles an EgressEndpoints' owner on every event,
// and rewrites the spec of each node whose slices of it changed, on the
// priority lane: claims and activations are the claim path.
func (r *Reconciler) handleEndpoints(inf cache.SharedIndexInformer) {
	changed := func(oldObj, newObj interface{}) {
		o, _ := unwrap(oldObj).(*purelbv2.EgressEndpoints)
		n, _ := unwrap(newObj).(*purelbv2.EgressEndpoints)
		for _, k := range r.ownerOf(firstNonNil(n, o)) {
			r.enqueue(k, false)
		}
		if e, _ := firstNonNil(n, o).(*purelbv2.EgressEndpoints); e != nil {
			r.enqueue(addressPrefix+e.Name, false)
		}
		if n != nil && n.Spec.State == stateQuarantined {
			r.enqueue(tombstonePrefix+n.Name, false)
		}
		for node := range slicesChanged(o, n) {
			r.enqueue(nodePrefix+node, true)
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { changed(nil, obj) },
		UpdateFunc: changed,
		DeleteFunc: func(obj interface{}) { changed(obj, nil) },
	})
}

// handleNodeStates reconciles the owners of the addresses a node's status
// change concerns: a new claim or ack on the priority lane, a drained
// report on the normal one. A new or deleted EgressNodeState rewrites its
// spec.
func (r *Reconciler) handleNodeStates(inf cache.SharedIndexInformer) {
	owners := func(ees map[string]bool, priority bool) {
		for ee := range ees {
			if ep, err := r.endpoints.EgressEndpointses(r.namespace).Get(ee); err == nil {
				for _, k := range r.ownerOf(ep) {
					r.enqueue(k, priority)
				}
			}
		}
	}
	all := func(obj interface{}) {
		ns, ok := unwrap(obj).(*purelbv2.EgressNodeState)
		if !ok {
			return
		}
		r.enqueue(nodePrefix+ns.Name, false)
		owners(statusRefs(ns), false)
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: all,
		UpdateFunc: func(oldObj, newObj interface{}) {
			o, ok1 := oldObj.(*purelbv2.EgressNodeState)
			n, ok2 := newObj.(*purelbv2.EgressNodeState)
			if !ok1 || !ok2 {
				return
			}
			owners(claimsAndAcksChanged(o, n), true)
			owners(drainedChanged(o, n), false)
			capChanged := !apiequality.Semantic.DeepEqual(o.Status.Capability, n.Status.Capability)
			if capChanged {
				r.enqueue(nodePrefix+n.Name, false)
			}
			if capChanged || !apiequality.Semantic.DeepEqual(o.Status.Conditions, n.Status.Conditions) ||
				!apiequality.Semantic.DeepEqual(o.Status.AddressFaults, n.Status.AddressFaults) {
				// The policies whose addresses the node holds, has slices
				// of, or reports a fault with (nodeConditions).
				owners(r.reportedOn(o, n), false)
			}
		},
		DeleteFunc: all,
	})
}

// reportedOn are the EgressEndpoints a node's report bears on, before
// and after a change: those it announces, claims or drains, has slices
// of, or reports a fault with.
func (r *Reconciler) reportedOn(o, n *purelbv2.EgressNodeState) map[string]bool {
	out := statusRefs(n)
	for ee := range statusRefs(o) {
		out[ee] = true
	}
	if objs, err := r.endpointsIdx.ByIndex(idxSliceNode, n.Name); err == nil {
		for _, obj := range objs {
			out[obj.(*purelbv2.EgressEndpoints).Name] = true
		}
	}
	for _, ns := range []*purelbv2.EgressNodeState{o, n} {
		for _, f := range ns.Status.AddressFaults {
			objs, _ := r.endpointsIdx.ByIndex(idxAddress, f.Address)
			for _, obj := range objs {
				out[obj.(*purelbv2.EgressEndpoints).Name] = true
			}
		}
	}
	return out
}

func unwrap(obj interface{}) interface{} {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return t.Obj
	}
	return obj
}

func firstNonNil(a, b *purelbv2.EgressEndpoints) interface{} {
	if a != nil {
		return a
	}
	return b
}

// slicesChanged are the nodes whose slices differ between o and n.
func slicesChanged(o, n *purelbv2.EgressEndpoints) map[string]bool {
	of := func(ep *purelbv2.EgressEndpoints) map[string][]purelbv2.EgressSlice {
		m := map[string][]purelbv2.EgressSlice{}
		if ep != nil {
			for _, s := range ep.Spec.Slices {
				if s.Node != "" {
					m[s.Node] = append(m[s.Node], s)
				}
			}
		}
		return m
	}
	a, b := of(o), of(n)
	out := map[string]bool{}
	for node, s := range a {
		if !apiequality.Semantic.DeepEqual(s, b[node]) {
			out[node] = true
		}
	}
	for node := range b {
		if _, ok := a[node]; !ok {
			out[node] = true
		}
	}
	return out
}

// statusRefs are the addresses n's status names.
func statusRefs(n *purelbv2.EgressNodeState) map[string]bool {
	out := map[string]bool{}
	for _, a := range n.Status.Announcing {
		out[a.Endpoints] = true
	}
	for _, c := range n.Status.Claims {
		out[c.Endpoints] = true
	}
	for _, d := range n.Status.Drained {
		out[d.Endpoints] = true
	}
	return out
}

// claimsAndAcksChanged are the addresses whose claim or announcing entry
// differs between o and n.
func claimsAndAcksChanged(o, n *purelbv2.EgressNodeState) map[string]bool {
	out := map[string]bool{}
	ack := func(ns *purelbv2.EgressNodeState) map[string]purelbv2.EgressAnnouncing {
		m := map[string]purelbv2.EgressAnnouncing{}
		for _, a := range ns.Status.Announcing {
			m[a.Endpoints] = a
		}
		return m
	}
	claim := func(ns *purelbv2.EgressNodeState) map[string]bool {
		m := map[string]bool{}
		for _, c := range ns.Status.Claims {
			m[c.Endpoints] = true
		}
		return m
	}
	oa, na := ack(o), ack(n)
	for k, v := range na {
		if oa[k] != v {
			out[k] = true
		}
	}
	for k := range oa {
		if _, ok := na[k]; !ok {
			out[k] = true
		}
	}
	oc, nc := claim(o), claim(n)
	for k := range nc {
		if !oc[k] {
			out[k] = true
		}
	}
	return out
}

// drainedChanged are the addresses with a new drained report in n.
func drainedChanged(o, n *purelbv2.EgressNodeState) map[string]bool {
	old := map[purelbv2.EgressDrained]bool{}
	for _, d := range o.Status.Drained {
		old[d] = true
	}
	out := map[string]bool{}
	for _, d := range n.Status.Drained {
		if !old[d] {
			out[d.Endpoints] = true
		}
	}
	return out
}
