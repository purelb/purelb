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
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// PodStage is the pod-node stage's netfilter program (design 6.3): a
// selected pod's new flow is classified per (pod, destination) to a slot
// (D24), the slot is kept in the conntrack mark and restored on every
// later packet with constant writes (D18), and the flow is SNATed into this
// node's slice of the slot's address.
type PodStage struct {
	// MarkShift places the 8-bit mark field (D18).
	MarkShift uint8
	// SourceIP classifies new flows by the pod's source address, which
	// pl_pod_tag's source check makes trustworthy, instead of its mark
	// (design 6.2).
	SourceIP bool
	// Cluster are the cluster networks, never egress.
	Cluster []netip.Prefix
	// Pods maps each selected pod's addresses to its pod-set id (SourceIP).
	Pods map[netip.Addr]uint8
	// PodSets are each pod-set id's policies, by name, in rank order.
	PodSets map[uint8][]string
	// Policies are the policies (a PerPod policy once per pod) by name.
	Policies map[string]Policy
	// Slots are this node's slots by id.
	Slots map[uint8]Slot
}

// Policy is one policy as a pod node classifies for it, per family; nil
// where it has no address in that family, so it never matches there.
type Policy struct {
	V4, V6 *PolicyFamily
}

// PolicyFamily is a policy's classification in one family.
type PolicyFamily struct {
	// A destination matching an Include entry and no Exclude entry is
	// the policy's.
	Include, Exclude []Destination
	// Slot is the slot its new flows take; 0 while none is Active.
	Slot uint8
	// Whole: the slot is a whole address (PerPod), any protocol;
	// otherwise a slice, which carries TCP, UDP, SCTP and ICMP echo only
	// (D4).
	Whole bool
	// FailOpen: without a slot, or for a port-less protocol, its flows
	// leave from the pod's address; otherwise they are dropped.
	FailOpen bool
	// Address is the policy's address in this family: port-less flows on
	// it are counted under it.
	Address netip.Addr
}

// Destination is some networks and, optionally, ports.
type Destination struct {
	Prefixes []netip.Prefix
	// Ports filters by destination port; none: every protocol and port.
	Ports []PortRange
}

// PortRange is a protocol's destination ports, First to Last.
type PortRange struct {
	Proto       uint8
	First, Last uint16
}

// Slot is one (policy, address) slice of this node's (design 6.2).
type Slot struct {
	Addr netip.Addr
	// First and Last are the slice's ports; both 0: the whole address.
	First, Last uint16
	// MSS is the TCP MSS its flows are clamped to; 0: none.
	MSS uint16
	// Reserved translate one pod's source port to the same port on Addr
	// ahead of the slice (D19).
	Reserved []ReservedPort
	// Helpers are the conntrack helpers its flows run (D23).
	Helpers []Helper
}

// ReservedPort is a pod's reserved source port.
type ReservedPort struct {
	Pod   netip.Addr
	Proto uint8
	Port  uint16
}

// Helper is one kernel conntrack helper on its well-known port.
type Helper struct {
	// Name is the kernel's helper name: ftp, sip, Q.931 or RAS.
	Name  string
	Proto uint8
	Port  uint16
}

// noEgress is the conntrack mark field of a flow classified not egress.
const noEgress = 0xff

// replyFlag is the mark field's bit pl_announce sets on replies (design
// 6.2).
const replyFlag = 0x80

// podStage adds the pod-node stage's sets and chains:
//
//	classify  prerouting dstnat+10: a reply keeps pl_announce's flag; an
//	          established flow gets its slot back from the ct mark; a
//	          selected pod's new flow goes to its pod-set's chain; every
//	          other exit clears the field
//	helpers   prerouting dstnat+20: a slot's helpers (D23)
//	mss       forward mangle: SYN and SYN-ACK clamped per slot
//	nat       postrouting srcnat-10: the slot's SNAT, ahead of the CNI's
//	          and kube-proxy's masquerade
func (b *builder) podStage(p PodStage) error {
	m := markField{shift: p.MarkShift}
	for _, f := range nftFamilies {
		set := &nftables.Set{Name: "cluster" + f.n, KeyType: addrType(f), Interval: true}
		if err := b.set(set, intervals(p.Cluster, f)); err != nil {
			return err
		}
	}

	// Each slot's restore, mark and SNAT chains.
	restore := []nftables.SetElement{m.verdictElem(noEgress, goTo("rs_none"))}
	snat := []nftables.SetElement{}
	b.rule(b.chain("rs_none", nil), m.set(0)...)
	noeg := b.chain("noeg", nil)
	b.rule(noeg, cat(m.set(0), m.setCt(noEgress), verdict(expr.VerdictAccept))...)
	for _, id := range sortedKeys(p.Slots) {
		s := p.Slots[id]
		rs, sl, sn := fmt.Sprintf("rs_%d", id), fmt.Sprintf("slot_%d", id), fmt.Sprintf("sn_%d", id)
		b.rule(b.chain(rs, nil), m.set(id)...)
		b.rule(b.chain(sl, nil), cat(m.set(id), m.setCt(id), verdict(expr.VerdictAccept))...)
		ch := b.chain(sn, nil)
		f := familyOf(s.Addr)
		for _, r := range s.Reserved {
			b.rule(ch, cat(f.match(), f.saddr(1), []expr.Any{&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: r.Pod.AsSlice()}},
				l4protoIs(r.Proto), sport(r.Port), snatTo(s.Addr, r.Port, r.Port))...)
		}
		b.rule(ch, cat(f.match(), snatTo(s.Addr, s.First, s.Last))...)
		restore = append(restore, m.verdictElem(id, goTo(rs)))
		snat = append(snat, m.verdictElem(id, goTo(sn)))
	}
	for _, x := range []struct {
		name  string
		elems []nftables.SetElement
	}{{"restore", restore}, {"snat", snat}} {
		if err := b.set(&nftables.Set{Name: x.name, IsMap: true, KeyType: nftables.TypeMark, KeyByteOrder: binaryutil.NativeEndian,
			DataType: nftables.TypeVerdict}, x.elems); err != nil {
			return err
		}
	}

	// The policies' chains.
	for _, name := range sortedKeys(p.Policies) {
		pol := p.Policies[name]
		for _, x := range []struct {
			f  nftFamily
			pf *PolicyFamily
		}{{nftFamilies[0], pol.V4}, {nftFamilies[1], pol.V6}} {
			if x.pf != nil {
				if err := b.policy(m, name+"_"+x.f.n, x.f, *x.pf); err != nil {
					return err
				}
			}
		}
	}
	// Each pod-set's chain: its policies in rank order, the first whose
	// destinations match writes the slot (D24).
	podsets := []nftables.SetElement{}
	for _, id := range sortedKeys(p.PodSets) {
		ch := b.chain(fmt.Sprintf("ps_%d", id), nil)
		b.rule(ch, cat(fibLocal(), goTo("noeg"))...)
		for _, f := range nftFamilies {
			b.rule(ch, cat(f.match(), f.daddr(1), b.lookup("cluster"+f.n, false), goTo("noeg"))...)
		}
		for _, name := range p.PodSets[id] {
			pol := p.Policies[name]
			if pol.V4 != nil {
				b.rule(ch, cat(nftFamilies[0].match(), jump("pol_"+name+"_4"))...)
			}
			if pol.V6 != nil {
				b.rule(ch, cat(nftFamilies[1].match(), jump("pol_"+name+"_6"))...)
			}
		}
		b.rule(ch, goTo("noeg")...)
		podsets = append(podsets, m.verdictElem(id, goTo(ch.Name)))
	}
	if p.SourceIP {
		pods := map[string][]nftables.SetElement{}
		for addr, id := range p.Pods {
			if _, ok := p.PodSets[id]; ok {
				f := familyOf(addr)
				pods[f.n] = append(pods[f.n], nftables.SetElement{Key: addr.AsSlice(),
					VerdictData: &expr.Verdict{Kind: expr.VerdictGoto, Chain: fmt.Sprintf("ps_%d", id)}})
			}
		}
		for _, f := range nftFamilies {
			if err := b.set(&nftables.Set{Name: "pods" + f.n, IsMap: true, KeyType: addrType(f), DataType: nftables.TypeVerdict},
				pods[f.n]); err != nil {
				return err
			}
		}
	} else if err := b.set(&nftables.Set{Name: "podset", IsMap: true, KeyType: nftables.TypeMark, KeyByteOrder: binaryutil.NativeEndian,
		DataType: nftables.TypeVerdict}, podsets); err != nil {
		return err
	}

	cl := b.chain("classify", filterHook(nftables.ChainHookPrerouting, -90))
	if !p.SourceIP {
		// Unselected pods and host traffic: nothing to do.
		b.rule(cl, cat(m.load(), isZero(), m.loadCt(), isZero(), verdict(expr.VerdictReturn))...)
	}
	b.rule(cl, cat(ctDirection(1), m.load(), bitsSet(m, replyFlag), verdict(expr.VerdictReturn))...)
	b.rule(cl, cat(ctDirection(1), m.set(0), verdict(expr.VerdictReturn))...)
	established := uint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED)
	b.rule(cl, cat(ctState(established), m.loadCt(), b.vmap("restore"))...)
	b.rule(cl, cat(ctState(established), m.set(0), verdict(expr.VerdictReturn))...)
	if p.SourceIP {
		for _, f := range nftFamilies {
			b.rule(cl, cat(ctState(expr.CtStateBitNEW), f.match(), f.saddr(1), b.vmap("pods"+f.n))...)
		}
	} else {
		b.rule(cl, cat(ctState(expr.CtStateBitNEW), m.load(), b.vmap("podset"))...)
	}
	b.rule(cl, m.set(0)...)

	if err := b.helpers(m, p.Slots); err != nil {
		return err
	}
	if err := b.mss(m, p.Slots); err != nil {
		return err
	}
	nat := b.chain("nat", &baseChain{hook: nftables.ChainHookPostrouting, prio: nftables.ChainPriorityRef(90),
		typ: nftables.ChainTypeNAT})
	b.rule(nat, cat(m.load(), b.vmap("snat"))...)
	return nil
}

// policy adds a policy's chains in one family, named after it:
//
//	pol_<n>  returns for an excluded destination; goes to hit_<n> for an
//	         included one; returns otherwise
//	hit_<n>  the slot, for a protocol it carries; pf_<n> otherwise; with
//	         no slot, drop (FailClosed) or noeg (FailOpen)
//	pf_<n>   counts a port-less flow on a slice, then drop or noeg
func (b *builder) policy(m markField, n string, f nftFamily, pf PolicyFamily) error {
	dest := func(prefix string, ds []Destination, v []expr.Any) ([][]expr.Any, error) {
		var rules [][]expr.Any
		for i, d := range ds {
			addrs := fmt.Sprintf("%s_%d", prefix, i)
			if err := b.set(&nftables.Set{Name: addrs, KeyType: addrType(f), Interval: true}, intervals(d.Prefixes, f)); err != nil {
				return nil, err
			}
			match := cat(f.match(), f.daddr(1), b.lookup(addrs, false))
			if len(d.Ports) == 0 {
				rules = append(rules, cat(match, v))
				continue
			}
			byProto := map[uint8][]PortRange{}
			for _, r := range d.Ports {
				byProto[r.Proto] = append(byProto[r.Proto], r)
			}
			for _, proto := range slices.Sorted(maps.Keys(byProto)) {
				ports := fmt.Sprintf("%s_%d", addrs, proto)
				if err := b.set(&nftables.Set{Name: ports, KeyType: nftables.TypeInetService, Interval: true},
					portIntervals(byProto[proto])); err != nil {
					return nil, err
				}
				rules = append(rules, cat(match, l4protoIs(proto), dport(1), b.lookup(ports, false), v))
			}
		}
		return rules, nil
	}
	// Each chain before the rules that go to it.
	fail := verdict(expr.VerdictDrop)
	if pf.FailOpen {
		fail = goTo("noeg")
	}
	hit := b.chain("hit_"+n, nil)
	switch {
	case pf.Slot == 0:
		b.rule(hit, fail...)
	case pf.Whole:
		b.rule(hit, goTo(fmt.Sprintf("slot_%d", pf.Slot))...)
	default:
		b.obj(&nftables.NamedObj{Name: "pf_" + n, Type: nftables.ObjTypeCounter, Obj: &expr.Counter{}})
		b.rule(b.chain("pf_"+n, nil), cat([]expr.Any{&expr.Objref{Type: int(nftables.ObjTypeCounter), Name: "pf_" + n}}, fail)...)
		slot := goTo(fmt.Sprintf("slot_%d", pf.Slot))
		for _, proto := range []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP, unix.IPPROTO_SCTP} {
			b.rule(hit, cat(l4protoIs(proto), slot)...)
		}
		if f.nfproto == unix.NFPROTO_IPV4 {
			b.rule(hit, cat(l4protoIs(unix.IPPROTO_ICMP), icmpType(8), slot)...)
		} else {
			b.rule(hit, cat(l4protoIs(unix.IPPROTO_ICMPV6), icmpType(128), slot)...)
		}
		b.rule(hit, goTo("pf_"+n)...)
	}

	pol := b.chain("pol_"+n, nil)
	ex, err := dest("ex_"+n, pf.Exclude, verdict(expr.VerdictReturn))
	if err != nil {
		return err
	}
	in, err := dest("in_"+n, pf.Include, goTo("hit_"+n))
	if err != nil {
		return err
	}
	for _, r := range append(ex, in...) {
		b.rule(pol, r...)
	}
	return nil
}

// helpers adds the slots' conntrack helpers (D23): a new flow of the slot
// to a helper's port runs it.
func (b *builder) helpers(m markField, slots map[uint8]Slot) error {
	var ch *nftables.Chain
	objs := map[string]bool{}
	for _, id := range sortedKeys(slots) {
		s := slots[id]
		f := familyOf(s.Addr)
		for _, h := range s.Helpers {
			name := helperObj(h, f)
			if !objs[name] {
				l3 := uint16(unix.NFPROTO_IPV4)
				if f.nfproto == unix.NFPROTO_IPV6 {
					l3 = unix.NFPROTO_IPV6
				}
				b.obj(&nftables.NamedObj{Name: name, Type: nftables.ObjTypeCtHelper,
					Obj: &expr.CtHelper{Name: h.Name, L3Proto: l3, L4Proto: h.Proto}})
				objs[name] = true
			}
			if ch == nil {
				ch = b.chain("helpers", filterHook(nftables.ChainHookPrerouting, -80))
			}
			b.rule(ch, cat(ctState(expr.CtStateBitNEW), m.loadCt(), m.is(id), f.match(), l4protoIs(h.Proto),
				dport(1), []expr.Any{&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(h.Port)},
					&expr.Objref{Type: int(nftables.ObjTypeCtHelper), Name: name}})...)
		}
	}
	return nil
}

// helperObj names the ct helper object of h in family f.
func helperObj(h Helper, f nftFamily) string {
	return fmt.Sprintf("%s_%d_%s", h.Name, h.Proto, f.n)
}

// mss clamps SYN and SYN-ACK of each slot's flows, both ways, to its MSS:
// one rule per value, over the slots it applies to (google/nftables has
// no 16-bit integer for the design's slot map). The kernel only ever
// lowers an MSS option, so a pod's smaller one is kept.
func (b *builder) mss(m markField, slots map[uint8]Slot) error {
	byMSS := map[uint16][]nftables.SetElement{}
	for _, id := range sortedKeys(slots) {
		if v := slots[id].MSS; v != 0 {
			byMSS[v] = append(byMSS[v], nftables.SetElement{Key: ne32(uint32(id) << m.shift)})
		}
	}
	if len(byMSS) == 0 {
		return nil
	}
	ch := b.chain("mss", filterHook(nftables.ChainHookForward, -150))
	for _, v := range slices.Sorted(maps.Keys(byMSS)) {
		name := fmt.Sprintf("mss_%d", v)
		if err := b.set(&nftables.Set{Name: name, KeyType: nftables.TypeMark, KeyByteOrder: binaryutil.NativeEndian}, byMSS[v]); err != nil {
			return err
		}
		b.rule(ch, cat(l4protoIs(unix.IPPROTO_TCP), synFlag(), m.loadCt(), b.lookup(name, false),
			[]expr.Any{
				&expr.Immediate{Register: 1, Data: be16(v)},
				&expr.Exthdr{Op: expr.ExthdrOpTcpopt, Type: 2, Offset: 2, Len: 2, SourceRegister: 1},
			})...)
	}
	return nil
}

// markField reads and writes the 8-bit mark field (D18) with constant
// mask-and-xor only: a register-to-register OR is missing in nf_tables on
// 6.6.
type markField struct{ shift uint8 }

func (m markField) mask() uint32 { return 0xff << m.shift }

// load puts the packet's field in register 1.
func (m markField) load() []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ne32(m.mask()), Xor: ne32(0)},
	}
}

// loadCt puts the conntrack mark's field in register 1.
func (m markField) loadCt() []expr.Any {
	return []expr.Any{
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ne32(m.mask()), Xor: ne32(0)},
	}
}

// is matches register 1, a loaded field, against v.
func (m markField) is(v uint8) []expr.Any {
	return []expr.Any{&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne32(uint32(v) << m.shift)}}
}

// set writes v to the packet's field, keeping the other 24 bits.
func (m markField) set(v uint8) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ne32(^m.mask()), Xor: ne32(uint32(v) << m.shift)},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	}
}

// setCt writes v to the conntrack mark's field, keeping the other bits.
func (m markField) setCt(v uint8) []expr.Any {
	return []expr.Any{
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ne32(^m.mask()), Xor: ne32(uint32(v) << m.shift)},
		&expr.Ct{Key: expr.CtKeyMARK, SourceRegister: true, Register: 1},
	}
}

// verdictElem maps field value v to a verdict.
func (m markField) verdictElem(v uint8, vd []expr.Any) nftables.SetElement {
	return nftables.SetElement{Key: ne32(uint32(v) << m.shift), VerdictData: vd[0].(*expr.Verdict)}
}

// bitsSet matches a loaded field that has every bit of v set.
func bitsSet(m markField, v uint8) []expr.Any {
	bits := uint32(v) << m.shift
	return []expr.Any{
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ne32(bits), Xor: ne32(0)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne32(bits)},
	}
}

func isZero() []expr.Any {
	return []expr.Any{&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne32(0)}}
}

// fibLocal matches a destination that is one of this node's addresses.
func fibLocal() []expr.Any {
	return []expr.Any{
		&expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne32(unix.RTN_LOCAL)},
	}
}

// synFlag matches a TCP segment with SYN set and RST clear.
func synFlag() []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x06}, Xor: []byte{0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x02}},
	}
}

func sport(port uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(port)},
	}
}

// snatTo translates the source to addr and a port in [first, last]; both
// 0: the address alone.
func snatTo(addr netip.Addr, first, last uint16) []expr.Any {
	family := uint32(unix.NFPROTO_IPV4)
	if addr.Is6() {
		family = unix.NFPROTO_IPV6
	}
	nat := &expr.NAT{Type: expr.NATTypeSourceNAT, Family: family, RegAddrMin: 1, RegAddrMax: 1}
	out := []expr.Any{&expr.Immediate{Register: 1, Data: addr.AsSlice()}}
	if first != 0 || last != 0 {
		// Registers 2 and 3: a 16-byte address fills register 1. Only a
		// port range is "specified": without one, it would be port 0.
		out = append(out, &expr.Immediate{Register: 2, Data: be16(first)}, &expr.Immediate{Register: 3, Data: be16(last)})
		nat.RegProtoMin, nat.RegProtoMax, nat.Specified = 2, 3, true
	}
	return append(out, nat)
}

func addrType(f nftFamily) nftables.SetDatatype {
	if f.nfproto == unix.NFPROTO_IPV4 {
		return nftables.TypeIPAddr
	}
	return nftables.TypeIP6Addr
}

// intervals is the prefixes of family f as an interval set's elements:
// overlapping and adjacent prefixes merged, each range its first address
// and an end element one past its last, which a range ending at the
// family's last address has none of.
func intervals(prefixes []netip.Prefix, f nftFamily) []nftables.SetElement {
	type span struct{ first, last netip.Addr }
	var spans []span
	for _, p := range prefixes {
		p = p.Masked()
		if (p.Addr().Is4()) != (f.nfproto == unix.NFPROTO_IPV4) {
			continue
		}
		spans = append(spans, span{p.Addr(), lastAddr(p)})
	}
	slices.SortFunc(spans, func(x, y span) int { return x.first.Compare(y.first) })
	var merged []span
	for _, s := range spans {
		if n := len(merged); n > 0 {
			cur := &merged[n-1]
			if next := cur.last.Next(); !next.IsValid() || s.first.Compare(next) <= 0 {
				if s.last.Compare(cur.last) > 0 {
					cur.last = s.last
				}
				continue
			}
		}
		merged = append(merged, s)
	}
	var out []nftables.SetElement
	for _, s := range merged {
		out = append(out, nftables.SetElement{Key: s.first.AsSlice()})
		if next := s.last.Next(); next.IsValid() {
			out = append(out, nftables.SetElement{Key: next.AsSlice(), IntervalEnd: true})
		}
	}
	return out
}

// lastAddr is p's last address.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// portIntervals is port ranges as an interval set's elements, merged.
func portIntervals(ranges []PortRange) []nftables.SetElement {
	rs := slices.Clone(ranges)
	slices.SortFunc(rs, func(x, y PortRange) int { return int(x.First) - int(y.First) })
	var merged []PortRange
	for _, r := range rs {
		if n := len(merged); n > 0 && int(r.First) <= int(merged[n-1].Last)+1 {
			merged[n-1].Last = max(merged[n-1].Last, r.Last)
			continue
		}
		merged = append(merged, r)
	}
	var out []nftables.SetElement
	for _, r := range merged {
		out = append(out, nftables.SetElement{Key: be16(r.First)})
		if r.Last != 0xffff {
			out = append(out, nftables.SetElement{Key: be16(r.Last + 1), IntervalEnd: true})
		}
	}
	return out
}

// PortlessDrops reads the port-less counters of p's policies (D4): the
// packets counted under each address since the table was written.
func PortlessDrops(c *nftables.Conn, p PodStage) (map[netip.Addr]uint64, error) {
	objs, err := c.GetNamedObjects(&nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable})
	if err != nil {
		return nil, err
	}
	counts := map[string]uint64{}
	for _, o := range objs {
		if n, ok := o.(*nftables.NamedObj); ok {
			if ctr, ok := n.Obj.(*expr.Counter); ok {
				counts[n.Name] = ctr.Packets
			}
		}
	}
	out := map[netip.Addr]uint64{}
	for name, pol := range p.Policies {
		for _, x := range []struct {
			n  string
			pf *PolicyFamily
		}{{"4", pol.V4}, {"6", pol.V6}} {
			if x.pf != nil && x.pf.Address.IsValid() {
				out[x.pf.Address] += counts["pf_"+name+"_"+x.n]
			}
		}
	}
	return out, nil
}

// natHelperModules are the NAT helper modules, by kernel helper name: the
// conntrack helper loads on demand, but NAT rewrites a helper's payload
// only once nf_nat_<h> is loaded, which nothing in nft requests. H.323's
// is IPv4 only.
var natHelperModules = map[string]string{"ftp": "nf_nat_ftp", "sip": "nf_nat_sip", "Q.931": "nf_nat_h323", "RAS": "nf_nat_h323"}

// HelperUsable reports why h can't run on flows of family v6, or "" when
// it can (D23): its conntrack helper must be available, which a scratch
// table's ct helper object tries (loading it on demand), and its NAT
// module loaded (sysModule, normally /sys/module).
func HelperUsable(c *nftables.Conn, sysModule string, h Helper, v6 bool) string {
	mod, ok := natHelperModules[h.Name]
	if !ok {
		return "unknown helper " + h.Name
	}
	if v6 && mod == "nf_nat_h323" {
		return "the kernel's H.323 NAT is IPv4 only"
	}
	l3 := uint16(unix.NFPROTO_IPV4)
	if v6 {
		l3 = unix.NFPROTO_IPV6
	}
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable + "_probe"}
	c.AddTable(t)
	c.AddObj(&nftables.NamedObj{Table: t, Name: "h", Type: nftables.ObjTypeCtHelper,
		Obj: &expr.CtHelper{Name: h.Name, L3Proto: l3, L4Proto: h.Proto}})
	err := c.Flush()
	c.DelTable(t)
	_ = c.Flush()
	if err != nil {
		return fmt.Sprintf("conntrack helper %s: %v", h.Name, err)
	}
	if _, err := os.Stat(filepath.Join(sysModule, mod)); err != nil {
		return mod + " is not loaded"
	}
	return ""
}
