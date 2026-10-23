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
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"

	coreallocator "purelb.io/internal/allocator"
	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	purelbfake "purelb.io/pkg/generated/clientset/versioned/fake"
)

const testNS = "purelb-system"

// nopEvents is a k8s.ServiceEvent that records nothing.
type nopEvents struct{}

func (nopEvents) Infof(runtime.Object, string, string, ...interface{})  {}
func (nopEvents) Errorf(runtime.Object, string, string, ...interface{}) {}
func (nopEvents) ForceSync()                                            {}

// queue records what the reconciler enqueues.
type queue struct {
	mu   sync.Mutex
	keys []string
}

func (q *queue) add(key string, _ bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.keys = append(q.keys, key)
}

func (q *queue) has(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, k := range q.keys {
		if k == key {
			return true
		}
	}
	return false
}

func (q *queue) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.keys = nil
}

// started returns a Reconciler started on fake clients, with the egress
// CRDs reported installed when crds is true.
func started(t *testing.T, crds bool, objs ...runtime.Object) (*Reconciler, *kubefake.Clientset, *purelbfake.Clientset, *queue) {
	t.Helper()
	var kubeObjs, purelbObjs []runtime.Object
	for _, o := range objs {
		switch o.(type) {
		case *purelbv2.Egress, *purelbv2.ClusterEgress, *purelbv2.EgressEndpoints, *purelbv2.EgressMembers, *purelbv2.EgressNodeState,
			*purelbv2.LBNodeAgent:
			purelbObjs = append(purelbObjs, o)
		default:
			kubeObjs = append(kubeObjs, o)
		}
	}
	kube := kubefake.NewSimpleClientset(kubeObjs...)
	if crds {
		kube.Resources = []*metav1.APIResourceList{{
			GroupVersion: purelbv2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{{Name: "egresses"}, {Name: "servicegroups"}},
		}}
	}
	purelb := purelbfake.NewSimpleClientset(purelbObjs...)
	q := &queue{}
	alloc := coreallocator.New(log.NewNopLogger())
	alloc.SetClient(nopEvents{})
	r := New(log.NewNopLogger(), alloc, testNS)
	alloc.SetEgress(r)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	require.NoError(t, r.start(stop, kube, purelb, q.add))
	require.Eventually(t, r.Synced, 5*time.Second, 10*time.Millisecond)
	return r, kube, purelb, q
}

// Without the egress CRDs egress is off: synced at once, so the allocator
// isn't held, and it neither lists nor syncs anything.
func TestStartWithoutCRDs(t *testing.T) {
	r, _, _, q := started(t, false)
	assert.False(t, r.enabled)
	assert.Nil(t, r.Keys())
	assert.NoError(t, r.Sync("egress/ns/e"))
	assert.Empty(t, q.keys)
}

// With them, every existing policy is queued once the informers sync, and
// events map to the policies they affect.
func TestStartQueuesPolicies(t *testing.T) {
	eg := &purelbv2.Egress{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip"}}
	ceg := &purelbv2.ClusterEgress{ObjectMeta: metav1.ObjectMeta{Name: "pci"}}
	r, kube, _, q := started(t, true, eg, ceg)
	assert.True(t, r.enabled)
	require.Eventually(t, func() bool { return q.has("egress/voice/sip") && q.has("clusteregress/pci") }, 5*time.Second, 10*time.Millisecond)
	assert.ElementsMatch(t, []string{"egress/voice/sip", "clusteregress/pci"}, r.Keys())

	// A pod in voice touches voice's Egresses and every ClusterEgress; a
	// pod elsewhere only the ClusterEgresses.
	q.reset()
	ctx := context.Background()
	_, err := kube.CoreV1().Pods("other").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "p"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return q.has("clusteregress/pci") }, 5*time.Second, 10*time.Millisecond)
	assert.False(t, q.has("egress/voice/sip"))
	_, err = kube.CoreV1().Pods("voice").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "p"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return q.has("egress/voice/sip") }, 5*time.Second, 10*time.Millisecond)
}

// A Lease renewal reconciles nothing; a change of a node's subnets
// reconciles every policy.
func TestLeaseEvents(t *testing.T) {
	holder := "n1"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "purelb-node-n1",
			Annotations: map[string]string{purelbv2.SubnetsAnnotation: "10.0.0.0/24"}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	eg := &purelbv2.Egress{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip"}}
	_, kube, _, q := started(t, true, lease, eg)
	require.Eventually(t, func() bool { return q.has("egress/voice/sip") }, 5*time.Second, 10*time.Millisecond)

	ctx := context.Background()
	q.reset()
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.RenewTime = &now
	_, err := kube.CoordinationV1().Leases(testNS).Update(ctx, lease, metav1.UpdateOptions{})
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)
	assert.False(t, q.has("egress/voice/sip"), "a renewal reconciles nothing")

	lease.Annotations[purelbv2.SubnetsAnnotation] = "10.0.0.0/24,10.0.1.0/24"
	_, err = kube.CoordinationV1().Leases(testNS).Update(ctx, lease, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return q.has("egress/voice/sip") }, 5*time.Second, 10*time.Millisecond)
}

func TestParsePolicyKey(t *testing.T) {
	for _, ref := range []egress.PolicyRef{
		{Kind: egress.KindEgress, Namespace: "ns", Name: "e"},
		{Kind: egress.KindClusterEgress, Name: "c"},
	} {
		got, err := parsePolicyKey(ref.String())
		require.NoError(t, err)
		assert.Equal(t, ref, got)
	}
	_, err := parsePolicyKey("bogus")
	assert.Error(t, err)
}
