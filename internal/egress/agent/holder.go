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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Elector is what egress asks the Lease election (internal/election).
type Elector interface {
	// WinnerAmong is the node that announces a local address: the
	// election among eligible nodes with the address's subnet, preferring
	// preferred, "" while this node's lease is unhealthy.
	WinnerAmong(key string, eligible func(node string) bool, preferred []string) string
	// SteeringWinner is the same election as this pod node sees it,
	// keeping the last good membership while its lease is unhealthy (D1).
	SteeringWinner(key string, eligible func(node string) bool, preferred []string) string
	// SteeringLive is the live nodes in the steering view.
	SteeringLive() []string
}

// SetElection gives egress the node's election. Call it before Start.
func (a *Agent) SetElection(e Elector) {
	a.elect = e
}

// eligibility is group g's local-pool eligibility test (§5.5) in this
// node's view: an egress-capable node (the D29 label) that g's
// announcerNodeSelector selects. The other two parts, a live lease listing
// the pool's subnet and a guard not standing down (which empties the
// lease's subnets), are the election's. A selector that doesn't convert
// selects no node.
func eligibility(nodes cache.Indexer, g *purelbv2.ServiceGroup) func(string) bool {
	sel := labels.Everything()
	if g != nil && g.Spec.Egress != nil && g.Spec.Egress.AnnouncerNodeSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(g.Spec.Egress.AnnouncerNodeSelector)
		if err != nil {
			return func(string) bool { return false }
		}
		sel = s
	}
	return func(name string) bool {
		obj, ok, _ := nodes.GetByKey(name)
		if !ok {
			return false
		}
		l := obj.(*corev1.Node).Labels
		return l[purelbv2.EgressCapableLabel] == "true" && sel.Matches(labels.Set(l))
	}
}

// localOwner is the node that announces local address addr of group g,
// preferring preferred (§5.5: preferPodNodes' Active-slice holders, or a
// PerPod address's pod node).
func (a *Agent) localOwner(s *session, addr string, g *purelbv2.ServiceGroup, preferred []string) string {
	return a.elect.WinnerAmong(addr, eligibility(s.nodes, g), preferred)
}

// steeringOwner is the node this pod node steers local address addr's
// traffic to (D1).
func (a *Agent) steeringOwner(s *session, addr string, g *purelbv2.ServiceGroup, preferred []string) string {
	return a.elect.SteeringWinner(addr, eligibility(s.nodes, g), preferred)
}
