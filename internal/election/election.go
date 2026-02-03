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
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"purelb.io/internal/logging"
)

const (
	// LeasePrefix is prepended to node names to form lease names
	LeasePrefix = "purelb-node-"

	// Default timing values (can be overridden via Config)
	DefaultLeaseDuration = 10 * time.Second
	DefaultRenewDeadline = 7 * time.Second
	DefaultRetryPeriod   = 2 * time.Second

	// maxRenewFailures is the number of consecutive renewal failures
	// before marking ourselves unhealthy
	maxRenewFailures = 3
)

// Config provides the configuration data that New() needs.
type Config struct {
	// Namespace is where PureLB leases are created
	Namespace string

	// NodeName is this node's name (used for lease identity)
	NodeName string

	// Client is the Kubernetes client
	Client kubernetes.Interface

	// LeaseDuration is how long a lease is valid
	LeaseDuration time.Duration

	// RenewDeadline is how long to retry renewals before giving up
	RenewDeadline time.Duration

	// RetryPeriod is the interval between renewal attempts
	RetryPeriod time.Duration

	// Logger for structured logging
	Logger log.Logger

	// StopCh signals shutdown
	StopCh <-chan struct{}

	// OnMemberChange is called when membership changes (node join/leave/update)
	// This typically calls client.ForceSync() to re-evaluate all services
	OnMemberChange func()

	// GetLocalSubnets returns this node's local subnets for the lease annotation
	GetLocalSubnets func() ([]string, error)
}

// electionState holds an immutable snapshot of election data.
// Used with atomic.Pointer for lock-free access.
type electionState struct {
	// liveNodes contains node names with valid (non-expired) leases
	liveNodes []string

	// subnetToNodes maps subnet CIDR to nodes that have that subnet
	// e.g., "192.168.1.0/24" -> ["node-a", "node-c"]
	subnetToNodes map[string][]string

	// nodeToSubnets maps node name to its subnets
	// e.g., "node-a" -> ["192.168.1.0/24", "10.0.0.0/24"]
	nodeToSubnets map[string][]string
}

// Election manages leader election for service IP announcements using
// Kubernetes Leases. Each node creates a lease with its subnet annotations,
// and election winners are determined by consistent hashing.
type Election struct {
	config Config

	// leaseName is this node's lease name (LeasePrefix + NodeName)
	leaseName string

	// state holds the current election state (atomic for lock-free access)
	state atomic.Pointer[electionState]

	// leaseInformer watches all PureLB leases in the namespace
	leaseInformer cache.SharedIndexInformer

	// informerFactory manages the informer lifecycle
	informerFactory informers.SharedInformerFactory

	// leaseHealthy tracks whether our own lease is valid
	// When false, Winner() returns "" to force withdrawal of all announcements
	leaseHealthy atomic.Bool

	// renewFailures counts consecutive renewal failures
	renewFailures atomic.Int32

	// renewTicker triggers periodic lease renewal
	renewTicker *time.Ticker

	// ctx and cancel for managing goroutines
	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a new Election instance. Call Start() to begin operation.
func New(cfg Config) (*Election, error) {
	// Apply defaults
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = DefaultLeaseDuration
	}
	if cfg.RenewDeadline == 0 {
		cfg.RenewDeadline = DefaultRenewDeadline
	}
	if cfg.RetryPeriod == 0 {
		cfg.RetryPeriod = DefaultRetryPeriod
	}

	if cfg.Client == nil {
		return nil, fmt.Errorf("Client is required")
	}
	if cfg.NodeName == "" {
		return nil, fmt.Errorf("NodeName is required")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("Namespace is required")
	}

	ctx, cancel := context.WithCancel(context.Background())

	e := &Election{
		config:    cfg,
		leaseName: LeasePrefix + cfg.NodeName,
		ctx:       ctx,
		cancel:    cancel,
	}

	// Initialize with empty state
	e.state.Store(&electionState{
		liveNodes:     []string{},
		subnetToNodes: make(map[string][]string),
		nodeToSubnets: make(map[string][]string),
	})

	// Mark ourselves as healthy initially (will be confirmed when lease is created)
	e.leaseHealthy.Store(true)

	return e, nil
}

// Start begins the election process:
// 1. Creates our lease
// 2. Starts the lease informer
// 3. Starts the renewal goroutine
func (e *Election) Start() error {
	logging.Info(e.config.Logger, "op", "election", "action", "starting",
		"node", e.config.NodeName, "namespace", e.config.Namespace,
		"leaseDuration", e.config.LeaseDuration.String())

	// Create our lease first
	if err := e.createOrUpdateLease(); err != nil {
		return fmt.Errorf("failed to create lease: %w", err)
	}

	// Set up informer factory with label selector for PureLB leases
	e.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		e.config.Client,
		0, // No resync period - we rely on watch events
		informers.WithNamespace(e.config.Namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			// Only watch leases that start with our prefix
			opts.FieldSelector = ""
		}),
	)

	// Get the lease informer
	e.leaseInformer = e.informerFactory.Coordination().V1().Leases().Informer()

	// Add event handlers
	_, err := e.leaseInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    e.onLeaseAdd,
		UpdateFunc: e.onLeaseUpdate,
		DeleteFunc: e.onLeaseDelete,
	})
	if err != nil {
		return fmt.Errorf("failed to add event handlers: %w", err)
	}

	// Start the informer
	e.informerFactory.Start(e.config.StopCh)

	// Wait for cache sync
	logging.Debug(e.config.Logger, "op", "election", "action", "waiting for cache sync")
	if !cache.WaitForCacheSync(e.config.StopCh, e.leaseInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for lease informer cache sync")
	}
	logging.Info(e.config.Logger, "op", "election", "action", "cache synced",
		"msg", "lease informer ready")

	// Build initial state
	e.rebuildMaps()

	// Start renewal goroutine
	e.renewTicker = time.NewTicker(e.config.LeaseDuration / 2)
	go e.renewLoop()

	// Watch for shutdown
	go func() {
		<-e.config.StopCh
		e.Shutdown()
	}()

	return nil
}

// Shutdown gracefully stops the election process
func (e *Election) Shutdown() {
	logging.Info(e.config.Logger, "op", "election", "action", "shutdown", "msg", "starting graceful shutdown")

	e.cancel()

	if e.renewTicker != nil {
		e.renewTicker.Stop()
	}

	// Mark ourselves unhealthy so Winner() returns ""
	e.MarkUnhealthy()

	// Delete our lease so other nodes see us gone immediately
	if err := e.DeleteOurLease(); err != nil {
		logging.Info(e.config.Logger, "op", "election", "action", "delete lease",
			"error", err, "msg", "failed to delete lease during shutdown")
	}

	logging.Info(e.config.Logger, "op", "election", "action", "shutdown", "msg", "complete")
}

// MarkUnhealthy marks this node as unhealthy, causing Winner() to return ""
// for all queries. Use this during graceful shutdown.
func (e *Election) MarkUnhealthy() {
	e.leaseHealthy.Store(false)
	logging.Info(e.config.Logger, "op", "election", "action", "markUnhealthy",
		"msg", "node marked unhealthy, withdrawing from elections")
}

// IsHealthy returns whether this node's lease is currently valid
func (e *Election) IsHealthy() bool {
	return e.leaseHealthy.Load()
}

// MemberCount returns the number of nodes with valid leases
func (e *Election) MemberCount() int {
	state := e.state.Load()
	return len(state.liveNodes)
}

// Winner returns the node name that should announce the given IP address.
// Returns "" if:
// - Our lease is unhealthy (prevents split-brain)
// - No nodes are available
// - The informer hasn't synced yet
//
// For Milestone 2, this does NOT filter by subnet - all live nodes are candidates.
// Milestone 3 will add subnet filtering.
func (e *Election) Winner(key string) string {
	// Self-health check: if our lease is unhealthy, we cannot participate
	// This prevents split-brain during API server partitions
	if !e.leaseHealthy.Load() {
		logging.Debug(e.config.Logger, "op", "election", "action", "winner",
			"key", key, "result", "", "reason", "lease unhealthy")
		return ""
	}

	// Check if informer has synced
	if e.leaseInformer != nil && !e.leaseInformer.HasSynced() {
		logging.Debug(e.config.Logger, "op", "election", "action", "winner",
			"key", key, "result", "", "reason", "informer not synced")
		return ""
	}

	state := e.state.Load()
	if state == nil || len(state.liveNodes) == 0 {
		logging.Debug(e.config.Logger, "op", "election", "action", "winner",
			"key", key, "result", "", "reason", "no live nodes")
		return ""
	}

	// For Milestone 2: use all live nodes as candidates
	// Milestone 3 will add subnet filtering here
	candidates := make([]string, len(state.liveNodes))
	copy(candidates, state.liveNodes)

	winner := election(key, candidates)[0]

	logging.Debug(e.config.Logger, "op", "election", "action", "winner",
		"key", key, "candidates", len(candidates), "winner", winner)

	return winner
}

// DeleteOurLease removes this node's lease from the cluster
func (e *Election) DeleteOurLease() error {
	err := e.config.Client.CoordinationV1().Leases(e.config.Namespace).Delete(
		e.ctx,
		e.leaseName,
		metav1.DeleteOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to delete lease %s: %w", e.leaseName, err)
	}
	logging.Info(e.config.Logger, "op", "election", "action", "deleteLease",
		"lease", e.leaseName, "msg", "lease deleted")
	return nil
}

// createOrUpdateLease creates our lease or updates it if it exists
func (e *Election) createOrUpdateLease() error {
	ctx := e.ctx

	// Get subnets for annotation
	var subnetsAnnotation string
	if e.config.GetLocalSubnets != nil {
		subnets, err := e.config.GetLocalSubnets()
		if err != nil {
			logging.Info(e.config.Logger, "op", "election", "action", "getSubnets",
				"error", err, "msg", "failed to get local subnets, continuing without")
		} else {
			subnetsAnnotation = FormatSubnetsAnnotation(subnets)
		}
	}

	// Get the Node object for OwnerReference (enables garbage collection)
	node, err := e.config.Client.CoreV1().Nodes().Get(ctx, e.config.NodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node for owner reference: %w", err)
	}

	now := metav1.NewMicroTime(time.Now())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.leaseName,
			Namespace: e.config.Namespace,
			Annotations: map[string]string{
				SubnetsAnnotation: subnetsAnnotation,
			},
			Labels: map[string]string{
				"app.kubernetes.io/component": "lbnodeagent",
				"app.kubernetes.io/name":      "purelb",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Node",
					Name:               node.Name,
					UID:                node.UID,
					BlockOwnerDeletion: ptr.To(false),
					Controller:         ptr.To(false),
				},
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &e.config.NodeName,
			LeaseDurationSeconds: ptr.To(int32(e.config.LeaseDuration.Seconds())),
			RenewTime:            &now,
		},
	}

	// Try to create the lease
	_, err = e.config.Client.CoordinationV1().Leases(e.config.Namespace).Create(
		ctx, lease, metav1.CreateOptions{},
	)
	if err == nil {
		logging.Info(e.config.Logger, "op", "election", "action", "createLease",
			"lease", e.leaseName, "subnets", subnetsAnnotation, "msg", "lease created")
		e.leaseHealthy.Store(true)
		e.renewFailures.Store(0)
		return nil
	}

	// Lease might already exist, try to update it
	existing, getErr := e.config.Client.CoordinationV1().Leases(e.config.Namespace).Get(
		ctx, e.leaseName, metav1.GetOptions{},
	)
	if getErr != nil {
		return fmt.Errorf("failed to create or get lease: create=%w, get=%v", err, getErr)
	}

	// Update the existing lease
	existing.Spec.RenewTime = &now
	existing.Annotations = lease.Annotations
	existing.OwnerReferences = lease.OwnerReferences

	_, err = e.config.Client.CoordinationV1().Leases(e.config.Namespace).Update(
		ctx, existing, metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update existing lease: %w", err)
	}

	logging.Info(e.config.Logger, "op", "election", "action", "updateLease",
		"lease", e.leaseName, "subnets", subnetsAnnotation, "msg", "lease updated")
	e.leaseHealthy.Store(true)
	e.renewFailures.Store(0)
	return nil
}

// renewLease renews our lease to indicate we're still alive
func (e *Election) renewLease() error {
	ctx := e.ctx

	// Get current lease
	lease, err := e.config.Client.CoordinationV1().Leases(e.config.Namespace).Get(
		ctx, e.leaseName, metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get lease for renewal: %w", err)
	}

	// Update renew time
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.RenewTime = &now

	// Optionally update subnets if they changed
	if e.config.GetLocalSubnets != nil {
		subnets, err := e.config.GetLocalSubnets()
		if err == nil {
			newAnnotation := FormatSubnetsAnnotation(subnets)
			if lease.Annotations == nil {
				lease.Annotations = make(map[string]string)
			}
			if lease.Annotations[SubnetsAnnotation] != newAnnotation {
				lease.Annotations[SubnetsAnnotation] = newAnnotation
				logging.Info(e.config.Logger, "op", "election", "action", "subnetsChanged",
					"lease", e.leaseName, "subnets", newAnnotation)
			}
		}
	}

	_, err = e.config.Client.CoordinationV1().Leases(e.config.Namespace).Update(
		ctx, lease, metav1.UpdateOptions{},
	)
	if err != nil {
		failures := e.renewFailures.Add(1)
		if failures >= maxRenewFailures {
			e.leaseHealthy.Store(false)
			logging.Info(e.config.Logger, "op", "election", "action", "renewFailed",
				"lease", e.leaseName, "failures", failures,
				"msg", "marking unhealthy after repeated failures")
		}
		return fmt.Errorf("failed to renew lease: %w", err)
	}

	// Success - reset failures and mark healthy
	if e.renewFailures.Load() > 0 {
		logging.Info(e.config.Logger, "op", "election", "action", "renewRecovered",
			"lease", e.leaseName, "msg", "lease renewal recovered")
	}
	e.renewFailures.Store(0)
	if !e.leaseHealthy.Load() {
		e.leaseHealthy.Store(true)
		logging.Info(e.config.Logger, "op", "election", "action", "healthRestored",
			"lease", e.leaseName, "msg", "re-enabling elections after recovery")
		if e.config.OnMemberChange != nil {
			e.config.OnMemberChange()
		}
	}

	logging.Debug(e.config.Logger, "op", "election", "action", "renewLease",
		"lease", e.leaseName, "msg", "lease renewed")
	return nil
}

// renewLoop periodically renews our lease
func (e *Election) renewLoop() {
	for {
		select {
		case <-e.renewTicker.C:
			if err := e.renewLease(); err != nil {
				logging.Info(e.config.Logger, "op", "election", "action", "renewLoop",
					"error", err, "msg", "lease renewal failed")
			}
		case <-e.ctx.Done():
			return
		}
	}
}

// rebuildMaps rebuilds the election state from the lease informer cache.
// Uses copy-on-write pattern: builds new state, then atomically swaps.
func (e *Election) rebuildMaps() {
	newState := &electionState{
		liveNodes:     make([]string, 0),
		subnetToNodes: make(map[string][]string),
		nodeToSubnets: make(map[string][]string),
	}

	now := time.Now()

	// Iterate through all leases in the cache
	for _, obj := range e.leaseInformer.GetStore().List() {
		lease, ok := obj.(*coordinationv1.Lease)
		if !ok {
			continue
		}

		// Only process PureLB leases
		if len(lease.Name) <= len(LeasePrefix) || lease.Name[:len(LeasePrefix)] != LeasePrefix {
			continue
		}

		// Check if lease is still valid (not expired)
		if !e.isLeaseValid(lease, now) {
			logging.Debug(e.config.Logger, "op", "election", "action", "rebuildMaps",
				"lease", lease.Name, "msg", "skipping expired lease")
			continue
		}

		// Extract node name from lease
		nodeName := lease.Name[len(LeasePrefix):]
		newState.liveNodes = append(newState.liveNodes, nodeName)

		// Parse subnet annotation
		if lease.Annotations != nil {
			subnetsStr := lease.Annotations[SubnetsAnnotation]
			subnets := ParseSubnetsAnnotation(subnetsStr)
			newState.nodeToSubnets[nodeName] = subnets

			for _, subnet := range subnets {
				newState.subnetToNodes[subnet] = append(newState.subnetToNodes[subnet], nodeName)
			}
		}
	}

	// Sort for determinism
	sort.Strings(newState.liveNodes)

	// Atomic swap
	e.state.Store(newState)

	logging.Info(e.config.Logger, "op", "election", "action", "rebuildMaps",
		"liveNodes", len(newState.liveNodes), "subnets", len(newState.subnetToNodes),
		"msg", "election state rebuilt")
}

// isLeaseValid checks if a lease is still valid (not expired)
func (e *Election) isLeaseValid(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return false
	}

	renewTime := lease.Spec.RenewTime.Time
	duration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	expiry := renewTime.Add(duration)

	return now.Before(expiry)
}

// Event handlers for lease informer

func (e *Election) onLeaseAdd(obj interface{}) {
	lease, ok := obj.(*coordinationv1.Lease)
	if !ok {
		return
	}

	// Only process PureLB leases
	if len(lease.Name) <= len(LeasePrefix) || lease.Name[:len(LeasePrefix)] != LeasePrefix {
		return
	}

	nodeName := lease.Name[len(LeasePrefix):]
	logging.Info(e.config.Logger, "op", "election", "event", "leaseAdd",
		"node", nodeName, "msg", "node joined cluster")

	e.rebuildMaps()
	if e.config.OnMemberChange != nil {
		e.config.OnMemberChange()
	}
}

func (e *Election) onLeaseUpdate(oldObj, newObj interface{}) {
	oldLease, ok1 := oldObj.(*coordinationv1.Lease)
	newLease, ok2 := newObj.(*coordinationv1.Lease)
	if !ok1 || !ok2 {
		return
	}

	// Only process PureLB leases
	if len(newLease.Name) <= len(LeasePrefix) || newLease.Name[:len(LeasePrefix)] != LeasePrefix {
		return
	}

	// Check if subnets changed
	oldSubnets := ""
	newSubnets := ""
	if oldLease.Annotations != nil {
		oldSubnets = oldLease.Annotations[SubnetsAnnotation]
	}
	if newLease.Annotations != nil {
		newSubnets = newLease.Annotations[SubnetsAnnotation]
	}

	if oldSubnets != newSubnets {
		nodeName := newLease.Name[len(LeasePrefix):]
		logging.Info(e.config.Logger, "op", "election", "event", "subnetsChanged",
			"node", nodeName, "oldSubnets", oldSubnets, "newSubnets", newSubnets)

		e.rebuildMaps()
		if e.config.OnMemberChange != nil {
			e.config.OnMemberChange()
		}
	}
}

func (e *Election) onLeaseDelete(obj interface{}) {
	lease, ok := obj.(*coordinationv1.Lease)
	if !ok {
		// Handle DeletedFinalStateUnknown
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		lease, ok = tombstone.Obj.(*coordinationv1.Lease)
		if !ok {
			return
		}
	}

	// Only process PureLB leases
	if len(lease.Name) <= len(LeasePrefix) || lease.Name[:len(LeasePrefix)] != LeasePrefix {
		return
	}

	nodeName := lease.Name[len(LeasePrefix):]
	logging.Info(e.config.Logger, "op", "election", "event", "leaseDelete",
		"node", nodeName, "msg", "node left cluster")

	e.rebuildMaps()
	if e.config.OnMemberChange != nil {
		e.config.OnMemberChange()
	}
}

// election conducts an election among the candidates based on the
// provided key. The order of the candidates in the return array is
// the result of the election.
func election(key string, candidates []string) []string {
	// Sort the slice by the hash of candidate name + service key. This
	// produces an ordering of ready candidates that is unique to this
	// service.
	sort.Slice(candidates, func(i, j int) bool {
		hi := sha256.Sum256([]byte(candidates[i] + "#" + key))
		hj := sha256.Sum256([]byte(candidates[j] + "#" + key))

		return bytes.Compare(hi[:], hj[:]) < 0
	})

	return candidates
}
