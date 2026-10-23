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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// policy is an Egress or a ClusterEgress, seen the same way by every
// reconcile stage.
type policy struct {
	ref  egress.PolicyRef
	meta metav1.ObjectMeta
	spec purelbv2.EgressPolicy

	serviceGroup string
	serviceRef   *purelbv2.EgressServiceRef // Egress only
	podSelector  *metav1.LabelSelector

	// ClusterEgress only.
	namespaceSelector *metav1.LabelSelector
	admittedOnly      bool

	priority int32
	optOut   string
	status   purelbv2.EgressStatus
}

func (p *policy) cluster() bool { return p.ref.Kind == egress.KindClusterEgress }

func (p *policy) rank() egress.Rank {
	return egress.Rank{Priority: p.priority, Created: p.meta.CreationTimestamp.Time, UID: string(p.meta.UID)}
}

// affinity is the policy's affinity mode, defaulted: Inherit with
// serviceRef, otherwise Hash.
func (p *policy) affinity() string {
	if p.spec.Affinity != nil && p.spec.Affinity.Mode != "" {
		return p.spec.Affinity.Mode
	}
	if p.serviceRef != nil {
		return "Inherit"
	}
	return "Hash"
}

// load returns the policy ref names, or false if it no longer exists.
func (r *Reconciler) load(ref egress.PolicyRef) (*policy, bool, error) {
	if ref.Kind == egress.KindClusterEgress {
		c, err := r.clusterEgresses.Get(ref.Name)
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return &policy{
			ref: ref, meta: c.ObjectMeta, spec: c.Spec.EgressPolicy,
			serviceGroup: c.Spec.ServiceGroup, podSelector: &c.Spec.PodSelector,
			namespaceSelector: &c.Spec.NamespaceSelector, admittedOnly: c.Spec.NamespaceScope == "AdmittedOnly",
			priority: c.Spec.Priority, optOut: c.Spec.OptOut, status: c.Status,
		}, true, nil
	}
	e, err := r.egresses.Egresses(ref.Namespace).Get(ref.Name)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &policy{
		ref: ref, meta: e.ObjectMeta, spec: e.Spec.EgressPolicy,
		serviceGroup: e.Spec.ServiceGroup, serviceRef: e.Spec.ServiceRef, podSelector: e.Spec.PodSelector,
		priority: e.Spec.Priority, optOut: e.Spec.OptOut, status: e.Status,
	}, true, nil
}
