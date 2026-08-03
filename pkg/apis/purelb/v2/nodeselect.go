// Copyright 2020-2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v2

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// AgentsForNode returns the agents whose NodeSelector matches
// nodeLabels, in precedence order: specific selectors before
// catch-alls, then namespace/name sort. A nil selector and an empty
// selector (no matchLabels, no matchExpressions) are both catch-alls —
// LabelSelectorAsSelector turns an empty selector into
// labels.Everything(), so ranking it "specific" would invert the
// default-plus-override precedence. Agents whose selector cannot be
// converted are skipped; the caller is responsible for logging or
// eventing skipped and ignored agents.
func AgentsForNode(agents []*LBNodeAgent, nodeLabels map[string]string) []*LBNodeAgent {
	var specific, catchAll []*LBNodeAgent

	set := labels.Set(nodeLabels)
	for _, agent := range agents {
		if agent == nil {
			continue
		}
		if isCatchAll(agent.Spec.NodeSelector) {
			catchAll = append(catchAll, agent)
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(agent.Spec.NodeSelector)
		if err != nil {
			// Invalid selector: this agent can't match any node.
			continue
		}
		if selector.Matches(set) {
			specific = append(specific, agent)
		}
	}

	sortByNamespaceName(specific)
	sortByNamespaceName(catchAll)
	return append(specific, catchAll...)
}

// FirstLocalAgent returns the first agent with Spec.Local != nil, or
// nil if there is none. It is shared by the announcer and the election
// selector so that both sides always choose the same agent.
func FirstLocalAgent(agents []*LBNodeAgent) *LBNodeAgent {
	for _, agent := range agents {
		if agent != nil && agent.Spec.Local != nil {
			return agent
		}
	}
	return nil
}

// isCatchAll reports whether the selector matches all nodes: nil, or
// empty (no matchLabels and no matchExpressions).
func isCatchAll(sel *metav1.LabelSelector) bool {
	return sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0)
}

func sortByNamespaceName(agents []*LBNodeAgent) {
	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].Namespace != agents[j].Namespace {
			return agents[i].Namespace < agents[j].Namespace
		}
		return agents[i].Name < agents[j].Name
	})
}
