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

package agent

import (
	"net/netip"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// slotBudget is the node's mark values, shared by slots and pod-set ids
// (D18): 0x01-0x7f.
const slotBudget = 127

// slotKey names a slot: one of this node's slices, of one (policy,
// address).
type slotKey struct {
	endpoints string
	index     int32
}

// slot is a slice of this node's, as its pods' traffic uses it.
type slot struct {
	policy    egress.PolicyRef
	address   netip.Addr
	sliceSize int32
	identity  string // PerPod: the whole address, for this pod
	state     string // Pending, Active or Draining
}

// podSet is the pods sharing one ordered list of policies (D24); a pod
// holding a reserved port has its own (D19).
type podSet struct {
	key      string
	policies []egress.PolicyRef // in rank order
	reserved string             // the pod holding reserved ports, or ""
}

// podState is what a pod node does with one of its pods.
type podState struct {
	ips    []netip.Addr
	podSet uint8 // 0: no pod-set, its traffic isn't egress
	// hold: a FailClosed policy selects it and its slot isn't Active yet,
	// so its traffic outside the cluster is dropped (§8.1).
	hold bool
}

// podNode is the pod-node stage's desired state: slots and pod-set ids
// (one budget, D18), and every local pod's. Built on the queue goroutine;
// read-only once built.
type podNode struct {
	slots   map[uint8]slot
	slotIDs map[slotKey]uint8
	sets    map[uint8]podSet
	setIDs  map[string]uint8
	pods    map[string]podState // namespace/name
	// retired are ids no longer used that conntrack may still carry, kept
	// out of use until it doesn't (D18).
	retired map[uint8]bool
	// short are the slots and pod-sets left without an id: the budget is
	// exhausted.
	short []string
}

// used is how many mark values the node holds.
func (pn *podNode) used() int {
	return len(pn.slots) + len(pn.sets) + len(pn.retired)
}

// pnPolicy is a policy as a pod node evaluates it.
type pnPolicy struct {
	ref    egress.PolicyRef
	rank   egress.Rank
	spec   purelbv2.EgressPolicy
	optOut string
	// pods selects the policy's pods: its podSelector, or its serviceRef
	// Service's selector.
	pods labels.Selector
	// ClusterEgress only: the namespaces it selects from, and its group.
	namespaces   labels.Selector
	admittedOnly bool
	group        string
	sg           *purelbv2.ServiceGroup
}

func (p *pnPolicy) perPod() bool {
	return p.spec.Affinity != nil && p.spec.Affinity.Mode == "PerPod"
}

func (p *pnPolicy) failClosed() bool {
	return p.spec.FailurePolicy != "FailOpen"
}

// identity is pod's PerPod identity under p, as the allocator names it.
func (p *pnPolicy) identity(pod *corev1.Pod) string {
	if p.ref.Kind == egress.KindClusterEgress {
		return pod.Namespace + "." + pod.Name
	}
	return pod.Name
}

// pnInput is what planPodNode reads.
type pnInput struct {
	pods       []*corev1.Pod
	policies   []*pnPolicy // in force: holding an Active address
	namespaces map[string]*corev1.Namespace
	bindings   purelbv2.NamespaceBindings
	// spec is this node's EgressNodeState spec; owners maps each of its
	// EgressEndpoints to its policy.
	spec   purelbv2.EgressNodeStateSpec
	owners map[string]egress.PolicyRef
	// reservedCluster are the ids only pod-sets led by a ClusterEgress may
	// take (§5.10).
	reservedCluster int
	// inUse reports whether conntrack still carries a retired id.
	inUse func(uint8) bool
}

// planPodNode computes the pod-node state from in, keeping prev's ids for
// what is still there, so a recompute never renumbers a live slot or
// pod-set.
func planPodNode(in pnInput, prev *podNode) *podNode {
	pn := &podNode{slots: map[uint8]slot{}, slotIDs: map[slotKey]uint8{}, sets: map[uint8]podSet{},
		setIDs: map[string]uint8{}, pods: map[string]podState{}, retired: map[uint8]bool{}}
	if prev == nil {
		prev = &podNode{}
	}

	// Slots: this node's slices, each of an address its policy holds.
	slotsOf := map[egress.PolicyRef][]slotKey{}
	want := map[slotKey]slot{}
	for _, s := range in.spec.Slices {
		ref, ok := in.owners[s.Endpoints]
		addr, err := netip.ParseAddr(s.Address)
		if !ok || err != nil {
			continue
		}
		k := slotKey{endpoints: s.Endpoints, index: s.Index}
		want[k] = slot{policy: ref, address: addr, sliceSize: s.SliceSize, identity: s.Identity, state: s.State}
		slotsOf[ref] = append(slotsOf[ref], k)
	}
	reservedBy := map[string]bool{} // pods holding reserved ports
	for _, r := range in.spec.Reserved {
		if ref, ok := in.owners[r.Endpoints]; ok {
			reservedBy[ref.String()+"/"+r.Pod] = true
		}
	}

	// Pods: the in-force policies selecting each, in rank order.
	policies := slices.Clone(in.policies)
	sort.Slice(policies, func(i, j int) bool { return egress.Before(policies[i].rank, policies[j].rank) })
	wantSets := map[string]podSet{}
	podSetKey := map[string]string{}
	for _, pod := range in.pods {
		if !egress.Live(pod) || pod.Spec.HostNetwork {
			continue
		}
		name := pod.Namespace + "/" + pod.Name
		st := podState{ips: podIPs(pod)}
		var parts []string
		var refs []egress.PolicyRef
		reserved := ""
		for _, p := range policies {
			if !p.selects(pod, in) {
				continue
			}
			ready := false
			for _, k := range slotsOf[p.ref] {
				s := want[k]
				if s.identity != "" && s.identity != p.identity(pod) {
					continue // another PerPod pod's address
				}
				ready = ready || s.state == "Active"
			}
			if !ready && p.failClosed() {
				st.hold = true
			}
			part := p.ref.String()
			if p.perPod() {
				part += "#" + p.identity(pod)
			}
			if reservedBy[p.ref.String()+"/"+p.identity(pod)] {
				reserved = name
			}
			parts = append(parts, part)
			refs = append(refs, p.ref)
		}
		if len(parts) > 0 {
			key := strings.Join(parts, ",")
			if reserved != "" {
				key += "|" + reserved
			}
			wantSets[key] = podSet{key: key, policies: refs, reserved: reserved}
			podSetKey[name] = key
		}
		pn.pods[name] = st
	}

	// Ids: what keeps its key keeps its id; ids freed stay retired while
	// conntrack carries them; new keys take the lowest free id, slots
	// first (the allocator budgeted them), then pod-sets by rank.
	taken := map[uint8]bool{}
	for k := range want {
		if id, ok := prev.slotIDs[k]; ok {
			pn.slotIDs[k], pn.slots[id], taken[id] = id, want[k], true
		}
	}
	for key := range wantSets {
		if id, ok := prev.setIDs[key]; ok {
			pn.setIDs[key], pn.sets[id], taken[id] = id, wantSets[key], true
		}
	}
	retire := func(id uint8) {
		if !taken[id] && in.inUse != nil && in.inUse(id) {
			pn.retired[id], taken[id] = true, true
		}
	}
	for id := range prev.retired {
		retire(id)
	}
	for id := range prev.slots {
		retire(id)
	}
	for id := range prev.sets {
		retire(id)
	}
	next := func(limit int) (uint8, bool) {
		for id := 1; id <= limit; id++ {
			if !taken[uint8(id)] {
				return uint8(id), true
			}
		}
		return 0, false
	}
	newSlots := make([]slotKey, 0, len(want))
	for k := range want {
		if _, ok := pn.slotIDs[k]; !ok {
			newSlots = append(newSlots, k)
		}
	}
	sort.Slice(newSlots, func(i, j int) bool {
		if newSlots[i].endpoints != newSlots[j].endpoints {
			return newSlots[i].endpoints < newSlots[j].endpoints
		}
		return newSlots[i].index < newSlots[j].index
	})
	for _, k := range newSlots {
		id, ok := next(slotBudget)
		if !ok {
			pn.short = append(pn.short, k.endpoints)
			continue
		}
		pn.slotIDs[k], pn.slots[id], taken[id] = id, want[k], true
	}
	newSets := make([]podSet, 0, len(wantSets))
	for key, ps := range wantSets {
		if _, ok := pn.setIDs[key]; !ok {
			newSets = append(newSets, ps)
		}
	}
	rankOf := map[egress.PolicyRef]egress.Rank{}
	for _, p := range policies {
		rankOf[p.ref] = p.rank
	}
	sort.Slice(newSets, func(i, j int) bool {
		a, b := rankOf[newSets[i].policies[0]], rankOf[newSets[j].policies[0]]
		if a != b {
			return egress.Before(a, b)
		}
		return newSets[i].key < newSets[j].key
	})
	for _, ps := range newSets {
		limit := slotBudget - in.reservedCluster
		if ps.policies[0].Kind == egress.KindClusterEgress {
			limit = slotBudget
		}
		id, ok := next(limit)
		if !ok {
			pn.short = append(pn.short, ps.key)
			continue
		}
		pn.setIDs[ps.key], pn.sets[id], taken[id] = id, ps, true
	}

	for name, key := range podSetKey {
		st := pn.pods[name]
		st.podSet = pn.setIDs[key]
		pn.pods[name] = st
	}
	sort.Strings(pn.short)
	return pn
}

// selects reports whether p selects pod (§5.3), with the exclusions the
// allocator also applies; outranking needs no check here, because the
// pod's policies run in rank order (D24).
func (p *pnPolicy) selects(pod *corev1.Pod, in pnInput) bool {
	switch p.ref.Kind {
	case egress.KindClusterEgress:
		ns, ok := in.namespaces[pod.Namespace]
		if !ok || !p.namespaces.Matches(labels.Set(ns.Labels)) ||
			!egress.NamespaceInScope(ns, p.admittedOnly, p.group, p.sg, in.bindings) {
			return false
		}
	default:
		if pod.Namespace != p.ref.Namespace {
			return false
		}
	}
	return p.pods.Matches(labels.Set(pod.Labels)) && egress.PodExcluded(pod, p.optOut, p.perPod()) == ""
}

// podIPs are a pod's addresses.
func podIPs(pod *corev1.Pod) []netip.Addr {
	var out []netip.Addr
	for _, ip := range pod.Status.PodIPs {
		if a, err := netip.ParseAddr(ip.IP); err == nil {
			out = append(out, a)
		}
	}
	return out
}
