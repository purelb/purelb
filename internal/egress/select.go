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

package egress

import (
	corev1 "k8s.io/api/core/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// The pod selection rules (§5.3) the allocator and the node agents share,
// so a pod node tags exactly the pods the allocator gave it slices for.

// Why a selected pod is excluded (EgressStatus.excludedPods[].reason).
const (
	ExcludedHostNetwork = "HostNetwork"
	ExcludedOptedOut    = "OptedOut"
	ExcludedNotSTS      = "NotStatefulSet"
	ExcludedUnsupported = "NodeUnsupported"
	ExcludedNotInScope  = "NamespaceNotInScope"
	ExcludedOutranked   = "Outranked"
)

// AdmittedLabel admits a namespace to ClusterEgresses with AdmittedOnly.
const AdmittedLabel = "purelb.io/cluster-egress-admitted"

// Live reports whether a pod counts: scheduled, not finished and not
// terminating. A terminating pod's slice drains rather than staying held.
func Live(pod *corev1.Pod) bool {
	return pod.Spec.NodeName != "" && pod.DeletionTimestamp == nil &&
		pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// StatefulSetOf is the name of the StatefulSet that owns pod, or "".
func StatefulSetOf(pod *corev1.Pod) string {
	for _, o := range pod.OwnerReferences {
		if o.Kind == "StatefulSet" && o.Controller != nil && *o.Controller {
			return o.Name
		}
	}
	return ""
}

// PodExcluded is why a pod the policy's selector matches is excluded by the
// pod itself, or "": host-network pods, pods opting out where the policy
// allows it, and pods not in a StatefulSet under PerPod. The allocator adds
// the exclusions that need cluster state (node, scope, outranking).
func PodExcluded(pod *corev1.Pod, optOut string, perPod bool) string {
	switch {
	case pod.Spec.HostNetwork:
		return ExcludedHostNetwork
	case optOut != "Denied" && pod.Annotations[purelbv2.EgressOptOutAnnotation] == "false":
		return ExcludedOptedOut
	case perPod && StatefulSetOf(pod) == "":
		return ExcludedNotSTS
	}
	return ""
}

// NamespaceInScope reports whether a ClusterEgress on group (sg, which
// the caller has resolved) may select pods in namespace ns, which its
// namespaceSelector matched: admitted when the policy is AdmittedOnly, and
// inside the group's namespace boundary.
func NamespaceInScope(ns *corev1.Namespace, admittedOnly bool, group string, sg *purelbv2.ServiceGroup,
	b purelbv2.NamespaceBindings) bool {
	if admittedOnly && ns.Labels[AdmittedLabel] != "true" {
		return false
	}
	if b.Fenced(group, sg, ns.Name) != purelbv2.Unfenced {
		return false
	}
	return purelbv2.ServesNamespace(sg, ns.Name)
}

// EffectivePriority is the priority a policy ranks by: an Egress's held to
// its group's priorityCeiling, reporting whether it was clamped; a
// ClusterEgress's its own.
func EffectivePriority(priority int32, cluster bool, sg *purelbv2.ServiceGroup) (int32, bool) {
	if cluster || sg == nil || sg.Spec.Egress == nil || sg.Spec.Egress.Tenancy == nil || sg.Spec.Egress.Tenancy.PriorityCeiling == nil {
		return priority, false
	}
	if ceiling := *sg.Spec.Egress.Tenancy.PriorityCeiling; priority > ceiling {
		return ceiling, true
	}
	return priority, false
}
