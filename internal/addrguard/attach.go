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
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	"purelb.io/internal/logging"
	"purelb.io/internal/netutil"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

const (
	hookTCX = "tcx"
	hookXDP = "xdp"
)

// attachment is the program attached to one link.
type attachment struct {
	link link.Link
	name string
	hook string // what is attached
	want string // what was asked for; differs after an XDP->tcx fallback
}

// attacher is the state owned by the attacher goroutine.
type attacher struct {
	g        *Guard
	md       *mapDataplane // nil when the program isn't loaded
	spec     *Spec
	agent    *purelbv2.LBNodeAgent
	attached map[int]*attachment
	special  map[netip.Addr]bool // broadcast/anycast entries we inserted
	defaults map[int]bool
	watch    *watcher
	// Event de-duplication: warn on transitions, not on every reconcile.
	warnedUnavailable bool
	warnedLinks       map[string]bool
	// standingDown is the stand-down this goroutine last logged, for logs
	// and events on transitions. The hook follows Guard.down instead.
	standingDown bool
	// notReady says why the guard isn't working, for the stand-down event.
	notReady string
	// chainPos is the guard's last reported position in each tcx chain it
	// is attached to, by ifindex: 1 means it runs first.
	chainPos map[int]int
	tcxID    ebpf.ProgramID
}

// run is the attacher goroutine. It alone touches attachments, the config
// map and the broadcast/anycast entries.
func (g *Guard) run() {
	defer close(g.done)
	a := &attacher{
		g:           g,
		attached:    map[int]*attachment{},
		special:     map[netip.Addr]bool{},
		warnedLinks: map[string]bool{},
	}
	if g.objs != nil {
		a.md = &mapDataplane{objs: g.objs}
	}
	// Nothing announces a program attached to tcx after the guard, so its
	// place in the chain is checked on a timer as well as after a reconcile.
	tick := time.NewTicker(chainCheckInterval)
	defer tick.Stop()
	for {
		var linkCh <-chan netlink.LinkUpdate
		var routeCh <-chan netlink.RouteUpdate
		if a.watch != nil {
			linkCh, routeCh = a.watch.links, a.watch.routes
		}
		select {
		case <-g.stop:
			a.watch.close()
			g.retained = a.attached
			return
		case u := <-g.cfgCh:
			a.apply(u)
		case <-tick.C:
			a.checkChain()
		case lu, ok := <-linkCh:
			if !ok {
				a.rewatch()
				continue
			}
			if a.relevantLink(lu) {
				a.reconcileAndPublish(false)
			}
		case ru, ok := <-routeCh:
			if !ok {
				a.rewatch()
				continue
			}
			if specialRouteEvent(ru) {
				a.reconcileSpecial()
			} else if netutil.IsDefaultRoute(ru.Route) {
				a.reconcileAndPublish(false)
			}
		}
	}
}

func (a *attacher) apply(u configUpdate) {
	defer a.g.applied.Store(true)
	a.spec, a.agent = u.spec, u.agent
	a.noteRestartRequired()
	if a.spec == nil {
		a.warnedUnavailable = false
		a.watch.close()
		a.watch = nil
		a.reconcileLinks(false)
		a.reconcileSpecial()
		a.publish(false, "")
		return
	}
	if !a.g.loadWanted {
		// Enabled after the agent started: the program isn't loaded, and
		// nothing changes until the agent restarts. The node keeps
		// announcing, unfiltered as it was a moment ago -- standing down
		// here would turn an LBNodeAgent edit into every node withdrawing
		// at once. publish leaves a guard that isn't loaded by choice out
		// of the stand-down.
		a.publish(false, "enabled after startup; waiting for an agent restart")
		return
	}
	if a.md == nil {
		consequence := "VIPs on this node are NOT filtered"
		if a.spec.Closed {
			consequence = "this node announces no addresses (failurePolicy: closed)"
		}
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardUnavailable", "error", a.g.loadErr,
			"msg", "address guard is configured but not running; "+consequence)
		if !a.warnedUnavailable {
			a.warn("AddressGuardUnavailable", "address guard is not running on node %s, so %s: %v", a.g.myNode, consequence, a.g.loadErr)
			a.warnedUnavailable = true
		}
		a.publish(false, fmt.Sprintf("not running: %v", a.g.loadErr))
		return
	}
	if err := a.md.setConfig(a.spec); err != nil {
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardMapWriteFailed", "write", "config", "error", err)
	}
	if a.watch == nil {
		a.watch = newWatcher(a.g)
	}
	a.reconcileAndPublish(true)
}

// noteRestartRequired records, and reports when it changes, whether the
// configuration differs from what the agent loaded at startup: an enabled
// guard that isn't loaded, or a disabled one that still is. Either takes
// full effect at the next agent restart.
func (a *attacher) noteRestartRequired() {
	want := (a.spec != nil) != a.g.loadWanted
	if a.g.restartRequired.Swap(want) == want {
		return
	}
	switch {
	case !want:
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardRestartNotRequired",
			"msg", "the address guard configuration matches what is loaded again")
	case a.spec != nil:
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardRestartRequired",
			"msg", "the address guard was enabled after lbnodeagent started: it is not loaded, and VIPs on this node stay unfiltered until lbnodeagent is restarted")
		a.warn("AddressGuardRestartRequired", "the address guard was enabled after lbnodeagent started on node %s; it takes effect when lbnodeagent is restarted", a.g.myNode)
	default:
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardRestartRequired",
			"msg", "the address guard was disabled and has detached; its program stays loaded until lbnodeagent is restarted")
		a.warn("AddressGuardRestartRequired", "the address guard was disabled on node %s and has detached; restart lbnodeagent to unload it", a.g.myNode)
	}
}

// reconcileAndPublish brings attachments and the broadcast/anycast entries
// in line, then publishes whether the guard is working.
func (a *attacher) reconcileAndPublish(logConfig bool) {
	ok, why := a.reconcileLinks(logConfig)
	a.reconcileSpecial()
	a.checkChain()
	a.publish(ok, why)
}

// chainCheckInterval is how often the attacher checks that the guard still
// runs first in each tcx chain.
const chainCheckInterval = 30 * time.Second

// queryTCX returns the programs attached to a link's tcx ingress chain, in
// the order they run.
var queryTCX = func(ifindex int) ([]ebpf.ProgramID, error) {
	res, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: ebpf.AttachTCXIngress})
	if err != nil {
		return nil, err
	}
	ids := make([]ebpf.ProgramID, 0, len(res.Programs))
	for _, p := range res.Programs {
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// chainPosition is ours' place in ids, counting from 1; 0 if absent.
func chainPosition(ids []ebpf.ProgramID, ours ebpf.ProgramID) int {
	for i, id := range ids {
		if id == ours {
			return i + 1
		}
	}
	return 0
}

// checkChain reports where the guard runs in each tcx chain it is attached
// to. It attaches at the head, but a program attached at the head later runs
// before it and can drop or redirect VIP traffic the guard never sees.
// Reported in the chain_position metric, and on a change in the log and as
// an Event naming the programs in front (bpftool prog show id N on the node
// says what they are). XDP has a single slot per link: no order to report.
func (a *attacher) checkChain() {
	if a.g.objs == nil {
		return
	}
	if a.chainPos == nil {
		a.chainPos = map[int]int{}
	}
	if a.tcxID == 0 {
		info, err := a.g.objs.AgTcx.Info()
		if err != nil {
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "chainCheckFailed", "error", err)
			return
		}
		a.tcxID, _ = info.ID()
	}
	for idx, at := range a.attached {
		if at.hook != hookTCX {
			a.forgetChain(idx, at.name)
			continue
		}
		ids, err := queryTCX(idx)
		if err != nil {
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "chainCheckFailed", "interface", at.name, "error", err)
			continue
		}
		pos := chainPosition(ids, a.tcxID)
		if pos == 0 {
			continue // detached underneath us; the next reconcile re-attaches
		}
		chainPositionVec.WithLabelValues(at.name).Set(float64(pos))
		prev := a.chainPos[idx]
		a.chainPos[idx] = pos
		switch {
		case pos > 1 && pos != prev:
			before := fmt.Sprint(ids[:pos-1])
			logging.Info(a.g.logger, "op", "addressGuard", "event", "guardNotFirst", "interface", at.name,
				"position", pos, "before", before,
				"msg", "other tcx programs run before the address guard and can drop or redirect VIP traffic it never sees")
			a.warn("AddressGuardNotFirst", "on node %s, tcx programs %s run before the address guard on %s; see bpftool prog show id <id> on the node",
				a.g.myNode, before, at.name)
		case pos == 1 && prev > 1:
			logging.Info(a.g.logger, "op", "addressGuard", "event", "guardFirstAgain", "interface", at.name,
				"msg", "the address guard runs first in the tcx chain again")
		}
	}
}

// forgetChain drops idx's chain position when it no longer applies.
func (a *attacher) forgetChain(idx int, name string) {
	if _, ok := a.chainPos[idx]; ok {
		delete(a.chainPos, idx)
		chainPositionVec.DeleteLabelValues(name)
	}
}

// publish records whether this node must stand down -- configured
// fail-closed and the guard isn't working -- and calls the node agent's hook
// when that changes. Called after every change the attacher makes. A guard
// enabled after startup isn't loaded by choice, not by failure, and never
// stands the node down.
func (a *attacher) publish(ready bool, why string) {
	if !ready {
		a.notReady = why
	}
	down := a.spec != nil && a.spec.Closed && a.g.loadWanted && !ready
	// Swap, not a comparison with what this goroutine last published:
	// SetConfig stands the node down before the first apply, and the hook
	// must hear when that ends, or Services withdrawn meanwhile stay
	// withdrawn until the next informer resync.
	prev := a.g.down.Swap(down)
	if down != a.standingDown {
		a.standingDown = down
		if down {
			logging.Info(a.g.logger, "op", "addressGuard", "event", "guardStandingDown", "reason", a.notReady,
				"msg", "the address guard isn't working and failurePolicy is closed: this node announces no addresses")
			a.warn("AddressGuardStandingDown", "node %s announces no addresses: the address guard isn't working (%s) and failurePolicy is closed", a.g.myNode, a.notReady)
		} else {
			logging.Info(a.g.logger, "op", "addressGuard", "event", "guardStandDownEnded",
				"msg", "the address guard is working: this node announces addresses again")
		}
	}
	if fn := a.g.standDownHook.Load(); fn != nil && down != prev {
		(*fn)(down)
	}
}

// rewatch replaces a subscription whose channel netlink closed (it does
// that on any receive error, e.g. ENOBUFS during a burst of pod veths), and
// reconciles in case an event was lost.
func (a *attacher) rewatch() {
	a.watch.close()
	a.watch = nil
	if a.spec == nil || a.md == nil {
		return
	}
	logging.Info(a.g.logger, "op", "addressGuard", "event", "netlinkResubscribe")
	a.watch = newWatcher(a.g)
	a.reconcileAndPublish(false)
}

// relevantLink filters link events: pod veths come and go constantly and
// are never guarded, unless one carries the default route or is listed.
func (a *attacher) relevantLink(lu netlink.LinkUpdate) bool {
	if lu.Link == nil || lu.Link.Type() != "veth" {
		return true
	}
	attrs := lu.Link.Attrs()
	return a.defaults[attrs.Index] || (a.spec != nil && slices.Contains(a.spec.Extra, attrs.Name))
}

// specialRouteEvent reports whether a route change is to a local-table
// broadcast or anycast route: one of the kernel-generated addresses on the
// dummy interface. ru.Type is the netlink message type (RTM_NEWROUTE...);
// the route's own type is ru.Route.Type -- reading the former matched
// nothing, so pools added after the guard was enabled went unguarded.
func specialRouteEvent(ru netlink.RouteUpdate) bool {
	return ru.Route.Table == unix.RT_TABLE_LOCAL &&
		(ru.Route.Type == unix.RTN_BROADCAST || ru.Route.Type == unix.RTN_ANYCAST)
}

// selectLinks is the attach rule. Guard where external traffic enters:
// physical NICs, bonds (in place of their slaves), and the default-route
// links whatever their kind; plus extra, minus exclude. A default-route
// VLAN or macvlan on a guarded parent is covered by the parent, which sees
// the packet first. It also returns the excluded names that exist.
func selectLinks(links []netlink.Link, defaults map[int]bool, extra, exclude []string) (map[int]netlink.Link, []string) {
	byIndex := map[int]netlink.Link{}
	for _, l := range links {
		byIndex[l.Attrs().Index] = l
	}
	selected := map[int]netlink.Link{}
	for _, l := range links {
		attrs := l.Attrs()
		if attrs.Flags&net.FlagLoopback != 0 {
			continue
		}
		switch l.Type() {
		case "device": // a physical NIC: no link kind
			if m, ok := byIndex[attrs.MasterIndex]; ok && m.Type() == "bond" {
				continue
			}
			selected[attrs.Index] = l
		case "bond":
			selected[attrs.Index] = l
		}
	}
	for idx := range defaults {
		l, ok := byIndex[idx]
		if !ok || l.Attrs().Flags&net.FlagLoopback != 0 {
			continue
		}
		switch l.Type() {
		case "vlan", "macvlan", "ipvlan", "macvtap":
			if _, parentGuarded := selected[l.Attrs().ParentIndex]; parentGuarded {
				continue
			}
		}
		selected[idx] = l
	}
	for _, l := range links {
		if name := l.Attrs().Name; name != "lo" && slices.Contains(extra, name) {
			selected[l.Attrs().Index] = l
		}
	}
	var excluded []string
	for idx, l := range selected {
		if name := l.Attrs().Name; slices.Contains(exclude, name) {
			delete(selected, idx)
			excluded = append(excluded, name)
		}
	}
	slices.Sort(excluded)
	return selected, excluded
}

// wantHook is the hook asked for on l: XDP only where asked and only on
// Ethernet links (XDP would misread an L3 device's packets).
func (a *attacher) wantHook(l netlink.Link) string {
	if a.spec.XDP && l.Attrs().EncapType == "ether" {
		return hookXDP
	}
	return hookTCX
}

func defaultIfindexes() map[int]bool {
	d := map[int]bool{}
	for _, fam := range []int{nl.FAMILY_V4, nl.FAMILY_V6} {
		if l, err := netutil.DefaultInterface(fam); err == nil && l != nil {
			d[l.Attrs().Index] = true
		}
	}
	return d
}

// reconcileLinks makes the attachments match the attach rule. It reports
// whether every link in the attach set is guarded, and if not, why.
func (a *attacher) reconcileLinks(logExcluded bool) (bool, string) {
	if a.spec == nil || a.md == nil {
		for idx := range a.attached {
			a.detach(idx)
		}
		return false, "not configured"
	}
	links, err := netlink.LinkList()
	if err != nil {
		logging.Info(a.g.logger, "op", "addressGuard", "event", "linkListFailed", "error", err)
		return false, fmt.Sprintf("listing links: %v", err)
	}
	a.defaults = defaultIfindexes()
	want, excluded := selectLinks(links, a.defaults, a.spec.Extra, a.spec.Exclude)
	if logExcluded {
		for _, name := range excluded {
			logging.Info(a.g.logger, "op", "addressGuard", "event", "guardInterfaceExcluded", "interface", name,
				"msg", "traffic to VIPs arriving on this interface is NOT filtered")
		}
	}

	for idx, at := range a.attached {
		if l, ok := want[idx]; !ok || l.Attrs().Name != at.name {
			a.detach(idx)
		}
	}
	var names, unguarded []string
	for idx, l := range want {
		names = append(names, l.Attrs().Name)
		hook := a.wantHook(l)
		old := a.attached[idx]
		if old != nil && old.want == hook {
			continue
		}
		at, err := a.attach(l, hook)
		if err != nil {
			name := l.Attrs().Name
			if old == nil {
				unguarded = append(unguarded, name)
			}
			attachErrors.WithLabelValues(name).Inc()
			logging.Info(a.g.logger, "op", "addressGuard", "event", "guardAttachFailed", "interface", name, "error", err,
				"msg", "traffic to VIPs arriving on this interface is NOT filtered")
			if !a.warnedLinks[name] {
				a.warn("AddressGuardAttachFailed", "address guard could not attach to %s on node %s; VIP traffic arriving there is not filtered: %v", name, a.g.myNode, err)
				a.warnedLinks[name] = true
			}
			continue
		}
		delete(a.warnedLinks, at.name)
		// Make before break: the new hook is in place before the old one
		// goes, so a hook change opens no window.
		if old != nil {
			_ = old.link.Close()
			attachedVec.DeleteLabelValues(old.name, old.hook)
		}
		a.attached[idx] = at
		attachedVec.WithLabelValues(at.name, at.hook).Set(1)
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardAttached", "interface", at.name, "hook", at.hook)
	}
	if logExcluded {
		slices.Sort(names)
		mode := "enforce"
		if a.spec.Monitor {
			mode = "monitor"
		}
		policy := "open"
		if a.spec.Closed {
			policy = "closed"
		}
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardConfigApplied", "mode", mode,
			"hook", map[bool]string{true: hookXDP, false: hookTCX}[a.spec.XDP], "failurePolicy", policy,
			"interfaces", names)
	}
	if len(unguarded) > 0 {
		slices.Sort(unguarded)
		return false, fmt.Sprintf("could not attach to %s", strings.Join(unguarded, ", "))
	}
	return true, ""
}

func (a *attacher) attach(l netlink.Link, hook string) (*attachment, error) {
	attrs := l.Attrs()
	at := &attachment{name: attrs.Name, want: hook}
	if hook == hookXDP {
		lk, err := link.AttachXDP(link.XDPOptions{Program: a.g.objs.AgXdp, Interface: attrs.Index, Flags: link.XDPDriverMode})
		if err == nil {
			at.link, at.hook = lk, hookXDP
			return at, nil
		}
		logging.Info(a.g.logger, "op", "addressGuard", "event", "xdpFallbackToTcx", "interface", attrs.Name, "error", err)
	}
	lk, err := link.AttachTCX(link.TCXOptions{
		Program:   a.g.objs.AgTcx,
		Interface: attrs.Index,
		Attach:    ebpf.AttachTCXIngress,
		Anchor:    link.Head(),
	})
	if err != nil {
		return nil, err
	}
	at.link, at.hook = lk, hookTCX
	return at, nil
}

func (a *attacher) detach(idx int) {
	at := a.attached[idx]
	_ = at.link.Close()
	attachedVec.DeleteLabelValues(at.name, at.hook)
	a.forgetChain(idx, at.name)
	delete(a.attached, idx)
	logging.Info(a.g.logger, "op", "addressGuard", "event", "guardDetached", "interface", at.name, "hook", at.hook)
}

// reconcileSpecial guards the extra local addresses the kernel creates on
// the dummy interface for each remote pool subnet: the IPv4 subnet
// broadcast and the IPv6 subnet-router anycast. They are read from the
// kernel's local routing table, so no mask logic is duplicated here, and
// get no allowed ports. Without this, UDP to them reaches every wildcard
// socket on the host (flannel's VXLAN port among them).
func (a *attacher) reconcileSpecial() {
	want := map[netip.Addr]bool{}
	if a.spec != nil && a.md != nil {
		if dummy, err := netlink.LinkByName(a.spec.Dummy); err == nil {
			filter := &netlink.Route{Table: unix.RT_TABLE_LOCAL, LinkIndex: dummy.Attrs().Index}
			for _, fam := range []int{nl.FAMILY_V4, nl.FAMILY_V6} {
				routes, err := netlink.RouteListFiltered(fam, filter, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
				if err != nil {
					logging.Info(a.g.logger, "op", "addressGuard", "event", "routeListFailed", "error", err)
					continue
				}
				for _, r := range routes {
					if (r.Type != unix.RTN_BROADCAST && r.Type != unix.RTN_ANYCAST) || r.Dst == nil {
						continue
					}
					addr, ok := netip.AddrFromSlice(r.Dst.IP)
					if !ok || addr.Unmap().IsLinkLocalUnicast() {
						continue
					}
					want[addr.Unmap()] = true
				}
			}
		}
	}
	for addr := range a.special {
		if !want[addr] && a.md != nil {
			if err := a.md.delVIP(addr); err == nil {
				delete(a.special, addr)
				logging.Debug(a.g.logger, "op", "addressGuard", "event", "specialUnguarded", "ip", addr)
			}
		}
	}
	for addr := range want {
		if a.special[addr] {
			continue
		}
		inserted, err := a.md.putSpecial(addr)
		if err != nil {
			logging.Info(a.g.logger, "op", "addressGuard", "event", "guardMapWriteFailed", "ip", addr, "write", "special address", "error", err)
			continue
		}
		if inserted {
			a.special[addr] = true
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "specialGuarded", "ip", addr)
		}
	}
}

func (a *attacher) warn(reason, msg string, args ...interface{}) {
	if c := a.g.client.Load(); c != nil && a.agent != nil {
		c.Errorf(a.agent, reason, msg, args...)
	}
}

// watcher holds the link and route subscriptions.
type watcher struct {
	links  chan netlink.LinkUpdate
	routes chan netlink.RouteUpdate
	done   chan struct{}
}

func newWatcher(g *Guard) *watcher {
	w := &watcher{
		links:  make(chan netlink.LinkUpdate, 64),
		routes: make(chan netlink.RouteUpdate, 64),
		done:   make(chan struct{}),
	}
	onErr := func(err error) {
		logging.Debug(g.logger, "op", "addressGuard", "event", "netlinkWatchError", "error", err)
	}
	if err := netlink.LinkSubscribeWithOptions(w.links, w.done, netlink.LinkSubscribeOptions{ErrorCallback: onErr}); err != nil {
		logging.Info(g.logger, "op", "addressGuard", "event", "linkSubscribeFailed", "error", err)
	}
	if err := netlink.RouteSubscribeWithOptions(w.routes, w.done, netlink.RouteSubscribeOptions{ErrorCallback: onErr}); err != nil {
		logging.Info(g.logger, "op", "addressGuard", "event", "routeSubscribeFailed", "error", err)
	}
	return w
}

// close is safe on a nil watcher.
func (w *watcher) close() {
	if w != nil {
		close(w.done)
	}
}
