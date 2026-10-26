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
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func pnPod(ns, name string, l map[string]string, ips ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: l},
		Spec: corev1.PodSpec{NodeName: testNode}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	for _, ip := range ips {
		p.Status.PodIPs = append(p.Status.PodIPs, corev1.PodIP{IP: ip})
	}
	return p
}

func pnEgress(ns, name string, prio int32, mod func(*pnPolicy)) *pnPolicy {
	p := &pnPolicy{ref: egress.PolicyRef{Kind: egress.KindEgress, Namespace: ns, Name: name},
		rank: egress.Rank{Priority: prio, UID: name}, pods: labels.SelectorFromSet(labels.Set{"app": "a"})}
	if mod != nil {
		mod(p)
	}
	return p
}

// pnIn is a planPodNode input with one slice in state for each policy's
// endpoints "e-<name>".
func pnIn(state string, pods []*corev1.Pod, policies ...*pnPolicy) pnInput {
	in := pnInput{pods: pods, policies: policies, owners: map[string]egress.PolicyRef{}}
	for i, p := range policies {
		ep := "e-" + p.ref.Name
		in.owners[ep] = p.ref
		in.spec.Slices = append(in.spec.Slices, purelbv2.EgressNodeSlice{Endpoints: ep,
			Address: fmt.Sprintf("192.0.2.%d", i+1), Index: 2, State: state, SliceSize: 4096})
	}
	return in
}

var appA = map[string]string{"app": "a"}

// A pod is in the pod-set of the policies selecting it, in rank order;
// pods with the same list share one. Unselected, host-network, opted-out
// and finished pods get none.
func TestPlanPodNodeSelection(t *testing.T) {
	hi := pnEgress("voice", "hi", 500, nil)
	lo := pnEgress("voice", "lo", 100, nil)
	other := pnEgress("web", "other", 100, nil)
	host := pnPod("voice", "host", appA)
	host.Spec.HostNetwork = true
	out := pnPod("voice", "out", appA)
	out.Annotations = map[string]string{purelbv2.EgressOptOutAnnotation: "false"}
	gone := pnPod("voice", "gone", appA)
	gone.Status.Phase = corev1.PodSucceeded
	pods := []*corev1.Pod{pnPod("voice", "a-1", appA, "10.1.0.5", "fd01::5"), pnPod("voice", "a-2", appA),
		pnPod("voice", "b", nil), host, out, gone}
	pn := planPodNode(pnIn("Active", pods, lo, other, hi), nil)

	require.Len(t, pn.slots, 3)
	require.Len(t, pn.sets, 1)
	a1 := pn.pods["voice/a-1"]
	assert.Equal(t, []egress.PolicyRef{hi.ref, lo.ref}, pn.sets[a1.podSet].policies,
		"rank order; the other namespace's policy doesn't select")
	assert.Equal(t, a1.podSet, pn.pods["voice/a-2"].podSet, "same policies, same pod-set")
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.1.0.5"), netip.MustParseAddr("fd01::5")}, a1.ips)
	assert.False(t, a1.hold)
	assert.Zero(t, pn.pods["voice/b"].podSet, "unselected")
	assert.Zero(t, pn.pods["voice/out"].podSet, "opted out")
	assert.NotContains(t, pn.pods, "voice/host")
	assert.NotContains(t, pn.pods, "voice/gone")
	assert.Equal(t, 4, pn.used(), "3 slots and 1 pod-set")
	assert.Empty(t, pn.short)

	// optOut Denied ignores the annotation.
	denied := pnEgress("voice", "lo", 100, func(p *pnPolicy) { p.optOut = "Denied" })
	pn = planPodNode(pnIn("Active", []*corev1.Pod{out}, denied), nil)
	assert.NotZero(t, pn.pods["voice/out"].podSet)
}

// A ClusterEgress selects pods in the namespaces its selector matches,
// inside its group's namespace boundary, and admitted ones under
// AdmittedOnly.
func TestPlanPodNodeClusterScope(t *testing.T) {
	sg := &purelbv2.ServiceGroup{ObjectMeta: metav1.ObjectMeta{Name: "pci"}} // serves every namespace
	tenant := &purelbv2.ServiceGroup{ObjectMeta: metav1.ObjectMeta{Name: "tenant"},
		Spec: purelbv2.ServiceGroupSpec{Namespaces: []string{"ledger"}, EnforceNamespaces: true}}
	ns := func(name string, admitted bool) *corev1.Namespace {
		l := map[string]string{"zone": "pci"}
		if admitted {
			l[egress.AdmittedLabel] = "true"
		}
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
	}
	ce := &pnPolicy{ref: egress.PolicyRef{Kind: egress.KindClusterEgress, Name: "pci"}, rank: egress.Rank{Priority: 1000},
		pods: labels.Everything(), namespaces: labels.SelectorFromSet(labels.Set{"zone": "pci"}), group: "pci", sg: sg}
	pods := []*corev1.Pod{pnPod("pay", "p", nil), pnPod("ledger", "l", nil), pnPod("other", "o", nil)}
	in := pnIn("Active", pods, ce)
	in.namespaces = map[string]*corev1.Namespace{"pay": ns("pay", true), "ledger": ns("ledger", false), "other": ns("other", true)}
	in.bindings = purelbv2.BindNamespaces([]*purelbv2.ServiceGroup{sg, tenant})

	pn := planPodNode(in, nil)
	assert.NotZero(t, pn.pods["pay/p"].podSet)
	assert.NotZero(t, pn.pods["other/o"].podSet)
	assert.Zero(t, pn.pods["ledger/l"].podSet, "ledger is confined to its tenant group")

	ce.admittedOnly = true
	in.bindings = purelbv2.BindNamespaces([]*purelbv2.ServiceGroup{sg})
	pn = planPodNode(in, nil)
	assert.NotZero(t, pn.pods["pay/p"].podSet)
	assert.Zero(t, pn.pods["ledger/l"].podSet, "not admitted")

	sg.Spec.Namespaces = []string{"pay"}
	pn = planPodNode(in, nil)
	assert.NotZero(t, pn.pods["pay/p"].podSet)
	assert.Zero(t, pn.pods["other/o"].podSet, "outside the group's namespaces")
}

// A FailClosed policy holds its pods until their slot is Active; a
// FailOpen one doesn't.
func TestPlanPodNodeHold(t *testing.T) {
	closed := pnEgress("voice", "closed", 100, nil)
	open := pnEgress("voice", "open", 100, func(p *pnPolicy) { p.spec.FailurePolicy = "FailOpen" })
	pods := []*corev1.Pod{pnPod("voice", "a", appA)}
	assert.True(t, planPodNode(pnIn("Pending", pods, closed), nil).pods["voice/a"].hold)
	assert.False(t, planPodNode(pnIn("Active", pods, closed), nil).pods["voice/a"].hold)
	assert.False(t, planPodNode(pnIn("Pending", pods, open), nil).pods["voice/a"].hold)
	assert.True(t, planPodNode(pnIn("Pending", pods, closed), nil).pods["voice/a"].podSet != 0,
		"the pod-set exists, for when the slot is")
}

// A PerPod pod has its own pod-set and only its own address's slot counts
// for it; a pod holding a reserved port has its own pod-set (D19).
func TestPlanPodNodePerPodAndReserved(t *testing.T) {
	pp := pnEgress("db", "pp", 100, func(p *pnPolicy) { p.spec.Affinity = &purelbv2.EgressAffinity{Mode: "PerPod"} })
	sts := func(name string) *corev1.Pod {
		p := pnPod("db", name, appA)
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: ptr(true)}}
		return p
	}
	in := pnInput{pods: []*corev1.Pod{sts("db-0"), sts("db-1"), pnPod("db", "loose", appA)}, policies: []*pnPolicy{pp},
		owners: map[string]egress.PolicyRef{"e-pp-0": pp.ref, "e-pp-1": pp.ref},
		spec: purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{
			{Endpoints: "e-pp-0", Address: "192.0.2.10", Index: 0, State: "Active", Identity: "db-0"},
			{Endpoints: "e-pp-1", Address: "192.0.2.11", Index: 0, State: "Pending", Identity: "db-1"},
		}}}
	pn := planPodNode(in, nil)
	assert.NotEqual(t, pn.pods["db/db-0"].podSet, pn.pods["db/db-1"].podSet)
	assert.False(t, pn.pods["db/db-0"].hold, "its own address is Active")
	assert.True(t, pn.pods["db/db-1"].hold, "its own address is Pending, whatever db-0's is")
	assert.Zero(t, pn.pods["db/loose"].podSet, "PerPod selects StatefulSet pods only")

	hash := pnEgress("voice", "sip", 100, nil)
	in = pnIn("Active", []*corev1.Pod{pnPod("voice", "a", appA), pnPod("voice", "b", appA), pnPod("voice", "c", appA)}, hash)
	in.spec.Reserved = []purelbv2.EgressNodeReserved{{Endpoints: "e-sip", Address: "192.0.2.1", Protocol: "UDP", Port: 5060, Pod: "a"}}
	pn = planPodNode(in, nil)
	assert.NotEqual(t, pn.pods["voice/a"].podSet, pn.pods["voice/b"].podSet)
	assert.Equal(t, pn.pods["voice/b"].podSet, pn.pods["voice/c"].podSet)
	assert.Equal(t, "voice/a", pn.sets[pn.pods["voice/a"].podSet].reserved)
}

// Ids survive a recompute; a freed id stays retired while conntrack
// carries it; new keys take the lowest free id.
func TestPlanPodNodeIDs(t *testing.T) {
	p1 := pnEgress("voice", "p1", 100, nil)
	p2 := pnEgress("voice", "p2", 200, func(p *pnPolicy) { p.pods = labels.SelectorFromSet(labels.Set{"app": "b"}) })
	pods := []*corev1.Pod{pnPod("voice", "a", appA), pnPod("voice", "b", map[string]string{"app": "b"})}
	first := planPodNode(pnIn("Active", pods, p1, p2), nil)
	require.Len(t, first.slots, 2)
	require.Len(t, first.sets, 2)

	again := planPodNode(pnIn("Active", pods, p1, p2), first)
	assert.Equal(t, first.slotIDs, again.slotIDs)
	assert.Equal(t, first.setIDs, again.setIDs)

	// p2 goes: its slot and pod-set ids retire while conntrack has them.
	carried := map[uint8]bool{}
	for id := range first.slots {
		if first.slots[id].policy == p2.ref {
			carried[id] = true
		}
	}
	in := pnIn("Active", pods[:1], p1)
	in.inUse = func(id uint8) bool { return carried[id] }
	gone := planPodNode(in, again)
	assert.Equal(t, carried, gone.retired)
	assert.Equal(t, 3, gone.used(), "1 slot, 1 pod-set, 1 retired")

	// A new policy can't take the retired id; once conntrack lets it go,
	// it's free again.
	p3 := pnEgress("voice", "p3", 300, nil)
	in = pnIn("Active", pods[:1], p1, p3)
	in.inUse = func(id uint8) bool { return carried[id] }
	next := planPodNode(in, gone)
	for id := range carried {
		_, slot := next.slots[id]
		_, set := next.sets[id]
		assert.False(t, slot || set, "retired id %d reused", id)
	}
	in.inUse = nil
	free := planPodNode(in, next)
	assert.Empty(t, free.retired)
}

// Beyond the budget, slots and pod-sets get no id and are reported;
// reservedClusterSlots keeps ids for pod-sets led by a ClusterEgress.
func TestPlanPodNodeBudget(t *testing.T) {
	var policies []*pnPolicy
	var pods []*corev1.Pod
	for i := 0; i < 64; i++ {
		l := map[string]string{"app": fmt.Sprint(i)}
		policies = append(policies, pnEgress("voice", fmt.Sprintf("p%02d", i), 100, func(p *pnPolicy) {
			p.pods = labels.SelectorFromSet(l)
		}))
		pods = append(pods, pnPod("voice", fmt.Sprintf("a%02d", i), l))
	}
	in := pnIn("Active", pods, policies...)
	pn := planPodNode(in, nil)
	assert.Equal(t, 127, pn.used())
	assert.Len(t, pn.short, 1, "64 slots + 64 pod-sets is one more than 127")

	in.reservedCluster = 10
	pn = planPodNode(in, nil)
	assert.Equal(t, 117, pn.used(), "namespaced pod-sets stop at 127 - 10")
	assert.Len(t, pn.short, 11)

	ce := &pnPolicy{ref: egress.PolicyRef{Kind: egress.KindClusterEgress, Name: "c"}, rank: egress.Rank{Priority: 1000},
		pods: labels.SelectorFromSet(labels.Set{"app": "0"}), namespaces: labels.Everything()}
	in = pnIn("Active", pods[:1], ce)
	in.namespaces = map[string]*corev1.Namespace{"voice": {ObjectMeta: metav1.ObjectMeta{Name: "voice"}}}
	in.reservedCluster = 127
	pn = planPodNode(in, nil)
	assert.Empty(t, pn.short, "a ClusterEgress may use the reserved ids")
}

func ptr[T any](v T) *T { return &v }

// The agent plans its pods from its informers and reports the slots it
// uses: a selected pod's pod-set comes and goes with the pod.
func TestPodNodeReportsSlots(t *testing.T) {
	const ep = "e-voice.sip-0a0b0c0d"
	ens := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode},
		Spec: purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{
			{Endpoints: ep, Address: "192.0.2.1", Index: 2, State: "Active", SliceSize: 4096}}}}
	ea := &purelbv2.EgressAddress{ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: ep,
		Labels: map[string]string{egress.LabelKind: egress.KindEgress, egress.LabelNamespace: "voice", egress.LabelName: "sip"}},
		Spec: purelbv2.EgressAddressSpec{State: "Active", Address: "192.0.2.1", Family: "IPv4", ServiceGroup: "default",
			PoolType: "Local", Placement: "SingleAnnouncer"}}
	eg := &purelbv2.Egress{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip", UID: "uid-sip"},
		Spec: purelbv2.EgressSpec{PodSelector: &metav1.LabelSelector{MatchLabels: appA}}}
	elsewhere := pnPod("voice", "a-2", appA)
	elsewhere.Spec.NodeName = "n2"
	f := newFixture(t, true, testNodeObj, ens, ea, eg, pnPod("voice", "a-1", appA, "10.1.0.5"), elsewhere)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true}))

	slots := func() *purelbv2.EgressSlots {
		if ns := f.nodeState(); ns != nil && ns.Status.Capability != nil {
			return ns.Status.Capability.Slots
		}
		return nil
	}
	require.Eventually(t, func() bool {
		s := slots()
		return s != nil && *s == purelbv2.EgressSlots{Used: 2, Capacity: 127, PodSets: 1}
	}, 5*time.Second, 20*time.Millisecond, "one slot and one pod-set; the other node's pod isn't this node's")

	require.NoError(t, f.kube.CoreV1().Pods("voice").Delete(context.Background(), "a-1", metav1.DeleteOptions{}))
	require.Eventually(t, func() bool {
		s := slots()
		return s != nil && *s == purelbv2.EgressSlots{Used: 1, Capacity: 127}
	}, 5*time.Second, 20*time.Millisecond, "the pod-set goes with its last pod")
}
