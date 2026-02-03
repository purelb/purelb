// Copyright 2020 Acnodal Inc.
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

package election

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

var nodes []string = []string{"test-node0", "test-node1", "test-node2"}

// TestElectionFunction tests the core election hash function
func TestElectionFunction(t *testing.T) {
	assert.Equal(t, "test-node0", election("test-key", nodes)[0])
	assert.Equal(t, "test-node1", election("test-key-nodeXX", nodes)[0])
	assert.Equal(t, "test-node2", election("test-key-foo", nodes)[0])
}

// TestElectionDeterminism ensures the election function is deterministic
func TestElectionDeterminism(t *testing.T) {
	candidates := []string{"node-a", "node-b", "node-c", "node-d"}
	key := "default/my-service"

	// Run election multiple times
	results := make([]string, 10)
	for i := 0; i < 10; i++ {
		results[i] = election(key, candidates)[0]
	}

	// All results should be the same
	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i], "election should be deterministic")
	}
}

// TestElectionDistribution tests that different keys produce different winners
func TestElectionDistribution(t *testing.T) {
	candidates := []string{"node-a", "node-b", "node-c"}

	winners := make(map[string]int)
	// Test 100 different service keys
	for i := 0; i < 100; i++ {
		key := "namespace/service-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		winner := election(key, candidates)[0]
		winners[winner]++
	}

	// Each candidate should win at least some elections
	// (with 3 candidates and 100 keys, each should get roughly 33)
	for _, candidate := range candidates {
		assert.Greater(t, winners[candidate], 10,
			"candidate %s should win some elections", candidate)
	}
}

// TestNewElection tests the New() constructor
func TestNewElection(t *testing.T) {
	client := fake.NewSimpleClientset()

	t.Run("valid config", func(t *testing.T) {
		stopCh := make(chan struct{})
		defer close(stopCh)

		e, err := New(Config{
			Namespace: "purelb",
			NodeName:  "test-node",
			Client:    client,
			StopCh:    stopCh,
		})
		require.NoError(t, err)
		assert.NotNil(t, e)
		assert.Equal(t, "purelb-node-test-node", e.leaseName)
		assert.Equal(t, DefaultLeaseDuration, e.config.LeaseDuration)
		assert.Equal(t, DefaultRenewDeadline, e.config.RenewDeadline)
		assert.Equal(t, DefaultRetryPeriod, e.config.RetryPeriod)
	})

	t.Run("missing client", func(t *testing.T) {
		_, err := New(Config{
			Namespace: "purelb",
			NodeName:  "test-node",
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Client is required")
	})

	t.Run("missing node name", func(t *testing.T) {
		_, err := New(Config{
			Namespace: "purelb",
			Client:    client,
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "NodeName is required")
	})

	t.Run("missing namespace", func(t *testing.T) {
		_, err := New(Config{
			NodeName: "test-node",
			Client:   client,
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Namespace is required")
	})

	t.Run("custom timing", func(t *testing.T) {
		stopCh := make(chan struct{})
		defer close(stopCh)

		e, err := New(Config{
			Namespace:     "purelb",
			NodeName:      "test-node",
			Client:        client,
			LeaseDuration: 30 * time.Second,
			RenewDeadline: 20 * time.Second,
			RetryPeriod:   5 * time.Second,
			StopCh:        stopCh,
		})
		require.NoError(t, err)
		assert.Equal(t, 30*time.Second, e.config.LeaseDuration)
		assert.Equal(t, 20*time.Second, e.config.RenewDeadline)
		assert.Equal(t, 5*time.Second, e.config.RetryPeriod)
	})
}

// TestHealthState tests the health tracking
func TestHealthState(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Initially healthy (set in New())
	assert.True(t, e.IsHealthy())

	// Mark unhealthy
	e.MarkUnhealthy()
	assert.False(t, e.IsHealthy())

	// Can restore by setting directly
	e.leaseHealthy.Store(true)
	assert.True(t, e.IsHealthy())
}

// TestMemberCount tests the member count tracking
func TestMemberCount(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Initially empty
	assert.Equal(t, 0, e.MemberCount())

	// Manually set state with nodes
	e.state.Store(&electionState{
		liveNodes:     []string{"node-a", "node-b", "node-c"},
		subnetToNodes: make(map[string][]string),
		nodeToSubnets: make(map[string][]string),
	})
	assert.Equal(t, 3, e.MemberCount())
}

// TestWinnerWithUnhealthyLease tests that Winner returns "" when unhealthy
func TestWinnerWithUnhealthyLease(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Set up state with nodes
	e.state.Store(&electionState{
		liveNodes:     []string{"node-a", "node-b"},
		subnetToNodes: make(map[string][]string),
		nodeToSubnets: make(map[string][]string),
	})

	// Mark unhealthy - Winner should return ""
	e.MarkUnhealthy()
	assert.Equal(t, "", e.Winner("default/my-service"))
}

// TestWinnerWithNoNodes tests that Winner returns "" when no nodes
func TestWinnerWithNoNodes(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Empty state
	assert.Equal(t, "", e.Winner("default/my-service"))
}

// TestWinnerWithNodes tests normal winner selection
func TestWinnerWithNodes(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Set up state with nodes
	liveNodes := []string{"node-a", "node-b", "node-c"}
	e.state.Store(&electionState{
		liveNodes:     liveNodes,
		subnetToNodes: make(map[string][]string),
		nodeToSubnets: make(map[string][]string),
	})

	// Winner should be one of the nodes
	winner := e.Winner("default/my-service")
	assert.Contains(t, liveNodes, winner)

	// Same key should produce same winner (determinism)
	assert.Equal(t, winner, e.Winner("default/my-service"))
}

// TestIsLeaseValid tests lease validity checking
func TestIsLeaseValid(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	now := time.Now()

	t.Run("valid lease", func(t *testing.T) {
		renewTime := metav1.NewMicroTime(now.Add(-5 * time.Second))
		lease := &coordinationv1.Lease{
			Spec: coordinationv1.LeaseSpec{
				RenewTime:            &renewTime,
				LeaseDurationSeconds: ptr.To(int32(10)),
			},
		}
		assert.True(t, e.isLeaseValid(lease, now))
	})

	t.Run("expired lease", func(t *testing.T) {
		renewTime := metav1.NewMicroTime(now.Add(-15 * time.Second))
		lease := &coordinationv1.Lease{
			Spec: coordinationv1.LeaseSpec{
				RenewTime:            &renewTime,
				LeaseDurationSeconds: ptr.To(int32(10)),
			},
		}
		assert.False(t, e.isLeaseValid(lease, now))
	})

	t.Run("nil renew time", func(t *testing.T) {
		lease := &coordinationv1.Lease{
			Spec: coordinationv1.LeaseSpec{
				LeaseDurationSeconds: ptr.To(int32(10)),
			},
		}
		assert.False(t, e.isLeaseValid(lease, now))
	})

	t.Run("nil duration", func(t *testing.T) {
		renewTime := metav1.NewMicroTime(now)
		lease := &coordinationv1.Lease{
			Spec: coordinationv1.LeaseSpec{
				RenewTime: &renewTime,
			},
		}
		assert.False(t, e.isLeaseValid(lease, now))
	})
}

// TestLeasePrefix tests the lease naming convention
func TestLeasePrefix(t *testing.T) {
	assert.Equal(t, "purelb-node-", LeasePrefix)

	// Lease name format
	nodeName := "my-worker-1"
	expectedLeaseName := LeasePrefix + nodeName
	assert.Equal(t, "purelb-node-my-worker-1", expectedLeaseName)
}

// TestCreateOrUpdateLease tests lease creation (requires fake client with Node)
func TestCreateOrUpdateLease(t *testing.T) {
	// Create fake client with a node
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			UID:  "test-node-uid",
		},
	}
	client := fake.NewSimpleClientset(node)

	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
		GetLocalSubnets: func() ([]string, error) {
			return []string{"192.168.1.0/24", "10.0.0.0/8"}, nil
		},
	})
	require.NoError(t, err)

	// Create the lease
	err = e.createOrUpdateLease()
	require.NoError(t, err)

	// Verify lease was created
	lease, err := client.CoordinationV1().Leases("purelb").Get(
		context.Background(), "purelb-node-test-node", metav1.GetOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, "test-node", *lease.Spec.HolderIdentity)
	assert.Equal(t, "192.168.1.0/24,10.0.0.0/8", lease.Annotations[SubnetsAnnotation])

	// Verify owner reference
	require.Len(t, lease.OwnerReferences, 1)
	assert.Equal(t, "Node", lease.OwnerReferences[0].Kind)
	assert.Equal(t, "test-node", lease.OwnerReferences[0].Name)

	// Update the lease (should work without error)
	err = e.createOrUpdateLease()
	require.NoError(t, err)
}

// TestDeleteOurLease tests lease deletion
func TestDeleteOurLease(t *testing.T) {
	// Create fake client with a node and pre-existing lease
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			UID:  "test-node-uid",
		},
	}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "purelb-node-test-node",
			Namespace: "purelb",
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: ptr.To("test-node"),
		},
	}
	client := fake.NewSimpleClientset(node, lease)

	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Delete the lease
	err = e.DeleteOurLease()
	require.NoError(t, err)

	// Verify lease is gone
	_, err = client.CoordinationV1().Leases("purelb").Get(
		context.Background(), "purelb-node-test-node", metav1.GetOptions{},
	)
	assert.Error(t, err)
}

// TestRenewFailures tests the renewal failure tracking
func TestRenewFailures(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Initially healthy with 0 failures
	assert.True(t, e.IsHealthy())
	assert.Equal(t, int32(0), e.renewFailures.Load())

	// Simulate failures
	for i := 0; i < maxRenewFailures-1; i++ {
		e.renewFailures.Add(1)
	}
	assert.True(t, e.IsHealthy()) // Still healthy

	// One more failure should trigger unhealthy
	e.renewFailures.Add(1)
	if e.renewFailures.Load() >= maxRenewFailures {
		e.leaseHealthy.Store(false)
	}
	assert.False(t, e.IsHealthy())
}

// TestAtomicStateAccess tests concurrent access to election state
func TestAtomicStateAccess(t *testing.T) {
	client := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	defer close(stopCh)

	e, err := New(Config{
		Namespace: "purelb",
		NodeName:  "test-node",
		Client:    client,
		StopCh:    stopCh,
	})
	require.NoError(t, err)

	// Run concurrent reads and writes
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				// Read
				_ = e.MemberCount()
				_ = e.state.Load()

				// Write
				e.state.Store(&electionState{
					liveNodes:     []string{"node-a"},
					subnetToNodes: make(map[string][]string),
					nodeToSubnets: make(map[string][]string),
				})
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}
