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

// Package allocator is the allocator's side of egress: it reconciles each
// Egress and ClusterEgress into addresses, slices and EgressEndpoints, on
// the allocator's work queue (D-conc). The allocator reaches it only
// through egresshook.
package allocator

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	coreallocator "purelb.io/internal/allocator"
	"purelb.io/internal/egress"
	"purelb.io/internal/egresshook"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
	"purelb.io/pkg/generated/informers/externalversions"
	listers "purelb.io/pkg/generated/listers/purelb/v2"
)

// resync is the informers' resync period: a floor under every
// level-triggered reconcile.
const resync = 10 * time.Minute

// writers is how many API writes may be in flight (D-conc).
const writers = 4

// Queue keys. Policies are "egress/<ns>/<name>" and
// "clusteregress/<name>" (egress.PolicyRef.String); a finished write is
// "write:<object key>".
const writePrefix = "write:"

// Reconciler is the allocator's egress controller. Its state belongs to
// the allocator's queue goroutine, which calls every hook; only the
// informers' handlers (which just enqueue) and the writer's workers (which
// just write) run elsewhere.
type Reconciler struct {
	logger    log.Logger
	alloc     *coreallocator.Allocator
	namespace string // PureLB's

	// synced is set once egress can replay: its informers synced, or its
	// CRDs aren't installed. Read by the allocator's populate gate.
	synced atomic.Bool
	// enabled is whether the egress CRDs exist. Set in Start.
	enabled bool

	kube    kubernetes.Interface
	purelb  versioned.Interface
	enqueue func(key string, priority bool)
	writer  *writer

	egresses        listers.EgressLister
	clusterEgresses listers.ClusterEgressLister
	endpoints       listers.EgressEndpointsLister
	members         listers.EgressMembersLister
	nodeStates      listers.EgressNodeStateLister
	pods            cache.Indexer
	namespaces      cache.Indexer
	nodes           cache.Indexer
	services        cache.Indexer
	leases          cache.Indexer
}

var (
	_ egresshook.Allocator  = (*Reconciler)(nil)
	_ egresshook.Controller = (*Reconciler)(nil)
)

// New returns a Reconciler for alloc; Start brings it up.
func New(logger log.Logger, alloc *coreallocator.Allocator, namespace string) *Reconciler {
	return &Reconciler{logger: logger, alloc: alloc, namespace: namespace}
}

// Start discovers the egress CRDs and, when they exist, starts the
// informers and the writer, and waits for the caches. Without the CRDs
// egress stays off, and synced is set at once so allocation proceeds.
func (r *Reconciler) Start(stop <-chan struct{}, cfg *rest.Config, enqueue func(string, bool)) error {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	purelb, err := versioned.NewForConfig(cfg)
	if err != nil {
		return err
	}
	return r.start(stop, kube, purelb, enqueue)
}

// start is Start with the clients built.
func (r *Reconciler) start(stop <-chan struct{}, kube kubernetes.Interface, purelb versioned.Interface, enqueue func(string, bool)) error {
	r.enqueue = enqueue
	r.kube, r.purelb = kube, purelb

	var err error
	r.enabled, err = crdsInstalled(kube)
	if err != nil {
		// Without knowing, allocation would wait forever; fail open to
		// "no egress" and say so.
		logging.Info(r.logger, "op", "egressStart", "error", err,
			"msg", "could not discover the egress CRDs; egress is off until the allocator restarts")
		r.synced.Store(true)
		return nil
	}
	if !r.enabled {
		logging.Info(r.logger, "op", "egressStart", "msg", "egress CRDs not installed; egress is off")
		r.synced.Store(true)
		return nil
	}

	r.writer = newWriter(writers, func(key string, priority bool) { enqueue(writePrefix+key, priority) })
	r.writer.run(stop)

	pf := externalversions.NewSharedInformerFactory(purelb, resync)
	pns := externalversions.NewSharedInformerFactoryWithOptions(purelb, resync, externalversions.WithNamespace(r.namespace))
	kf := kubeinformers.NewSharedInformerFactory(kube, resync)
	kns := kubeinformers.NewSharedInformerFactoryWithOptions(kube, resync, kubeinformers.WithNamespace(r.namespace))

	eg := pf.Purelb().V2().Egresses()
	ceg := pf.Purelb().V2().ClusterEgresses()
	ens := pf.Purelb().V2().EgressNodeStates()
	ee := pns.Purelb().V2().EgressEndpointses()
	em := pns.Purelb().V2().EgressMemberses()
	pods := kf.Core().V1().Pods().Informer()
	if err := pods.SetTransform(k8s.SlimPod(purelbv2.EgressOptOutAnnotation)); err != nil {
		return err
	}
	nss := kf.Core().V1().Namespaces().Informer()
	nodes := kf.Core().V1().Nodes().Informer()
	svcs := kf.Core().V1().Services().Informer()
	leases := kns.Coordination().V1().Leases().Informer()

	r.egresses, r.clusterEgresses, r.nodeStates = eg.Lister(), ceg.Lister(), ens.Lister()
	r.endpoints, r.members = ee.Lister(), em.Lister()
	r.pods, r.namespaces, r.nodes = pods.GetIndexer(), nss.GetIndexer(), nodes.GetIndexer()
	r.services, r.leases = svcs.GetIndexer(), leases.GetIndexer()

	r.handle(eg.Informer(), r.policyOf)
	r.handle(ceg.Informer(), r.policyOf)
	r.handle(ee.Informer(), r.ownerOf)
	r.handle(em.Informer(), r.ownerOf)
	r.handle(ens.Informer(), func(interface{}) []string { return r.allPolicies() })
	r.handle(pods, r.podPolicies)
	r.handle(nss, func(interface{}) []string { return r.clusterPolicies() })
	r.handle(nodes, func(interface{}) []string { return r.allPolicies() })
	r.handle(svcs, r.servicePolicies)
	r.handleLeases(leases)

	pf.Start(stop)
	pns.Start(stop)
	kf.Start(stop)
	kns.Start(stop)
	go func() {
		if !cache.WaitForCacheSync(stop, eg.Informer().HasSynced, ceg.Informer().HasSynced, ens.Informer().HasSynced,
			ee.Informer().HasSynced, em.Informer().HasSynced, pods.HasSynced, nss.HasSynced, nodes.HasSynced,
			svcs.HasSynced, leases.HasSynced) {
			return
		}
		r.synced.Store(true)
		logging.Info(r.logger, "op", "egressStart", "msg", "egress informers synced")
		for _, k := range r.Keys() {
			enqueue(k, false)
		}
	}()
	return nil
}

// crdsInstalled reports whether the API server serves the Egress kind.
func crdsInstalled(kube kubernetes.Interface) (bool, error) {
	res, err := kube.Discovery().ServerResourcesForGroupVersion(purelbv2.SchemeGroupVersion.String())
	if err != nil {
		return false, err
	}
	for _, r := range res.APIResources {
		if r.Name == "egresses" {
			return true, nil
		}
	}
	return false, nil
}

// handle enqueues the policy keys keys returns for every event on inf.
func (r *Reconciler) handle(inf cache.SharedIndexInformer, keys func(obj interface{}) []string) {
	add := func(obj interface{}) {
		if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = t.Obj
		}
		for _, k := range keys(obj) {
			r.enqueue(k, false)
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    add,
		UpdateFunc: func(_, obj interface{}) { add(obj) },
		DeleteFunc: add,
	})
}

// handleLeases reconciles every policy when the election's membership or
// a node's subnets change, ignoring the renewals that make up nearly all
// Lease updates.
func (r *Reconciler) handleLeases(inf cache.SharedIndexInformer) {
	all := func(interface{}) {
		for _, k := range r.allPolicies() {
			r.enqueue(k, false)
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: all,
		UpdateFunc: func(oldObj, newObj interface{}) {
			o, ok1 := oldObj.(*coordinationv1.Lease)
			n, ok2 := newObj.(*coordinationv1.Lease)
			if ok1 && ok2 && o.Annotations[purelbv2.SubnetsAnnotation] == n.Annotations[purelbv2.SubnetsAnnotation] &&
				ptrEqual(o.Spec.HolderIdentity, n.Spec.HolderIdentity) {
				return
			}
			all(newObj)
		},
		DeleteFunc: all,
	})
}

func ptrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// policyOf is the key of an Egress or ClusterEgress.
func (r *Reconciler) policyOf(obj interface{}) []string {
	switch o := obj.(type) {
	case *purelbv2.Egress:
		return []string{egress.PolicyRef{Kind: egress.KindEgress, Namespace: o.Namespace, Name: o.Name}.String()}
	case *purelbv2.ClusterEgress:
		return []string{egress.PolicyRef{Kind: egress.KindClusterEgress, Name: o.Name}.String()}
	}
	return nil
}

// ownerOf is the key of the policy that owns an EgressEndpoints or
// EgressMembers, from its labels.
func (r *Reconciler) ownerOf(obj interface{}) []string {
	var l map[string]string
	switch o := obj.(type) {
	case *purelbv2.EgressEndpoints:
		l = o.Labels
	case *purelbv2.EgressMembers:
		l = o.Labels
	}
	if l[egress.LabelName] == "" {
		return nil
	}
	return []string{egress.PolicyRef{Kind: l[egress.LabelKind], Namespace: l[egress.LabelNamespace], Name: l[egress.LabelName]}.String()}
}

// podPolicies are the policies a pod's change can affect: the Egresses in
// its namespace and every ClusterEgress.
func (r *Reconciler) podPolicies(obj interface{}) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	keys := r.clusterPolicies()
	egs, _ := r.egresses.Egresses(pod.Namespace).List(labels.Everything())
	for _, e := range egs {
		keys = append(keys, r.policyOf(e)...)
	}
	return keys
}

// servicePolicies are the Egresses in a Service's namespace that refer to
// it.
func (r *Reconciler) servicePolicies(obj interface{}) []string {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return nil
	}
	var keys []string
	egs, _ := r.egresses.Egresses(svc.Namespace).List(labels.Everything())
	for _, e := range egs {
		if e.Spec.ServiceRef != nil && e.Spec.ServiceRef.Name == svc.Name {
			keys = append(keys, r.policyOf(e)...)
		}
	}
	return keys
}

func (r *Reconciler) clusterPolicies() []string {
	var keys []string
	cegs, _ := r.clusterEgresses.List(labels.Everything())
	for _, c := range cegs {
		keys = append(keys, r.policyOf(c)...)
	}
	return keys
}

func (r *Reconciler) allPolicies() []string {
	keys := r.clusterPolicies()
	egs, _ := r.egresses.List(labels.Everything())
	for _, e := range egs {
		keys = append(keys, r.policyOf(e)...)
	}
	return keys
}

// Keys returns every policy key, for ForceSync.
func (r *Reconciler) Keys() []string {
	if !r.enabled || !r.synced.Load() {
		return nil
	}
	return r.allPolicies()
}

// Synced reports whether egress has replayed its state, or has none.
func (r *Reconciler) Synced() bool {
	return r.synced.Load()
}

// Sync processes one queue key on the allocator's queue goroutine.
func (r *Reconciler) Sync(key string) error {
	if !r.enabled || !r.synced.Load() {
		return nil
	}
	if obj, ok := strings.CutPrefix(key, writePrefix); ok {
		return r.writeDone(obj)
	}
	ref, err := parsePolicyKey(key)
	if err != nil {
		logging.Info(r.logger, "op", "egressSync", "key", key, "error", err)
		return nil
	}
	return r.reconcile(ref)
}

// parsePolicyKey parses egress.PolicyRef.String.
func parsePolicyKey(key string) (egress.PolicyRef, error) {
	parts := strings.Split(key, "/")
	switch {
	case len(parts) == 3 && parts[0] == "egress":
		return egress.PolicyRef{Kind: egress.KindEgress, Namespace: parts[1], Name: parts[2]}, nil
	case len(parts) == 2 && parts[0] == "clusteregress":
		return egress.PolicyRef{Kind: egress.KindClusterEgress, Name: parts[1]}, nil
	}
	return egress.PolicyRef{}, fmt.Errorf("not an egress policy key")
}

// writeDone collects a finished write's outcome. A failed write requeues
// the policy that owns the object, which recomputes and rewrites it.
func (r *Reconciler) writeDone(obj string) error {
	err, ok := r.writer.done(obj)
	if !ok || err == nil {
		return nil
	}
	logging.Info(r.logger, "op", "egressWrite", "object", obj, "error", err)
	return nil
}

// reconcile brings one policy's addresses, slices and status in line with
// its spec and the cluster. Filled in by the reconcile stages.
func (r *Reconciler) reconcile(ref egress.PolicyRef) error {
	logging.Debug(r.logger, "op", "egressReconcile", "policy", ref.String())
	return nil
}

// Replay records egress's allocations and holds in the pools after a
// rebuild. Filled in with address allocation.
func (r *Reconciler) Replay(func(pool, key string, ips []net.IP, reference bool) error) {}

// ReleaseService lets the allocator release a Service's addresses. Filled
// in with tombstones.
func (r *Reconciler) ReleaseService(string, string, net.IP) bool { return true }
