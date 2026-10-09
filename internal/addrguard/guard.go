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

package addrguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"

	"github.com/go-kit/log"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// portSpec is one allowed (protocol, port) on a VIP.
type portSpec struct {
	proto uint8
	port  uint16
}

// svcState is what one Service contributes: its ingress addresses, and the
// ports allowed on each of them.
type svcState struct {
	vips  []netip.Addr
	ports []portSpec
}

func (s svcState) equal(o svcState) bool {
	return slices.Equal(s.vips, o.vips) && slices.Equal(s.ports, o.ports)
}

// Guard is the address guard announcer. It keeps the BPF maps in step with
// the PureLB Services: every VIP of every PureLB Service is guarded on every
// node (guarding a VIP this node doesn't hold is harmless, and it means the
// rules exist before any node adds the address).
//
// Concurrency, without locks (see CLAUDE.md):
//   - The Service state below is owned by the service-sync goroutine, the
//     only caller of SetBalancer and DeleteBalancer.
//   - Interface attachment and the config map are owned by the attacher
//     goroutine (attach.go), fed by SetConfig through a one-slot channel.
//   - What the metrics collector needs across goroutines is in atomics.
//   - BPF maps are kernel objects, safe for concurrent use; they are never
//     closed while the process runs.
type Guard struct {
	logger  log.Logger
	myNode  string
	dp      dataplane
	objs    *addrguardObjects // nil when the program could not be loaded
	loadErr error

	client atomic.Pointer[k8s.Client]

	// Service-sync goroutine state.
	perService     map[string]svcState
	vipRefs        map[netip.Addr]int
	ports          map[netip.Addr]map[portSpec]int
	installedVIP   map[netip.Addr]bool
	installedPorts map[netip.Addr]map[portSpec]bool
	failed         map[netip.Addr]bool

	// Read by the metrics collector and, through StandingDown, by the node
	// agent and the local announcer.
	vipCount    atomic.Int64
	failedCount atomic.Int64
	// Under failurePolicy closed, failed VIPs that aren't announced
	// (withheld) and ones filtered by rules that don't match the Service
	// yet (incomplete).
	withheldCount   atomic.Int64
	incompleteCount atomic.Int64
	configured      atomic.Bool
	closed          atomic.Bool // failurePolicy: closed
	// down is the stand-down verdict StandingDown returns. The attacher
	// decides it (publish) and calls the hook whenever it changes, so every
	// stand-down a Service sync acts on is followed by a re-sync when it
	// ends. The one other writer is SetConfig before the attacher's first
	// apply (applied), and it only ever sets it: see SetConfig.
	down          atomic.Bool
	applied       atomic.Bool
	standDownHook atomic.Pointer[func(bool)]
	// loadWanted is the startup decision: the guard was configured for this
	// node when the agent started, so the program was loaded (or tried to
	// be). Fixed before any goroutine runs. The program is loaded only for a
	// guard that is enabled, so enabling it later, or disabling it, takes
	// effect fully at the next agent restart: restartRequired says so.
	loadWanted      bool
	restartRequired atomic.Bool

	// Attacher goroutine.
	cfgCh chan configUpdate
	stop  chan struct{}
	done  chan struct{}
	// retained keeps the attachments reachable after the attacher stops:
	// cilium/ebpf closes a link's fd when it is garbage collected, which
	// would detach the guard before the process exits.
	retained map[int]*attachment
}

// configUpdate is what SetConfig hands the attacher: the parsed spec (nil
// means disabled) and the agent it came from, for events.
type configUpdate struct {
	spec  *Spec
	agent *purelbv2.LBNodeAgent
}

// Spec is the parsed address guard configuration.
type Spec struct {
	Monitor bool
	XDP     bool
	// Closed is failurePolicy: closed -- stand down rather than announce
	// unguarded.
	Closed           bool
	AllowedProtocols []uint8
	Extra, Exclude   []string
	// Dummy is the interface carrying remote VIPs, whose kernel-generated
	// broadcast and anycast addresses are guarded too.
	Dummy string
}

// NewAnnouncer starts the guard's attacher, loading the guard program only
// if enabled -- the guard is configured for this node at startup (Enabled).
// A guard that isn't enabled loads nothing into the kernel. A load failure
// is not an error: the failure policy decides what the node does, once the
// guard is configured.
func NewAnnouncer(l log.Logger, myNode string, enabled bool) *Guard {
	var objs *addrguardObjects
	var err error
	if enabled {
		objs, err = loadProgram()
	} else {
		logging.Info(l, "op", "addressGuard", "event", "guardNotLoaded",
			"msg", "address guard not configured for this node at startup; the program is not loaded")
	}
	g := newGuard(l, myNode, enabled, objs, err)
	if objs != nil {
		g.dp = &mapDataplane{objs: objs}
		logging.Info(l, "op", "addressGuard", "event", "guardLoaded", "maxMapBytes", worstCaseMapBytes())
	}
	registerCollector(g)
	go g.run()
	return g
}

// Enabled reports whether the address guard is configured for a node with
// nodeLabels, given every LBNodeAgent: the same resolution the node agent
// applies to each config delivery (AgentsForNode, then FirstLocalAgent).
// The node agent asks once, at startup, to decide whether to load the
// program.
func Enabled(agents []*purelbv2.LBNodeAgent, nodeLabels map[string]string) bool {
	spec, _ := specFor(&purelbv2.Config{Agents: purelbv2.AgentsForNode(agents, nodeLabels)})
	return spec != nil
}

func newGuard(l log.Logger, myNode string, loadWanted bool, objs *addrguardObjects, loadErr error) *Guard {
	return &Guard{
		logger:         l,
		myNode:         myNode,
		loadWanted:     loadWanted,
		dp:             nopDataplane{},
		objs:           objs,
		loadErr:        loadErr,
		perService:     map[string]svcState{},
		vipRefs:        map[netip.Addr]int{},
		ports:          map[netip.Addr]map[portSpec]int{},
		installedVIP:   map[netip.Addr]bool{},
		installedPorts: map[netip.Addr]map[portSpec]bool{},
		failed:         map[netip.Addr]bool{},
		cfgCh:          make(chan configUpdate, 1),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
}

// StandingDown reports whether this node must announce nothing: the guard
// is configured fail-closed and isn't working (not loaded, an interface not
// guarded, or not attached yet at startup). Safe from any goroutine.
func (g *Guard) StandingDown() bool {
	return g.down.Load()
}

// SetStandDownHook registers fn, called from the attacher goroutine with
// the new StandingDown() whenever it changes. The node agent uses it to
// pull the node out of (or back into) elections and re-sync Services.
func (g *Guard) SetStandDownHook(fn func(standingDown bool)) {
	g.standDownHook.Store(&fn)
}

// SetClient implements lbnodeagent.Announcer.
func (g *Guard) SetClient(c *k8s.Client) { g.client.Store(c) }

// SetElection implements lbnodeagent.Announcer. The guard doesn't use the
// election: it guards every VIP whether or not this node holds it.
func (g *Guard) SetElection(*election.Election) {}

// SetConfig implements lbnodeagent.Announcer. It never fails and never
// blocks: CEL has validated the input, and attaching (which can take a
// while for XDP) happens on the attacher goroutine. An error here would
// also fail the local announcer's config sync.
func (g *Guard) SetConfig(cfg *purelbv2.Config) error {
	spec, agent := specFor(cfg)
	g.configured.Store(spec != nil)
	g.closed.Store(spec != nil && spec.Closed)
	// At startup nothing is attached yet, so a fail-closed node stands down
	// from the moment its config lands until the guard has attached: no
	// unguarded window. After the first apply the attacher alone decides,
	// and a live change (e.g. turning the guard on) tries to attach first,
	// standing down only if that fails -- the VIPs were unfiltered a moment
	// earlier anyway, and a stand-down here would withdraw and re-announce
	// every address. This store happens before the update is queued, so the
	// attacher's publish for it sees the stand-down and reports its end.
	// A guard enabled only after startup isn't loaded, and waits for a
	// restart without standing down: see apply.
	if spec != nil && spec.Closed && g.loadWanted && !g.applied.Load() {
		g.down.Store(true)
	}
	u := configUpdate{spec: spec, agent: agent}
	// Latest wins: replace an update the attacher hasn't picked up yet.
	select {
	case <-g.cfgCh:
	default:
	}
	g.cfgCh <- u
	return nil
}

// specFor parses the guard configuration out of the agent governing this
// node. No agent, or an agent without addressGuard, means disabled.
func specFor(cfg *purelbv2.Config) (*Spec, *purelbv2.LBNodeAgent) {
	if cfg == nil {
		return nil, nil
	}
	agent := purelbv2.FirstLocalAgent(cfg.Agents)
	if agent == nil || agent.Spec.Local.AddressGuard == nil {
		return nil, agent
	}
	ag := agent.Spec.Local.AddressGuard
	s := &Spec{
		Monitor: ag.Mode == "monitor",
		XDP:     ag.Hook == "xdp",
		Closed:  ag.FailurePolicy != "open", // closed is the default
		Extra:   ag.ExtraInterfaces,
		Exclude: ag.ExcludeInterfaces,
		Dummy:   agent.Spec.Local.DummyInterface,
	}
	if s.Dummy == "" {
		s.Dummy = "kube-lb0"
	}
	for _, p := range ag.AllowedProtocols {
		if p >= 0 && p <= 255 {
			s.AllowedProtocols = append(s.AllowedProtocols, uint8(p))
		}
	}
	return s, agent
}

// desiredFor is what svc contributes to the guard.
func desiredFor(svc *v1.Service) svcState {
	var s svcState
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		a, err := netip.ParseAddr(ing.IP)
		if err != nil {
			continue
		}
		a = a.Unmap()
		if !slices.Contains(s.vips, a) {
			s.vips = append(s.vips, a)
		}
	}
	for _, p := range svc.Spec.Ports {
		var proto uint8
		switch p.Protocol {
		case v1.ProtocolTCP, "":
			proto = 6
		case v1.ProtocolUDP:
			proto = 17
		case v1.ProtocolSCTP:
			proto = 132
		default:
			continue
		}
		// The Service port, never the NodePort: VIP:nodePort is exactly the
		// kind of host exposure the guard removes.
		ps := portSpec{proto: proto, port: uint16(p.Port)}
		if !slices.Contains(s.ports, ps) {
			s.ports = append(s.ports, ps)
		}
	}
	slices.SortFunc(s.vips, func(a, b netip.Addr) int { return a.Compare(b) })
	slices.SortFunc(s.ports, func(a, b portSpec) int {
		if a.proto != b.proto {
			return int(a.proto) - int(b.proto)
		}
		return int(a.port) - int(b.port)
	})
	return s
}

// SetBalancer implements lbnodeagent.Announcer. The controller calls it
// before the local announcer, so a VIP's rules exist before the address.
func (g *Guard) SetBalancer(svc *v1.Service, _ []*discoveryv1.EndpointSlice) error {
	return g.update(svc.Namespace+"/"+svc.Name, desiredFor(svc))
}

// DeleteBalancer implements lbnodeagent.Announcer. The controller calls it
// after the local announcer, so rules go only once the address is gone.
func (g *Guard) DeleteBalancer(nsName, _ string, _ net.IP) error {
	return g.update(nsName, svcState{})
}

// update replaces nsName's contribution with want and brings the maps for
// every affected VIP in line.
func (g *Guard) update(nsName string, want svcState) error {
	old, had := g.perService[nsName]
	touched := slices.Clone(old.vips)
	for _, v := range want.vips {
		if !slices.Contains(touched, v) {
			touched = append(touched, v)
		}
	}
	// Every failed VIP is retried, not just this Service's: a VIP whose
	// removal failed belongs to no Service any more, so nothing else would
	// ever touch it again.
	for v := range g.failed {
		if !slices.Contains(touched, v) {
			touched = append(touched, v)
		}
	}
	// Called on every EndpointSlice change: an unchanged Service costs no
	// syscalls, unless a VIP is waiting for a retry.
	if had && old.equal(want) && len(g.failed) == 0 {
		return nil
	}

	g.contribute(old, -1)
	g.contribute(want, +1)
	if len(want.vips) == 0 {
		delete(g.perService, nsName)
	} else {
		g.perService[nsName] = want
	}

	var errs error
	for _, v := range touched {
		if err := g.reconcileVIP(v); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	g.vipCount.Store(int64(len(g.vipRefs)))
	g.failedCount.Store(int64(len(g.failed)))
	var withheld, incomplete int64
	for v := range g.failed {
		if g.withheld(v) {
			withheld++
		} else if g.closed.Load() && g.installedVIP[v] {
			incomplete++
		}
	}
	g.withheldCount.Store(withheld)
	g.incompleteCount.Store(incomplete)
	return errs
}

// Withheld reports whether ip must not be announced: under failurePolicy
// closed, a VIP whose key couldn't be written to the map can't be
// filtered, so it isn't announced until a retry writes it. The local
// announcer asks, on the Service-sync goroutine that owns this state,
// right after the guard's SetBalancer for the same Service.
func (g *Guard) Withheld(ip net.IP) bool {
	v, ok := netip.AddrFromSlice(ip)
	return ok && g.withheld(v.Unmap())
}

func (g *Guard) withheld(v netip.Addr) bool {
	_, noMaps := g.dp.(nopDataplane)
	return !noMaps && g.closed.Load() && g.configured.Load() &&
		g.vipRefs[v] > 0 && !g.installedVIP[v]
}

func (g *Guard) contribute(s svcState, delta int) {
	for _, v := range s.vips {
		g.vipRefs[v] += delta
		if g.vipRefs[v] <= 0 {
			delete(g.vipRefs, v)
		}
		for _, p := range s.ports {
			if g.ports[v] == nil {
				g.ports[v] = map[portSpec]int{}
			}
			g.ports[v][p] += delta
			if g.ports[v][p] <= 0 {
				delete(g.ports[v], p)
			}
		}
		if len(g.ports[v]) == 0 {
			delete(g.ports, v)
		}
	}
}

// reconcileVIP makes the kernel maps match the wanted state for v.
//
// Order matters, because the program treats "VIP key present" as "filter
// this address": ports are written before the VIP key is published, and the
// VIP key is removed before its ports. If a write fails, the VIP key is
// removed -- the VIP fails open rather than dropping real Service ports --
// and v is retried on the next sync.
func (g *Guard) reconcileVIP(v netip.Addr) error {
	want := g.ports[v]
	wantVIP := g.vipRefs[v] > 0
	have := g.installedPorts[v]
	if have == nil {
		have = map[portSpec]bool{}
		g.installedPorts[v] = have
	}

	// A failed write leaves v's rules incomplete. Fail-closed keeps the
	// address filtered (a Service port not yet written is blocked until the
	// retry succeeds); fail-open removes the VIP key so nothing about v is
	// filtered rather than dropping real Service ports.
	failOpen := func(op string, err error) error {
		if g.closed.Load() {
			if !g.installedVIP[v] && wantVIP {
				if perr := g.dp.putVIP(v); perr == nil {
					g.installedVIP[v] = true
				}
			}
		} else if g.installedVIP[v] {
			if derr := g.dp.delVIP(v); derr == nil {
				delete(g.installedVIP, v)
			}
		}
		if g.failed[v] {
			logging.Debug(g.logger, "op", "addressGuard", "event", "guardMapWriteFailed", "ip", v, "write", op, "error", err)
		} else {
			logging.Info(g.logger, "op", "addressGuard", "event", "guardMapWriteFailed", "ip", v, "write", op, "error", err,
				"withheld", g.withheld(v))
		}
		g.failed[v] = true
		return fmt.Errorf("address guard %s for %s: %w", op, v, err)
	}

	if wantVIP {
		for p := range want {
			if have[p] {
				continue
			}
			if err := g.dp.putPort(v, p); err != nil {
				return failOpen("allow port", err)
			}
			have[p] = true
			logging.Debug(g.logger, "op", "addressGuard", "event", "portAllowed", "ip", v, "proto", p.proto, "port", p.port)
		}
		if !g.installedVIP[v] {
			if err := g.dp.putVIP(v); err != nil {
				return failOpen("guard address", err)
			}
			g.installedVIP[v] = true
			logging.Debug(g.logger, "op", "addressGuard", "event", "vipGuarded", "ip", v)
		}
	} else if g.installedVIP[v] {
		if err := g.dp.delVIP(v); err != nil {
			return failOpen("unguard address", err)
		}
		delete(g.installedVIP, v)
		logging.Debug(g.logger, "op", "addressGuard", "event", "vipUnguarded", "ip", v)
	}

	for p := range have {
		if want[p] > 0 && wantVIP {
			continue
		}
		if err := g.dp.delPort(v, p); err != nil {
			return failOpen("remove port", err)
		}
		delete(have, p)
		logging.Debug(g.logger, "op", "addressGuard", "event", "portRemoved", "ip", v, "proto", p.proto, "port", p.port)
	}
	if len(have) == 0 && !wantVIP {
		delete(g.installedPorts, v)
	}
	delete(g.failed, v)
	return nil
}

// Shutdown implements lbnodeagent.Announcer. It stops the attacher but
// deliberately does not detach: the links close when the process exits,
// which is after the local announcer has withdrawn every address, so a
// graceful shutdown never leaves a VIP on an interface unguarded.
func (g *Guard) Shutdown() {
	close(g.stop)
	<-g.done
}
