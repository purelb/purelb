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
	"hash/fnv"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"

	"purelb.io/internal/bpfpin"
)

// CgroupRoot is where the egress install mounts the node's cgroup2 root, read
// only: pl_pod_inbound attaches there, and needs nothing beyond CAP_BPF and
// CAP_NET_ADMIN (measured 2026-11-03).
var CgroupRoot = "/host-cgroup"

// InboundLink names pl_pod_inbound's link, pinned under it.
const InboundLink = "pl_pod_inbound"

// Where an eBPF-stage slot's traffic leaves by (design 6.4).
const (
	TargetProhibit uint8 = iota
	TargetTunnel
	TargetDirect
)

// SlotRoute is how one slot's traffic leaves this node.
type SlotRoute struct {
	Target uint8
	Table  uint32 // TargetDirect: the routing table
	MTU    uint16 // the path MTU; 0: unchecked
}

// nat states, as the programs write them.
const (
	natOut uint8 = iota + 1
	natIn
)

const (
	nsNew uint8 = iota + 1
	nsEst
	nsFin
	nsClosed
)

// maxPodSetPolicies is how many policies the programs walk per pod-set and
// family: pod_set_policies' index is 8 bits.
const maxPodSetPolicies = 255

// Program is the eBPF stage's classification as the maps hold it, the same
// decision the nft stage's chains make (design 6.3). Policy ids are stable
// across a rewrite of the others, so only what changed moves: each is a hash
// of the policy's name and family, the next free where two collide.
type program struct {
	slots    map[uint32]egressPodSlot
	sets     map[egressPodSetKey]uint32
	policies map[uint32]egressPodPolicy
	dest4    map[egressDest4Key]uint32
	dest6    map[egressDest6Key]uint32
	ports    map[egressDestPortKey]uint8
	reserved map[egressPortKey]uint8
}

// programOf builds the maps' contents for stage p and its slots' routes.
func programOf(p PodStage, routes map[uint8]SlotRoute) (*program, error) {
	o := &program{slots: map[uint32]egressPodSlot{}, sets: map[egressPodSetKey]uint32{},
		policies: map[uint32]egressPodPolicy{}, dest4: map[egressDest4Key]uint32{},
		dest6: map[egressDest6Key]uint32{}, ports: map[egressDestPortKey]uint8{}, reserved: map[egressPortKey]uint8{}}

	for _, id := range sortedKeys(p.Slots) {
		s := p.Slots[id]
		w, fam := words(s.Addr)
		r := routes[id]
		v := egressPodSlot{Addr: w, Family: fam, Mss: s.MSS, Mtu: r.MTU, Target: r.Target, Table: r.Table}
		if s.First != 0 || s.Last != 0 {
			size := uint32(s.Last) - uint32(s.First) + 1
			if size == 0 || size&(size-1) != 0 || size > 1<<15 {
				return nil, fmt.Errorf("slot %d: %d ports is not a power of two up to 32768", id, size)
			}
			v.First, v.Size = s.First, uint16(size)
		}
		o.slots[uint32(id)] = v
		for _, rp := range s.Reserved {
			w, fam := words(rp.Pod)
			o.reserved[egressPortKey{Addr: w, Family: fam, Proto: rp.Proto, Port: rp.Port}] = id
		}
	}

	ids := map[string]uint32{}
	used := map[uint32]bool{}
	policyID := func(name string, v6 bool) uint32 {
		k := fmt.Sprintf("%s/%v", name, v6)
		if id, ok := ids[k]; ok {
			return id
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(k))
		id := h.Sum32() >> 1 // ids are doubled for their two destination lists
		for used[id] {
			id = (id + 1) & 0x7fffffff
		}
		used[id], ids[k] = true, id
		return id
	}
	sets := map[string]uint32{}
	used6 := map[uint32]bool{}
	portSet := func(rs []PortRange) uint32 {
		k := portSetName(rs)
		if id, ok := sets[k]; ok {
			return id
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(k))
		id := h.Sum32()
		for id == 0 || used6[id] {
			id++
		}
		used6[id], sets[k] = true, id
		for _, r := range rs {
			for _, b := range portBlocks(r.First, r.Last) {
				k := egressDestPortKey{Prefixlen: uint32(48 + b.bits), Set: id, Proto: r.Proto, Port: be16Native(b.first)}
				o.ports[k] = 1
			}
		}
		return id
	}

	for _, setID := range sortedKeys(p.PodSets) {
		idx := [2]int{}
		for _, name := range p.PodSets[setID] {
			pol, ok := p.Policies[name]
			if !ok {
				continue
			}
			for fi, pf := range []*PolicyFamily{pol.V4, pol.V6} {
				if pf == nil {
					continue
				}
				if idx[fi] >= maxPodSetPolicies {
					return nil, fmt.Errorf("pod-set %d: more than %d policies", setID, maxPodSetPolicies)
				}
				v6 := fi == 1
				id := policyID(name, v6)
				fam := uint8(familyV4)
				if v6 {
					fam = familyV6
				}
				o.sets[egressPodSetKey{PodSet: setID, Family: fam, Index: uint8(idx[fi])}] = id
				idx[fi]++
				o.policies[id] = egressPodPolicy{Slot: pf.Slot, Whole: b2u(pf.Whole), FailOpen: b2u(pf.FailOpen)}
				for li, list := range [][]Destination{pf.Include, pf.Exclude} {
					listID := id*2 + uint32(li)
					for prefix, rs := range flattenDestinations(list, v6) {
						set := uint32(0)
						if rs != nil {
							set = portSet(rs)
						}
						w, _ := words(prefix.Addr())
						if v6 {
							o.dest6[egressDest6Key{Prefixlen: uint32(32 + prefix.Bits()), Id: listID, Addr: w}] = set
						} else {
							o.dest4[egressDest4Key{Prefixlen: uint32(32 + prefix.Bits()), Id: listID, Addr: w[0]}] = set
						}
					}
				}
			}
		}
	}
	return o, nil
}

// flattenDestinations turns a list's destinations into what a longest-prefix
// lookup can answer: every prefix of the family, with the ports of every
// destination holding it. A nil value is every port. Prefixes nest or are
// disjoint, so what covers an address's longest match covers the address.
func flattenDestinations(list []Destination, v6 bool) map[netip.Prefix][]PortRange {
	out := map[netip.Prefix][]PortRange{}
	for _, d := range list {
		for _, p := range d.Prefixes {
			if p.Addr().Is6() != v6 {
				continue
			}
			p = p.Masked()
			if _, ok := out[p]; ok {
				continue
			}
			any := false
			var rs []PortRange
			for _, c := range list {
				if !holds(c.Prefixes, p) {
					continue
				}
				if len(c.Ports) == 0 {
					any = true
				}
				rs = append(rs, c.Ports...)
			}
			if any {
				rs = nil
			} else {
				rs = mergePorts(rs)
			}
			out[p] = rs
		}
	}
	return out
}

func holds(prefixes []netip.Prefix, p netip.Prefix) bool {
	for _, q := range prefixes {
		if q.Addr().Is6() == p.Addr().Is6() && q.Bits() <= p.Bits() && q.Masked().Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// mergePorts sorts and merges ranges per protocol, into a canonical form.
func mergePorts(rs []PortRange) []PortRange {
	byProto := map[uint8][]PortRange{}
	for _, r := range rs {
		byProto[r.Proto] = append(byProto[r.Proto], r)
	}
	var out []PortRange
	for _, proto := range slices.Sorted(maps.Keys(byProto)) {
		l := byProto[proto]
		slices.SortFunc(l, func(x, y PortRange) int { return int(x.First) - int(y.First) })
		for _, r := range l {
			if n := len(out); n > 0 && out[n-1].Proto == proto && uint32(r.First) <= uint32(out[n-1].Last)+1 {
				if r.Last > out[n-1].Last {
					out[n-1].Last = r.Last
				}
				continue
			}
			out = append(out, r)
		}
	}
	return out
}

func portSetName(rs []PortRange) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "%d:%d-%d,", r.Proto, r.First, r.Last)
	}
	return b.String()
}

// portBlock is an aligned run of ports: first and its fixed leading bits.
type portBlock struct {
	first uint16
	bits  int
}

// portBlocks covers first..last with aligned blocks, for a longest-prefix
// lookup.
func portBlocks(first, last uint16) []portBlock {
	var out []portBlock
	for p := uint32(first); p <= uint32(last); {
		size := uint32(1)
		bits := 16
		for bits > 0 && p&(size*2-1) == 0 && p+size*2-1 <= uint32(last) {
			size *= 2
			bits--
		}
		out = append(out, portBlock{uint16(p), bits})
		p += size
	}
	return out
}

// be16Native is v's bytes in network order, as a native __be16 field holds
// them.
func be16Native(v uint16) uint16 {
	return v>>8 | v<<8
}

// SyncEBPFStage makes the eBPF stage's maps hold stage p with its slots'
// routes: slots first, then destinations, policies and pod-sets, each added
// or changed before anything stale goes, so a new flow finds a whole decision
// (an old one or a new one) at every step.
func (o *Objects) SyncEBPFStage(p PodStage, routes map[uint8]SlotRoute) error {
	want, err := programOf(p, routes)
	if err != nil {
		return err
	}
	var prune []func() error
	step := func(put, del func() error) error {
		if err := put(); err != nil {
			return err
		}
		prune = append(prune, del)
		return nil
	}
	for _, s := range []struct{ put, del func() error }{
		syncMap(o.PodSlots, want.slots),
		syncMap(o.PodReserved, want.reserved),
		syncMap(o.PodDestPorts, want.ports),
		syncMap(o.PodDest4, want.dest4),
		syncMap(o.PodDest6, want.dest6),
		syncMap(o.PodPolicies, want.policies),
		syncMap(o.PodSetPolicies, want.sets),
	} {
		if err := step(s.put, s.del); err != nil {
			return err
		}
	}
	var errs []error
	for i := len(prune) - 1; i >= 0; i-- {
		errs = append(errs, prune[i]())
	}
	return errors.Join(errs...)
}

// ClearEBPFStage empties the eBPF stage's classification.
func (o *Objects) ClearEBPFStage() error {
	return o.SyncEBPFStage(PodStage{}, nil)
}

// syncMap is a map's writes and its pruning, to run in that order.
func syncMap[K, V comparable](m *ebpf.Map, want map[K]V) struct{ put, del func() error } {
	var stale []K
	return struct{ put, del func() error }{
		put: func() error {
			var errs []error
			have := map[K]V{}
			var k K
			var v V
			it := m.Iterate()
			for it.Next(&k, &v) {
				have[k] = v
				if _, ok := want[k]; !ok {
					stale = append(stale, k)
				}
			}
			errs = append(errs, it.Err())
			for k, v := range want {
				if cur, ok := have[k]; !ok || cur != v {
					errs = append(errs, m.Put(&k, &v))
				}
			}
			return errors.Join(errs...)
		},
		del: func() error {
			var errs []error
			for i := range stale {
				errs = append(errs, ignoreNotExist(m.Delete(&stale[i])))
			}
			return errors.Join(errs...)
		},
	}
}

// RecoverPodSlots reads the slots back from the maps, as an earlier agent
// left them, so a restarted agent gives flows the ids they carry (D12, D18).
func (o *Objects) RecoverPodSlots() (map[uint8]SlotNAT, error) {
	out := map[uint8]SlotNAT{}
	var k uint32
	var v egressPodSlot
	it := o.PodSlots.Iterate()
	for it.Next(&k, &v) {
		s := SlotNAT{Addr: unwords(v.Addr, v.Family == familyV6)}
		if v.Size != 0 {
			s.First, s.Last = v.First, v.First+v.Size-1
		}
		out[uint8(k)] = s
	}
	return out, it.Err()
}

// NatTimeouts are how long a flow may be idle, by protocol and state (D32).
// Established TCP is long: a flow dropped alive leaves untranslated (design
// 6.4, the residual of user decision 2026-11-04).
const (
	natTCPSyn         = 120 * time.Second
	natTCPEstablished = 6 * time.Hour
	natClosing        = 10 * time.Second
	natUDPNew         = 30 * time.Second
	natUDPEstablished = 120 * time.Second
	natICMP           = 30 * time.Second
	natOtherNew       = 30 * time.Second
	natOtherEst       = 10 * time.Minute
)

func natTimeout(proto, state uint8) time.Duration {
	if state == nsFin || state == nsClosed {
		return natClosing
	}
	est := state == nsEst
	switch proto {
	case unix.IPPROTO_TCP:
		if est {
			return natTCPEstablished
		}
		return natTCPSyn
	case unix.IPPROTO_UDP:
		if est {
			return natUDPEstablished
		}
		return natUDPNew
	case unix.IPPROTO_ICMP, unix.IPPROTO_ICMPV6:
		return natICMP
	}
	if est {
		return natOtherEst
	}
	return natOtherNew
}

// NatFlow is one translated flow, as its inbound entry holds it.
type NatFlow struct {
	key   egressNatKey
	Pod   netip.Addr
	Slot  uint8
	Proto uint8
	State uint8
	// Seen is the last packet's time, on bpf_ktime_get_ns's clock.
	Seen time.Duration
	port uint16
}

// NatFlows lists the translated flows.
func (o *Objects) NatFlows() ([]NatFlow, error) {
	var out []NatFlow
	var k egressNatKey
	var v egressNatVal
	it := o.Nat.Iterate()
	for it.Next(&k, &v) {
		if k.Dir != natIn {
			continue
		}
		out = append(out, NatFlow{key: k, Pod: unwords(v.Addr, k.Family == familyV6), Slot: v.Slot, Proto: k.Proto,
			State: v.State, Seen: time.Duration(v.Ts), port: v.Port})
	}
	return out, it.Err()
}

// NatDelete removes a flow, both of its entries. Its next packet is a new
// flow if TCP's SYN or another protocol's, and otherwise leaves untranslated
// until the far end resets it.
func (o *Objects) NatDelete(f NatFlow) error {
	w, _ := words(f.Pod)
	out := egressNatKey{Raddr: f.key.Raddr, Laddr: w, Rport: f.key.Rport, Lport: f.port, Proto: f.key.Proto,
		Family: f.key.Family, Dir: natOut}
	return errors.Join(ignoreNotExist(o.Nat.Delete(&out)), ignoreNotExist(o.Nat.Delete(&f.key)))
}

// NatGC deletes the flows idle past their timeout at now, a time on
// bpf_ktime_get_ns's clock, and returns how many.
func (o *Objects) NatGC(now time.Duration) (int, error) {
	flows, err := o.NatFlows()
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, f := range flows {
		if now-f.Seen <= natTimeout(f.Proto, f.State) {
			continue
		}
		if err := o.NatDelete(f); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// MonotonicNow is the time on bpf_ktime_get_ns's clock.
func MonotonicNow() (time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return time.Duration(ts.Nano()), nil
}

// PortlessCounts are the port-less flows each slot's slice couldn't carry
// (D4), since these maps were created.
func (o *Objects) PortlessCounts() (map[uint8]uint64, error) {
	out := map[uint8]uint64{}
	for id := range uint32(256) {
		var per []uint64
		if err := o.PodPortless.Lookup(id, &per); err != nil {
			return nil, err
		}
		var n uint64
		for _, v := range per {
			n += v
		}
		if n != 0 {
			out[uint8(id)] = n
		}
	}
	return out, nil
}

// AttachNetkitPeer attaches prog at the head of the netkit primary ifindex's
// peer hook, where a pod's packet still has its source, ahead of the CNI
// (design 6.4). As AttachTCX, it pins and takes over.
func AttachNetkitPeer(pinDir, name string, ifindex int, prog *ebpf.Program) (link.Link, error) {
	path := ""
	if pinDir != "" {
		path = filepath.Join(pinDir, name)
		if l, err := bpfpin.Adopt(path, ifindex, prog); err != nil || l != nil {
			return l, err
		}
	}
	l, err := link.AttachNetkit(link.NetkitOptions{Program: prog, Interface: ifindex, Attach: ebpf.AttachNetkitPeer, Anchor: link.Head()})
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

// AttachInbound attaches pl_pod_inbound to the root cgroup, pinned, taking
// over a link an earlier agent pinned. It is on every socket's delivery on the
// node, and only on eBPF-stage nodes.
func AttachInbound(pinDir string, prog *ebpf.Program) (link.Link, error) {
	path := ""
	if pinDir != "" {
		path = filepath.Join(pinDir, InboundLink)
		l, err := link.LoadPinnedLink(path, nil)
		if err == nil {
			if err := l.Update(prog); err != nil {
				_ = l.Close()
				return nil, fmt.Errorf("replacing the program of pinned link %s: %w", path, err)
			}
			return l, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("loading pinned link %s: %w", path, err)
		}
	}
	l, err := link.AttachCgroup(link.CgroupOptions{Path: CgroupRoot, Attach: ebpf.AttachCGroupInetIngress, Program: prog})
	if err != nil {
		return nil, fmt.Errorf("attaching to the root cgroup at %s: %w", CgroupRoot, err)
	}
	if path != "" {
		if err := l.Pin(path); err != nil {
			_ = l.Close()
			return nil, err
		}
	}
	return l, nil
}

// AtHead reports whether prog's id is first in ifindex's chain at hook; true
// when the chain can't be read (the device went: the next apply follows).
func AtHead(ifindex int, hook ebpf.AttachType, prog *ebpf.Program) bool {
	info, err := prog.Info()
	if err != nil {
		return true
	}
	id, ok := info.ID()
	if !ok {
		return true
	}
	res, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: hook})
	if err != nil || len(res.Programs) == 0 {
		return true
	}
	return res.Programs[0].ID == id
}

// PinnedEBPFStage reports whether an earlier agent ran the eBPF stage here:
// a restarted agent with no new pod would otherwise never learn its stage.
func PinnedEBPFStage(pinDir string) bool {
	m, err := ebpf.LoadPinnedMap(filepath.Join(pinDir, "egress_cfg"), &ebpf.LoadPinOptions{ReadOnly: true})
	if err != nil {
		return false
	}
	defer m.Close()
	var v egressEgressConfig
	if err := m.Lookup(uint32(0), &v); err != nil {
		return false
	}
	return v.EbpfStage != 0
}

// PinnedPodSlots reads the slots from the map pinned in pinDir, as an earlier
// agent left it; none without such a map.
func PinnedPodSlots(pinDir string) (map[uint8]SlotNAT, error) {
	m, err := ebpf.LoadPinnedMap(filepath.Join(pinDir, "pod_slots"), &ebpf.LoadPinOptions{ReadOnly: true})
	if errors.Is(err, os.ErrNotExist) {
		return map[uint8]SlotNAT{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer m.Close()
	return (&Objects{egressObjects: egressObjects{egressMaps: egressMaps{PodSlots: m}}}).RecoverPodSlots()
}
