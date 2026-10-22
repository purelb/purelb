package main

import (
	"context"
	"sort"
	"time"
)

// The tracker owns all ack bookkeeping in one goroutine (no locks). It
// receives (a) every allocator write that must be acked, (b) every node ack
// observed by the allocator's EgressNodeState watch, and (c) Lease renewals.
// When every gating node has acked a write it records the latency and, for
// writes that published Pending slices, tells the allocator to activate them.

type pendingReg struct {
	group       int   // original: policy index; decided: 0 (one global epoch); scoped: groupAddr or groupNode
	nodes       []int // scoped: the only nodes that gate this write (nil: every node)
	ver         int64
	t           time.Time
	key         string
	activate    []*sliceState
	readyClaims []int64
	stage       map[string][]int64 // scoped: node -> reserve claims to hand to the node once acked
}

type ackUpdate struct {
	node int
	// original: per-policy generation; decided: vers[0] = observedEpoch
	vers map[int]int64
}

type leaseUpdate struct {
	node  int
	renew time.Time
}

type pendingItem struct {
	pendingReg
	need  int
	got   int
	acked []bool
}

type trackerStats struct {
	pending  int
	ackLat   []float64 // seconds, per write
	claimLat []float64 // seconds, per claim
}

type tracker struct {
	nodes      int
	design     string
	leaseDur   time.Duration
	regCh      chan pendingReg
	ackCh      chan ackUpdate
	leaseCh    chan leaseUpdate
	statsCh    chan chan trackerStats // returns and resets latency samples
	activateCh chan<- activation
}

func newTracker(nodes int, design string, leaseDur time.Duration, act chan<- activation) *tracker {
	return &tracker{
		nodes: nodes, design: design, leaseDur: leaseDur,
		regCh: make(chan pendingReg, 1<<16), ackCh: make(chan ackUpdate, 1<<18),
		leaseCh: make(chan leaseUpdate, 1<<16), statsCh: make(chan chan trackerStats),
		activateCh: act,
	}
}

func (t *tracker) run(ctx context.Context) {
	nodeAck := make([]map[int]int64, t.nodes)
	for i := range nodeAck {
		nodeAck[i] = map[int]int64{}
	}
	renew := make([]time.Time, t.nodes)
	pending := map[int][]*pendingItem{}
	var ackLat, claimLat []float64

	resolve := func(it *pendingItem, now time.Time) {
		ackLat = append(ackLat, now.Sub(it.t).Seconds())
		for _, ts := range it.readyClaims {
			claimLat = append(claimLat, now.Sub(time.Unix(0, ts)).Seconds())
		}
		if len(it.activate) > 0 || len(it.stage) > 0 {
			t.activateCh <- activation{key: it.key, slices: it.activate, claims: it.stage}
		}
	}
	gating := func(n int, now time.Time) bool {
		if t.design == "orig" {
			return true // original: every eligible node gates
		}
		// decided: only nodes with a live Lease gate activation
		return !renew[n].IsZero() && now.Sub(renew[n]) <= t.leaseDur
	}

	for {
		select {
		case <-ctx.Done():
			return
		case r := <-t.regCh:
			now := time.Now()
			it := &pendingItem{pendingReg: r, acked: make([]bool, t.nodes)}
			gate := r.nodes
			if gate == nil {
				gate = make([]int, t.nodes)
				for n := range gate {
					gate[n] = n
				}
			} else {
				for n := range it.acked {
					it.acked[n] = true // outside the gating set
				}
			}
			for _, n := range gate {
				it.acked[n] = false
				if !gating(n, now) {
					it.acked[n] = true // not gating: treat as satisfied
					continue
				}
				it.need++
				if nodeAck[n][r.group] >= r.ver {
					it.acked[n] = true
					it.got++
				}
			}
			if it.got >= it.need {
				resolve(it, now)
				continue
			}
			pending[r.group] = append(pending[r.group], it)
		case u := <-t.ackCh:
			now := time.Now()
			for g, v := range u.vers {
				if v <= nodeAck[u.node][g] {
					continue
				}
				nodeAck[u.node][g] = v
				list := pending[g]
				if len(list) == 0 {
					continue
				}
				kept := list[:0]
				for _, it := range list {
					if it.ver <= v && !it.acked[u.node] {
						it.acked[u.node] = true
						it.got++
					}
					if it.got >= it.need {
						resolve(it, now)
						continue
					}
					kept = append(kept, it)
				}
				pending[g] = kept
			}
		case l := <-t.leaseCh:
			if l.renew.After(renew[l.node]) {
				renew[l.node] = l.renew
			}
		case reply := <-t.statsCh:
			n := 0
			for _, l := range pending {
				n += len(l)
			}
			reply <- trackerStats{pending: n, ackLat: ackLat, claimLat: claimLat}
			ackLat, claimLat = nil, nil
		}
	}
}

// scoped ack groups: one per (policy, address) slice table, one per node's
// own EgressNodeState spec. Group 0 stays the decided design's global epoch.
func groupAddr(p, a, addrsPer int) int { return 1 + p*addrsPer + a }
func groupNode(n int) int              { return 1<<30 + n }

func (t *tracker) stats() trackerStats {
	ch := make(chan trackerStats)
	t.statsCh <- ch
	return <-ch
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(p * float64(len(s)-1))
	return s[i]
}
