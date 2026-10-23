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
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// defaultSliceSize applies when neither the policy nor the group sets one.
const defaultSliceSize = 4096

// endpointsSpec is the EgressEndpoints spec for one address, apart from
// the slices, reserved ports and excluded nodes, which the slice and
// gating stages own.
func (r *Reconciler) endpointsSpec(run *run, a address) purelbv2.EgressEndpointsSpec {
	p := run.p
	spec := purelbv2.EgressEndpointsSpec{
		State:        "Active",
		Address:      a.ip.String(),
		Family:       "IPv4",
		ServiceGroup: run.group.Name,
		Pool:         a.entry,
		PoolType:     "Local",
		Placement:    "SingleAnnouncer",
		Identity:     a.identity,
		SliceSize:    sliceSize(run),
		Policy: purelbv2.EgressEndpointsPolicy{
			Priority:      p.priority,
			FailurePolicy: p.spec.FailurePolicy,
			AffinityMode:  p.affinity(),
			Helpers:       p.spec.Helpers,
			Destinations:  p.spec.Destinations,
		},
	}
	if spec.Pool == "" {
		spec.Pool = p.spec.Pool
	}
	if a.ip.Is6() {
		spec.Family = "IPv6"
	}
	if routed(run) {
		spec.PoolType, spec.Placement = "Routed", "MultiAnnouncer"
	}
	if a.shared {
		spec.SharedWith = &purelbv2.EgressServiceRef{Name: run.svc.Name}
		spec.ServicePorts = r.servicePorts(a.ip)
	}
	return spec
}

// routed reports whether the group's addresses are announced by routing
// (remote pools, and external pools announced remotely) rather than on
// the node's own subnet.
func routed(run *run) bool {
	g := run.group.Spec
	return g.Remote != nil || (g.External != nil && g.External.Announce == purelbv2.PoolTypeRemote)
}

func sliceSize(run *run) int32 {
	if run.p.spec.Ports != nil && run.p.spec.Ports.SliceSize != nil {
		return *run.p.spec.Ports.SliceSize
	}
	if eg := run.group.Spec.Egress; eg != nil && eg.SliceSize != 0 {
		return eg.SliceSize
	}
	return defaultSliceSize
}

// servicePorts are the ports of every Service on a shared address, which
// no slice may cover (§5.6).
func (r *Reconciler) servicePorts(ip netip.Addr) []purelbv2.EgressServicePort {
	seen := map[purelbv2.EgressServicePort]bool{}
	var out []purelbv2.EgressServicePort
	for _, o := range r.services.List() {
		svc := o.(*corev1.Service)
		on := false
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			on = on || ing.IP == ip.String()
		}
		if !on {
			continue
		}
		for _, sp := range svc.Spec.Ports {
			proto := string(sp.Protocol)
			if proto == "" {
				proto = string(corev1.ProtocolTCP)
			}
			k := purelbv2.EgressServicePort{Protocol: proto, Port: sp.Port}
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// endpointsLabels are an address's EgressEndpoints labels: its owner's,
// plus the group and pool the routed nodes watch by (D6).
func endpointsLabels(run *run, spec purelbv2.EgressEndpointsSpec) map[string]string {
	l := ownerLabels(run.p)
	l[egress.LabelServiceGroup] = spec.ServiceGroup
	l[egress.LabelPool] = egress.PoolLabelValue(spec.Pool)
	return l
}

// syncEndpoints writes an EgressEndpoints for each of the policy's
// addresses, keeping each one's slices, reserved ports and excluded
// nodes. Releasing addresses the policy no longer holds is the teardown
// stage's.
func (r *Reconciler) syncEndpoints(run *run, addrs []address) {
	client := r.purelb.PurelbV2().EgressEndpointses(r.namespace)
	for _, a := range addrs {
		name := egress.EndpointsName(run.p.ref, a.ip)
		spec := r.endpointsSpec(run, a)
		cur, err := r.endpoints.EgressEndpointses(r.namespace).Get(name)
		if err != nil {
			want := &purelbv2.EgressEndpoints{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: endpointsLabels(run, spec)},
				Spec:       spec,
			}
			r.write(run.p, "EgressEndpoints/"+name, false, func(ctx context.Context) error {
				_, err := client.Create(ctx, want, metav1.CreateOptions{})
				return err
			})
			continue
		}
		spec.Slices, spec.Reserved, spec.ExcludedNodes = cur.Spec.Slices, cur.Spec.Reserved, cur.Spec.ExcludedNodes
		lbls := endpointsLabels(run, spec)
		if apiequality.Semantic.DeepEqual(cur.Spec, spec) && apiequality.Semantic.DeepEqual(cur.Labels, lbls) {
			continue
		}
		upd := cur.DeepCopy()
		upd.Spec, upd.Labels = spec, lbls
		r.write(run.p, "EgressEndpoints/"+name, false, func(ctx context.Context) error {
			_, err := client.Update(ctx, upd, metav1.UpdateOptions{})
			return err
		})
	}
}

// Replay records egress's addresses in the pools after a rebuild, after
// the Services: each Active address under its owner key, a shared
// address as a reference hold.
func (r *Reconciler) Replay(record func(pool, key string, ips []net.IP, reference bool) error) {
	if !r.enabled {
		return
	}
	eps, _ := r.endpoints.EgressEndpointses(r.namespace).List(labels.Everything())
	for _, ep := range eps {
		if ep.Spec.State != "Active" {
			continue
		}
		ref := egress.PolicyRef{Kind: ep.Labels[egress.LabelKind], Namespace: ep.Labels[egress.LabelNamespace], Name: ep.Labels[egress.LabelName]}
		ip := net.ParseIP(ep.Spec.Address)
		if ip == nil || ref.Name == "" {
			continue
		}
		_ = record(ep.Spec.ServiceGroup, egress.SidecarKey(ref, ep.Spec.Identity), []net.IP{ip}, ep.Spec.SharedWith != nil)
	}
}

// ReleaseService holds the release of a Service's address in an external
// pool while an egress policy uses it, since the sidecar would otherwise
// free it under the policy. In-tree pools need nothing: the policy's
// reference hold already keeps the address.
func (r *Reconciler) ReleaseService(pool, key string, ip net.IP) bool {
	if !r.enabled || !r.alloc.PoolIsExternal(pool) {
		return true
	}
	ns, name, _ := strings.Cut(key, "/")
	eps, _ := r.endpoints.EgressEndpointses(r.namespace).List(labels.SelectorFromSet(labels.Set{egress.LabelNamespace: ns}))
	for _, ep := range eps {
		if ep.Spec.SharedWith == nil || ep.Spec.SharedWith.Name != name || ep.Spec.ServiceGroup != pool {
			continue
		}
		if ip == nil || net.ParseIP(ep.Spec.Address).Equal(ip) {
			return false
		}
	}
	return true
}
