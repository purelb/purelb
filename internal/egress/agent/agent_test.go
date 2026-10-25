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
	"net/netip"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	purelbfake "purelb.io/pkg/generated/clientset/versioned/fake"
)

const testNode = "n1"

var testNodeObj = &corev1.Node{
	ObjectMeta: metav1.ObjectMeta{Name: testNode, UID: "uid-n1"},
	Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
		{Type: corev1.NodeInternalIP, Address: "fd00::1"},
		{Type: corev1.NodeHostName, Address: testNode},
	}},
}

// fixture is an Agent started on fake clients, with its work queue run by
// one goroutine, as the agent's queue goroutine runs it. Its datapath is a
// fake that records each desired state (the kernel's needs privileges:
// internal/egress/datapath tests it).
type fixture struct {
	a      *Agent
	kube   *kubefake.Clientset
	purelb *purelbfake.Clientset
	wants  chan dpWant
}

func newFixture(t *testing.T, crds bool, objs ...runtime.Object) *fixture {
	t.Helper()
	var kubeObjs, purelbObjs []runtime.Object
	for _, o := range objs {
		switch o.(type) {
		case *purelbv2.EgressNodeState, *purelbv2.EgressAddress, *purelbv2.EgressEndpoints:
			purelbObjs = append(purelbObjs, o)
		default:
			kubeObjs = append(kubeObjs, o)
		}
	}
	kube := kubefake.NewSimpleClientset(kubeObjs...)
	if crds {
		kube.Resources = []*metav1.APIResourceList{{
			GroupVersion: purelbv2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{{Name: "egressnodestates"}, {Name: "lbnodeagents"}},
		}}
	}
	purelb := purelbfake.NewSimpleClientset(purelbObjs...)
	a := New(log.NewNopLogger(), testNode, "purelb-system")
	a.statusInterval = 50 * time.Millisecond
	wants := make(chan dpWant, 100)
	a.apply = func(w dpWant) (*purelbv2.EgressCapability, error) {
		wants <- w
		c := probeCapability(w.spec)
		c.TunnelEncapsulation = "L3"
		return c, nil
	}
	keys := make(chan string, 1000)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	require.NoError(t, a.start(stop, kube, purelb, func(k string, _ bool) { keys <- k }))
	go func() {
		for {
			select {
			case <-stop:
				return
			case k := <-keys:
				_ = a.Sync(k)
			}
		}
	}()
	return &fixture{a: a, kube: kube, purelb: purelb, wants: wants}
}

func agentWith(spec *purelbv2.LBNodeAgentEgressSpec) *purelbv2.Config {
	return &purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{{Spec: purelbv2.LBNodeAgentSpec{Egress: spec}}}}
}

// nodeState returns this node's EgressNodeState, or nil.
func (f *fixture) nodeState() *purelbv2.EgressNodeState {
	ens, err := f.purelb.PurelbV2().EgressNodeStates().Get(context.Background(), testNode, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return ens
}

// writes counts the agent's EgressNodeState writes so far.
func (f *fixture) writes() int {
	n := 0
	for _, a := range f.purelb.Actions() {
		if a.GetResource().Resource == "egressnodestates" && (a.GetVerb() == "patch" || a.GetVerb() == "create") {
			n++
		}
	}
	return n
}

// Without the egress CRDs the agent does nothing, whatever its
// LBNodeAgent says.
func TestOffWithoutCRDs(t *testing.T) {
	f := newFixture(t, false, testNodeObj)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true}))
	assert.NoError(t, f.a.Sync(keyConfig))
	assert.Nil(t, f.a.on)
	assert.Empty(t, f.purelb.Actions())
}

// With egress off (D-idle) nothing egress runs: no informer, no write, only
// the one read that checks egress wasn't left reported on.
func TestIdle(t *testing.T) {
	f := newFixture(t, true, testNodeObj)
	f.a.Configure(agentWith(nil))
	time.Sleep(300 * time.Millisecond)
	for _, a := range f.purelb.Actions() {
		assert.Equal(t, "get", a.GetVerb(), "only the startup check: %v", a)
	}
	assert.Zero(t, f.writes())
	for _, a := range f.kube.Actions() {
		assert.NotContains(t, []string{"list", "watch"}, a.GetVerb(), "no Node informer: %v", a)
	}
}

// Enabling egress creates the node's EgressNodeState, owned by its Node,
// and reports the capability with both underlay addresses.
func TestEnableReportsCapability(t *testing.T) {
	f := newFixture(t, true, testNodeObj)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true}))
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && ens.Status.Capability != nil
	}, 5*time.Second, 20*time.Millisecond)
	ens := f.nodeState()
	require.Len(t, ens.OwnerReferences, 1)
	assert.Equal(t, metav1.OwnerReference{APIVersion: "v1", Kind: "Node", Name: testNode, UID: "uid-n1"}, ens.OwnerReferences[0])
	c := ens.Status.Capability
	assert.NotEmpty(t, c.Kernel)
	assert.Equal(t, defaultMarkField, c.MarkField)
	assert.Equal(t, "L3", c.TunnelEncapsulation)
	assert.Equal(t, &purelbv2.EgressUnderlay{IPv4: "10.0.0.1", IPv6: "fd00::1"}, c.Underlay)
}

// An EgressNodeState the allocator created first is reused.
func TestEnableReusesNodeState(t *testing.T) {
	existing := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode},
		Spec: purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{{Endpoints: "e-a", Address: "192.0.2.1", Index: 1, State: "Active"}}}}
	f := newFixture(t, true, testNodeObj, existing)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true, MarkField: "0x0000ff00"}))
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && ens.Status.Capability != nil
	}, 5*time.Second, 20*time.Millisecond)
	ens := f.nodeState()
	assert.Equal(t, "0x0000ff00", ens.Status.Capability.MarkField)
	assert.Len(t, ens.Spec.Slices, 1, "the allocator's spec is left alone")
	for _, a := range f.purelb.Actions() {
		assert.NotEqual(t, "create", a.GetVerb())
	}
}

// Disabling egress reports egress off, which removes the node's
// egress-capable label (D29), and stops the informers.
func TestDisableReportsOff(t *testing.T) {
	f := newFixture(t, true, testNodeObj)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true}))
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && ens.Status.Capability != nil && ens.Status.Capability.EgressCapable
	}, 5*time.Second, 20*time.Millisecond)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: false}))
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return ens != nil && !ens.Status.Capability.EgressCapable
	}, 5*time.Second, 20*time.Millisecond)
}

// An agent that starts with egress off corrects a capability left on by
// an earlier run.
func TestStartOffCorrectsStaleCapability(t *testing.T) {
	stale := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode},
		Status: purelbv2.EgressNodeStateStatus{Capability: &purelbv2.EgressCapability{EgressCapable: true, Kernel: "6.12.0"}}}
	f := newFixture(t, true, testNodeObj, stale)
	f.a.Configure(agentWith(nil))
	require.Eventually(t, func() bool {
		ens := f.nodeState()
		return !ens.Status.Capability.EgressCapable
	}, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, "6.12.0", f.nodeState().Status.Capability.Kernel, "the rest of the status is kept")
}

// The status writer writes at most once per interval, the latest status,
// and skips a status already written.
func TestStatusWriterDebounce(t *testing.T) {
	purelb := purelbfake.NewSimpleClientset(&purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode}})
	w := newStatusWriter(log.NewNopLogger(), testNode, kubefake.NewSimpleClientset(testNodeObj), purelb)
	w.interval = 300 * time.Millisecond
	stop := make(chan struct{})
	defer close(stop)
	go w.run(stop)
	patches := func() int {
		n := 0
		for _, a := range purelb.Actions() {
			if a.GetVerb() == "patch" {
				n++
			}
		}
		return n
	}
	for i := 0; i < 20; i++ {
		w.set(&purelbv2.EgressNodeStateStatus{Capability: &purelbv2.EgressCapability{Kernel: string(rune('a' + i))}})
		time.Sleep(10 * time.Millisecond)
	}
	require.Eventually(t, func() bool {
		ens, _ := purelb.PurelbV2().EgressNodeStates().Get(context.Background(), testNode, metav1.GetOptions{})
		return ens.Status.Capability != nil && ens.Status.Capability.Kernel == "t"
	}, 5*time.Second, 20*time.Millisecond)
	assert.LessOrEqual(t, patches(), 2, "200 ms of updates at a 300 ms interval")
	n := patches()
	w.set(&purelbv2.EgressNodeStateStatus{Capability: &purelbv2.EgressCapability{Kernel: "t"}})
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, n, patches(), "an unchanged status is not rewritten")
}

// A failed write is retried.
func TestStatusWriterRetries(t *testing.T) {
	purelb := purelbfake.NewSimpleClientset(&purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode}})
	fail := 2
	purelb.PrependReactor("patch", "egressnodestates", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail > 0 {
			fail--
			return true, nil, assert.AnError
		}
		return false, nil, nil
	})
	w := newStatusWriter(log.NewNopLogger(), testNode, kubefake.NewSimpleClientset(testNodeObj), purelb)
	w.interval, w.retry = 10*time.Millisecond, 50*time.Millisecond
	stop := make(chan struct{})
	defer close(stop)
	go w.run(stop)
	w.set(&purelbv2.EgressNodeStateStatus{Capability: &purelbv2.EgressCapability{Kernel: "6.12"}})
	require.Eventually(t, func() bool {
		ens, _ := purelb.PurelbV2().EgressNodeStates().Get(context.Background(), testNode, metav1.GetOptions{})
		return ens.Status.Capability != nil
	}, 5*time.Second, 20*time.Millisecond)
}

// Configure takes the first matching LBNodeAgent that has an egress spec.
func TestConfigurePicksFirstEgressAgent(t *testing.T) {
	a := New(log.NewNopLogger(), testNode, "purelb-system")
	a.Configure(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
		{Spec: purelbv2.LBNodeAgentSpec{}},
		{Spec: purelbv2.LBNodeAgentSpec{Egress: &purelbv2.LBNodeAgentEgressSpec{Enabled: true, TunnelPort: 7000}}},
		{Spec: purelbv2.LBNodeAgentSpec{Egress: &purelbv2.LBNodeAgentEgressSpec{Enabled: false}}},
	}})
	d := a.config.Load()
	require.NotNil(t, d.spec)
	assert.Equal(t, int32(7000), d.spec.TunnelPort)
}

// The datapath runs only the latest item, and fails liveness only when
// one item runs past the bound.
func TestDatapath(t *testing.T) {
	d := newRunner()
	release := make(chan struct{})
	ran := make(chan int, 10)
	d.submit(func() { <-release; ran <- 0 })
	stop := make(chan struct{})
	defer close(stop)
	go d.run(stop)
	require.Eventually(t, func() bool { return d.started.Load() != 0 }, time.Second, time.Millisecond)
	for i := 1; i <= 3; i++ {
		d.submit(func() { ran <- i })
	}
	assert.NoError(t, d.live(time.Now()))
	assert.Error(t, d.live(time.Now().Add(livenessStuckAfter+time.Second)))
	close(release)
	assert.Equal(t, 0, <-ran)
	assert.Equal(t, 3, <-ran, "only the latest pending item runs")
	select {
	case i := <-ran:
		t.Fatalf("item %d ran", i)
	case <-time.After(100 * time.Millisecond):
	}
	require.Eventually(t, func() bool { return d.live(time.Now().Add(time.Hour)) == nil }, time.Second, time.Millisecond)
}

// The datapath gets every node's underlay addresses, and again when one
// changes, but not for an unrelated Node update (status heartbeats).
func TestDatapathWant(t *testing.T) {
	peer := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.2"}}}}
	f := newFixture(t, true, testNodeObj, peer)
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true, TunnelPort: 7000}))
	var w dpWant
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
			return len(w.nodes) == 2
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, testNode, w.self)
	assert.Equal(t, int32(7000), w.spec.TunnelPort)
	assert.Equal(t, dpNode{name: "n1", v4: netip.MustParseAddr("10.0.0.1"), v6: netip.MustParseAddr("fd00::1")}, w.nodes[0])
	assert.Equal(t, dpNode{name: "n2", v4: netip.MustParseAddr("10.0.0.2")}, w.nodes[1])

	drain := func() {
		for {
			select {
			case <-f.wants:
			case <-time.After(300 * time.Millisecond):
				return
			}
		}
	}
	drain()
	ctx := context.Background()
	// An update egress doesn't read (an annotation: labels decide
	// eligibility and addresses the node map) re-applies nothing.
	peer.Annotations = map[string]string{"x": "y"}
	_, err := f.kube.CoreV1().Nodes().Update(ctx, peer, metav1.UpdateOptions{})
	require.NoError(t, err)
	select {
	case w := <-f.wants:
		t.Fatalf("re-applied for a Node update egress doesn't read: %v", w)
	case <-time.After(300 * time.Millisecond):
	}
	peer.Status.Addresses = append(peer.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fd00::2"})
	_, err = f.kube.CoreV1().Nodes().Update(ctx, peer, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
			return len(w.nodes) == 2 && w.nodes[1].v6.IsValid()
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)
}
