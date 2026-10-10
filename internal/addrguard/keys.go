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

package addrguard

import (
	"encoding/binary"
	"net/netip"
)

// The BPF maps hold addresses and ports in network byte order, exactly as
// they appear in the packet. cilium/ebpf marshals integers in host order
// (little-endian on every architecture we build for), so these helpers
// place the network-order bytes where the program will compare them.

// Families, actions and reasons mirror the enums in bpf/addrguard.c.
const (
	famV4 = 0
	famV6 = 1

	actPass      = 0
	actDrop      = 1
	actWouldDrop = 2
	actMax       = 3

	reasonPortAllowed  = 0
	reasonProtoAllowed = 1
	reasonICMP         = 2
	reasonFragment     = 3
	reasonPortDenied   = 4
	reasonProtoDenied  = 5
	reasonICMPDenied   = 6
	reasonMalformed    = 7
	reasonMax          = 8

	// ag_unread reasons (enum unread_reason).
	unreadTruncated = 0
	unreadVLANDepth = 1
	unreadMax       = 2

	modeEnforce = 0
	modeMonitor = 1
)

// unreadIndex is the ag_unread slot for (reason, monitor).
func unreadIndex(reason uint32, monitor bool) uint32 {
	if monitor {
		return reason*2 + 1
	}
	return reason * 2
}

// reasonIndex is the ag_reasons slot for (action, reason, family).
func reasonIndex(action, reason, fam uint32) uint32 {
	return (action*reasonMax+reason)*2 + fam
}

// vip4Key is the ag_vip4 key for a: the address bytes as they sit in the
// IPv4 header.
func vip4Key(a netip.Addr) [4]byte { return a.As4() }

// vip6Key is the ag_vip6 key for a.
func vip6Key(a netip.Addr) [16]byte { return a.As16() }

// portKey is the ag_ports key allowing proto/port on address a.
func portKey(a netip.Addr, proto uint8, port uint16) addrguardPortKey {
	k := addrguardPortKey{Proto: proto}
	if a.Is4() {
		b := a.As4()
		k.Addr[0] = binary.LittleEndian.Uint32(b[:])
		k.Family = famV4
	} else {
		b := a.As16()
		for i := range k.Addr {
			k.Addr[i] = binary.LittleEndian.Uint32(b[i*4:])
		}
		k.Family = famV6
	}
	// Network order in memory: the high byte first.
	k.Port = port>>8 | port<<8
	return k
}
