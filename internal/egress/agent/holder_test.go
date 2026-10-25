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
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// nodeIndexer holds nodes with the given labels.
func nodeIndexer(t *testing.T, nodes map[string]map[string]string) cache.Indexer {
	t.Helper()
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for name, l := range nodes {
		require.NoError(t, idx.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}))
	}
	return idx
}

var capable = map[string]string{purelbv2.EgressCapableLabel: "true"}

func withLabel(k, v string) map[string]string {
	return map[string]string{purelbv2.EgressCapableLabel: "true", k: v}
}

func group(sel *metav1.LabelSelector) *purelbv2.ServiceGroup {
	return &purelbv2.ServiceGroup{Spec: purelbv2.ServiceGroupSpec{Egress: &purelbv2.ServiceGroupEgressSpec{AnnouncerNodeSelector: sel}}}
}

// A node is eligible when it is egress-capable and the group's
// announcerNodeSelector selects it (§5.5).
func TestEligibility(t *testing.T) {
	nodes := nodeIndexer(t, map[string]map[string]string{
		"a": capable,
		"b": withLabel("edge", "true"),
		"c": {"edge": "true"}, // not egress-capable
	})
	all := eligibility(nodes, group(nil))
	assert.True(t, all("a"))
	assert.True(t, all("b"))
	assert.False(t, all("c"), "not egress-capable")
	assert.False(t, all("gone"), "an unknown node")
	assert.True(t, eligibility(nodes, nil)("a"), "no group: capability only")

	edge := eligibility(nodes, group(&metav1.LabelSelector{MatchLabels: map[string]string{"edge": "true"}}))
	assert.False(t, edge("a"))
	assert.True(t, edge("b"))
	assert.False(t, edge("c"))

	bad := eligibility(nodes, group(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "edge", Operator: "Bogus"}}}))
	assert.False(t, bad("b"), "a selector that doesn't convert selects no node")
}

// fakeElector records what the owner lookups pass to the election.
type fakeElector struct {
	eligible  func(string) bool
	preferred []string
	steering  bool
}

func (f *fakeElector) WinnerAmong(key string, eligible func(string) bool, preferred []string) string {
	f.eligible, f.preferred, f.steering = eligible, preferred, false
	return "owner"
}

func (f *fakeElector) SteeringWinner(key string, eligible func(string) bool, preferred []string) string {
	f.eligible, f.preferred, f.steering = eligible, preferred, true
	return "steer"
}

func (f *fakeElector) SteeringLive() []string { return nil }

func (f *fakeElector) LastRenewal() time.Time { return time.Time{} }

func (f *fakeElector) LeaseDuration() time.Duration { return 10 * time.Second }

// Owning asks WinnerAmong and steering SteeringWinner, both with the
// group's eligibility and the preferred nodes.
func TestOwnerLookups(t *testing.T) {
	e := &fakeElector{}
	a := New(log.NewNopLogger(), testNode, "purelb-system")
	a.SetElection(e)
	s := &session{nodes: nodeIndexer(t, map[string]map[string]string{"a": capable, "c": nil})}
	g := group(nil)

	assert.Equal(t, "owner", a.localOwner(s, "10.0.0.9", g, []string{"a"}))
	assert.False(t, e.steering)
	assert.Equal(t, []string{"a"}, e.preferred)
	assert.True(t, e.eligible("a"))
	assert.False(t, e.eligible("c"))

	assert.Equal(t, "steer", a.steeringOwner(s, "10.0.0.9", g, nil))
	assert.True(t, e.steering)
	assert.False(t, e.eligible("c"))
}
