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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Why a selected pod is excluded (EgressStatus.excludedPods[].reason).
const (
	excludedHostNetwork = "HostNetwork"
	excludedOptedOut    = "OptedOut"
	excludedNotSTS      = "NotStatefulSet"
	excludedUnsupported = "NodeUnsupported"
	excludedNotInScope  = "NamespaceNotInScope"
	excludedOutranked   = "Outranked"
	maxExcludedPods     = 100
	admittedLabel       = "purelb.io/cluster-egress-admitted"
)

// member is one selected pod.
type member struct {
	namespace, name, uid string
	// identity keys a PerPod address: the StatefulSet pod's name, with
	// its namespace for a ClusterEgress, whose pods span namespaces.
	identity string
}

// selection is a policy's selected pods per node, and the pods it
// excluded.
type selection struct {
	nodes     map[string][]member
	selected  int32
	excluded  []purelbv2.EgressExcludedPod
	truncated int32 // excluded pods beyond maxExcludedPods
}

func (s *selection) exclude(pod *corev1.Pod, reason string) {
	if len(s.excluded) >= maxExcludedPods {
		s.truncated++
		return
	}
	s.excluded = append(s.excluded, purelbv2.EgressExcludedPod{Pod: pod.Name, Namespace: pod.Namespace, Reason: reason})
}

// selectPods selects the policy's pods (§5.3): from its namespace by
// podSelector, from the Service's namespace by the Service's selector
// (never EndpointSlices, D20), or across the namespaces a ClusterEgress
// selects within its group's scope.
func (r *Reconciler) selectPods(run *run) {
	p := run.p
	run.sel = selection{nodes: map[string][]member{}}

	var podSel labels.Selector
	switch {
	case run.svc != nil:
		podSel = labels.SelectorFromSet(run.svc.Spec.Selector)
	default:
		s, err := metav1.LabelSelectorAsSelector(p.podSelector)
		if err != nil {
			return // CEL and the API server's own validation make this unreachable
		}
		podSel = s
	}

	namespaces := []string{p.ref.Namespace}
	inScope := map[string]bool{p.ref.Namespace: true}
	if p.cluster() {
		namespaces, inScope = r.clusterNamespaces(run)
	}

	perPod := p.affinity() == "PerPod"
	for _, ns := range namespaces {
		objs, _ := r.pods.ByIndex(cache.NamespaceIndex, ns)
		for _, o := range objs {
			pod := o.(*corev1.Pod)
			if !podSel.Matches(labels.Set(pod.Labels)) || !live(pod) {
				continue
			}
			switch {
			case !inScope[ns]:
				run.sel.exclude(pod, excludedNotInScope)
			case pod.Spec.HostNetwork:
				run.sel.exclude(pod, excludedHostNetwork)
			case p.optOut != "Denied" && pod.Annotations[purelbv2.EgressOptOutAnnotation] == "false":
				run.sel.exclude(pod, excludedOptedOut)
			case perPod && statefulSetOf(pod) == "":
				run.sel.exclude(pod, excludedNotSTS)
			case !r.egressCapable(pod.Spec.NodeName):
				run.sel.exclude(pod, excludedUnsupported)
			default:
				m := member{namespace: pod.Namespace, name: pod.Name, uid: string(pod.UID)}
				if perPod {
					m.identity = pod.Name
					if p.cluster() {
						m.identity = pod.Namespace + "." + pod.Name
					}
				}
				run.sel.nodes[pod.Spec.NodeName] = append(run.sel.nodes[pod.Spec.NodeName], m)
				run.sel.selected++
			}
		}
	}
	for _, ms := range run.sel.nodes {
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].namespace != ms[j].namespace {
				return ms[i].namespace < ms[j].namespace
			}
			return ms[i].name < ms[j].name
		})
	}
}

// live reports whether a pod counts: scheduled, not finished and not
// terminating. A terminating pod's slice drains rather than staying held.
func live(pod *corev1.Pod) bool {
	return pod.Spec.NodeName != "" && pod.DeletionTimestamp == nil &&
		pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// statefulSetOf is the name of the StatefulSet that owns pod, or "".
func statefulSetOf(pod *corev1.Pod) string {
	for _, o := range pod.OwnerReferences {
		if o.Kind == "StatefulSet" && o.Controller != nil && *o.Controller {
			return o.Name
		}
	}
	return ""
}

// egressCapable reports whether a node carries the egress-capable label
// (D29).
func (r *Reconciler) egressCapable(node string) bool {
	obj, exists, _ := r.nodes.GetByKey(node)
	return exists && obj.(*corev1.Node).Labels[purelbv2.EgressCapableLabel] == "true"
}

// clusterNamespaces returns the namespaces a ClusterEgress's selector
// matches, and which of those its group serves (and, with AdmittedOnly,
// that carry the admission label). Pods in a matched namespace outside
// the scope are reported, not selected.
func (r *Reconciler) clusterNamespaces(run *run) ([]string, map[string]bool) {
	p := run.p
	sel, err := metav1.LabelSelectorAsSelector(p.namespaceSelector)
	if err != nil {
		return nil, nil
	}
	var names []string
	inScope := map[string]bool{}
	for _, o := range r.namespaces.List() {
		ns := o.(*corev1.Namespace)
		if !sel.Matches(labels.Set(ns.Labels)) {
			continue
		}
		names = append(names, ns.Name)
		if p.admittedOnly && ns.Labels[admittedLabel] != "true" {
			continue
		}
		if sg, _, err := r.alloc.EgressGroup(ns.Name, run.group.Name); err == nil && sg.Name == run.group.Name {
			inScope[ns.Name] = true
		}
	}
	sort.Strings(names)
	return names, inScope
}

// policyRefOf is the PolicyRef of an Egress.
func policyRefOf(e *purelbv2.Egress) egress.PolicyRef {
	return egress.PolicyRef{Kind: egress.KindEgress, Namespace: e.Namespace, Name: e.Name}
}

// egressBefore reports whether policy a ranks ahead of b (D24).
func egressBefore(a, b *policy) bool {
	return egress.Before(a.rank(), b.rank())
}
