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
	"net"
	"net/netip"
)

// ProbePort is the inner UDP port of tunnel peer probes (egress.c
// PROBE_PORT).
const ProbePort = 65534

// ProbePacket is the UDP payload of a tunnel peer probe from self to peer,
// their underlay addresses in one family (design 6.8): a Geneve header and
// the inner probe, an Ethernet frame in L2 mode (selfMAC, peerMAC: the
// tunnel MACs). Sent to the peer's tunnel port from self, the peer's
// pl_announce_tun reflects it, and this node's records the reply
// (ProbeReplies).
func ProbePacket(mode string, self, peer netip.Addr, selfMAC, peerMAC net.HardwareAddr) []byte {
	udp := make([]byte, 16)
	binary.BigEndian.PutUint16(udp[0:], ProbePort)
	binary.BigEndian.PutUint16(udp[2:], ProbePort)
	binary.BigEndian.PutUint16(udp[4:], 16)
	copy(udp[8:], []byte{'P', 'L', 'P', 'B', 1, 0, 0, 0})
	var ip []byte
	ethertype := uint16(0x0800)
	if self.Is4() {
		ip = make([]byte, 20)
		ip[0], ip[8], ip[9] = 0x45, 64, 17
		binary.BigEndian.PutUint16(ip[2:], 20+16)
		s4, p4 := self.As4(), peer.As4()
		copy(ip[12:], s4[:])
		copy(ip[16:], p4[:])
		binary.BigEndian.PutUint16(ip[10:], inetChecksum(ip, 0))
	} else {
		ethertype = 0x86dd
		ip = make([]byte, 40)
		ip[0], ip[6], ip[7] = 0x60, 17, 64
		binary.BigEndian.PutUint16(ip[4:], 16)
		s6, p6 := self.As16(), peer.As16()
		copy(ip[8:], s6[:])
		copy(ip[24:], p6[:])
		// The pseudo header: addresses, length, next header.
		var sum uint32
		for i := 8; i < 40; i += 2 {
			sum += uint32(binary.BigEndian.Uint16(ip[i:]))
		}
		sum += 16 + 17
		c := inetChecksum(udp, sum)
		if c == 0 {
			c = 0xffff // 0 means none, which IPv6 doesn't allow
		}
		binary.BigEndian.PutUint16(udp[6:], c)
	}
	geneve := []byte{0, 0, 0, 0, 0, 0, 1, 0} // VNI 1 (egress.c GENEVE_VNI)
	inner := append(ip, udp...)
	if mode == ModeL2 {
		binary.BigEndian.PutUint16(geneve[2:], 0x6558) // Ethernet
		eth := make([]byte, 14)
		copy(eth[0:], peerMAC)
		copy(eth[6:], selfMAC)
		binary.BigEndian.PutUint16(eth[12:], ethertype)
		inner = append(eth, inner...)
	} else {
		binary.BigEndian.PutUint16(geneve[2:], ethertype)
	}
	return append(geneve, inner...)
}

// inetChecksum is the Internet checksum of b, starting from sum.
func inetChecksum(b []byte, sum uint32) uint16 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// ProbeReplies returns when each peer last answered a probe, by the
// underlay address it answered from, in CLOCK_MONOTONIC nanoseconds.
func (o *Objects) ProbeReplies() (map[netip.Addr]uint64, error) {
	out := map[netip.Addr]uint64{}
	var k egressUnderlayKey
	var v uint64
	it := o.PeerSeen.Iterate()
	for it.Next(&k, &v) {
		out[unwords(k.Addr, k.V6 != 0)] = v
	}
	return out, it.Err()
}
