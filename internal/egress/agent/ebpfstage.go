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
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"path/filepath"
	"slices"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"purelb.io/internal/addrguard"
	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// stageOutPrefix names pl_pod_out's links, pinned as pl_pod_out-<interface>.
const stageOutPrefix = "pl_pod_out-"

var (
	chainReattached = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "chain_reattached_total",
		Help: "Stage programs attached again because another program went ahead of them in a chain (a CNI restart).",
	})
	natInsertFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "nat_insert_failed_total",
		Help: "New flows of the eBPF pod stage dropped for no free port in their slice, or a full NAT table.",
	}, []string{"family"})
	natFlows = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "nat_flows",
		Help: "Flows in the eBPF pod stage's NAT table.",
	})
)

func init() {
	prometheus.MustRegister(chainReattached, natInsertFailed, natFlows)
}

// linkIfindex is the device a tcx or netkit link is attached to, 0 when it is
// defunct.
func linkIfindex(info *link.Info) int {
	if t := info.TCX(); t != nil {
		return int(t.Ifindex)
	}
	if n := info.Netkit(); n != nil {
		return int(n.Ifindex)
	}
	return 0
}

// podTagAtHead reports whether pl_pod_tag is first in d's chain (design 6.4).
func podTagAtHead(st *dpState, d datapath.PodDevice) bool {
	hook := ebpf.AttachTCXIngress
	if d.Kind == "netkit" {
		hook = ebpf.AttachNetkitPeer
	}
	return datapath.AtHead(d.Ifindex, hook, st.objs.PlPodTag)
}

// reattaching reports a stage program another program went ahead of: design
// 6.4's DatapathDegraded, with the program attached again at once.
func (a *Agent) reattaching(name string, ifindex int) {
	chainReattached.Inc()
	logging.Info(a.logger, "op", "egressStage", "node", a.node, "link", name, "ifindex", ifindex,
		"msg", "another program went ahead of this one in its chain; attaching it again at the head")
	if a.events != nil {
		a.events.Eventf(&corev1.ObjectReference{Kind: "Node", Name: a.node, UID: types.UID(a.node)}, corev1.EventTypeWarning,
			"DatapathDegraded", "%s: another program went ahead of it in its chain; attached again at the head", name)
	}
}

// stageUplinks are the devices a pod's packet can leave by: the default-route
// and pool interfaces, and every other physical, VLAN or bond device that is
// up with a global address. A route out of a device the stage isn't on would
// leave untranslated.
func stageUplinks(h *netlink.Handle, pools map[int]dpPool) ([]int, error) {
	idxs, err := defaultUplinks(h, pools)
	if err != nil {
		return nil, err
	}
	set := map[int]bool{}
	for _, i := range idxs {
		set[i] = true
	}
	links, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		a := l.Attrs()
		switch l.Type() {
		case "device", "vlan", "bond":
		default:
			continue
		}
		if a.Flags&unix.IFF_UP == 0 || a.Flags&unix.IFF_LOOPBACK != 0 {
			continue
		}
		addrs, err := h.AddrList(l, netlink.FAMILY_ALL)
		if err != nil {
			return nil, err
		}
		for _, ad := range addrs {
			if ad.Scope == unix.RT_SCOPE_UNIVERSE {
				set[a.Index] = true
				break
			}
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// syncStageLinks attaches the stage's programs (design 6.4): pl_pod_out at the
// head of tcx egress of each uplink, moved back there if something went ahead
// of it, and pl_pod_inbound on the root cgroup. Datapath goroutine only, with
// the stage's maps already written.
func (a *Agent) syncStageLinks(st *dpState, w dpWant) error {
	idxs, err := stageUplinks(st.nl, w.pools)
	if err != nil {
		return fmt.Errorf("finding the stage's uplinks: %w", err)
	}
	type out struct {
		ifindex int
		prog    *ebpf.Program
	}
	wantOut := map[string]out{}
	for _, idx := range idxs {
		l, err := st.nl.LinkByIndex(idx)
		if err != nil || l.Attrs().Name == datapath.TunnelName {
			continue
		}
		l3, err := addrguard.LinkLayer(l)
		if err != nil {
			logging.Debug(a.logger, "op", "egressStage", "node", a.node, "interface", l.Attrs().Name, "error", err,
				"msg", "pl_pod_out can't parse this device")
			continue
		}
		prog := st.objs.PlPodOutEth
		if l3 {
			prog = st.objs.PlPodOutL3
		}
		wantOut[stageOutPrefix+l.Attrs().Name] = out{idx, prog}
	}
	var errs []error
	for name, l := range st.links {
		if len(name) < len(stageOutPrefix) || name[:len(stageOutPrefix)] != stageOutPrefix {
			continue
		}
		o, ok := wantOut[name]
		if ok && st.linkIfs[name] == o.ifindex {
			if datapath.AtHead(o.ifindex, ebpf.AttachTCXEgress, o.prog) {
				continue
			}
			a.reattaching(name, o.ifindex)
		}
		if err := detach(l); err != nil {
			errs = append(errs, fmt.Errorf("detaching %s: %w", name, err))
			continue
		}
		delete(st.links, name)
		delete(st.linkIfs, name)
	}
	for _, name := range slices.Sorted(maps.Keys(wantOut)) {
		if _, ok := st.links[name]; ok {
			continue
		}
		o := wantOut[name]
		l, err := datapath.AttachTCX(st.pinDir, name, o.ifindex, o.prog, ebpf.AttachTCXEgress, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("attaching %s: %w", name, err))
			continue
		}
		st.links[name], st.linkIfs[name] = l, o.ifindex
		logging.Info(a.logger, "op", "egressStage", "node", a.node, "link", name, "msg", "the eBPF stage's program is on this uplink")
	}
	if _, ok := st.links[datapath.InboundLink]; !ok {
		l, err := datapath.AttachInbound(st.pinDir, st.objs.PlPodInbound)
		if err != nil {
			errs = append(errs, err)
		} else {
			st.links[datapath.InboundLink], st.linkIfs[datapath.InboundLink] = l, 0
		}
	}
	if st.pinDir != "" {
		if _, err := bpfpin.Sweep(st.pinDir, stageOutPrefix, func(path string) bool {
			_, ok := st.links[filepath.Base(path)]
			return ok
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncNat is syncConntrack on the eBPF stage: flows idle past their timeout go;
// a retired slot's are deleted (the flows leave untranslated and are reset by
// the far end, design 6.4), so its id can be used again; a deleted pod's go;
// a Draining slot with none is reported drained (§5.6). Datapath goroutine
// only.
func (a *Agent) syncNat(st *dpState, w dpWant) {
	var errs []error
	now, err := datapath.MonotonicNow()
	if err == nil {
		var n int
		if n, err = st.objs.NatGC(now); err == nil && n > 0 {
			logging.Debug(a.logger, "op", "egressNat", "node", a.node, "flows", n, "msg", "expired idle flows")
		}
	}
	errs = append(errs, err)
	flows, err := st.objs.NatFlows()
	if err != nil {
		logging.Info(a.logger, "op", "egressNat", "node", a.node, "error", err, "msg", "reading the NAT table; retrying")
		return
	}

	bySlot := map[uint8][]datapath.NatFlow{}
	for _, f := range flows {
		bySlot[f.Slot] = append(bySlot[f.Slot], f)
	}
	flushed := map[uint8]bool{}
	for _, id := range w.retired {
		n, failed := 0, false
		for _, f := range bySlot[id] {
			if err := st.objs.NatDelete(f); err != nil {
				errs, failed = append(errs, err), true
				continue
			}
			n++
		}
		if n > 0 {
			ctDeleted.WithLabelValues("slotRetired").Add(float64(n))
			logging.Info(a.logger, "op", "egressNat", "node", a.node, "slot", id, "flows", n, "msg", "reset the flows left on a retired slot")
		}
		if !failed {
			flushed[id] = true
			delete(bySlot, id)
		}
	}

	live := map[netip.Addr]bool{}
	for _, p := range w.pods {
		for _, ip := range []netip.Addr{p.v4, p.v6} {
			if ip.IsValid() {
				live[ip] = true
			}
		}
	}
	for ip := range st.podIPs {
		if !live[ip] {
			st.gone[ip] = true
		}
	}
	maps.DeleteFunc(st.gone, func(ip netip.Addr, _ bool) bool { return live[ip] })
	st.podIPs = live
	if len(st.gone) > 0 {
		n, failed := 0, false
		for id, fs := range bySlot {
			kept := fs[:0]
			for _, f := range fs {
				if !st.gone[f.Pod] {
					kept = append(kept, f)
					continue
				}
				if err := st.objs.NatDelete(f); err != nil {
					errs, failed = append(errs, err), true
					kept = append(kept, f)
					continue
				}
				n++
			}
			bySlot[id] = kept
		}
		if n > 0 {
			ctDeleted.WithLabelValues("podDeleted").Add(float64(n))
			logging.Info(a.logger, "op", "egressNat", "node", a.node, "addresses", len(st.gone), "flows", n, "msg", "deleted the flows of pods gone from this node")
		}
		if !failed {
			clear(st.gone)
		}
	}

	drained := slices.Clone(w.drainedNoSlot)
	for _, id := range slices.Sorted(maps.Keys(w.draining)) {
		n := len(bySlot[id])
		logging.Debug(a.logger, "op", "egressNat", "node", a.node, "slot", id, "endpoints", w.draining[id].Endpoints,
			"index", w.draining[id].Index, "flows", n, "msg", "draining slot")
		if n == 0 {
			drained = append(drained, w.draining[id])
		}
	}
	slices.SortFunc(drained, func(x, y purelbv2.EgressDrained) int {
		return cmp.Or(cmp.Compare(x.Endpoints, y.Endpoints), cmp.Compare(x.Index, y.Index))
	})
	if old := a.drained.Load(); old == nil || !slices.Equal(*old, drained) {
		a.drained.Store(&drained)
		a.add(keyNode, false)
	}
	if old := a.ctFlushed.Load(); old == nil || !maps.Equal(*old, flushed) {
		a.ctFlushed.Store(&flushed)
		if len(flushed) > 0 {
			a.add(keyPods, false)
		}
	}
	total := 0
	for _, fs := range bySlot {
		total += len(fs)
	}
	natFlows.Set(float64(total))
	if err := errors.Join(errs...); err != nil {
		logging.Info(a.logger, "op", "egressNat", "node", a.node, "error", err, "msg", "syncing the NAT table with the slots; retrying")
	}
}

// syncStageSeen is the eBPF stage's check that it sees what it should (user
// decision 2026-11-03): in each family, selected pods' SYNs leaving their
// devices with none reaching pl_pod_out over forwardWindows windows mean their
// traffic isn't being translated. It also moves the insert-failed metric.
// Datapath goroutine only.
func (a *Agent) syncStageSeen(st *dpState) {
	c, err := st.objs.Counts()
	if err != nil {
		logging.Info(a.logger, "op", "egressStage", "node", a.node, "error", err, "msg", "reading the stage's counters; retrying")
		return
	}
	now := a.now()
	var blocked []string
	for i, fam := range []string{"IPv4", "IPv6"} {
		f := datapath.SlotFlows{Marked: c["pod_syn"][i], Translated: c["pod_out_syn"][i]}
		if stepForward(&st.fwd[i], st.table, now, f) {
			logging.Debug(a.logger, "op", "egressStage", "node", a.node, "family", fam, "blocked", st.fwd[i].blocked, "msg", "stage window")
		}
		if st.fwd[i].blocked >= forwardWindows {
			blocked = append(blocked, fam)
		}
		if failed := c["nat_insert_failed"][i]; failed > st.insertFailed[i] {
			natInsertFailed.WithLabelValues(fam).Add(float64(failed - st.insertFailed[i]))
			st.insertFailed[i] = failed
		}
	}
	a.setForwardBlocked(st, blocked)
}

// readPortlessEBPF is readPortless on the eBPF stage, from pod_portless. The
// map outlives an agent, so the first read of a session only takes the counts.
func (a *Agent) readPortlessEBPF(st *dpState) error {
	counts, err := st.objs.PortlessCounts()
	if err != nil {
		return fmt.Errorf("reading the port-less counters: %w", err)
	}
	if st.portless == nil {
		st.portless = map[netip.Addr]portlessSeen{}
	}
	drops := map[netip.Addr]uint64{}
	for id, n := range counts {
		if s, ok := st.ebpfStage.Slots[id]; ok {
			drops[s.Addr] += n
		}
	}
	now := a.now()
	for addr, n := range drops {
		seen := st.portless[addr]
		if n > seen.count && st.portlessRead {
			if seen.rose.IsZero() || now.Sub(seen.rose) >= portlessWindow {
				logging.Info(a.logger, "op", "egressPods", "node", a.node, "address", addr, "packets", n-seen.count,
					"msg", "pods sent SCTP or a protocol without ports to a slice address, which can't carry it (D4)")
			}
			seen.rose = now
		}
		seen.count = n
		st.portless[addr] = seen
	}
	st.portlessRead = true
	return nil
}

// pinnedSlots reads the slots an earlier agent left in the pinned maps: the
// eBPF stage's flows carry those ids.
func (a *Agent) pinnedSlots() (map[uint8]datapath.SlotNAT, error) {
	return a.pinnedSlotsIn(a.dp.pinRoot)
}

func (a *Agent) pinnedSlotsIn(dir string) (map[uint8]datapath.SlotNAT, error) {
	if !bpfpin.Usable(dir) {
		return map[uint8]datapath.SlotNAT{}, nil
	}
	return datapath.PinnedPodSlots(dir)
}
