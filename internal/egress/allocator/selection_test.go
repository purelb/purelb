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
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	kubefake "k8s.io/client-go/kubernetes/fake"
	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	purelbfake "purelb.io/pkg/generated/clientset/versioned/fake"
)

func ptr[T any](v T) *T { return &v }

// group is a local ServiceGroup that allows egress.
func group(name string, namespaces ...string) *purelbv2.ServiceGroup {
	return &purelbv2.ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: purelbv2.ServiceGroupSpec{
			Namespaces: namespaces,
			Local: &purelbv2.ServiceGroupLocalSpec{V4Pool: &purelbv2.AddressPool{
				Name: "public", Pool: "192.168.10.10-192.168.10.20", Subnet: "192.168.10.0/24"}},
			Egress: &purelbv2.ServiceGroupEgressSpec{Allowed: true, AffinityModes: []string{"Hash", "PerPod", "Inherit"}},
		},
	}
}

func node(name string, capable bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if capable {
		n.Labels[purelbv2.EgressCapableLabel] = "true"
	}
	return n
}

func namespace(name string, l map[string]string) *corev1.Namespace {
	if l == nil {
		l = map[string]string{}
	}
	l["kubernetes.io/metadata.name"] = name
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

func pod(ns, name, nodeName string, l map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name), Labels: l, Annotations: map[string]string{}},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func stsPod(ns, name, nodeName string, l map[string]string) *corev1.Pod {
	p := pod(ns, name, nodeName, l)
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: ptr(true)}}
	return p
}

func egressObj(ns, name string, mod func(*purelbv2.EgressSpec)) *purelbv2.Egress {
	e := &purelbv2.Egress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name), CreationTimestamp: metav1.Now()},
		Spec: purelbv2.EgressSpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
			EgressPolicy: purelbv2.EgressPolicy{Destinations: purelbv2.EgressDestinations{
				Include: []purelbv2.EgressDestination{{CIDRs: []string{"0.0.0.0/0"}}}}},
			OptOut: "Allowed",
		},
	}
	if mod != nil {
		mod(&e.Spec)
	}
	return e
}

// fixture is a started Reconciler with groups loaded into its allocator.
type fixture struct {
	r      *Reconciler
	kube   *kubefake.Clientset
	purelb *purelbfake.Clientset
	q      *queue
}

func newFixture(t *testing.T, groups []*purelbv2.ServiceGroup, objs ...runtime.Object) *fixture {
	t.Helper()
	r, kube, purelb, q := started(t, true, objs...)
	r.alloc.SetListServices(func() []corev1.Service {
		var out []corev1.Service
		for _, o := range r.services.List() {
			out = append(out, *o.(*corev1.Service))
		}
		return out
	})
	require.NoError(t, r.alloc.SetPools(groups))
	return &fixture{r: r, kube: kube, purelb: purelb, q: q}
}

// reconcile runs one reconcile of key on the test goroutine, as the queue
// goroutine would, then completes its writes and waits for the informers
// to see them.
func (f *fixture) reconcile(t *testing.T, key string) {
	t.Helper()
	require.NoError(t, f.r.Sync(key))
	f.settle(t)
}

// settle collects every write notification until none is in flight.
func (f *fixture) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.q.mu.Lock()
		var writes, rest []string
		for _, k := range f.q.keys {
			if strings.HasPrefix(k, writePrefix) {
				writes = append(writes, k)
			} else {
				rest = append(rest, k)
			}
		}
		f.q.keys = rest
		f.q.mu.Unlock()
		for _, k := range writes {
			require.NoError(t, f.r.Sync(k))
		}
		if f.r.writer.inFlight == 0 && len(f.r.writer.pending) == 0 && len(writes) == 0 {
			break
		}
		require.True(t, time.Now().Before(deadline), "writes didn't settle")
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let the informers see the writes
}

func (f *fixture) members(t *testing.T) []purelbv2.EgressMembers {
	t.Helper()
	l, err := f.purelb.PurelbV2().EgressMemberses(testNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items
}

// podsOf flattens EgressMembers to "node/[ns.]pod[=identity]".
func podsOf(ms []purelbv2.EgressMembers) []string {
	var out []string
	for _, m := range ms {
		for _, n := range m.Spec.Nodes {
			for _, p := range n.Pods {
				s := n.Node + "/" + p.Name
				if p.Namespace != "" {
					s = n.Node + "/" + p.Namespace + "." + p.Name
				}
				if p.Identity != "" {
					s += "=" + p.Identity
				}
				out = append(out, s)
			}
		}
	}
	return out
}

// An Egress selects its namespace's matching pods on egress-capable
// nodes, reports the ones it excludes, and lists the rest in
// EgressMembers. When its group stops allowing egress, they go.
func TestSelection(t *testing.T) {
	sel := map[string]string{"app": "a"}
	hostNet := pod("voice", "hostnet", "n1", sel)
	hostNet.Spec.HostNetwork = true
	optedOut := pod("voice", "optout", "n1", sel)
	optedOut.Annotations[purelbv2.EgressOptOutAnnotation] = "false"
	done := pod("voice", "done", "n1", sel)
	done.Status.Phase = corev1.PodSucceeded
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default")},
		node("n1", true), node("n2", true), node("old", false), namespace("voice", nil),
		pod("voice", "a1", "n1", sel), pod("voice", "a2", "n2", sel), pod("voice", "b", "n1", map[string]string{"app": "b"}),
		pod("voice", "unscheduled", "", sel), pod("voice", "on-old", "old", sel), hostNet, optedOut, done,
		pod("other", "elsewhere", "n1", sel),
		egressObj("voice", "sip", nil))

	run := &run{conds: conditions{}}
	p, _, _ := f.r.load(egress.PolicyRef{Kind: egress.KindEgress, Namespace: "voice", Name: "sip"})
	run.p = p
	f.r.scope(run)
	require.True(t, run.conds.ok(condContained), "%v", run.conds)
	f.r.selectPods(run)
	assert.Equal(t, int32(2), run.sel.selected)
	var excluded []string
	for _, e := range run.sel.excluded {
		excluded = append(excluded, e.Pod+":"+e.Reason)
	}
	assert.ElementsMatch(t, []string{"hostnet:HostNetwork", "optout:OptedOut", "on-old:NodeUnsupported"}, excluded)

	f.reconcile(t, "egress/voice/sip")
	ms := f.members(t)
	require.Len(t, ms, 1)
	assert.Equal(t, "e-voice.sip-0", ms[0].Name)
	assert.Equal(t, "sip", ms[0].Labels[egress.LabelName])
	assert.Equal(t, []string{"n1/a1", "n2/a2"}, podsOf(ms))

	// The group stops allowing egress: the selection is withdrawn.
	g := group("default")
	g.Spec.Egress.Allowed = false
	require.NoError(t, f.r.alloc.SetPools([]*purelbv2.ServiceGroup{g}))
	f.reconcile(t, "egress/voice/sip")
	assert.Empty(t, f.members(t))
}

// A ClusterEgress selects across the namespaces its selector matches that
// its group serves; with AdmittedOnly, only admitted ones. PerPod pods
// carry their namespace in their identity.
func TestClusterSelection(t *testing.T) {
	sel := map[string]string{"app": "db"}
	ceg := &purelbv2.ClusterEgress{
		ObjectMeta: metav1.ObjectMeta{Name: "pci", UID: "uid-pci", CreationTimestamp: metav1.Now()},
		Spec: purelbv2.ClusterEgressSpec{
			ServiceGroup:      "pci",
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"zone": "pci"}},
			PodSelector:       metav1.LabelSelector{MatchLabels: sel},
			NamespaceScope:    "AdmittedOnly",
			EgressPolicy: purelbv2.EgressPolicy{
				Affinity:     &purelbv2.EgressAffinity{Mode: "PerPod"},
				Destinations: purelbv2.EgressDestinations{Include: []purelbv2.EgressDestination{{CIDRs: []string{"0.0.0.0/0"}}}}},
			Priority: 1000, OptOut: "Denied",
		},
	}
	f := newFixture(t, []*purelbv2.ServiceGroup{group("pci", "payments", "ledger", "unadmitted")},
		node("n1", true),
		namespace("payments", map[string]string{"zone": "pci", admittedLabel: "true"}),
		namespace("ledger", map[string]string{"zone": "pci", admittedLabel: "true"}),
		namespace("unadmitted", map[string]string{"zone": "pci"}),
		namespace("unserved", map[string]string{"zone": "pci", admittedLabel: "true"}),
		namespace("web", nil),
		stsPod("payments", "db-0", "n1", sel), stsPod("ledger", "db-0", "n1", sel),
		stsPod("unadmitted", "db-0", "n1", sel), stsPod("unserved", "db-0", "n1", sel), stsPod("web", "db-0", "n1", sel),
		pod("payments", "loose", "n1", sel), ceg)

	f.reconcile(t, "clusteregress/pci")
	assert.Equal(t, []string{"n1/ledger.db-0=ledger.db-0", "n1/payments.db-0=payments.db-0"}, podsOf(f.members(t)))

	run := &run{conds: conditions{}}
	run.p, _, _ = f.r.load(egress.PolicyRef{Kind: egress.KindClusterEgress, Name: "pci"})
	f.r.scope(run)
	f.r.selectPods(run)
	var excluded []string
	for _, e := range run.sel.excluded {
		excluded = append(excluded, e.Namespace+"/"+e.Pod+":"+e.Reason)
	}
	assert.ElementsMatch(t, []string{"unadmitted/db-0:NamespaceNotInScope", "unserved/db-0:NamespaceNotInScope",
		"payments/loose:NotStatefulSet"}, excluded)
}

// The group bounds what a policy may do.
func TestScopeChecks(t *testing.T) {
	pci := group("pci")
	pci.Spec.Local.V4Pool = &purelbv2.AddressPool{Name: "public", Pool: "192.168.11.10-192.168.11.20", Subnet: "192.168.11.0/24"}
	pci.Spec.Egress.AffinityModes = []string{"Hash"}
	pci.Spec.Egress.Tenancy = &purelbv2.EgressTenancy{PriorityCeiling: ptr(int32(50)), MaxEgressesPerNamespace: ptr(int32(1))}
	unbounded := &purelbv2.ClusterEgress{ObjectMeta: metav1.ObjectMeta{Name: "wide"},
		Spec: purelbv2.ClusterEgressSpec{ServiceGroup: "default",
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"zone": "x"}}}}
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default"), pci}, namespace("t", nil), unbounded,
		egressObj("t", "perpod", func(s *purelbv2.EgressSpec) {
			s.ServiceGroup = "pci"
			s.Affinity = &purelbv2.EgressAffinity{Mode: "PerPod"}
		}),
		egressObj("t", "helper", func(s *purelbv2.EgressSpec) { s.Helpers = []purelbv2.EgressHelper{"SIP"} }),
		egressObj("t", "missing", func(s *purelbv2.EgressSpec) { s.ServiceGroup = "nope" }),
		egressObj("t", "high", func(s *purelbv2.EgressSpec) { s.ServiceGroup = "pci"; s.Priority = 900 }),
		egressObj("t", "second", func(s *purelbv2.EgressSpec) { s.ServiceGroup = "pci" }))

	reason := func(ref egress.PolicyRef) (string, *run) {
		run := &run{conds: conditions{}}
		run.p, _, _ = f.r.load(ref)
		f.r.scope(run)
		return run.conds[condContained].reason, run
	}
	eg := func(name string) egress.PolicyRef {
		return egress.PolicyRef{Kind: egress.KindEgress, Namespace: "t", Name: name}
	}

	r, run := reason(eg("perpod"))
	assert.Equal(t, reasonFeatureNotAllowed, r)
	assert.True(t, run.revoked)
	r, _ = reason(eg("helper"))
	assert.Equal(t, reasonFeatureNotAllowed, r, "the default group allows no helpers") // and helpers need PerPod
	r, _ = reason(eg("missing"))
	assert.Equal(t, reasonServiceGroupNotFound, r)
	r, run = reason(eg("high"))
	assert.Equal(t, reasonPriorityClamped, r)
	assert.False(t, run.revoked, "a clamped policy keeps running")
	assert.Equal(t, int32(50), run.p.priority)
	r, _ = reason(egress.PolicyRef{Kind: egress.KindClusterEgress, Name: "wide"})
	assert.Equal(t, reasonScopeUnbounded, r)

	// Quota 1 on pci: of perpod, high and second (all on pci), only the
	// first created fits. They share a creation second, so UID decides:
	// uid-high < uid-perpod < uid-second.
	r, _ = reason(eg("second"))
	assert.Equal(t, reasonQuotaExceeded, r)
}

// Selections split at 500 pods per EgressMembers, the same way every time.
func TestMembersSplit(t *testing.T) {
	run := &run{p: &policy{ref: egress.PolicyRef{Kind: egress.KindEgress, Namespace: "ns", Name: "e"}},
		sel: selection{nodes: map[string][]member{}}}
	for i := 0; i < 501; i++ {
		n := fmt.Sprintf("n%d", i%3)
		run.sel.nodes[n] = append(run.sel.nodes[n], member{name: fmt.Sprintf("p%03d", i)})
	}
	ms := desiredMembers(run)
	require.Len(t, ms, 2)
	count := func(m *purelbv2.EgressMembers) (c int) {
		for _, n := range m.Spec.Nodes {
			c += len(n.Pods)
		}
		return
	}
	assert.Equal(t, 500, count(ms[0]))
	assert.Equal(t, 1, count(ms[1]))
	assert.Equal(t, []string{"e-ns.e-0", "e-ns.e-1"}, []string{ms[0].Name, ms[1].Name})
	assert.Equal(t, ms, desiredMembers(run))
}
