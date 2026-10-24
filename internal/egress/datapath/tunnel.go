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
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/bpfpin"
)

// TunnelName is egress's tunnel device (D3).
const TunnelName = "purelb-eg0"

// Tunnel encapsulations (LBNodeAgent spec.egress.tunnelEncapsulation).
const (
	ModeAuto = "Auto"
	ModeL3   = "L3"
	ModeL2   = "L2"
)

// The pod-node target tables (design 6.11): the tunnel and prohibit
// targets. Each carries a throw route for every node's underlay address, a
// backstop that keeps a tunnel's outer packets out of the slot tables
// should the mark ever survive steering.
const (
	TunnelTable   = 7700
	ProhibitTable = 7799
)

// Tunnel overhead: Geneve 8, UDP 8 and the outer IP header, plus 14 for
// L2's inner Ethernet header (design 6.13).
func overhead(mode string, outer6 bool) int {
	n := 8 + 8 + 20
	if outer6 {
		n = 8 + 8 + 40
	}
	if mode == ModeL2 {
		n += 14
	}
	return n
}

// TunnelMAC is a node's fixed tunnel MAC in L2 mode: locally administered,
// from its NodeID, so every node knows every other's without resolution
// (purelb-eg0 is NOARP).
func TunnelMAC(id uint64) net.HardwareAddr {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return net.HardwareAddr{0x02, b[3], b[4], b[5], b[6], b[7]}
}

// EnsureTunnel makes purelb-eg0 a collect-metadata Geneve device on port:
// L3 (innerprotoinherit) for L3, and for Auto where the kernel supports
// it; otherwise L2, with mac and no ARP (an ARP frame carries no tunnel
// key). An existing device that already matches is kept, so a restart
// leaves its programs attached; one that doesn't is replaced. It returns
// the mode it resolved and the device, up.
func EnsureTunnel(h *netlink.Handle, want string, port uint16, mac net.HardwareAddr) (string, netlink.Link, error) {
	modes := []string{ModeL3, ModeL2}
	switch want {
	case ModeL3, ModeL2:
		modes = []string{want}
	case ModeAuto, "":
	default:
		return "", nil, fmt.Errorf("unknown tunnel encapsulation %q", want)
	}
	if cur, err := h.LinkByName(TunnelName); err == nil {
		g, ok := cur.(*netlink.Geneve)
		if ok && g.FlowBased && g.Dport == port {
			mode := ModeL2
			if g.InnerProtoInherit {
				mode = ModeL3
			}
			if mode == modes[0] || (len(modes) == 2 && mode == ModeL2 && !l3Supported(h, port)) {
				if mode == ModeL2 && cur.Attrs().HardwareAddr.String() != mac.String() {
					if err := h.LinkSetHardwareAddr(cur, mac); err != nil {
						return "", nil, err
					}
				}
				return mode, cur, h.LinkSetUp(cur)
			}
		}
		if err := h.LinkDel(cur); err != nil {
			return "", nil, fmt.Errorf("replacing %s: %w", TunnelName, err)
		}
	}
	var errs []error
	for _, mode := range modes {
		l, err := createTunnel(h, mode, port, mac)
		if err == nil {
			return mode, l, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", mode, err))
	}
	return "", nil, fmt.Errorf("creating %s: %w", TunnelName, errors.Join(errs...))
}

func createTunnel(h *netlink.Handle, mode string, port uint16, mac net.HardwareAddr) (netlink.Link, error) {
	g := &netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: TunnelName}, FlowBased: true, Dport: port,
		InnerProtoInherit: mode == ModeL3}
	if mode == ModeL2 {
		g.HardwareAddr = mac
	}
	if err := h.LinkAdd(g); err != nil {
		return nil, err
	}
	l, err := h.LinkByName(TunnelName)
	if err != nil {
		return nil, err
	}
	if mode == ModeL3 && !l.(*netlink.Geneve).InnerProtoInherit {
		// An old kernel ignored the attribute: not L3 after all.
		_ = h.LinkDel(l)
		return nil, errors.New("the kernel ignored innerprotoinherit")
	}
	if mode == ModeL2 {
		if err := h.LinkSetARPOff(l); err != nil {
			return nil, err
		}
	}
	if err := h.LinkSetUp(l); err != nil {
		return nil, err
	}
	return h.LinkByName(TunnelName)
}

// l3Supported reports whether this kernel can make an L3 Geneve device,
// by making a scratch one.
func l3Supported(h *netlink.Handle, port uint16) bool {
	const probe = "purelb-egprobe"
	g := &netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: probe}, FlowBased: true, Dport: port + 1, InnerProtoInherit: true}
	if err := h.LinkAdd(g); err != nil {
		return false
	}
	defer func() { _ = h.LinkDel(g) }()
	l, err := h.LinkByName(probe)
	return err == nil && l.(*netlink.Geneve).InnerProtoInherit
}

// SyncThrows makes the throw routes in TunnelTable and ProhibitTable
// exactly one per underlay address, both families.
func SyncThrows(h *netlink.Handle, underlay []netip.Addr) error {
	want := map[string]bool{}
	for _, a := range underlay {
		want[netip.PrefixFrom(a, a.BitLen()).String()] = true
	}
	var errs []error
	for _, table := range []int{TunnelTable, ProhibitTable} {
		have := map[string]bool{}
		for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			routes, err := h.RouteListFiltered(fam, &netlink.Route{Table: table, Type: unix.RTN_THROW},
				netlink.RT_FILTER_TABLE|netlink.RT_FILTER_TYPE)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, r := range routes {
				if r.Dst == nil {
					continue
				}
				p := r.Dst.String()
				if !want[p] {
					if err := h.RouteDel(&r); err != nil {
						errs = append(errs, err)
					}
					continue
				}
				have[p] = true
			}
		}
		for p := range want {
			if have[p] {
				continue
			}
			_, dst, _ := net.ParseCIDR(p)
			r := &netlink.Route{Dst: dst, Table: table, Type: unix.RTN_THROW}
			if err := h.RouteAdd(r); err != nil && !errors.Is(err, unix.EEXIST) {
				errs = append(errs, fmt.Errorf("throw %s table %d: %w", p, table, err))
			}
		}
	}
	return errors.Join(errs...)
}

// TunnelMTU is the largest inner packet the tunnel carries to every peer
// over an IPv6 underlay (D13: from each peer's route, D30), which is what
// pl_announce answers oversize replies against; 0 with no IPv6 peer (an
// IPv4 underlay fragments instead).
func TunnelMTU(h *netlink.Handle, mode string, peers6 []netip.Addr) int {
	mtu := 0
	for _, p := range peers6 {
		routes, err := h.RouteGet(p.AsSlice())
		if err != nil || len(routes) == 0 {
			continue
		}
		m := routes[0].MTU
		if m == 0 {
			if l, err := h.LinkByIndex(routes[0].LinkIndex); err == nil {
				m = l.Attrs().MTU
			}
		}
		if m == 0 {
			continue
		}
		if m -= overhead(mode, true); mtu == 0 || m < mtu {
			mtu = m
		}
	}
	return mtu
}

// AttachTCX attaches prog to ifindex at hook, right after program after
// when it is in that chain (the guard: [guard, pl_announce]) and at the
// head otherwise. With pinDir set (D12) the link is pinned there as name,
// and a link an earlier agent pinned as name is taken over in place
// instead. Programs are matched by ID: finding one by name would need
// CAP_SYS_ADMIN.
func AttachTCX(pinDir, name string, ifindex int, prog *ebpf.Program, hook ebpf.AttachType, after ebpf.ProgramID) (link.Link, error) {
	path := ""
	if pinDir != "" {
		path = filepath.Join(pinDir, name)
		if l, err := bpfpin.Adopt(path, ifindex, prog); err != nil || l != nil {
			return l, err
		}
	}
	anchor := link.Head()
	if after != 0 && inChain(ifindex, hook, after) {
		anchor = link.AfterProgramByID(after)
	}
	l, err := link.AttachTCX(link.TCXOptions{Program: prog, Interface: ifindex, Attach: hook, Anchor: anchor})
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := l.Pin(path); err != nil {
			_ = l.Close()
			return nil, err
		}
	}
	return l, nil
}

// inChain reports whether program id is attached at ifindex's hook.
func inChain(ifindex int, hook ebpf.AttachType, id ebpf.ProgramID) bool {
	res, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: hook})
	if err != nil {
		return false
	}
	for _, p := range res.Programs {
		if p.ID == id {
			return true
		}
	}
	return false
}
