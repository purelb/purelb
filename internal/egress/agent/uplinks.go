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
	"path/filepath"
	"slices"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/addrguard"
	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
)

// uplinkPrefix names pl_announce's links, pinned as pl_announce-<interface>.
const uplinkPrefix = "pl_announce-"

// syncUplinks attaches pl_announce at tcx ingress of the uplinks while
// this node holds an egress address, and only then (design 6.5): the
// interfaces the address guard is attached to, right behind it ([guard,
// pl_announce]); without the guard, the default-route interfaces of each
// family and the held local addresses' pool interfaces (user decision
// 2026-10-25). Links to interfaces no longer wanted are detached, an
// earlier agent's pins included. An uplink of a type pl_announce can't
// parse is skipped and logged when that changes. Datapath goroutine only.
func (a *Agent) syncUplinks(st *dpState, w dpWant) error {
	want := map[string]netlink.Link{}
	if holdsAny(st.addrs) { // as installed: kept entries count (D12)
		idxs := w.guardLinks
		if len(idxs) == 0 {
			var err error
			if idxs, err = defaultUplinks(st.nl, w.pools); err != nil {
				return fmt.Errorf("finding the uplinks: %w", err)
			}
		}
		for _, idx := range idxs {
			l, err := st.nl.LinkByIndex(idx)
			if err != nil || l.Attrs().Name == datapath.TunnelName {
				continue // gone meanwhile: the next apply follows
			}
			want[uplinkPrefix+l.Attrs().Name] = l
		}
	}
	var errs []error
	unparsed := map[string]bool{}
	defer func() { st.unparsed = unparsed }()
	for name, l := range st.links {
		if !uplink(name) {
			continue
		}
		if u, ok := want[name]; ok && st.linkIfs[name] == u.Attrs().Index {
			continue
		}
		if err := detach(l); err != nil {
			errs = append(errs, fmt.Errorf("detaching %s: %w", name, err))
			continue
		}
		delete(st.links, name)
		delete(st.linkIfs, name)
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if _, ok := st.links[name]; ok {
			continue
		}
		l := want[name]
		l3, err := addrguard.LinkLayer(l)
		if err != nil {
			if !st.unparsed[name] {
				logging.Info(a.logger, "op", "egressDatapath", "node", a.node, "interface", l.Attrs().Name, "error", err,
					"msg", "not attaching pl_announce: egress replies arriving on this uplink are not handled")
			}
			unparsed[name] = true
			continue
		}
		prog := st.objs.PlAnnounceEth
		if l3 {
			prog = st.objs.PlAnnounceL3
		}
		ln, err := datapath.AttachTCX(st.pinDir, name, l.Attrs().Index, prog, ebpf.AttachTCXIngress, w.guardProgs)
		if err != nil {
			errs = append(errs, fmt.Errorf("attaching %s: %w", name, err))
			continue
		}
		st.links[name], st.linkIfs[name] = ln, l.Attrs().Index
	}
	if st.pinDir != "" {
		if _, err := bpfpin.Sweep(st.pinDir, uplinkPrefix, func(path string) bool {
			_, ok := st.links[filepath.Base(path)]
			return ok
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// uplink reports whether link name is one of pl_announce's uplinks, not
// its tunnel link.
func uplink(name string) bool {
	return strings.HasPrefix(name, uplinkPrefix) && name != uplinkPrefix+datapath.TunnelName
}

// holdsAny reports whether this node holds any egress address.
func holdsAny(addrs map[netip.Addr]*datapath.AddressEntries) bool {
	for _, e := range addrs {
		if e.Held {
			return true
		}
	}
	return false
}

// defaultUplinks is the main table's default-route interfaces, both
// families, and the pool interfaces.
func defaultUplinks(h *netlink.Handle, pools map[int]dpPool) ([]int, error) {
	set := map[int]bool{}
	for idx := range pools {
		set[idx] = true
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := h.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return nil, err
		}
		for _, r := range routes {
			if !defaultRoute(r) {
				continue
			}
			if r.LinkIndex != 0 {
				set[r.LinkIndex] = true
			}
			for _, nh := range r.MultiPath {
				set[nh.LinkIndex] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// detach removes a link and its pin.
func detach(l link.Link) error {
	if err := l.Unpin(); err != nil {
		return err
	}
	if err := l.Detach(); err != nil && !errors.Is(err, ebpf.ErrNotSupported) {
		return err
	}
	return l.Close()
}
