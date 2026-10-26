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
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// candidatePods narrows to the pods carrying an equality or set
// requirement's value, and falls back to the namespace otherwise.
func TestCandidatePods(t *testing.T) {
	f := newFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("a", nil), namespace("b", nil),
		pod("a", "web-1", "n1", map[string]string{"app": "web"}), pod("a", "db-1", "n1", map[string]string{"app": "db"}),
		pod("a", "other", "n1", map[string]string{"tier": "x"}), pod("b", "web-2", "n1", map[string]string{"app": "web"}))
	names := func(sel *metav1.LabelSelector) []string {
		s, err := metav1.LabelSelectorAsSelector(sel)
		require.NoError(t, err)
		var out []string
		for _, o := range f.r.candidatePods("a", s) {
			out = append(out, o.(*corev1.Pod).Name)
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, []string{"web-1"}, names(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}))
	assert.Equal(t, []string{"db-1", "web-1"}, names(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web", "db"}}}}))
	assert.Equal(t, []string{"db-1", "other", "web-1"}, names(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"web"}}}}), "no equality: the namespace")
}

// The allocator plans from its own last write until the informer has it.
func TestLatestEndpoints(t *testing.T) {
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil), egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	ep := g.ep(t)

	newer := ep.DeepCopy()
	newer.Generation++
	g.r.wrote(ep.Name, newer, nil)
	got, ok := g.r.latestEndpoints(ep.Name)
	require.True(t, ok)
	assert.Equal(t, newer.Generation, got.Generation, "own newer write")
	_, kept := g.r.written.Load(ep.Name)
	assert.True(t, kept)

	g.r.wrote(ep.Name, ep.DeepCopy(), nil) // the informer is as new
	got, _ = g.r.latestEndpoints(ep.Name)
	assert.Equal(t, ep.Generation, got.Generation)
	_, kept = g.r.written.Load(ep.Name)
	assert.False(t, kept, "caught up: forgotten")
}

// While an address's write is in flight, a reconcile leaves the address
// alone instead of queueing a write that would conflict, and the policy
// reruns when the write finishes.
func TestDeferWhileInFlight(t *testing.T) {
	sel := map[string]string{"app": "a"}
	g := newGatingFixture(t, []*purelbv2.ServiceGroup{group("default")}, namespace("voice", nil),
		node("n1", true), node("n2", true), pod("voice", "a-1", "n1", sel), egressObj("voice", "sip", nil))
	g.reconcile(t, "egress/voice/sip")
	key := "EgressEndpoints/" + g.ep(t).Name

	// The fake clientset holds one lock while a reactor runs, so nothing
	// below calls the API until release.
	release := make(chan struct{})
	var updates int32
	g.purelb.PrependReactor("update", "egressendpoints", func(k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&updates, 1)
		<-release
		return false, nil, nil
	})
	require.NoError(t, g.r.Sync("egress/voice/sip")) // activates n1's slice: a write goes out
	require.Eventually(t, func() bool { return atomic.LoadInt32(&updates) == 1 }, time.Second, 10*time.Millisecond)
	_, err := g.kube.CoreV1().Pods("voice").Create(context.Background(), pod("voice", "a-2", "n2", sel), metav1.CreateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	g.q.resetKeepingWrites()
	require.True(t, g.r.writer.inProgress(key))
	require.NoError(t, g.r.Sync("egress/voice/sip")) // n2 now wanted, but the write is in flight
	close(release)
	g.settle(t)
	assert.Equal(t, int32(1), atomic.LoadInt32(&updates), "no second write queued behind the first")
	assert.True(t, g.q.has("egress/voice/sip"), "rerun when the write finished")
}
