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
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// PodDev is what pl_pod_tag knows of one pod's host-side device.
type PodDev struct {
	// V4 and V6 are the pod's addresses, the only sources it may use
	// (D20); invalid when it has none in that family.
	V4, V6 netip.Addr
	// PodSet is its pod-set id, 0 when no policy selects it.
	PodSet uint8
	// Hold drops its traffic leaving the cluster (FailClosed).
	Hold bool
}

// SetPodDev programs the pod device ifindex.
func (o *Objects) SetPodDev(ifindex int, d PodDev) error {
	v := egressPodDev{PodSet: d.PodSet, Hold: b2u(d.Hold)}
	if d.V4.IsValid() {
		w, _ := words(d.V4)
		v.V4, v.HasV4 = w[0], 1
	}
	if d.V6.IsValid() {
		v.V6, _ = words(d.V6)
		v.HasV6 = 1
	}
	return o.PodDevs.Put(uint32(ifindex), &v)
}

// DeletePodDev removes the pod device ifindex.
func (o *Objects) DeletePodDev(ifindex int) error {
	return ignoreNotExist(o.PodDevs.Delete(uint32(ifindex)))
}

// RecoverPodDevs reads the pod devices back from the maps (D12).
func (o *Objects) RecoverPodDevs() (map[int]PodDev, error) {
	out := map[int]PodDev{}
	var k uint32
	var v egressPodDev
	it := o.PodDevs.Iterate()
	for it.Next(&k, &v) {
		d := PodDev{PodSet: v.PodSet, Hold: v.Hold != 0}
		if v.HasV4 != 0 {
			d.V4 = unwords([4]uint32{v.V4}, false)
		}
		if v.HasV6 != 0 {
			d.V6 = unwords(v.V6, true)
		}
		out[int(k)] = d
	}
	return out, it.Err()
}

// SyncCluster makes the cluster-network maps hold exactly prefixes.
func (o *Objects) SyncCluster(prefixes []netip.Prefix) error {
	want4, want6 := map[egressLpm4]bool{}, map[egressLpm6]bool{}
	for _, p := range prefixes {
		p = p.Masked()
		w, _ := words(p.Addr())
		if p.Addr().Is4() {
			want4[egressLpm4{Prefixlen: uint32(p.Bits()), Addr: w[0]}] = true
		} else {
			want6[egressLpm6{Prefixlen: uint32(p.Bits()), Addr: w}] = true
		}
	}
	var errs []error
	one := uint8(1)
	var k4 egressLpm4
	var v uint8
	var stale4 []egressLpm4
	it := o.Cluster4.Iterate()
	for it.Next(&k4, &v) {
		if !want4[k4] {
			stale4 = append(stale4, k4)
		}
		delete(want4, k4)
	}
	errs = append(errs, it.Err())
	for _, k := range stale4 {
		errs = append(errs, ignoreNotExist(o.Cluster4.Delete(&k)))
	}
	for k := range want4 {
		errs = append(errs, o.Cluster4.Put(&k, &one))
	}
	var k6 egressLpm6
	var stale6 []egressLpm6
	it = o.Cluster6.Iterate()
	for it.Next(&k6, &v) {
		if !want6[k6] {
			stale6 = append(stale6, k6)
		}
		delete(want6, k6)
	}
	errs = append(errs, it.Err())
	for _, k := range stale6 {
		errs = append(errs, ignoreNotExist(o.Cluster6.Delete(&k)))
	}
	for k := range want6 {
		errs = append(errs, o.Cluster6.Put(&k, &one))
	}
	return errors.Join(errs...)
}

// PodDevice is a pod network namespace's host-side device.
type PodDevice struct {
	Ifindex int
	Name    string
	// Kind is the link type: "veth" or "netkit".
	Kind string
}

// PodDevices maps every address in a pod network namespace to that
// namespace's host-side device. Each veth or netkit in this namespace names
// its peer's namespace by id (link-netnsid); one address dump per such
// namespace, by id (IFA_TARGET_NETNSID), lists the peer's addresses
// without entering it, so no capability beyond CAP_NET_ADMIN is needed, no
// traffic is sent, and bridge and routed CNIs are found the same way.
func PodDevices(h *netlink.Handle) (map[netip.Addr]PodDevice, error) {
	links, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	out := map[netip.Addr]PodDevice{}
	for _, l := range links {
		a := l.Attrs()
		if a.NetNsID < 0 || (l.Type() != "veth" && l.Type() != "netkit") {
			continue
		}
		addrs, err := addrsInNetns(a.NetNsID)
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EINVAL) {
			continue // the namespace went
		}
		if err != nil {
			return nil, fmt.Errorf("addresses of %s's peer: %w", a.Name, err)
		}
		for _, ip := range addrs[a.ParentIndex] {
			if !ip.IsLinkLocalUnicast() {
				out[ip] = PodDevice{Ifindex: a.Index, Name: a.Name, Kind: l.Type()}
			}
		}
	}
	return out, nil
}

// addrsInNetns dumps the addresses of the network namespace with id nsid,
// by interface index.
func addrsInNetns(nsid int) (map[int][]netip.Addr, error) {
	s, err := nl.Subscribe(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	// The kernel reads IFA_TARGET_NETNSID from a dump request only under
	// strict checking.
	if err := unix.SetsockoptInt(s.GetFd(), unix.SOL_NETLINK, unix.NETLINK_GET_STRICT_CHK, 1); err != nil {
		return nil, err
	}
	req := nl.NewNetlinkRequest(unix.RTM_GETADDR, unix.NLM_F_DUMP)
	req.AddData(nl.NewIfAddrmsg(unix.AF_UNSPEC))
	id := make([]byte, 4)
	binary.NativeEndian.PutUint32(id, uint32(nsid))
	req.AddData(nl.NewRtAttr(unix.IFA_TARGET_NETNSID, id))
	if err := s.Send(req); err != nil {
		return nil, err
	}
	out := map[int][]netip.Addr{}
	for {
		msgs, _, err := s.Receive()
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			switch m.Header.Type {
			case unix.NLMSG_DONE:
				return out, nil
			case unix.NLMSG_ERROR:
				if errno := -int32(binary.NativeEndian.Uint32(m.Data[:4])); errno != 0 {
					return nil, syscall.Errno(errno)
				}
				return out, nil
			case unix.RTM_NEWADDR:
			default:
				continue
			}
			ifa := nl.DeserializeIfAddrmsg(m.Data)
			attrs, err := nl.ParseRouteAttr(m.Data[unix.SizeofIfAddrmsg:])
			if err != nil {
				return nil, err
			}
			for _, at := range attrs {
				if at.Attr.Type != unix.IFA_ADDRESS {
					continue
				}
				if ip, ok := netip.AddrFromSlice(at.Value); ok {
					out[int(ifa.Index)] = append(out[int(ifa.Index)], ip.Unmap())
				}
			}
		}
	}
}

// PinnedPodDevs reads the pod devices from the map pinned in pinDir, as an
// earlier agent left it; none when there is no such map.
func PinnedPodDevs(pinDir string) (map[int]PodDev, error) {
	m, err := ebpf.LoadPinnedMap(filepath.Join(pinDir, "pod_devs"), &ebpf.LoadPinOptions{ReadOnly: true})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer m.Close()
	o := &Objects{}
	o.PodDevs = m
	return o.RecoverPodDevs()
}

// ClusterNetworks reads the cluster-network maps back, sorted.
func (o *Objects) ClusterNetworks() ([]netip.Prefix, error) {
	var out []netip.Prefix
	var k4 egressLpm4
	var k6 egressLpm6
	var v uint8
	it := o.Cluster4.Iterate()
	for it.Next(&k4, &v) {
		out = append(out, netip.PrefixFrom(unwords([4]uint32{k4.Addr}, false), int(k4.Prefixlen)))
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	it = o.Cluster6.Iterate()
	for it.Next(&k6, &v) {
		out = append(out, netip.PrefixFrom(unwords(k6.Addr, true), int(k6.Prefixlen)))
	}
	slices.SortFunc(out, func(x, y netip.Prefix) int {
		if c := x.Addr().Compare(y.Addr()); c != 0 {
			return c
		}
		return x.Bits() - y.Bits()
	})
	return out, it.Err()
}
