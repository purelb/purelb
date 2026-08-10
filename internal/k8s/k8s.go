// Copyright 2017 Google Inc.
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

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
	"purelb.io/pkg/generated/informers/externalversions"

	"purelb.io/internal/election"

	"github.com/go-kit/log"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
)

// serviceNameIndexName is the name of the custom indexer that maps
// EndpointSlices to their parent Service name.
const serviceNameIndexName = "serviceName"

// svcResyncPeriod is the informer resync interval for Services. It bounds how
// long a dropped write can leave the cluster out of sync; see the comment at
// the Service informer for why this is a correctness floor rather than a
// polling loop.
const svcResyncPeriod = 10 * time.Minute

// Client watches a Kubernetes cluster and translates events into
// Controller method calls.
type Client struct {
	logger log.Logger

	client   *kubernetes.Clientset
	crClient versioned.Interface
	events   record.EventRecorder
	queue    workqueue.TypedRateLimitingInterface[queueItem]

	// shuttingDown is set to true when stopCh closes, signaling that
	// API calls should use shorter timeouts to allow graceful shutdown.
	shuttingDown atomic.Bool

	svcIndexer      cache.Indexer
	svcInformer     cache.Controller
	epSliceIndexer  cache.Indexer
	epSliceInformer cache.Controller

	crInformerFactory externalversions.SharedInformerFactory
	crController      Controller

	syncFuncs []cache.InformerSynced

	serviceChanged func(*corev1.Service, []*discoveryv1.EndpointSlice) SyncState
	serviceDeleted func(string) SyncState
	configChanged  func(*purelbv2.Config) SyncState
	synced         func()
	shutdown       func()

	// publishPoolStatus republishes every address pool's ServiceGroup
	// .status. Set by SetPoolStatusPublisher; nil in consumers that own
	// no pools (lbnodeagent), which makes EnqueuePoolStatus a no-op.
	publishPoolStatus func(context.Context) error
}

// SetPoolStatusPublisher wires the callback invoked when a poolStatus
// work item is processed. Only the allocator sets this.
func (c *Client) SetPoolStatusPublisher(fn func(context.Context) error) {
	c.publishPoolStatus = fn
}

// EnqueuePoolStatus schedules a republish of every pool's ServiceGroup
// .status on the work queue.
//
// The publish has to happen on the queue goroutine: pools are swapped
// in from the CR controller goroutine, while status writing belongs to
// the same goroutine that allocates, so that the two cannot interleave
// on a pool's counters. Repeated calls collapse into a single pending
// sweep.
func (c *Client) EnqueuePoolStatus() {
	if c.publishPoolStatus == nil {
		return
	}
	c.queue.Add(poolStatus{})
}

// ServiceEvent adds events to services.
type ServiceEvent interface {
	Infof(obj runtime.Object, desc, msg string, args ...interface{})
	Errorf(obj runtime.Object, desc, msg string, args ...interface{})
	ForceSync()
}

// ServiceGroupStatusWriter is the surface the allocator needs from
// *Client to write ServiceGroup .status: the write itself, a
// timeout-bounded cancellable context, and a way to classify shutdown
// errors as expected (silent). Kept narrow and separate from
// ServiceEvent — different semantic concern (CRD status writes vs
// k8s Event emission). *Client satisfies both interfaces.
//
// APIContext lives here, not just as a renamed public method on
// *Client, because the allocator only holds the narrow ServiceEvent
// interface today and has no path to *Client.apiContext(). Putting
// APIContext on the new interface lets the allocator construct
// cancellable contexts without needing a *Client reference.
type ServiceGroupStatusWriter interface {
	// UpdateServiceGroupStatus writes sg.Status via the status
	// subresource. The caller supplies ctx so that a bulk publish can
	// bound the cost of the whole sweep with one deadline rather than
	// paying a fresh timeout per ServiceGroup.
	UpdateServiceGroupStatus(ctx context.Context, sg *purelbv2.ServiceGroup) error
	APIContext() (context.Context, context.CancelFunc)
	IsShutdownError(err error) bool
}

// APIContext is the public wrapper over the unexported apiContext.
// Returns a context.Context with the standard PureLB API timeout
// (10s normally, 500ms during shutdown). Callers MUST invoke the
// returned CancelFunc.
func (c *Client) APIContext() (context.Context, context.CancelFunc) {
	return c.apiContext()
}

// IsShutdownError is the public wrapper over isShutdownError.
// Returns true when err is a context.DeadlineExceeded or
// context.Canceled originating from the shutdown-mode short timeout.
func (c *Client) IsShutdownError(err error) bool {
	return c.isShutdownError(err)
}

// UpdateServiceGroupStatus writes sg's .status subresource using a
// server-side apply patch.
//
// Apply rather than UpdateStatus deliberately. UpdateStatus is a
// read-modify-write keyed on ResourceVersion, which forced the
// allocator to cache an RV per ServiceGroup and refresh it after every
// write. That cache is only rebuilt when a ServiceGroup's *generation*
// changes (see sgUpdateNeedsReconcile), so any RV bump that leaves the
// spec alone — a label, a GitOps tracking annotation, a finalizer, or a
// second allocator during a rolling upgrade — stranded the cached RV
// and made every subsequent status write conflict permanently. PureLB
// is the sole writer of ServiceGroup .status, so the optimistic
// concurrency check bought nothing and cost that failure mode.
//
// Apply also gives correct removal semantics: fields PureLB owns but
// omits (availableIPv4/6 when capacity is unknowable) are deleted
// rather than left stale.
func (c *Client) UpdateServiceGroupStatus(ctx context.Context, sg *purelbv2.ServiceGroup) error {
	// Minimal apply configuration: identity plus the fields we own. No
	// ResourceVersion — apply is not conditional on one.
	body, err := json.Marshal(map[string]interface{}{
		"apiVersion": purelbv2.SchemeGroupVersion.String(),
		"kind":       "ServiceGroup",
		"metadata": map[string]interface{}{
			"name":      sg.Name,
			"namespace": sg.Namespace,
		},
		"status": sg.Status,
	})
	if err != nil {
		return err
	}

	_, err = c.crClient.PurelbV2().ServiceGroups(sg.Namespace).Patch(ctx, sg.Name,
		types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: "purelb-allocator", Force: ptr.To(true)},
		"status")
	// The error is returned even during shutdown: the write did not
	// happen, so the caller must not cache the status as persisted.
	// The caller suppresses the log for shutdown errors via
	// IsShutdownError.
	return err
}

// SyncState is the result of calling synchronization callbacks.
type SyncState int

const (
	// SyncStateSuccess indicates that the update succeeded.
	SyncStateSuccess SyncState = iota
	// SyncStateError indicates that the update caused a transient error
	// and the k8s client should retry later.
	SyncStateError
	// SyncStateReprocessAll indicates that the update succeeded but
	// requires reprocessing all watched services.
	SyncStateReprocessAll
)

// saNamespacePath is where the ServiceAccount token projection puts the
// namespace a Pod is running in.
const saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// InstallNamespace determines the namespace PureLB is installed in. An
// explicit flag or PURELB_NAMESPACE wins; otherwise it falls back to the
// ServiceAccount projection, which keeps deployments working whose manifests
// predate the downward-API env var.
//
// An empty result is deliberately not fatal. Both binaries derive config from
// this value, and refusing to start on a missing namespace would turn a
// cosmetic packaging gap into an outage; callers degrade instead. Whitespace
// is trimmed because the projected file has no trailing newline guarantee.
func InstallNamespace(logger log.Logger, flagValue string) string {
	if ns := strings.TrimSpace(flagValue); ns != "" {
		return ns
	}
	data, err := os.ReadFile(saNamespacePath)
	if err != nil {
		logger.Log("op", "startup", "error", err,
			"msg", "could not determine install namespace; set --namespace or PURELB_NAMESPACE")
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Config specifies the configuration of the Kubernetes
// client/watcher.
type Config struct {
	// Namespace is the namespace PureLB is installed in, as resolved by
	// InstallNamespace. May be empty if it could not be determined.
	Namespace          string
	ProcessName        string
	NodeName           string
	ReadEndpointSlices bool
	Logger             log.Logger
	Kubeconfig         string

	ServiceChanged func(*corev1.Service, []*discoveryv1.EndpointSlice) SyncState
	ServiceDeleted func(string) SyncState
	ConfigChanged  func(*purelbv2.Config) SyncState
	Synced         func()
	Shutdown       func()
}

// queueItem is a union type for items that can be added to the work queue.
// Using an interface with a marker method provides compile-time type safety
// with the TypedRateLimitingQueue.
type queueItem interface {
	isQueueItem()
}

type svcKey string

func (svcKey) isQueueItem() {}

type synced string

func (synced) isQueueItem() {}

// poolStatus asks the consumer to republish .status for every
// configured address pool. A singleton: all instances compare equal, so
// the work queue collapses a burst of triggers into one sweep.
type poolStatus struct{}

func (poolStatus) isQueueItem() {}

// New connects to masterAddr, using kubeconfig to authenticate.
//
// The client uses processName to identify itself to the cluster
// (e.g. when logging events).
func New(cfg *Config) (*Client, error) {
	var (
		k8sConfig *rest.Config
		err       error
	)

	k8sConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building client config: %s", err)
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %s", err)
	}
	crClient, err := versioned.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("creating custom resource client: %s", err)
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: typedcorev1.New(clientset.CoreV1().RESTClient()).Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: cfg.ProcessName})

	queue := workqueue.NewTypedRateLimitingQueue[queueItem](workqueue.DefaultTypedControllerRateLimiter[queueItem]())

	c := &Client{
		logger:   cfg.Logger,
		client:   clientset,
		crClient: crClient,
		events:   recorder,
		queue:    queue,
	}

	// Custom Resource Watcher

	// The 10-minute resync replays the cached CRs through the update
	// handlers (no apiserver load). ServiceGroup replays are filtered by
	// the generation check, but LBNodeAgent replays re-deliver config:
	// that bounds how long a node-label-only change can go unnoticed by
	// nodeSelector evaluation and gives config delivery a self-healing
	// floor if a delivery was lost.
	c.crInformerFactory = externalversions.NewSharedInformerFactory(crClient, time.Minute*10)
	c.crController = *NewCRController(c.logger, cfg.Namespace, cfg.ConfigChanged, c.ForceSync, c.EnqueuePoolStatus, clientset, crClient, c.crInformerFactory)

	// Service Watcher

	svcHandlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(svcKey(key))
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			// A node writing a purelb.io/announcing-* annotation generates a
			// Service update that every node's informer receives. Nothing in
			// the reconcile path reads that annotation -- it is display-only --
			// so re-enqueuing on it wakes every node for another node's write,
			// an O(N^2) storm that is at its worst during the VIP migration the
			// annotation exists to record. Skip the re-enqueue when a peer's
			// announcing write is the only thing that changed.
			oldSvc, oldOK := old.(*corev1.Service)
			newSvc, newOK := new.(*corev1.Service)
			if oldOK && newOK && announcingOnlyUpdate(oldSvc, newSvc) {
				return
			}
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				c.queue.Add(svcKey(key))
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(svcKey(key))
			}
		},
	}
	svcWatcher := cache.NewListWatchFromClient(c.client.CoreV1().RESTClient(), "services", corev1.NamespaceAll, fields.Everything())
	// A non-zero resync period re-delivers every Service periodically. It is a
	// self-healing floor: if a write is ever dropped (e.g. an Update conflict
	// whose retry is lost) the next resync reconciles it. In steady state the
	// recomputed object is identical, so maybeUpdateService's diff short-
	// circuits and resync produces no API writes.
	c.svcIndexer, c.svcInformer = cache.NewIndexerInformer(svcWatcher, &corev1.Service{}, svcResyncPeriod, svcHandlers, cache.Indexers{})

	c.serviceChanged = cfg.ServiceChanged
	c.serviceDeleted = cfg.ServiceDeleted
	c.syncFuncs = append(c.syncFuncs, c.svcInformer.HasSynced)

	// EndpointSlice Watcher (used by node agents, not the allocator)

	if cfg.ReadEndpointSlices {
		// Custom indexer function to map EndpointSlices to their parent Service.
		// This allows us to efficiently fetch all slices for a given service.
		serviceNameIndexFunc := func(obj interface{}) ([]string, error) {
			slice, ok := obj.(*discoveryv1.EndpointSlice)
			if !ok {
				return nil, nil
			}
			serviceName, ok := slice.Labels[discoveryv1.LabelServiceName]
			if !ok {
				return nil, nil
			}
			return []string{slice.Namespace + "/" + serviceName}, nil
		}

		epSliceHandlers := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				slice, ok := obj.(*discoveryv1.EndpointSlice)
				if !ok {
					return
				}
				// Extract the service name from the label, not the slice name
				serviceName, ok := slice.Labels[discoveryv1.LabelServiceName]
				if !ok {
					return
				}
				c.queue.Add(svcKey(slice.Namespace + "/" + serviceName))
			},
			UpdateFunc: func(old interface{}, new interface{}) {
				slice, ok := new.(*discoveryv1.EndpointSlice)
				if !ok {
					return
				}
				serviceName, ok := slice.Labels[discoveryv1.LabelServiceName]
				if !ok {
					return
				}
				c.queue.Add(svcKey(slice.Namespace + "/" + serviceName))
			},
			DeleteFunc: func(obj interface{}) {
				slice, ok := obj.(*discoveryv1.EndpointSlice)
				if !ok {
					// Handle DeletedFinalStateUnknown
					tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
					if !ok {
						return
					}
					slice, ok = tombstone.Obj.(*discoveryv1.EndpointSlice)
					if !ok {
						return
					}
				}
				serviceName, ok := slice.Labels[discoveryv1.LabelServiceName]
				if !ok {
					return
				}
				c.queue.Add(svcKey(slice.Namespace + "/" + serviceName))
			},
		}
		epSliceWatcher := cache.NewListWatchFromClient(c.client.DiscoveryV1().RESTClient(), "endpointslices", corev1.NamespaceAll, fields.Everything())
		c.epSliceIndexer, c.epSliceInformer = cache.NewIndexerInformer(epSliceWatcher, &discoveryv1.EndpointSlice{}, 0, epSliceHandlers, cache.Indexers{
			serviceNameIndexName: serviceNameIndexFunc,
		})

		c.syncFuncs = append(c.syncFuncs, c.epSliceInformer.HasSynced)
	}

	// Sync Watcher

	c.synced = cfg.Synced

	// Shutdown hook

	c.shutdown = cfg.Shutdown

	return c, nil
}

// GetPods get the pods in the namespace matched by the labels string.
func (c *Client) getPods(namespace string, labels string) (*corev1.PodList, error) {
	ctx, cancel := c.apiContext()
	defer cancel()
	pl, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels})
	if err != nil {
		return nil, err
	}
	return pl, nil
}

// GetPodsIPs get the IPs from the pods in the namespace matched by
// the labels string.
func (c *Client) GetPodsIPs(namespace string, labels string) ([]string, error) {
	pl, err := c.getPods(namespace, labels)
	if err != nil {
		return nil, err
	}
	iplist := []string{}
	for _, pod := range pl.Items {
		iplist = append(iplist, pod.Status.PodIP)
	}
	return iplist, nil
}

// apiContext returns a context with an appropriate timeout for API calls.
// During normal operation, a 10-second timeout is used. During shutdown,
// a much shorter 500ms timeout is used to ensure graceful termination.
func (c *Client) apiContext() (context.Context, context.CancelFunc) {
	if c.shuttingDown.Load() {
		return context.WithTimeout(context.Background(), 500*time.Millisecond)
	}
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// isShutdownError returns true if the error is due to context cancellation
// or timeout during shutdown, which should be treated as non-fatal.
func (c *Client) isShutdownError(err error) bool {
	if err == nil {
		return false
	}
	return c.shuttingDown.Load() && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
}

// Run watches for events on the Kubernetes cluster, and dispatches
// calls to the Controller.
func (c *Client) Run(stopCh <-chan struct{}) error {
	c.crInformerFactory.Start(stopCh)
	go func() {
		if err := c.crController.Run(1, stopCh); err != nil {
			c.logger.Log("CR controller init error", err)
		}
	}()

	if c.svcInformer != nil {
		go c.svcInformer.Run(stopCh)
	}
	if c.epSliceInformer != nil {
		go c.epSliceInformer.Run(stopCh)
	}

	if !cache.WaitForCacheSync(stopCh, c.syncFuncs...) {
		return errors.New("timed out waiting for cache sync")
	}

	c.queue.Add(synced(""))

	if stopCh != nil {
		go func() {
			<-stopCh
			c.shuttingDown.Store(true) // Signal API calls to use short timeouts
			c.queue.ShutDown()
		}()
	}

	for {
		key, quit := c.queue.Get()
		if quit {
			if c.shutdown != nil {
				c.shutdown()
			}
			return nil
		}
		updates.Inc()
		st := c.sync(key)
		// c.logger.Log("sync", key, "result", st)
		switch st {
		case SyncStateSuccess:
			c.queue.Forget(key)
		case SyncStateError:
			updateErrors.Inc()
			c.queue.AddRateLimited(key)
		case SyncStateReprocessAll:
			c.queue.Forget(key)
			c.ForceSync()
		}
	}
}

// ForceSync reprocesses all watched services. This is called when
// election membership changes and we need to re-evaluate all services
// immediately. We use Add() instead of AddRateLimited() because this
// is not an error retry - it's a legitimate need to reprocess due to
// cluster state changes.
func (c *Client) ForceSync() {
	if c.svcIndexer != nil {
		for _, k := range c.svcIndexer.ListKeys() {
			c.queue.Add(svcKey(k))
		}
	}
}

// Clientset returns the underlying Kubernetes clientset.
// This is used by components that need direct API access, such as the election.
func (c *Client) Clientset() kubernetes.Interface {
	return c.client
}

// ListServices returns all services from the informer cache. This is
// used by the allocator to populate pool state from existing allocations.
func (c *Client) ListServices() []corev1.Service {
	var result []corev1.Service
	if c.svcIndexer == nil {
		return result
	}
	for _, obj := range c.svcIndexer.List() {
		if svc, ok := obj.(*corev1.Service); ok {
			result = append(result, *svc)
		}
	}
	return result
}

// ActiveSubnets returns the set of subnets that have at least one healthy
// lbnodeagent, determined by listing PureLB leases and checking their
// renewal timestamps. This is a one-shot API call (not cached) because
// it's only called during multi-pool allocation, which is a rare event.
func (c *Client) ActiveSubnets(namespace string) ([]string, error) {
	ctx, cancel := c.apiContext()
	defer cancel()

	leases, err := c.client.CoordinationV1().Leases(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing leases: %w", err)
	}

	subnets := map[string]bool{}
	now := time.Now()

	for _, lease := range leases.Items {
		// Only consider PureLB node leases
		if !strings.HasPrefix(lease.Name, election.LeasePrefix) {
			continue
		}
		// Skip leases without renewal info
		if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
			continue
		}
		// Skip expired leases
		expiry := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
		if now.After(expiry) {
			continue
		}
		// Extract subnets from the lease annotation
		if ann, ok := lease.Annotations[election.SubnetsAnnotation]; ok {
			for _, s := range election.ParseSubnetsAnnotation(ann) {
				subnets[s] = true
			}
		}
	}

	result := make([]string, 0, len(subnets))
	for s := range subnets {
		result = append(result, s)
	}
	return result, nil
}

// maybeUpdateService writes the "is" service back to the cluster, but
// only if it's different than the "was" service.
func (c *Client) maybeUpdateService(was, is *corev1.Service) error {
	var (
		svcUpdated *corev1.Service
		err        error
	)

	ctx, cancel := c.apiContext()
	defer cancel()

	if !reflect.DeepEqual(was.Status, is.Status) {
		svcUpdated, err = c.client.CoreV1().Services(is.Namespace).UpdateStatus(ctx, is, metav1.UpdateOptions{})
		if err != nil {
			if c.isShutdownError(err) {
				c.logger.Log("op", "updateServiceStatus", "msg", "skipping update during shutdown")
				return nil
			}
			c.logger.Log("op", "updateServiceStatus", "error", err, "msg", "failed to update service status")
			return err
		}
	}
	if !(reflect.DeepEqual(was.Annotations, is.Annotations) && reflect.DeepEqual(was.Spec, is.Spec)) {
		ann := is.Annotations
		spec := is.Spec.DeepCopy()
		if svcUpdated != nil {
			svcUpdated.DeepCopyInto(is)
		} else {
			c.logger.Log("msg", "svcUpdated is nil")
		}
		is.Annotations = ann
		spec.DeepCopyInto(&is.Spec)
		if _, err = c.client.CoreV1().Services(is.Namespace).Update(ctx, is, metav1.UpdateOptions{}); err != nil {
			if c.isShutdownError(err) {
				c.logger.Log("op", "updateService", "msg", "skipping update during shutdown")
				return nil
			}
			c.logger.Log("op", "updateService", "error", err, "msg", "failed to update service")
			return err
		}
	}

	return nil
}

// Infof logs an informational event about obj to the Kubernetes cluster.
func (c *Client) Infof(obj runtime.Object, kind, msg string, args ...interface{}) {
	c.events.Eventf(obj, corev1.EventTypeNormal, kind, msg, args...)
}

// Errorf logs an error event about obj to the Kubernetes cluster.
func (c *Client) Errorf(obj runtime.Object, kind, msg string, args ...interface{}) {
	c.events.Eventf(obj, corev1.EventTypeWarning, kind, msg, args...)
}

// announcingOnlyUpdate reports whether the sole difference between two versions
// of a Service is one or more purelb.io/announcing-* annotations. When true the
// update can be dropped: nothing in the reconcile path reads that annotation.
//
// It is deliberately conservative. It skips only when Spec and Status are
// unchanged and the annotation maps differ purely in announcing-* keys. An
// identical redelivery (informer resync) has equal annotation maps and is NOT
// skipped, so the resync self-healing floor keeps working; any change to Spec,
// Status, or a non-announcing annotation falls through to a normal enqueue.
func announcingOnlyUpdate(old, new *corev1.Service) bool {
	if !reflect.DeepEqual(old.Spec, new.Spec) {
		return false
	}
	if !reflect.DeepEqual(old.Status, new.Status) {
		return false
	}
	if reflect.DeepEqual(old.Annotations, new.Annotations) {
		// No annotation change (resync, or some other field churned). Let it
		// through rather than guess whether the other change matters.
		return false
	}
	return annotationsEqualIgnoringAnnouncing(old.Annotations, new.Annotations)
}

// annotationsEqualIgnoringAnnouncing compares two annotation maps while
// ignoring every purelb.io/announcing-* key.
func annotationsEqualIgnoringAnnouncing(a, b map[string]string) bool {
	countNonAnnouncing := func(m map[string]string) int {
		n := 0
		for k := range m {
			if !strings.HasPrefix(k, purelbv2.AnnounceAnnotation) {
				n++
			}
		}
		return n
	}
	if countNonAnnouncing(a) != countNonAnnouncing(b) {
		return false
	}
	for k, av := range a {
		if strings.HasPrefix(k, purelbv2.AnnounceAnnotation) {
			continue
		}
		if bv, ok := b[k]; !ok || bv != av {
			return false
		}
	}
	return true
}

func (c *Client) sync(key queueItem) SyncState {
	defer c.queue.Done(key)

	switch k := key.(type) {
	case svcKey:
		svcName := string(k)
		l := log.With(c.logger, "service", svcName)

		// there are two "special" services: "kubernetes" and
		// "kube-dns". We don't care about them so we don't want them
		// generating log spam.
		if svcName == "default/kubernetes" || svcName == "kube-system/kube-dns" {
			return SyncStateSuccess
		}

		// there are two "special" endpoints:
		// kube-system/kube-controller-manager and
		// kube-system/kube-scheduler. They cause event spam because
		// they hold the leader election leases which update
		// frequently. These events are useless so we want to return
		// silently and not spam the logs. We can remove this check
		// if https://github.com/kubernetes/kubernetes/issues/34627
		// is ever fixed.
		if svcName == "kube-system/kube-controller-manager" || svcName == "kube-system/kube-scheduler" {
			return SyncStateSuccess
		}

		svcMaybe, exists, err := c.svcIndexer.GetByKey(svcName)
		if err != nil {
			l.Log("op", "getService", "error", err, "msg", "failed to get service")
			return SyncStateError
		}
		if !exists {
			// l.Log("op", "getService", "msg", "doesn't exist")
			return c.serviceDeleted(svcName)
		}
		// DeepCopy: svcIndexer returns the shared informer cache object.
		// The announcer mutates the Service it is handed (annotations, and
		// ExternalTrafficPolicy), so we must not let it write into the cache
		// -- both because mutating cache objects is a client-go prohibition
		// and because, on an Update conflict, the requeued sync must re-read
		// the pristine server state rather than our own failed mutation.
		svc := svcMaybe.(*corev1.Service).DeepCopy()

		// Fetch all EndpointSlices for this service using our custom indexer
		var epSlices []*discoveryv1.EndpointSlice
		if c.epSliceIndexer != nil {
			sliceObjs, err := c.epSliceIndexer.ByIndex(serviceNameIndexName, svcName)
			if err != nil {
				l.Log("op", "getEndpointSlices", "error", err, "msg", "failed to get endpoint slices")
				return SyncStateError
			}
			for _, obj := range sliceObjs {
				if slice, ok := obj.(*discoveryv1.EndpointSlice); ok {
					epSlices = append(epSlices, slice)
				}
			}
		}

		// stash a copy of the service so we'll be able to tell if the app
		// changes it
		svcOriginal := svc.DeepCopy()

		// tell the app about the service change
		status := c.serviceChanged(svc, epSlices)

		// write any changes to the service back to the cluster
		if status == SyncStateSuccess {
			err = c.maybeUpdateService(svcOriginal, svc)
			if err != nil {
				l.Log("op", "updateService", "error", err)
				status = SyncStateError
			}
		}

		return status

	case synced:
		if c.synced != nil {
			c.synced()
		}
		// Publish pool status once the caches are warm, so pools that
		// own no Services still get a status even if the config arrived
		// before this point.
		c.EnqueuePoolStatus()
		return SyncStateSuccess

	case poolStatus:
		if c.publishPoolStatus == nil {
			return SyncStateSuccess
		}
		ctx, cancel := c.apiContext()
		defer cancel()
		if err := c.publishPoolStatus(ctx); err != nil {
			c.logger.Log("op", "publishPoolStatus", "error", err)
		}
		// Deliberately never SyncStateError. The failures that persist
		// here are permanent, not transient: a ServiceGroup CRD without
		// the status subresource (404) or a missing servicegroups/status
		// RBAC grant (403). Requeuing those would spin forever on the
		// goroutine that allocates addresses. Transient failures are
		// covered by how often a publish is triggered anyway — every
		// config change, every Service deletion, and the LBNodeAgent
		// informer resync.
		return SyncStateSuccess

	default:
		panic(fmt.Errorf("unknown key type for %#v (%T)", key, key))
	}
}
