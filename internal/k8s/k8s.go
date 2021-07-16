// Copyright 2017 Google Inc.
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

package k8s

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	purelbv1 "purelb.io/pkg/apis/v1"
	"purelb.io/pkg/generated/clientset/versioned"
	"purelb.io/pkg/generated/informers/externalversions"

	"github.com/go-kit/kit/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// Client watches a Kubernetes cluster and translates events into
// Controller method calls.
type Client struct {
	logger log.Logger

	client *kubernetes.Clientset
	events record.EventRecorder
	queue  workqueue.RateLimitingInterface

	svcIndexer  cache.Indexer
	svcInformer cache.Controller
	epIndexer   cache.Indexer
	epInformer  cache.Controller

	crInformerFactory externalversions.SharedInformerFactory
	crController      Controller

	syncFuncs []cache.InformerSynced

	serviceChanged func(*corev1.Service, *corev1.Endpoints) SyncState
	serviceDeleted func(string) SyncState
	configChanged  func(*purelbv1.Config) SyncState
	synced         func()
	shutdown       func()
}

// ServiceEvent adds events to services.
type ServiceEvent interface {
	Infof(svc *corev1.Service, desc, msg string, args ...interface{})
	Errorf(svc *corev1.Service, desc, msg string, args ...interface{})
	ForceSync()
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
	// Labels used to select pods that should participate in Memberlist
	// elections
	mlLabels = "app=purelb,component=lbnodeagent"
)

// Config specifies the configuration of the Kubernetes
// client/watcher.
type Config struct {
	ProcessName   string
	NodeName      string
	ReadEndpoints bool
	Logger        log.Logger
	Kubeconfig    string
	PollInterval  time.Duration

	ServiceChanged func(*corev1.Service, *corev1.Endpoints) SyncState
	ServiceDeleted func(string) SyncState
	ConfigChanged  func(*purelbv1.Config) SyncState
	Synced         func()
	Shutdown       func()
}

type svcKey string
type synced string

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

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	c := &Client{
		logger: cfg.Logger,
		client: clientset,
		events: recorder,
		queue:  queue,
	}

	// Custom Resource Watcher

	c.crInformerFactory = externalversions.NewSharedInformerFactory(crClient, cfg.PollInterval)
	c.crController = *NewCRController(c.logger, cfg.ConfigChanged, c.ForceSync, clientset, crClient, c.crInformerFactory)

	// Service Watcher

	svcHandlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(svcKey(key))
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
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
	c.svcIndexer, c.svcInformer = cache.NewIndexerInformer(svcWatcher, &corev1.Service{}, 0, svcHandlers, cache.Indexers{})

	c.serviceChanged = cfg.ServiceChanged
	c.serviceDeleted = cfg.ServiceDeleted
	c.syncFuncs = append(c.syncFuncs, c.svcInformer.HasSynced)

	// Endpoint Watcher (used by node agents, not the allocator)

	if cfg.ReadEndpoints {
		epHandlers := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(svcKey(key))
				}
			},
			UpdateFunc: func(old interface{}, new interface{}) {
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
		epWatcher := cache.NewListWatchFromClient(c.client.CoreV1().RESTClient(), "endpoints", corev1.NamespaceAll, fields.Everything())
		c.epIndexer, c.epInformer = cache.NewIndexerInformer(epWatcher, &corev1.Endpoints{}, 0, epHandlers, cache.Indexers{})

		c.syncFuncs = append(c.syncFuncs, c.epInformer.HasSynced)
	}

	// Sync Watcher

	c.synced = cfg.Synced

	// Shutdown hook

	c.shutdown = cfg.Shutdown

	return c, nil
}

// GetPods get the pods in the namespace matched by the labels string.
func (c *Client) getPods(namespace string) (*corev1.PodList, error) {
	pl, err := c.client.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: mlLabels})
	if err != nil {
		return nil, err
	}
	return pl, nil
}

// GetPodsIPs get the IPs from the pods in the namespace matched by
// the labels string.
func (c *Client) GetPodsIPs(namespace string) ([]string, error) {
	pl, err := c.getPods(namespace)
	if err != nil {
		return nil, err
	}
	iplist := []string{}
	for _, pod := range pl.Items {
		iplist = append(iplist, pod.Status.PodIP)
	}
	return iplist, nil
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
	if c.epInformer != nil {
		go c.epInformer.Run(stopCh)
	}

	if !cache.WaitForCacheSync(stopCh, c.syncFuncs...) {
		return errors.New("timed out waiting for cache sync")
	}

	c.queue.Add(synced(""))

	if stopCh != nil {
		go func() {
			<-stopCh
			c.queue.ShutDown()
		}()
	}

	for {
		key, quit := c.queue.Get()
		if quit {
			c.shutdown()
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

// ForceSync reprocess all watched services
func (c *Client) ForceSync() {
	if c.svcIndexer != nil {
		for _, k := range c.svcIndexer.ListKeys() {
			c.queue.AddRateLimited(svcKey(k))
		}
	}
}

// maybeUpdateService writes the "is" service back to the cluster, but
// only if it's different than the "was" service.
func (c *Client) maybeUpdateService(was, is *corev1.Service) error {
	var (
		svcUpdated *corev1.Service
		err        error
	)

	if !reflect.DeepEqual(was.Status, is.Status) {
		svcUpdated, err = c.client.CoreV1().Services(is.Namespace).UpdateStatus(context.TODO(), is, metav1.UpdateOptions{})
		if err != nil {
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
		if _, err = c.client.CoreV1().Services(is.Namespace).Update(context.TODO(), is, metav1.UpdateOptions{}); err != nil {
			c.logger.Log("op", "updateService", "error", err, "msg", "failed to update service")
			return err
		}
	}

	return nil
}

// Infof logs an informational event about svc to the Kubernetes cluster.
func (c *Client) Infof(svc *corev1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, corev1.EventTypeNormal, kind, msg, args...)
}

// Errorf logs an error event about svc to the Kubernetes cluster.
func (c *Client) Errorf(svc *corev1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, corev1.EventTypeWarning, kind, msg, args...)
}

func (c *Client) sync(key interface{}) SyncState {
	defer c.queue.Done(key)

	switch key.(type) {
	case svcKey:
		svcName := string(key.(svcKey))
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
		svc := svcMaybe.(*corev1.Service)

		var eps *corev1.Endpoints = &corev1.Endpoints{}
		if c.epIndexer != nil {
			epsIntf, exists, err := c.epIndexer.GetByKey(svcName)
			if err != nil {
				l.Log("op", "getEndpoints", "error", err, "msg", "failed to get endpoints")
				return SyncStateError
			}
			if exists {
				eps = epsIntf.(*corev1.Endpoints)
			}
		}

		// stash a copy of the service so we'll be able to tell if the app
		// changes it
		svcOriginal := svc.DeepCopy()

		// tell the app about the service change
		status := c.serviceChanged(svc, eps)

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
		return SyncStateSuccess

	default:
		panic(fmt.Errorf("unknown key type for %#v (%T)", key, key))
	}
}
