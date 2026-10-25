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
	"net/netip"
	"time"

	"github.com/go-kit/log"
	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
)

// The datapath's kernel objects can change under the agent: the main
// table's routes, which the pool-interface tables mirror (design 6.9),
// and anything else removing what the agent made (another firewall
// flushing the ruleset, networkd deleting routes). Watchers re-run the
// apply at once; reconcileInterval re-runs it regardless, for whatever no
// watcher sees. The apply is idempotent and writes nothing when the
// kernel already matches, so the events of its own writes settle after
// one more pass.
const (
	reconcileInterval = 30 * time.Second
	// resubscribe is how long a watcher waits to subscribe again after
	// its socket failed (an overrun, for one).
	resubscribe = 5 * time.Second
)

// watch runs subscriptions until stop closes, passing each one's events
// to handle, and calling kick once per subscription for whatever changed
// while it wasn't subscribed. subscribe's channel must close once done
// closes or the subscription fails; it is drained to the end, since a
// subscription blocks on a full channel.
func watch[T any](logger log.Logger, what string, stop <-chan struct{},
	subscribe func(done <-chan struct{}) (<-chan T, error), handle func(T), kick func()) {
	for {
		done := make(chan struct{})
		ch, err := subscribe(done)
		if err != nil {
			close(done)
			logging.Info(logger, "op", "egressWatch", "watch", what, "error", err, "msg", "cannot watch; retrying")
		} else {
			kick()
			ended := make(chan struct{})
			go func() {
				select {
				case <-stop:
				case <-ended:
				}
				close(done)
			}()
			for e := range ch {
				handle(e)
			}
			close(ended)
		}
		select {
		case <-stop:
			return
		case <-time.After(resubscribe):
		}
	}
}

// watchRoutes kicks when a main-table route out of a pool interface or a
// default route changes, or a route in one of the agent's own tables is
// deleted. pools
// is read on this goroutine; the datapath goroutine publishes it.
func watchRoutes(logger log.Logger, stop <-chan struct{}, pools func() map[int]bool, kick func()) {
	watch(logger, "routes", stop, func(done <-chan struct{}) (<-chan netlink.RouteUpdate, error) {
		ch := make(chan netlink.RouteUpdate, 256)
		return ch, netlink.RouteSubscribeWithOptions(ch, done, netlink.RouteSubscribeOptions{
			ErrorCallback: func(err error) {
				logging.Info(logger, "op", "egressWatch", "watch", "routes", "error", err, "msg", "watch failed; resubscribing")
			}})
	}, func(u netlink.RouteUpdate) {
		if poolRoute(u.Route, pools()) || (u.Type == unix.RTM_DELROUTE && ownTable(u.Route.Table)) {
			kick()
		}
	}, kick)
}

// watchTunnel kicks when the tunnel device is deleted or set down.
func watchTunnel(logger log.Logger, stop <-chan struct{}, kick func()) {
	watch(logger, "links", stop, func(done <-chan struct{}) (<-chan netlink.LinkUpdate, error) {
		ch := make(chan netlink.LinkUpdate, 64)
		return ch, netlink.LinkSubscribeWithOptions(ch, done, netlink.LinkSubscribeOptions{
			ErrorCallback: func(err error) {
				logging.Info(logger, "op", "egressWatch", "watch", "links", "error", err, "msg", "watch failed; resubscribing")
			}})
	}, func(u netlink.LinkUpdate) {
		if u.Link.Attrs().Name == datapath.TunnelName && (u.Header.Type == unix.RTM_DELLINK || u.IfInfomsg.Flags&unix.IFF_UP == 0) {
			kick()
		}
	}, kick)
}

// watchAddrs kicks when an address egress placed is deleted, as an IPv6
// address is when its interface goes down (D31: placed again at once).
// placed is read on this goroutine; the queue goroutine publishes it.
func watchAddrs(logger log.Logger, stop <-chan struct{}, placed func() map[netip.Addr]bool, kick func()) {
	watch(logger, "addresses", stop, func(done <-chan struct{}) (<-chan netlink.AddrUpdate, error) {
		ch := make(chan netlink.AddrUpdate, 256)
		return ch, netlink.AddrSubscribeWithOptions(ch, done, netlink.AddrSubscribeOptions{
			ErrorCallback: func(err error) {
				logging.Info(logger, "op", "egressWatch", "watch", "addresses", "error", err, "msg", "watch failed; resubscribing")
			}})
	}, func(u netlink.AddrUpdate) {
		if addr, ok := netip.AddrFromSlice(u.LinkAddress.IP); ok && !u.NewAddr && placed()[addr.Unmap()] {
			kick()
		}
	}, kick)
}

// watchNft kicks when anything in the hardening table is deleted.
func watchNft(logger log.Logger, stop <-chan struct{}, kick func()) {
	watch(logger, "nftables", stop, func(done <-chan struct{}) (<-chan *nftables.MonitorEvent, error) {
		c, err := nftables.New()
		if err != nil {
			return nil, err
		}
		// Every action: in google/nftables v0.3.0 the delete-only filter
		// drops every message ("flags&1<<type" is "(flags&1)<<type"), the
		// generation messages that release each batch included.
		m := nftables.NewMonitor(nftables.WithMonitorAction(nftables.MonitorActionAny),
			nftables.WithMonitorObject(nftables.MonitorObjectAny))
		ch, err := c.AddMonitor(m)
		if err != nil {
			return nil, err
		}
		go func() {
			<-done
			_ = m.Close()
		}()
		return ch, nil
	}, func(e *nftables.MonitorEvent) {
		if nftDeletion(e.Type) && nftTableOf(e) == datapath.NftTable {
			kick()
		}
	}, kick)
}

// nftDeletion reports whether an event type deletes a table, chain, rule
// or set.
func nftDeletion(t nftables.MonitorEventType) bool {
	switch t {
	case nftables.MonitorEventTypeDelTable, nftables.MonitorEventTypeDelChain,
		nftables.MonitorEventTypeDelRule, nftables.MonitorEventTypeDelSet:
		return true
	}
	return false
}

// nftTableOf is the table an nftables event is about; "" when the event
// doesn't say. The library leaves the table off set events, so a deleted
// set counts as ours (kube-proxy's nftables mode changes elements, not
// sets). Element events never name it either, and kube-proxy makes many,
// so the periodic reconcile, not this watch, restores a deleted element.
func nftTableOf(e *nftables.MonitorEvent) string {
	switch d := e.Data.(type) {
	case *nftables.Table:
		if d != nil {
			return d.Name
		}
	case *nftables.Chain:
		if d != nil && d.Table != nil {
			return d.Table.Name
		}
	case *nftables.Rule:
		if d != nil && d.Table != nil {
			return d.Table.Name
		}
	case *nftables.Set:
		return datapath.NftTable
	}
	return ""
}

// poolRoute reports whether r is a main-table route out of one of pools,
// or a main-table default route (what pl_announce's uplinks follow without
// the guard).
func poolRoute(r netlink.Route, pools map[int]bool) bool {
	if r.Table != unix.RT_TABLE_MAIN {
		return false
	}
	if pools[r.LinkIndex] || defaultRoute(r) {
		return true
	}
	for _, nh := range r.MultiPath {
		if pools[nh.LinkIndex] {
			return true
		}
	}
	return false
}

// ownTable reports whether table is one the agent fills: the tunnel and
// prohibit targets, or a pool interface's.
func ownTable(table int) bool {
	return table == datapath.TunnelTable || table == datapath.ProhibitTable ||
		(table >= datapath.PoolTableBase && table < datapath.PoolTableBase+datapath.MaxPoolInterfaces)
}

// defaultRoute reports whether r is a default route, either family.
func defaultRoute(r netlink.Route) bool {
	if r.Dst == nil {
		return true
	}
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}
