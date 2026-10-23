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
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/egress"
	"purelb.io/internal/logging"
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
// addresses with its slices and reserved ports (§5.6). Slices change only once every gating node has acked
// the current generation (§5.7), so one generation is in flight at a
// time; a change of slices goes on the priority lane. Releasing addresses
// the policy no longer holds is the teardown stage's.
func (r *Reconciler) syncEndpoints(run *run, addrs []address) {
	client := r.purelb.PurelbV2().EgressEndpointses(r.namespace)
	owner := run.p.ref.String()
	added := map[string]int{} // slots this reconcile takes per node
	for _, a := range addrs {
		name := egress.EndpointsName(run.p.ref, a.ip)
		spec := r.endpointsSpec(run, a)
		spec.Reserved = r.reservedPorts(run, a, spec.ServicePorts)
		cur, err := r.endpoints.EgressEndpointses(r.namespace).Get(name)
		if err != nil {
			spec.Slices = r.planAddress(run, a, name, spec.SliceSize, nil, nil, added)
			want := &purelbv2.EgressEndpoints{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: endpointsLabels(run, spec)},
				Spec:       spec,
			}
			r.write(owner, "EgressEndpoints/"+name, len(spec.Slices) > 0, func(ctx context.Context) error {
				_, err := client.Create(ctx, want, metav1.CreateOptions{})
				return err
			})
			continue
		}
		if len(cur.Spec.Slices) > 0 && cur.Spec.SliceSize != spec.SliceSize {
			// In-use indices keep their meaning: a new size applies once
			// the address has no slices.
			logging.Debug(r.logger, "op", "egressEndpoints", "endpoints", name, "sliceSize", cur.Spec.SliceSize,
				"wanted", spec.SliceSize, "msg", "keeping the slice size while slices exist")
			spec.SliceSize = cur.Spec.SliceSize
		}
		acked, excluded := r.gate(run, cur, owner)
		spec.ExcludedNodes = excluded
		spec.Slices = cur.Spec.Slices
		if acked {
			spec.Slices = r.planAddress(run, a, name, spec.SliceSize, activate(cur.Spec.Slices), r.freedSlices(cur, owner), added)
		}
		lbls := endpointsLabels(run, spec)
		if apiequality.Semantic.DeepEqual(cur.Spec, spec) && apiequality.Semantic.DeepEqual(cur.Labels, lbls) {
			continue
		}
		upd := cur.DeepCopy()
		upd.Spec, upd.Labels = spec, lbls
		r.write(owner, "EgressEndpoints/"+name, !apiequality.Semantic.DeepEqual(cur.Spec.Slices, spec.Slices),
			func(ctx context.Context) error {
				_, err := client.Update(ctx, upd, metav1.UpdateOptions{})
				return err
			})
	}
}

// planAddress plans one address's slices: one for each node with selected
// pods and each node claiming one, within the node's slot budget. A PerPod
// address has none: its node gets the whole address.
func (r *Reconciler) planAddress(run *run, a address, ee string, size int32, cur []purelbv2.EgressSlice,
	freed map[int32]bool, added map[string]int) []purelbv2.EgressSlice {
	if a.identity != "" {
		return nil
	}
	want := map[string]bool{}
	for n := range run.sel.nodes {
		want[n] = true
	}
	for _, n := range r.claimants(ee) {
		want[n] = true
	}
	var ports []int32
	for _, sp := range r.servicePorts(a.ip) {
		ports = append(ports, sp.Port)
	}
	if run.p.spec.Ports != nil {
		for _, rp := range run.p.spec.Ports.Reserved {
			ports = append(ports, rp.Port)
		}
	}
	before := map[int32]string{} // index -> node
	for _, s := range cur {
		before[s.Index] = s.Node
	}
	plan := planSlices(sliceInput{size: size, current: cur, want: want, ports: ports, reserve: reserveSlices(run),
		freed: freed, slotOK: func(n string) bool {
			return r.slotsUsed(n)+added[n] < r.slotLimit(n, run.p.ref.Kind)
		}})
	for _, s := range plan.slices {
		if n, ok := before[s.Index]; s.Node != "" && (!ok || n != s.Node) {
			added[s.Node]++
		}
	}
	if len(plan.exhausted) > 0 {
		run.conds.fail(condPortCapacity, reasonSliceExhausted,
			"%s: no free slice for nodes %v; usable slices = floor((65536-1024)/%d) - blocks holding a Service or reserved port - draining = %d",
			a.ip, plan.exhausted, size, sliceCapacity(size, ports, plan.slices))
	}
	if len(plan.overBudget) > 0 {
		run.conds.fail(condDatapathReady, reasonSlotsExhausted, "%s: nodes %v have no slot left (%d per node)",
			a.ip, plan.overBudget, slotBudget)
	}
	return plan.slices
}

// reservedPorts assigns each reserved port to the node and pod its
// selector picks among the policy's selected pods (D19). A port matching
// several pods, or a Service port on the address, goes to no pod.
func (r *Reconciler) reservedPorts(run *run, a address, svcPorts []purelbv2.EgressServicePort) []purelbv2.EgressReservedAssignment {
	if run.p.spec.Ports == nil || a.identity != "" {
		return nil
	}
	var out []purelbv2.EgressReservedAssignment
	for _, rp := range run.p.spec.Ports.Reserved {
		ra := purelbv2.EgressReservedAssignment{Protocol: rp.Protocol, Port: rp.Port}
		if ra.Protocol == "" {
			ra.Protocol = string(corev1.ProtocolTCP)
		}
		out = append(out, ra)
		if slices.Contains(svcPorts, purelbv2.EgressServicePort{Protocol: ra.Protocol, Port: ra.Port}) {
			run.conds.fail(condPortCapacity, reasonReservedPortConflict, "reserved port %s/%d is a Service port on %s",
				ra.Protocol, ra.Port, a.ip)
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&rp.PodSelector)
		if err != nil {
			continue
		}
		var matched []string
		var node, pod string
		for n, ms := range run.sel.nodes {
			for _, m := range ms {
				obj, ok, _ := r.pods.GetByKey(m.namespace + "/" + m.name)
				if ok && sel.Matches(labels.Set(obj.(*corev1.Pod).Labels)) {
					matched = append(matched, m.namespace+"/"+m.name)
					node, pod = n, m.name
					if run.p.ref.Kind == egress.KindClusterEgress {
						pod = m.namespace + "." + m.name
					}
				}
			}
		}
		switch {
		case len(matched) == 1:
			out[len(out)-1].Node, out[len(out)-1].Pod = node, pod
		case len(matched) > 1:
			sort.Strings(matched)
			run.conds.fail(condPortCapacity, reasonReservedPortAmbiguous, "reserved port %s/%d selects %d pods: %v",
				ra.Protocol, ra.Port, len(matched), matched)
		}
	}
	return out
}

// activate marks every Pending slice Active: the gating nodes have acked
// the generation that published them.
func activate(slices []purelbv2.EgressSlice) []purelbv2.EgressSlice {
	out := append([]purelbv2.EgressSlice(nil), slices...)
	for i := range out {
		if out[i].State == slicePending {
			out[i].State = sliceActive
		}
	}
	return out
}

func reserveSlices(run *run) int {
	if eg := run.group.Spec.Egress; eg != nil && eg.ReserveSlices != nil {
		return int(*eg.ReserveSlices)
	}
	return defaultReserveSlices
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
