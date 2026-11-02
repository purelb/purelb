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
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/xt"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Where a foreign rule sees packets, against the pod stage's classify
// chain (prerouting -90), which writes the mark field (D18).
const (
	// SeesBefore: ahead of classify (netdev and inet ingress, prerouting
	// below -90, bridge input): only the tag is there.
	SeesBefore uint8 = 1 << iota
	// SeesAfter: prerouting from -90 on, forward, postrouting: the slot.
	SeesAfter
	// SeesLocal: input and output, which egress's forwarded flows never
	// pass.
	SeesLocal
)

// allBits is a whole-mark set or exact compare.
const allBits = ^uint32(0)

// MarkUse is one foreign rule's use of the packet and conntrack marks:
// the bits it matches (Mark, Ct) and modifies (MarkSet, CtSet), and where
// it sees packets (Sees*, a regular chain those of every base chain
// reaching it).
type MarkUse struct {
	Where         string
	Sees          uint8
	Mark, MarkSet uint32
	Ct, CtSet     uint32
}

// Conflicts reports whether u conflicts with the mark field at shift
// (D18): it modifies the field's bits in the conntrack mark anywhere, or
// matches or modifies them after classify writes the slot; on a node that
// classifies by the tag (tag), also before.
func (u MarkUse) Conflicts(shift uint8, tag bool) bool {
	b := uint32(0xff) << shift
	switch {
	case u.Sees != 0 && u.CtSet&b != 0:
		return true
	case u.Sees&SeesAfter != 0 && (u.Mark|u.MarkSet|u.Ct)&b != 0:
		return true
	}
	return tag && u.Sees&SeesBefore != 0 && (u.Mark|u.MarkSet)&b != 0
}

// RulesetGen is the nf_tables ruleset's generation, which every change
// to it raises.
func RulesetGen() (uint32, error) {
	req := nl.NewNetlinkRequest(unix.NFNL_SUBSYS_NFTABLES<<8|unix.NFT_MSG_GETGEN, 0)
	req.AddData(&nl.Nfgenmsg{NfgenFamily: unix.AF_UNSPEC, Version: nl.NFNETLINK_V0})
	msgs, err := req.Execute(unix.NETLINK_NETFILTER, 0)
	if err != nil {
		return 0, fmt.Errorf("reading the nftables generation: %w", err)
	}
	for _, m := range msgs {
		if len(m) < 4 {
			continue
		}
		attrs, err := nl.ParseRouteAttr(m[4:])
		if err != nil {
			return 0, err
		}
		for _, a := range attrs {
			if a.Attr.Type&nl.NLA_TYPE_MASK == unix.NFTA_GEN_ID && len(a.Value) == 4 {
				return binary.BigEndian.Uint32(a.Value), nil
			}
		}
	}
	return 0, fmt.Errorf("the nftables generation reply has no id")
}

// ForeignMarks lists the rules outside PureLB's table that use the packet
// or conntrack mark, in every nf_tables family: native nft, kube-proxy's
// nftables mode, and iptables-nft's rules, its compat mark, MARK,
// connmark, CONNMARK and TPROXY expressions included.
func ForeignMarks(c *nftables.Conn) ([]MarkUse, error) {
	tables, err := c.ListTables()
	if err != nil {
		return nil, fmt.Errorf("listing the nftables tables: %w", err)
	}
	chains, err := c.ListChains()
	if err != nil {
		return nil, fmt.Errorf("listing the nftables chains: %w", err)
	}
	g := newRuleGraph()
	for _, t := range tables {
		if t.Family == nftables.TableFamilyINet && t.Name == NftTable {
			continue
		}
		// Maps a rule looks up may hold verdicts (the library reports a
		// verdict map's data type in its key type, so every map is read).
		vmaps := map[string]*nftables.Set{}
		if sets, err := c.GetSets(t); err == nil {
			for _, s := range sets {
				if s.IsMap {
					vmaps[s.Name] = s
				}
			}
		} else {
			return nil, fmt.Errorf("listing the sets of table %s: %w", t.Name, err)
		}
		elems := map[string][]string{}
		for _, ch := range chains {
			if ch.Table.Name != t.Name || ch.Table.Family != t.Family {
				continue
			}
			id := chainID(t, ch.Name)
			if ch.Hooknum != nil && ch.Priority != nil {
				g.base(id, nftSees(t.Family, *ch.Hooknum, int32(*ch.Priority)))
			}
			rules, err := c.GetRules(t, ch)
			if err != nil {
				return nil, fmt.Errorf("listing the rules of %s: %w", id, err)
			}
			for _, r := range rules {
				u := MarkUse{Where: id}
				for _, to := range nftRule(r.Exprs, &u) {
					g.edge(id, chainID(t, to))
				}
				for _, e := range r.Exprs {
					l, ok := e.(*expr.Lookup)
					if !ok || vmaps[l.SetName] == nil {
						continue
					}
					if _, ok := elems[l.SetName]; !ok {
						es, err := c.GetSetElements(vmaps[l.SetName])
						if err != nil {
							return nil, fmt.Errorf("listing map %s of table %s: %w", l.SetName, t.Name, err)
						}
						for _, el := range es {
							if to := verdictChain(el.Val); to != "" {
								elems[l.SetName] = append(elems[l.SetName], to)
							}
						}
						if elems[l.SetName] == nil {
							elems[l.SetName] = []string{}
						}
					}
					for _, to := range elems[l.SetName] {
						g.edge(id, chainID(t, to))
					}
				}
				g.use(u)
			}
		}
	}
	return g.uses(), nil
}

// verdictChain is the chain a map element's verdict jumps or goes to, ""
// for none. The library doesn't decode an element's verdict: it leaves
// NFTA_DATA_VERDICT's attributes (code, then NFTA_VERDICT_CHAIN) in Val.
func verdictChain(val []byte) string {
	attrs, err := nl.ParseRouteAttr(val)
	if err != nil {
		return ""
	}
	for _, a := range attrs {
		if a.Attr.Type&nl.NLA_TYPE_MASK == unix.NFTA_VERDICT_CHAIN {
			return cString(a.Value)
		}
	}
	return ""
}

func chainID(t *nftables.Table, chain string) string {
	return fmt.Sprintf("%s %s %s", familyName(t.Family), t.Name, chain)
}

func familyName(f nftables.TableFamily) string {
	switch f {
	case nftables.TableFamilyINet:
		return "inet"
	case nftables.TableFamilyIPv4:
		return "ip"
	case nftables.TableFamilyIPv6:
		return "ip6"
	case nftables.TableFamilyNetdev:
		return "netdev"
	case nftables.TableFamilyBridge:
		return "bridge"
	case nftables.TableFamilyARP:
		return "arp"
	}
	return fmt.Sprint(uint8(f))
}

// Hooks the library doesn't name: the bridge family's
// (linux/netfilter_bridge.h), and the inet family's ingress, which isn't
// the netdev family's (linux/netfilter.h).
const (
	brPreRouting nftables.ChainHook = 0 // NF_BR_PRE_ROUTING
	brLocalIn    nftables.ChainHook = 1 // NF_BR_LOCAL_IN
	inetIngress  nftables.ChainHook = 5 // NF_INET_INGRESS
)

// nftSees is where a base chain of family f at hook and prio sees
// packets.
func nftSees(f nftables.TableFamily, hook nftables.ChainHook, prio int32) uint8 {
	switch f {
	case nftables.TableFamilyNetdev:
		if hook == *nftables.ChainHookIngress {
			return SeesBefore
		}
		return 0
	case nftables.TableFamilyBridge:
		// Bridged frames; a pod's routed traffic enters the host through
		// the bridge's input.
		if hook == brPreRouting || hook == brLocalIn {
			return SeesBefore
		}
		return 0
	case nftables.TableFamilyARP:
		return 0
	}
	switch hook {
	case *nftables.ChainHookPrerouting:
		if prio < -90 {
			return SeesBefore
		}
		return SeesAfter // an equal priority runs in no set order
	case *nftables.ChainHookForward, *nftables.ChainHookPostrouting:
		return SeesAfter
	case *nftables.ChainHookInput, *nftables.ChainHookOutput:
		return SeesLocal
	case inetIngress:
		return SeesBefore
	}
	return 0
}

// regVal is what an nft register holds: a mark, as (mark & mask) ^ xor,
// or (kind 0) anything else.
type regVal struct {
	kind      uint8 // markPkt, markCt
	mask, xor uint32
}

const (
	markPkt uint8 = 1 + iota
	markCt
)

// nftRule adds the marks rule exprs uses to u and returns the chains it
// jumps or goes to. Registers are tracked through loads, bitwise and
// compares, as nft writes mark expressions; an expression not followed
// forgets them all, so a later set counts as overwriting the mark.
func nftRule(exprs []expr.Any, u *MarkUse) []string {
	regs := map[uint32]regVal{}
	used := func(r uint32) {
		switch v := regs[reg32(r)]; v.kind {
		case markPkt:
			u.Mark |= v.mask
		case markCt:
			u.Ct |= v.mask
		}
	}
	set := func(r uint32, kind uint8) uint32 {
		if v := regs[reg32(r)]; v.kind == kind {
			return ^v.mask | v.xor
		}
		return allBits
	}
	// last is the register last written: the library doesn't parse a ct
	// set's source register, which nft fills just before.
	var to []string
	var last uint32
	for _, e := range exprs {
		switch e := e.(type) {
		case *expr.Meta:
			switch {
			case e.Key == expr.MetaKeyMARK && e.SourceRegister:
				u.MarkSet |= set(e.Register, markPkt)
			case e.Key == expr.MetaKeyMARK:
				regs[reg32(e.Register)] = regVal{kind: markPkt, mask: allBits}
				last = e.Register
			case !e.SourceRegister:
				delete(regs, reg32(e.Register))
				last = e.Register
			}
		case *expr.Ct:
			switch {
			case e.Key == expr.CtKeyMARK && (e.SourceRegister || e.Register == 0):
				r := e.Register
				if !e.SourceRegister {
					r = last
				}
				u.CtSet |= set(r, markCt)
			case e.Key == expr.CtKeyMARK:
				regs[reg32(e.Register)] = regVal{kind: markCt, mask: allBits}
				last = e.Register
			case !e.SourceRegister && e.Register != 0:
				delete(regs, reg32(e.Register))
				last = e.Register
			}
		case *expr.Bitwise:
			v := regs[reg32(e.SourceRegister)]
			last = e.DestRegister
			switch {
			case v.kind == 0:
				delete(regs, reg32(e.DestRegister))
			case len(e.Mask) == 4 && len(e.Xor) == 4:
				m, x := binary.NativeEndian.Uint32(e.Mask), binary.NativeEndian.Uint32(e.Xor)
				regs[reg32(e.DestRegister)] = regVal{kind: v.kind, mask: v.mask & m, xor: (v.xor & m) ^ x}
			default:
				// A shift, or a wider value: every bit taken.
				used(e.SourceRegister)
				regs[reg32(e.DestRegister)] = regVal{kind: v.kind, mask: allBits, xor: allBits}
			}
		case *expr.Cmp:
			// An exact compare of the whole mark with C counts C's bits
			// (user, 2026-10-31): flannel's "meta mark 0x4000"; of the
			// mark xor X, those of C ^ X.
			v := regs[reg32(e.Register)]
			bits := v.mask
			if v.mask == allBits && len(e.Data) == 4 {
				bits = binary.NativeEndian.Uint32(e.Data) ^ v.xor
			}
			switch v.kind {
			case markPkt:
				u.Mark |= bits
			case markCt:
				u.Ct |= bits
			}
		case *expr.Range:
			used(e.Register)
		case *expr.Lookup:
			used(e.SourceRegister)
			if e.IsDestRegSet {
				delete(regs, reg32(e.DestRegister))
			}
		case *expr.Immediate:
			delete(regs, reg32(e.Register))
			last = e.Register
		case *expr.Verdict:
			if (e.Kind == expr.VerdictJump || e.Kind == expr.VerdictGoto) && e.Chain != "" {
				to = append(to, e.Chain)
			}
		case *expr.Match:
			xtUse(e.Name, e.Rev, false, xtBytes(e.Info), u)
		case *expr.Target:
			xtUse(e.Name, e.Rev, true, xtBytes(e.Info), u)
		case *expr.Counter, *expr.Log, *expr.Limit, *expr.Objref:
		default:
			clear(regs)
		}
	}
	return to
}

// reg32 names a register by its first 32-bit register: nft's 16-byte
// registers 1-4 are the 32-bit ones 8-11, 12-15, and so on.
func reg32(r uint32) uint32 {
	if r >= 1 && r <= 4 {
		return 8 + (r-1)*4
	}
	return r
}

func xtBytes(i xt.InfoAny) []byte {
	if b, ok := i.(*xt.Unknown); ok {
		return *b
	}
	return nil
}

// The CONNMARK target's modes (linux/netfilter/xt_connmark.h).
const (
	connmarkSet = iota
	connmarkSave
	connmarkRestore
)

// xtUse adds the marks an x_tables match or target (target) named name, at
// revision rev, with info b, uses to u: from iptables-nft's compat
// expressions or legacy iptables' entries. A revision without 32-bit
// fields counts as every bit.
func xtUse(name string, rev uint32, target bool, b []byte, u *MarkUse) {
	u32 := func(off int) (uint32, bool) {
		if len(b) < off+4 {
			return 0, false
		}
		return binary.NativeEndian.Uint32(b[off:]), true
	}
	switch {
	case !target && name == "mark", !target && name == "connmark":
		// xt_mark_mtinfo1, xt_connmark_mtinfo1: mark, mask, invert. No
		// mask (all of it) is an exact compare: the mark's bits count.
		v, _ := u32(0)
		m, ok := u32(4)
		switch {
		case rev < 1 || !ok:
			m = allBits
		case m == allBits:
			m = v
		}
		if name == "mark" {
			u.Mark |= m
		} else {
			u.Ct |= m
		}
	case target && name == "MARK":
		// xt_mark_tginfo2: mark, mask; the mask's bits cleared, then mark
		// xored in.
		v, ok1 := u32(0)
		m, ok2 := u32(4)
		if rev < 2 || !ok1 || !ok2 {
			u.MarkSet = allBits
			return
		}
		u.MarkSet |= m | v
	case target && name == "TPROXY":
		// xt_tproxy_target_info(_v1): mark_mask, mark_value first.
		m, ok1 := u32(0)
		v, ok2 := u32(4)
		if !ok1 || !ok2 {
			u.MarkSet = allBits
			return
		}
		u.MarkSet |= m | v
	case target && name == "CONNMARK":
		// xt_connmark_tginfo1: ctmark, ctmask, nfmask, mode;
		// xt_connmark_tginfo2 adds shift_dir and shift_bits before mode.
		ctmark, ok1 := u32(0)
		ctmask, ok2 := u32(4)
		nfmask, ok3 := u32(8)
		modeAt, shifted := 12, false
		if rev == 2 {
			modeAt, shifted = 14, len(b) > 13 && b[13] != 0
		}
		if rev < 1 || !ok1 || !ok2 || !ok3 || len(b) <= modeAt || shifted {
			u.MarkSet, u.CtSet, u.Mark, u.Ct = allBits, allBits, allBits, allBits
			return
		}
		switch b[modeAt] {
		case connmarkSet:
			u.CtSet |= ctmask | ctmark
		case connmarkSave:
			u.CtSet |= ctmask
			u.Mark |= nfmask
		case connmarkRestore:
			u.MarkSet |= nfmask
			u.Ct |= ctmask
		}
	}
}

// ruleGraph collects rules' mark uses and the chains reaching them.
type ruleGraph struct {
	sees  map[string]uint8
	edges map[string][]string
	rules []MarkUse
}

func newRuleGraph() *ruleGraph {
	return &ruleGraph{sees: map[string]uint8{}, edges: map[string][]string{}}
}

func (g *ruleGraph) base(id string, sees uint8) { g.sees[id] |= sees }
func (g *ruleGraph) edge(from, to string)       { g.edges[from] = append(g.edges[from], to) }

func (g *ruleGraph) use(u MarkUse) {
	if u.Mark|u.MarkSet|u.Ct|u.CtSet != 0 {
		g.rules = append(g.rules, u)
	}
}

// uses are the rules that use a mark, each with where its chain sees
// packets: the base chains' places, carried along every jump and goto.
func (g *ruleGraph) uses() []MarkUse {
	queue := make([]string, 0, len(g.sees))
	for id := range g.sees {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, to := range g.edges[id] {
			if g.sees[to]|g.sees[id] != g.sees[to] {
				g.sees[to] |= g.sees[id]
				queue = append(queue, to)
			}
		}
	}
	out := make([]MarkUse, 0, len(g.rules))
	for _, u := range g.rules {
		u.Sees = g.sees[u.Where]
		out = append(out, u)
	}
	return out
}
