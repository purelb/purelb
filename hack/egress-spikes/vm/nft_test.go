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

package spikes

import (
	"encoding/binary"
	"flag"
	"fmt"
	"net/netip"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Spike 7: the netfilter pod-node stage as decided, built with
// google/nftables and loadable on 6.6 (no register-to-register OR):
//
//   - per-slot constant SNAT rules ("meta mark & 0xff == N snat to A:p1-p2"),
//     not a concatenated-range map;
//   - a ct-mark vmap whose per-slot chains write the slot with a constant
//     mask-and-xor, which is how established packets get their slot back.
type snatSlot struct {
	slot           uint32
	addr           string // egress address
	portLo, portHi uint16
}

// nftBuf, when set, is the netlink socket buffer size (bytes) nftEgress
// requests (spike 10: does a raised buffer fit 127 slots in one transaction?).
var nftBuf = flag.Int("nftbuf", 0, "netlink socket buffer bytes for nftEgress (0: library default)")

// nftEgress installs table inet purelb_spike in the node's namespace.
func nftEgress(t *testing.T, n *node, slots []snatSlot) {
	t.Helper()
	opts := []nftables.ConnOption{nftables.WithNetNSFd(int(n.h))}
	if *nftBuf > 0 {
		// Raise both netlink buffers (SO_RCVBUFFORCE / SO_SNDBUFFORCE with
		// CAP_NET_ADMIN), so a large table still fits one transaction.
		opts = append(opts, nftables.WithSockOptions(func(c *netlink.Conn) error {
			if err := c.SetReadBuffer(*nftBuf); err != nil {
				return err
			}
			return c.SetWriteBuffer(*nftBuf)
		}))
	}
	c, err := nftables.New(opts...)
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "purelb_spike"})

	// prerouting, after conntrack (-200) and before DNAT (-100): restore the
	// slot of an established flow from its ct mark with constant writes.
	pre := c.AddChain(&nftables.Chain{
		Name: "classify", Table: tbl, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-150),
	})
	vmap := &nftables.Set{Table: tbl, Name: "slot_restore", IsMap: true,
		KeyType: nftables.TypeMark, DataType: nftables.TypeVerdict}
	var elems []nftables.SetElement
	for _, s := range slots {
		ch := c.AddChain(&nftables.Chain{Name: chainName(s.slot), Table: tbl})
		c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: le32(0xffffff00), Xor: le32(s.slot)},
			&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
		}})
		elems = append(elems, nftables.SetElement{Key: le32(s.slot),
			VerdictData: &expr.Verdict{Kind: expr.VerdictJump, Chain: chainName(s.slot)}})
	}
	require.NoError(t, c.AddSet(vmap, elems))
	// ct mark & 0xff vmap @slot_restore, for packets with no slot yet.
	c.AddRule(&nftables.Rule{Table: tbl, Chain: pre, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(0xff), Xor: le32(0)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: le32(0)},
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(0xff), Xor: le32(0)},
		&expr.Lookup{SourceRegister: 1, SetName: vmap.Name, SetID: vmap.ID, IsDestRegSet: true},
	}})
	// Save the slot in the ct mark (constant per rule) so it can be restored.
	for _, s := range slots {
		c.AddRule(&nftables.Rule{Table: tbl, Chain: pre, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(0xff), Xor: le32(0)},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: le32(s.slot)},
			&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(0xffffff00), Xor: le32(s.slot)},
			&expr.Ct{Key: expr.CtKeyMARK, SourceRegister: true, Register: 1},
		}})
	}

	// postrouting at srcnat-10: the first NAT binding wins, so CNI and
	// kube-proxy masquerade at 100 never see these flows.
	post := c.AddChain(&nftables.Chain{
		Name: "nat", Table: tbl, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityRef(90),
	})
	for _, s := range slots {
		a := netip.MustParseAddr(s.addr)
		fam, nfproto := uint32(unix.NFPROTO_IPV4), byte(unix.NFPROTO_IPV4)
		raw := a.AsSlice()
		if a.Is6() {
			fam, nfproto = unix.NFPROTO_IPV6, unix.NFPROTO_IPV6
		}
		c.AddRule(&nftables.Rule{Table: tbl, Chain: post, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{nfproto}},
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(0xff), Xor: le32(0)},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: le32(s.slot)},
			&expr.Immediate{Register: 1, Data: raw},
			&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(s.portLo)},
			&expr.Immediate{Register: 3, Data: binaryutil.BigEndian.PutUint16(s.portHi)},
			&expr.NAT{Type: expr.NATTypeSourceNAT, Family: fam,
				RegAddrMin: 1, RegAddrMax: 1, RegProtoMin: 2, RegProtoMax: 3, Specified: true},
		}})
	}
	require.NoError(t, c.Flush(), "%s: nft flush", n.name)
	t.Cleanup(func() {
		c.DelTable(tbl)
		_ = c.Flush()
	})
}

func chainName(slot uint32) string { return fmt.Sprintf("slot_%d", slot) }

// le32 encodes v in host (little-endian) order, as nft compares meta/ct mark.
func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}
