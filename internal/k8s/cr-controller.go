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
	"context"
	"fmt"
	"time"

	"github.com/go-kit/kit/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"purelb.io/internal/config"

	purelbv1 "purelb.io/pkg/apis/v1"
	clientset "purelb.io/pkg/generated/clientset/versioned"
	purelbscheme "purelb.io/pkg/generated/clientset/versioned/scheme"
	informers "purelb.io/pkg/generated/informers/externalversions/apis/v1"
	listers "purelb.io/pkg/generated/listers/apis/v1"
)

const controllerAgentName = "cr-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a ServiceGroup is synced
	SuccessSynced = "Synced"

	// MessageResourceSynced is the message used for an Event fired when a ServiceGroup
	// is synced successfully
	MessageResourceSynced = "ServiceGroup synced successfully"
)

// Controller is the controller implementation for ServiceGroup resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// purelbclientset is a clientset for our own API group
	purelbclientset clientset.Interface

	serviceGroupsLister listers.ServiceGroupLister
	serviceGroupsSynced cache.InformerSynced
	configCB            func(log.Logger, *config.Config) SyncState

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder

	logger log.Logger
}

// NewController returns a new purelb controller
func NewCRController(
	logger log.Logger,
	configCB func(log.Logger, *config.Config) SyncState,
	kubeclientset kubernetes.Interface,
	purelbclientset clientset.Interface,
	serviceGroupInformer informers.ServiceGroupInformer) *Controller {

	// Create event broadcaster
	// Add cr-controller types to the default Kubernetes Scheme so Events can be
	// logged for cr-controller types.
	utilruntime.Must(purelbscheme.AddToScheme(scheme.Scheme))
	logger.Log("msg", "Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		logger:              logger,
		configCB:            configCB,
		kubeclientset:       kubeclientset,
		purelbclientset:     purelbclientset,
		serviceGroupsLister: serviceGroupInformer.Lister(),
		serviceGroupsSynced: serviceGroupInformer.Informer().HasSynced,
		workqueue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ServiceGroups"),
		recorder:            recorder,
	}

	logger.Log("msg", "Setting up event handlers")
	// Set up an event handler for when ServiceGroup resources change
	serviceGroupInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueServiceGroup,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueServiceGroup(new)
		},
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	c.logger.Log("msg", "Starting ServiceGroup controller")

	// Wait for the caches to be synced before starting workers
	c.logger.Log("msg", "Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.serviceGroupsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	c.logger.Log("msg", "Starting workers")
	// Launch two workers to process ServiceGroup resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	c.logger.Log("msg", "Started workers")
	<-stopCh
	c.logger.Log("msg", "Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
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
		// Run the syncHandler, passing it the namespace/name string of the
		// ServiceGroup resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
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

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the ServiceGroup resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	c.logger.Log("service group change", key)

	groups, err := c.serviceGroupsLister.ServiceGroups("").List(labels.Everything())
	if err == nil {
		cfg, err := config.ParseServiceGroups(groups)
		if err == nil {
			c.configCB(c.logger, cfg)
			return nil
		}
	}

	return err
}

func (c *Controller) updateServiceGroupStatus(serviceGroup *purelbv1.ServiceGroup) error {
	copy := serviceGroup.DeepCopy()
	_, err := c.purelbclientset.PurelbV1().ServiceGroups(serviceGroup.Namespace).Update(context.TODO(), copy, metav1.UpdateOptions{})
	return err
}

// enqueueServiceGroup takes a ServiceGroup resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than ServiceGroup.
func (c *Controller) enqueueServiceGroup(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the ServiceGroup resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that ServiceGroup resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		c.logger.Log("Recovered deleted object from tombstone", object.GetName())
	}
	c.logger.Log("Processing object", object.GetName())
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		// If this object is not owned by a ServiceGroup, we should not do anything more
		// with it.
		if ownerRef.Kind != "ServiceGroup" {
			return
		}

		serviceGroup, err := c.serviceGroupsLister.ServiceGroups(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			c.logger.Log("ignoring orphaned object", object.GetSelfLink(), "serviceGroup", ownerRef.Name)
			return
		}

		c.enqueueServiceGroup(serviceGroup)
		return
	}
}
