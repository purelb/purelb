package main

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// A simulated node agent. One event-loop goroutine owns all agent state; the
// watch goroutine and the CNI-ADD simulation feed it through channels, and a
// writer goroutine performs the EgressNodeState status write so that the
// loop never blocks on the rate limiter.

type agentShared struct {
	design       string
	ackInterval  time.Duration // decided: debounce (min interval between ack writes)
	origInterval time.Duration // original: 0 = write whenever any generation changes
	policyUIDs   []string
	addrs        [][]string // [policy][addr] address strings
	rxBytes      atomic.Int64
	ackWrites    atomic.Int64
	claimWrites  atomic.Int64
	writeErrors  atomic.Int64
	leaseRenews  atomic.Int64
	leaseLate    atomic.Int64 // renewal gap > lease duration
	leaseMaxGap  atomic.Int64 // ns
	readyAgents  atomic.Int64 // agents that completed their first ack write
	leaseDur     time.Duration
	leaseRenew   time.Duration
	// option C: nodes 0..selK-1 are the selector; with routedWatch each
	// also watches every host-route slice table and acks it.
	selK         int
	routedWatch  bool
	selAckWrites atomic.Int64
	selRxEvents  atomic.Int64
	selRxBytes   atomic.Int64
}

type agentObj struct {
	Metadata struct {
		Name       string            `json:"name"`
		Generation int64             `json:"generation"`
		Labels     map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Epoch     int64 `json:"epoch"`
		Addresses []struct {
			Slices []struct {
				Node  string `json:"node"`
				State string `json:"state"`
			} `json:"slices"`
		} `json:"addresses"`
	} `json:"spec"`
}

type pa struct{ p, a int }

type agent struct {
	idx    int
	name   string
	sh     *agentShared
	c      *client
	evCh   chan []agentObj
	cniCh  chan int
	doneCh chan bool
	wrCh   chan []byte
	ndCh   chan scNodeObj // scoped: this node's own EgressNodeState
}

// scNodeObj is the part of a node's own EgressNodeState the scoped agent reads.
type scNodeObj struct {
	Metadata struct {
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec scNodeSpec `json:"spec"`
}

func newAgent(idx int, name string, sh *agentShared, c *client) *agent {
	return &agent{
		idx: idx, name: name, sh: sh, c: c,
		evCh: make(chan []agentObj, 4096), cniCh: make(chan int, 1024),
		doneCh: make(chan bool, 1), wrCh: make(chan []byte, 1), ndCh: make(chan scNodeObj, 1024),
	}
}

func (ag *agent) statusPath() string {
	if ag.sh.design == "orig" {
		return gv + "/origegressnodestates/" + ag.name + "/status"
	}
	return gv + "/egressnodestates/" + ag.name + "/status"
}

func (ag *agent) start(ctx context.Context) {
	if ag.sh.design == "none" {
		// baseline: only the Lease renewal, as an LB-only lbnodeagent; CNI
		// ADDs are consumed and ignored.
		go ag.renewLease(ctx)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-ag.cniCh:
				}
			}
		}()
		return
	}
	if ag.sh.design == "scoped" {
		ag.startScoped(ctx)
		return
	}
	go ag.loop(ctx)
	go ag.writer(ctx)
	go ag.renewLease(ctx)
	path := gv + "/namespaces/" + nsPureLB + "/egressendpoints"
	if ag.sh.design == "orig" {
		path = gv + "/origegressendpoints"
	}
	decode := func(raw json.RawMessage) (agentObj, bool) {
		var o agentObj
		if err := json.Unmarshal(raw, &o); err != nil {
			return o, false
		}
		return o, true
	}
	go ag.c.watchLoop(ctx, path, nil,
		func(items []json.RawMessage) {
			objs := make([]agentObj, 0, len(items))
			for _, it := range items {
				if o, ok := decode(it); ok {
					objs = append(objs, o)
				}
			}
			ag.evCh <- objs
		},
		func(typ string, raw json.RawMessage) {
			if typ == "DELETED" {
				return
			}
			if o, ok := decode(raw); ok {
				ag.evCh <- []agentObj{o}
			}
		}, &ag.sh.rxBytes)
}

func (ag *agent) loop(ctx context.Context) {
	sh := ag.sh
	gens := map[int]int64{}
	var high int64
	mine := map[pa]string{}
	claims := map[pa]int64{}
	dirty, claimDirty, writing, listed, first := false, false, false, false, true
	var lastWrite time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	timerArmed := false

	maybeWrite := func() {
		if !listed || writing || (!dirty && !claimDirty) {
			return
		}
		interval := sh.ackInterval
		if sh.design == "orig" {
			interval = sh.origInterval
		}
		if !claimDirty && interval > 0 {
			if wait := interval - time.Since(lastWrite); wait > 0 {
				if !timerArmed {
					timer.Reset(wait)
					timerArmed = true
				}
				return
			}
		}
		var cl []claimJSON
		for k, ts := range claims {
			cl = append(cl, claimJSON{EgressUID: sh.policyUIDs[k.p], Address: sh.addrs[k.p][k.a], TS: ts})
		}
		if cl == nil {
			cl = []claimJSON{}
		}
		var body []byte
		if sh.design == "orig" {
			obs := make([]observedJSON, 0, len(sh.policyUIDs))
			for p, uid := range sh.policyUIDs {
				obs = append(obs, observedJSON{EgressUID: uid, Generation: gens[p]})
			}
			body = mustJSON(map[string]any{"status": origNodeStatus{
				Observed: obs, Kernel: "6.12.0-amd64", PodNodeStage: "Netfilter", CNI: cniInfo, KubeProxy: kubeProxyInfo,
				PolicySlots: map[string]int{"used": 14, "capacity": 127}, Claims: cl, Announcing: []any{},
				AddressGuard: map[string]any{"loaded": true, "standingDown": false, "entries": 64}, Conditions: staticConds,
			}})
		} else {
			body = mustJSON(map[string]any{"status": decNodeStatus{
				ObservedEpoch: high,
				Capability:    map[string]any{"kernel": "6.12.0-amd64", "podNodeStage": "Netfilter", "cni": cniInfo, "kubeProxy": kubeProxyInfo, "tunnel": "L3"},
				Claims:        cl, Conditions: staticConds,
			}})
		}
		if claimDirty {
			sh.claimWrites.Add(1)
		} else {
			sh.ackWrites.Add(1)
		}
		dirty, claimDirty, writing = false, false, true
		lastWrite = time.Now()
		ag.wrCh <- body
	}

	apply := func(objs []agentObj) {
		for i := range objs {
			o := &objs[i]
			p, err := strconv.Atoi(o.Metadata.Labels["spike.purelb.io/policy-idx"])
			if err != nil {
				continue
			}
			for ai, ad := range o.Spec.Addresses {
				a := ai
				if sh.design != "orig" {
					a, _ = strconv.Atoi(o.Metadata.Labels["spike.purelb.io/addr-idx"])
				}
				st := ""
				for _, s := range ad.Slices {
					if s.Node == ag.name {
						st = s.State
					}
				}
				k := pa{p, a}
				mine[k] = st
				if st != "" {
					delete(claims, k) // assigned; drop the claim on the next write
				}
			}
			if sh.design == "orig" {
				if o.Metadata.Generation > gens[p] {
					gens[p] = o.Metadata.Generation
					dirty = true
				}
			} else if o.Spec.Epoch > high {
				high = o.Spec.Epoch
				dirty = true
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case objs := <-ag.evCh:
			apply(objs)
			// drain queued events so one write covers them all
			for more := true; more; {
				select {
				case o2 := <-ag.evCh:
					apply(o2)
				default:
					more = false
				}
			}
			if !listed {
				listed = true
				dirty = true
			}
		case p := <-ag.cniCh:
			// simulated CNI ADD: claim the reserve for every address of
			// the pod's policy where this node holds no slice
			for a := range sh.addrs[p] {
				k := pa{p, a}
				if mine[k] == "" {
					if _, ok := claims[k]; !ok {
						claims[k] = time.Now().UnixNano()
						claimDirty = true
					}
				}
			}
		case ok := <-ag.doneCh:
			writing = false
			if !ok {
				dirty = true
			} else if first {
				first = false
				sh.readyAgents.Add(1)
			}
		case <-timer.C:
			timerArmed = false
		}
		maybeWrite()
	}
}

func (ag *agent) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case body := <-ag.wrCh:
			_, err := ag.c.mergePatch(ctx, ag.statusPath(), body)
			if err != nil && ctx.Err() == nil {
				ag.sh.writeErrors.Add(1)
				logf("agent %s write: %v", ag.name, err)
			}
			select {
			case ag.doneCh <- err == nil:
			case <-ctx.Done():
				return
			}
		}
	}
}

// renewLease renews this node's PureLB Lease every leaseRenew (PureLB:
// LeaseDuration/2), through the agent's own rate-limited client, as the real
// lbnodeagent does.
func (ag *agent) renewLease(ctx context.Context) {
	t := time.NewTicker(ag.sh.leaseRenew)
	defer t.Stop()
	last := time.Now()
	path := "/apis/coordination.k8s.io/v1/namespaces/" + nsPureLB + "/leases/purelb-node-" + ag.name
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		body := []byte(`{"spec":{"holderIdentity":"` + ag.name + `","renewTime":"` + now.UTC().Format("2006-01-02T15:04:05.000000Z") + `"}}`)
		if _, err := ag.c.mergePatch(ctx, path, body); err != nil {
			if ctx.Err() == nil {
				logf("lease %s: %v", ag.name, err)
			}
			continue
		}
		done := time.Now()
		gap := done.Sub(last)
		last = done
		ag.sh.leaseRenews.Add(1)
		if gap > ag.sh.leaseDur {
			ag.sh.leaseLate.Add(1)
		}
		for {
			old := ag.sh.leaseMaxGap.Load()
			if int64(gap) <= old || ag.sh.leaseMaxGap.CompareAndSwap(old, int64(gap)) {
				break
			}
		}
	}
}

// startScoped: the agent watches only the slice tables of the addresses it
// announces (label spike.purelb.io/ann-<node>) and its own EgressNodeState
// (field selector), and acks only those versions.
func (ag *agent) startScoped(ctx context.Context) {
	go ag.loopScoped(ctx)
	go ag.writer(ctx)
	go ag.renewLease(ctx)
	decode := func(raw json.RawMessage) (agentObj, bool) {
		var o agentObj
		return o, json.Unmarshal(raw, &o) == nil
	}
	go ag.c.watchLoop(ctx, gv+"/namespaces/"+nsPureLB+"/egressendpoints",
		map[string]string{"labelSelector": "spike.purelb.io/ann-" + ag.name + "=1"},
		func(items []json.RawMessage) {
			objs := make([]agentObj, 0, len(items))
			for _, it := range items {
				if o, ok := decode(it); ok {
					objs = append(objs, o)
				}
			}
			ag.evCh <- objs
		},
		func(typ string, raw json.RawMessage) {
			if typ == "DELETED" {
				return
			}
			if o, ok := decode(raw); ok {
				ag.evCh <- []agentObj{o}
			}
		}, &ag.sh.rxBytes)
	if ag.sh.routedWatch && ag.idx < ag.sh.selK {
		// option C: a selector node advertises every host-route address,
		// so it watches and acks every host-route slice table.
		go ag.c.watchLoop(ctx, gv+"/namespaces/"+nsPureLB+"/egressendpoints",
			map[string]string{"labelSelector": "spike.purelb.io/routed=1"},
			func(items []json.RawMessage) {
				objs := make([]agentObj, 0, len(items))
				for _, it := range items {
					if o, ok := decode(it); ok {
						objs = append(objs, o)
					}
				}
				ag.evCh <- objs
			},
			func(typ string, raw json.RawMessage) {
				if typ == "DELETED" {
					return
				}
				ag.sh.selRxEvents.Add(1)
				if o, ok := decode(raw); ok {
					ag.evCh <- []agentObj{o}
				}
			}, &ag.sh.selRxBytes)
	}
	decodeNode := func(raw json.RawMessage) (scNodeObj, bool) {
		var o scNodeObj
		return o, json.Unmarshal(raw, &o) == nil
	}
	go ag.c.watchLoop(ctx, gv+"/egressnodestates",
		map[string]string{"fieldSelector": "metadata.name=" + ag.name},
		func(items []json.RawMessage) {
			for _, it := range items {
				if o, ok := decodeNode(it); ok {
					ag.ndCh <- o
				}
			}
			ag.ndCh <- scNodeObj{} // end of list
		},
		func(typ string, raw json.RawMessage) {
			if typ == "DELETED" {
				return
			}
			if o, ok := decodeNode(raw); ok {
				ag.ndCh <- o
			}
		}, &ag.sh.rxBytes)
}

func (ag *agent) loopScoped(ctx context.Context) {
	sh := ag.sh
	addrGen := map[string]int64{} // slice tables this node announces
	var nodeGen int64
	mine := map[pa]string{}
	claims := map[pa]int64{}
	dirty, claimDirty, writing, listedEP, listedNode, first := false, false, false, false, false, true
	var lastWrite time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	timerArmed := false

	maybeWrite := func() {
		if !listedEP || !listedNode || writing || (!dirty && !claimDirty) {
			return
		}
		if !claimDirty && sh.ackInterval > 0 {
			if wait := sh.ackInterval - time.Since(lastWrite); wait > 0 {
				if !timerArmed {
					timer.Reset(wait)
					timerArmed = true
				}
				return
			}
		}
		cl := []claimJSON{}
		for k, ts := range claims {
			cl = append(cl, claimJSON{EgressUID: sh.policyUIDs[k.p], Address: sh.addrs[k.p][k.a], TS: ts})
		}
		obs := make([]addrAckJSON, 0, len(addrGen))
		for n, g := range addrGen {
			obs = append(obs, addrAckJSON{Name: n, Generation: g})
		}
		sort.Slice(obs, func(i, j int) bool { return obs[i].Name < obs[j].Name })
		body := mustJSON(map[string]any{"status": scNodeStatus{
			ObservedAddrs: obs, ObservedNodeGeneration: nodeGen,
			Capability: map[string]any{"kernel": "6.12.0-amd64", "podNodeStage": "Netfilter", "cni": cniInfo, "kubeProxy": kubeProxyInfo, "tunnel": "L3"},
			Claims:     cl, Conditions: staticConds,
		}})
		if claimDirty {
			sh.claimWrites.Add(1)
		} else {
			sh.ackWrites.Add(1)
		}
		if !claimDirty && ag.idx < sh.selK {
			sh.selAckWrites.Add(1)
		}
		dirty, claimDirty, writing = false, false, true
		lastWrite = time.Now()
		ag.wrCh <- body
	}

	for {
		select {
		case <-ctx.Done():
			return
		case objs := <-ag.evCh:
			for _, o := range objs {
				if o.Metadata.Generation > addrGen[o.Metadata.Name] {
					addrGen[o.Metadata.Name] = o.Metadata.Generation
					dirty = true
				}
			}
			if !listedEP {
				listedEP, dirty = true, true
			}
		case o := <-ag.ndCh:
			if o.Metadata.Name == "" { // end of the initial list
				if !listedNode {
					listedNode, dirty = true, true
				}
				break
			}
			if o.Metadata.Generation > nodeGen {
				nodeGen = o.Metadata.Generation
				dirty = true
				mine = map[pa]string{}
				for _, s := range o.Spec.Slices {
					mine[pa{s.Policy, s.Addr}] = s.State
				}
				for k := range claims {
					if mine[k] != "" {
						delete(claims, k) // assigned; drop the claim on the next write
					}
				}
			}
		case p := <-ag.cniCh:
			for a := range sh.addrs[p] {
				k := pa{p, a}
				if mine[k] == "" {
					if _, ok := claims[k]; !ok {
						claims[k] = time.Now().UnixNano()
						claimDirty = true
					}
				}
			}
		case ok := <-ag.doneCh:
			writing = false
			if !ok {
				dirty = true
			} else if first {
				first = false
				sh.readyAgents.Add(1)
			}
		case <-timer.C:
			timerArmed = false
		}
		maybeWrite()
	}
}
