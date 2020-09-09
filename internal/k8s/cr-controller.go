// Copyright 2020 Acnodal Inc.
// Copyright 2017 The Kubernetes Authors.
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
	"time"

	"github.com/go-kit/kit/log"
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

	purelbv1 "purelb.io/pkg/apis/v1"
	clientset "purelb.io/pkg/generated/clientset/versioned"
	purelbscheme "purelb.io/pkg/generated/clientset/versioned/scheme"
	"purelb.io/pkg/generated/informers/externalversions"
	listers "purelb.io/pkg/generated/listers/apis/v1"
)

const controllerAgentName = "cr-controller"

// Controller is the controller implementation for ServiceGroup
// resources.
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// purelbclientset is a clientset for our own API group
	purelbclientset clientset.Interface

	sgsSynced   cache.InformerSynced
	configCB    func(*purelbv1.Config) SyncState
	sgLister    listers.ServiceGroupLister
	lbnasSynced cache.InformerSynced
	lbnaLister  listers.LBNodeAgentLister

	// workqueue is a rate limited work queue. This is used to queue
	// work to be processed instead of performing it as soon as a change
	// happens. This means we can ensure we only process a fixed amount
	// of resources at a time, and makes it easy to ensure we are never
	// processing the same item simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to
	// the Kubernetes API.
	recorder record.EventRecorder

	logger log.Logger
}

// NewCRController returns a new controller that watches for changes
// to PureLB custom resources.
func NewCRController(
	logger log.Logger,
	configCB func(*purelbv1.Config) SyncState,
	kubeclientset kubernetes.Interface,
	purelbclientset clientset.Interface,
	informerFactory externalversions.SharedInformerFactory) *Controller {

	sgInformer := informerFactory.Purelb().V1().ServiceGroups()
	lbnaInformer := informerFactory.Purelb().V1().LBNodeAgents()

	// Create event broadcaster
	// Add cr-controller types to the default Kubernetes Scheme so Events can be
	// logged for cr-controller types.
	utilruntime.Must(purelbscheme.AddToScheme(scheme.Scheme))
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		logger:          logger,
		configCB:        configCB,
		kubeclientset:   kubeclientset,
		purelbclientset: purelbclientset,
		lbnaLister:      lbnaInformer.Lister(),
		lbnasSynced:     lbnaInformer.Informer().HasSynced,
		sgLister:        sgInformer.Lister(),
		sgsSynced:       sgInformer.Informer().HasSynced,
		workqueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ServiceGroups"),
		recorder:        recorder,
	}

	// Set up event handlers for when resources change
	sgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(added interface{}) {
			controller.enqueueResource("sg", added)
		},
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueResource("sg", new)
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
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if
		// we do not want this work item being re-queued. For example, we
		// do not call Forget if a transient error occurs, instead the
		// item is put back on the workqueue and attempted again after a
		// back-off period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of
		// the ServiceGroup resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient
			// errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		c.logger.Log("successfully synced", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler notifies the callback that the config has changed.
func (c *Controller) syncHandler(key string) error {
	groups, err := c.sgLister.ServiceGroups("").List(labels.Everything())
	if err != nil {
		c.logger.Log("error listing service groups", key)
		return err
	}
	nodeagents, err := c.lbnaLister.LBNodeAgents("").List(labels.Everything())
	if err != nil {
		c.logger.Log("error listing node agents", key)
		return err
	}
	cfg, err := purelbv1.ParseConfig(groups, nodeagents)
	if err == nil {
		c.configCB(cfg)
		return nil
	}

	return err
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
