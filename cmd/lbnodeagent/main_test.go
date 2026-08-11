// Copyright 2020-2026 Acnodal Inc.
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

package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-kit/log"
	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

const testNode = "node-1"

// recordedEvent is one client.Errorf call. The event kind is the
// operator-facing contract -- "ConfigError" is what tells someone their
// node is announcing nothing -- so the tests assert on it by name.
type recordedEvent struct {
	kind string
	obj  runtime.Object
}

type fakeNodeClient struct {
	clientset kubernetes.Interface
	events    []recordedEvent
}

func (f *fakeNodeClient) Clientset() kubernetes.Interface { return f.clientset }

func (f *fakeNodeClient) Errorf(obj runtime.Object, kind, _ string, _ ...interface{}) {
	f.events = append(f.events, recordedEvent{kind: kind, obj: obj})
}

func (f *fakeNodeClient) kinds() []string {
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, e.kind)
	}
	return out
}

// fakeConfigSetter stands in for the announcer. It records the config it
// was handed so tests can assert that a rejected delivery never reached
// it, and that nodeSelector filtering happened before it did.
type fakeConfigSetter struct {
	ret     k8s.SyncState
	calls   int
	lastCfg *purelbv2.Config
}

func (f *fakeConfigSetter) SetConfig(cfg *purelbv2.Config) k8s.SyncState {
	f.calls++
	f.lastCfg = cfg
	return f.ret
}

func nodeWithLabels(labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNode, Labels: labels}}
}

func lbna(name string, selector *metav1.LabelSelector, local *purelbv2.LBNodeAgentLocalSpec) *purelbv2.LBNodeAgent {
	return &purelbv2.LBNodeAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: name},
		Spec:       purelbv2.LBNodeAgentSpec{NodeSelector: selector, Local: local},
	}
}

// harness wires newConfigChanged to fakes and exposes what the tests
// need to assert on.
type harness struct {
	client   *fakeNodeClient
	ctrl     *fakeConfigSetter
	selector *atomic.Pointer[election.InterfaceSelector]
	deliver  func(*purelbv2.Config) k8s.SyncState
}

func newHarness(t *testing.T, node *corev1.Node, ret k8s.SyncState) *harness {
	t.Helper()
	h := &harness{
		client:   &fakeNodeClient{clientset: k8sfake.NewSimpleClientset(node)},
		ctrl:     &fakeConfigSetter{ret: ret},
		selector: &atomic.Pointer[election.InterfaceSelector]{},
	}
	h.deliver = newConfigChanged(log.NewNopLogger(),
		func() nodeClient { return h.client }, h.ctrl, testNode, h.selector)
	return h
}

// assertSelectorState checks that exactly one of the four states is
// active. Reading only the expected label would pass even if the gauge
// reported every state at once.
func assertSelectorState(t *testing.T, want string) {
	t.Helper()
	for _, state := range []string{"default", "configured", "deselected", "invalid"} {
		expected := 0.0
		if state == want {
			expected = 1.0
		}
		assert.Equal(t, expected, ptu.ToFloat64(selectorState.WithLabelValues(state)),
			"selector_state{state=%q}", state)
	}
}

// TestConfigChangedNodeGetFailure: a delivery that cannot read its own
// Node must be requeued, and must NOT reach the announcer. Applying a
// config whose nodeSelector was never evaluated would announce from a
// node that may have been deselected.
func TestConfigChangedNodeGetFailure(t *testing.T) {
	h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateSuccess)
	h.client.clientset.(*k8sfake.Clientset).PrependReactor("get", "nodes",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})

	got := h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
		lbna("default", nil, &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"}),
	}})

	assert.Equal(t, k8s.SyncStateError, got, "delivery must be requeued")
	assert.Zero(t, h.ctrl.calls, "announcer must not be configured from an unevaluated delivery")
	assert.Nil(t, h.selector.Load(), "selector must be left untouched")
}

// TestConfigChangedSelectorStates covers all four states of the
// selector/announcer coherence machine. The invariant under test is that
// the lease never advertises a subnet the announcer cannot announce on.
func TestConfigChangedSelectorStates(t *testing.T) {
	localSpec := &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"}

	t.Run("configured", func(t *testing.T) {
		h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateSuccess)
		got := h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
			lbna("default", nil, localSpec),
		}})
		assert.Equal(t, k8s.SyncStateSuccess, got)
		assertSelectorState(t, "configured")
		require.NotNil(t, h.selector.Load(), "a working local config must store a selector")
		assert.True(t, h.selector.Load().UseDefault)
	})

	t.Run("default when no agent has a local spec", func(t *testing.T) {
		// Remote-only cluster: the selector stays nil so the election
		// falls back to default-interface detection, which is the
		// pre-config behaviour.
		h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateSuccess)
		got := h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
			lbna("remote-only", nil, nil),
		}})
		assert.Equal(t, k8s.SyncStateSuccess, got)
		assertSelectorState(t, "default")
		assert.Nil(t, h.selector.Load(), "nil selector means default detection")
	})

	t.Run("deselected", func(t *testing.T) {
		// Every CR's nodeSelector excludes this node. The selector must
		// become EMPTY, not nil: a nil (default-detection) selector here
		// would win elections this node cannot serve -- a blackhole.
		h := newHarness(t, nodeWithLabels(map[string]string{"role": "worker"}), k8s.SyncStateSuccess)
		got := h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
			lbna("scoped", &metav1.LabelSelector{
				MatchLabels: map[string]string{"role": "storage"},
			}, localSpec),
		}})
		assert.Equal(t, k8s.SyncStateSuccess, got)
		assertSelectorState(t, "deselected")
		sel := h.selector.Load()
		require.NotNil(t, sel, "deselected must store an empty selector, not nil")
		assert.False(t, sel.UseDefault, "an empty selector advertises nothing")
		assert.Empty(t, sel.Interfaces)
	})

	t.Run("invalid when the announcer rejects the config", func(t *testing.T) {
		h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateError)
		got := h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
			lbna("default", nil, localSpec),
		}})
		assert.Equal(t, k8s.SyncStateError, got)
		assertSelectorState(t, "invalid")
		sel := h.selector.Load()
		require.NotNil(t, sel)
		assert.False(t, sel.UseDefault, "a rejected config must advertise nothing")
		assert.Contains(t, h.client.kinds(), "ConfigError",
			"the operator must be told the node is announcing nothing")
	})

	t.Run("no agents at all is default", func(t *testing.T) {
		// preFilter == 0, so this is "nothing configured yet", not
		// "deselected". The distinction matters: a fresh pod before its
		// first CR delivery must not blackhole.
		h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateSuccess)
		got := h.deliver(&purelbv2.Config{})
		assert.Equal(t, k8s.SyncStateSuccess, got)
		assertSelectorState(t, "default")
		assert.Nil(t, h.selector.Load())
	})
}

// TestConfigChangedReportsInvalidNodeSelector: AgentsForNode silently
// drops an agent whose selector will not convert, which looks exactly
// like the CR not existing. The event is the only signal an operator gets.
func TestConfigChangedReportsInvalidNodeSelector(t *testing.T) {
	h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateSuccess)
	bad := lbna("broken", &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "role", Operator: "NotAnOperator", Values: []string{"x"}},
		},
	}, &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"})

	h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{bad}})

	assert.Contains(t, h.client.kinds(), "InvalidNodeSelector")
}

// TestConfigChangedReportsIgnoredAgents: when several CRs match, the one
// reported as "in force" must be the first with a Local spec, not simply
// the highest-precedence match. An LBNodeAgent carrying only a
// nodeSelector sorts to the front and is skipped.
func TestConfigChangedReportsIgnoredAgents(t *testing.T) {
	h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateSuccess)
	selectorOnly := lbna("aaa-selector-only", nil, nil)
	withLocal := lbna("bbb-with-local", nil, &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"})

	h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{selectorOnly, withLocal}})

	require.Len(t, h.client.events, 1, "exactly the non-winning agent is evented")
	assert.Equal(t, "ConfigIgnored", h.client.events[0].kind)
	assert.Same(t, selectorOnly, h.client.events[0].obj,
		"the selector-only CR is the one ignored; the local-spec CR is in force")
	assertSelectorState(t, "configured")
}

// TestConfigChangedFiltersBeforeAnnouncer pins the ordering: the
// announcer must receive only the agents that apply to this node.
func TestConfigChangedFiltersBeforeAnnouncer(t *testing.T) {
	h := newHarness(t, nodeWithLabels(map[string]string{"role": "worker"}), k8s.SyncStateSuccess)
	mine := lbna("mine", &metav1.LabelSelector{
		MatchLabels: map[string]string{"role": "worker"},
	}, &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "default"})
	theirs := lbna("theirs", &metav1.LabelSelector{
		MatchLabels: map[string]string{"role": "storage"},
	}, &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "eth9"})

	h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{mine, theirs}})

	require.Equal(t, 1, h.ctrl.calls)
	require.Len(t, h.ctrl.lastCfg.Agents, 1)
	assert.Equal(t, "mine", h.ctrl.lastCfg.Agents[0].Name)
}

// TestConfigChangedInvalidLocalInterfaceRegex: SelectorFromConfig fails
// on an uncompilable regex. Even though SetConfig succeeded here, the
// selector must go empty rather than advertise something the announcer
// cannot match.
func TestConfigChangedInvalidLocalInterfaceRegex(t *testing.T) {
	h := newHarness(t, nodeWithLabels(nil), k8s.SyncStateSuccess)
	h.deliver(&purelbv2.Config{Agents: []*purelbv2.LBNodeAgent{
		lbna("default", nil, &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "eth[0-"}),
	}})

	assertSelectorState(t, "invalid")
	sel := h.selector.Load()
	require.NotNil(t, sel)
	assert.False(t, sel.UseDefault)
	assert.Nil(t, sel.Regex)
}

func TestRecordSelectorState(t *testing.T) {
	for _, state := range []string{"default", "configured", "deselected", "invalid"} {
		t.Run(state, func(t *testing.T) {
			recordSelectorState(state)
			assertSelectorState(t, state)
		})
	}

	t.Run("unknown state clears every label", func(t *testing.T) {
		recordSelectorState("not-a-state")
		for _, state := range []string{"default", "configured", "deselected", "invalid"} {
			assert.Zero(t, ptu.ToFloat64(selectorState.WithLabelValues(state)))
		}
	})
}

func TestParseDurationEnv(t *testing.T) {
	const key = "PURELB_TEST_DURATION"
	def := 7 * time.Second

	t.Run("unset returns the default", func(t *testing.T) {
		t.Setenv(key, "")
		assert.Equal(t, def, parseDurationEnv(key, def))
	})

	t.Run("valid duration is parsed", func(t *testing.T) {
		t.Setenv(key, "1500ms")
		assert.Equal(t, 1500*time.Millisecond, parseDurationEnv(key, def))
	})

	t.Run("unparseable falls back to the default", func(t *testing.T) {
		// A typo in PURELB_LEASE_DURATION must not crash the agent or
		// yield a zero lease duration.
		t.Setenv(key, "not-a-duration")
		assert.Equal(t, def, parseDurationEnv(key, def))
	})

	t.Run("bare number falls back", func(t *testing.T) {
		// "10" has no unit, which time.ParseDuration rejects.
		t.Setenv(key, "10")
		assert.Equal(t, def, parseDurationEnv(key, def))
	})
}
