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
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/xt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ne(v uint32) []byte { return binaryutil.NativeEndian.PutUint32(v) }

// loadMark, setMark: nft's register code for a mark expression.
func loadMark(ct bool) expr.Any {
	if ct {
		return &expr.Ct{Key: expr.CtKeyMARK, Register: 1}
	}
	return &expr.Meta{Key: expr.MetaKeyMARK, Register: 1}
}

func setMark(ct bool) expr.Any {
	if ct {
		return &expr.Ct{Key: expr.CtKeyMARK, Register: 1, SourceRegister: true}
	}
	return &expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true}
}

func bitwise(mask, xor uint32) expr.Any {
	return &expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ne(mask), Xor: ne(xor)}
}

func xtData(vs ...uint32) *xt.Unknown {
	var b []byte
	for _, v := range vs {
		b = binary.NativeEndian.AppendUint32(b, v)
	}
	u := xt.Unknown(b)
	return &u
}

func TestNftRuleMarks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exprs []expr.Any
		want  MarkUse
		to    []string
	}{
		{"kube-proxy masquerade match", []expr.Any{loadMark(false), bitwise(0x4000, 0),
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(0x4000)}, &expr.Verdict{Kind: expr.VerdictReturn}},
			MarkUse{Mark: 0x4000}, nil},
		{"or a bit in", []expr.Any{loadMark(false), bitwise(^uint32(0x4000), 0x4000), setMark(false)},
			MarkUse{MarkSet: 0x4000}, nil},
		{"whole set from a constant", []expr.Any{&expr.Immediate{Register: 1, Data: ne(0x200)}, setMark(false)},
			MarkUse{MarkSet: allBits}, nil},
		{"Calico eBPF's clear", []expr.Any{loadMark(false), bitwise(0x03000000, 0x03000000), setMark(false)},
			MarkUse{MarkSet: allBits}, nil},
		// An exact compare counts the constant's bits (user, 2026-10-31).
		{"exact compare", []expr.Any{loadMark(false), &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(0x10200)}},
			MarkUse{Mark: 0x10200}, nil},
		{"flannel's exact compare", []expr.Any{loadMark(false), &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(0x4000)},
			&expr.Verdict{Kind: expr.VerdictReturn}}, MarkUse{Mark: 0x4000}, nil},
		{"exact ct compare, not equal", []expr.Any{loadMark(true), &expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ne(0x5)}},
			MarkUse{Ct: 0x5}, nil},
		{"exact compare of the mark xored", []expr.Any{loadMark(false), bitwise(allBits, 0x1),
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(0x4001)}}, MarkUse{Mark: 0x4000}, nil},
		{"a range over the whole mark", []expr.Any{loadMark(false), &expr.Range{Op: expr.CmpOpEq, Register: 1,
			FromData: ne(1), ToData: ne(9)}}, MarkUse{Mark: allBits}, nil},
		{"an exact compare, then the mark set from the same register", []expr.Any{loadMark(false),
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(0x4000)}, setMark(false)},
			MarkUse{Mark: 0x4000}, nil},
		{"ct mark from the packet's", []expr.Any{loadMark(false), bitwise(0xff, 0), setMark(true)},
			MarkUse{CtSet: allBits}, nil},
		{"ct mark kept outside a byte", []expr.Any{loadMark(true), bitwise(^uint32(0xff00), 0x0100), setMark(true)},
			MarkUse{CtSet: 0xff00}, nil},
		// As the library reads a ct set back: no source register.
		{"ct set as read back, from a masked ct mark", []expr.Any{loadMark(true), bitwise(^uint32(0xff00), 0x0100),
			&expr.Ct{Key: expr.CtKeyMARK}}, MarkUse{CtSet: 0xff00}, nil},
		{"ct set as read back, from a constant", []expr.Any{&expr.Immediate{Register: 1, Data: ne(5)},
			&expr.Ct{Key: expr.CtKeyMARK}}, MarkUse{CtSet: allBits}, nil},
		{"chained bitwise: a kept bit flipped", []expr.Any{loadMark(false), bitwise(^uint32(0xff00), 0x1),
			bitwise(allBits, 0), setMark(false)}, MarkUse{MarkSet: 0xff01}, nil},
		{"32-bit register", []expr.Any{&expr.Meta{Key: expr.MetaKeyMARK, Register: 8},
			&expr.Bitwise{SourceRegister: 8, DestRegister: 8, Len: 4, Mask: ne(0xf0), Xor: ne(0)},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 8, Data: ne(0)}}, MarkUse{Mark: 0xf0}, nil},
		{"map lookup on a masked mark, jump", []expr.Any{loadMark(false), bitwise(0xff000000, 0),
			&expr.Lookup{SourceRegister: 1, SetName: "m"}, &expr.Verdict{Kind: expr.VerdictJump, Chain: "c"}},
			MarkUse{Mark: 0xff000000}, []string{"c"}},
		{"an unknown expression forgets the register", []expr.Any{loadMark(false), &expr.Payload{DestRegister: 1, Len: 4},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(1)}}, MarkUse{}, nil},
		{"compat mark match", []expr.Any{&expr.Match{Name: "mark", Rev: 1, Info: xtData(0x10, 0xf0, 0)}},
			MarkUse{Mark: 0xf0}, nil},
		{"compat mark match, no mask", []expr.Any{&expr.Match{Name: "mark", Rev: 1, Info: xtData(0x4000, allBits, 0)}},
			MarkUse{Mark: 0x4000}, nil},
		{"compat MARK --set-xmark", []expr.Any{&expr.Target{Name: "MARK", Rev: 2, Info: xtData(0x100, 0x300)}},
			MarkUse{MarkSet: 0x300}, nil},
		{"compat MARK rev 0", []expr.Any{&expr.Target{Name: "MARK", Rev: 0, Info: xtData(1, 0)}},
			MarkUse{MarkSet: allBits}, nil},
		{"compat TPROXY", []expr.Any{&expr.Target{Name: "TPROXY", Rev: 1, Info: xtData(0xffffffff, 0x200)}},
			MarkUse{MarkSet: allBits}, nil},
		{"compat connmark match", []expr.Any{&expr.Match{Name: "connmark", Rev: 1, Info: xtData(1, 0xff, 0)}},
			MarkUse{Ct: 0xff}, nil},
		{"compat CONNMARK save", []expr.Any{&expr.Target{Name: "CONNMARK", Rev: 1,
			Info: xtData(0, 0xff00, 0xff, connmarkSave)}}, MarkUse{Mark: 0xff, CtSet: 0xff00}, nil},
		{"compat CONNMARK restore", []expr.Any{&expr.Target{Name: "CONNMARK", Rev: 1,
			Info: xtData(0, 0xff00, 0xff, connmarkRestore)}}, MarkUse{MarkSet: 0xff, Ct: 0xff00}, nil},
		{"compat CONNMARK set", []expr.Any{&expr.Target{Name: "CONNMARK", Rev: 1,
			Info: xtData(0x1, 0xf0, 0, connmarkSet)}}, MarkUse{CtSet: 0xf1}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u MarkUse
			assert.Equal(t, tc.to, nftRule(tc.exprs, &u))
			assert.Equal(t, tc.want, u)
		})
	}
}

func TestMarkUseConflicts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		u        MarkUse
		shift    uint8
		tag, out bool
	}{
		{"matched after classify", MarkUse{Sees: SeesAfter, Mark: 0x4000}, 8, false, true},
		{"another byte", MarkUse{Sees: SeesAfter, Mark: 0x4000}, 0, false, false},
		{"set before classify, source-IP node", MarkUse{Sees: SeesBefore, MarkSet: allBits}, 0, false, false},
		{"set before classify, tag node", MarkUse{Sees: SeesBefore, MarkSet: allBits}, 0, true, true},
		{"ct mark set anywhere", MarkUse{Sees: SeesLocal, CtSet: 0xff}, 0, false, true},
		{"ct mark matched after", MarkUse{Sees: SeesAfter, Ct: 0xff}, 0, false, true},
		{"ct mark matched before", MarkUse{Sees: SeesBefore, Ct: 0xff}, 0, true, false},
		{"local only", MarkUse{Sees: SeesLocal, Mark: 0xff, MarkSet: 0xff}, 0, true, false},
		{"unreachable", MarkUse{CtSet: allBits}, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.out, tc.u.Conflicts(tc.shift, tc.tag))
		})
	}
}

func TestNftSees(t *testing.T) {
	for _, tc := range []struct {
		f    nftables.TableFamily
		hook nftables.ChainHook
		prio int32
		want uint8
	}{
		{nftables.TableFamilyIPv4, *nftables.ChainHookPrerouting, -150, SeesBefore},
		{nftables.TableFamilyIPv4, *nftables.ChainHookPrerouting, -91, SeesBefore},
		{nftables.TableFamilyINet, *nftables.ChainHookPrerouting, -90, SeesAfter},
		{nftables.TableFamilyIPv6, *nftables.ChainHookPrerouting, 0, SeesAfter},
		{nftables.TableFamilyIPv4, *nftables.ChainHookForward, -300, SeesAfter},
		{nftables.TableFamilyINet, *nftables.ChainHookPostrouting, 100, SeesAfter},
		{nftables.TableFamilyIPv4, *nftables.ChainHookInput, 0, SeesLocal},
		{nftables.TableFamilyIPv4, *nftables.ChainHookOutput, -150, SeesLocal},
		{nftables.TableFamilyINet, inetIngress, 0, SeesBefore},
		{nftables.TableFamilyINet, *nftables.ChainHookIngress, 0, SeesAfter}, // netdev's ingress number is prerouting here
		{nftables.TableFamilyNetdev, *nftables.ChainHookIngress, 0, SeesBefore},
		{nftables.TableFamilyNetdev, *nftables.ChainHookEgress, 0, 0},
		{nftables.TableFamilyBridge, brPreRouting, 0, SeesBefore},
		{nftables.TableFamilyBridge, brLocalIn, 0, SeesBefore},
		{nftables.TableFamilyBridge, 2, 0, 0}, // forward: bridged frames only
		{nftables.TableFamilyARP, 0, 0, 0},
	} {
		assert.Equal(t, tc.want, nftSees(tc.f, tc.hook, tc.prio), "%s hook %d prio %d", familyName(tc.f), tc.hook, tc.prio)
	}
}

func TestRuleGraphSees(t *testing.T) {
	g := newRuleGraph()
	g.base("pre", SeesBefore)
	g.base("fwd", SeesAfter)
	g.edge("pre", "shared")
	g.edge("fwd", "shared")
	g.edge("shared", "deep")
	g.edge("deep", "shared") // a loop ends
	g.use(MarkUse{Where: "deep", Mark: 1})
	g.use(MarkUse{Where: "orphan", Mark: 1})
	g.use(MarkUse{Where: "pre"}) // no mark: not kept
	assert.Equal(t, []MarkUse{{Where: "deep", Mark: 1, Sees: SeesBefore | SeesAfter}, {Where: "orphan", Mark: 1}}, g.uses())
}

// TestNetnsForeignMarks reads a namespace's native nft rules (a jump and a
// verdict map carrying where a chain runs), not PureLB's table, and no
// legacy table, creating none.
func TestNetnsForeignMarks(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "node")
	var gen0 uint32
	require.NoError(t, h.in(func() (err error) { gen0, err = RulesetGen(); return err }))

	require.NoError(t, h.in(func() error {
		c, err := nftables.New()
		if err != nil {
			return err
		}
		tb := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "t1"})
		base := func(name string, hook *nftables.ChainHook, prio nftables.ChainPriority) *nftables.Chain {
			return c.AddChain(&nftables.Chain{Name: name, Table: tb, Type: nftables.ChainTypeFilter, Hooknum: hook,
				Priority: nftables.ChainPriorityRef(prio)})
		}
		fwd := base("fwd", nftables.ChainHookForward, 0)
		pre := base("pre", nftables.ChainHookPrerouting, -150)
		out := base("out", nftables.ChainHookOutput, 0)
		post := base("post", nftables.ChainHookPostrouting, 100)
		natc := base("nat", nftables.ChainHookPostrouting, 100)
		c.AddRule(&nftables.Rule{Table: tb, Chain: natc, Exprs: []expr.Any{loadMark(false),
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(0x4000)}, &expr.Verdict{Kind: expr.VerdictReturn}}})
		sub := c.AddChain(&nftables.Chain{Name: "sub", Table: tb})
		vsub := c.AddChain(&nftables.Chain{Name: "vsub", Table: tb})
		c.AddRule(&nftables.Rule{Table: tb, Chain: fwd, Exprs: []expr.Any{loadMark(false), bitwise(0x00ff0000, 0),
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ne(0x00010000)}, &expr.Verdict{Kind: expr.VerdictAccept}}})
		c.AddRule(&nftables.Rule{Table: tb, Chain: pre, Exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: "sub"}}})
		c.AddRule(&nftables.Rule{Table: tb, Chain: sub, Exprs: []expr.Any{&expr.Immediate{Register: 1, Data: ne(0x200)}, setMark(false)}})
		c.AddRule(&nftables.Rule{Table: tb, Chain: out, Exprs: []expr.Any{&expr.Immediate{Register: 1, Data: ne(5)}, setMark(true)}})
		c.AddRule(&nftables.Rule{Table: tb, Chain: vsub, Exprs: []expr.Any{loadMark(false), bitwise(^uint32(0x100), 0x100), setMark(false)}})
		vm := &nftables.Set{Table: tb, Name: "vm", IsMap: true, KeyType: nftables.TypeMark, DataType: nftables.TypeVerdict}
		if err := c.AddSet(vm, []nftables.SetElement{{Key: ne(0x01000000),
			VerdictData: &expr.Verdict{Kind: expr.VerdictJump, Chain: "vsub"}}}); err != nil {
			return err
		}
		c.AddRule(&nftables.Rule{Table: tb, Chain: post, Exprs: []expr.Any{loadMark(false), bitwise(0xff000000, 0),
			&expr.Lookup{SourceRegister: 1, SetName: "vm", SetID: vm.ID, IsDestRegSet: true, DestRegister: 0}}})
		// PureLB's own table is never read.
		own := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable})
		ch := c.AddChain(&nftables.Chain{Name: "classify", Table: own, Type: nftables.ChainTypeFilter,
			Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-90)})
		c.AddRule(&nftables.Rule{Table: own, Chain: ch, Exprs: []expr.Any{&expr.Immediate{Register: 1, Data: ne(1)}, setMark(false)}})
		return c.Flush()
	}))

	var nft, legacy []MarkUse
	var gen1 uint32
	require.NoError(t, h.in(func() error {
		c, err := nftables.New()
		if err != nil {
			return err
		}
		if nft, err = ForeignMarks(c); err != nil {
			return err
		}
		if legacy, err = LegacyMarks(); err != nil {
			return err
		}
		gen1, err = RulesetGen()
		return err
	}))
	assert.NotEqual(t, gen0, gen1, "the generation moves with the ruleset")
	assert.Empty(t, legacyTableNames(t, h), "reading created a legacy table")
	assert.Empty(t, legacy)
	assert.ElementsMatch(t, []MarkUse{
		{Where: "ip t1 fwd", Sees: SeesAfter, Mark: 0x00ff0000},
		{Where: "ip t1 sub", Sees: SeesBefore, MarkSet: allBits},
		{Where: "ip t1 out", Sees: SeesLocal, CtSet: allBits},
		{Where: "ip t1 post", Sees: SeesAfter, Mark: 0xff000000},
		{Where: "ip t1 vsub", Sees: SeesAfter, MarkSet: 0x100},
		{Where: "ip t1 nat", Sees: SeesAfter, Mark: 0x4000},
	}, nft)
}

// legacyTableNames are the legacy iptables tables existing in h, both
// families.
func legacyTableNames(t *testing.T, h *host) (out []string) {
	t.Helper()
	require.NoError(t, h.in(func() error {
		for _, f := range legacyFamilies {
			names, err := legacyTables(f)
			out = append(out, names...)
			if err != nil {
				return err
			}
		}
		return nil
	}))
	return out
}

// TestNetnsXtablesMarks reads what the iptables tools write: iptables-nft's
// rules (compat expressions included) and legacy iptables' in both
// families, a user chain's place from the built-in chain jumping to it.
// It runs the tools, which need their table modules loaded beforehand;
// EGRESS_XTABLES_TESTS=required turns their absence into a failure.
func TestNetnsXtablesMarks(t *testing.T) {
	netnsOrSkip(t)
	for _, bin := range []string{"iptables-nft", "iptables-legacy", "ip6tables-legacy"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("EGRESS_XTABLES_TESTS") == "required" {
				t.Fatalf("%s is needed: %v", bin, err)
			}
			t.Skipf("%s is needed: %v", bin, err)
		}
	}
	h := newHost(t, "node")
	run := func(args ...string) {
		t.Helper()
		require.NoError(t, h.in(func() error {
			out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
			if err != nil {
				t.Logf("%s: %s", strings.Join(args, " "), out)
			}
			return err
		}))
	}
	run("iptables-nft", "-t", "mangle", "-A", "POSTROUTING", "-j", "MARK", "--set-xmark", "0x100/0x300")
	run("iptables-nft", "-t", "mangle", "-A", "PREROUTING", "-m", "connmark", "--mark", "0x10/0xf0", "-j", "ACCEPT")
	run("iptables-legacy", "-t", "mangle", "-N", "X")
	run("iptables-legacy", "-t", "mangle", "-A", "FORWARD", "-j", "X")
	run("iptables-legacy", "-t", "mangle", "-A", "X", "-m", "mark", "--mark", "0x1000000/0xff000000",
		"-j", "CONNMARK", "--save-mark", "--nfmask", "0xff", "--ctmask", "0xff00")
	run("ip6tables-legacy", "-t", "mangle", "-A", "POSTROUTING", "-m", "mark", "--mark", "0x10/0xff", "-j", "ACCEPT")

	before := legacyTableNames(t, h)
	var nft, legacy []MarkUse
	require.NoError(t, h.in(func() error {
		c, err := nftables.New()
		if err != nil {
			return err
		}
		if nft, err = ForeignMarks(c); err != nil {
			return err
		}
		legacy, err = LegacyMarks()
		return err
	}))
	assert.Equal(t, []string{"mangle", "mangle"}, before)
	assert.Equal(t, before, legacyTableNames(t, h), "reading created a legacy table")
	assert.ElementsMatch(t, []MarkUse{
		{Where: "ip mangle POSTROUTING", Sees: SeesAfter, MarkSet: 0x300},
		{Where: "ip mangle PREROUTING", Sees: SeesBefore, Ct: 0xf0},
	}, nft)
	assert.ElementsMatch(t, []MarkUse{
		{Where: "legacy ip mangle X", Sees: SeesAfter, Mark: 0xff0000ff, CtSet: 0xff00},
		{Where: "legacy ip6 mangle POSTROUTING", Sees: SeesAfter, Mark: 0xff},
	}, legacy)
}
