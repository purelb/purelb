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

package k8s

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"purelb.io/internal/egresshook"
)

func TestAnnouncingOnlyUpdate(t *testing.T) {
	base := func() *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns", Name: "svc",
				Annotations: map[string]string{
					"purelb.io/allocated-from":  "default",
					"purelb.io/announcing-IPv4": "node-a,eth0,10.0.0.1",
				},
			},
			Spec:   corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status: corev1.ServiceStatus{},
		}
	}
	withAnn := func(s *corev1.Service, k, v string) *corev1.Service {
		s.Annotations[k] = v
		return s
	}

	tests := []struct {
		name     string
		old, new *corev1.Service
		wantSkip bool
	}{
		{
			name: "identical (resync) is not skipped",
			old:  base(), new: base(),
			wantSkip: false,
		},
		{
			name:     "announcing-IPv4 changed -> skip",
			old:      base(),
			new:      withAnn(base(), "purelb.io/announcing-IPv4", "node-b,eth0,10.0.0.1"),
			wantSkip: true,
		},
		{
			name: "announcing added where absent -> skip",
			old: func() *corev1.Service {
				s := base()
				delete(s.Annotations, "purelb.io/announcing-IPv4")
				return s
			}(),
			new:      base(),
			wantSkip: true,
		},
		{
			name:     "announcing-IPv6 added alongside existing v4 -> skip",
			old:      base(),
			new:      withAnn(base(), "purelb.io/announcing-IPv6", "node-a,eth0,2001:db8::1"),
			wantSkip: true,
		},
		{
			name:     "non-announcing annotation changed -> enqueue",
			old:      base(),
			new:      withAnn(base(), "purelb.io/allocated-from", "other-pool"),
			wantSkip: false,
		},
		{
			name: "announcing AND allocated-from both changed -> enqueue",
			old:  base(),
			new: withAnn(
				withAnn(base(), "purelb.io/announcing-IPv4", "node-b,eth0,10.0.0.1"),
				"purelb.io/allocated-from", "other-pool"),
			wantSkip: false,
		},
		{
			name: "status changed -> enqueue",
			old:  base(),
			new: func() *corev1.Service {
				s := base()
				s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.0.0.1"}}
				return s
			}(),
			wantSkip: false,
		},
		{
			name: "spec changed -> enqueue",
			old:  base(),
			new: func() *corev1.Service {
				s := base()
				s.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
				return s
			}(),
			wantSkip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantSkip, announcingOnlyUpdate(tt.old, tt.new))
		})
	}
}

// TestSync_DoesNotMutateCachedService verifies that sync hands the callback a
// copy of the Service, not the shared informer cache object. The announcer
// mutates the Service it is given (annotations, ExternalTrafficPolicy); if that
// reached the cache object it would violate client-go's prohibition on mutating
// cache objects and, worse, poison the baseline used to detect changes on an
// Update-conflict retry. The callback returns SyncStateError here so the
// client-coupled write path (maybeUpdateService) is skipped -- cache isolation
// is independent of whether the write happens.
func TestSync_DoesNotMutateCachedService(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	cached := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "ns",
			Name:        "svc",
			Annotations: map[string]string{"purelb.io/announcing-IPv4": "original"},
		},
	}
	require.NoError(t, indexer.Add(cached))

	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)
	defer queue.ShutDown()
	queue.Add(svcKey("ns/svc")) // so Done() has something to mark complete

	var handed *corev1.Service
	c := &Client{
		logger:     log.NewNopLogger(),
		queue:      queue,
		svcIndexer: indexer,
		serviceChanged: func(s *corev1.Service, _ []*discoveryv1.EndpointSlice) SyncState {
			handed = s
			// Mutate the way the announcer does.
			s.Annotations["purelb.io/announcing-IPv4"] = "mutated"
			// Skip maybeUpdateService, which needs a real client.
			return SyncStateError
		},
	}

	c.sync(svcKey("ns/svc"))

	require.NotNil(t, handed, "serviceChanged was not called")
	assert.NotSame(t, cached, handed, "callback was handed the shared cache object, not a copy")

	stored, exists, err := indexer.GetByKey("ns/svc")
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, "original", stored.(*corev1.Service).Annotations["purelb.io/announcing-IPv4"],
		"the cached Service was mutated by the callback")
}

// TestForceSync_UsesImmediateAdd verifies that ForceSync uses Add() instead
// of AddRateLimited(). This is important because ForceSync is called on
// memberlist events and should not be subject to rate limiting delays.
func TestForceSync_UsesImmediateAdd(t *testing.T) {
	// Create a rate limiter with a long base delay so we can detect if
	// AddRateLimited was used (items would be delayed)
	rateLimiter := workqueue.NewTypedItemExponentialFailureRateLimiter[queueItem](
		1*time.Second, // baseDelay - long enough to detect
		10*time.Second,
	)
	queue := workqueue.NewTypedRateLimitingQueue(rateLimiter)

	// Create a mock indexer that returns known keys
	indexer := &mockIndexer{
		keys: []string{"test/svc1", "test/svc2"},
	}

	client := &Client{
		queue:      queue,
		svcIndexer: indexer,
	}

	// Call ForceSync - this should use Add(), not AddRateLimited()
	client.ForceSync()

	// Verify items are immediately available in queue
	// If AddRateLimited was used with 1s delay, Len() would still show items
	// but they wouldn't be retrievable immediately
	assert.Equal(t, 2, queue.Len(), "ForceSync should queue all services immediately")

	// Verify the rate limiter is NOT tracking these items
	// Add() doesn't register with the rate limiter, AddRateLimited() does
	testKey1 := svcKey("test/svc1")
	testKey2 := svcKey("test/svc2")
	assert.Equal(t, 0, rateLimiter.NumRequeues(testKey1),
		"ForceSync should use Add() which doesn't track requeues for svc1")
	assert.Equal(t, 0, rateLimiter.NumRequeues(testKey2),
		"ForceSync should use Add() which doesn't track requeues for svc2")

	// Clean up
	queue.ShutDown()
}

// TestForceSync_QueuesAllServices verifies that ForceSync queues all
// services from the indexer.
func TestForceSync_QueuesAllServices(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)

	// Create a mock indexer that returns known keys
	indexer := &mockIndexer{
		keys: []string{"ns1/svc1", "ns2/svc2", "ns3/svc3"},
	}

	client := &Client{
		queue:      queue,
		svcIndexer: indexer,
	}

	// Call ForceSync
	client.ForceSync()

	// Verify all services were queued
	assert.Equal(t, 3, queue.Len(), "ForceSync should queue all services")

	// Verify the correct keys were queued
	expectedKeys := map[queueItem]bool{
		svcKey("ns1/svc1"): true,
		svcKey("ns2/svc2"): true,
		svcKey("ns3/svc3"): true,
	}

	for i := 0; i < 3; i++ {
		item, shutdown := queue.Get()
		assert.False(t, shutdown)
		assert.True(t, expectedKeys[item], "Unexpected key in queue: %v", item)
		delete(expectedKeys, item)
		queue.Done(item)
	}

	assert.Empty(t, expectedKeys, "Not all expected keys were queued")
	queue.ShutDown()
}

// TestForceSync_NilIndexer verifies that ForceSync handles nil indexer gracefully.
func TestForceSync_NilIndexer(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)

	client := &Client{
		queue:      queue,
		svcIndexer: nil, // nil indexer
	}

	// Should not panic
	client.ForceSync()

	// Queue should be empty
	assert.Equal(t, 0, queue.Len(), "ForceSync with nil indexer should not queue anything")
	queue.ShutDown()
}

// mockIndexer implements cache.Indexer for testing purposes.
// It only implements the methods needed for ForceSync testing.
type mockIndexer struct {
	keys []string
}

func (m *mockIndexer) ListKeys() []string {
	return m.keys
}

// Unused methods required by cache.Indexer interface
func (m *mockIndexer) Add(obj interface{}) error    { return nil }
func (m *mockIndexer) Update(obj interface{}) error { return nil }
func (m *mockIndexer) Delete(obj interface{}) error { return nil }
func (m *mockIndexer) List() []interface{}          { return nil }
func (m *mockIndexer) Get(obj interface{}) (item interface{}, exists bool, err error) {
	return nil, false, nil
}
func (m *mockIndexer) GetByKey(key string) (item interface{}, exists bool, err error) {
	return nil, false, nil
}
func (m *mockIndexer) Replace([]interface{}, string) error { return nil }
func (m *mockIndexer) Resync() error                       { return nil }
func (m *mockIndexer) Index(indexName string, obj interface{}) ([]interface{}, error) {
	return nil, nil
}
func (m *mockIndexer) IndexKeys(indexName, indexedValue string) ([]string, error)    { return nil, nil }
func (m *mockIndexer) ListIndexFuncValues(indexName string) []string                 { return nil }
func (m *mockIndexer) ByIndex(indexName, indexedValue string) ([]interface{}, error) { return nil, nil }
func (m *mockIndexer) GetIndexers() cache.Indexers                                   { return nil }
func (m *mockIndexer) AddIndexers(newIndexers cache.Indexers) error                  { return nil }

// TestEnqueuePoolStatus_NoPublisher verifies that a consumer which owns
// no address pools (lbnodeagent) never puts a dead item on the queue.
func TestEnqueuePoolStatus_NoPublisher(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)
	defer queue.ShutDown()

	c := &Client{queue: queue} // publishPoolStatus left nil
	c.EnqueuePoolStatus()

	assert.Equal(t, 0, queue.Len(), "no publisher wired should mean nothing queued")
}

// TestEnqueuePoolStatus_Collapses verifies the sweep is a singleton: a
// burst of triggers (one per Service deletion, say) must collapse into a
// single pending sweep rather than queueing one item each.
func TestEnqueuePoolStatus_Collapses(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)
	defer queue.ShutDown()

	c := &Client{queue: queue}
	c.SetPoolStatusPublisher(func(context.Context) error { return nil })

	for i := 0; i < 5; i++ {
		c.EnqueuePoolStatus()
	}

	assert.Equal(t, 1, queue.Len(), "repeated triggers should collapse into one sweep")

	item, _ := queue.Get()
	assert.Equal(t, poolStatus{}, item)
	queue.Done(item)
}

// TestSyncPoolStatus_NeverRequeues is the guard on the no-retry
// decision. The failures that persist on this path (a ServiceGroup CRD
// with no status subresource, or a missing servicegroups/status RBAC
// grant) are permanent, so requeuing would spin forever on the same
// goroutine that allocates addresses.
func TestSyncPoolStatus_NeverRequeues(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)
	defer queue.ShutDown()

	calls := 0
	c := &Client{queue: queue, logger: log.NewNopLogger()}
	c.SetPoolStatusPublisher(func(context.Context) error {
		calls++
		return errors.New("servicegroups.purelb.io \"x\" not found")
	})

	assert.Equal(t, SyncStateSuccess, c.sync(poolStatus{}),
		"a failed publish must not be requeued")
	assert.Equal(t, 1, calls)
}

// TestSyncPoolStatus_NilPublisher verifies sync tolerates the item
// arriving with no publisher wired.
func TestSyncPoolStatus_NilPublisher(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)
	defer queue.ShutDown()

	c := &Client{queue: queue, logger: log.NewNopLogger()}
	assert.Equal(t, SyncStateSuccess, c.sync(poolStatus{}))
}

// TestSyncSynced_PublishesPoolStatus verifies that reaching the synced
// milestone schedules a publish, so pools that own no Services still get
// a status when the config arrived before the caches were warm.
func TestSyncSynced_PublishesPoolStatus(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)
	defer queue.ShutDown()

	c := &Client{queue: queue, logger: log.NewNopLogger()}
	c.SetPoolStatusPublisher(func(context.Context) error { return nil })
	c.synced = func() {}

	assert.Equal(t, SyncStateSuccess, c.sync(synced("")))

	require.Equal(t, 1, queue.Len(), "synced should schedule a pool status publish")
	item, _ := queue.Get()
	assert.Equal(t, poolStatus{}, item)
	queue.Done(item)
}

// fakeEgressController records the egress keys it syncs and fails the
// ones listed in fail.
type fakeEgressController struct {
	egresshook.Noop
	keys   []string
	fail   map[string]bool
	synced []string
}

func (f *fakeEgressController) Keys() []string { return f.keys }
func (f *fakeEgressController) Sync(key string) error {
	f.synced = append(f.synced, key)
	if f.fail[key] {
		return errors.New("sync failed")
	}
	return nil
}

// ForceSync reprocesses egress's keys along with the Services, and an
// egress key is synced by the hook, a failure requeuing it.
func TestEgressKeys(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[queueItem](),
	)
	defer queue.ShutDown()
	eg := &fakeEgressController{keys: []string{"e1", "e2"}, fail: map[string]bool{"e2": true}}
	c := &Client{queue: queue, logger: log.NewNopLogger(), egress: eg}

	c.ForceSync()
	assert.Equal(t, 2, queue.Len())
	assert.Equal(t, "egress", queueItemKind(egressKey("e1")))

	assert.Equal(t, SyncStateSuccess, c.sync(egressKey("e1")))
	assert.Equal(t, SyncStateError, c.sync(egressKey("e2")))
	assert.Equal(t, []string{"e1", "e2"}, eg.synced)
}

// The configured QPS and Burst reach the client, and the election's
// clientset has its own limiter at its own rate.
func TestClientRateLimits(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "http://127.0.0.1:1"}}]
users: [{name: u, user: {}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`), 0o600))

	c, err := New(&Config{Logger: log.NewNopLogger(), Kubeconfig: kubeconfig, ProcessName: "test", QPS: 50, Burst: 100})
	require.NoError(t, err)
	main := c.client.CoreV1().RESTClient().GetRateLimiter()
	assert.Equal(t, float32(50), main.QPS())

	ec, err := c.ElectionClientset()
	require.NoError(t, err)
	el := ec.CoordinationV1().RESTClient().GetRateLimiter()
	assert.Equal(t, float32(electionQPS), el.QPS())
	assert.NotSame(t, main, el, "the election must not share the agent's limiter")
	assert.Same(t, main, c.client.CoordinationV1().RESTClient().GetRateLimiter(),
		"without its own clientset the election would share the agent's limiter")
}

// orderEgress records the order the egress hook syncs keys in, failing
// each key in fail once.
type orderEgress struct {
	egresshook.Noop
	order []string
	fail  map[string]bool
}

func (o *orderEgress) Sync(key string) error {
	o.order = append(o.order, key)
	if o.fail[key] {
		delete(o.fail, key)
		return errors.New("transient")
	}
	return nil
}

// A priority key is synced ahead of keys already queued, and one that
// fails comes back on the priority lane after its backoff, with a wake.
func TestPriorityLane(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[queueItem]())
	defer queue.ShutDown()
	eg := &orderEgress{fail: map[string]bool{"claim": true}}
	c := &Client{
		queue: queue, logger: log.NewNopLogger(), egress: eg,
		priority:        workqueue.NewTypedDelayingQueue[string](),
		priorityLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
	}
	defer c.priority.ShutDown()

	c.enqueueEgress("ordinary", false)
	c.enqueueEgress("claim", true)
	assert.Equal(t, 2, queue.Len(), "the ordinary key and one wake")

	// One turn of Run: take the next queued item, drain the lane first.
	key, _ := queue.Get()
	c.drainPriority()
	c.sync(key)
	assert.Equal(t, []string{"claim", "ordinary"}, eg.order, "the priority key ran first")

	// The failed claim returns after its backoff, with a wake for it.
	require.Eventually(t, func() bool { return c.priority.Len() == 1 && queue.Len() == 1 },
		time.Second, 5*time.Millisecond)
	key, _ = queue.Get()
	c.drainPriority()
	assert.Equal(t, SyncStateSuccess, c.sync(key))
	assert.Equal(t, []string{"claim", "ordinary", "claim"}, eg.order)
}
