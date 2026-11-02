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
	"cmp"
	"encoding/binary"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// NftTable is egress's nftables table, inet family (design 6.11).
const NftTable = "purelb_egress"

// nftBuffer is the netlink socket buffer the table is written through: a
// full table doesn't fit the defaults, 1 MiB fits 127 slots in both
// families (RESULTS, "The nft table does not fit one default netlink
// transaction").
const nftBuffer = 1 << 20

// NftConn opens an nftables connection whose socket buffers fit the whole
// table in one transaction (SO_RCVBUFFORCE / SO_SNDBUFFORCE, under
// CAP_NET_ADMIN).
func NftConn(opts ...nftables.ConnOption) (*nftables.Conn, error) {
	return nftables.New(append(opts, nftables.WithSockOptions(func(c *netlink.Conn) error {
		if err := c.SetReadBuffer(nftBuffer); err != nil {
			return err
		}
		return c.SetWriteBuffer(nftBuffer)
	}))...)
}

// Table is egress's whole table: the host hardening chains on every egress
// node, and the pod-node stage where it runs.
type Table struct {
	Hardening Hardening
	// Pods is the pod-node stage; nil: none.
	Pods *PodStage
}

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

// SyncTable replaces egress's table with t in one transaction, so the host
// is never without the hardening chains and never sees half a table.
func SyncTable(c *nftables.Conn, t Table) error {
	if _, err := build(c, t); err != nil {
		return err
	}
	return c.Flush()
}

// TableIntact reports whether the table SyncTable writes for t is in the
// kernel as written: every chain with its rule count, every set with
// exactly t's elements, every object. Something else can remove part of it:
// another firewall flushing the ruleset, for one.
func TableIntact(c *nftables.Conn, t Table) (bool, error) {
	// Built on a connection that is never flushed: only its shape is used.
	want, err := build(&nftables.Conn{}, t)
	if err != nil {
		return false, err
	}
	tbl, err := c.ListTableOfFamily(NftTable, nftables.TableFamilyINet)
	if err != nil || tbl == nil {
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
		n, ok := want.chains[ch.Name]
		if !ok {
			return false, nil
		}
		rules, err := c.GetRules(tbl, ch)
		if err != nil {
			return false, err
		}
		if len(rules) != n {
			return false, nil
		}
		seen++
	}
	if seen != len(want.chains) {
		return false, nil
	}
	sets, err := c.GetSets(tbl)
	if err != nil {
		return false, err
	}
	named := 0
	for _, s := range sets {
		if s.Anonymous {
			continue
		}
		elems, ok := want.sets[s.Name]
		if !ok {
			return false, nil
		}
		have, err := c.GetSetElements(s)
		if err != nil {
			return false, err
		}
		if !sameElements(have, elems) {
			return false, nil
		}
		named++
	}
	if named != len(want.sets) {
		return false, nil
	}
	objs, err := c.GetNamedObjects(tbl)
	if err != nil {
		return false, err
	}
	have := map[string]bool{}
	for _, o := range objs {
		if n, ok := o.(*nftables.NamedObj); ok {
			have[n.Name] = true
		}
	}
	for _, name := range want.objs {
		if !have[name] {
			return false, nil
		}
	}
	return len(have) == len(want.objs), nil
}

// shape is what a table holds: each chain's rule count, each named set's
// elements, each object's name.
type shape struct {
	chains map[string]int
	sets   map[string][]nftables.SetElement
	objs   []string
}

// builder adds a table to a connection and records its shape.
type builder struct {
	c     *nftables.Conn
	t     *nftables.Table
	sets  map[string]*nftables.Set
	shape shape
}

// build adds t to c, replacing what the kernel holds, and returns its
// shape.
func build(c *nftables.Conn, t Table) (shape, error) {
	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable}
	// Adding first makes the delete valid when the table doesn't exist.
	c.AddTable(tbl)
	c.DelTable(tbl)
	c.AddTable(tbl)
	b := &builder{c: c, t: tbl, sets: map[string]*nftables.Set{},
		shape: shape{chains: map[string]int{}, sets: map[string][]nftables.SetElement{}}}
	if err := b.hardening(t.Hardening); err != nil {
		return shape{}, err
	}
	if t.Pods != nil {
		if err := b.podStage(*t.Pods); err != nil {
			return shape{}, err
		}
	}
	return b.shape, nil
}

// baseChain is a chain's hook, priority and type; regular chains have none.
type baseChain struct {
	hook *nftables.ChainHook
	prio *nftables.ChainPriority
	typ  nftables.ChainType
}

func filterHook(hook *nftables.ChainHook, prio int32) *baseChain {
	return &baseChain{hook: hook, prio: nftables.ChainPriorityRef(nftables.ChainPriority(prio)), typ: nftables.ChainTypeFilter}
}

func (b *builder) chain(name string, base *baseChain) *nftables.Chain {
	ch := &nftables.Chain{Name: name, Table: b.t}
	if base != nil {
		accept := nftables.ChainPolicyAccept
		ch.Hooknum, ch.Priority, ch.Type, ch.Policy = base.hook, base.prio, base.typ, &accept
	}
	b.shape.chains[name] = 0
	return b.c.AddChain(ch)
}

func (b *builder) rule(ch *nftables.Chain, e ...expr.Any) {
	b.c.AddRule(&nftables.Rule{Table: b.t, Chain: ch, Exprs: e})
	b.shape.chains[ch.Name]++
}

// set adds a named set (or map) with its elements.
func (b *builder) set(s *nftables.Set, elems []nftables.SetElement) error {
	s.Table = b.t
	if err := b.c.AddSet(s, elems); err != nil {
		return fmt.Errorf("set %s: %w", s.Name, err)
	}
	b.sets[s.Name] = s
	b.shape.sets[s.Name] = elems
	return nil
}

func (b *builder) obj(o *nftables.NamedObj) {
	o.Table = b.t
	b.c.AddObj(o)
	b.shape.objs = append(b.shape.objs, o.Name)
}

// lookup matches register 1 against the named set.
func (b *builder) lookup(name string, invert bool) []expr.Any {
	s := b.sets[name]
	return []expr.Any{&expr.Lookup{SourceRegister: 1, SetName: s.Name, SetID: s.ID, Invert: invert}}
}

// vmap jumps or goes to the verdict register 1 maps to in the named map.
func (b *builder) vmap(name string) []expr.Any {
	s := b.sets[name]
	return []expr.Any{&expr.Lookup{SourceRegister: 1, SetName: s.Name, SetID: s.ID, IsDestRegSet: true}}
}

// hardening adds the host hardening chains (design 6.12):
//
//	announce_in  prerouting -150 (D32): new or invalid packets to an
//	             egress address, other than to a Service port of a shared
//	             one, and from the tunnel are dropped, every protocol
//	out_src      output (D20): the host never sources an egress address,
//	             but for its own NS/NA and replies
//	tun_fwd      forward (D20): no forwarded packet reaches a node's
//	             tunnel port
//	tun_in       input (D20): the tunnel port accepts nodes only
//	tun_raw,     raw (prerouting, output): node-to-node tunnel packets
//	tun_raw_out  skip conntrack
func (b *builder) hardening(h Hardening) error {
	elems := hardeningElems(h)
	for _, s := range []struct {
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
	} {
		if err := b.set(&nftables.Set{Name: s.name, KeyType: s.typ, Concatenation: s.concat}, elems[s.name]); err != nil {
			return err
		}
	}
	port := h.TunnelPort

	in := b.chain("announce_in", filterHook(nftables.ChainHookPrerouting, -150))
	for _, f := range nftFamilies {
		b.rule(in, cat(f.match(), f.daddr(1), l4proto(f.next(1)), dport(f.next(1)+1),
			b.lookup("svcports"+f.n, false), verdict(expr.VerdictAccept))...)
	}
	for _, f := range nftFamilies {
		b.rule(in, cat(newOrInvalid(), f.match(), f.daddr(1), b.lookup("egress"+f.n, false),
			verdict(expr.VerdictDrop))...)
	}
	b.rule(in, cat(newOrInvalid(), iifname(TunnelName), verdict(expr.VerdictDrop))...)

	out := b.chain("out_src", filterHook(nftables.ChainHookOutput, 0))
	for _, typ := range []byte{135, 136} { // the node's own NS and NA
		b.rule(out, cat(nftFamilies[1].match(), l4protoIs(unix.IPPROTO_ICMPV6), icmpType(typ),
			hopLimit255(), verdict(expr.VerdictAccept))...)
	}
	b.rule(out, cat(ctDirection(1), verdict(expr.VerdictAccept))...)
	for _, f := range nftFamilies {
		b.rule(out, cat(f.match(), f.saddr(1), b.lookup("egress"+f.n, false), verdict(expr.VerdictDrop))...)
	}

	fwd := b.chain("tun_fwd", filterHook(nftables.ChainHookForward, 0))
	tin := b.chain("tun_in", filterHook(nftables.ChainHookInput, 0))
	raw := b.chain("tun_raw", filterHook(nftables.ChainHookPrerouting, -300))
	rawOut := b.chain("tun_raw_out", filterHook(nftables.ChainHookOutput, -300))
	for _, f := range nftFamilies {
		nodes := "nodes" + f.n
		b.rule(fwd, cat(f.match(), udpPort(port), f.daddr(1), b.lookup(nodes, false), verdict(expr.VerdictDrop))...)
		b.rule(tin, cat(f.match(), udpPort(port), f.saddr(1), b.lookup(nodes, true), verdict(expr.VerdictDrop))...)
		b.rule(raw, cat(f.match(), udpPort(port), f.saddr(1), b.lookup(nodes, false), []expr.Any{&expr.Notrack{}})...)
		b.rule(rawOut, cat(f.match(), udpPort(port), f.daddr(1), b.lookup(nodes, false), []expr.Any{&expr.Notrack{}})...)
	}
	return nil
}

// hardeningElems is each hardening set's elements for h.
func hardeningElems(h Hardening) map[string][]nftables.SetElement {
	elems := map[string][]nftables.SetElement{}
	add := func(set string, a netip.Addr, key []byte) {
		set += famSuffix(a)
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

func famSuffix(a netip.Addr) string {
	if a.Is4() {
		return "4"
	}
	return "6"
}

// sameElements reports whether two element lists hold the same elements,
// verdicts included, in any order.
func sameElements(a, b []nftables.SetElement) bool {
	if len(a) != len(b) {
		return false
	}
	keys := map[string]int{}
	for _, e := range a {
		keys[elementKey(e)]++
	}
	for _, e := range b {
		if keys[elementKey(e)]--; keys[elementKey(e)] < 0 {
			return false
		}
	}
	return true
}

func elementKey(e nftables.SetElement) string {
	k := string(e.Key)
	if e.IntervalEnd {
		k += "|end"
	}
	switch {
	case e.VerdictData != nil:
		k += fmt.Sprintf("|%d:%s", e.VerdictData.Kind, e.VerdictData.Chain)
	case len(e.Val) > 0:
		// A map's verdict as the kernel lists it: its code and chain as
		// netlink attributes.
		code, chain := int32(0), ""
		if ad, err := netlink.NewAttributeDecoder(e.Val); err == nil {
			ad.ByteOrder = binary.BigEndian
			for ad.Next() {
				switch ad.Type() {
				case unix.NFTA_VERDICT_CODE:
					code = int32(ad.Uint32())
				case unix.NFTA_VERDICT_CHAIN:
					chain = ad.String()
				}
			}
		}
		k += fmt.Sprintf("|%d:%s", code, chain)
	}
	return k
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

// familyOf is a's nftFamily.
func familyOf(a netip.Addr) nftFamily {
	if a.Is4() {
		return nftFamilies[0]
	}
	return nftFamilies[1]
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

func l4protoIs(p byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{p}},
	}
}

func dport(reg uint32) []expr.Any {
	return []expr.Any{&expr.Payload{DestRegister: reg, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}}
}

func udpPort(port uint16) []expr.Any {
	return cat(l4protoIs(unix.IPPROTO_UDP), dport(1), []expr.Any{&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(port)}})
}

// icmpType matches the ICMP or ICMPv6 type.
func icmpType(t byte) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{t}},
	}
}

func hopLimit255() []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 7, Len: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{255}},
	}
}

// ctState matches any of the conntrack states in bits.
func ctState(bits uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ne32(bits), Xor: ne32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ne32(0)},
	}
}

func newOrInvalid() []expr.Any {
	return ctState(expr.CtStateBitNEW | expr.CtStateBitINVALID)
}

// ctDirection matches the packet's conntrack direction: 0 original, 1
// reply.
func ctDirection(dir byte) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeyDIRECTION},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{dir}},
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

func verdict(k expr.VerdictKind) []expr.Any {
	return []expr.Any{&expr.Verdict{Kind: k}}
}

func goTo(chain string) []expr.Any {
	return []expr.Any{&expr.Verdict{Kind: expr.VerdictGoto, Chain: chain}}
}

func jump(chain string) []expr.Any {
	return []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: chain}}
}

// ne32 is v as nft compares a 32-bit mark or state: in host order.
func ne32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func cat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// sortedKeys is m's keys in order.
func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}
