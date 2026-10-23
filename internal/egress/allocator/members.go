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

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// maxMembersPods is the most pods one EgressMembers holds (D5), as an
// EndpointSlice holds a bounded number of endpoints.
const maxMembersPods = 500

// ownerLabels labels an object a policy owns, so its handler finds the
// policy and the policy finds its objects.
func ownerLabels(p *policy) map[string]string {
	return map[string]string{
		egress.LabelKind:      p.ref.Kind,
		egress.LabelNamespace: p.ref.Namespace,
		egress.LabelName:      p.ref.Name,
		egress.LabelUID:       string(p.meta.UID),
	}
}

// ownedSelector selects the objects a policy owns, whatever their UID
// label: a policy recreated under the same name takes them over.
func ownedSelector(ref egress.PolicyRef) labels.Selector {
	return labels.SelectorFromSet(labels.Set{
		egress.LabelKind:      ref.Kind,
		egress.LabelNamespace: ref.Namespace,
		egress.LabelName:      ref.Name,
	})
}

// desiredMembers lays the selection out in EgressMembers objects of at
// most maxMembersPods pods, nodes and pods in order, so the same
// selection always produces the same objects.
func desiredMembers(run *run) []*purelbv2.EgressMembers {
	if run.revoked {
		return nil
	}
	nodes := make([]string, 0, len(run.sel.nodes))
	for n := range run.sel.nodes {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	var out []*purelbv2.EgressMembers
	var cur *purelbv2.EgressMembers
	count := 0
	for _, n := range nodes {
		for _, m := range run.sel.nodes[n] {
			if cur == nil || count == maxMembersPods {
				cur = &purelbv2.EgressMembers{ObjectMeta: metav1.ObjectMeta{
					Name: egress.MembersName(run.p.ref, len(out)), Labels: ownerLabels(run.p)}}
				out = append(out, cur)
				count = 0
			}
			nodes := cur.Spec.Nodes
			if len(nodes) == 0 || nodes[len(nodes)-1].Node != n {
				cur.Spec.Nodes = append(cur.Spec.Nodes, purelbv2.EgressMemberNode{Node: n})
			}
			last := &cur.Spec.Nodes[len(cur.Spec.Nodes)-1]
			pod := purelbv2.EgressMemberPod{Name: m.name, UID: m.uid, Identity: m.identity}
			if run.p.cluster() {
				pod.Namespace = m.namespace
			}
			last.Pods = append(last.Pods, pod)
			count++
		}
	}
	return out
}

// syncMembers writes the policy's EgressMembers: creates, updates and
// deletes against what the informer holds.
func (r *Reconciler) syncMembers(run *run) {
	if run.retained {
		return
	}
	existing, _ := r.members.EgressMemberses(r.namespace).List(ownedSelector(run.p.ref))
	have := map[string]*purelbv2.EgressMembers{}
	for _, m := range existing {
		have[m.Name] = m
	}
	r.podEvents(run, existing)
	client := r.purelb.PurelbV2().EgressMemberses(r.namespace)
	for _, want := range desiredMembers(run) {
		want.Namespace = r.namespace
		cur, ok := have[want.Name]
		delete(have, want.Name)
		switch {
		case !ok:
			r.write(run.p.ref.String(), "EgressMembers/"+want.Name, false, func(ctx context.Context) error {
				_, err := client.Create(ctx, want, metav1.CreateOptions{})
				return err
			})
		case !apiequality.Semantic.DeepEqual(cur.Spec, want.Spec) || !apiequality.Semantic.DeepEqual(cur.Labels, want.Labels):
			upd := cur.DeepCopy()
			upd.Spec, upd.Labels = want.Spec, want.Labels
			r.write(run.p.ref.String(), "EgressMembers/"+want.Name, false, func(ctx context.Context) error {
				_, err := client.Update(ctx, upd, metav1.UpdateOptions{})
				return err
			})
		}
	}
	for name := range have {
		name := name
		r.write(run.p.ref.String(), "EgressMembers/"+name, false, func(ctx context.Context) error {
			return client.Delete(ctx, name, metav1.DeleteOptions{})
		})
	}
}

// podEvents records PodSelected and PodDeselected against the policy's
// current EgressMembers.
func (r *Reconciler) podEvents(run *run, existing []*purelbv2.EgressMembers) {
	was := map[string]bool{}
	for _, m := range existing {
		for k := range membersPods(m) {
			was[k] = true
		}
	}
	now := map[string]bool{}
	for _, ms := range run.sel.nodes {
		for _, m := range ms {
			now[m.namespace+"/"+m.name] = true
		}
	}
	emit := func(key, reason, verb string) {
		if obj, ok, _ := r.pods.GetByKey(key); ok {
			r.recorder.Eventf(obj.(*corev1.Pod), corev1.EventTypeNormal, reason, "%s by %s", verb, run.p.ref.String())
		}
	}
	for k := range now {
		if !was[k] {
			emit(k, "PodSelected", "selected")
		}
	}
	for k := range was {
		if !now[k] {
			emit(k, "PodDeselected", "deselected")
		}
	}
}

// membersPods is the "namespace/name" of every pod an EgressMembers lists.
func membersPods(m *purelbv2.EgressMembers) map[string]bool {
	keys, _ := memberPodIndex(m)
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}
