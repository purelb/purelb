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

package datapath

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/google/nftables"
	"github.com/vishvananda/netlink"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/diagreport"
)

// DeleteHardening removes egress's table, if there is one (D26): the
// hardening chains and the pod-node stage.
func DeleteHardening(c *nftables.Conn) error {
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable}
	// Adding first makes the delete valid when the table doesn't exist.
	c.AddTable(t)
	c.DelTable(t)
	return c.Flush()
}

// DeleteTunnel removes the tunnel device, if there is one; the links
// attached to it go with it.
func DeleteTunnel(h *netlink.Handle) error {
	l, err := h.LinkByName(TunnelName)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return h.LinkDel(l)
}

// TunnelExists reports whether the tunnel device exists, for finding an
// earlier agent's datapath while egress is off.
func TunnelExists(h *netlink.Handle) bool {
	_, err := h.LinkByName(TunnelName)
	return err == nil
}

// pinPrefixes are the names of egress's pinned links.
var pinPrefixes = []string{"pl_announce-", "pl_steer-", "pl_pod_tag-", "pl_pod_out-", InboundLink}

// RemovePins removes what egress pinned in pinDir (D12): its links, which
// detaches them, and its maps. Other pins there (the guard's) are left.
func RemovePins(pinDir string) error {
	if !bpfpin.Usable(pinDir) {
		return nil
	}
	var errs []error
	for _, prefix := range pinPrefixes {
		if _, err := bpfpin.Sweep(pinDir, prefix, func(string) bool { return false }); err != nil {
			errs = append(errs, err)
		}
	}
	spec, err := loadEgress()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for name := range spec.Maps {
		if err := os.Remove(filepath.Join(pinDir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// PinnedCount counts what egress pinned in pinDir; other pins there (the
// guard's) aren't counted. An empty directory costs one read, so it is cheap
// on a node with egress off.
func PinnedCount(pinDir string) int {
	if !bpfpin.Usable(pinDir) {
		return 0
	}
	entries, err := os.ReadDir(pinDir)
	if err != nil || len(entries) == 0 {
		return 0
	}
	spec, err := loadEgress()
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if isEgressPin(spec, e.Name()) {
			n++
		}
	}
	return n
}

// OpenPinned opens egress's maps pinned in pinDir read-only, for reading
// them from outside the agent. No programs are loaded; only the maps
// `lbnodeagent diag` reads are set.
func OpenPinned(pinDir string) (*Objects, error) {
	o := &Objects{}
	for name, m := range map[string]**ebpf.Map{
		"addr_cfg": &o.AddrCfg, "slices": &o.Slices, "reserved": &o.Reserved, "svc_ports": &o.SvcPorts, "nodes": &o.Nodes,
		"egress_reasons": &o.EgressReasons, "egress_traffic": &o.EgressTraffic, "pod_devs": &o.PodDevs,
		"pod_slots": &o.PodSlots, "nat": &o.Nat,
	} {
		p, err := ebpf.LoadPinnedMap(filepath.Join(pinDir, name), &ebpf.LoadPinOptions{ReadOnly: true})
		if err != nil {
			_ = o.Close()
			return nil, fmt.Errorf("opening %s: %w", name, err)
		}
		*m = p
	}
	return o, nil
}

// ChainOrders reads where the address guard and pl_announce run on each
// interface pl_announce is pinned on (D12's chain invariant).
func ChainOrders(pinDir string) []diagreport.Chain {
	entries, err := os.ReadDir(pinDir)
	if err != nil {
		return nil
	}
	var out []diagreport.Chain
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), "pl_announce-")
		if !ok || name == TunnelName {
			continue
		}
		iface, err := net.InterfaceByName(name)
		if err != nil {
			continue
		}
		res, err := link.QueryPrograms(link.QueryOptions{Target: iface.Index, Attach: ebpf.AttachTCXIngress})
		if err != nil {
			continue
		}
		c := diagreport.Chain{Interface: name, Announce: pinnedPosition(res, filepath.Join(pinDir, e.Name()))}
		for _, g := range entries {
			if strings.HasPrefix(g.Name(), "addrguard-") && strings.HasSuffix(g.Name(), "-"+name) {
				c.Guard = pinnedPosition(res, filepath.Join(pinDir, g.Name()))
			}
		}
		out = append(out, c)
	}
	return out
}

// pinnedPosition is where the program of the link pinned at path is in res,
// from 1; 0 when it can't be told.
func pinnedPosition(res *link.QueryResult, path string) int {
	l, err := link.LoadPinnedLink(path, &ebpf.LoadPinOptions{ReadOnly: true})
	if err != nil {
		return 0
	}
	defer func() { _ = l.Close() }()
	info, err := l.Info()
	if err != nil {
		return 0
	}
	for i, p := range res.Programs {
		if p.ID == info.Program {
			return i + 1
		}
	}
	return 0
}

// Pinned reports whether pinDir holds anything egress pinned.
func Pinned(pinDir string) bool {
	return PinnedCount(pinDir) > 0
}

func isEgressPin(spec *ebpf.CollectionSpec, name string) bool {
	if _, isMap := spec.Maps[name]; isMap {
		return true
	}
	for _, prefix := range pinPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
