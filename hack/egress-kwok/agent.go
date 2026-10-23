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

package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
)

// shared is what the agents report to the driver.
type shared struct {
	c         config
	nodes     int
	policyEEs map[int][]string // set before the first CNI ADD

	leaseRenews, leaseLate, leaseMaxGap atomic.Int64
	statusWrites, writeErrors           atomic.Int64
	outstanding                         atomic.Int64 // claims not yet Active
	latCh                               chan float64 // claim latencies, drained by the phase
}

// drainLat takes every claim latency reported so far.
func (sh *shared) drainLat() []float64 {
	var out []float64
	for {
		select {
		case x := <-sh.latCh:
			out = append(out, x)
		default:
			return out
		}
	}
}

// agent simulates one node's lbnodeagent on the egress control plane
// (D6, D17): its Lease, its EgressNodeState status, a name watch of each
// address it announces and of its own EgressNodeState, and claims at CNI
// ADD. Its state belongs to its loop goroutine; everything else reaches
// it through ev.
type agent struct {
	i     int
	name  string
	sh    *shared
	kube  kubernetes.Interface // the election's own client (D-lease)
	pl    versioned.Interface
	ev    chan func()
	ctx   context.Context
	slice map[string]map[int32]string // own spec: endpoints -> index -> state

	specGen   int64
	acks      map[string]purelbv2.EgressAnnouncing
	claims    map[string]time.Time
	drainSeen map[purelbv2.EgressDrained]time.Time // Draining slices, when first seen
	drained   map[purelbv2.EgressDrained]bool
	dirty     bool
	urgent    bool
	lastWrite time.Time
}

func newAgent(ctx context.Context, i int, sh *shared, base *rest.Config) *agent {
	cfg := rest.CopyConfig(base)
	cfg.QPS, cfg.Burst = sh.c.QPS, sh.c.Burst
	elect := rest.CopyConfig(base)
	elect.QPS, elect.Burst = 5, 10
	return &agent{i: i, name: nodeName(i), sh: sh, ctx: ctx,
		kube: kubernetes.NewForConfigOrDie(elect), pl: versioned.NewForConfigOrDie(cfg),
		ev: make(chan func(), 1024), slice: map[string]map[int32]string{},
		acks: map[string]purelbv2.EgressAnnouncing{}, claims: map[string]time.Time{},
		drainSeen: map[purelbv2.EgressDrained]time.Time{}, drained: map[purelbv2.EgressDrained]bool{}}
}

func (ag *agent) start(ctx context.Context) error {
	_, err := ag.kube.CoordinationV1().Leases(nsPureLB).Create(ctx, lease(ag.name, ag.sh.c.LeaseDur, time.Now()), metav1.CreateOptions{})
	if ignoreExists(err) != nil {
		return err
	}
	_, err = ag.pl.PurelbV2().EgressNodeStates().Create(ctx, &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: ag.name}}, metav1.CreateOptions{})
	if ignoreExists(err) != nil {
		return err
	}
	ag.urgent = true // report capability at once
	go ag.renewLease(ctx)
	go ag.loop(ctx)
	ag.watch(ctx, "egressnodestates", "", ag.name, &purelbv2.EgressNodeState{}, func(o runtime.Object) {
		ag.onSpec(o.(*purelbv2.EgressNodeState))
	})
	return nil
}

// announce makes the agent an announcer of an address: it watches the
// address's EgressEndpoints by name and acks each generation (D6).
func (ag *agent) announce(ee string) {
	ag.watch(ag.ctx, "egressendpointses", nsPureLB, ee, &purelbv2.EgressEndpoints{}, func(o runtime.Object) {
		ep := o.(*purelbv2.EgressEndpoints)
		ag.acks[ep.Name] = purelbv2.EgressAnnouncing{Endpoints: ep.Name, Address: ep.Spec.Address, Generation: ep.Generation}
		ag.dirty = true
	})
}

// cniAdd is a selected pod of policy p starting on the node: claim a slice
// of each of its addresses the node has no Active slice of (D17).
func (ag *agent) cniAdd(p int) {
	now := time.Now()
	ag.ev <- func() {
		for _, ee := range ag.sh.policyEEs[p] {
			if _, open := ag.claims[ee]; open || ag.active(ee) {
				continue
			}
			ag.claims[ee] = now
			ag.sh.outstanding.Add(1)
			ag.urgent = true
		}
	}
}

func (ag *agent) active(ee string) bool {
	for _, st := range ag.slice[ee] {
		if st == "Active" {
			return true
		}
	}
	return false
}

// watch runs a name watch of one object; onObj runs on the loop goroutine.
func (ag *agent) watch(ctx context.Context, resource, ns, name string, typ runtime.Object, onObj func(runtime.Object)) {
	sel := fields.OneTermEqualSelector("metadata.name", name).String()
	c := ag.pl.PurelbV2()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			o.FieldSelector = sel
			if resource == "egressnodestates" {
				return c.EgressNodeStates().List(ctx, o)
			}
			return c.EgressEndpointses(ns).List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.FieldSelector = sel
			if resource == "egressnodestates" {
				return c.EgressNodeStates().Watch(ctx, o)
			}
			return c.EgressEndpointses(ns).Watch(ctx, o)
		},
	}
	inf := cache.NewSharedInformer(lw, typ, 0)
	h := func(o interface{}) {
		obj := o.(runtime.Object)
		select {
		case ag.ev <- func() { onObj(obj) }:
		case <-ctx.Done():
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: h, UpdateFunc: func(_, o interface{}) { h(o) }})
	go inf.RunWithContext(ctx)
}

// onSpec takes the node's own slices: claims complete when their slice is
// Active; a Draining slice is reported drained after the drain delay.
func (ag *agent) onSpec(ns *purelbv2.EgressNodeState) {
	ag.specGen = ns.Generation
	ag.slice = map[string]map[int32]string{}
	present := map[purelbv2.EgressDrained]bool{}
	for _, s := range ns.Spec.Slices {
		if ag.slice[s.Endpoints] == nil {
			ag.slice[s.Endpoints] = map[int32]string{}
		}
		ag.slice[s.Endpoints][s.Index] = s.State
		k := purelbv2.EgressDrained{Endpoints: s.Endpoints, Index: s.Index}
		present[k] = true
		if s.State == "Draining" {
			if _, ok := ag.drainSeen[k]; !ok {
				ag.drainSeen[k] = time.Now()
			}
		} else {
			delete(ag.drainSeen, k)
			delete(ag.drained, k)
		}
	}
	for k := range ag.drainSeen {
		if !present[k] {
			delete(ag.drainSeen, k)
			delete(ag.drained, k)
		}
	}
	for ee, t := range ag.claims {
		if ag.active(ee) {
			ag.sh.latCh <- time.Since(t).Seconds()
			ag.sh.outstanding.Add(-1)
			delete(ag.claims, ee)
		}
	}
	ag.dirty = true
}

func (ag *agent) loop(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-ag.ev:
			f()
		case <-t.C:
		}
		for k, since := range ag.drainSeen {
			if !ag.drained[k] && time.Since(since) >= ag.sh.c.DrainDelay {
				ag.drained[k] = true
				ag.dirty = true
			}
		}
		if ag.urgent || (ag.dirty && time.Since(ag.lastWrite) >= ag.sh.c.AckInterval) {
			ag.writeStatus(ctx)
		}
	}
}

// writeStatus writes the whole status: acks, claims, drained slices and
// capability, debounced to one write per ack interval except for claims.
func (ag *agent) writeStatus(ctx context.Context) {
	st := map[string]any{
		"observedGeneration": ag.specGen,
		"announcing":         []purelbv2.EgressAnnouncing{},
		"claims":             []purelbv2.EgressClaim{},
		"drained":            []purelbv2.EgressDrained{},
		"capability":         purelbv2.EgressCapability{EgressCapable: true},
	}
	var ann []purelbv2.EgressAnnouncing
	for _, a := range ag.acks {
		ann = append(ann, a)
	}
	var cl []purelbv2.EgressClaim
	for ee := range ag.claims {
		cl = append(cl, purelbv2.EgressClaim{Endpoints: ee})
	}
	var dr []purelbv2.EgressDrained
	for d := range ag.drained {
		dr = append(dr, d)
	}
	if ann != nil {
		st["announcing"] = ann
	}
	if cl != nil {
		st["claims"] = cl
	}
	if dr != nil {
		st["drained"] = dr
	}
	body, _ := json.Marshal(map[string]any{"status": st})
	ag.dirty, ag.urgent, ag.lastWrite = false, false, time.Now()
	ag.sh.statusWrites.Add(1)
	if _, err := ag.pl.PurelbV2().EgressNodeStates().Patch(ctx, ag.name, types.MergePatchType, body, metav1.PatchOptions{}, "status"); err != nil {
		if ctx.Err() == nil {
			ag.sh.writeErrors.Add(1)
			logf("agent %s status: %v", ag.name, err)
			ag.dirty = true
		}
	}
}

// renewLease renews the node's Lease every half lease duration through the
// election's own client, recording gaps beyond the lease duration.
func (ag *agent) renewLease(ctx context.Context) {
	t := time.NewTicker(ag.sh.c.LeaseDur / 2)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := ag.kube.CoordinationV1().Leases(nsPureLB).Patch(ctx, purelbv2.LeasePrefix+ag.name, types.MergePatchType,
			renewPatch(ag.name, time.Now()), metav1.PatchOptions{})
		if err != nil {
			if ctx.Err() == nil {
				logf("lease %s: %v", ag.name, err)
			}
			continue
		}
		now := time.Now()
		gap := now.Sub(last)
		last = now
		ag.sh.leaseRenews.Add(1)
		if gap > ag.sh.c.LeaseDur {
			ag.sh.leaseLate.Add(1)
		}
		for old := ag.sh.leaseMaxGap.Load(); int64(gap) > old && !ag.sh.leaseMaxGap.CompareAndSwap(old, int64(gap)); old = ag.sh.leaseMaxGap.Load() {
		}
	}
}
