// Copyright 2017 The Kubernetes Authors.
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
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-kit/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	clientset "purelb.io/pkg/generated/clientset/versioned"
	purelbscheme "purelb.io/pkg/generated/clientset/versioned/scheme"
	"purelb.io/pkg/generated/informers/externalversions"
	listers "purelb.io/pkg/generated/listers/purelb/v2"
)

const controllerAgentName = "cr-controller"

// Controller is the controller implementation for ServiceGroup
// resources.
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// purelbclientset is a clientset for our own API group
	purelbclientset clientset.Interface

	// installNamespace is the namespace PureLB is installed in, as resolved
	// by InstallNamespace. Empty when it could not be determined.
	installNamespace string

	sgsSynced   cache.InformerSynced
	configCB    func(*purelbv2.Config) SyncState
	forceSync   func()
	sgLister    listers.ServiceGroupLister
	lbnasSynced cache.InformerSynced
	lbnaLister  listers.LBNodeAgentLister

	// publishPoolStatus asks the consumer to republish every pool's
	// ServiceGroup .status after a config change. Separate from
	// forceSync because a pool with no Services has nothing for
	// forceSync to reprocess — which is exactly the case that left
	// .status blank.
	publishPoolStatus func()

	// workqueue is a rate limited work queue. This is used to queue
	// work to be processed instead of performing it as soon as a change
	// happens. This means we can ensure we only process a fixed amount
	// of resources at a time, and makes it easy to ensure we are never
	// processing the same item simultaneously in two different workers.
	workqueue workqueue.TypedRateLimitingInterface[string]
	// recorder is an event recorder for recording Event resources to
	// the Kubernetes API.
	recorder record.EventRecorder

	logger log.Logger
}

// NewCRController returns a new controller that watches for changes
// to PureLB custom resources.
func NewCRController(
	logger log.Logger,
	installNamespace string,
	configCB func(*purelbv2.Config) SyncState,
	forceSync func(),
	publishPoolStatus func(),
	kubeclientset kubernetes.Interface,
	purelbclientset clientset.Interface,
	informerFactory externalversions.SharedInformerFactory) *Controller {

	sgInformer := informerFactory.Purelb().V2().ServiceGroups()
	lbnaInformer := informerFactory.Purelb().V2().LBNodeAgents()

	// Create event broadcaster
	// Add cr-controller types to the default Kubernetes Scheme so Events can be
	// logged for cr-controller types.
	utilruntime.Must(purelbscheme.AddToScheme(scheme.Scheme))
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		logger:            logger,
		installNamespace:  installNamespace,
		configCB:          configCB,
		forceSync:         forceSync,
		publishPoolStatus: publishPoolStatus,
		kubeclientset:     kubeclientset,
		purelbclientset:   purelbclientset,
		lbnaLister:        lbnaInformer.Lister(),
		lbnasSynced:       lbnaInformer.Informer().HasSynced,
		sgLister:          sgInformer.Lister(),
		sgsSynced:         sgInformer.Informer().HasSynced,
		workqueue:         workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "ServiceGroups"}),
		recorder:          recorder,
	}

	// Set up event handlers for when resources change
	sgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(added interface{}) {
			controller.enqueueResource("sg", added)
		},
		UpdateFunc: func(old, new interface{}) {
			if sgUpdateNeedsReconcile(old, new) {
				controller.enqueueResource("sg", new)
			}
		},
		DeleteFunc: func(deleted interface{}) {
			controller.enqueueResource("sg", deleted)
		},
	})
	lbnaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(added interface{}) {
			controller.enqueueResource("lbna", added)
		},
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueResource("lbna", new)
		},
		DeleteFunc: func(deleted interface{}) {
			controller.enqueueResource("lbna", deleted)
		},
	})

	return controller
}

// Run will set up the event handlers for types we are interested in,
// as well as syncing informer caches and starting workers. It will
// block until stopCh is closed, at which point it will shutdown the
// workqueue and wait for workers to finish processing their current
// work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Wait for the caches to be synced before starting workers
	if ok := cache.WaitForCacheSync(stopCh, c.sgsSynced, c.lbnasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	// Launch workers to process ServiceGroup resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	c.logger.Log("msg", "workers started")
	<-stopCh
	c.logger.Log("msg", "Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message
// on the workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue
// and attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	key, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We call Done here so the workqueue knows we have finished
	// processing this item. We also must remember to call Forget if
	// we do not want this work item being re-queued. For example, we
	// do not call Forget if a transient error occurs, instead the
	// item is put back on the workqueue and attempted again after a
	// back-off period.
	defer c.workqueue.Done(key)

	// Run the syncHandler, passing it the namespace/name string of
	// the ServiceGroup resource to be synced.
	if err := c.syncHandler(); err != nil {
		// Put the item back on the workqueue to handle any transient
		// errors.
		c.workqueue.AddRateLimited(key)
		utilruntime.HandleError(fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error()))
		return true
	}

	// Finally, if no error occurs we Forget this item so it does not
	// get queued again until another change happens.
	c.workqueue.Forget(key)
	return true
}

// syncHandler notifies the callback that there's a new config.  We
// scopeToInstallNamespace splits groups into those PureLB will use and those
// it ignores because they live outside the install namespace.
//
// A ServiceGroup's namespace has never meant anything: it selects where
// .status is written and nothing else. A group in "tenant-a" contributes a
// pool to the same cluster-wide table as one in the install namespace, while
// pools are keyed by name alone -- so out-of-namespace groups bought no
// isolation and cost cross-namespace name collisions and a route to claim the
// "default" pool. They are no longer read.
//
// Two deliberate fail-open branches. Both exist because getting this wrong
// silently removes every pool, and an allocator that reports healthy while
// failing every allocation is worse than one that ignores a misplaced object.
func (c *Controller) scopeToInstallNamespace(groups []*purelbv2.ServiceGroup) (kept, dropped []*purelbv2.ServiceGroup) {
	// 1. Namespace unknown: filter nothing. Better to honour a misplaced
	//    ServiceGroup than to discard every correctly-placed one.
	if c.installNamespace == "" {
		c.logger.Log("op", "scopeServiceGroups", "event", "installNamespaceUnknown",
			"msg", "install namespace could not be determined; ServiceGroups are not being scoped. Set --namespace or PURELB_NAMESPACE")
		return groups, nil
	}

	for _, g := range groups {
		if g.Namespace == c.installNamespace {
			kept = append(kept, g)
		} else {
			dropped = append(dropped, g)
		}
	}

	// 2. Every group dropped: the namespace is wrong, not the cluster.
	//    Nobody installs PureLB and then places all of their ServiceGroups
	//    somewhere else, so treat this as a misconfigured namespace.
	if len(kept) == 0 && len(dropped) > 0 {
		c.logger.Log("op", "scopeServiceGroups", "event", "installNamespaceSuspect",
			"installNamespace", c.installNamespace, "wouldDrop", len(dropped),
			"msg", "scoping would discard every ServiceGroup; keeping them all and continuing. Check --namespace or PURELB_NAMESPACE")
		return groups, nil
	}

	return kept, dropped
}

// process the config as a single unit so anytime we get a
// notification that something changed we read everything.
func (c *Controller) syncHandler() error {
	var (
		err error
		cfg purelbv2.Config = purelbv2.Config{}
	)

	cfg.Groups, err = c.sgLister.ServiceGroups("").List(labels.Everything())
	if err != nil {
		c.logger.Log("error listing ServiceGroups", err)
		return err
	}
	// The lister walks the indexer's map, so its order varies between calls.
	// parseGroups is order-sensitive -- it rejects the LATER of two
	// overlapping-range ServiceGroups -- so without a stable order which one
	// survives flips from one reconcile to the next. Sort by
	// (namespace, name): a total order, because that pair is unique for any
	// Kubernetes object. Sorting by name alone would leave two same-named
	// ServiceGroups in different namespaces tied, and sort.Slice is not
	// stable, so the flapping would persist.
	//
	// Sorting in place is safe: List appends into a fresh slice on every
	// call, so both the header and the backing array are per-call and the
	// shared cache objects are never touched. Do not "fix" this with a
	// defensive clone.
	slices.SortFunc(cfg.Groups, func(a, b *purelbv2.ServiceGroup) int {
		if n := strings.Compare(a.Namespace, b.Namespace); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	cfg.Groups, cfg.DroppedGroups = c.scopeToInstallNamespace(cfg.Groups)
	cfg.Agents, err = c.lbnaLister.LBNodeAgents("").List(labels.Everything())
	if err != nil {
		c.logger.Log("error listing node agents", err)
		return err
	}

	// Check whether we should be the default service announcer
	if os.Getenv("DEFAULT_ANNOUNCER") == purelbv2.Brand {
		cfg.DefaultAnnouncer = true
	}

	switch c.configCB(&cfg) {
	case SyncStateSuccess:
		updates.WithLabelValues("config").Inc()
		configLoaded.Set(1)
	case SyncStateError:
		// Count the error AND return non-nil so processNextWorkItem
		// requeues this delivery with rate-limited backoff. Without the
		// requeue a transient failure inside the callback (e.g. a Node
		// GET during config delivery) would silently leave the consumer
		// on stale config until the next CR event. Retrying a permanent
		// config error is bounded (the rate limiter caps backoff and
		// Forget resets it on the next success) and both consumers'
		// callbacks are idempotent. forceSync is deliberately not called.
		updateErrors.WithLabelValues("config").Inc()
		return fmt.Errorf("config rejected by consumer, requeuing")
	case SyncStateReprocessAll:
		updates.WithLabelValues("config").Inc()
		configLoaded.Set(1)
		c.forceSync()
		// Republish pool status too. forceSync only re-enqueues
		// Services, so a ServiceGroup that owns none would otherwise
		// never have its status written. Enqueued after forceSync so
		// the sweep lands behind the Service keys it depends on.
		if c.publishPoolStatus != nil {
			c.publishPoolStatus()
		}
	}

	return nil
}

// enqueueResource takes a resource and converts it into a
// thing/namespace/name string which is then put onto the work
// queue. This method should *not* be passed resources of any type
// other than ServiceGroup or LBNodeAgent.
func (c *Controller) enqueueResource(thing string, obj interface{}) {
	var key string
	var err error
	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(thing + "/" + key)
}

// sgUpdateNeedsReconcile reports whether a ServiceGroup update event
// should trigger reconciliation. It skips status-only updates: the
// allocator writes SG .status via the status subresource, and those
// writes don't bump .metadata.generation but still fire a watch event.
// Without this filter every status write would trigger a full
// SetConfig → SetPools → forceSync cycle over all services — O(N) per
// status write, catastrophic at scale.
//
// Unknown / non-ServiceGroup payloads default to reconcile (fail safe).
func sgUpdateNeedsReconcile(old, new interface{}) bool {
	oldSG, okOld := old.(*purelbv2.ServiceGroup)
	newSG, okNew := new.(*purelbv2.ServiceGroup)
	if !okOld || !okNew {
		return true
	}
	// Generation bumps only on spec changes (status subresource is
	// enabled), so equal generations means a status-only update.
	return oldSG.Generation != newSG.Generation
}
