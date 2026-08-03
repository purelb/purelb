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

package v2

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func agent(namespace, name string, selector *metav1.LabelSelector, local *LBNodeAgentLocalSpec) *LBNodeAgent {
	return &LBNodeAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       LBNodeAgentSpec{NodeSelector: selector, Local: local},
	}
}

func names(agents []*LBNodeAgent) []string {
	result := []string{}
	for _, a := range agents {
		result = append(result, a.Namespace+"/"+a.Name)
	}
	return result
}

func TestAgentsForNode(t *testing.T) {
	nodeLabels := map[string]string{
		"kubernetes.io/hostname": "node-1",
		"zone":                   "a",
	}

	matchNode1 := &metav1.LabelSelector{
		MatchLabels: map[string]string{"kubernetes.io/hostname": "node-1"},
	}
	matchOther := &metav1.LabelSelector{
		MatchLabels: map[string]string{"kubernetes.io/hostname": "node-2"},
	}
	matchExprZone := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "b"}},
		},
	}
	invalid := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "zone", Operator: "BogusOperator"},
		},
	}

	tests := []struct {
		name   string
		agents []*LBNodeAgent
		want   []string
	}{
		{name: "empty agents", agents: nil, want: []string{}},
		{
			name:   "nil selector matches all",
			agents: []*LBNodeAgent{agent("purelb", "default", nil, nil)},
			want:   []string{"purelb/default"},
		},
		{
			name:   "matchLabels match and no-match",
			agents: []*LBNodeAgent{agent("purelb", "scoped", matchNode1, nil), agent("purelb", "other", matchOther, nil)},
			want:   []string{"purelb/scoped"},
		},
		{
			name:   "matchExpressions",
			agents: []*LBNodeAgent{agent("purelb", "zoned", matchExprZone, nil)},
			want:   []string{"purelb/zoned"},
		},
		{
			name:   "specific beats catch-all regardless of list order",
			agents: []*LBNodeAgent{agent("purelb", "aaa-default", nil, nil), agent("purelb", "zzz-scoped", matchNode1, nil)},
			want:   []string{"purelb/zzz-scoped", "purelb/aaa-default"},
		},
		{
			name:   "empty selector is catch-all, not specific",
			agents: []*LBNodeAgent{agent("purelb", "aaa-empty", &metav1.LabelSelector{}, nil), agent("purelb", "zzz-scoped", matchNode1, nil)},
			want:   []string{"purelb/zzz-scoped", "purelb/aaa-empty"},
		},
		{
			name: "namespace/name tie-break within each class",
			agents: []*LBNodeAgent{
				agent("zeta", "a", matchNode1, nil),
				agent("alpha", "b", matchNode1, nil),
				agent("alpha", "a", matchNode1, nil),
				agent("beta", "catch", nil, nil),
				agent("alpha", "catch", nil, nil),
			},
			want: []string{"alpha/a", "alpha/b", "zeta/a", "alpha/catch", "beta/catch"},
		},
		{
			name:   "invalid selector skipped, others still considered",
			agents: []*LBNodeAgent{agent("purelb", "broken", invalid, nil), agent("purelb", "default", nil, nil)},
			want:   []string{"purelb/default"},
		},
		{
			name:   "no match at all",
			agents: []*LBNodeAgent{agent("purelb", "other", matchOther, nil)},
			want:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, names(AgentsForNode(tt.agents, nodeLabels)))
		})
	}
}

func TestFirstLocalAgent(t *testing.T) {
	local := &LBNodeAgentLocalSpec{LocalInterface: "default"}

	assert.Nil(t, FirstLocalAgent(nil))
	assert.Nil(t, FirstLocalAgent([]*LBNodeAgent{agent("purelb", "remote-only", nil, nil)}))

	first := agent("purelb", "first", nil, local)
	second := agent("purelb", "second", nil, local)
	assert.Same(t, first, FirstLocalAgent([]*LBNodeAgent{agent("purelb", "remote-only", nil, nil), first, second}))
}
