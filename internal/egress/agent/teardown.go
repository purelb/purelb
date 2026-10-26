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

// endDatapath tears the datapath down on the datapath goroutine, then
// clears this node's status, which tells the allocator and the Helm
// pre-delete hook that nothing egress is left (D26). A failure is retried.
func (a *Agent) endDatapath() {
	a.runner.submit(func() {
		if err := a.tear(); err != nil {
			logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "error", err,
				"msg", "removing the egress datapath; retrying")
			time.AfterFunc(datapathRetry, func() { a.add(keyTeardown, false) })
			return
		}
		a.capability.Store(nil)
		a.status.disable()
	})
}

// syncTeardown retries a failed teardown unless egress is back on.
func (a *Agent) syncTeardown() {
	if a.on == nil {
		a.endDatapath()
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
	errs = append(errs, datapath.SyncThrows(st.nl, nil))
	if _, err := datapath.SyncPoolTables(st.nl, nil); err != nil {
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
	a.pools.Store(nil)
	logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "msg", "egress datapath removed")
	return nil
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
