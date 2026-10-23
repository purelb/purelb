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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	selectionop "k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Informer indexes that keep a reconcile's cost to the policy's own pods
// and objects rather than everything in a namespace.
const (
	// idxOwner indexes EgressEndpoints and EgressMembers by their owner
	// policy's key, whatever its UID: a policy recreated under the same
	// name takes them over.
	idxOwner = "owner"
	// idxPodLabel indexes pods by "namespace/key=value" of each label.
	idxPodLabel = "podLabel"
)

func ownerIndex(obj interface{}) ([]string, error) {
	var l map[string]string
	switch o := obj.(type) {
	case *purelbv2.EgressEndpoints:
		l = o.Labels
	case *purelbv2.EgressMembers:
		l = o.Labels
	}
	if l[egress.LabelName] == "" {
		return nil, nil
	}
	return []string{egress.PolicyRef{Kind: l[egress.LabelKind], Namespace: l[egress.LabelNamespace], Name: l[egress.LabelName]}.String()}, nil
}

func podLabelIndex(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(pod.Labels))
	for k, v := range pod.Labels {
		out = append(out, pod.Namespace+"/"+k+"="+v)
	}
	return out, nil
}

// ownedEndpoints are the EgressEndpoints a policy owns.
func (r *Reconciler) ownedEndpoints(ref egress.PolicyRef) []*purelbv2.EgressEndpoints {
	objs, _ := r.endpointsIdx.ByIndex(idxOwner, ref.String())
	out := make([]*purelbv2.EgressEndpoints, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(*purelbv2.EgressEndpoints))
	}
	return out
}

// ownedMembers are the EgressMembers a policy owns.
func (r *Reconciler) ownedMembers(ref egress.PolicyRef) []*purelbv2.EgressMembers {
	objs, _ := r.membersIdx.ByIndex(idxOwner, ref.String())
	out := make([]*purelbv2.EgressMembers, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(*purelbv2.EgressMembers))
	}
	return out
}

// latestEndpoints is the newest copy of an EgressEndpoints: the
// informer's, or the one the allocator's own last write returned while the
// informer has yet to see it. Planning from it builds on what was written
// (read-your-writes) instead of failing the write's resourceVersion check
// and requeueing.
func (r *Reconciler) latestEndpoints(name string) (*purelbv2.EgressEndpoints, bool) {
	cur, err := r.endpoints.EgressEndpointses(r.namespace).Get(name)
	if v, ok := r.written.Load(name); ok {
		w := v.(*purelbv2.EgressEndpoints)
		if err != nil || w.Generation > cur.Generation {
			return w, true
		}
		r.written.Delete(name) // the informer has caught up
	}
	return cur, err == nil
}

// busy reports whether an EgressEndpoints has a write in flight, and if so
// has owner rerun when it finishes: a plan made now would start from the
// state before that write and fail its resourceVersion check (one write
// in flight per address, as one generation is).
func (r *Reconciler) busy(name, owner string, priority bool) bool {
	key := "EgressEndpoints/" + name
	if !r.writer.inProgress(key) {
		return false
	}
	d := r.deferred[key]
	r.deferred[key] = writeOwner{key: owner, priority: priority || d.priority}
	return true
}

// wrote records a successful EgressEndpoints write's result for
// latestEndpoints, or forgets the object when it is gone. It runs on a
// writer worker.
func (r *Reconciler) wrote(name string, out *purelbv2.EgressEndpoints, err error) {
	switch {
	case err == nil && out != nil:
		r.written.Store(name, out)
	case err == nil || apierrors.IsNotFound(err):
		r.written.Delete(name)
	}
}

// candidatePods are the pods in ns that sel may match: those carrying the
// value of one of its equality or set requirements, or every pod in ns
// when it has none. The caller still matches sel.
func (r *Reconciler) candidatePods(ns string, sel labels.Selector) []interface{} {
	reqs, _ := sel.Requirements()
	for _, rq := range reqs {
		switch rq.Operator() {
		case selectionop.Equals, selectionop.DoubleEquals, selectionop.In:
			var out []interface{}
			for _, v := range rq.Values().UnsortedList() {
				objs, _ := r.pods.ByIndex(idxPodLabel, ns+"/"+rq.Key()+"="+v)
				out = append(out, objs...)
			}
			return out
		}
	}
	objs, _ := r.pods.ByIndex(cache.NamespaceIndex, ns)
	return objs
}
