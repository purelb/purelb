package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The simulated allocator: one writer, one worker goroutine that owns all
// allocation state, a keyed FIFO dirty set (dedup while queued, like a
// client-go workqueue), and every write going through the allocator's own
// client-go rate limiter.

const (
	sliceSize  = 256
	portFirst  = 1024
	portLimit  = 65536
	stActive   = "Active"
	stPending  = "Pending"
	stDraining = "Draining"
)

type sliceState struct {
	node        string
	start       int
	state       string
	activeSince int64
	claimTS     []int64 // claims waiting for this slice to become Active and acked
	// scoped: an Active reserve just assigned to a node. The announcers
	// have not seen its owner yet, so its claims wait for their ack too.
	reserveClaim bool
}

type addrState struct {
	p, a       int
	address    string
	family     string
	announcers []string
	slices     []*sliceState
	nextStart  int
	claims     map[string]int64 // node -> earliest claim ts
	exhausted  int
	// option A: a routed address in D7 host-route mode, and its advertiser
	// set as last computed (joined node names).
	hostRoute bool
	lastAdv   string
}

type policyState struct {
	idx   int
	name  string
	uid   string
	addrs []*addrState
	gen   int64 // original: last metadata.generation written
}

type podInfo struct {
	name, uid, node, ip string
	policy              int
	terminating         bool
}

type podEvent struct {
	key     string
	pod     podInfo
	deleted bool
}

type claimMsg struct {
	node   string
	policy int
	addr   int
	ts     int64
}

type activation struct {
	key    string
	slices []*sliceState
	claims map[string][]int64 // scoped: node -> reserve claims the announcers have now acked
}

type allocStats struct {
	writesEndpoints atomic.Int64
	writesMembers   atomic.Int64
	writesNodes     atomic.Int64 // scoped: EgressNodeState spec writes
	writeErrors     atomic.Int64
	dirty           atomic.Int64
	exhausted       atomic.Int64
	// option A: advertiser-set changes, the holders each change rewrites,
	// spec writes that only changed advertisers (the extra writes) or
	// changed advertisers together with slices, and the bytes the
	// advertisers field adds to every EgressNodeState spec write.
	advChanges     atomic.Int64
	advHoldersSum  atomic.Int64
	advHoldersMax  atomic.Int64 // reset per phase by the driver
	advOnlyWrites  atomic.Int64
	advPiggyWrites atomic.Int64
	advBytes       atomic.Int64
	mergeDeferrals atomic.Int64 // ordinary-queue writes held back by a merge window
	// writes submitted per lane: slice tables and node specs, from the
	// priority lane (claims, activations, the claimant's spec) or not.
	epHi, epLo, nsHi, nsLo atomic.Int64
}

type allocator struct {
	design   string
	c        *client
	boot     *client // bootstrap creates only (not measured)
	policies []*policyState
	nodes    []string
	nodeIdx  map[string]int
	tr       *tracker
	podCh    chan podEvent
	claimCh  chan claimMsg
	actCh    chan activation
	stats    allocStats
	epoch    int64
	pods     map[string]*podInfo
	// last written spec bytes per key
	last map[string][]byte
	// dirty FIFO
	dirtySet map[string]bool
	dirtyQ   []string
	ready    chan struct{}
	// scoped: per slice table, node -> slice state last written (to find
	// the nodes a change affects); per node, claim timestamps waiting for
	// that node to ack an Active slice, and the last spec generation.
	lastSlices map[string]map[string]string
	nodeClaims map[string][]int64
	nodeGen    map[string]int64
	addrGen    map[string]int64 // scoped: last written generation per slice table
	// writers > 1 (scoped only): HTTP writes run on a pool, results come
	// back to the owning goroutine on doneCh; a key never has two writes in
	// flight, and a key dirtied while in flight is redone after it.
	writers int
	// priority (scoped only): a second FIFO served first, for the keys on a
	// claim's path. A key in both queues is processed once (dirtySet).
	priority bool
	reserve  int // unassigned reserve slices kept per address (D17: 1)
	hiQ      []string
	hiSet    map[string]bool
	inflight map[string]bool
	redo     map[string]bool
	jobCh    chan writeJob
	doneCh   chan writeDone
	// option A: the announcerNodeSelector nodes (sorted), and per node the
	// slices and advertisers parts of the spec last written.
	selector   []string
	selSet     map[string]bool
	lastNodeSl map[string][]byte
	lastNodeAd map[string][]byte
	// option C: every selector node advertises every host-route address
	// and is its slice-table announcer; no spec.advertisers is written.
	optC bool
	// merge windows (scoped): a slice table (epMerge) or node spec
	// (specMerge) is written from the ordinary queue at most once per
	// window; changes in between fold into the next write. The priority
	// lane (claims, activations, the claimant's spec) is never delayed.
	epMerge   time.Duration
	specMerge time.Duration
	lastWrite map[string]time.Time // last write submitted per key
	deferred  map[string]time.Time // key -> when its window ends
	curHi     bool                 // the key being reconciled came from the priority lane
}

type writeJob struct {
	key  string
	do   func() ([]byte, error)
	then func([]byte, error)
}

type writeDone struct {
	key  string
	raw  []byte
	err  error
	then func([]byte, error)
}

func newAllocator(design string, c, boot *client, policies []*policyState, nodes []string, tr *tracker, act chan activation) *allocator {
	al := &allocator{
		design: design, c: c, boot: boot, policies: policies, nodes: nodes, tr: tr,
		podCh: make(chan podEvent, 1<<17), claimCh: make(chan claimMsg, 1<<17), actCh: act,
		pods: map[string]*podInfo{}, last: map[string][]byte{}, dirtySet: map[string]bool{},
		nodeIdx: map[string]int{}, ready: make(chan struct{}),
		lastSlices: map[string]map[string]string{}, nodeClaims: map[string][]int64{}, nodeGen: map[string]int64{}, addrGen: map[string]int64{},
		writers: 1, reserve: 1, inflight: map[string]bool{}, redo: map[string]bool{}, hiSet: map[string]bool{},
		selSet: map[string]bool{}, lastNodeSl: map[string][]byte{}, lastNodeAd: map[string][]byte{},
		lastWrite: map[string]time.Time{}, deferred: map[string]time.Time{},
	}
	for i, n := range nodes {
		al.nodeIdx[n] = i
	}
	return al
}

// setSelector sets the announcerNodeSelector nodes (option A).
func (al *allocator) setSelector(sel []string) {
	al.selector = append([]string(nil), sel...)
	sort.Strings(al.selector)
	for _, n := range sel {
		al.selSet[n] = true
	}
}

// advertisersOf is the D7 host-route rule: selector nodes holding an Active
// slice of the address, or every selector node when there are none.
func (al *allocator) advertisersOf(as *addrState) []string {
	var adv []string
	for _, s := range as.slices {
		if s.state == stActive && al.selSet[s.node] {
			adv = append(adv, s.node)
		}
	}
	if len(adv) == 0 {
		return al.selector
	}
	sort.Strings(adv)
	return adv
}

func (al *allocator) markDirty(k string) {
	if !al.dirtySet[k] {
		al.dirtySet[k] = true
		al.dirtyQ = append(al.dirtyQ, k)
	}
}

// markHi queues k ahead of other work when priority is on.
func (al *allocator) markHi(k string) {
	if !al.priority {
		al.markDirty(k)
		return
	}
	if !al.hiSet[k] {
		al.hiSet[k] = true
		al.dirtySet[k] = true
		al.hiQ = append(al.hiQ, k)
	}
}

func (al *allocator) markPolicy(p int) {
	if al.design == "orig" {
		al.markDirty("p/" + strconv.Itoa(p))
		return
	}
	al.markDirty("m/" + strconv.Itoa(p))
	for a := range al.policies[p].addrs {
		al.markDirty(fmt.Sprintf("e/%d/%d", p, a))
	}
}

func (al *allocator) handlePod(ev podEvent) {
	old := al.pods[ev.key]
	if ev.deleted {
		if old == nil {
			return
		}
		delete(al.pods, ev.key)
		al.markPolicy(old.policy)
		return
	}
	pi := ev.pod
	if old != nil && *old == pi {
		return
	}
	al.pods[ev.key] = &pi
	al.markPolicy(pi.policy)
	if old != nil && old.policy != pi.policy {
		al.markPolicy(old.policy)
	}
}

func (al *allocator) handleClaim(c claimMsg) {
	if c.policy < 0 || c.policy >= len(al.policies) || c.addr >= len(al.policies[c.policy].addrs) {
		return
	}
	as := al.policies[c.policy].addrs[c.addr]
	for _, s := range as.slices {
		if s.node == c.node {
			if s.state == stDraining {
				al.markKeyFor(c.policy, c.addr)
			}
			return // already holds a slice
		}
	}
	if old, ok := as.claims[c.node]; ok && old <= c.ts {
		return
	}
	as.claims[c.node] = c.ts
	if al.design == "scoped" {
		al.markHi(fmt.Sprintf("e/%d/%d", c.policy, c.addr))
		return
	}
	al.markKeyFor(c.policy, c.addr)
}

func (al *allocator) markKeyFor(p, a int) {
	if al.design == "orig" {
		al.markDirty("p/" + strconv.Itoa(p))
	} else {
		al.markDirty(fmt.Sprintf("e/%d/%d", p, a))
	}
}

func (al *allocator) handleActivation(a activation) {
	changed := false
	for _, s := range a.slices {
		if s.state == stPending {
			s.state = stActive
			s.activeSince = -1 // filled at write time
			changed = true
		}
	}
	if changed {
		al.markHi(a.key)
	}
	for node, ts := range a.claims {
		al.nodeClaims[node] = append(al.nodeClaims[node], ts...)
		al.markHi("n/" + node)
	}
}

func (al *allocator) drain() bool {
	got := false
	for {
		select {
		case ev := <-al.podCh:
			al.handlePod(ev)
		case c := <-al.claimCh:
			al.handleClaim(c)
		case a := <-al.actCh:
			al.handleActivation(a)
		default:
			return got
		}
		got = true
	}
}

func (al *allocator) run(ctx context.Context) {
	<-al.ready
	if al.writers > 1 {
		al.jobCh = make(chan writeJob, al.writers)
		al.doneCh = make(chan writeDone, al.writers)
		for i := 0; i < al.writers; i++ {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case j := <-al.jobCh:
						raw, err := j.do()
						select {
						case al.doneCh <- writeDone{key: j.key, raw: raw, err: err, then: j.then}:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
		}
	}
	for ctx.Err() == nil {
		al.drain()
		al.drainDone()
		var next <-chan time.Time // fires when the earliest merge window ends
		if len(al.deferred) > 0 {
			now := time.Now()
			earliest := time.Time{}
			for k, due := range al.deferred {
				if !due.After(now) {
					delete(al.deferred, k)
					al.markDirty(k)
				} else if earliest.IsZero() || due.Before(earliest) {
					earliest = due
				}
			}
			if !earliest.IsZero() {
				next = time.After(earliest.Sub(now))
			}
		}
		al.stats.dirty.Store(int64(len(al.dirtySet) + len(al.inflight) + len(al.deferred)))
		if (len(al.dirtyQ) == 0 && len(al.hiQ) == 0) || len(al.inflight) >= al.writers {
			select {
			case <-ctx.Done():
				return
			case <-next:
			case ev := <-al.podCh:
				al.handlePod(ev)
			case c := <-al.claimCh:
				al.handleClaim(c)
			case a := <-al.actCh:
				al.handleActivation(a)
			case d := <-al.doneCh: // nil (never ready) with one writer
				al.finish(d)
			}
			continue
		}
		var k string
		fromHi := len(al.hiQ) > 0
		if fromHi {
			k = al.hiQ[0]
			al.hiQ = al.hiQ[1:]
			delete(al.hiSet, k)
		} else {
			k = al.dirtyQ[0]
			al.dirtyQ = al.dirtyQ[1:]
			if !al.dirtySet[k] {
				continue // already processed from the priority queue
			}
		}
		delete(al.dirtySet, k)
		if fromHi {
			delete(al.deferred, k) // the claim-path write carries the merged changes
		} else if w := al.mergeWindow(k); w > 0 {
			if last, ok := al.lastWrite[k]; ok && time.Since(last) < w {
				if _, held := al.deferred[k]; !held {
					al.deferred[k] = last.Add(w)
					al.stats.mergeDeferrals.Add(1)
				}
				continue
			}
		}
		al.stats.dirty.Store(int64(len(al.dirtySet) + len(al.inflight) + len(al.deferred)))
		if al.inflight[k] {
			al.redo[k] = al.redo[k] || fromHi // value: redo with priority
			continue
		}
		al.curHi = fromHi
		al.reconcile(ctx, k, false)
		al.curHi = false
	}
}

// submit performs one write: inline with one writer (and at bootstrap),
// otherwise on the writer pool. then runs on the owning goroutine either way.
func (al *allocator) submit(key string, inline bool, do func() ([]byte, error), then func([]byte, error)) {
	if inline || al.writers <= 1 {
		raw, err := do()
		then(raw, err)
		return
	}
	al.inflight[key] = true
	al.lastWrite[key] = time.Now()
	switch {
	case key[0] == 'e' && al.curHi:
		al.stats.epHi.Add(1)
	case key[0] == 'e':
		al.stats.epLo.Add(1)
	case key[0] == 'n' && al.curHi:
		al.stats.nsHi.Add(1)
	case key[0] == 'n':
		al.stats.nsLo.Add(1)
	}
	al.jobCh <- writeJob{key: key, do: do, then: then}
}

// mergeWindow is the merge window for an ordinary-queue write of key.
func (al *allocator) mergeWindow(key string) time.Duration {
	switch key[0] {
	case 'e':
		return al.epMerge
	case 'n':
		return al.specMerge
	}
	return 0
}

func (al *allocator) finish(d writeDone) {
	delete(al.inflight, d.key)
	d.then(d.raw, d.err)
	if hi, ok := al.redo[d.key]; ok {
		delete(al.redo, d.key)
		if hi {
			al.markHi(d.key)
		} else {
			al.markDirty(d.key)
		}
	}
}

func (al *allocator) drainDone() {
	for {
		select {
		case d := <-al.doneCh:
			al.finish(d)
		default:
			return
		}
	}
}

// nodesWithPods returns the set of nodes running a non-terminating selected
// pod of policy p.
func (al *allocator) podsOf(p int) ([]*podInfo, map[string]bool) {
	var list []*podInfo
	nodes := map[string]bool{}
	for _, pi := range al.pods {
		if pi.policy != p || pi.terminating || pi.node == "" {
			continue
		}
		list = append(list, pi)
		nodes[pi.node] = true
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	return list, nodes
}

func (al *allocator) newSlice(as *addrState, node, state string) *sliceState {
	if as.nextStart+sliceSize > portLimit {
		as.exhausted++
		al.stats.exhausted.Add(1)
		return nil
	}
	s := &sliceState{node: node, start: as.nextStart, state: state}
	as.nextStart += sliceSize
	as.slices = append(as.slices, s)
	return s
}

// updateAddr applies pod-node and claim changes to one address.
func (al *allocator) updateAddr(as *addrState, has map[string]bool) {
	for _, s := range as.slices {
		if s.node == "" {
			continue
		}
		switch {
		case s.state == stActive && !has[s.node] && len(s.claimTS) == 0:
			s.state = stDraining
		case s.state == stDraining && has[s.node]:
			s.state = stActive
		}
	}
	if len(as.claims) > 0 {
		type cl struct {
			node string
			ts   int64
		}
		var cls []cl
		for n, ts := range as.claims {
			cls = append(cls, cl{n, ts})
		}
		sort.Slice(cls, func(i, j int) bool { return cls[i].ts < cls[j].ts })
		for _, c := range cls {
			delete(as.claims, c.node)
			var mine *sliceState
			for _, s := range as.slices {
				if s.node == c.node {
					mine = s
				}
			}
			if mine != nil {
				if mine.state == stDraining {
					mine.state = stActive
				}
				continue
			}
			// D17: the claim takes the Active, unassigned reserve; concurrent
			// claims beyond it get new Pending slices in the same generation.
			var reserve *sliceState
			for _, s := range as.slices {
				if s.node == "" && s.state == stActive {
					reserve = s
					break
				}
			}
			if reserve != nil {
				reserve.node = c.node
				reserve.claimTS = append(reserve.claimTS, c.ts)
				reserve.reserveClaim = true
				continue
			}
			if s := al.newSlice(as, c.node, stPending); s != nil {
				s.claimTS = append(s.claimTS, c.ts)
			}
		}
	}
	// Keep al.reserve unassigned reserves (Active, or Pending until the
	// announcers ack). D17 keeps one; more let a burst of claims take
	// already-acked slices instead of waiting for an announcer round trip.
	unassigned := 0
	for _, s := range as.slices {
		if s.node == "" {
			unassigned++
		}
	}
	for ; unassigned < al.reserve; unassigned++ {
		if al.newSlice(as, "", stPending) == nil {
			break
		}
	}
}

func (al *allocator) addrJSON(as *addrState, ver int64, activate *[]*sliceState, ready *[]int64) addrJSON {
	aj := addrJSON{
		Address: as.address, Family: as.family, Pool: "egress-pool", Interface: "kube-lb0",
		Placement: "MultiAnnouncer", Announcers: as.announcers, Reserved: []any{},
	}
	for _, s := range as.slices {
		if s.state == stActive && s.activeSince <= 0 {
			s.activeSince = ver
		}
		if s.state == stPending {
			*activate = append(*activate, s)
		}
		if s.state == stActive && s.node != "" && len(s.claimTS) > 0 {
			*ready = append(*ready, s.claimTS...)
			s.claimTS = nil
		}
		aj.Slices = append(aj.Slices, sliceJSON{Node: s.node, Start: s.start, End: s.start + sliceSize - 1, State: s.state, ActiveSince: s.activeSince})
	}
	return aj
}

func endpointsOf(list []*podInfo) []endpointJSON {
	eps := make([]endpointJSON, 0, len(list))
	for _, pi := range list {
		if pi.ip == "" {
			continue
		}
		eps = append(eps, endpointJSON{Pod: pi.name, UID: pi.uid, Node: pi.node, PodIPs: []string{pi.ip, v6Of(pi.ip)}})
	}
	return eps
}

func policyBlock(ps *policyState) policyJSON {
	return policyJSON{
		UID: ps.uid, Helpers: []string{}, FailurePolicy: "FailClosed",
		Destinations: destJSON{Include: []string{"0.0.0.0/0", "::/0"}, Exclude: []string{"10.0.0.0/8", "fd00::/8"}},
	}
}

func objMeta(name, ns string, lbl map[string]string, owner *policyState) map[string]any {
	m := map[string]any{"name": name, "labels": lbl}
	if ns != "" {
		m["namespace"] = ns
	}
	if owner != nil {
		m["ownerReferences"] = []map[string]any{{
			"apiVersion": group + "/v1", "kind": "Egress", "name": owner.name, "uid": owner.uid,
		}}
	}
	return m
}

type genOnly struct {
	Metadata struct {
		Generation int64 `json:"generation"`
	} `json:"metadata"`
}

// reconcile writes one key if its desired spec changed. boot=true creates the
// object with the bootstrap client (setup, not measured).
func (al *allocator) reconcile(ctx context.Context, key string, boot bool) {
	if key[0] == 'n' { // scoped: a node's own EgressNodeState spec
		al.reconcileNode(ctx, key[2:], boot)
		return
	}
	var p, a int
	switch key[0] {
	case 'p', 'm':
		p, _ = strconv.Atoi(key[2:])
	case 'e':
		fmt.Sscanf(key, "e/%d/%d", &p, &a)
	}
	ps := al.policies[p]
	list, has := al.podsOf(p)
	lbl := map[string]string{"purelb.io/egress": ps.name, "purelb.io/egress-kind": "Egress", "spike.purelb.io/policy-idx": strconv.Itoa(p)}

	switch key[0] {
	case 'm': // decided: EgressMembers, allocator-only, no ack
		spec := membersSpec{PolicyUID: ps.uid, Endpoints: endpointsOf(list)}
		b := mustJSON(spec)
		if bytes.Equal(b, al.last[key]) {
			return
		}
		name := ps.name
		al.submit(key, boot, func() ([]byte, error) {
			if boot {
				return al.boot.post(ctx, gv+"/namespaces/"+nsPureLB+"/egressmembers",
					mustJSON(map[string]any{"apiVersion": group + "/v1", "kind": "EgressMembers", "metadata": objMeta(name, nsPureLB, lbl, nil), "spec": spec}))
			}
			return al.c.mergePatch(ctx, gv+"/namespaces/"+nsPureLB+"/egressmembers/"+name, mustJSON(map[string]any{"spec": spec}))
		}, func(_ []byte, err error) {
			if err != nil {
				al.writeErr(key, err)
				return
			}
			al.last[key] = b
			al.stats.writesMembers.Add(1)
		})
		return

	case 'p': // original: one object per policy with endpoints
		for _, as := range ps.addrs {
			al.updateAddr(as, has)
		}
		// Build with a provisional version for activeSince; the real
		// generation is read back from the response.
		ver := ps.gen + 1
		var activate []*sliceState
		var ready []int64
		spec := origSpec{Policy: policyBlock(ps), Endpoints: endpointsOf(list)}
		for _, as := range ps.addrs {
			spec.Addresses = append(spec.Addresses, al.addrJSON(as, ver, &activate, &ready))
		}
		b := mustJSON(spec)
		if bytes.Equal(b, al.last[key]) {
			return
		}
		var raw []byte
		var err error
		if boot {
			raw, err = al.boot.post(ctx, gv+"/namespaces/"+nsLoad+"/origegressendpoints",
				mustJSON(map[string]any{"apiVersion": group + "/v1", "kind": "OrigEgressEndpoints", "metadata": objMeta(ps.name, nsLoad, lbl, ps), "spec": spec}))
		} else {
			raw, err = al.c.mergePatch(ctx, gv+"/namespaces/"+nsLoad+"/origegressendpoints/"+ps.name, mustJSON(map[string]any{"spec": spec}))
		}
		if err != nil {
			al.writeErr(key, err)
			return
		}
		var g genOnly
		_ = json.Unmarshal(raw, &g)
		ps.gen = g.Metadata.Generation
		al.last[key] = b
		al.stats.writesEndpoints.Add(1)
		al.tr.regCh <- pendingReg{group: p, ver: ps.gen, t: time.Now(), key: key, activate: activate, readyClaims: ready}
		return

	case 'e': // decided: one object per (policy, address)
		if al.design == "scoped" {
			al.reconcileScopedAddr(ctx, key, p, a, ps, has, lbl, boot)
			return
		}
		as := ps.addrs[a]
		al.updateAddr(as, has)
		ver := al.epoch + 1
		var activate []*sliceState
		var ready []int64
		aj := al.addrJSON(as, ver, &activate, &ready)
		cmp := mustJSON(aj) // compare without the epoch
		if bytes.Equal(cmp, al.last[key]) {
			return
		}
		spec := decSpec{Policy: policyBlock(ps), Epoch: ver, Addresses: []addrJSON{aj}}
		name := fmt.Sprintf("%s-a%d", ps.name, a)
		lbl["spike.purelb.io/addr-idx"] = strconv.Itoa(a)
		var err error
		if boot {
			_, err = al.boot.post(ctx, gv+"/namespaces/"+nsPureLB+"/egressendpoints",
				mustJSON(map[string]any{"apiVersion": group + "/v1", "kind": "EgressEndpoints", "metadata": objMeta(name, nsPureLB, lbl, nil), "spec": spec}))
		} else {
			_, err = al.c.mergePatch(ctx, gv+"/namespaces/"+nsPureLB+"/egressendpoints/"+name, mustJSON(map[string]any{"spec": spec}))
		}
		if err != nil {
			al.writeErr(key, err)
			return
		}
		al.epoch = ver
		al.last[key] = cmp
		al.stats.writesEndpoints.Add(1)
		al.tr.regCh <- pendingReg{group: 0, ver: ver, t: time.Now(), key: key, activate: activate, readyClaims: ready}
	}
}

// reconcileScopedAddr writes one address's slice table. Only the address's
// announcers watch it and only they gate it; the nodes whose own slice
// changed are queued for an EgressNodeState spec write.
func (al *allocator) reconcileScopedAddr(ctx context.Context, key string, p, a int, ps *policyState, has map[string]bool, lbl map[string]string, boot bool) {
	as := ps.addrs[a]
	al.updateAddr(as, has)
	// A claim completes when the address's announcers know the slice's
	// owner and the node acks its own spec showing the slice Active. A slice
	// that was Pending already had its owner acked by the announcers before
	// activation, so its claims go straight to the node. A reserve just
	// assigned has not: its claims ride on this write's announcer ack first.
	stage := map[string][]int64{}
	for _, s := range as.slices {
		if s.state == stActive && s.node != "" && len(s.claimTS) > 0 {
			if s.reserveClaim {
				stage[s.node] = append(stage[s.node], s.claimTS...)
			} else {
				al.nodeClaims[s.node] = append(al.nodeClaims[s.node], s.claimTS...)
				al.markHi("n/" + s.node)
			}
			s.claimTS = nil
			s.reserveClaim = false
		}
	}
	var activate []*sliceState
	var ready []int64
	aj := al.addrJSON(as, 0, &activate, &ready)
	cur := map[string]string{}
	for _, s := range as.slices {
		if s.node != "" {
			cur[s.node] = s.state
		}
	}
	prev := al.lastSlices[key]
	for n, st := range cur {
		if prev[n] != st {
			al.markDirty("n/" + n)
		}
	}
	for n := range prev {
		if _, ok := cur[n]; !ok {
			al.markDirty("n/" + n)
		}
	}
	al.lastSlices[key] = cur
	// option A: when the address's advertiser set changes, every node
	// holding a slice of it gets its spec rewritten (ordinary queue, not the
	// priority lane; a claimant's own spec is already queued there).
	if as.hostRoute && !al.optC {
		adv := strings.Join(al.advertisersOf(as), ",")
		if adv != as.lastAdv {
			as.lastAdv = adv
			if !boot {
				al.stats.advChanges.Add(1)
				al.stats.advHoldersSum.Add(int64(len(cur)))
				for h := int64(len(cur)); ; {
					m := al.stats.advHoldersMax.Load()
					if h <= m || al.stats.advHoldersMax.CompareAndSwap(m, h) {
						break
					}
				}
			}
			for n := range cur {
				al.markDirty("n/" + n)
			}
		}
	}
	cmp := mustJSON(aj)
	gate := make([]int, 0, len(as.announcers))
	for _, n := range as.announcers {
		gate = append(gate, al.nodeIdx[n])
	}
	ackGroup := groupAddr(p, a, len(ps.addrs))
	if bytes.Equal(cmp, al.last[key]) {
		if len(stage) > 0 { // nothing to write: gate on the version already written
			al.tr.regCh <- pendingReg{group: ackGroup, nodes: gate, ver: al.addrGen[key], t: time.Now(), key: key, stage: stage}
		}
		return
	}
	spec := scSpec{Policy: policyBlock(ps), Addresses: []addrJSON{aj}}
	name := fmt.Sprintf("%s-a%d", ps.name, a)
	lbl["spike.purelb.io/addr-idx"] = strconv.Itoa(a)
	if as.hostRoute && al.optC {
		lbl["spike.purelb.io/routed"] = "1" // watched by every selector node
	} else {
		for _, n := range as.announcers {
			lbl["spike.purelb.io/ann-"+n] = "1"
		}
	}
	al.submit(key, boot, func() ([]byte, error) {
		if boot {
			return al.boot.post(ctx, gv+"/namespaces/"+nsPureLB+"/egressendpoints",
				mustJSON(map[string]any{"apiVersion": group + "/v1", "kind": "EgressEndpoints", "metadata": objMeta(name, nsPureLB, lbl, nil), "spec": spec}))
		}
		return al.c.mergePatch(ctx, gv+"/namespaces/"+nsPureLB+"/egressendpoints/"+name, mustJSON(map[string]any{"spec": spec}))
	}, func(raw []byte, err error) {
		if err != nil {
			al.writeErr(key, err)
			return
		}
		var g genOnly
		_ = json.Unmarshal(raw, &g)
		al.last[key] = cmp
		al.addrGen[key] = g.Metadata.Generation
		al.stats.writesEndpoints.Add(1)
		al.tr.regCh <- pendingReg{group: ackGroup, nodes: gate, ver: g.Metadata.Generation,
			t: time.Now(), key: key, activate: activate, readyClaims: ready, stage: stage}
	})
}

// reconcileNode writes a node's own slices into its EgressNodeState spec.
// Only that node watches the object and only that node gates the write.
func (al *allocator) reconcileNode(ctx context.Context, node string, boot bool) {
	key := "n/" + node
	var spec scNodeSpec
	for p, ps := range al.policies {
		for a, as := range ps.addrs {
			held := false
			for _, s := range as.slices {
				if s.node == node {
					held = true
					spec.Slices = append(spec.Slices, nodeSliceJSON{Policy: p, Addr: a, Address: as.address,
						Start: s.start, End: s.start + sliceSize - 1, State: s.state})
				}
			}
			if held && as.hostRoute && !al.optC {
				spec.Advertisers = append(spec.Advertisers, advJSON{Address: as.address, Nodes: al.advertisersOf(as)})
			}
		}
	}
	if spec.Slices == nil {
		spec.Slices = []nodeSliceJSON{}
	}
	claims := al.nodeClaims[node]
	delete(al.nodeClaims, node)
	b := mustJSON(spec)
	bSl := mustJSON(scNodeSpec{Slices: spec.Slices}) // the spec without option A
	bAd := mustJSON(spec.Advertisers)
	register := func() {
		al.tr.regCh <- pendingReg{group: groupNode(al.nodeIdx[node]), nodes: []int{al.nodeIdx[node]},
			ver: al.nodeGen[node], t: time.Now(), key: key, readyClaims: claims}
	}
	if bytes.Equal(b, al.last[key]) {
		if len(claims) > 0 {
			register()
		}
		return
	}
	c := al.c
	if boot {
		c = al.boot
	}
	al.submit(key, boot, func() ([]byte, error) {
		return c.mergePatch(ctx, gv+"/egressnodestates/"+node, mustJSON(map[string]any{"spec": spec}))
	}, func(raw []byte, err error) {
		if err != nil {
			al.nodeClaims[node] = append(al.nodeClaims[node], claims...)
			al.writeErr(key, err)
			return
		}
		var g genOnly
		_ = json.Unmarshal(raw, &g)
		al.nodeGen[node] = g.Metadata.Generation
		al.last[key] = b
		al.stats.writesNodes.Add(1)
		if !boot {
			slChanged := !bytes.Equal(bSl, al.lastNodeSl[node])
			adChanged := !bytes.Equal(bAd, al.lastNodeAd[node])
			switch {
			case adChanged && !slChanged:
				al.stats.advOnlyWrites.Add(1)
			case adChanged:
				al.stats.advPiggyWrites.Add(1)
			}
			al.stats.advBytes.Add(int64(len(b) - len(bSl)))
		}
		al.lastNodeSl[node], al.lastNodeAd[node] = bSl, bAd
		register()
	})
}

func (al *allocator) writeErr(key string, err error) {
	al.stats.writeErrors.Add(1)
	logf("allocator write %s: %v", key, err)
	al.markDirty(key) // retry
}

// bootstrap assigns an Active slice to every node that already runs pods of
// each policy, plus one Active reserve, and creates all objects.
func (al *allocator) bootstrap(ctx context.Context) {
	for p, ps := range al.policies {
		_, has := al.podsOf(p)
		nodes := make([]string, 0, len(has))
		for n := range has {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		for _, as := range ps.addrs {
			for _, n := range nodes {
				al.newSlice(as, n, stActive)
			}
			for r := 0; r < al.reserve; r++ {
				al.newSlice(as, "", stActive)
			}
		}
	}
	var keys []string
	for p, ps := range al.policies {
		if al.design == "orig" {
			keys = append(keys, "p/"+strconv.Itoa(p))
		} else {
			keys = append(keys, "m/"+strconv.Itoa(p))
			for a := range ps.addrs {
				keys = append(keys, fmt.Sprintf("e/%d/%d", p, a))
			}
		}
	}
	if al.design == "scoped" {
		for _, n := range al.nodes {
			keys = append(keys, "n/"+n)
		}
	}
	for _, k := range keys {
		al.reconcile(ctx, k, true)
	}
	// clear any dirty marks produced while loading pods
	al.dirtySet = map[string]bool{}
	al.dirtyQ = nil
	al.hiSet = map[string]bool{}
	al.hiQ = nil
}
