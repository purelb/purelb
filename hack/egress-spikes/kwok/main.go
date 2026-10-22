// Command loadgen is the Phase 0b spike 8 load generator: it simulates the
// PureLB egress allocator and N node agents against a kwok control plane and
// measures API writes, watch traffic, etcd volume and ack latency for the
// "original" design and the "decided" (D5/D6) design.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	rng2 "math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	Mode          string        `json:"-"`
	Kubeconfig    string        `json:"-"`
	EtcdURL       string        `json:"-"`
	Out           string        `json:"-"`
	Design        string        `json:"design"`
	Nodes         int           `json:"nodes"`
	Policies      int           `json:"policies"`
	AddrsPer      int           `json:"addrsPerPolicy"`
	Pods          int           `json:"pods"`
	QPS           float64       `json:"qps"`
	Burst         int           `json:"burst"`
	AllocQPS      float64       `json:"allocQPS"`
	AllocBurst    int           `json:"allocBurst"`
	AllocWriters  int           `json:"allocWriters"`
	AllocPriority bool          `json:"allocPriority"`
	Reserve       int           `json:"reserve"`
	AckInterval   time.Duration `json:"ackInterval"`
	OrigInterval  time.Duration `json:"origAckInterval"`
	ChurnRate     float64       `json:"churnRatePodsPerSec"`
	ChurnPods     int           `json:"churnPods"`
	BurstClaims   int           `json:"burstClaims"`
	Steady        time.Duration `json:"steady"`
	PhaseCap      time.Duration `json:"phaseCap"`
	LeaseDuration time.Duration `json:"leaseDuration"`
	Seed          int64         `json:"seed"`
	// option A (scoped): routed host-route-mode addresses whose advertiser
	// set the allocator writes into every holder's EgressNodeState spec.
	HostRouteFrac  float64 `json:"hostRouteFrac,omitempty"`
	SelectorNodes  int     `json:"selectorNodes,omitempty"`
	SelectorNoPods bool    `json:"selectorNoPods,omitempty"`
	// A: allocator writes advertisers into holders' specs; C: every
	// selector node advertises every host-route address and watches and
	// acks its slice table (the slice table's announcers = the selector).
	HostRouteModel string `json:"hostRouteModel,omitempty"`
	// scoped: merge windows for ordinary-queue slice-table and node-spec
	// writes (the priority lane is never delayed).
	EPMerge   time.Duration `json:"epMerge,omitempty"`
	SpecMerge time.Duration `json:"specMerge,omitempty"`
}

type phaseResult struct {
	Name        string       `json:"name"`
	Seconds     float64      `json:"seconds"`
	Quiesced    bool         `json:"quiesced"`
	API         phaseMetrics `json:"api"`
	AllocWrites int64        `json:"allocWritesEndpoints"`
	MemberWr    int64        `json:"allocWritesMembers"`
	NodeWr      int64        `json:"allocWritesNodes"` // scoped: EgressNodeState spec writes
	AllocErrs   int64        `json:"allocWriteErrors"`
	AckWrites   int64        `json:"agentAckWrites"`
	ClaimWrites int64        `json:"agentClaimWrites"`
	AgentErrs   int64        `json:"agentWriteErrors"`
	AgentRx     int64        `json:"agentWatchRxBytes"`
	LeaseRenews int64        `json:"leaseRenewals"`
	LeaseLate   int64        `json:"leaseRenewalsLate"`
	LeaseMaxGap float64      `json:"leaseMaxGapSec"`
	AckN        int          `json:"ackN"`
	AckP50      float64      `json:"ackP50"`
	AckP99      float64      `json:"ackP99"`
	AckMax      float64      `json:"ackMax"`
	ClaimN      int          `json:"claimN"`
	ClaimP50    float64      `json:"claimP50"`
	ClaimP99    float64      `json:"claimP99"`
	ClaimMax    float64      `json:"claimMax"`
	Exhausted   int64        `json:"sliceExhausted"`
	// CPU cores used on average during the phase, per component.
	CPUCores map[string]float64 `json:"cpuCores"`
	// Peak memory in MiB during the phase, per component (sampled every second).
	MemPeakMiB map[string]float64 `json:"memPeakMiB"`
	// Allocator queue depth (dirty keys + writes in flight), sampled every
	// 250 ms during the phase.
	AllocQueueMax int64 `json:"allocQueueMax"`
	// scoped: the takeover re-read (D6): one GET of an address's slice table
	// by name through a random agent's own client, every 250 ms (seconds).
	TakeoverN      int     `json:"takeoverN,omitempty"`
	TakeoverP50    float64 `json:"takeoverP50,omitempty"`
	TakeoverP99    float64 `json:"takeoverP99,omitempty"`
	TakeoverMax    float64 `json:"takeoverMax,omitempty"`
	AllocQueueMean float64 `json:"allocQueueMean"`
	// option A: advertiser-set changes; EgressNodeState spec writes that
	// changed only advertisers (extra writes) or advertisers with slices;
	// holders rewritten per change; bytes the advertisers field added to
	// spec writes.
	AdvChanges     int64   `json:"advChanges,omitempty"`
	AdvOnlyWrites  int64   `json:"advOnlyWrites,omitempty"`
	AdvPiggyWrites int64   `json:"advPiggybackWrites,omitempty"`
	AdvHoldersMax  int64   `json:"advHoldersMax,omitempty"`
	AdvHoldersMean float64 `json:"advHoldersMean,omitempty"`
	AdvBytes       int64   `json:"advSpecBytes,omitempty"`
	// option C: ack writes by selector nodes, and the slice-table watch
	// traffic the selector nodes receive on their routed-address watch.
	SelAckWrites int64 `json:"selectorAckWrites,omitempty"`
	SelRxEvents  int64 `json:"selectorWatchEvents,omitempty"`
	SelRxBytes   int64 `json:"selectorWatchBytes,omitempty"`
	// merge windows: ordinary-queue writes held back by a window.
	MergeDeferrals int64 `json:"mergeDeferrals,omitempty"`
	// scoped: writes submitted from the priority lane vs the ordinary queue.
	EPWritesHi int64 `json:"epWritesPriority"`
	EPWritesLo int64 `json:"epWritesOrdinary"`
	NSWritesHi int64 `json:"specWritesPriority"`
	NSWritesLo int64 `json:"specWritesOrdinary"`
}

type sizeStat struct {
	Count int     `json:"count"`
	Avg   float64 `json:"avgBytes"`
	Max   int     `json:"maxBytes"`
}

type runResult struct {
	Config  config              `json:"config"`
	Started time.Time           `json:"started"`
	Phases  []phaseResult       `json:"phases"`
	Sizes   map[string]sizeStat `json:"objectSizes"`
}

func main() {
	var c config
	flag.StringVar(&c.Mode, "mode", "run", "setup | run | report (-out = results dir)")
	flag.StringVar(&c.Kubeconfig, "kubeconfig", "", "kwok cluster kubeconfig")
	flag.StringVar(&c.EtcdURL, "etcd-metrics", "http://127.0.0.1:32379/metrics", "etcd metrics URL")
	flag.StringVar(&c.Out, "out", "", "result JSON path")
	flag.StringVar(&c.Design, "design", "decided", "orig | decided | scoped (acks and slice tables scoped to the nodes involved) | none (baseline: the same pod churn and Lease renewals, no egress)")
	flag.IntVar(&c.Nodes, "nodes", 100, "simulated nodes")
	flag.IntVar(&c.Policies, "policies", 50, "egress policies")
	flag.IntVar(&c.AddrsPer, "addrs", 2, "addresses per policy (alternating IPv4/IPv6)")
	flag.IntVar(&c.Pods, "pods", 5000, "selected pods")
	flag.Float64Var(&c.QPS, "qps", 5, "agent client-go QPS")
	flag.IntVar(&c.Burst, "burst", 10, "agent client-go Burst")
	flag.Float64Var(&c.AllocQPS, "alloc-qps", 0, "allocator QPS (0 = same as -qps)")
	flag.IntVar(&c.AllocBurst, "alloc-burst", 0, "allocator Burst (0 = same as -burst)")
	flag.IntVar(&c.Reserve, "reserve", 1, "unassigned reserve slices kept per address (D17: 1)")
	flag.BoolVar(&c.AllocPriority, "alloc-priority", false, "scoped: claims, activations and the claimant's spec go ahead of other allocator work")
	flag.IntVar(&c.AllocWriters, "alloc-writers", 1, "scoped: allocator writes in flight at once (distinct objects); 1 = one write at a time")
	flag.DurationVar(&c.AckInterval, "ack-interval", time.Second, "decided: min interval between ack writes")
	flag.DurationVar(&c.OrigInterval, "orig-ack-interval", 0, "original: min interval between ack writes (0 = on every change)")
	flag.Float64Var(&c.ChurnRate, "churn-rate", 50, "rolling restart: pod replacements per second")
	flag.IntVar(&c.ChurnPods, "churn-pods", 5000, "rolling restart: pods replaced")
	flag.IntVar(&c.BurstClaims, "burst-claims", 200, "concurrent reserve claims in the burst phase")
	flag.DurationVar(&c.Steady, "steady", 60*time.Second, "steady-state phase length")
	flag.DurationVar(&c.PhaseCap, "phase-cap", 300*time.Second, "max wait for quiescence after a phase's work")
	flag.DurationVar(&c.LeaseDuration, "lease-duration", 10*time.Second, "PureLB lease duration (renew every half); 10s is the shipped default")
	flag.Int64Var(&c.Seed, "seed", 1, "random seed")
	flag.Float64Var(&c.HostRouteFrac, "hostroute-frac", 0, "scoped, option A: fraction of addresses that are routed host-route-mode addresses (D7)")
	flag.IntVar(&c.SelectorNodes, "selector-nodes", 0, "scoped, option A: announcerNodeSelector size (nodes 0..K-1)")
	flag.BoolVar(&c.SelectorNoPods, "selector-nopods", false, "place no selected pods on the -selector-nodes nodes")
	flag.DurationVar(&c.EPMerge, "ep-merge", 0, "scoped: write each EgressEndpoints at most once per window from the ordinary queue (claims and activations are not delayed)")
	flag.DurationVar(&c.SpecMerge, "spec-merge", 0, "scoped: the same window for EgressNodeState spec writes off the priority lane")
	flag.StringVar(&c.HostRouteModel, "hostroute-model", "A", "host-route advertisers: A (allocator writes spec.advertisers to holders) | C (every selector node advertises and acks every host-route address)")
	flag.Parse()
	if c.AllocQPS == 0 {
		c.AllocQPS = c.QPS
	}
	if c.AllocBurst == 0 {
		c.AllocBurst = c.Burst
	}
	if c.Reserve < 1 {
		must(fmt.Errorf("-reserve must be at least 1"))
	}
	if c.AllocWriters < 1 || ((c.AllocWriters > 1 || c.AllocPriority) && c.Design != "scoped") {
		must(fmt.Errorf("-alloc-writers > 1 and -alloc-priority need -design scoped (the decided design's global epoch assumes one write at a time)"))
	}
	if c.HostRouteFrac < 0 || c.HostRouteFrac > 1 {
		must(fmt.Errorf("-hostroute-frac must be in [0,1]"))
	}
	if c.HostRouteFrac > 0 && (c.Design != "scoped" || c.SelectorNodes < 1 || c.SelectorNodes > c.Nodes) {
		must(fmt.Errorf("-hostroute-frac needs -design scoped and 1 <= -selector-nodes <= -nodes"))
	}
	if (c.EPMerge > 0 || c.SpecMerge > 0) && c.Design != "scoped" {
		must(fmt.Errorf("-ep-merge and -spec-merge need -design scoped"))
	}
	if c.HostRouteModel != "A" && c.HostRouteModel != "C" {
		must(fmt.Errorf("-hostroute-model must be A or C"))
	}
	if c.SelectorNoPods && (c.SelectorNodes < 1 || c.SelectorNodes >= c.Nodes) {
		must(fmt.Errorf("-selector-nopods needs 1 <= -selector-nodes < -nodes"))
	}
	if c.Mode == "report" {
		must(report(c.Out))
		return
	}
	ctx := context.Background()
	base, err := adminConfig(c.Kubeconfig)
	must(err)
	admin, err := newClient(base, 2000, 4000)
	must(err)
	switch c.Mode {
	case "setup":
		must(setup(ctx, admin, c.Nodes))
	case "run":
		res := run(ctx, c, admin)
		b, _ := json.MarshalIndent(res, "", "  ")
		if c.Out != "" {
			must(os.WriteFile(c.Out, b, 0o644))
		}
		printSummary(res)
	default:
		must(fmt.Errorf("unknown mode %q", c.Mode))
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func addrFor(p, a int) (string, string) {
	if a%2 == 0 {
		return fmt.Sprintf("172.32.%d.%d", (p*8+a/2)/250, (p*8+a/2)%250+1), "IPv4"
	}
	return fmt.Sprintf("2001:db8:e9::%x:%x", p, a), "IPv6"
}

func run(ctx context.Context, c config, admin *client) runResult {
	res := runResult{Config: c, Started: time.Now(), Sizes: map[string]sizeStat{}}
	rng := rand.New(rand.NewSource(c.Seed))
	base, _ := adminConfig(c.Kubeconfig)
	// pods go on nodes [podFirst, N): with -selector-nopods the selector
	// nodes (0..K-1) run no selected pods.
	podFirst := 0
	if c.SelectorNoPods {
		podFirst = c.SelectorNodes
	}

	allocTok, err := saToken(ctx, admin, "allocator")
	must(err)
	agentTok, err := saToken(ctx, admin, "lbnodeagent")
	must(err)
	allocClient, err := newClient(tokenConfig(base, allocTok), float32(c.AllocQPS), c.AllocBurst)
	must(err)

	logf("cleanup")
	must(cleanup(ctx, admin))
	must(ensurePods(ctx, admin, c.Pods, c.Policies, podFirst, c.Nodes, rng))

	// node UIDs for ownerRefs
	nodeUID := map[string]string{}
	items, _, err := admin.listAll(ctx, "/api/v1/nodes", map[string]string{"labelSelector": "type=kwok"})
	must(err)
	for _, it := range items {
		var m struct {
			Metadata struct{ Name, UID string } `json:"metadata"`
		}
		_ = json.Unmarshal(it, &m)
		nodeUID[m.Metadata.Name] = m.Metadata.UID
	}
	nodes := make([]string, c.Nodes)
	for i := range nodes {
		nodes[i] = nodeName(i)
	}

	// policies
	policies := make([]*policyState, c.Policies)
	uidToPolicy := map[string]int{}
	addrIdx := make([]map[string]int, c.Policies)
	sh := &agentShared{design: c.Design, ackInterval: c.AckInterval, origInterval: c.OrigInterval,
		leaseDur: c.LeaseDuration, leaseRenew: c.LeaseDuration / 2}
	if c.HostRouteFrac > 0 {
		sh.selK = c.SelectorNodes
		sh.routedWatch = c.HostRouteModel == "C"
	}
	sh.policyUIDs = make([]string, c.Policies)
	sh.addrs = make([][]string, c.Policies)
	// none: the baseline. Pods, their churn and every node's Lease renewal
	// stay; no egress object is created, watched or written.
	egress := c.Design != "none"
	must(parallel(c.Policies, 32, func(p int) error {
		name := fmt.Sprintf("pol-%d", p)
		if !egress {
			policies[p] = &policyState{idx: p, name: name}
			return nil
		}
		raw, err := admin.post(ctx, gv+"/namespaces/"+nsLoad+"/egresses", []byte(fmt.Sprintf(
			`{"apiVersion":"%s/v1","kind":"Egress","metadata":{"name":%q,"namespace":%q},"spec":{"podSelector":{"matchLabels":{"egress":%q}},"serviceGroup":"egress-pool","addresses":%d,"failurePolicy":"FailClosed"}}`,
			group, name, nsLoad, name, c.AddrsPer)))
		if err != nil {
			return err
		}
		var m struct {
			Metadata struct{ UID string } `json:"metadata"`
		}
		_ = json.Unmarshal(raw, &m)
		ps := &policyState{idx: p, name: name, uid: m.Metadata.UID}
		for a := 0; a < c.AddrsPer; a++ {
			addr, fam := addrFor(p, a)
			k := (p*c.AddrsPer + a) * 7
			i := p*c.AddrsPer + a // spread host-route addresses evenly over policies and families
			hr := float64(i%100) < c.HostRouteFrac*100
			ann := []string{nodes[k%c.Nodes], nodes[(k+1)%c.Nodes]}
			if hr && c.HostRouteModel == "C" {
				ann = append([]string(nil), nodes[:c.SelectorNodes]...)
			}
			ps.addrs = append(ps.addrs, &addrState{p: p, a: a, address: addr, family: fam,
				announcers: ann, nextStart: portFirst, claims: map[string]int64{}, hostRoute: hr})
		}
		policies[p] = ps
		return nil
	}))
	for p, ps := range policies {
		uidToPolicy[ps.uid] = p
		sh.policyUIDs[p] = ps.uid
		addrIdx[p] = map[string]int{}
		for a, as := range ps.addrs {
			sh.addrs[p] = append(sh.addrs[p], as.address)
			addrIdx[p][as.address] = a
		}
	}

	// leases and EgressNodeState objects (setup, bulk via admin)
	nsKind, nsPath := "EgressNodeState", gv+"/egressnodestates"
	if c.Design == "orig" {
		nsKind, nsPath = "OrigEgressNodeState", gv+"/origegressnodestates"
	}
	must(parallel(c.Nodes, 64, func(i int) error {
		n := nodes[i]
		now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
		_, err := admin.post(ctx, "/apis/coordination.k8s.io/v1/namespaces/"+nsPureLB+"/leases", []byte(fmt.Sprintf(
			`{"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":"purelb-node-%s","labels":{%q:"1"}},"spec":{"holderIdentity":%q,"leaseDurationSeconds":%d,"renewTime":%q}}`,
			n, leaseLabel, n, int(c.LeaseDuration.Seconds()), now)))
		if ignoreExists(err) != nil || !egress {
			return ignoreExists(err)
		}
		meta := map[string]any{"name": n}
		if c.Design != "orig" { // D6: EgressNodeState has an ownerRef to its Node
			meta["ownerReferences"] = []map[string]any{{"apiVersion": "v1", "kind": "Node", "name": n, "uid": nodeUID[n]}}
		}
		_, err = admin.post(ctx, nsPath, mustJSON(map[string]any{"apiVersion": group + "/v1", "kind": nsKind, "metadata": meta}))
		return ignoreExists(err)
	}))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	actCh := make(chan activation, 1<<16)
	tr := newTracker(c.Nodes, c.Design, c.LeaseDuration, actCh)
	go tr.run(runCtx)
	al := newAllocator(c.Design, allocClient, admin, policies, nodes, tr, actCh)
	al.writers = c.AllocWriters
	al.priority = c.AllocPriority
	al.optC = c.HostRouteModel == "C"
	al.epMerge, al.specMerge = c.EPMerge, c.SpecMerge
	al.reserve = c.Reserve
	al.setSelector(nodes[:c.SelectorNodes])

	// allocator watches: pods, leases, node states
	podsListed := make(chan struct{})
	var podsOnce sync.Once
	toPod := func(raw json.RawMessage) (podEvent, bool) {
		var pm podMini
		if json.Unmarshal(raw, &pm) != nil {
			return podEvent{}, false
		}
		p, err := strconv.Atoi(pm.Metadata.Labels["spike.purelb.io/policy-idx"])
		if err != nil {
			return podEvent{}, false
		}
		return podEvent{key: pm.Metadata.Name, pod: podInfo{name: pm.Metadata.Name, uid: pm.Metadata.UID, node: pm.Spec.NodeName,
			ip: pm.Status.PodIP, policy: p, terminating: pm.Metadata.DeletionTimestamp != nil}}, true
	}
	if egress {
		go allocClient.watchLoop(runCtx, "/api/v1/namespaces/"+nsLoad+"/pods", nil, func(items []json.RawMessage) {
			for _, it := range items {
				if ev, ok := toPod(it); ok {
					al.podCh <- ev
				}
			}
			podsOnce.Do(func() { close(podsListed) })
		}, func(typ string, raw json.RawMessage) {
			if ev, ok := toPod(raw); ok {
				ev.deleted = typ == "DELETED"
				al.podCh <- ev
			}
		}, nil)
		toLease := func(raw json.RawMessage) {
			var l struct {
				Spec struct {
					HolderIdentity string `json:"holderIdentity"`
					RenewTime      string `json:"renewTime"`
				} `json:"spec"`
			}
			if json.Unmarshal(raw, &l) != nil {
				return
			}
			var i int
			if _, err := fmt.Sscanf(l.Spec.HolderIdentity, "kwok-node-%d", &i); err != nil || i >= c.Nodes {
				return
			}
			t, err := time.Parse(time.RFC3339Nano, l.Spec.RenewTime)
			if err != nil {
				return
			}
			tr.leaseCh <- leaseUpdate{node: i, renew: t}
		}
		go allocClient.watchLoop(runCtx, "/apis/coordination.k8s.io/v1/namespaces/"+nsPureLB+"/leases",
			map[string]string{"labelSelector": leaseLabel + "=1"},
			func(items []json.RawMessage) {
				for _, it := range items {
					toLease(it)
				}
			}, func(_ string, raw json.RawMessage) { toLease(raw) }, nil)
		toState := func(raw json.RawMessage) {
			var s struct {
				Metadata struct{ Name string } `json:"metadata"`
				Status   struct {
					Observed               []observedJSON `json:"observed"`
					ObservedEpoch          int64          `json:"observedEpoch"`
					ObservedAddrs          []addrAckJSON  `json:"observedAddrs"`
					ObservedNodeGeneration int64          `json:"observedNodeGeneration"`
					Claims                 []claimJSON    `json:"claims"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &s) != nil {
				return
			}
			var n int
			if _, err := fmt.Sscanf(s.Metadata.Name, "kwok-node-%d", &n); err != nil || n >= c.Nodes {
				return
			}
			u := ackUpdate{node: n, vers: map[int]int64{}}
			if c.Design == "orig" {
				for _, o := range s.Status.Observed {
					if p, ok := uidToPolicy[o.EgressUID]; ok {
						u.vers[p] = o.Generation
					}
				}
			} else if c.Design == "scoped" {
				for _, o := range s.Status.ObservedAddrs {
					var p, a int
					if _, err := fmt.Sscanf(o.Name, "pol-%d-a%d", &p, &a); err == nil {
						u.vers[groupAddr(p, a, c.AddrsPer)] = o.Generation
					}
				}
				u.vers[groupNode(n)] = s.Status.ObservedNodeGeneration
			} else {
				u.vers[0] = s.Status.ObservedEpoch
			}
			tr.ackCh <- u
			for _, cl := range s.Status.Claims {
				p, ok := uidToPolicy[cl.EgressUID]
				if !ok {
					continue
				}
				al.claimCh <- claimMsg{node: s.Metadata.Name, policy: p, addr: addrIdx[p][cl.Address], ts: cl.TS}
			}
		}
		go allocClient.watchLoop(runCtx, nsPath, nil, func(items []json.RawMessage) {
			for _, it := range items {
				toState(it)
			}
		}, func(_ string, raw json.RawMessage) { toState(raw) }, nil)

		<-podsListed
		time.Sleep(2 * time.Second) // let lease list reach the tracker
		al.drain()
		logf("bootstrap: creating egress objects")
		al.bootstrap(ctx)
		close(al.ready)
		go al.run(runCtx)
	}

	logf("starting %d agents (qps %.0f burst %d)", c.Nodes, c.QPS, c.Burst)
	agents := make([]*agent, c.Nodes)
	agentCfg := tokenConfig(base, agentTok)
	for i := range agents {
		ac, err := newClient(agentCfg, float32(c.QPS), c.Burst)
		must(err)
		agents[i] = newAgent(i, nodes[i], sh, ac)
		agents[i].start(runCtx)
	}
	// scoped: measure the takeover re-read. The probe goroutine owns nothing
	// shared; it sends each latency on a channel the phase code drains.
	takeCh := make(chan float64, 1<<14)
	if c.Design == "scoped" {
		go func() {
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
				}
				p, a := rng2.IntN(c.Policies), rng2.IntN(c.AddrsPer)
				start := time.Now()
				if _, err := agents[rng2.IntN(c.Nodes)].c.get(runCtx, fmt.Sprintf("%s/namespaces/%s/egressendpoints/pol-%d-a%d", gv, nsPureLB, p, a)); err == nil {
					select {
					case takeCh <- time.Since(start).Seconds():
					default:
					}
				}
			}
		}()
	}
	drainTake := func() []float64 {
		var xs []float64
		for {
			select {
			case x := <-takeCh:
				xs = append(xs, x)
			default:
				return xs
			}
		}
	}
	deadline := time.Now().Add(c.PhaseCap)
	for time.Now().Before(deadline) {
		st := tr.stats()
		if (!egress || sh.readyAgents.Load() == int64(c.Nodes)) && st.pending == 0 && al.stats.dirty.Load() == 0 {
			break
		}
		logf("bootstrap: agents ready %d/%d, pending %d", sh.readyAgents.Load(), c.Nodes, st.pending)
		time.Sleep(2 * time.Second)
	}
	time.Sleep(10 * time.Second)

	quiet := func(minWait time.Duration) bool {
		start := time.Now()
		stable := 0
		for time.Since(start) < c.PhaseCap {
			st := tr.stats()
			// keep latency samples: put them back by not resetting -- stats()
			// resets, so accumulate into the phase collector
			collect(st)
			idle := st.pending == 0 && al.stats.dirty.Load() == 0 && len(al.podCh) == 0 && len(al.claimCh) == 0 && len(al.actCh) == 0
			if idle && time.Since(start) >= minWait {
				stable++
				if stable >= 3 {
					return true
				}
			} else {
				stable = 0
			}
			time.Sleep(time.Second)
		}
		return false
	}

	used := map[pa]bool{}
	podItems, _, err := admin.listAll(ctx, "/api/v1/namespaces/"+nsLoad+"/pods", nil)
	must(err)
	type podRef struct {
		name string
		p    int
	}
	var current []podRef
	for _, it := range podItems {
		var pm podMini
		_ = json.Unmarshal(it, &pm)
		p, _ := strconv.Atoi(pm.Metadata.Labels["spike.purelb.io/policy-idx"])
		var ni int
		fmt.Sscanf(pm.Spec.NodeName, "kwok-node-%d", &ni)
		used[pa{p, ni}] = true
		current = append(current, podRef{pm.Metadata.Name, p})
	}

	mw := newMemWatch(ctx, "spike")
	phase := func(name string, work func()) {
		logf("phase %s: start", name)
		mw.reset()
		drainTake() // discard samples from before the phase
		qStop := make(chan struct{})
		qDone := make(chan [2]float64)
		go func() { // only this goroutine touches its counters
			var max, sum, n int64
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-qStop:
					mean := 0.0
					if n > 0 {
						mean = float64(sum) / float64(n)
					}
					qDone <- [2]float64{float64(max), mean}
					return
				case <-t.C:
					d := al.stats.dirty.Load()
					if d > max {
						max = d
					}
					sum += d
					n++
				}
			}
		}()
		resetCollect()
		tr.stats() // discard samples from before the phase
		sh.leaseMaxGap.Store(0)
		a0, err := scrapeAPIServer(ctx, admin)
		must(err)
		e0, _ := scrapeEtcd(ctx, c.EtcdURL)
		aw0, mw0, ae0 := al.stats.writesEndpoints.Load(), al.stats.writesMembers.Load(), al.stats.writeErrors.Load()
		nw0 := al.stats.writesNodes.Load()
		adc0, adh0, ado0, adp0, adb0 := al.stats.advChanges.Load(), al.stats.advHoldersSum.Load(), al.stats.advOnlyWrites.Load(), al.stats.advPiggyWrites.Load(), al.stats.advBytes.Load()
		al.stats.advHoldersMax.Store(0)
		ack0, cl0, er0, rx0 := sh.ackWrites.Load(), sh.claimWrites.Load(), sh.writeErrors.Load(), sh.rxBytes.Load()
		lr0, ll0, ex0 := sh.leaseRenews.Load(), sh.leaseLate.Load(), al.stats.exhausted.Load()
		sa0, se0, sb0 := sh.selAckWrites.Load(), sh.selRxEvents.Load(), sh.selRxBytes.Load()
		md0 := al.stats.mergeDeferrals.Load()
		eh0, el0, nh0, nl0 := al.stats.epHi.Load(), al.stats.epLo.Load(), al.stats.nsHi.Load(), al.stats.nsLo.Load()
		cpu0 := cpuSample("spike")
		t0 := time.Now()
		work()
		q := quiet(0)
		dur := time.Since(t0).Seconds()
		a1, err := scrapeAPIServer(ctx, admin)
		must(err)
		e1, _ := scrapeEtcd(ctx, c.EtcdURL)
		acks, claims := collected()
		cpu1 := cpuSample("spike")
		cores := map[string]float64{}
		for k, v := range cpu1 {
			cores[k] = (v - cpu0[k]) / dur
		}
		pr := phaseResult{Name: name, Seconds: dur, Quiesced: q, API: diff(a0, a1, e0, e1),
			AllocWrites: al.stats.writesEndpoints.Load() - aw0, MemberWr: al.stats.writesMembers.Load() - mw0, NodeWr: al.stats.writesNodes.Load() - nw0,
			AllocErrs: al.stats.writeErrors.Load() - ae0, AckWrites: sh.ackWrites.Load() - ack0,
			ClaimWrites: sh.claimWrites.Load() - cl0, AgentErrs: sh.writeErrors.Load() - er0, AgentRx: sh.rxBytes.Load() - rx0,
			LeaseRenews: sh.leaseRenews.Load() - lr0, LeaseLate: sh.leaseLate.Load() - ll0,
			LeaseMaxGap: time.Duration(sh.leaseMaxGap.Load()).Seconds(), Exhausted: al.stats.exhausted.Load() - ex0,
			AckN: len(acks), AckP50: percentile(acks, .5), AckP99: percentile(acks, .99), AckMax: percentile(acks, 1),
			ClaimN: len(claims), ClaimP50: percentile(claims, .5), ClaimP99: percentile(claims, .99), ClaimMax: percentile(claims, 1),
			CPUCores: cores, MemPeakMiB: mw.peaks(),
		}
		pr.MergeDeferrals = al.stats.mergeDeferrals.Load() - md0
		pr.EPWritesHi, pr.EPWritesLo = al.stats.epHi.Load()-eh0, al.stats.epLo.Load()-el0
		pr.NSWritesHi, pr.NSWritesLo = al.stats.nsHi.Load()-nh0, al.stats.nsLo.Load()-nl0
		pr.SelAckWrites = sh.selAckWrites.Load() - sa0
		pr.SelRxEvents = sh.selRxEvents.Load() - se0
		pr.SelRxBytes = sh.selRxBytes.Load() - sb0
		pr.AdvChanges = al.stats.advChanges.Load() - adc0
		pr.AdvOnlyWrites = al.stats.advOnlyWrites.Load() - ado0
		pr.AdvPiggyWrites = al.stats.advPiggyWrites.Load() - adp0
		pr.AdvHoldersMax = al.stats.advHoldersMax.Load()
		pr.AdvBytes = al.stats.advBytes.Load() - adb0
		if pr.AdvChanges > 0 {
			pr.AdvHoldersMean = float64(al.stats.advHoldersSum.Load()-adh0) / float64(pr.AdvChanges)
		}
		close(qStop)
		qs := <-qDone
		pr.AllocQueueMax, pr.AllocQueueMean = int64(qs[0]), qs[1]
		if tk := drainTake(); len(tk) > 0 {
			pr.TakeoverN, pr.TakeoverP50, pr.TakeoverP99, pr.TakeoverMax = len(tk), percentile(tk, .5), percentile(tk, .99), percentile(tk, 1)
		}
		res.Phases = append(res.Phases, pr)
		logf("phase %s: done in %.1fs quiesced=%v allocWrites=%d memberWrites=%d nodeSpecWrites=%d ackWrites=%d claimWrites=%d ack p50/p99 %.2f/%.2fs claim p50/p99 %.2f/%.2fs (n=%d) memPeakMiB=%v allocQueue max=%d mean=%.1f takeover p50/p99 %.3f/%.3fs (n=%d) advChanges=%d advOnlyWrites=%d advPiggyback=%d advHolders max/mean %d/%.1f advSpecBytes=%d",
			name, dur, q, pr.AllocWrites, pr.MemberWr, pr.NodeWr, pr.AckWrites, pr.ClaimWrites, pr.AckP50, pr.AckP99, pr.ClaimP50, pr.ClaimP99, pr.ClaimN, pr.MemPeakMiB, pr.AllocQueueMax, pr.AllocQueueMean, pr.TakeoverP50, pr.TakeoverP99, pr.TakeoverN,
			pr.AdvChanges, pr.AdvOnlyWrites, pr.AdvPiggyWrites, pr.AdvHoldersMax, pr.AdvHoldersMean, pr.AdvBytes)
	}

	phase("steady", func() { time.Sleep(c.Steady) })

	phase("rollout", func() {
		// interleave policies, as if every Deployment restarts at once
		sort.SliceStable(current, func(i, j int) bool { return current[i].name < current[j].name })
		byP := map[int][]podRef{}
		for _, pr := range current {
			byP[pr.p] = append(byP[pr.p], pr)
		}
		var order []podRef
		for i := 0; len(order) < len(current); i++ {
			for p := 0; p < c.Policies; p++ {
				if i < len(byP[p]) {
					order = append(order, byP[p][i])
				}
			}
		}
		if c.ChurnPods < len(order) {
			order = order[:c.ChurnPods]
		}
		tick := time.NewTicker(time.Duration(float64(time.Second) / c.ChurnRate))
		defer tick.Stop()
		var wg sync.WaitGroup
		sem := make(chan struct{}, 512)
		var fails atomic.Int64
		for _, old := range order {
			<-tick.C
			ni := podFirst + rng.Intn(c.Nodes-podFirst)
			used[pa{old.p, ni}] = true
			sem <- struct{}{}
			wg.Add(1)
			go func(old podRef, ni int) {
				defer wg.Done()
				defer func() { <-sem }()
				name := fmt.Sprintf("pol-%d-%s", old.p, randSuffix())
				if _, err := admin.post(ctx, "/api/v1/namespaces/"+nsLoad+"/pods", podBody(name, old.p, c.Policies, nodes[ni])); err != nil {
					fails.Add(1)
					return
				}
				agents[ni].cniCh <- old.p // CNI ADD on the new pod's node
				if err := admin.delete(ctx, "/api/v1/namespaces/"+nsLoad+"/pods/"+old.name, nil); err != nil {
					fails.Add(1)
				}
			}(old, ni)
		}
		wg.Wait()
		logf("rollout: replaced %d pods (%d errors)", len(order), fails.Load())
	})

	phase("claim-burst", func() {
		// B new pods, each on a node that never ran a pod of its policy,
		// so every one is a reserve-slice claim; all at once.
		type pair struct{ p, n int }
		var pairs []pair
		perm := rng.Perm(c.Nodes)
		for i := 0; len(pairs) < c.BurstClaims && i < c.BurstClaims*20; i++ {
			n := perm[i%c.Nodes]
			p := rng.Intn(c.Policies)
			if n < podFirst || used[pa{p, n}] {
				continue
			}
			used[pa{p, n}] = true
			pairs = append(pairs, pair{p, n})
		}
		var wg sync.WaitGroup
		for _, pp := range pairs {
			wg.Add(1)
			go func(pp pair) {
				defer wg.Done()
				name := fmt.Sprintf("pol-%d-%s", pp.p, randSuffix())
				if _, err := admin.post(ctx, "/api/v1/namespaces/"+nsLoad+"/pods", podBody(name, pp.p, c.Policies, nodes[pp.n])); err != nil {
					logf("burst pod: %v", err)
					return
				}
				agents[pp.n].cniCh <- pp.p
			}(pp)
		}
		wg.Wait()
		logf("claim-burst: %d claims issued (%d addresses each)", len(pairs), c.AddrsPer)
		time.Sleep(2 * time.Second)
	})

	// object sizes as served (JSON; CRDs are stored in etcd as JSON)
	for _, r := range []string{
		gv + "/origegressendpoints", gv + "/origegressnodestates",
		gv + "/egressendpoints", gv + "/egressmembers", gv + "/egressnodestates",
	} {
		items, _, err := admin.listAll(ctx, r, nil)
		if err != nil || len(items) == 0 {
			continue
		}
		st := sizeStat{Count: len(items)}
		tot := 0
		for _, it := range items {
			n := len(compact(it))
			tot += n
			if n > st.Max {
				st.Max = n
			}
		}
		st.Avg = float64(tot) / float64(len(items))
		res.Sizes[r[strings.LastIndexByte(r, '/')+1:]] = st
	}
	cancel()
	return res
}

// latency sample collector shared by quiet() and phase() (same goroutine).
var colAcks, colClaims []float64

func resetCollect() { colAcks, colClaims = nil, nil }
func collect(st trackerStats) {
	colAcks = append(colAcks, st.ackLat...)
	colClaims = append(colClaims, st.claimLat...)
}
func collected() ([]float64, []float64) { return colAcks, colClaims }

func printSummary(r runResult) {
	c := r.Config
	fmt.Printf("\n== %s N=%d P=%d A=%d E=%d qps=%.0f/%d ackInterval=%s ==\n", c.Design, c.Nodes, c.Policies, c.AddrsPer, c.Pods, c.QPS, c.Burst, c.AckInterval)
	for _, p := range r.Phases {
		fmt.Printf("-- %s %.1fs quiesced=%v\n", p.Name, p.Seconds, p.Quiesced)
		keys := make([]string, 0, len(p.API.Writes))
		for k := range p.API.Writes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("   write %-45s %8.0f  %8.2f/s\n", k, p.API.Writes[k], p.API.Writes[k]/p.Seconds)
		}
		keys = keys[:0]
		for k := range p.API.WatchBytes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("   watch %-45s %8.0f ev/s %10.0f B/s\n", k, p.API.WatchEvents[k]/p.Seconds, p.API.WatchBytes[k]/p.Seconds)
		}
		fmt.Printf("   etcd %v\n   429s %v apf %v\n", p.API.Etcd, p.API.Rejected429, p.API.APFRejected)
		fmt.Printf("   cpu cores %v\n", p.CPUCores)
		fmt.Printf("   mem peak MiB %v\n", p.MemPeakMiB)
		fmt.Printf("   ack n=%d p50=%.3f p99=%.3f max=%.3f | claim n=%d p50=%.3f p99=%.3f max=%.3f | lease late=%d maxGap=%.1fs\n",
			p.AckN, p.AckP50, p.AckP99, p.AckMax, p.ClaimN, p.ClaimP50, p.ClaimP99, p.ClaimMax, p.LeaseLate, p.LeaseMaxGap)
		fmt.Printf("   allocator writes: endpoints=%d members=%d nodeSpec=%d errors=%d | agent ack=%d claim=%d errors=%d\n",
			p.AllocWrites, p.MemberWr, p.NodeWr, p.AllocErrs, p.AckWrites, p.ClaimWrites, p.AgentErrs)
		if c.Design == "scoped" {
			fmt.Printf("   lanes: slice-table writes priority=%d ordinary=%d | node-spec writes priority=%d ordinary=%d | slice-table versions/address=%.2f\n",
				p.EPWritesHi, p.EPWritesLo, p.NSWritesHi, p.NSWritesLo, float64(p.AllocWrites)/float64(c.Policies*c.AddrsPer))
		}
		if c.EPMerge > 0 || c.SpecMerge > 0 {
			fmt.Printf("   merge: epMerge=%s specMerge=%s deferrals=%d slice-table versions/address=%.2f\n",
				c.EPMerge, c.SpecMerge, p.MergeDeferrals, float64(p.AllocWrites)/float64(c.Policies*c.AddrsPer))
		}
		if c.HostRouteFrac > 0 && c.HostRouteModel == "C" {
			k := float64(c.SelectorNodes)
			fmt.Printf("   optionC: selectorAckWrites=%d (of %d acks) selectorSliceTableWatch events=%d bytes=%d | per selector node %.1f ev/s %.0f B/s\n",
				p.SelAckWrites, p.AckWrites, p.SelRxEvents, p.SelRxBytes, float64(p.SelRxEvents)/p.Seconds/k, float64(p.SelRxBytes)/p.Seconds/k)
		} else if c.HostRouteFrac > 0 {
			fmt.Printf("   optionA: advChanges=%d advOnlyWrites=%d (%.2f/s) advPiggyback=%d holders/change max=%d mean=%.1f advSpecBytes=%d (%.0f B/s)\n",
				p.AdvChanges, p.AdvOnlyWrites, float64(p.AdvOnlyWrites)/p.Seconds, p.AdvPiggyWrites, p.AdvHoldersMax, p.AdvHoldersMean, p.AdvBytes, float64(p.AdvBytes)/p.Seconds)
		}
	}
	for k, s := range r.Sizes {
		fmt.Printf("size %-24s n=%d avg=%.0f max=%d\n", k, s.Count, s.Avg, s.Max)
	}
}
