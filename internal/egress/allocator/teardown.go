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
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/egress"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// EgressEndpoints states.
const (
	stateActive      = "Active"
	stateQuarantined = "Quarantined"
)

// tombstonePrefix keys a tombstone's expiry on the queue.
const tombstonePrefix = "tombstone:"

// defaultReleaseQuarantine applies when the group doesn't set one (D16).
const defaultReleaseQuarantine = time.Hour

// retire releases the policy's addresses it no longer holds (§5.4,
// §5.9): those not in keep, apart from a PerPod address whose pod is only
// restarting. It returns how many of its addresses are still Active,
// counting keep, which the informer may not show yet.
func (r *Reconciler) retire(run *run, keep []address) int {
	ref := run.p.ref
	kept := map[string]bool{}
	for _, a := range keep {
		kept[egress.EndpointsName(ref, a.ip)] = true
	}
	active := len(kept)
	eps, _ := r.endpoints.EgressEndpointses(r.namespace).List(ownedSelector(ref))
	for _, ep := range eps {
		if ep.Spec.State != stateActive || kept[ep.Name] {
			continue
		}
		active++
		if !run.revoked && r.ordinalRetained(ep) {
			continue
		}
		r.retireEndpoints(ref.String(), run.conds, ep)
	}
	return active
}

// retireOrphans retires the addresses of a policy that is gone without
// its finalizer (deleted before its first allocation's finalizer landed),
// and deletes its EgressMembers.
func (r *Reconciler) retireOrphans(ref egress.PolicyRef) {
	eps, _ := r.endpoints.EgressEndpointses(r.namespace).List(ownedSelector(ref))
	for _, ep := range eps {
		if ep.Spec.State == stateActive {
			r.retireEndpoints(ref.String(), conditions{}, ep)
		}
	}
	ms, _ := r.members.EgressMemberses(r.namespace).List(ownedSelector(ref))
	client := r.purelb.PurelbV2().EgressMemberses(r.namespace)
	for _, m := range ms {
		name := m.Name
		r.write(ref.String(), "EgressMembers/"+name, false, func(ctx context.Context) error {
			return client.Delete(ctx, name, metav1.DeleteOptions{})
		})
	}
}

// retireEndpoints moves one address toward its tombstone: every slice
// drains and is freed through the usual gating, then, once the holders
// have acked a generation with no slices, the address becomes a
// tombstone held for the group's releaseQuarantine (D16).
func (r *Reconciler) retireEndpoints(owner string, conds conditions, ep *purelbv2.EgressEndpoints) {
	spec := *ep.Spec.DeepCopy()
	acked, excluded := r.gate(conds, ep, owner)
	spec.ExcludedNodes = excluded
	if acked {
		freed := r.freedSlices(ep, owner)
		spec.Slices = nil
		for _, s := range ep.Spec.Slices {
			if freed[s.Index] || s.Node == "" || s.State == slicePending {
				continue
			}
			s.State = sliceDraining
			spec.Slices = append(spec.Slices, s)
		}
		if len(ep.Spec.Slices) == 0 {
			spec.State, spec.ExcludedNodes, spec.Reserved = stateQuarantined, nil, nil
			spec.Until = &metav1.Time{Time: r.now().Add(r.releaseQuarantine(ep.Spec.ServiceGroup)).Truncate(time.Second)}
			logging.Info(r.logger, "op", "egressRelease", "endpoints", ep.Name, "address", ep.Spec.Address,
				"until", spec.Until.Time, "msg", "address quarantined")
			if ref, ok := r.ownerRef(ep.Labels); ok {
				r.event(ref, nil, nil, false, false, "AddressQuarantined", "%s quarantined until %s",
					ep.Spec.Address, spec.Until.Format(time.RFC3339))
			}
		}
	}
	if apiequality.Semantic.DeepEqual(ep.Spec, spec) {
		return
	}
	upd := ep.DeepCopy()
	upd.Spec = spec
	client := r.purelb.PurelbV2().EgressEndpointses(r.namespace)
	r.write(owner, "EgressEndpoints/"+ep.Name, !apiequality.Semantic.DeepEqual(ep.Spec.Slices, spec.Slices),
		func(ctx context.Context) error {
			_, err := client.Update(ctx, upd, metav1.UpdateOptions{})
			return err
		})
}

func (r *Reconciler) releaseQuarantine(group string) time.Duration {
	if sg, _, ok := r.alloc.ServiceGroup(group); ok && sg.Spec.Egress != nil && sg.Spec.Egress.ReleaseQuarantine != nil {
		return sg.Spec.Egress.ReleaseQuarantine.Duration
	}
	return defaultReleaseQuarantine
}

// ordinalRetained reports whether a PerPod address's identity is still
// one of its StatefulSet's ordinals, so a restarting pod gets the address
// back (D27); a scaled-down ordinal is released.
func (r *Reconciler) ordinalRetained(ep *purelbv2.EgressEndpoints) bool {
	id := ep.Spec.Identity
	if id == "" {
		return false
	}
	ns := ep.Labels[egress.LabelNamespace]
	if ep.Labels[egress.LabelKind] == egress.KindClusterEgress {
		ns, id, _ = strings.Cut(id, ".")
	}
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return false
	}
	ordinal, err := strconv.Atoi(id[i+1:])
	if err != nil {
		return false
	}
	obj, ok, _ := r.statefulSets.GetByKey(ns + "/" + id[:i])
	if !ok {
		return false
	}
	replicas := int32(1)
	if sts := obj.(*appsv1.StatefulSet); sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	return int32(ordinal) < replicas
}

// syncFinalizer adds the finalizer once the policy holds an address
// (D5) and, when the policy is being deleted or its group no longer
// allows egress, removes it once nothing remains: no Active address and
// no EgressMembers.
func (r *Reconciler) syncFinalizer(run *run, active int, clear bool) {
	p := run.p
	has := slices.Contains(p.meta.Finalizers, egress.Finalizer)
	want := has || active > 0
	if clear {
		ms, _ := r.members.EgressMemberses(r.namespace).List(ownedSelector(p.ref))
		want = has && (active > 0 || len(ms) > 0)
	}
	if want == has {
		return
	}
	fins := slices.DeleteFunc(slices.Clone(p.meta.Finalizers), func(f string) bool { return f == egress.Finalizer })
	if want {
		fins = append(fins, egress.Finalizer)
	}
	owner := p.ref.String()
	key := p.ref.Kind + "/" + p.ref.Namespace + "/" + p.ref.Name
	if p.cluster() {
		c, err := r.clusterEgresses.Get(p.ref.Name)
		if err != nil {
			return
		}
		upd := c.DeepCopy()
		upd.Finalizers = fins
		r.write(owner, key, false, func(ctx context.Context) error {
			_, err := r.purelb.PurelbV2().ClusterEgresses().Update(ctx, upd, metav1.UpdateOptions{})
			return err
		})
		return
	}
	e, err := r.egresses.Egresses(p.ref.Namespace).Get(p.ref.Name)
	if err != nil {
		return
	}
	upd := e.DeepCopy()
	upd.Finalizers = fins
	r.write(owner, key, false, func(ctx context.Context) error {
		_, err := r.purelb.PurelbV2().Egresses(p.ref.Namespace).Update(ctx, upd, metav1.UpdateOptions{})
		return err
	})
}

// expireTombstone frees a tombstone's address once its quarantine ends,
// then deletes the tombstone (D16): the owner's allocation, or for a
// shared address its reference hold, and in an external pool the
// Service's own release if egress held it back (ReleaseService) and the
// Service has since let the address go. Release is never sent for the
// egress key of a shared address (D15).
func (r *Reconciler) expireTombstone(name string) error {
	ep, err := r.endpoints.EgressEndpointses(r.namespace).Get(name)
	if err != nil || ep.Spec.State != stateQuarantined {
		return nil
	}
	key := tombstonePrefix + name
	if ep.Spec.Until != nil && r.now().Before(ep.Spec.Until.Time) {
		r.requeueAfter(ep.Spec.Until.Sub(r.now()), key)
		return nil
	}
	ip := net.ParseIP(ep.Spec.Address)
	ref := egress.PolicyRef{Kind: ep.Labels[egress.LabelKind], Namespace: ep.Labels[egress.LabelNamespace], Name: ep.Labels[egress.LabelName]}
	group := ep.Spec.ServiceGroup
	switch {
	case ep.Spec.SharedWith == nil || !r.alloc.PoolIsExternal(group):
		err = r.alloc.ReleaseEgress(group, egress.SidecarKey(ref, ep.Spec.Identity), ip)
	case !r.serviceHolds(ref.Namespace, ep.Spec.SharedWith.Name, ep.Spec.Address):
		err = r.alloc.ReleaseEgress(group, ref.Namespace+"/"+ep.Spec.SharedWith.Name, ip)
	}
	if err != nil {
		logging.Info(r.logger, "op", "egressRelease", "endpoints", name, "address", ep.Spec.Address, "error", err)
		return err
	}
	logging.Info(r.logger, "op", "egressRelease", "endpoints", name, "address", ep.Spec.Address, "msg", "quarantine ended; address released")
	r.event(ref, nil, nil, false, false, "AddressReleased", "%s released", ep.Spec.Address)
	forgetAddress(ep.Spec.Address)
	client := r.purelb.PurelbV2().EgressEndpointses(r.namespace)
	r.write(key, "EgressEndpoints/"+name, false, func(ctx context.Context) error {
		return client.Delete(ctx, name, metav1.DeleteOptions{})
	})
	return nil
}

// serviceHolds reports whether a Service still has address in its status.
func (r *Reconciler) serviceHolds(ns, name, address string) bool {
	obj, ok, _ := r.services.GetByKey(ns + "/" + name)
	if !ok {
		return false
	}
	for _, ing := range obj.(*corev1.Service).Status.LoadBalancer.Ingress {
		if ing.IP == address {
			return true
		}
	}
	return false
}

// tombstoneKeys are the queue keys of every tombstone, for ForceSync.
func (r *Reconciler) tombstoneKeys() []string {
	eps, _ := r.endpoints.EgressEndpointses(r.namespace).List(labels.Everything())
	var keys []string
	for _, ep := range eps {
		if ep.Spec.State == stateQuarantined {
			keys = append(keys, tombstonePrefix+ep.Name)
		}
	}
	return keys
}

// slimStatefulSet keeps what ordinalRetained reads: a StatefulSet's
// identity and replicas, not its pod template.
func slimStatefulSet(obj interface{}) (interface{}, error) {
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok {
		return obj, nil
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: sts.Name, Namespace: sts.Namespace, UID: sts.UID, ResourceVersion: sts.ResourceVersion},
		Spec:       appsv1.StatefulSetSpec{Replicas: sts.Spec.Replicas},
	}, nil
}
