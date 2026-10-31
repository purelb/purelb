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
	"net/netip"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// The ctnetlink attributes the kernel's headers name and the library
// doesn't (linux/netfilter/nfnetlink_conntrack.h).
const (
	ctaMarkMask = 21 // CTA_MARK_MASK
	ctaTupleIP  = 1  // CTA_TUPLE_IP
	ctaIPv4Src  = 1  // CTA_IP_V4_SRC
	ctaIPv6Src  = 3  // CTA_IP_V6_SRC
)

// CtFlow is one conntrack entry: its original source, and its attributes
// as the kernel sent them, which delete it.
type CtFlow struct {
	Src    netip.Addr
	family uint8
	attrs  []byte
}

// CtFlows lists the conntrack entries of both families whose mark field
// at shift is v (D18): one dump, filtered by the kernel.
func CtFlows(shift, v uint8) ([]CtFlow, error) {
	msgs, err := ctMarkRequest(nl.IPCTNL_MSG_CT_GET, unix.NLM_F_DUMP, shift, v).Execute(unix.NETLINK_NETFILTER, 0)
	if err != nil {
		return nil, fmt.Errorf("listing conntrack entries of mark %#x: %w", v, err)
	}
	out := make([]CtFlow, 0, len(msgs))
	for _, m := range msgs {
		if len(m) < 4 {
			continue
		}
		f := CtFlow{family: m[0], attrs: m[4:]}
		f.Src = origSrc(f.attrs)
		out = append(out, f)
	}
	return out, nil
}

// CtFlush deletes the conntrack entries of both families whose mark field
// at shift is v, in one request.
func CtFlush(shift, v uint8) error {
	_, err := ctMarkRequest(nl.IPCTNL_MSG_CT_DELETE, unix.NLM_F_ACK, shift, v).Execute(unix.NETLINK_NETFILTER, 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("deleting conntrack entries of mark %#x: %w", v, err)
	}
	return nil
}

// CtDelete deletes f; one already gone is no error.
func CtDelete(f CtFlow) error {
	req := ctRequest(nl.IPCTNL_MSG_CT_DELETE, unix.NLM_F_ACK, f.family)
	req.AddRawData(f.attrs)
	if _, err := req.Execute(unix.NETLINK_NETFILTER, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("deleting the conntrack entry from %s: %w", f.Src, err)
	}
	return nil
}

func ctRequest(op, flags int, family uint8) *nl.NetlinkRequest {
	req := nl.NewNetlinkRequest(unix.NFNL_SUBSYS_CTNETLINK<<8|op, flags)
	req.AddData(&nl.Nfgenmsg{NfgenFamily: family, Version: nl.NFNETLINK_V0})
	return req
}

// ctMarkRequest is op on the entries whose mark field at shift is v; the
// family AF_UNSPEC covers both.
func ctMarkRequest(op, flags int, shift, v uint8) *nl.NetlinkRequest {
	req := ctRequest(op, flags, unix.AF_UNSPEC)
	req.AddData(nl.NewRtAttr(nl.CTA_MARK, nl.BEUint32Attr(uint32(v)<<shift)))
	req.AddData(nl.NewRtAttr(ctaMarkMask, nl.BEUint32Attr(uint32(0xff)<<shift)))
	return req
}

// origSrc is the original direction's source address in an entry's
// attributes; invalid if it has none.
func origSrc(attrs []byte) netip.Addr {
	find := func(b []byte, typ uint16) []byte {
		as, err := nl.ParseRouteAttr(b)
		if err != nil {
			return nil
		}
		for _, a := range as {
			if a.Attr.Type&nl.NLA_TYPE_MASK == typ {
				return a.Value
			}
		}
		return nil
	}
	ip := find(find(attrs, nl.CTA_TUPLE_ORIG), ctaTupleIP)
	for _, typ := range []uint16{ctaIPv4Src, ctaIPv6Src} {
		if a, ok := netip.AddrFromSlice(find(ip, typ)); ok {
			return a
		}
	}
	return netip.Addr{}
}
