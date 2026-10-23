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
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// PolicyEffective reasons (§4.8).
const (
	reasonOutrankedBy        = "OutrankedBy"
	reasonPartiallyOutranked = "PartiallyOutranked"
)

// idxMemberPod indexes EgressMembers by the "namespace/name" of each pod
// they list.
const idxMemberPod = "memberPod"

func memberPodIndex(obj interface{}) ([]string, error) {
	m, ok := obj.(*purelbv2.EgressMembers)
	if !ok {
		return nil, nil
	}
	var out []string
	for _, n := range m.Spec.Nodes {
		for _, p := range n.Pods {
			ns := p.Namespace
			if ns == "" {
				ns = m.Labels[egress.LabelNamespace]
			}
			out = append(out, ns+"/"+p.Name)
		}
	}
	return out, nil
}

// outranking is what the higher-ranked policies selecting a policy's pods
// take from it (D24): every policy selecting a pod contributes, and the
// first whose destinations match a flow supplies its slot.
type outranking struct {
	by      map[string]bool // policies that take every destination of some pod
	partial bool            // some pod shares destinations with a higher policy
}

// outranked reports whether a higher-ranked policy that selects pod takes
// every destination the policy has for it, so it never supplies the pod's
// slot. The other policies' selections are their EgressMembers, which
// record exactly the pods they selected.
func (r *Reconciler) outranked(run *run, pod *corev1.Pod) bool {
	objs, _ := r.membersIdx.ByIndex(idxMemberPod, pod.Namespace+"/"+pod.Name)
	if run.outrank.by == nil {
		run.outrank.by = map[string]bool{}
	}
	mine := run.p.rank()
	full := false
	for _, o := range objs {
		ref, ok := r.ownerRef(o.(*purelbv2.EgressMembers).Labels)
		if !ok || ref == run.p.ref {
			continue
		}
		q, found, _ := r.load(ref)
		if !found || !egress.Before(q.rank(), mine) || !contained(q) ||
			!destinationsOverlap(q.spec.Destinations, run.p.spec.Destinations) {
			continue
		}
		if destinationsCover(q.spec.Destinations, run.p.spec.Destinations) {
			run.outrank.by[ref.String()] = true
			full = true
			continue
		}
		run.outrank.partial = true
	}
	return full
}

// policyEffective reports the outranking: OutrankedBy when every pod the
// policy would select is taken, PartiallyOutranked when some pod or some
// destinations are.
func policyEffective(run *run) {
	o := run.outrank
	if len(o.by) == 0 && !o.partial {
		return
	}
	by := make([]string, 0, len(o.by))
	for k := range o.by {
		by = append(by, k)
	}
	sort.Strings(by)
	if run.sel.selected == 0 && len(by) > 0 {
		run.conds.fail(condPolicyEffective, reasonOutrankedBy, "every selected pod's destinations are taken by %s", strings.Join(by, ", "))
		return
	}
	run.conds.fail(condPolicyEffective, reasonPartiallyOutranked, "higher-ranked policies take some selected pods or destinations: %s",
		strings.Join(by, ", "))
}

func (r *Reconciler) ownerRef(l map[string]string) (egress.PolicyRef, bool) {
	ref := egress.PolicyRef{Kind: l[egress.LabelKind], Namespace: l[egress.LabelNamespace], Name: l[egress.LabelName]}
	return ref, ref.Name != ""
}

// contained reports whether q's last published status has Contained True
// or not yet reported; a revoked policy takes nothing.
func contained(q *policy) bool {
	c := apimeta.FindStatusCondition(q.status.Conditions, condContained)
	return c == nil || c.Status != "False"
}

// destinationsCover reports whether hi matches every flow lo matches:
// hi excludes nothing, and each of lo's entries lies inside one of hi's.
func destinationsCover(hi, lo purelbv2.EgressDestinations) bool {
	if len(hi.Exclude) > 0 {
		return false
	}
	for _, l := range lo.Include {
		covered := false
		for _, h := range hi.Include {
			covered = covered || (cidrsCover(h.CIDRs, l.CIDRs) && portsCover(h.Ports, l.Ports))
		}
		if !covered {
			return false
		}
	}
	return true
}

// destinationsOverlap reports whether some network is in both a's and
// b's includes; ports and excludes are ignored, so it may say yes for
// policies that never meet.
func destinationsOverlap(a, b purelbv2.EgressDestinations) bool {
	for _, x := range a.Include {
		for _, y := range b.Include {
			for _, cx := range x.CIDRs {
				for _, cy := range y.CIDRs {
					px, err1 := netip.ParsePrefix(cx)
					py, err2 := netip.ParsePrefix(cy)
					if err1 == nil && err2 == nil && px.Overlaps(py) {
						return true
					}
				}
			}
		}
	}
	return false
}

// cidrsCover reports whether every prefix in lo lies inside one in hi.
func cidrsCover(hi, lo []string) bool {
	for _, l := range lo {
		pl, err := netip.ParsePrefix(l)
		if err != nil {
			return false
		}
		in := false
		for _, h := range hi {
			ph, err := netip.ParsePrefix(h)
			in = in || (err == nil && ph.Addr().Is4() == pl.Addr().Is4() && ph.Bits() <= pl.Bits() && ph.Contains(pl.Addr()))
		}
		if !in {
			return false
		}
	}
	return true
}

// portsCover reports whether hi's ports match every port lo's do: hi has
// none (every port), or each of lo's ranges lies in one of hi's with the
// same protocol.
func portsCover(hi, lo []purelbv2.EgressPortRange) bool {
	if len(hi) == 0 {
		return true
	}
	if len(lo) == 0 {
		return false
	}
	end := func(p purelbv2.EgressPortRange) int32 {
		if p.EndPort != nil {
			return *p.EndPort
		}
		return p.Port
	}
	proto := func(p purelbv2.EgressPortRange) string {
		if p.Protocol == "" {
			return string(corev1.ProtocolTCP)
		}
		return p.Protocol
	}
	for _, l := range lo {
		in := false
		for _, h := range hi {
			in = in || (proto(h) == proto(l) && h.Port <= l.Port && end(l) <= end(h))
		}
		if !in {
			return false
		}
	}
	return true
}
