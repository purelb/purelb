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
	"maps"
	"slices"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// addressPrefix keys an EgressAddress on the queue, by its EgressEndpoints'
// name, which it shares.
const addressPrefix = "address:"

// syncAddress writes the EgressAddress of the EgressEndpoints name: the
// address as every egress node needs it, without the slices. It is
// written only when it changes, so nodes see one event per address
// change, not per slice change. The EgressEndpoints owns it, so it goes
// with it.
func (r *Reconciler) syncAddress(name string) error {
	ep, err := r.endpoints.EgressEndpointses(r.namespace).Get(name)
	if err != nil {
		return nil // deleted: garbage collection removes the EgressAddress
	}
	spec := r.addressSpec(ep)
	client := r.purelb.PurelbV2().EgressAddresses(r.namespace)
	key := addressPrefix + name
	cur, err := r.addresses.EgressAddresses(r.namespace).Get(name)
	if err != nil {
		want := &purelbv2.EgressAddress{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: maps.Clone(ep.Labels),
				OwnerReferences: []metav1.OwnerReference{{APIVersion: purelbv2.SchemeGroupVersion.String(),
					Kind: "EgressEndpoints", Name: ep.Name, UID: ep.UID}}},
			Spec: spec,
		}
		r.write(key, "EgressAddress/"+name, false, func(ctx context.Context) error {
			_, err := client.Create(ctx, want, metav1.CreateOptions{})
			return err
		})
		return nil
	}
	if apiequality.Semantic.DeepEqual(cur.Spec, spec) || r.statusWritten("EgressAddress/"+name, spec) {
		return nil
	}
	patch := specPatch(spec)
	r.write(key, "EgressAddress/"+name, false, func(ctx context.Context) error {
		_, err := client.Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{})
		return err
	})
	return nil
}

// addressSpec is an EgressEndpoints' address as every node sees it. The
// preferred nodes (§5.5) are a PerPod address's pod node, and with the
// group's preferPodNodes the nodes holding an Active or Draining slice (a
// Draining slice lasts at most drainMax, which damps churn); otherwise
// none.
func (r *Reconciler) addressSpec(ep *purelbv2.EgressEndpoints) purelbv2.EgressAddressSpec {
	s := ep.Spec
	spec := purelbv2.EgressAddressSpec{State: s.State, Address: s.Address, Family: s.Family, ServiceGroup: s.ServiceGroup,
		Pool: s.Pool, PoolType: s.PoolType, Placement: s.Placement, SharedWith: s.SharedWith}
	if s.State != stateActive {
		return spec
	}
	ref := egress.PolicyRef{Kind: ep.Labels[egress.LabelKind], Namespace: ep.Labels[egress.LabelNamespace], Name: ep.Labels[egress.LabelName]}
	if p, ok, _ := r.load(ref); ok && s.SharedWith == nil && p.spec.SkipIPv6DAD != nil {
		skip := *p.spec.SkipIPv6DAD
		spec.SkipIPv6DAD = &skip
	}
	prefer := s.Identity != ""
	if g, _, ok := r.alloc.ServiceGroup(s.ServiceGroup); ok && g.Spec.Egress != nil && g.Spec.Egress.PreferPodNodes {
		prefer = true
	}
	if !prefer {
		return spec
	}
	for _, sl := range s.Slices {
		if sl.Node == "" || (s.Identity == "" && sl.State != "Active" && sl.State != "Draining") {
			continue
		}
		if !slices.Contains(spec.PreferredNodes, sl.Node) {
			spec.PreferredNodes = append(spec.PreferredNodes, sl.Node)
		}
	}
	slices.Sort(spec.PreferredNodes)
	return spec
}

// addressKeys are the queue keys of every EgressEndpoints' address, for
// ForceSync.
func (r *Reconciler) addressKeys() []string {
	var keys []string
	for _, k := range r.endpointsIdx.ListKeys() {
		if _, n, err := cache.SplitMetaNamespaceKey(k); err == nil {
			keys = append(keys, addressPrefix+n)
		}
	}
	slices.Sort(keys)
	return keys
}
