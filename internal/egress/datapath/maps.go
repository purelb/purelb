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
	"math/bits"
	"net"
	"net/netip"

	"github.com/cilium/ebpf"
)

// Config is the node-wide part of the programs' configuration.
type Config struct {
	Self       uint64 // this node's NodeID
	TunIfindex int    // purelb-eg0
	TunMTU     uint32 // largest inner packet the tunnel carries; 0: no PTB
	TunMAC     net.HardwareAddr
	L2         bool  // L2 Geneve
	MarkShift  uint8 // 0, 8, 16 or 24: the byte of the mark (D18)
	Underlay4  bool  // this node has an IPv4 underlay
	Underlay6  bool  // and an IPv6 one
}

// SetConfig writes the node-wide configuration.
func (o *Objects) SetConfig(c Config) error {
	v := egressEgressConfig{Self: c.Self, TunIfindex: uint32(c.TunIfindex), TunMtu: c.TunMTU,
		MarkShift: c.MarkShift, HasV4: b2u(c.Underlay4), HasV6: b2u(c.Underlay6), L2: b2u(c.L2)}
	copy(v.TunMac[:], c.TunMAC)
	return o.EgressCfg.Put(uint32(0), &v)
}

// Address is what a node knows of one egress address.
type Address struct {
	Held       bool   // this node announces or advertises it
	WholeOwner uint64 // PerPod: the NodeID owning every port; 0: sliced
	SliceSize  uint32 // a power of two
	Base       uint16 // first slice port
	Table      uint32 // the pool interface's routing table, for transit
}

// SetAddress installs or updates a.
func (o *Objects) SetAddress(a netip.Addr, v Address) error {
	if v.SliceSize == 0 || v.SliceSize&(v.SliceSize-1) != 0 || v.SliceSize > 1<<15 {
		return fmt.Errorf("slice size %d is not a power of two up to 32768", v.SliceSize)
	}
	e := egressAddrEntry{Held: b2u(v.Held), WholeOwner: v.WholeOwner, Base: v.Base, Table: v.Table,
		Shift: uint8(bits.TrailingZeros32(v.SliceSize))}
	k := addrKey(a)
	return o.AddrCfg.Put(&k, &e)
}

// DeleteAddress removes a.
func (o *Objects) DeleteAddress(a netip.Addr) error {
	k := addrKey(a)
	return ignoreNotExist(o.AddrCfg.Delete(&k))
}

// SetSlice sets the owner of slice index of a; owner 0 means drop.
func (o *Objects) SetSlice(a netip.Addr, index uint16, owner uint64) error {
	k := sliceKey(a, index)
	return o.Slices.Put(&k, owner)
}

// DeleteSlice frees slice index of a.
func (o *Objects) DeleteSlice(a netip.Addr, index uint16) error {
	k := sliceKey(a, index)
	return ignoreNotExist(o.Slices.Delete(&k))
}

// SetReserved gives (proto, port) of a to owner (D19).
func (o *Objects) SetReserved(a netip.Addr, proto uint8, port uint16, owner uint64) error {
	k := portKey(a, proto, port)
	return o.Reserved.Put(&k, owner)
}

// DeleteReserved removes a reserved port.
func (o *Objects) DeleteReserved(a netip.Addr, proto uint8, port uint16) error {
	k := portKey(a, proto, port)
	return ignoreNotExist(o.Reserved.Delete(&k))
}

// SetServicePort passes (proto, port) of a shared address to kube-proxy.
func (o *Objects) SetServicePort(a netip.Addr, proto uint8, port uint16) error {
	k := portKey(a, proto, port)
	return o.SvcPorts.Put(&k, uint8(1))
}

// DeleteServicePort removes one.
func (o *Objects) DeleteServicePort(a netip.Addr, proto uint8, port uint16) error {
	k := portKey(a, proto, port)
	return ignoreNotExist(o.SvcPorts.Delete(&k))
}

// Node is one node's underlay addresses and tunnel MAC.
type Node struct {
	V4, V6 netip.Addr // invalid when the node has none in that family
	MAC    net.HardwareAddr
}

// SetNode installs node id, and its underlay addresses as the ones its
// tunnel packets may come from.
func (o *Objects) SetNode(id uint64, n Node) error {
	v := egressNode{}
	if n.V4.IsValid() {
		w, _ := words(n.V4)
		v.V4, v.HasV4 = w[0], 1
		k := egressUnderlayKey{Addr: w}
		if err := o.NodeByUnderlay.Put(&k, id); err != nil {
			return err
		}
	}
	if n.V6.IsValid() {
		w, _ := words(n.V6)
		v.V6, v.HasV6 = w, 1
		k := egressUnderlayKey{Addr: w, V6: 1}
		if err := o.NodeByUnderlay.Put(&k, id); err != nil {
			return err
		}
	}
	copy(v.Mac[:], n.MAC)
	return o.Nodes.Put(id, &v)
}

// DeleteNode removes node id and its underlay addresses.
func (o *Objects) DeleteNode(id uint64, n Node) error {
	for _, a := range []netip.Addr{n.V4, n.V6} {
		if !a.IsValid() {
			continue
		}
		w, _ := words(a)
		k := egressUnderlayKey{Addr: w, V6: b2u(a.Is6())}
		if err := ignoreNotExist(o.NodeByUnderlay.Delete(&k)); err != nil {
			return err
		}
	}
	return ignoreNotExist(o.Nodes.Delete(id))
}

// SetPoolInterface numbers pool interface ifindex: its routing table is
// 7900 + n and a reply arriving on it directly is marked 0x81 + n.
func (o *Objects) SetPoolInterface(ifindex int, n uint32) error {
	if n > 63 {
		return fmt.Errorf("pool interface number %d is over 63", n)
	}
	return o.PoolIfs.Put(uint32(ifindex), &egressPoolIf{N: n})
}

// DeletePoolInterface removes one.
func (o *Objects) DeletePoolInterface(ifindex int) error {
	return ignoreNotExist(o.PoolIfs.Delete(uint32(ifindex)))
}

// Counts returns each reason's packet counts for IPv4 and IPv6, summed
// over CPUs.
func (o *Objects) Counts() (map[string][2]uint64, error) {
	out := map[string][2]uint64{}
	for i, r := range Reasons {
		var c [2]uint64
		for fam := 0; fam < 2; fam++ {
			var per []uint64
			if err := o.EgressReasons.Lookup(uint32(i*2+fam), &per); err != nil {
				return nil, err
			}
			for _, v := range per {
				c[fam] += v
			}
		}
		out[r] = c
	}
	return out, nil
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func ignoreNotExist(err error) error {
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil
	}
	return err
}
