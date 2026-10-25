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
	// capability is the datapath's last probe, nil before the first.
	// Written by the datapath goroutine, read by the queue goroutine.
	capability atomic.Pointer[purelbv2.EgressCapability]
	// pools are the pool interfaces' ifindexes, for the route watcher.
	// Written by the datapath goroutine.
	pools atomic.Pointer[map[int]bool]

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
}

var (
	_ egresshook.Agent      = (*Agent)(nil)
	_ egresshook.Controller = (*Agent)(nil)
)

// New returns the agent's egress controller for node; Start brings it up.
func New(logger log.Logger, node, namespace string) *Agent {
	a := &Agent{logger: logger, node: node, namespace: namespace, runner: newRunner(), now: time.Now,
		statusInterval: statusInterval, dp: &dpState{pinRoot: bpfpin.Dir}}
	a.apply = a.applyDatapath
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
	case keyDatapath:
		a.syncDatapath()
	}
	return nil
}

// Keys returns the agent's keys, for ForceSync. It runs on the caller's
// goroutine, so it reads no queue-goroutine state.
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
		a.status.disable()
	case !want && first:
		// Egress may have been on before this agent started.
		a.status.disable()
	}
}

// startSession starts egress's informers and its first capability probe.
func (a *Agent) startSession(spec purelbv2.LBNodeAgentEgressSpec) {
	s := &session{stop: make(chan struct{}), spec: spec}
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

	// The status is recomputed when this node's own objects change; the
	// datapath when any node's underlay addresses do (nodes map).
	mine := func(obj interface{}) {
		if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = t.Obj
		}
		if m, ok := obj.(metav1.Object); ok && m.GetName() == a.node {
			a.add(keyNode, false)
		}
	}
	_, _ = ens.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: mine,
		UpdateFunc: func(_, o interface{}) { mine(o) }, DeleteFunc: mine})
	nodeChanged := func(obj interface{}) {
		mine(obj)
		a.add(keyDatapath, false)
	}
	_, _ = nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: nodeChanged, DeleteFunc: nodeChanged,
		UpdateFunc: func(o, n interface{}) {
			if !equality.Semantic.DeepEqual(o.(*corev1.Node).Status.Addresses, n.(*corev1.Node).Status.Addresses) {
				nodeChanged(n)
			}
		}})

	own.Start(s.stop)
	kf.Start(s.stop)
	kick := func() { a.add(keyDatapath, false) }
	go watchRoutes(a.logger, s.stop, a.poolSet, kick)
	go watchTunnel(a.logger, s.stop, kick)
	go watchNft(a.logger, s.stop, kick)
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
		if !cache.WaitForCacheSync(s.stop, ens.Informer().HasSynced, nodes.HasSynced) {
			return
		}
		s.synced.Store(true)
		a.add(keyDatapath, false)
	}()
	a.on = s
	logging.Info(a.logger, "op", "egressEnable", "node", a.node, "msg", "egress enabled on this node")
}

// poolSet is the pool interfaces the datapath last mirrored.
func (a *Agent) poolSet() map[int]bool {
	if p := a.pools.Load(); p != nil {
		return *p
	}
	return nil
}

// stopSession stops egress's informers.
func (a *Agent) stopSession() {
	close(a.on.stop)
	a.on = nil
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
	a.status.set(&purelbv2.EgressNodeStateStatus{Capability: c})
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

// Referenced reports whether egress uses ip on this node.
func (a *Agent) Referenced(net.IP) bool { return false }

// ServiceAddress records a Service address this node announces.
func (a *Agent) ServiceAddress(string, net.IP, bool) {}

// PrepareShutdown runs before any address is withdrawn.
func (a *Agent) PrepareShutdown() {}

// apiContext bounds one API call, as internal/k8s does.
func apiContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}
