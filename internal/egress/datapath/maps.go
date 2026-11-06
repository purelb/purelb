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
	"os"
	"path/filepath"

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
	// Self4 and Self6 are this node's underlay addresses (InternalIPs),
	// the tunnel's outer source; invalid where it has none.
	Self4, Self6 netip.Addr
	// TunPort is the tunnel's UDP port.
	TunPort uint16
	// EBPFStage: this node runs the eBPF pod stage (design 6.4).
	EBPFStage bool
	// PodBySrc: netkit pods, whose packets reach the NIC with no ingress
	// device, so the stage finds them by source address.
	PodBySrc bool
}

// SetConfig writes the node-wide configuration.
func (o *Objects) SetConfig(c Config) error {
	v := egressEgressConfig{Self: c.Self, TunIfindex: uint32(c.TunIfindex), TunMtu: c.TunMTU,
		MarkShift: c.MarkShift, HasV4: b2u(c.Underlay4), HasV6: b2u(c.Underlay6), L2: b2u(c.L2),
		TunPort: be16Native(c.TunPort), EbpfStage: b2u(c.EBPFStage), PodBySrc: b2u(c.PodBySrc)}
	copy(v.TunMac[:], c.TunMAC)
	if c.Self4.IsValid() {
		w, _ := words(c.Self4)
		v.SelfV4 = w[0]
	}
	if c.Self6.IsValid() {
		v.SelfV6, _ = words(c.Self6)
	}
	return o.EgressCfg.Put(uint32(0), &v)
}

// Address is what a node knows of one egress address.
type Address struct {
	Held       bool   // this node announces or advertises it
	WholeOwner uint64 // PerPod: the NodeID owning every port; 0: sliced
	SliceSize  uint32 // a power of two; 0 when not held and not known
	Base       uint16 // first slice port
	Table      uint32 // the pool interface's routing table, for transit
}

// SetAddress installs or updates a.
func (o *Objects) SetAddress(a netip.Addr, v Address) error {
	unknown := v.SliceSize == 0 && !v.Held && v.WholeOwner == 0
	if !unknown && (v.SliceSize == 0 || v.SliceSize&(v.SliceSize-1) != 0 || v.SliceSize > 1<<15) {
		return fmt.Errorf("slice size %d is not a power of two up to 32768", v.SliceSize)
	}
	e := egressAddrEntry{Held: b2u(v.Held), WholeOwner: v.WholeOwner, Base: v.Base, Table: v.Table}
	if !unknown {
		e.Shift = uint8(bits.TrailingZeros32(v.SliceSize))
	}
	k := addrKey(a)
	if err := o.EnsureTraffic(a); err != nil {
		return err
	}
	return o.AddrCfg.Put(&k, &e)
}

// EnsureTraffic adds a's traffic counters unless it has them. The programs
// only look them up, so an address without an entry isn't counted.
func (o *Objects) EnsureTraffic(a netip.Addr) error {
	k := addrKey(a)
	zero := make([]egressTraffic, ebpf.MustPossibleCPU())
	err := o.EgressTraffic.Update(&k, zero, ebpf.UpdateNoExist)
	if errors.Is(err, ebpf.ErrKeyExist) {
		return nil
	}
	return err
}

// PortKey is one (protocol, port) of an address.
type PortKey struct {
	Proto uint8
	Port  uint16
}

// AddressEntries are one address's entries in the maps.
type AddressEntries struct {
	Address
	Slices   map[uint16]uint64 // index: owner
	Reserved map[PortKey]uint64
	Services map[PortKey]bool
}

// RecoverAddresses returns the address entries the maps hold, as an
// earlier agent left them in the pinned maps, so the next apply prunes the
// ones gone meanwhile (D12: recover from the kernel, then prune). Slices,
// reserved and Service ports of an address with no addr_cfg entry come
// back under it too, so they are pruned with it.
func (o *Objects) RecoverAddresses() (map[netip.Addr]*AddressEntries, error) {
	out := map[netip.Addr]*AddressEntries{}
	get := func(w [4]uint32, family uint8) *AddressEntries {
		a := unwords(w, family == familyV6)
		e := out[a]
		if e == nil {
			e = &AddressEntries{Slices: map[uint16]uint64{}, Reserved: map[PortKey]uint64{}, Services: map[PortKey]bool{}}
			out[a] = e
		}
		return e
	}
	var ak egressAddrKey
	var av egressAddrEntry
	it := o.AddrCfg.Iterate()
	for it.Next(&ak, &av) {
		e := get(ak.Addr, ak.Family)
		e.Address = Address{Held: av.Held != 0, WholeOwner: av.WholeOwner, Base: av.Base, Table: av.Table,
			SliceSize: 1 << av.Shift}
		if av.Shift == 0 && av.Held == 0 && av.WholeOwner == 0 {
			e.SliceSize = 0
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	var sk egressSliceKey
	var owner uint64
	si := o.Slices.Iterate()
	for si.Next(&sk, &owner) {
		get(sk.Addr, sk.Family).Slices[sk.Index] = owner
	}
	if err := si.Err(); err != nil {
		return nil, err
	}
	var pk egressPortKey
	ri := o.Reserved.Iterate()
	for ri.Next(&pk, &owner) {
		get(pk.Addr, pk.Family).Reserved[PortKey{Proto: pk.Proto, Port: pk.Port}] = owner
	}
	if err := ri.Err(); err != nil {
		return nil, err
	}
	var one uint8
	vi := o.SvcPorts.Iterate()
	for vi.Next(&pk, &one) {
		get(pk.Addr, pk.Family).Services[PortKey{Proto: pk.Proto, Port: pk.Port}] = true
	}
	return out, vi.Err()
}

// DeleteAddress removes a.
func (o *Objects) DeleteAddress(a netip.Addr) error {
	k := addrKey(a)
	if err := ignoreNotExist(o.EgressTraffic.Delete(&k)); err != nil {
		return err
	}
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

// ReadNodes reads the nodes, changing nothing.
func (o *Objects) ReadNodes() (map[uint64]Node, error) {
	nodes := map[uint64]Node{}
	var id uint64
	var v egressNode
	it := o.Nodes.Iterate()
	for it.Next(&id, &v) {
		n := Node{MAC: append(net.HardwareAddr(nil), v.Mac[:]...)}
		if v.HasV4 != 0 {
			n.V4 = unwords([4]uint32{v.V4}, false)
		}
		if v.HasV6 != 0 {
			n.V6 = unwords(v.V6, true)
		}
		nodes[id] = n
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

// RecoverNodes returns the nodes the maps hold, as an earlier agent left
// them in the pinned maps, and removes underlay addresses that belong to
// none of them (D12: recover from the kernel, then prune).
func (o *Objects) RecoverNodes() (map[uint64]Node, error) {
	nodes, err := o.ReadNodes()
	if err != nil {
		return nil, err
	}
	var id uint64
	var k egressUnderlayKey
	var stale []egressUnderlayKey
	ui := o.NodeByUnderlay.Iterate()
	for ui.Next(&k, &id) {
		n, ok := nodes[id]
		if a := unwords(k.Addr, k.V6 != 0); !ok || (a != n.V4 && a != n.V6) {
			stale = append(stale, k)
		}
	}
	if err := ui.Err(); err != nil {
		return nil, err
	}
	for i := range stale {
		if err := ignoreNotExist(o.NodeByUnderlay.Delete(&stale[i])); err != nil {
			return nil, err
		}
	}
	return nodes, nil
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

// Traffic is one address's packets and bytes, by Traffic* kind.
type Traffic struct {
	Packets, Bytes [TrafficKinds]uint64
}

// The kinds of Traffic, in the programs' order (enum traffic_kind).
const (
	TrafficOutPod = iota
	TrafficReturnPod
	TrafficOutAnnouncer
	TrafficReturnAnnouncer
	TrafficKinds
)

// TrafficCounts returns each address's traffic, summed over CPUs.
func (o *Objects) TrafficCounts() (map[netip.Addr]Traffic, error) {
	out := map[netip.Addr]Traffic{}
	var k egressAddrKey
	var per []egressTraffic
	it := o.EgressTraffic.Iterate()
	for it.Next(&k, &per) {
		var t Traffic
		for _, v := range per {
			for i := range TrafficKinds {
				t.Packets[i] += v.Packets[i]
				t.Bytes[i] += v.Bytes[i]
			}
		}
		out[unwords(k.Addr, k.Family == familyV6)] = t
	}
	return out, it.Err()
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

// MaxHolders is the most holders a slot steers among.
const MaxHolders = 32

// SetSteer makes slot steer among holders (D1, D7); the pod node's
// traffic in it picks one by flow hash.
func (o *Objects) SetSteer(slot uint8, holders []uint64) error {
	if slot == 0 || slot > 127 {
		return fmt.Errorf("slot %d is not 1-127", slot)
	}
	if len(holders) == 0 || len(holders) > MaxHolders {
		return fmt.Errorf("%d holders: a slot steers among 1 to %d", len(holders), MaxHolders)
	}
	v := egressHolders{N: uint32(len(holders))}
	copy(v.Holders[:], holders)
	return o.Steer.Put(uint32(slot), &v)
}

// DeleteSteer removes slot's holders: its traffic is dropped.
func (o *Objects) DeleteSteer(slot uint8) error {
	return ignoreNotExist(o.Steer.Delete(uint32(slot)))
}

// MarkShift is the mark field's byte the configuration holds (D18).
func (o *Objects) MarkShift() (uint8, error) {
	var v egressEgressConfig
	if err := o.EgressCfg.Lookup(uint32(0), &v); err != nil {
		return 0, err
	}
	return v.MarkShift, nil
}

// PinnedMarkShift reads the mark field's byte from the configuration
// pinned in pinDir, as an earlier agent left it; ok is false without one.
func PinnedMarkShift(pinDir string) (shift uint8, ok bool, err error) {
	m, err := ebpf.LoadPinnedMap(filepath.Join(pinDir, "egress_cfg"), &ebpf.LoadPinOptions{ReadOnly: true})
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer m.Close()
	o := &Objects{}
	o.EgressCfg = m
	shift, err = o.MarkShift()
	return shift, err == nil, err
}
