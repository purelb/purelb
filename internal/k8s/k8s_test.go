// Copyright 2020 Acnodal Inc.
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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

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
func (m *mockIndexer) Add(obj interface{}) error                              { return nil }
func (m *mockIndexer) Update(obj interface{}) error                           { return nil }
func (m *mockIndexer) Delete(obj interface{}) error                           { return nil }
func (m *mockIndexer) List() []interface{}                                    { return nil }
func (m *mockIndexer) Get(obj interface{}) (item interface{}, exists bool, err error) { return nil, false, nil }
func (m *mockIndexer) GetByKey(key string) (item interface{}, exists bool, err error) { return nil, false, nil }
func (m *mockIndexer) Replace([]interface{}, string) error                    { return nil }
func (m *mockIndexer) Resync() error                                          { return nil }
func (m *mockIndexer) Index(indexName string, obj interface{}) ([]interface{}, error) { return nil, nil }
func (m *mockIndexer) IndexKeys(indexName, indexedValue string) ([]string, error)     { return nil, nil }
func (m *mockIndexer) ListIndexFuncValues(indexName string) []string          { return nil }
func (m *mockIndexer) ByIndex(indexName, indexedValue string) ([]interface{}, error)  { return nil, nil }
func (m *mockIndexer) GetIndexers() cache.Indexers                            { return nil }
func (m *mockIndexer) AddIndexers(newIndexers cache.Indexers) error           { return nil }
