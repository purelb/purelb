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
	l3   bool   // tcx: ag_tcx_l3 (no link-layer header), not ag_tcx
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
	tcxL3ID  ebpf.ProgramID
	xdpID    ebpf.ProgramID
	// cfgErr is the last failure writing the config map (mode, allowed
	// protocols); while set the guard isn't working as configured, and the
	// chain-check tick retries the write.
	cfgErr error
	// setConfig writes the config map; nil means md.setConfig. Tests
	// replace it to make the write fail.
	setConfig func(*Spec) error
	// pending: a reconcile couldn't complete because a netlink dump failed
	// (or stayed interrupted); the tick runs it again until one does.
	pending bool
	// linksOK and linksWhy are reconcileLinks' last complete result,
	// returned unchanged when a reconcile has to be deferred.
	linksOK  bool
	linksWhy string
}

// The link and route dumps, replaceable in tests.
var (
	linkList  = netlink.LinkList
	routeList = netlink.RouteListFiltered
)

// dumpAttempts is how often an interrupted netlink dump is tried. The
// kernel interrupts a dump when what it lists changes underneath it -- pod
// veths come and go constantly -- so it says nothing about the guard: the
// dump is simply run again.
const dumpAttempts = 3

// dump runs f until it isn't interrupted, dumpAttempts at most.
func dump[T any](f func() (T, error)) (T, error) {
	var v T
	var err error
	for range dumpAttempts {
		if v, err = f(); !errors.Is(err, netlink.ErrDumpInterrupted) {
			return v, err
		}
	}
	return v, err
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
		linksWhy:    "not attached yet",
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
			a.retry()
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
	a.writeConfig()
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
	if a.checkChain() {
		// A link lost its program underneath us: attach it again now.
		ok, why = a.reconcileLinks(false)
	}
	if ok && a.cfgErr != nil {
		ok, why = false, fmt.Sprintf("writing the guard's configuration: %v", a.cfgErr)
	}
	a.publish(ok, why)
}

// writeConfig writes the mode and allowed protocols to the config map. A
// failure leaves the program running with the previous ones -- enforcing
// in monitor mode, or the reverse -- so it counts as the guard not working
// until a retry succeeds.
func (a *attacher) writeConfig() {
	set := a.setConfig
	if set == nil {
		set = a.md.setConfig
	}
	err := set(a.spec)
	switch {
	case err != nil && a.cfgErr == nil:
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardMapWriteFailed", "write", "config", "error", err)
	case err != nil:
		logging.Debug(a.g.logger, "op", "addressGuard", "event", "guardMapWriteFailed", "write", "config", "error", err)
	case a.cfgErr != nil:
		logging.Info(a.g.logger, "op", "addressGuard", "event", "guardConfigWritten", "msg", "the config map write succeeded on retry")
	}
	a.cfgErr = err
}

// retry runs on the chain-check tick: it retries what has no event to
// trigger it again -- a config map write, a netlink subscription that
// failed -- and checks every attachment is still in place.
func (a *attacher) retry() {
	if a.spec == nil || a.md == nil {
		a.checkChain()
		return
	}
	retried := false
	if a.cfgErr != nil {
		a.writeConfig()
		retried = true
	}
	if a.watch != nil && a.watch.failed {
		a.rewatch() // reconciles
		return
	}
	if a.checkChain() || retried || a.pending {
		a.reconcileAndPublish(false)
	}
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

// queryXDP returns the program attached to a link's XDP hook, 0 if none.
var queryXDP = func(ifindex int) (ebpf.ProgramID, error) {
	l, err := netlink.LinkByIndex(ifindex)
	if err != nil {
		return 0, err
	}
	if x := l.Attrs().Xdp; x != nil && x.Attached {
		return ebpf.ProgramID(x.ProgId), nil
	}
	return 0, nil
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
//
// It also returns whether any attachment has lost its program -- detached
// by something else, which the kernel doesn't tell us about -- after
// dropping it from attached, so the caller's reconcile attaches it again.
func (a *attacher) checkChain() (lost bool) {
	if a.g.objs == nil {
		return false
	}
	if a.chainPos == nil {
		a.chainPos = map[int]int{}
	}
	if a.tcxID == 0 {
		info, err := a.g.objs.AgTcx.Info()
		if err != nil {
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "chainCheckFailed", "error", err)
			return false
		}
		a.tcxID, _ = info.ID()
	}
	if a.tcxL3ID == 0 {
		info, err := a.g.objs.AgTcxL3.Info()
		if err != nil {
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "chainCheckFailed", "error", err)
			return false
		}
		a.tcxL3ID, _ = info.ID()
	}
	if a.xdpID == 0 {
		info, err := a.g.objs.AgXdp.Info()
		if err != nil {
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "chainCheckFailed", "error", err)
			return false
		}
		a.xdpID, _ = info.ID()
	}
	for idx, at := range a.attached {
		if at.hook != hookTCX {
			a.forgetChain(idx, at.name)
			id, err := queryXDP(idx)
			if err != nil {
				logging.Debug(a.g.logger, "op", "addressGuard", "event", "chainCheckFailed", "interface", at.name, "error", err)
				continue // gone, most likely: the link watch reconciles it
			}
			if id != a.xdpID {
				a.lose(idx, at)
				lost = true
			}
			continue
		}
		ids, err := queryTCX(idx)
		if err != nil {
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "chainCheckFailed", "interface", at.name, "error", err)
			continue
		}
		ours := a.tcxID
		if at.l3 {
			ours = a.tcxL3ID
		}
		pos := chainPosition(ids, ours)
		if pos == 0 {
			a.lose(idx, at)
			lost = true
			continue
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
	return lost
}

// lose drops an attachment whose program something else detached.
func (a *attacher) lose(idx int, at *attachment) {
	logging.Info(a.g.logger, "op", "addressGuard", "event", "guardDetachedExternally", "interface", at.name, "hook", at.hook,
		"msg", "something else detached the address guard; attaching it again")
	a.warn("AddressGuardDetached", "on node %s, something detached the address guard from %s (%s); it is being attached again",
		a.g.myNode, at.name, at.hook)
	_ = at.link.Close()
	attachedVec.DeleteLabelValues(at.name, at.hook)
	a.forgetChain(idx, at.name)
	delete(a.attached, idx)
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
		unattachedVec.Reset()
		return false, "not configured"
	}
	links, err := dump(linkList)
	if errors.Is(err, netlink.ErrDumpInterrupted) {
		// Still interrupted: the list may be missing links, and acting on
		// it could detach a link that's there. Change nothing -- the
		// attachments stay, and so does whether the guard is working --
		// and try again on the tick.
		a.pending = true
		reconcileDeferred.WithLabelValues("links").Inc()
		logging.Debug(a.g.logger, "op", "addressGuard", "event", "reconcileDeferred", "dump", "links", "error", err)
		return a.linksOK, a.linksWhy
	}
	if err != nil {
		a.pending = true
		logging.Info(a.g.logger, "op", "addressGuard", "event", "linkListFailed", "error", err)
		return false, fmt.Sprintf("listing links: %v", err)
	}
	a.pending = false
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
		at, err := a.attach(l, hook, old)
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
		if old != nil && at.link == old.link {
			a.attached[idx] = at
			logging.Debug(a.g.logger, "op", "addressGuard", "event", "guardAttachmentKept", "interface", at.name,
				"hook", at.hook, "want", at.want)
			continue
		}
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
	// The state, for alerting: attach_errors_total says only that an attempt
	// failed, not whether the interface is unguarded now.
	unattachedVec.Reset()
	for _, name := range unguarded {
		unattachedVec.WithLabelValues(name).Set(1)
	}
	a.linksOK, a.linksWhy = true, ""
	if len(unguarded) > 0 {
		slices.Sort(unguarded)
		a.linksOK, a.linksWhy = false, fmt.Sprintf("could not attach to %s", strings.Join(unguarded, ", "))
	}
	return a.linksOK, a.linksWhy
}

// arphrdNone is how netlink names ARPHRD_NONE, the link type of tun devices
// and WireGuard: no link-layer header.
const arphrdNone = "none"

// linkLayer reports which tcx program fits l: ag_tcx for Ethernet, whose
// network header follows a 14-byte Ethernet header, or ag_tcx_l3 (l3 true)
// for links with no link-layer header, whose packets start with it. Each
// type here was measured at tcx ingress (QinQ included: its inner tag sits
// at 14); the guard refuses any other type rather than read a link it
// can't parse.
func linkLayer(l netlink.Link) (l3 bool, err error) {
	switch l.Attrs().EncapType {
	case "ether":
		return false, nil
	case "ipip", "tunnel6", "sit", "gre":
		return true, nil
	}
	switch l.Type() {
	case "ip6gre": // netlink has no name for ARPHRD_IP6GRE
		return true, nil
	case "wireguard", "tuntap":
		if l.Attrs().EncapType == arphrdNone { // a tap is "ether", above
			return true, nil
		}
	}
	return false, fmt.Errorf("the address guard can't parse %s links (link type %s)", l.Type(), l.Attrs().EncapType)
}

// attach attaches at hook, falling back to tcx where native XDP isn't
// available. old is what is attached to l now, or nil. If tcx is the result
// and old is already the tcx attachment, old's link is kept: the kernel
// refuses to put one program into a tcx chain twice (EEXIST).
func (a *attacher) attach(l netlink.Link, hook string, old *attachment) (*attachment, error) {
	attrs := l.Attrs()
	at := &attachment{name: attrs.Name, want: hook}
	l3, err := linkLayer(l)
	if err != nil {
		return nil, err
	}
	at.l3 = l3
	if hook == hookXDP {
		lk, err := link.AttachXDP(link.XDPOptions{Program: a.g.objs.AgXdp, Interface: attrs.Index, Flags: link.XDPDriverMode})
		if err == nil {
			at.link, at.hook = lk, hookXDP
			return at, nil
		}
		logging.Info(a.g.logger, "op", "addressGuard", "event", "xdpFallbackToTcx", "interface", attrs.Name, "error", err)
	}
	if old != nil && old.hook == hookTCX && old.l3 == l3 {
		at.link, at.hook = old.link, hookTCX
		return at, nil
	}
	prog := a.g.objs.AgTcx
	if l3 {
		prog = a.g.objs.AgTcxL3
	}
	lk, err := link.AttachTCX(link.TCXOptions{
		Program:   prog,
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
				routes, err := dump(func() ([]netlink.Route, error) {
					return routeList(fam, filter, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
				})
				if err != nil {
					// An incomplete list would unguard the addresses missing
					// from it: change nothing, and try again on the tick.
					a.pending = true
					if errors.Is(err, netlink.ErrDumpInterrupted) {
						reconcileDeferred.WithLabelValues("routes").Inc()
						logging.Debug(a.g.logger, "op", "addressGuard", "event", "reconcileDeferred", "dump", "routes", "error", err)
					} else {
						logging.Info(a.g.logger, "op", "addressGuard", "event", "routeListFailed", "error", err)
					}
					return
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
	// Which subscriptions are running. netlink closes a running one's
	// channel when it stops; a failed one is retried on the tick.
	linksOK, routesOK bool
	failed            bool
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
		logging.Info(g.logger, "op", "addressGuard", "event", "linkSubscribeFailed", "error", err,
			"msg", "new or changed interfaces go unnoticed until the retry succeeds")
	} else {
		w.linksOK = true
	}
	if err := netlink.RouteSubscribeWithOptions(w.routes, w.done, netlink.RouteSubscribeOptions{ErrorCallback: onErr}); err != nil {
		logging.Info(g.logger, "op", "addressGuard", "event", "routeSubscribeFailed", "error", err,
			"msg", "default-route moves and new pool subnets go unnoticed until the retry succeeds")
	} else {
		w.routesOK = true
	}
	w.failed = !w.linksOK || !w.routesOK
	return w
}

// close is safe on a nil watcher.
//
// netlink's receive goroutine sends on its channel without watching done,
// so a goroutine blocked on a full channel nobody reads any more would
// never exit: each running subscription's channel is drained until
// netlink closes it, which it does once done has closed its socket.
func (w *watcher) close() {
	if w == nil {
		return
	}
	close(w.done)
	if w.linksOK {
		go func() {
			for range w.links {
			}
		}()
	}
	if w.routesOK {
		go func() {
			for range w.routes {
			}
		}()
	}
}
