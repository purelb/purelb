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
	"maps"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/egresshook"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// dpSteer is where a pod node sends one slot's traffic (D1, design 6.8):
// the holders of its address in this node's steering view, this node
// included when it is one, and the address's pool MTU (D13).
type dpSteer struct {
	addr    netip.Addr
	holders []string
	// routed: an unreachable holder is skipped while another remains
	// (resolution 16); a local address keeps its elected owner.
	routed bool
	mtu    int
}

// ServicePlacement records how the LB path places a Service address, for
// steering to a shared egress address's holders (Inherit). It runs on the
// work-queue goroutine.
func (a *Agent) ServicePlacement(key string, ip net.IP, p *egresshook.ServicePlacement) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return
	}
	addr = addr.Unmap()
	cur, had := a.placements[addr][key]
	if p == nil {
		if !had {
			return
		}
		delete(a.placements[addr], key)
		if len(a.placements[addr]) == 0 {
			delete(a.placements, addr)
		}
	} else {
		v := egresshook.ServicePlacement{Remote: p.Remote, ETPLocal: p.ETPLocal,
			Preferred: slices.Clone(p.Preferred), EndpointNodes: slices.Clone(p.EndpointNodes)}
		if had && samePlacement(cur, v) {
			return
		}
		if a.placements[addr] == nil {
			a.placements[addr] = map[string]egresshook.ServicePlacement{}
		}
		a.placements[addr][key] = v
	}
	a.add(keyDatapath, false)
}

func samePlacement(x, y egresshook.ServicePlacement) bool {
	return x.Remote == y.Remote && x.ETPLocal == y.ETPLocal &&
		slices.Equal(x.Preferred, y.Preferred) && slices.Equal(x.EndpointNodes, y.EndpointNodes)
}

// steering is where each of stage's slots goes, in this node's steering
// view. Queue goroutine only.
func (a *Agent) steering(s *session, stage *datapath.PodStage) map[uint8]dpSteer {
	return a.steeringWithout(s, stage, "")
}

// steeringWithout is steering with node without left out of every
// address's holders: where the slots go once it has gone (D7's repoint).
func (a *Agent) steeringWithout(s *session, stage *datapath.PodStage, without string) map[uint8]dpSteer {
	out := map[uint8]dpSteer{}
	if stage == nil {
		return out
	}
	d := a.config.Load()
	eas, _ := s.addresses.List(labels.Everything())
	byAddr := map[netip.Addr]*purelbv2.EgressAddress{}
	for _, ea := range eas {
		if addr, err := netip.ParseAddr(ea.Spec.Address); err == nil && ea.Spec.State == "Active" {
			byAddr[addr] = ea
		}
	}
	live := a.elect.SteeringLive()
	for id, sl := range stage.Slots {
		st := dpSteer{addr: sl.Addr}
		if ea := byAddr[sl.Addr]; ea != nil && d != nil {
			st.holders = a.holdersOf(s, ea, d.groups[ea.Spec.ServiceGroup], sl.Addr, live, without)
			st.routed = ea.Spec.PoolType == "Routed"
			st.mtu = int(ea.Spec.PoolMTU)
		}
		out[id] = st
	}
	return out
}

// holdersOf are the holders of egress address ea of group g in this
// node's steering view (§5.5, D1): a dedicated local address's election
// winner; a dedicated routed address's live advertisers (D7); a shared
// address's nodes as its Services' LB path places it (Inherit). Only
// egress-capable nodes can hold one, and never node without.
func (a *Agent) holdersOf(s *session, ea *purelbv2.EgressAddress, g *purelbv2.ServiceGroup, addr netip.Addr,
	live []string, without string) []string {
	capable := func(name string) bool {
		obj, ok, _ := s.nodes.GetByKey(name)
		return ok && name != without && obj.(*corev1.Node).Labels[purelbv2.EgressCapableLabel] == "true"
	}
	// The Service's own election, which egress-capability doesn't enter:
	// unfiltered, or without that node.
	var notWithout func(string) bool
	if without != "" {
		notWithout = func(n string) bool { return n != without }
	}
	set := map[string]bool{}
	switch {
	case g == nil:
	case ea.Spec.SharedWith != nil:
		for _, p := range a.placements[addr] {
			if !p.Remote {
				if w := a.elect.SteeringWinner(addr.String(), notWithout, p.Preferred); w != "" && capable(w) {
					set[w] = true
				}
				continue
			}
			for _, n := range live {
				if capable(n) && (!p.ETPLocal || slices.Contains(p.EndpointNodes, n)) {
					set[n] = true
				}
			}
		}
	case ea.Spec.PoolType == "Routed":
		eligible := eligibility(s.nodes, g)
		for _, n := range live {
			if n != without && eligible(n) {
				set[n] = true
			}
		}
	default:
		if w := a.steeringOwner(s, ea.Spec.Address, g, ea.Spec.PreferredNodes, without); w != "" {
			set[w] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// steeredStage is stage with each FailOpen policy's slot left out where
// steering found no holder for it, so its new flows take the CNI's path
// (design 11.2); a FailClosed one keeps it, and the slot's rule sends it
// to prohibit.
func steeredStage(stage *datapath.PodStage, steer map[uint8]dpSteer) *datapath.PodStage {
	if stage == nil {
		return nil
	}
	out := *stage
	out.Policies = map[string]datapath.Policy{}
	for name, p := range stage.Policies {
		for _, f := range []**datapath.PolicyFamily{&p.V4, &p.V6} {
			if *f == nil || !(*f).FailOpen || (*f).Slot == 0 || len(steer[(*f).Slot].holders) > 0 {
				continue
			}
			c := **f
			c.Slot = 0
			*f = &c
		}
		out.Policies[name] = p
	}
	return &out
}

// syncSteering routes the pod-node stage's slots (design 6.3, 6.8): a slot
// whose address this node holds goes direct, in its pool interface's
// table (a routed one in main); one with holders elsewhere into the
// tunnel, steered to them; one with none to prohibit. The target tables,
// steer map and rules change in that order, then the steering no rule
// uses any more is pruned, so a slot is never without somewhere to go. A
// reply the tunnel brings (0x80) or a pool interface does (0x81 + n) is
// routed and source-checked by its own rule, under strict rp_filter with
// src_valid_mark. Datapath goroutine only.
func (a *Agent) syncSteering(st *dpState, w dpWant, addrs map[netip.Addr]*datapath.AddressEntries,
	cluster []netip.Prefix, mode string, tun netlink.Link) error {
	var self dpNode
	nodes := map[string]dpNode{}
	throws := slices.Clone(cluster)
	for _, n := range w.nodes {
		nodes[n.name] = n
		if n.name == w.self {
			self = n
			continue
		}
		// The underlay backstop (D3).
		for _, ip := range []netip.Addr{n.v4, n.v6} {
			if ip.IsValid() {
				throws = append(throws, netip.PrefixFrom(ip, ip.BitLen()))
			}
		}
	}
	lost := a.lostPeers(st)
	mtu4, mtu6 := pathMTU(st, w, mode)
	t := datapath.Targets{Throws: throws, Tunnel: tun.Attrs().Index}
	rules := []datapath.MarkRule{{Pref: datapath.PrefReply, Mark: replyFlag, Table: datapath.ReplyTable}}
	for _, n := range st.pools {
		rules = append(rules, datapath.MarkRule{Pref: datapath.PrefDirect, Mark: replyFlag + 1 + uint8(n),
			Table: datapath.PoolTable(n)})
	}
	// Every slot the stage marks has a rule: one steering doesn't cover
	// goes to prohibit, never by main with its SNAT.
	steer := map[uint8][]uint64{}
	prohibited := map[uint8]bool{}
	repointed := map[uint8][]string{}
	var slots []uint8
	if w.stage != nil {
		slots = slices.Sorted(maps.Keys(w.stage.Slots))
	}
	for _, id := range slots {
		sl := w.steer[id]
		sl.addr = w.stage.Slots[id].Addr
		table := datapath.ProhibitTable
		e := addrs[sl.addr]
		held := e != nil && e.Held && e.Table != 0
		ids := steerTo(sl, w.self, self, nodes, lost)
		// Leaving, a held address's slot goes through the tunnel to the
		// holders that remain (D7); with none, it stays direct.
		away := held && w.leaving && len(ids) > 0
		if away {
			repointed[id] = sl.holders
		}
		if held && !away {
			table = int(e.Table)
		} else if len(ids) > 0 {
			table, steer[id] = datapath.TunnelTable, ids
			// The tunnel route's MTU is the smallest of its slots'.
			m := slotMTU(sl.addr, mtu4, mtu6, sl.mtu)
			cur := &t.MTU4
			if sl.addr.Is6() {
				cur = &t.MTU6
				if m > 0 {
					m = max(m, minMTU6)
				}
			}
			if m > 0 && (*cur == 0 || m < *cur) {
				*cur = m
			}
		}
		rules = append(rules, datapath.MarkRule{Pref: datapath.PrefSlot, Mark: id, Table: table})
		prohibited[id] = table == datapath.ProhibitTable
	}
	if t.MTU4 == 0 {
		t.MTU4 = mtu4
	}
	if t.MTU6 == 0 && mtu6 > 0 {
		t.MTU6 = max(mtu6, minMTU6)
	}

	var errs []error
	errs = append(errs, datapath.SyncTargets(st.nl, t))
	errs = append(errs, st.objs.SyncSteer(steer, false))
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := datapath.SyncRules(st.nl, st.shift, rules); err != nil {
		return err
	}
	st.prohibited, st.repointed = prohibited, repointed
	if err := st.objs.SyncSteer(steer, true); err != nil {
		return err
	}
	ifaces := map[int]string{tun.Attrs().Index: tun.Attrs().Name}
	for ifindex := range st.pools {
		if p, ok := w.pools[ifindex]; ok && p.v4 {
			ifaces[ifindex] = p.name
		}
	}
	for ifindex, name := range ifaces {
		set, err := datapath.EnsureSrcValidMark(ifindex, name)
		if err != nil {
			errs = append(errs, err)
		} else if set {
			logging.Info(a.logger, "op", "egressSteer", "node", a.node, "interface", name,
				"msg", "set src_valid_mark, so strict rp_filter accepts egress replies (design 6.3)")
		}
	}
	logging.Debug(a.logger, "op", "egressSteer", "node", a.node, "slots", len(slots), "tunnelled", len(steer),
		"mtu4", t.MTU4, "mtu6", t.MTU6, "msg", "slots routed")
	return errors.Join(errs...)
}

var repointedSlots = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "repointed_slots_total",
	Help: "Slots of addresses this node held that a graceful shutdown sent through the tunnel to the holders that remain (D7).",
})

func init() {
	prometheus.MustRegister(repointedSlots)
}

// repointWait bounds the wait for the datapath goroutine to finish its
// last item at shutdown.
const repointWait = 5 * time.Second

// repoint sends this node's pods' slots of the addresses it holds through
// the tunnel to the holders that remain once it has gone (D7), before
// they are withdrawn: their flows keep their source and port, out and
// back through one holder. Shutdown goroutine only, once the queue
// goroutine has stopped; the datapath goroutine has stopped too, and what
// it owned is this one's once it has exited.
func (a *Agent) repoint() {
	if a.podnode == nil || a.stageWant == nil {
		return
	}
	steer := a.steeringWithout(a.on, a.stageWant, a.node)
	select {
	case <-a.runner.exited:
	case <-time.After(repointWait):
		logging.Info(a.logger, "op", "egressRepoint", "node", a.node,
			"msg", "the datapath is still busy; this node's slots aren't repointed before withdrawing")
		return
	}
	if err := a.repointSlots(steer); err != nil {
		logging.Info(a.logger, "op", "egressRepoint", "node", a.node, "error", err,
			"msg", "repointing this node's slots before withdrawing")
	}
}

// repointSlots applies steering steer to the desired state last applied,
// as a leaving node: slots of the addresses it holds go through the
// tunnel. Datapath-owner goroutine only.
func (a *Agent) repointSlots(steer map[uint8]dpSteer) error {
	st := a.dp
	if st.applied == nil || st.tun == nil {
		return nil
	}
	w := *st.applied
	w.steer, w.leaving = steer, true
	if err := a.syncSteering(st, w, st.addrs, st.cluster, st.mode, st.tun); err != nil {
		return err
	}
	if len(st.repointed) > 0 {
		repointedSlots.Add(float64(len(st.repointed)))
		for _, id := range slices.Sorted(maps.Keys(st.repointed)) {
			logging.Info(a.logger, "op", "egressRepoint", "node", a.node, "slot", id,
				"address", w.stage.Slots[id].Addr, "holders", st.repointed[id],
				"msg", "shutting down: the slot goes through the tunnel to the holders that remain")
		}
	}
	return nil
}

// resetSrcValidMark turns src_valid_mark off on ifindex once it is no
// longer a pool interface (user, 2026-10-31): whoever turned it on, since
// its value before egress isn't known. Nothing when the interface is gone.
func (a *Agent) resetSrcValidMark(h *netlink.Handle, ifindex int) error {
	l, err := h.LinkByIndex(ifindex)
	if err != nil {
		if errors.As(err, new(netlink.LinkNotFoundError)) {
			return nil
		}
		return err
	}
	reset, err := datapath.ResetSrcValidMark(ifindex, l.Attrs().Name)
	if reset {
		logging.Info(a.logger, "op", "egressSteer", "node", a.node, "interface", l.Attrs().Name,
			"msg", "reset src_valid_mark: no longer an egress pool interface")
	}
	return err
}

// replyFlag is the mark field's reply flag (D18): 0x80 for a reply the
// tunnel brought, 0x81 + n for one pool interface n did.
const replyFlag = 0x80

// steerTo are the node ids sl's traffic is steered among: its holders but
// this node, those with a known underlay; for a routed address, without
// the unreachable ones while another remains (resolution 16).
func steerTo(sl dpSteer, selfName string, self dpNode, nodes map[string]dpNode, lost map[netip.Addr]bool) []uint64 {
	var all, reachable []uint64
	for _, h := range sl.holders {
		n, ok := nodes[h]
		if h == selfName || !ok {
			continue
		}
		id := datapath.NodeID(h)
		all = append(all, id)
		if !lost[outerAddr(self, n, sl.addr.Is6())] {
			reachable = append(reachable, id)
		}
	}
	if sl.routed && len(reachable) > 0 {
		return reachable
	}
	return all
}

// outerAddr is the peer address pl_steer tunnels inner traffic of the
// family to: IPv4 where both ends have IPv4 and the traffic is IPv4, else
// IPv6 where both have IPv6, else IPv4.
func outerAddr(self, peer dpNode, inner6 bool) netip.Addr {
	if (inner6 || !self.v4.IsValid() || !peer.v4.IsValid()) && self.v6.IsValid() && peer.v6.IsValid() {
		return peer.v6
	}
	return peer.v4
}

// holderMTU is the MTU of the interface a held address's traffic leaves
// this node by (D13): its pool interface's, or for a routed address the
// main table's default route's; 0 when not held here or not known.
// Datapath goroutine only.
func holderMTU(st *dpState, address string) int {
	addr, err := netip.ParseAddr(address)
	if err != nil || st.nl == nil {
		return 0
	}
	e := st.addrs[addr]
	if e == nil || !e.Held {
		return 0
	}
	if e.Table == mainTable {
		return datapath.DefaultRouteMTU(st.nl, addr.Is6())
	}
	for ifindex, n := range st.pools {
		if uint32(datapath.PoolTable(n)) == e.Table {
			if l, err := st.nl.LinkByIndex(ifindex); err == nil {
				return l.Attrs().MTU
			}
		}
	}
	return 0
}
