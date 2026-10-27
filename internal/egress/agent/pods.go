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

package agent

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/egress"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/informers/externalversions"
)

// podNodeInformers are the pod-node stage's informers, to start and wait
// for with the session's.
type podNodeInformers struct {
	start  []func(<-chan struct{})
	synced []cache.InformerSynced
}

// podNodeInformers sets up the pod-node stage's informers on s: this
// node's pods only (field selector, §11.3), and the policies, namespaces
// and Services its pods are selected by. Any change recomputes the
// pod-node state.
func (a *Agent) podNodeInformers(s *session, kf kubeinformers.SharedInformerFactory) (podNodeInformers, error) {
	kick := func() { a.add(keyPods, false) }
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { kick() },
		UpdateFunc: func(_, _ interface{}) { kick() },
		DeleteFunc: func(interface{}) { kick() },
	}
	pf := kubeinformers.NewSharedInformerFactoryWithOptions(a.kube, resync,
		kubeinformers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", a.node).String()
		}))
	pods := pf.Core().V1().Pods().Informer()
	if err := pods.SetTransform(k8s.SlimPod(purelbv2.EgressOptOutAnnotation)); err != nil {
		return podNodeInformers{}, err
	}
	nss := kf.Core().V1().Namespaces().Informer()
	if err := nss.SetTransform(slimNamespace); err != nil {
		return podNodeInformers{}, err
	}
	svcs := kf.Core().V1().Services().Informer()
	if err := svcs.SetTransform(slimService); err != nil {
		return podNodeInformers{}, err
	}
	ef := externalversions.NewSharedInformerFactory(a.purelb, resync)
	egs := ef.Purelb().V2().Egresses()
	cegs := ef.Purelb().V2().ClusterEgresses()
	// Updates count only where they can change selection: Services and
	// namespaces update often for reasons selection never reads.
	changed := func(read func(interface{}) interface{}) cache.ResourceEventHandlerFuncs {
		h := handler
		h.UpdateFunc = func(o, n interface{}) {
			if !equality.Semantic.DeepEqual(read(o), read(n)) {
				kick()
			}
		}
		return h
	}
	_, _ = pods.AddEventHandler(changed(func(o interface{}) interface{} {
		p := o.(*corev1.Pod)
		return []interface{}{p.Labels, p.Annotations, p.Status.Phase, p.Status.PodIPs, p.DeletionTimestamp == nil}
	}))
	_, _ = nss.AddEventHandler(changed(func(o interface{}) interface{} { return o.(*corev1.Namespace).Labels }))
	_, _ = svcs.AddEventHandler(changed(func(o interface{}) interface{} { return o.(*corev1.Service).Spec.Selector }))
	for _, inf := range []cache.SharedIndexInformer{egs.Informer(), cegs.Informer()} {
		_, _ = inf.AddEventHandler(handler)
	}
	s.pods, s.namespaces, s.services = pods.GetIndexer(), nss.GetIndexer(), svcs.GetIndexer()
	s.egresses, s.clusterEgresses = egs.Lister(), cegs.Lister()
	inf := podNodeInformers{
		start:  []func(<-chan struct{}){pf.Start, ef.Start},
		synced: []cache.InformerSynced{pods.HasSynced, nss.HasSynced, svcs.HasSynced, egs.Informer().HasSynced, cegs.Informer().HasSynced},
	}

	// The cluster networks (design 6.3): the Service CIDRs, and the API
	// server's addresses, which may lie outside every cluster CIDR.
	kickDP := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { a.add(keyDatapath, false) },
		UpdateFunc: func(_, _ interface{}) { a.add(keyDatapath, false) },
		DeleteFunc: func(interface{}) { a.add(keyDatapath, false) },
	}
	af := kubeinformers.NewSharedInformerFactoryWithOptions(a.kube, resync, kubeinformers.WithNamespace(corev1.NamespaceDefault),
		kubeinformers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = discoveryv1.LabelServiceName + "=kubernetes"
		}))
	eps := af.Discovery().V1().EndpointSlices().Informer()
	_, _ = eps.AddEventHandler(kickDP)
	s.apiServer = eps.GetIndexer()
	inf.start = append(inf.start, af.Start)
	inf.synced = append(inf.synced, eps.HasSynced)
	if serviceCIDRsServed(a.kube) {
		cidrs := kf.Networking().V1().ServiceCIDRs().Informer()
		_, _ = cidrs.AddEventHandler(kickDP)
		s.serviceCIDRs = cidrs.GetIndexer()
		inf.synced = append(inf.synced, cidrs.HasSynced)
	} else {
		logging.Info(a.logger, "op", "egressStart", "node", a.node,
			"msg", "the API serves no ServiceCIDRs (networking.k8s.io/v1): list the Service CIDRs in the LBNodeAgent's excludedNetworks")
	}
	return inf, nil
}

// serviceCIDRsServed reports whether the API serves networking.k8s.io/v1
// ServiceCIDRs (Kubernetes 1.33 and later).
func serviceCIDRsServed(kube kubernetes.Interface) bool {
	res, err := kube.Discovery().ServerResourcesForGroupVersion(networkingv1.SchemeGroupVersion.String())
	if err != nil {
		return false
	}
	for _, r := range res.APIResources {
		if r.Name == "servicecidrs" {
			return true
		}
	}
	return false
}

// syncPods recomputes the pod-node state.
func (a *Agent) syncPods() {
	s := a.on
	if s == nil || !s.synced.Load() {
		return
	}
	in := a.podNodeInput(s)
	prev := a.podnode
	if prev == nil {
		// The session's first plan: keep the pod-set ids an earlier
		// agent left in the pinned maps (D12).
		prev = seedPodSets(in, recoveredPodSets(a.dp.pinRoot))
	}
	pn := planPodNode(in, prev)
	if len(pn.short) > 0 && (a.podnode == nil || !slices.Equal(a.podnode.short, pn.short)) {
		logging.Info(a.logger, "op", "egressPods", "node", a.node, "used", pn.used(), "capacity", slotBudget,
			"short", pn.short, "msg", "slot budget exhausted: these get no slot")
	}
	a.podnode = pn
	logging.Debug(a.logger, "op", "egressPods", "node", a.node, "slots", len(pn.slots), "podSets", len(pn.sets),
		"pods", len(pn.pods), "retired", len(pn.retired))
	a.add(keyNode, false) // capability.slots and SlotsExhausted
	if pods := dpPods(pn); !slices.Equal(pods, a.podsWant) || a.podsWant == nil {
		a.podsWant = pods
		a.add(keyDatapath, false)
	}
}

// podNodeInput gathers planPodNode's input from the session's caches.
func (a *Agent) podNodeInput(s *session) pnInput {
	d := a.config.Load()
	in := pnInput{namespaces: map[string]*corev1.Namespace{}, owners: map[string]egress.PolicyRef{},
		reservedCluster: int(s.spec.ReservedClusterSlots)}
	var groups []*purelbv2.ServiceGroup
	if d != nil {
		for _, g := range d.groups {
			groups = append(groups, g)
		}
	}
	in.bindings = purelbv2.BindNamespaces(groups)
	if ns, err := s.nodeStates.Get(a.node); err == nil {
		in.spec = ns.Spec
	}
	for _, o := range s.pods.List() {
		if pod := o.(*corev1.Pod); pod.Spec.NodeName == a.node {
			in.pods = append(in.pods, pod)
		}
	}
	for _, o := range s.namespaces.List() {
		ns := o.(*corev1.Namespace)
		in.namespaces[ns.Name] = ns
	}

	// A policy is in force on this node while it holds an Active address;
	// each address names its policy and resolved group.
	eas, _ := s.addresses.List(labels.Everything())
	groupOf := map[egress.PolicyRef]string{}
	for _, ea := range eas {
		ref := egress.PolicyRef{Kind: ea.Labels[egress.LabelKind], Namespace: ea.Labels[egress.LabelNamespace],
			Name: ea.Labels[egress.LabelName]}
		in.owners[ea.Name] = ref
		if ea.Spec.State == "Active" {
			groupOf[ref] = ea.Spec.ServiceGroup
		}
	}
	for ref, group := range groupOf {
		var sg *purelbv2.ServiceGroup
		if d != nil {
			sg = d.groups[group]
		}
		if p := a.pnPolicy(s, ref, group, sg); p != nil {
			in.policies = append(in.policies, p)
		}
	}
	return in
}

// pnPolicy loads policy ref for a pod node, nil when it is gone or its
// selector can't be used.
func (a *Agent) pnPolicy(s *session, ref egress.PolicyRef, group string, sg *purelbv2.ServiceGroup) *pnPolicy {
	if ref.Kind == egress.KindClusterEgress {
		c, err := s.clusterEgresses.Get(ref.Name)
		if err != nil {
			return nil
		}
		pods, err1 := metav1.LabelSelectorAsSelector(&c.Spec.PodSelector)
		nss, err2 := metav1.LabelSelectorAsSelector(&c.Spec.NamespaceSelector)
		if err1 != nil || err2 != nil {
			return nil
		}
		prio, _ := egress.EffectivePriority(c.Spec.Priority, true, sg)
		return &pnPolicy{ref: ref, rank: egress.Rank{Priority: prio, Created: c.CreationTimestamp.Time, UID: string(c.UID)},
			spec: c.Spec.EgressPolicy, optOut: c.Spec.OptOut, pods: pods, namespaces: nss,
			admittedOnly: c.Spec.NamespaceScope == "AdmittedOnly", group: group, sg: sg}
	}
	e, err := s.egresses.Egresses(ref.Namespace).Get(ref.Name)
	if err != nil {
		return nil
	}
	var pods labels.Selector
	switch {
	case e.Spec.ServiceRef != nil:
		obj, ok, _ := s.services.GetByKey(e.Namespace + "/" + e.Spec.ServiceRef.Name)
		if !ok || len(obj.(*corev1.Service).Spec.Selector) == 0 {
			return nil
		}
		pods = labels.SelectorFromSet(obj.(*corev1.Service).Spec.Selector)
	case e.Spec.PodSelector != nil:
		if pods, err = metav1.LabelSelectorAsSelector(e.Spec.PodSelector); err != nil {
			return nil
		}
	default:
		return nil
	}
	prio, _ := egress.EffectivePriority(e.Spec.Priority, false, sg)
	return &pnPolicy{ref: ref, rank: egress.Rank{Priority: prio, Created: e.CreationTimestamp.Time, UID: string(e.UID)},
		spec: e.Spec.EgressPolicy, optOut: e.Spec.OptOut, pods: pods, group: group, sg: sg}
}

// slimNamespace is a Namespace informer transform keeping its name and
// labels.
func slimNamespace(obj interface{}) (interface{}, error) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return obj, nil
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name, Labels: ns.Labels, ResourceVersion: ns.ResourceVersion}}, nil
}

// slimService is a Service informer transform keeping its name and
// selector.
func slimService(obj interface{}) (interface{}, error) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return obj, nil
	}
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace, ResourceVersion: svc.ResourceVersion},
		Spec: corev1.ServiceSpec{Selector: svc.Spec.Selector}}, nil
}
