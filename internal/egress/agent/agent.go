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

// Package agent is the node agent's side of egress. Its control work runs
// on the agent's work queue (D-conc); kernel work runs on one datapath
// goroutine fed by the queue goroutine. The agent reaches it only through
// egresshook.
package agent

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egresshook"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
	"purelb.io/pkg/generated/informers/externalversions"
	listers "purelb.io/pkg/generated/listers/purelb/v2"
)

// resync is the informers' resync period.
const resync = 10 * time.Minute

// Queue keys.
const (
	// keyConfig turns egress on or off after a config delivery.
	keyConfig = "agent:config"
	// keyNode recomputes this node's EgressNodeState status.
	keyNode = "agent:node"
	// keyPods recomputes the pod-node state: slots, pod-sets and this
	// node's pods.
	keyPods = "agent:pods"
)

// Agent is the node agent's egress controller. Its state belongs to the
// agent's queue goroutine, which calls Start and Sync; Configure runs on
// the CR-controller goroutine and the informers' handlers only enqueue,
// so what they share is atomic.
type Agent struct {
	logger    log.Logger
	node      string
	namespace string // PureLB's

	// config is the latest delivery's egress spec for this node, nil
	// until the first delivery. Written by Configure.
	config atomic.Pointer[delivery]
	// enqueue adds a key to the work queue; nil until Start.
	enqueue atomic.Pointer[func(string, bool)]

	// crds is whether the egress CRDs exist. Set in Start.
	crds   bool
	kube   kubernetes.Interface
	purelb versioned.Interface

	// on is the running egress session, nil while egress is off on this
	// node. Queue goroutine only.
	on *session
	// configured is whether a config delivery has been acted on. Queue
	// goroutine only.
	configured bool

	// elect is the node's Lease election, set before Start.
	elect Elector

	status *statusWriter
	runner *runner
	// dp is the datapath goroutine's state; apply brings it in line with
	// a desired state (tests replace it).
	dp    *dpState
	apply func(dpWant) (*purelbv2.EgressCapability, error)
	// tear removes the datapath (D26; tests replace it).
	tear func() error
	// capability is the datapath's last probe, nil before the first.
	// Written by the datapath goroutine, read by the queue goroutine.
	capability atomic.Pointer[purelbv2.EgressCapability]
	// pools are the pool interfaces' ifindexes, for the route watcher.
	// Written by the datapath goroutine.
	pools atomic.Pointer[map[int]bool]
	// acks are the EgressEndpoints generations the datapath last
	// installed, for status.announcing (D6). Written by the datapath
	// goroutine.
	acks atomic.Pointer[[]purelbv2.EgressAnnouncing]

	// guard is the node's address guard, nil without one; guarded the
	// addresses egress has put in it. Queue goroutine only.
	guard   Guard
	guarded map[netip.Addr]bool
	// services are the Service addresses this node announces, with the
	// Services announcing each (Inherit). Queue goroutine only.
	services map[netip.Addr]map[string]bool

	// placer places dedicated addresses (the announcer). placed are the
	// ones placed, shared the held shared ones (placed by their Service),
	// dadFailed the placed ones logged as failing DAD; partitionTimer
	// syncs when the lease would count as partitioned. Queue goroutine
	// only.
	placer         egresshook.Placer
	placed         map[netip.Addr]placement
	shared         map[netip.Addr]bool
	dadFailed      map[netip.Addr]*dadBackoff
	partitionTimer *time.Timer
	// installed are the addresses the datapath last installed held.
	// Written by the datapath goroutine.
	installed atomic.Pointer[map[netip.Addr]bool]
	// placedSet is placed's addresses, for the address watcher. Written
	// by the queue goroutine.
	placedSet atomic.Pointer[map[netip.Addr]bool]
	// dadLost are placed addresses the kernel deleted while tentative:
	// DAD failed. Stored by the address watcher.
	dadLost sync.Map // netip.Addr: true
	// addrFlags lists the node's addresses and their IFA flags.
	addrFlags func() (map[netip.Addr]int, error)

	// podnode is the pod-node state last planned. Queue goroutine only.
	podnode *podNode

	// faults are this node's address problems and conds its conditions,
	// as last reported. Queue goroutine only.
	faults map[netip.Addr]purelbv2.EgressAddressFault
	conds  []metav1.Condition
	// health is what the datapath last found. Written by the datapath
	// goroutine.
	health atomic.Pointer[dpHealth]
	// probing are the tunnel peers being probed (runProbes); sendProbe
	// sends one probe (tests replace it).
	probing   atomic.Pointer[map[netip.Addr]probeTarget]
	sendProbe func(from, to netip.Addr, port int, pkt []byte) error

	now func() time.Time
	// statusInterval is the status writer's interval; tests shorten it.
	statusInterval time.Duration
}

// delivery is what egress takes from one config delivery: this node's
// egress spec, nil when no LBNodeAgent for this node has one, and the
// ServiceGroups by name. Read-only once stored.
type delivery struct {
	spec   *purelbv2.LBNodeAgentEgressSpec
	groups map[string]*purelbv2.ServiceGroup
}

// session is what runs while egress is enabled on this node.
type session struct {
	stop       chan struct{}
	spec       purelbv2.LBNodeAgentEgressSpec
	synced     atomic.Bool
	nodeStates listers.EgressNodeStateLister
	nodes      cache.Indexer
	// addresses are every egress address (EgressAddress); watches the
	// scoped EgressEndpoints watches (D6). Queue goroutine only.
	addresses listers.EgressAddressNamespaceLister
	watches   map[string]*eeWatch
	// For the pod-node stage: this node's pods, the policies, namespaces
	// and Services (serviceRef selectors).
	pods            cache.Indexer
	egresses        listers.EgressLister
	clusterEgresses listers.ClusterEgressLister
	namespaces      cache.Indexer
	services        cache.Indexer
}

var (
	_ egresshook.Agent      = (*Agent)(nil)
	_ egresshook.Controller = (*Agent)(nil)
)

// New returns the agent's egress controller for node; Start brings it up.
func New(logger log.Logger, node, namespace string) *Agent {
	a := &Agent{logger: logger, node: node, namespace: namespace, runner: newRunner(), now: time.Now,
		statusInterval: statusInterval, dp: &dpState{pinRoot: bpfpin.Dir},
		guarded: map[netip.Addr]bool{}, services: map[netip.Addr]map[string]bool{},
		placed: map[netip.Addr]placement{}, dadFailed: map[netip.Addr]*dadBackoff{}, addrFlags: listAddrFlags,
		sendProbe: sendUDPProbe}
	a.apply, a.tear = a.applyDatapath, a.teardown
	return a
}

// Configure takes a config delivery, its agents already filtered to this
// node in precedence order: the egress spec of the first LBNodeAgent that
// has one (the rule the allocator's slot budget uses), and the
// ServiceGroups. It runs on the CR-controller goroutine.
func (a *Agent) Configure(cfg *purelbv2.Config) {
	d := &delivery{groups: map[string]*purelbv2.ServiceGroup{}, spec: purelbv2.EgressSpecFor(cfg.Agents).DeepCopy()}
	for _, g := range cfg.Groups {
		d.groups[g.Name] = g
	}
	a.config.Store(d)
	a.add(keyConfig, false)
	a.add(keyPods, false) // groups: scope and priority ceilings
}

// add enqueues key once Start has run; Start enqueues keyConfig itself.
func (a *Agent) add(key string, priority bool) {
	if f := a.enqueue.Load(); f != nil {
		(*f)(key, priority)
	}
}

// Start discovers the egress CRDs. Without them egress stays off on this
// node whatever its LBNodeAgent says.
func (a *Agent) Start(stop <-chan struct{}, cfg *rest.Config, enqueue func(string, bool)) error {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	purelb, err := versioned.NewForConfig(cfg)
	if err != nil {
		return err
	}
	return a.start(stop, kube, purelb, enqueue)
}

// start is Start with the clients built.
func (a *Agent) start(stop <-chan struct{}, kube kubernetes.Interface, purelb versioned.Interface, enqueue func(string, bool)) error {
	a.kube, a.purelb = kube, purelb
	var err error
	a.crds, err = crdsInstalled(kube)
	if err != nil {
		logging.Info(a.logger, "op", "egressStart", "error", err,
			"msg", "could not discover the egress CRDs; egress is off until the agent restarts")
		return nil
	}
	if !a.crds {
		logging.Info(a.logger, "op", "egressStart", "msg", "egress CRDs not installed; egress is off")
		return nil
	}
	a.status = newStatusWriter(a.logger, a.node, kube, purelb)
	a.status.interval = a.statusInterval
	go a.status.run(stop)
	go a.runner.run(stop)
	a.enqueue.Store(&enqueue)
	enqueue(keyConfig, false)
	return nil
}

// crdsInstalled reports whether the API server serves EgressNodeState.
func crdsInstalled(kube kubernetes.Interface) (bool, error) {
	res, err := kube.Discovery().ServerResourcesForGroupVersion(purelbv2.SchemeGroupVersion.String())
	if err != nil {
		return false, err
	}
	for _, r := range res.APIResources {
		if r.Name == "egressnodestates" {
			return true, nil
		}
	}
	return false, nil
}

// Sync processes one egress key on the agent's queue goroutine.
func (a *Agent) Sync(key string) error {
	if !a.crds {
		return nil
	}
	switch key {
	case keyConfig:
		a.syncConfig()
	case keyNode:
		a.syncNode()
	case keyPods:
		a.syncPods()
	case keyDatapath:
		a.syncDatapath()
	case keyTeardown:
		a.syncTeardown()
	}
	return nil
}

// Keys returns the agent's keys, for ForceSync. It runs on the caller's
// goroutine, so it reads no queue-goroutine state. ForceSync follows Lease
// membership, which the pod-node state doesn't read: its own informers
// and Configure queue keyPods.
func (a *Agent) Keys() []string {
	return []string{keyConfig, keyNode, keyDatapath}
}

// Live fails once one datapath item has run for longer than
// livenessStuckAfter.
func (a *Agent) Live() error {
	return a.runner.live(a.now())
}

// syncConfig turns egress on or off to match the latest delivery (D-idle:
// with egress off nothing egress runs, not even an informer).
func (a *Agent) syncConfig() {
	d := a.config.Load()
	if d == nil {
		return // no delivery yet
	}
	want := d.spec != nil && d.spec.Enabled
	first := !a.configured
	a.configured = true
	switch {
	case want && a.on == nil:
		a.startSession(*d.spec)
	case want && !equality.Semantic.DeepEqual(a.on.spec, *d.spec):
		a.on.spec = *d.spec
		a.syncDatapath()
	case !want && a.on != nil:
		a.stopSession()
		a.endDatapath()
	case !want && first:
		// Egress may have been on before this agent started: remove
		// what an earlier agent left (D26), if anything.
		a.endDatapath()
	}
}

// startSession starts egress's informers and its first capability probe.
func (a *Agent) startSession(spec purelbv2.LBNodeAgentEgressSpec) {
	s := &session{stop: make(chan struct{}), spec: spec, watches: map[string]*eeWatch{}}
	own := externalversions.NewSharedInformerFactoryWithOptions(a.purelb, resync,
		externalversions.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", a.node).String()
		}))
	kf := kubeinformers.NewSharedInformerFactory(a.kube, resync)
	ens := own.Purelb().V2().EgressNodeStates()
	nodes := kf.Core().V1().Nodes().Informer()
	if err := nodes.SetTransform(slimNode); err != nil {
		logging.Info(a.logger, "op", "egressStart", "error", err)
		return
	}
	s.nodeStates, s.nodes = ens.Lister(), nodes.GetIndexer()
	// Every egress address, on every egress node (§6.10, D6).
	pf := externalversions.NewSharedInformerFactoryWithOptions(a.purelb, resync, externalversions.WithNamespace(a.namespace))
	eas := pf.Purelb().V2().EgressAddresses()
	s.addresses = eas.Lister().EgressAddresses(a.namespace)
	kickDP := func(interface{}) {
		a.add(keyDatapath, false)
		a.add(keyPods, false) // which policies are in force
	}
	_, _ = eas.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: kickDP, DeleteFunc: kickDP,
		UpdateFunc: func(_, o interface{}) { kickDP(o) }})

	// The status is recomputed when this node's own objects change; the
	// datapath when any node's underlay addresses (nodes map) or labels
	// (eligibility) do.
	mine := func(obj interface{}) {
		if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = t.Obj
		}
		if m, ok := obj.(metav1.Object); ok && m.GetName() == a.node {
			a.add(keyNode, false)
		}
	}
	// The spec (this node's slices) feeds the pod-node state; the status
	// is the agent's own writes.
	minePods := func(obj interface{}) {
		mine(obj)
		a.add(keyPods, false)
	}
	_, _ = ens.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: minePods, DeleteFunc: minePods,
		UpdateFunc: func(o, n interface{}) {
			if !equality.Semantic.DeepEqual(o.(*purelbv2.EgressNodeState).Spec, n.(*purelbv2.EgressNodeState).Spec) {
				minePods(n)
				return
			}
			mine(n)
		}})
	nodeChanged := func(obj interface{}) {
		mine(obj)
		a.add(keyDatapath, false)
	}
	// A node's labels decide its eligibility (the D29 label and
	// announcerNodeSelector, §5.5), so a label change re-runs the holder
	// decision as an address change does.
	_, _ = nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: nodeChanged, DeleteFunc: nodeChanged,
		UpdateFunc: func(o, n interface{}) {
			on, nn := o.(*corev1.Node), n.(*corev1.Node)
			if !equality.Semantic.DeepEqual(on.Status.Addresses, nn.Status.Addresses) ||
				!equality.Semantic.DeepEqual(on.Labels, nn.Labels) {
				nodeChanged(n)
			}
		}})

	podInformers, err := a.podNodeInformers(s, kf)
	if err != nil {
		logging.Info(a.logger, "op", "egressStart", "error", err)
		return
	}

	own.Start(s.stop)
	kf.Start(s.stop)
	pf.Start(s.stop)
	for _, start := range podInformers.start {
		start(s.stop)
	}
	kick := func() { a.add(keyDatapath, false) }
	go watchRoutes(a.logger, s.stop, a.poolSet, kick)
	go watchTunnel(a.logger, s.stop, kick)
	go watchNft(a.logger, s.stop, kick)
	go watchAddrs(a.logger, s.stop, a.placedAddrs, func(addr netip.Addr) { a.dadLost.Store(addr, true) }, kick)
	go a.runProbes(s)
	go func() {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				kick()
			}
		}
	}()
	go func() {
		synced := append([]cache.InformerSynced{ens.Informer().HasSynced, nodes.HasSynced, eas.Informer().HasSynced},
			podInformers.synced...)
		if !cache.WaitForCacheSync(s.stop, synced...) {
			return
		}
		s.synced.Store(true)
		a.add(keyDatapath, false)
		a.add(keyPods, false)
	}()
	a.on = s
	logging.Info(a.logger, "op", "egressEnable", "node", a.node, "msg", "egress enabled on this node")
}

// placedAddrs is the addresses egress last placed.
func (a *Agent) placedAddrs() map[netip.Addr]bool {
	if p := a.placedSet.Load(); p != nil {
		return *p
	}
	return nil
}

// poolSet is the pool interfaces the datapath last mirrored.
func (a *Agent) poolSet() map[int]bool {
	if p := a.pools.Load(); p != nil {
		return *p
	}
	return nil
}

// stopSession stops egress's informers and takes its addresses out of the
// guard.
func (a *Agent) stopSession() {
	for _, w := range a.on.watches {
		close(w.stop)
	}
	close(a.on.stop)
	a.on = nil
	if a.guard != nil {
		for addr := range a.guarded {
			if err := a.guard.UpdateEgress(addr, false, nil, nil); err != nil {
				logging.Info(a.logger, "op", "egressDisable", "node", a.node, "address", addr, "error", err)
			}
		}
	}
	a.guarded = map[netip.Addr]bool{}
	a.withdrawPlaced("egress disabled")
	a.shared, a.faults, a.conds, a.podnode = nil, nil, nil, nil
	a.installed.Store(nil)
	a.probing.Store(nil)
	a.acks.Store(nil)
	logging.Info(a.logger, "op", "egressDisable", "node", a.node, "msg", "egress disabled on this node")
}

// syncNode hands this node's EgressNodeState status to the status writer.
func (a *Agent) syncNode() {
	s := a.on
	if s == nil || !s.synced.Load() {
		return
	}
	c := a.capability.Load()
	if c == nil {
		return // first probe still running
	}
	c = c.DeepCopy()
	c.Underlay = underlay(s.nodes, a.node)
	c.Slots = &purelbv2.EgressSlots{Capacity: slotBudget}
	if pn := a.podnode; pn != nil {
		c.Slots.Used, c.Slots.PodSets = int32(pn.used()), int32(len(pn.sets))
	}
	st := &purelbv2.EgressNodeStateStatus{Capability: c, AddressFaults: a.faultList(),
		Conditions: a.nodeConditions(c, a.health.Load(), a.podnode)}
	if acks := a.acks.Load(); acks != nil {
		// Only what this node announces now: an address it holds but
		// couldn't place isn't announced, and gates nothing (D6).
		for _, ack := range *acks {
			addr, err := netip.ParseAddr(ack.Address)
			if _, placed := a.placed[addr]; err == nil && (placed || a.shared[addr]) {
				st.Announcing = append(st.Announcing, ack)
			}
		}
	}
	a.status.set(st)
}

// underlay is the node's InternalIPs, one per family (D3).
func underlay(nodes cache.Indexer, name string) *purelbv2.EgressUnderlay {
	obj, ok, _ := nodes.GetByKey(name)
	if !ok {
		return nil
	}
	u := &purelbv2.EgressUnderlay{}
	for _, a := range obj.(*corev1.Node).Status.Addresses {
		ip := net.ParseIP(a.Address)
		if a.Type != corev1.NodeInternalIP || ip == nil {
			continue
		}
		if ip.To4() != nil && u.IPv4 == "" {
			u.IPv4 = a.Address
		} else if ip.To4() == nil && u.IPv6 == "" {
			u.IPv6 = a.Address
		}
	}
	if *u == (purelbv2.EgressUnderlay{}) {
		return nil
	}
	return u
}

// slimNode is a Node informer transform keeping what egress reads: name,
// UID, labels and addresses.
func slimNode(obj interface{}) (interface{}, error) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: n.Name, UID: n.UID, Labels: n.Labels, ResourceVersion: n.ResourceVersion},
		Status:     corev1.NodeStatus{Addresses: n.Status.Addresses},
	}, nil
}

// PrepareShutdown runs at the start of a graceful shutdown, after the
// work-queue goroutine has stopped: egress withdraws the addresses it
// placed while the lease still stands (design 5.5; the repoint ahead of it
// is Phase 5's).
func (a *Agent) PrepareShutdown() {
	if a.on == nil {
		return
	}
	a.withdrawPlaced("shutdown")
}

// apiContext bounds one API call, as internal/k8s does.
func apiContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}
