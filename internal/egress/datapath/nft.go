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
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// NftTable is egress's nftables table, inet family (design 6.11).
const NftTable = "purelb_egress"

// Hardening is what the host hardening chains are built from (design
// 6.12).
type Hardening struct {
	TunnelPort uint16
	// Nodes are every node's underlay addresses, this node's included.
	Nodes []netip.Addr
	// Egress are the egress addresses; ServicePorts the Service ports of
	// those shared with a Service, which stay open to new connections.
	Egress       []netip.Addr
	ServicePorts []ServicePort
}

// ServicePort is one Service port of a shared egress address.
type ServicePort struct {
	Addr  netip.Addr
	Proto uint8
	Port  uint16
}

// The concatenated key types of the Service-port sets.
var (
	svcPortType4 = nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetProto, nftables.TypeInetService)
	svcPortType6 = nftables.MustConcatSetType(nftables.TypeIP6Addr, nftables.TypeInetProto, nftables.TypeInetService)
)

// SyncHardening replaces egress's table with h's chains in one
// transaction, so the host is never without them and never sees half a
// table:
//
//	announce_in  prerouting -150 (D32): new or invalid packets to an
//	             egress address, other than to a Service port of a shared
//	             one, and from the tunnel are dropped, every protocol
//	tun_fwd      forward (D20): no forwarded packet reaches a node's
//	             tunnel port
//	tun_in       input (D20): the tunnel port accepts nodes only
//	tun_raw,     raw (prerouting, output): node-to-node tunnel packets
//	tun_raw_out  skip conntrack
func SyncHardening(c *nftables.Conn, h Hardening) error {
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable}
	// Adding first makes the delete valid when the table doesn't exist.
	c.AddTable(t)
	c.DelTable(t)
	c.AddTable(t)

	sets := map[string]*nftables.Set{}
	elems := hardeningElems(h)
	for _, s := range hardeningSets {
		set := &nftables.Set{Table: t, Name: s.name, KeyType: s.typ, Concatenation: s.concat}
		if err := c.AddSet(set, elems[s.name]); err != nil {
			return err
		}
		sets[s.name] = set
	}

	accept := nftables.ChainPolicyAccept
	chain := func(name string, hook *nftables.ChainHook, prio *nftables.ChainPriority) *nftables.Chain {
		return c.AddChain(&nftables.Chain{Name: name, Table: t, Type: nftables.ChainTypeFilter,
			Hooknum: hook, Priority: prio, Policy: &accept})
	}
	rule := func(ch *nftables.Chain, e ...expr.Any) {
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: e})
	}
	port := h.TunnelPort

	in := chain("announce_in", nftables.ChainHookPrerouting, nftables.ChainPriorityRef(-150))
	for _, f := range nftFamilies {
		rule(in, cat(f.match(), f.daddr(1), l4proto(f.next(1)), dport(f.next(1)+1),
			lookup(sets["svcports"+f.n], false), verdict(expr.VerdictAccept))...)
	}
	for _, f := range nftFamilies {
		rule(in, cat(newOrInvalid(), f.match(), f.daddr(1), lookup(sets["egress"+f.n], false),
			verdict(expr.VerdictDrop))...)
	}
	rule(in, cat(newOrInvalid(), iifname(TunnelName), verdict(expr.VerdictDrop))...)

	fwd := chain("tun_fwd", nftables.ChainHookForward, nftables.ChainPriorityFilter)
	tin := chain("tun_in", nftables.ChainHookInput, nftables.ChainPriorityFilter)
	raw := chain("tun_raw", nftables.ChainHookPrerouting, nftables.ChainPriorityRaw)
	rawOut := chain("tun_raw_out", nftables.ChainHookOutput, nftables.ChainPriorityRaw)
	for _, f := range nftFamilies {
		nodes := sets["nodes"+f.n]
		rule(fwd, cat(f.match(), udpPort(port), f.daddr(1), lookup(nodes, false), verdict(expr.VerdictDrop))...)
		rule(tin, cat(f.match(), udpPort(port), f.saddr(1), lookup(nodes, true), verdict(expr.VerdictDrop))...)
		rule(raw, cat(f.match(), udpPort(port), f.saddr(1), lookup(nodes, false), []expr.Any{&expr.Notrack{}})...)
		rule(rawOut, cat(f.match(), udpPort(port), f.daddr(1), lookup(nodes, false), []expr.Any{&expr.Notrack{}})...)
	}
	return c.Flush()
}

// The hardening table's sets, and its chains with their rule counts.
var (
	hardeningSets = []struct {
		name   string
		typ    nftables.SetDatatype
		concat bool
	}{
		{"egress4", nftables.TypeIPAddr, false},
		{"egress6", nftables.TypeIP6Addr, false},
		{"nodes4", nftables.TypeIPAddr, false},
		{"nodes6", nftables.TypeIP6Addr, false},
		{"svcports4", svcPortType4, true},
		{"svcports6", svcPortType6, true},
	}
	hardeningChains = map[string]int{"announce_in": 5, "tun_fwd": 2, "tun_in": 2, "tun_raw": 2, "tun_raw_out": 2}
)

// hardeningElems is each hardening set's elements for h.
func hardeningElems(h Hardening) map[string][]nftables.SetElement {
	elems := map[string][]nftables.SetElement{}
	add := func(set string, a netip.Addr, key []byte) {
		if a.Is4() {
			set += "4"
		} else {
			set += "6"
		}
		elems[set] = append(elems[set], nftables.SetElement{Key: key})
	}
	for _, a := range h.Egress {
		add("egress", a, a.AsSlice())
	}
	for _, a := range h.Nodes {
		add("nodes", a, a.AsSlice())
	}
	for _, p := range h.ServicePorts {
		// Each field of a concatenated key is padded to 4 bytes.
		add("svcports", p.Addr, append(p.Addr.AsSlice(), p.Proto, 0, 0, 0, byte(p.Port>>8), byte(p.Port), 0, 0))
	}
	return elems
}

// HardeningIntact reports whether the table SyncHardening writes for h is
// in the kernel as written: every chain with its rules, every set with
// exactly h's elements. Something else can remove it: another firewall
// flushing the ruleset, for one.
func HardeningIntact(c *nftables.Conn, h Hardening) (bool, error) {
	t, err := c.ListTableOfFamily(NftTable, nftables.TableFamilyINet)
	if err != nil || t == nil {
		// A missing table is an error from the kernel (ENOENT).
		return false, nil
	}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		return false, err
	}
	seen := 0
	for _, ch := range chains {
		if ch.Table.Name != NftTable {
			continue
		}
		want, ok := hardeningChains[ch.Name]
		if !ok {
			return false, nil
		}
		rules, err := c.GetRules(t, ch)
		if err != nil {
			return false, err
		}
		if len(rules) != want {
			return false, nil
		}
		seen++
	}
	if seen != len(hardeningChains) {
		return false, nil
	}
	elems := hardeningElems(h)
	for _, s := range hardeningSets {
		set, err := c.GetSetByName(t, s.name)
		if err != nil {
			return false, nil
		}
		have, err := c.GetSetElements(set)
		if err != nil {
			return false, err
		}
		if !sameKeys(have, elems[s.name]) {
			return false, nil
		}
	}
	return true, nil
}

func sameKeys(a, b []nftables.SetElement) bool {
	if len(a) != len(b) {
		return false
	}
	keys := map[string]int{}
	for _, e := range a {
		keys[string(e.Key)]++
	}
	for _, e := range b {
		if keys[string(e.Key)]--; keys[string(e.Key)] < 0 {
			return false
		}
	}
	return true
}

// nftFamily builds an inet table's per-family expressions.
type nftFamily struct {
	n       string // set name suffix
	nfproto byte
	addrLen uint32
	saddrAt uint32 // offsets in the network header
	daddrAt uint32
}

var nftFamilies = []nftFamily{
	{n: "4", nfproto: unix.NFPROTO_IPV4, addrLen: 4, saddrAt: 12, daddrAt: 16},
	{n: "6", nfproto: unix.NFPROTO_IPV6, addrLen: 16, saddrAt: 8, daddrAt: 24},
}

func (f nftFamily) match() []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{f.nfproto}},
	}
}

func (f nftFamily) saddr(reg uint32) []expr.Any {
	return []expr.Any{&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseNetworkHeader, Offset: f.saddrAt, Len: f.addrLen}}
}

func (f nftFamily) daddr(reg uint32) []expr.Any {
	return []expr.Any{&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseNetworkHeader, Offset: f.daddrAt, Len: f.addrLen}}
}

// next is the 32-bit register after an address loaded into reg: a
// concatenated key fills consecutive 32-bit registers, and 16-byte
// register 1 is 32-bit registers 8 to 11.
func (f nftFamily) next(reg uint32) uint32 {
	return 8 + (reg-1)*4 + f.addrLen/4
}

func l4proto(reg uint32) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: reg}}
}

func dport(reg uint32) []expr.Any {
	return []expr.Any{&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}}
}

func udpPort(port uint16) []expr.Any {
	p := make([]byte, 2)
	binary.BigEndian.PutUint16(p, port)
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: p},
	}
}

func newOrInvalid() []expr.Any {
	mask := make([]byte, 4)
	binary.NativeEndian.PutUint32(mask, expr.CtStateBitNEW|expr.CtStateBitINVALID)
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
	}
}

func iifname(name string) []expr.Any {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b},
	}
}

func lookup(s *nftables.Set, invert bool) []expr.Any {
	return []expr.Any{&expr.Lookup{SourceRegister: 1, SetName: s.Name, SetID: s.ID, Invert: invert}}
}

func verdict(k expr.VerdictKind) []expr.Any {
	return []expr.Any{&expr.Verdict{Kind: k}}
}

func cat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
