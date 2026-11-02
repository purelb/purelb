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
	"errors"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	coreallocator "purelb.io/internal/allocator"
	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Contained=False reasons (§4.8). ServiceGroupNotFound isn't in the
// design's list, which has none for a policy whose group doesn't exist.
const (
	reasonNamespaceNotServed      = "NamespaceNotServed"
	reasonServiceGroupNotFound    = "ServiceGroupNotFound"
	reasonEgressNotAllowed        = "EgressNotAllowed"
	reasonFeatureNotAllowed       = "FeatureNotAllowed"
	reasonHelpersRequirePerPod    = "HelpersRequirePerPod"
	reasonClusterEgressNotAllowed = "ClusterEgressNotAllowed"
	reasonScopeUnbounded          = "ScopeUnbounded"
	reasonQuotaExceeded           = "QuotaExceeded"
	reasonPriorityClamped         = "PriorityClamped"
	reasonServiceNotFound         = "ServiceNotFound"
	reasonServiceNotLoadBalancer  = "ServiceNotLoadBalancer"
	reasonServiceHasNoSelector    = "ServiceHasNoSelector"
)

// run is one reconcile of one policy.
type run struct {
	p     *policy
	conds conditions

	group *purelbv2.ServiceGroup
	pool  coreallocator.Pool
	svc   *corev1.Service // serviceRef target

	// revoked: the policy keeps nothing (withdraw everything it holds).
	// retained: it keeps what it holds and changes nothing.
	revoked, retained bool
	// disallowed: the group no longer allows egress, which also clears
	// the finalizer once withdrawn (§5.9).
	disallowed bool

	sel     selection
	outrank outranking
}

// scope resolves the policy's group and checks it against the group's
// egress bounds (§5.2). A violation sets Contained=False and then
// revokes or retains per the group's tenancy.onRevocation; allowed: false
// always revokes.
func (r *Reconciler) scope(run *run) {
	p := run.p
	if p.serviceRef != nil && !r.scopeService(run) {
		run.revoked = true
		return
	}

	var err error
	switch {
	case p.cluster():
		var ok bool
		run.group, run.pool, ok = r.alloc.ServiceGroup(p.serviceGroup)
		if !ok {
			err = coreallocator.ErrUnknownPool
		}
	case run.svc != nil:
		run.group, run.pool, err = r.alloc.EgressGroup(p.ref.Namespace, serviceGroupOf(run.svc))
	default:
		run.group, run.pool, err = r.alloc.EgressGroup(p.ref.Namespace, p.serviceGroup)
	}
	if err != nil {
		reason := reasonServiceGroupNotFound
		if errors.Is(err, coreallocator.ErrNamespaceDenied) || errors.Is(err, coreallocator.ErrAmbiguousDefault) {
			reason = reasonNamespaceNotServed
		}
		run.conds.fail(condContained, reason, "%v", err)
		run.revoked = true
		return
	}

	eg := run.group.Spec.Egress
	if eg == nil || !eg.Allowed {
		run.conds.fail(condContained, reasonEgressNotAllowed, "ServiceGroup %s doesn't allow egress", run.group.Name)
		run.revoked, run.disallowed = true, true
		return
	}
	r.checkFeatures(run, eg)
	if !run.conds.ok(condContained) {
		if eg.Tenancy != nil && eg.Tenancy.OnRevocation == "RetainUntilDelete" {
			run.retained = true
		} else {
			run.revoked = true
		}
		return
	}
	r.clampPriority(run)
}

// scopeService resolves a serviceRef's Service, reporting false when the
// policy can't use it (§5.8).
func (r *Reconciler) scopeService(run *run) bool {
	p := run.p
	obj, exists, _ := r.services.GetByKey(p.ref.Namespace + "/" + p.serviceRef.Name)
	if !exists {
		run.conds.fail(condContained, reasonServiceNotFound, "Service %s not found", p.serviceRef.Name)
		return false
	}
	svc := obj.(*corev1.Service)
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		run.conds.fail(condContained, reasonServiceNotLoadBalancer, "Service %s isn't a LoadBalancer", svc.Name)
		return false
	}
	if len(svc.Spec.Selector) == 0 {
		run.conds.fail(condContained, reasonServiceHasNoSelector, "Service %s has no selector", svc.Name)
		return false
	}
	run.svc = svc
	return true
}

// serviceGroupOf is the ServiceGroup a Service's address came from: the
// first of its allocated-from pools.
func serviceGroupOf(svc *corev1.Service) string {
	pools := svc.Annotations[purelbv2.PoolAnnotation]
	first, _, _ := strings.Cut(pools, ",")
	return strings.TrimSpace(first)
}

// checkFeatures checks the policy against what its group allows.
func (r *Reconciler) checkFeatures(run *run, eg *purelbv2.ServiceGroupEgressSpec) {
	p := run.p
	mode := p.affinity()
	if len(eg.AffinityModes) > 0 && !slices.Contains(eg.AffinityModes, mode) {
		run.conds.fail(condContained, reasonFeatureNotAllowed, "ServiceGroup %s doesn't allow affinity %s", run.group.Name, mode)
	}
	if p.serviceRef != nil && !eg.AllowSharedServiceAddress {
		run.conds.fail(condContained, reasonFeatureNotAllowed, "ServiceGroup %s doesn't allow shared Service addresses", run.group.Name)
	}
	for _, h := range p.spec.Helpers {
		if !slices.Contains(eg.Helpers, h) {
			run.conds.fail(condContained, reasonFeatureNotAllowed, "ServiceGroup %s doesn't allow helper %s", run.group.Name, h)
		}
	}
	if len(p.spec.Helpers) > 0 && mode != "PerPod" {
		run.conds.fail(condContained, reasonHelpersRequirePerPod, "conntrack helpers need PerPod affinity")
	}
	if p.cluster() {
		if eg.Tenancy != nil && eg.Tenancy.AllowClusterEgress != nil && !*eg.Tenancy.AllowClusterEgress {
			run.conds.fail(condContained, reasonClusterEgressNotAllowed, "ServiceGroup %s doesn't allow ClusterEgress", run.group.Name)
		}
		if len(run.group.Spec.Namespaces) == 0 && !constrainsNamespaceName(p.namespaceSelector) {
			run.conds.fail(condContained, reasonScopeUnbounded,
				"namespaceSelector must constrain kubernetes.io/metadata.name, or ServiceGroup %s must list its namespaces", run.group.Name)
		}
		return
	}
	if eg.Tenancy != nil && eg.Tenancy.MaxEgressesPerNamespace != nil {
		r.checkQuota(run, int(*eg.Tenancy.MaxEgressesPerNamespace))
	}
}

// constrainsNamespaceName reports whether sel bounds the namespaces by
// name, through kubernetes.io/metadata.name, which only the API server
// sets.
func constrainsNamespaceName(sel *metav1.LabelSelector) bool {
	const key = "kubernetes.io/metadata.name"
	if sel == nil {
		return false
	}
	if _, ok := sel.MatchLabels[key]; ok {
		return true
	}
	for _, e := range sel.MatchExpressions {
		if e.Key == key && e.Operator == metav1.LabelSelectorOpIn {
			return true
		}
	}
	return false
}

// checkQuota fails the policy if it ranks beyond the group's
// maxEgressesPerNamespace among its namespace's Egresses using the group.
func (r *Reconciler) checkQuota(run *run, max int) {
	p := run.p
	egs, _ := r.egresses.Egresses(p.ref.Namespace).List(labels.Everything())
	ahead := 0
	for _, e := range egs {
		if e.UID == p.meta.UID {
			continue
		}
		sg, _, err := r.alloc.EgressGroup(e.Namespace, e.Spec.ServiceGroup)
		if err != nil || sg.Name != run.group.Name {
			continue
		}
		other, _, _ := r.load(policyRefOf(e))
		if other != nil && egressBefore(other, p) {
			ahead++
		}
	}
	if ahead >= max {
		run.conds.fail(condContained, reasonQuotaExceeded,
			"namespace %s already has %d Egresses on ServiceGroup %s", p.ref.Namespace, max, run.group.Name)
	}
}

// clampPriority holds an Egress's priority to its group's ceiling. The
// policy keeps running at the clamped priority, so this is reported but
// revokes nothing.
func (r *Reconciler) clampPriority(run *run) {
	p := run.p
	if prio, clamped := egress.EffectivePriority(p.priority, p.cluster(), run.group); clamped {
		run.conds.fail(condContained, reasonPriorityClamped, "priority %d clamped to the ServiceGroup's ceiling %d", p.priority, prio)
		p.priority = prio
	}
}
