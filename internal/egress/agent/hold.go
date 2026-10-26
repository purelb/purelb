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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/cilium/ebpf"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/addrguard"
	"purelb.io/internal/egress"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/informers/externalversions"
)

// Guard is what egress needs of the address guard (internal/addrguard).
// Its state belongs to the work-queue goroutine, which runs egress's keys
// and Services alike.
type Guard interface {
	UpdateEgress(a netip.Addr, present bool, ports []addrguard.EgressPort, slices []addrguard.EgressSlice) error
	// StandingDown reports whether the fail-closed guard isn't working:
	// the node then withdraws its routed advertisements (design 6.10).
	StandingDown() bool
	// Interfaces are the interfaces the guard is attached to, and
	// ProgramIDs its programs: pl_announce goes right behind it there.
	// SetLinksHook's function runs when Interfaces changes.
	Interfaces() []int
	ProgramIDs() []ebpf.ProgramID
	SetLinksHook(func())
}

// SetGuard gives egress the node's address guard. Call it before Start.
func (a *Agent) SetGuard(g Guard) {
	a.guard = g
	g.SetLinksHook(func() { a.add(keyDatapath, false) })
}

// eeWatch is one scoped EgressEndpoints watch (D6): a name watch for an
// address this node announces, or the label-selector watch for the routed
// groups it advertises.
type eeWatch struct {
	stop  chan struct{}
	store cache.Store
	sync  cache.InformerSynced
}

// holdings is what syncHoldings derives for the datapath: an entry for
// every egress address (held set only where this node holds it and has
// read its EgressEndpoints in full), every address for the hardening
// sets, the Service ports of the held shared ones, and the acks the
// datapath publishes once it has installed them.
type holdings struct {
	addrs    map[netip.Addr]*datapath.AddressEntries
	egress   []netip.Addr
	services []datapath.ServicePort
	acks     []purelbv2.EgressAnnouncing
	// ifaces are the held local addresses' pool interfaces (ifindex),
	// whose tables they forward transit in; pools those interfaces.
	ifaces map[netip.Addr]int
	pools  map[int]dpPool
	// place is how to place each held dedicated address (D31); shared
	// the held addresses the Service path places.
	place  map[netip.Addr]placeWant
	shared map[netip.Addr]bool
	// unheld are the dedicated (egress-only) addresses this node doesn't
	// hold, with their placement keys. One it holds but hasn't read in
	// full yet is in neither place nor unheld.
	unheld map[netip.Addr]string
	// faults are this node's problems with addresses (addressFaults).
	faults map[netip.Addr]purelbv2.EgressAddressFault
}

// fault records reason for addr, keeping the first.
func (h holdings) fault(addr netip.Addr, reason, format string, args ...any) {
	if _, ok := h.faults[addr]; ok {
		return
	}
	h.faults[addr] = purelbv2.EgressAddressFault{Address: addr.String(), Reason: reason,
		Message: truncate(fmt.Sprintf(format, args...), 1024)}
}

// truncate cuts s to at most n bytes.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// syncHoldings decides which egress addresses this node holds (§5.5),
// keeps a scoped EgressEndpoints watch for each (D6), and builds what the
// datapath installs. A held address counts as held only once every one of
// its watches has done its initial full read, so a node taking an address
// over never serves it from a stale slice table. Queue goroutine only.
func (a *Agent) syncHoldings(s *session) holdings {
	h := holdings{addrs: map[netip.Addr]*datapath.AddressEntries{}, ifaces: map[netip.Addr]int{},
		pools: map[int]dpPool{}, place: map[netip.Addr]placeWant{}, shared: map[netip.Addr]bool{},
		unheld: map[netip.Addr]string{}, faults: map[netip.Addr]purelbv2.EgressAddressFault{}}
	d := a.config.Load()
	if d == nil {
		return h
	}
	eas, _ := s.addresses.List(labels.Everything())
	sort.Slice(eas, func(i, j int) bool { return eas[i].Name < eas[j].Name })

	type heldAddr struct {
		ea    *purelbv2.EgressAddress
		g     *purelbv2.ServiceGroup
		addr  netip.Addr
		names []string // name-watched EgressEndpoints; none for routed
	}
	var held []heldAddr
	wantWatch := map[string]bool{}
	var routedGroups []string
	for _, ea := range eas {
		addr, err := netip.ParseAddr(ea.Spec.Address)
		if err != nil || ea.Spec.State != "Active" {
			continue
		}
		h.egress = append(h.egress, addr)
		h.addrs[addr] = &datapath.AddressEntries{}
		g := d.groups[ea.Spec.ServiceGroup]
		if !a.holds(s, ea, g, addr) {
			if ea.Spec.SharedWith == nil {
				h.unheld[addr] = placementKey(ea)
			}
			if ea.Spec.PoolType == "Routed" && ea.Spec.SharedWith == nil && g != nil &&
				a.guard != nil && a.guard.StandingDown() && eligibility(s.nodes, g)(a.node) {
				h.fault(addr, "GuardStandingDown", "the fail-closed address guard isn't working: not advertising it")
			}
			continue
		}
		x := heldAddr{ea: ea, g: g, addr: addr}
		if ea.Spec.PoolType == "Routed" && ea.Spec.SharedWith == nil {
			if !slices.Contains(routedGroups, ea.Spec.ServiceGroup) {
				routedGroups = append(routedGroups, ea.Spec.ServiceGroup)
			}
		} else {
			for _, o := range ea.OwnerReferences {
				if o.Kind == "EgressEndpoints" {
					x.names = append(x.names, o.Name)
					wantWatch[nameWatch(o.Name)] = true
				}
			}
		}
		held = append(held, x)
	}
	sort.Strings(routedGroups)
	if len(routedGroups) > 0 {
		wantWatch[routedWatch(routedGroups)] = true
	}
	a.reconcileWatches(s, wantWatch, routedGroups)

	guarded := map[netip.Addr]bool{}
	for _, x := range held {
		eps, ready := a.endpointsOf(s, x.addr, x.names, routedGroups)
		if !ready || len(eps) == 0 {
			continue
		}
		e, ports, gslices, err := entriesFor(x.ea, eps)
		if err != nil {
			logging.Info(a.logger, "op", "egressHold", "node", a.node, "address", x.addr, "error", err)
			continue
		}
		ifindex, pw, err := a.placing(s, x.ea, x.g, x.addr)
		if err != nil {
			// Not held: without its pool interface it has no table and
			// can't be placed. Retried at the next sync.
			logging.Info(a.logger, "op", "egressHold", "node", a.node, "address", x.addr, "error", err,
				"msg", "no interface to place the address on; not holding it")
			h.fault(x.addr, "AddressAddFailed", "no interface to place it on: %v", err)
			continue
		}
		if a.guard != nil {
			if err := a.guard.UpdateEgress(x.addr, true, ports, gslices); err != nil {
				// Not acked: a slice the guard can't pass stays Pending
				// (GuardMapFull, §6.10); retried at the next sync.
				logging.Info(a.logger, "op", "egressHold", "node", a.node, "address", x.addr, "error", err,
					"msg", "the address guard refused the address's entries; not acknowledging it")
				if errors.Is(err, addrguard.ErrMapFull) {
					h.fault(x.addr, "GuardMapFull", "%v", err)
				}
				continue
			}
			guarded[x.addr] = true
		}
		h.addrs[x.addr] = e
		if ifindex != 0 {
			h.ifaces[x.addr] = ifindex
			pool := h.pools[ifindex]
			pool.name = pw.iface
			pool.v4 = pool.v4 || x.addr.Is4()
			pool.v6 = pool.v6 || x.addr.Is6()
			h.pools[ifindex] = pool
		}
		if x.ea.Spec.SharedWith != nil {
			h.shared[x.addr] = true
		} else {
			h.place[x.addr] = pw
		}
		for p := range e.Services {
			h.services = append(h.services, datapath.ServicePort{Addr: x.addr, Proto: p.Proto, Port: p.Port})
		}
		for _, ep := range eps {
			h.acks = append(h.acks, purelbv2.EgressAnnouncing{Endpoints: ep.Name, Address: x.ea.Spec.Address,
				Generation: ep.Generation})
		}
	}
	// Every egress address is guarded on every egress node (§6.10); only a
	// holder's carries its slices and ports.
	if a.guard != nil {
		for addr := range h.addrs {
			if !guarded[addr] {
				if err := a.guard.UpdateEgress(addr, true, nil, nil); err == nil {
					guarded[addr] = true
				}
			}
		}
		for addr := range a.guarded {
			if _, ok := h.addrs[addr]; !ok {
				if err := a.guard.UpdateEgress(addr, false, nil, nil); err != nil {
					guarded[addr] = true // retried at the next sync
				}
			}
		}
		a.guarded = guarded
	}
	sort.Slice(h.acks, func(i, j int) bool { return h.acks[i].Endpoints < h.acks[j].Endpoints })
	return h
}

// holds reports whether this node holds egress address ea of group g
// (§5.5): a shared address where this node announces its Service (the
// Service's own placement, Inherit), a local one where it wins the
// election, a routed one where it is one of the group's advertisers (D7).
func (a *Agent) holds(s *session, ea *purelbv2.EgressAddress, g *purelbv2.ServiceGroup, addr netip.Addr) bool {
	switch {
	case g == nil:
		return false
	case ea.Spec.SharedWith != nil:
		return len(a.services[addr]) > 0
	case ea.Spec.PoolType == "Routed":
		// Every live selector node advertises (D7); one whose guard
		// stands down or that has been cut off from the API for 3 lease
		// durations withdraws (design 6.10, 8.8).
		return eligibility(s.nodes, g)(a.node) && (a.guard == nil || !a.guard.StandingDown()) && !a.partitioned()
	default:
		return a.localOwner(s, ea.Spec.Address, g, ea.Spec.PreferredNodes) == a.node
	}
}

func nameWatch(ee string) string { return "name:" + ee }

func routedWatch(groups []string) string { return "routed:" + strings.Join(groups, ",") }

// reconcileWatches starts the watches in want and stops the others. A
// change of routed groups replaces the one routed watch.
func (a *Agent) reconcileWatches(s *session, want map[string]bool, routedGroups []string) {
	for k, w := range s.watches {
		if !want[k] {
			close(w.stop)
			delete(s.watches, k)
			logging.Debug(a.logger, "op", "egressHold", "node", a.node, "watch", k, "msg", "stopped")
		}
	}
	for k := range want {
		if _, ok := s.watches[k]; ok {
			continue
		}
		tweak := func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", strings.TrimPrefix(k, "name:")).String()
		}
		if strings.HasPrefix(k, "routed:") {
			r, err := labels.NewRequirement(egress.LabelServiceGroup, selection.In, routedGroups)
			if err != nil {
				logging.Info(a.logger, "op", "egressHold", "node", a.node, "error", err)
				continue
			}
			sel := labels.NewSelector().Add(*r).String()
			tweak = func(o *metav1.ListOptions) { o.LabelSelector = sel }
		}
		f := externalversions.NewSharedInformerFactoryWithOptions(a.purelb, resync,
			externalversions.WithNamespace(a.namespace), externalversions.WithTweakListOptions(tweak))
		inf := f.Purelb().V2().EgressEndpointses().Informer()
		kick := func(interface{}) { a.add(keyDatapath, false) }
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: kick, DeleteFunc: kick,
			UpdateFunc: func(_, o interface{}) { kick(o) }})
		w := &eeWatch{stop: make(chan struct{}), store: inf.GetStore(), sync: inf.HasSynced}
		f.Start(w.stop)
		go func() {
			// The full read done: the addresses it covers can be served.
			if cache.WaitForCacheSync(w.stop, w.sync) {
				a.add(keyDatapath, false)
			}
		}()
		s.watches[k] = w
		logging.Debug(a.logger, "op", "egressHold", "node", a.node, "watch", k, "msg", "started")
	}
}

// endpointsOf is addr's EgressEndpoints from its watches, and whether each
// watch covering it has done its full read.
func (a *Agent) endpointsOf(s *session, addr netip.Addr, names, routedGroups []string) ([]*purelbv2.EgressEndpoints, bool) {
	var out []*purelbv2.EgressEndpoints
	if len(names) == 0 {
		w := s.watches[routedWatch(routedGroups)]
		if w == nil || !w.sync() {
			return nil, false
		}
		for _, o := range w.store.List() {
			ep := o.(*purelbv2.EgressEndpoints)
			if ep.Spec.PoolType == "Routed" && ep.Spec.Address == addr.String() {
				out = append(out, ep)
			}
		}
	} else {
		for _, n := range names {
			w := s.watches[nameWatch(n)]
			if w == nil || !w.sync() {
				return nil, false
			}
			if o, ok, _ := w.store.GetByKey(a.namespace + "/" + n); ok {
				out = append(out, o.(*purelbv2.EgressEndpoints))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, true
}

// entriesFor is a held address's datapath entries and guard entries from
// its EgressEndpoints. An address used by more than one policy merges
// their slices; they must agree on the slice size and on each slice's
// owner.
func entriesFor(ea *purelbv2.EgressAddress, eps []*purelbv2.EgressEndpoints) (*datapath.AddressEntries,
	[]addrguard.EgressPort, []addrguard.EgressSlice, error) {
	e := &datapath.AddressEntries{Address: datapath.Address{Held: true, Base: egress.FirstSlicePort},
		Slices: map[uint16]uint64{}, Reserved: map[datapath.PortKey]uint64{}, Services: map[datapath.PortKey]bool{}}
	if ea.Spec.PoolType == "Routed" {
		e.Table = mainTable
	}
	var ports []addrguard.EgressPort
	var gslices []addrguard.EgressSlice
	for _, ep := range eps {
		if ep.Spec.State == "Quarantined" {
			continue
		}
		size := uint32(ep.Spec.SliceSize)
		if e.SliceSize != 0 && e.SliceSize != size {
			return nil, nil, nil, fmt.Errorf("EgressEndpoints %s slices by %d, another by %d", ep.Name, size, e.SliceSize)
		}
		e.SliceSize = size
		if ep.Spec.Identity != "" && len(ea.Spec.PreferredNodes) > 0 {
			// PerPod: the pod's node owns every port and protocol (D4).
			e.WholeOwner = datapath.NodeID(ea.Spec.PreferredNodes[0])
			gslices = append(gslices, addrguard.EgressSlice{})
		}
		for _, sl := range ep.Spec.Slices {
			if sl.Node == "" {
				continue // unassigned reserve: owner 0, dropped
			}
			idx, owner := uint16(sl.Index), datapath.NodeID(sl.Node)
			if o, ok := e.Slices[idx]; ok && o != owner {
				return nil, nil, nil, fmt.Errorf("slice %d has two owners", idx)
			}
			e.Slices[idx] = owner
			gslices = append(gslices, addrguard.EgressSlice{Base: uint16(uint32(idx) * size), Size: size})
		}
		for _, r := range ep.Spec.Reserved {
			p := datapath.PortKey{Proto: protoNumber(r.Protocol), Port: uint16(r.Port)}
			if r.Node != "" {
				e.Reserved[p] = datapath.NodeID(r.Node)
				ports = append(ports, addrguard.EgressPort{Proto: p.Proto, Port: p.Port})
			}
		}
		for _, sp := range ep.Spec.ServicePorts {
			p := datapath.PortKey{Proto: protoNumber(sp.Protocol), Port: uint16(sp.Port)}
			e.Services[p] = true
			ports = append(ports, addrguard.EgressPort{Proto: p.Proto, Port: p.Port})
		}
	}
	if e.SliceSize == 0 {
		return nil, nil, nil, fmt.Errorf("no EgressEndpoints gives a slice size")
	}
	return e, ports, gslices, nil
}

// mainTable is the routing table routed addresses forward transit in
// (user decision 2026-10-24: the routing the node's BGP sessions use).
const mainTable = 254

func protoNumber(p string) uint8 {
	switch p {
	case "UDP":
		return 17
	case "SCTP":
		return 132
	}
	return 6
}

// Referenced reports whether egress has placed ip on this node. It runs
// on the work-queue goroutine.
func (a *Agent) Referenced(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	_, placed := a.placed[addr.Unmap()]
	return placed
}

// ServiceAddress records a Service address this node announces (true) or
// no longer announces: a shared egress address is held where its Service
// is (Inherit). It runs on the work-queue goroutine.
func (a *Agent) ServiceAddress(key string, ip net.IP, announced bool) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return
	}
	addr = addr.Unmap()
	if announced == a.services[addr][key] {
		return
	}
	if announced {
		if a.services[addr] == nil {
			a.services[addr] = map[string]bool{}
		}
		a.services[addr][key] = true
	} else {
		delete(a.services[addr], key)
		if len(a.services[addr]) == 0 {
			delete(a.services, addr)
		}
	}
	a.add(keyDatapath, false)
}
