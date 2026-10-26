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
	"testing"

	"github.com/stretchr/testify/assert"
)

// A probe is a Geneve header (VNI 1, the inner protocol, Ethernet in L2
// mode) and a valid inner IPv4 or IPv6 UDP datagram to ProbePort carrying
// "PLPB" and the request type.
func TestProbePacket(t *testing.T) {
	macA, macB := TunnelMAC(NodeID("a")), TunnelMAC(NodeID("b"))
	v4 := ProbePacket(ModeL3, netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), macA, macB)
	assert.Equal(t, []byte{0, 0, 0x08, 0x00, 0, 0, 1, 0}, v4[:8])
	ip := v4[8:28]
	assert.Zero(t, checksum(ip), "IPv4 header checksum")
	assert.Equal(t, []byte{10, 0, 0, 1, 10, 0, 0, 2}, ip[12:20])
	udp := v4[28:]
	assert.Equal(t, uint16(ProbePort), binary.BigEndian.Uint16(udp[2:]))
	assert.Equal(t, []byte{'P', 'L', 'P', 'B', 1, 0, 0, 0}, udp[8:])

	v6 := ProbePacket(ModeL2, netip.MustParseAddr("fd00::1"), netip.MustParseAddr("fd00::2"), macA, macB)
	assert.Equal(t, []byte{0, 0, 0x65, 0x58, 0, 0, 1, 0}, v6[:8])
	eth := v6[8:22]
	assert.Equal(t, []byte(macB), eth[0:6], "to the peer's tunnel MAC")
	assert.Equal(t, []byte(macA), eth[6:12])
	assert.Equal(t, []byte{0x86, 0xdd}, eth[12:14])
	ip6, udp6 := v6[22:62], v6[62:]
	ps := append(append(append([]byte{}, ip6[8:40]...), 0, 0, 0, 16), 0, 0, 0, 17)
	assert.Zero(t, checksum(append(ps, udp6...)), "UDP checksum over the IPv6 pseudo header")
	assert.NotZero(t, binary.BigEndian.Uint16(udp6[6:]), "IPv6 UDP needs a checksum")
}
