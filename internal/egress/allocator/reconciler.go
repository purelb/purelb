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
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	coreallocator "purelb.io/internal/allocator"
	"purelb.io/internal/egress"
	"purelb.io/internal/egresshook"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
	purelbscheme "purelb.io/pkg/generated/clientset/versioned/scheme"
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
	// writeOwner maps an object key with a write in progress to the
	// queue key that owns it, which a failed write requeues. Queue
	// goroutine only.
	writeOwner map[string]string

	// Queue goroutine only: when each lagging gating node fell behind,
	// per EgressEndpoints; when each Draining slice was first seen, per
	// EgressEndpoints; each requeueAfter key's pending deadline.
	behind     map[string]map[string]time.Time
	drainSince map[string]map[int32]time.Time
	timers     map[string]time.Time
	now        func() time.Time
	// Queue goroutine only: when each SingleAnnouncer address first had
	// two announcers, and whether its SplitBrain event was recorded.
	split         map[string]time.Time
	splitReported map[string]bool

	recorder record.EventRecorder

	egresses        listers.EgressLister
	clusterEgresses listers.ClusterEgressLister
	endpoints       listers.EgressEndpointsLister
	members         listers.EgressMembersLister
	nodeStates      listers.EgressNodeStateLister
	agents          listers.LBNodeAgentLister
	nodeStateIdx    cache.Indexer
	endpointsIdx    cache.Indexer
	membersIdx      cache.Indexer
	pods            cache.Indexer
	namespaces      cache.Indexer
	nodes           cache.Indexer
	services        cache.Indexer
	statefulSets    cache.Indexer
	leases          cache.Indexer
}

var (
	_ egresshook.Allocator  = (*Reconciler)(nil)
	_ egresshook.Controller = (*Reconciler)(nil)
)

// New returns a Reconciler for alloc; Start brings it up.
func New(logger log.Logger, alloc *coreallocator.Allocator, namespace string) *Reconciler {
	return &Reconciler{logger: logger, alloc: alloc, namespace: namespace, writeOwner: map[string]string{},
		behind: map[string]map[string]time.Time{}, drainSince: map[string]map[int32]time.Time{},
		timers: map[string]time.Time{}, now: time.Now, split: map[string]time.Time{}, splitReported: map[string]bool{}}
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

	if r.recorder == nil {
		sch := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(sch); err != nil {
			return err
		}
		if err := purelbscheme.AddToScheme(sch); err != nil {
			return err
		}
		b := record.NewBroadcaster()
		b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
		r.recorder = b.NewRecorder(sch, corev1.EventSource{Component: "purelb-egress"})
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
	lbna := pf.Purelb().V2().LBNodeAgents()
	ee := pns.Purelb().V2().EgressEndpointses()
	if err := ens.Informer().AddIndexers(cache.Indexers{idxAnnouncing: announcingIndex, idxClaims: claimsIndex}); err != nil {
		return err
	}
	if err := ee.Informer().AddIndexers(cache.Indexers{idxSliceNode: sliceNodeIndex}); err != nil {
		return err
	}
	em := pns.Purelb().V2().EgressMemberses()
	if err := em.Informer().AddIndexers(cache.Indexers{idxMemberPod: memberPodIndex}); err != nil {
		return err
	}
	pods := kf.Core().V1().Pods().Informer()
	if err := pods.SetTransform(k8s.SlimPod(purelbv2.EgressOptOutAnnotation)); err != nil {
		return err
	}
	nss := kf.Core().V1().Namespaces().Informer()
	nodes := kf.Core().V1().Nodes().Informer()
	svcs := kf.Core().V1().Services().Informer()
	stss := kf.Apps().V1().StatefulSets().Informer()
	if err := stss.SetTransform(slimStatefulSet); err != nil {
		return err
	}
	leases := kns.Coordination().V1().Leases().Informer()

	r.egresses, r.clusterEgresses, r.nodeStates, r.agents = eg.Lister(), ceg.Lister(), ens.Lister(), lbna.Lister()
	r.endpoints, r.members = ee.Lister(), em.Lister()
	r.nodeStateIdx, r.endpointsIdx, r.membersIdx = ens.Informer().GetIndexer(), ee.Informer().GetIndexer(), em.Informer().GetIndexer()
	r.pods, r.namespaces, r.nodes = pods.GetIndexer(), nss.GetIndexer(), nodes.GetIndexer()
	r.services, r.leases, r.statefulSets = svcs.GetIndexer(), leases.GetIndexer(), stss.GetIndexer()

	r.handle(eg.Informer(), r.policyOf)
	r.handle(ceg.Informer(), r.policyOf)
	r.handleEndpoints(ee.Informer())
	r.handle(em.Informer(), r.membersPolicies)
	r.handleNodeStates(ens.Informer())
	r.handle(lbna.Informer(), func(interface{}) []string { return r.allPolicies() })
	r.handle(pods, r.podPolicies)
	r.handle(nss, func(interface{}) []string { return r.clusterPolicies() })
	r.handle(nodes, func(obj interface{}) []string {
		keys := r.allPolicies()
		if n, ok := obj.(*corev1.Node); ok {
			keys = append(keys, nodePrefix+n.Name)
		}
		return keys
	})
	r.handle(svcs, r.servicePolicies)
	r.handle(stss, r.statefulSetPolicies)
	r.handleLeases(leases)

	pf.Start(stop)
	pns.Start(stop)
	kf.Start(stop)
	kns.Start(stop)
	go func() {
		if !cache.WaitForCacheSync(stop, eg.Informer().HasSynced, ceg.Informer().HasSynced, ens.Informer().HasSynced, lbna.Informer().HasSynced,
			ee.Informer().HasSynced, em.Informer().HasSynced, pods.HasSynced, nss.HasSynced, nodes.HasSynced,
			svcs.HasSynced, leases.HasSynced, stss.HasSynced) {
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

// membersPolicies are the policies an EgressMembers change can affect:
// its owner, and the lower-ranked policies its pods may now outrank, in
// its namespace (every policy for a ClusterEgress's).
func (r *Reconciler) membersPolicies(obj interface{}) []string {
	m, ok := obj.(*purelbv2.EgressMembers)
	if !ok {
		return nil
	}
	if m.Labels[egress.LabelKind] == egress.KindClusterEgress {
		return r.allPolicies()
	}
	keys := r.clusterPolicies()
	egs, _ := r.egresses.Egresses(m.Labels[egress.LabelNamespace]).List(labels.Everything())
	for _, e := range egs {
		keys = append(keys, r.policyOf(e)...)
	}
	return keys
}

// statefulSetPolicies are the policies a StatefulSet's scale can affect:
// the Egresses in its namespace and every ClusterEgress.
func (r *Reconciler) statefulSetPolicies(obj interface{}) []string {
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok {
		return nil
	}
	keys := r.clusterPolicies()
	egs, _ := r.egresses.Egresses(sts.Namespace).List(labels.Everything())
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
	return append(append(r.allPolicies(), r.nodeKeys()...), r.tombstoneKeys()...)
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
	if node, ok := strings.CutPrefix(key, nodePrefix); ok {
		return r.syncNodeState(node)
	}
	if name, ok := strings.CutPrefix(key, tombstonePrefix); ok {
		return r.expireTombstone(name)
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

// write submits a write of the object key, owned by the queue key owner.
func (r *Reconciler) write(owner, key string, priority bool, w func(context.Context) error) {
	r.writeOwner[key] = owner
	r.writer.submit(key, priority, w)
}

// writeDone collects a finished write's outcome. A failed write requeues
// its owner, which recomputes and rewrites it from fresh informer state.
func (r *Reconciler) writeDone(obj string) error {
	err, ok := r.writer.done(obj)
	if !ok {
		return nil
	}
	owner := r.writeOwner[obj]
	if !r.writer.inProgress(obj) {
		delete(r.writeOwner, obj)
	}
	if err != nil {
		logging.Info(r.logger, "op", "egressWrite", "object", obj, "error", err)
		if owner != "" {
			r.enqueue(owner, false)
		}
	}
	return nil
}

// reconcile brings one policy's selection, addresses, slices and status in
// line with its spec and the cluster (§5.1). Each stage writes only what
// changed.
func (r *Reconciler) reconcile(ref egress.PolicyRef) error {
	p, found, err := r.load(ref)
	if err != nil {
		return err
	}
	if !found {
		r.retireOrphans(ref)
		return nil
	}
	run := &run{p: p, conds: conditions{}}
	r.scope(run)
	deleting := p.meta.DeletionTimestamp != nil
	if deleting {
		run.revoked, run.retained = true, false
	}
	if !run.revoked && !run.retained {
		r.selectPods(run)
		policyEffective(run)
	}
	r.syncMembers(run)
	var addrs []address
	var eps map[string]*purelbv2.EgressEndpoints
	if !run.revoked && !run.retained {
		addrs = r.allocate(run)
		eps = r.syncEndpoints(run, addrs)
	}
	if !run.retained {
		r.syncFinalizer(run, r.retire(run, addrs), deleting || run.disallowed)
	}
	r.syncStatus(run, addrs, eps)
	logging.Debug(r.logger, "op", "egressReconcile", "policy", ref.String(),
		"selected", run.sel.selected, "revoked", run.revoked, "retained", run.retained)
	return nil
}
