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
	"slices"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
)

// keyTeardown retries a teardown that failed, while egress stays off.
const keyTeardown = "agent:teardown"

// detachWait bounds the wait for links to leave their chains: on 6.6 a
// link detaches asynchronously after its last reference goes (design 6.11).
const detachWait = 5 * time.Second

// endDatapath, on the datapath goroutine, takes the plugin out of the CNI
// chain, tears the datapath down, removes the plugin's files once nothing
// can run it, then clears this node's status, which tells the allocator
// and the Helm pre-delete hook that nothing egress is left (D26). A
// failure is retried. snapDone closes once the session's snapshot writer
// has stopped (nil: none).
func (a *Agent) endDatapath(snapDone <-chan struct{}) {
	a.runner.submit(func() {
		unchained := a.unchain(snapDone)
		if err := a.tear(); err != nil {
			logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "error", err,
				"msg", "removing the egress datapath; retrying")
			time.AfterFunc(datapathRetry, func() { a.add(keyTeardown, false) })
			return
		}
		if unchained {
			a.removePluginFiles()
		}
		a.capability.Store(nil)
		a.status.disable()
	})
}

// syncTeardown retries a failed teardown unless egress is back on.
func (a *Agent) syncTeardown() {
	if a.on == nil {
		a.endDatapath(nil)
	}
}

// teardown removes this node's egress datapath in D26 order: links and
// pins, the hardening table, the routing tables, the tunnel device. It
// also removes what an earlier agent left (egress disabled while no agent
// ran) and does nothing on a node that never had egress. Datapath
// goroutine only.
func (a *Agent) teardown() error {
	st := a.dp
	if st.objs == nil && !datapath.Pinned(st.pinRoot) {
		h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return fmt.Errorf("opening a netlink socket: %w", err)
		}
		defer h.Close()
		if !datapath.TunnelExists(h) {
			return nil // never had egress (D-idle): nothing to touch
		}
	}
	logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "msg", "removing the egress datapath")
	if st.nl == nil {
		h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return fmt.Errorf("opening a netlink socket: %w", err)
		}
		st.nl = h
	}
	var errs []error
	// src_valid_mark off on the pool interfaces, before the pins that
	// record them go (an earlier agent's are in its pins); the tunnel's
	// goes with the device.
	pools := st.pools
	if st.objs == nil {
		p, err := datapath.PinnedPoolInterfaces(st.pinRoot)
		errs = append(errs, err)
		pools = p
	}
	for ifindex := range pools {
		errs = append(errs, a.resetSrcValidMark(st.nl, ifindex))
	}
	// The mark field's byte, read before the pins go: egress's flows are
	// deleted from conntrack once nothing marks them (user, 2026-10-31).
	shift, shiftKnown := uint8(0), false
	if st.objs != nil {
		var err error
		shift, err = st.objs.MarkShift()
		errs, shiftKnown = append(errs, err), err == nil
	} else {
		var err error
		shift, shiftKnown, err = datapath.PinnedMarkShift(st.pinRoot)
		errs = append(errs, err)
	}
	ours := progIDs(st.objs)
	ifs := map[int]bool{}
	for name, l := range st.links {
		ifs[st.linkIfs[name]] = true
		if err := detach(l); err != nil {
			errs = append(errs, fmt.Errorf("detaching %s: %w", name, err))
			continue
		}
		delete(st.links, name)
		delete(st.linkIfs, name)
	}
	// An earlier agent's links, and the maps.
	errs = append(errs, datapath.RemovePins(st.pinRoot))
	waitDetached(ifs, ours)

	if st.nft == nil {
		c, err := nftables.New()
		if err != nil {
			errs = append(errs, err)
		} else {
			st.nft = c
		}
	}
	if st.nft != nil {
		errs = append(errs, datapath.DeleteHardening(st.nft))
	}
	if shiftKnown {
		errs = append(errs, a.flushSlots(shift))
	} else {
		logging.Info(a.logger, "op", "egressTeardown", "node", a.node,
			"msg", "no record of the mark field (nothing pinned): egress's conntrack entries are left to expire")
	}
	errs = append(errs, datapath.SyncRules(st.nl, 0, nil))
	errs = append(errs, datapath.SyncTargets(st.nl, datapath.Targets{}))
	if _, err := datapath.SyncPoolTables(st.nl, nil, nil); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, datapath.DeleteTunnel(st.nl))
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if st.objs != nil {
		_ = st.objs.Close()
	}
	st.nl.Close()
	*st = dpState{pinRoot: st.pinRoot}
	markConflict.Set(0)
	a.setForwardBlocked(st, nil)
	a.pools.Store(nil)
	logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "msg", "egress datapath removed")
	return nil
}

// flushSlots deletes, at teardown, the conntrack entries of every slot
// value (1-127) in the mark field at shift: egress's flows, which would
// otherwise keep their SNAT. Entries classified not egress, and other
// bytes' marks, stay.
func (a *Agent) flushSlots(shift uint8) error {
	var errs []error
	n := 0
	for v := uint8(1); v <= slotBudget; v++ {
		flows, err := datapath.CtFlows(shift, v)
		if err == nil && len(flows) > 0 {
			err = datapath.CtFlush(shift, v)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n += len(flows)
	}
	if n > 0 {
		ctDeleted.WithLabelValues("teardown").Add(float64(n))
		logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "flows", n,
			"msg", "reset egress's flows: deleted their conntrack entries")
	}
	return errors.Join(errs...)
}

// progIDs are the ids of egress's programs, none before they load.
func progIDs(o *datapath.Objects) []ebpf.ProgramID {
	if o == nil {
		return nil
	}
	var ids []ebpf.ProgramID
	for _, p := range []*ebpf.Program{o.PlAnnounceEth, o.PlAnnounceL3, o.PlAnnounceTun, o.PlSteer} {
		if info, err := p.Info(); err == nil {
			if id, ok := info.ID(); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// waitDetached waits, up to detachWait, until none of programs ours is in
// the tcx chains of ifs.
func waitDetached(ifs map[int]bool, ours []ebpf.ProgramID) {
	if len(ours) == 0 {
		return
	}
	deadline := time.Now().Add(detachWait)
	for time.Now().Before(deadline) {
		attached := false
		for ifindex := range ifs {
			for _, hook := range []ebpf.AttachType{ebpf.AttachTCXIngress, ebpf.AttachTCXEgress} {
				res, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: hook})
				if err != nil {
					continue // the interface is gone: nothing attached
				}
				for _, p := range res.Programs {
					if slices.Contains(ours, p.ID) {
						attached = true
					}
				}
			}
		}
		if !attached {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
