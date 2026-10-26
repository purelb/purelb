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
	"maps"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egresshook"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// partitionLeases is how many lease durations this node's lease may go
// unrenewed before it withdraws its routed advertisements (resolution 17;
// user decision 2026-10-23).
const partitionLeases = 3

// placement is one dedicated egress address this node placed (D31).
type placement struct {
	key     string // "egress/<EgressAddress uid>": the renewal key's name
	routed  bool
	ifindex int // local: the pool interface
}

// placeWant is how syncPlacement places one held dedicated address.
type placeWant struct {
	placement
	iface string // local: the pool interface's name
	p     egresshook.EgressPlacement
}

// placementKey is the name an egress address is placed and renewed under
// (D31): "egress/<uid>", which no Service name can take.
func placementKey(ea *purelbv2.EgressAddress) string {
	return "egress/" + string(ea.UID)
}

// SetPlacer gives egress the announcer that places its addresses. Call it
// before Start.
func (a *Agent) SetPlacer(p egresshook.Placer) {
	a.placer = p
}

// placing works out where held address addr of group g goes: a local
// address's pool interface (also a shared one's: its table), and how
// syncPlacement places a dedicated one. Queue goroutine only.
func (a *Agent) placing(s *session, ea *purelbv2.EgressAddress, g *purelbv2.ServiceGroup, addr netip.Addr) (int, placeWant, error) {
	w := placeWant{placement: placement{key: placementKey(ea)}}
	if ea.Spec.PoolType == "Routed" {
		w.routed = true
		if ea.Spec.SharedWith != nil {
			return 0, w, nil
		}
		if g.Spec.Remote == nil {
			return 0, w, fmt.Errorf("group %s has no routed pools", g.Name)
		}
		pool, err := g.Spec.Remote.PoolForAddress(addr.AsSlice())
		if err != nil {
			return 0, w, err
		}
		w.p = egresshook.EgressPlacement{Routed: true, Subnet: pool.Subnet, Aggregation: pool.Aggregation}
		return 0, w, nil
	}
	if a.placer == nil {
		return 0, w, errors.New("no announcer to place addresses")
	}
	ifindex, name, err := a.placer.EgressInterface(addr.AsSlice())
	if err != nil {
		return 0, w, err
	}
	w.ifindex, w.iface = ifindex, name
	skip := g.Spec.Local != nil && g.Spec.Local.SkipIPv6DAD
	if ea.Spec.SkipIPv6DAD != nil {
		skip = *ea.Spec.SkipIPv6DAD // the policy's, for a dedicated address (D31)
	}
	preferred := ea.Spec.PreferredNodes
	w.p = egresshook.EgressPlacement{SkipDAD: skip && addr.Is6(), Still: func() bool {
		return a.localOwner(s, addr.String(), g, preferred) == a.node
	}}
	return ifindex, w, nil
}

// partitioned reports whether this node's lease has gone unrenewed for
// partitionLeases lease durations, as when the node is cut off from the
// API server (design 8.8). Until then a late renewal arms a sync for the
// moment it would be. Queue goroutine only.
func (a *Agent) partitioned() bool {
	last := a.elect.LastRenewal()
	if last.IsZero() {
		return false
	}
	lease := a.elect.LeaseDuration()
	late := a.now().Sub(last)
	if late >= partitionLeases*lease {
		return true
	}
	if late > lease {
		left := partitionLeases*lease - late
		if a.partitionTimer == nil {
			a.partitionTimer = time.AfterFunc(left, func() { a.add(keyDatapath, false) })
		} else {
			a.partitionTimer.Reset(left)
		}
	}
	return false
}

// syncPlacement brings the placed addresses in line with h: an address no
// longer held goes first, before the datapath clears its held flag; a
// held one is placed once the datapath has installed it held (the flag
// before the address, design 6.7, so a returning gateway advertises only
// after its datapath is programmed); a placed one found missing, as after
// an interface flap, is placed again. Queue goroutine only.
func (a *Agent) syncPlacement(h holdings) {
	if a.placer == nil {
		return // nothing placed without one (placing holds nothing local)
	}
	changed := false
	for addr, pl := range a.placed {
		if w, ok := h.place[addr]; ok && w.placement == pl {
			continue
		}
		if err := a.placer.WithdrawEgress(pl.key, addr.AsSlice()); err != nil {
			// Retried at the next sync; its held flag goes meanwhile, so
			// pl_announce drops what still arrives (not_announcer).
			logging.Info(a.logger, "op", "egressWithdraw", "node", a.node, "address", addr, "error", err)
			continue
		}
		delete(a.placed, addr)
		delete(a.dadFailed, addr)
		a.dadLost.Delete(addr)
		changed = true
	}
	var installed map[netip.Addr]bool
	if p := a.installed.Load(); p != nil {
		installed = *p
	}
	flags, err := a.addrFlags()
	if err != nil {
		logging.Info(a.logger, "op", "egressPlace", "node", a.node, "error", err,
			"msg", "listing the node's addresses; not checking placed ones this sync")
	}
	for addr, w := range h.place {
		f, present := flags[addr]
		if d := a.dadFailed[addr]; d != nil {
			h.fault(addr, "DADFailed", "duplicate address detection failed: another host on the link has it; trying again in %s",
				d.retry.Sub(a.now()).Round(time.Second))
			if a.now().Before(d.retry) {
				continue // backing off (user decision 2026-10-26)
			}
		}
		if _, ok := a.placed[addr]; ok {
			_, lost := a.dadLost.LoadAndDelete(addr)
			switch {
			case err == nil && present && f&unix.IFA_F_DADFAILED != 0, !present && err == nil && lost:
				// DAD failed: the kernel deletes a finite-lifetime address
				// that does, and flags a permanent one. Not fought.
				a.failDAD(addr, w.key)
				delete(a.placed, addr)
				changed = true
				h.fault(addr, "DADFailed", "duplicate address detection failed: another host on the link has it; trying again in %s",
					a.dadFailed[addr].retry.Sub(a.now()).Round(time.Second))
				continue
			case err != nil || present:
				if present && f&unix.IFA_F_TENTATIVE == 0 && a.dadFailed[addr] != nil {
					logging.Info(a.logger, "op", "egressPlace", "node", a.node, "address", addr,
						"msg", "duplicate address detection passed")
					delete(a.dadFailed, addr)
					delete(h.faults, addr)
				}
				continue
			}
			logging.Info(a.logger, "op", "egressPlace", "node", a.node, "address", addr,
				"msg", "a placed address was removed outside the agent; placing it again")
		} else if !installed[addr] {
			continue // its held flag isn't installed yet
		}
		if err := a.placer.PlaceEgress(w.key, addr.AsSlice(), w.p); err != nil {
			logging.Info(a.logger, "op", "egressPlace", "node", a.node, "address", addr, "error", err)
			h.fault(addr, "AddressAddFailed", "%v", err)
			if _, ok := a.placed[addr]; ok {
				delete(a.placed, addr)
				changed = true
			}
			continue
		}
		a.placed[addr] = w.placement
		changed = true
	}
	// A dedicated address on the node that this node doesn't hold and this
	// agent didn't place: an earlier agent's, from before a restart in
	// which the node stopped holding it (D12: recover from the kernel, then
	// prune). WithdrawEgress leaves one a Service announces.
	for addr, key := range h.unheld {
		if _, ok := flags[addr]; !ok || err != nil {
			continue
		}
		if _, ok := a.placed[addr]; ok {
			continue
		}
		logging.Info(a.logger, "op", "egressWithdraw", "node", a.node, "address", addr,
			"msg", "removing an egress address this node no longer holds, placed before the agent restarted")
		if werr := a.placer.WithdrawEgress(key, addr.AsSlice()); werr != nil {
			logging.Info(a.logger, "op", "egressWithdraw", "node", a.node, "address", addr, "error", werr)
		}
	}
	if changed {
		a.publishPlaced()
		a.add(keyNode, false)
	}
}

// publishPlaced publishes placed's addresses for the address watcher.
func (a *Agent) publishPlaced() {
	set := make(map[netip.Addr]bool, len(a.placed))
	for addr := range a.placed {
		set[addr] = true
	}
	a.placedSet.Store(&set)
}

// dadBackoff is how a DAD-failed address is retried: after backoff, from
// dadRetryMin doubling to dadRetryMax (user decision 2026-10-26).
type dadBackoff struct {
	backoff time.Duration
	retry   time.Time
}

const (
	dadRetryMin = 30 * time.Second
	dadRetryMax = 5 * time.Minute
)

// failDAD records that addr failed DAD and schedules the retry: no
// placing again until then, since every attempt runs DAD against a host
// that has the address. A flagged (permanent) one is removed first.
func (a *Agent) failDAD(addr netip.Addr, key string) {
	d := a.dadFailed[addr]
	if d == nil {
		d = &dadBackoff{backoff: dadRetryMin}
		a.dadFailed[addr] = d
	} else if d.backoff *= 2; d.backoff > dadRetryMax {
		d.backoff = dadRetryMax
	}
	d.retry = a.now().Add(d.backoff)
	if err := a.placer.WithdrawEgress(key, addr.AsSlice()); err != nil {
		logging.Info(a.logger, "op", "egressPlace", "node", a.node, "address", addr, "error", err)
	}
	logging.Info(a.logger, "op", "egressPlace", "node", a.node, "address", addr, "retry", d.backoff,
		"msg", "duplicate address detection failed: another host on the link has this address")
	time.AfterFunc(d.backoff, func() { a.add(keyDatapath, false) })
}

// withdrawPlaced withdraws every address this node placed: on disable and
// at the start of a graceful shutdown, before the lease goes (design 5.5).
func (a *Agent) withdrawPlaced(why string) {
	for addr, pl := range a.placed {
		if err := a.placer.WithdrawEgress(pl.key, addr.AsSlice()); err != nil {
			logging.Info(a.logger, "op", "egressWithdraw", "node", a.node, "address", addr, "why", why, "error", err)
			continue
		}
		delete(a.placed, addr)
	}
	clear(a.dadFailed)
	a.publishPlaced()
}

// installedHeld records the addresses an apply installed held and, when
// they changed, syncs again so syncPlacement places the new ones.
// Datapath goroutine.
func (a *Agent) installedHeld(w dpWant) {
	held := map[netip.Addr]bool{}
	for addr, e := range w.hold.addrs {
		if e.Held {
			held[addr] = true
		}
	}
	if prev := a.installed.Swap(&held); prev == nil || !maps.Equal(*prev, held) {
		a.add(keyDatapath, false)
	}
}

// listAddrFlags lists the node's addresses with their IFA flags (the
// default addrFlags).
func listAddrFlags() (map[netip.Addr]int, error) {
	list, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err // a partial dump too: no presence verdicts this sync
	}
	out := make(map[netip.Addr]int, len(list))
	for _, l := range list {
		if ip, ok := netip.AddrFromSlice(l.IP); ok {
			out[ip.Unmap()] = l.Flags
		}
	}
	return out, nil
}
