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

package uninstall

import (
	"context"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	purelbfake "purelb.io/pkg/generated/clientset/versioned/fake"
)

func agent(ns, name string, egress *purelbv2.LBNodeAgentEgressSpec) *purelbv2.LBNodeAgent {
	return &purelbv2.LBNodeAgent{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: purelbv2.LBNodeAgentSpec{Egress: egress}}
}

func nodeState(name, programmed string) *purelbv2.EgressNodeState {
	return &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: purelbv2.EgressNodeStateStatus{Programmed: programmed}}
}

// Every LBNodeAgent with egress on is disabled, the rest left alone; the
// hook returns once no node reports egress programmed.
func TestRun(t *testing.T) {
	purelb := purelbfake.NewSimpleClientset(
		agent("purelb-system", "default", &purelbv2.LBNodeAgentEgressSpec{Enabled: true, FlowLog: true}),
		agent("purelb-system", "edge", &purelbv2.LBNodeAgentEgressSpec{Enabled: false}),
		agent("other", "lb", nil),
		nodeState("node-a", "v0.17.0"), nodeState("node-b", ""))
	ctx := context.Background()
	go func() {
		// node-a's agent tears down once its LBNodeAgent says so.
		for range 200 {
			a, err := purelb.PurelbV2().LBNodeAgents("purelb-system").Get(ctx, "default", metav1.GetOptions{})
			if err == nil && !a.Spec.Egress.Enabled {
				time.Sleep(50 * time.Millisecond)
				_, _ = purelb.PurelbV2().EgressNodeStates().UpdateStatus(ctx, nodeState("node-a", ""), metav1.UpdateOptions{})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	require.NoError(t, Run(ctx, log.NewNopLogger(), purelb, 5*time.Second, 20*time.Millisecond))

	a, err := purelb.PurelbV2().LBNodeAgents("purelb-system").Get(ctx, "default", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, purelbv2.LBNodeAgentEgressSpec{Enabled: false, FlowLog: true}, *a.Spec.Egress, "only enabled changed")
	patches := 0
	for _, act := range purelb.Actions() {
		if act.GetVerb() == "patch" {
			patches++
		}
	}
	assert.Equal(t, 1, patches, "only the LBNodeAgent with egress on")
	b, err := purelb.PurelbV2().LBNodeAgents("other").Get(ctx, "lb", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, b.Spec.Egress)
}

// A node that never reports its teardown fails the hook, naming it.
func TestRunTimeout(t *testing.T) {
	purelb := purelbfake.NewSimpleClientset(nodeState("node-b", "v0.17.0"), nodeState("node-a", "v0.17.0"), nodeState("node-c", ""))
	start := time.Now()
	err := Run(context.Background(), log.NewNopLogger(), purelb, 200*time.Millisecond, 20*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[node-a node-b]")
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
}
