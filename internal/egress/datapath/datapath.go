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
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
)

// Address families as the programs store them (AF_INET, AF_INET6).
const (
	familyV4 = 2
	familyV6 = 10
)

// Reasons are the programs' counter reasons, in index order
// (egress_reasons: reason * 2 + family).
var Reasons = []string{
	"to_tunnel", "owner_local", "service_port", "not_announcer", "no_owner", "frag_missing",
	"ptb_sent", "ptb_failed", "no_node", "malformed", "transit", "reply_local", "forged_tunnel",
	"forged_inner", "forged_inner_destination", "ttl_expired", "no_route", "steered", "no_holder",
}

// Objects are egress's loaded programs and maps.
type Objects struct {
	egressObjects
}

// Load loads egress's programs and maps. With pinDir set (egress is
// enabled and the bpffs is mounted, D12), the maps that hold state are
// pinned there and reused by the next agent, so programs that stay
// attached across a restart keep their tables; a pinned map whose layout
// changed (an upgrade) is replaced by an empty one, which the agent fills
// from its desired state.
func Load(pinDir string) (*Objects, error) {
	spec, err := loadEgress()
	if err != nil {
		return nil, err
	}
	opts := &ebpf.CollectionOptions{}
	if pinDir != "" {
		for name, m := range spec.Maps {
			if name != "egress_scratch" && name != ".rodata" && name != ".bss" {
				m.Pinning = ebpf.PinByName
			}
		}
		opts.Maps.PinPath = pinDir
	}
	o := &Objects{}
	err = spec.LoadAndAssign(&o.egressObjects, opts)
	if errors.Is(err, ebpf.ErrMapIncompatible) && pinDir != "" {
		// An upgrade changed a map's layout: start those maps empty.
		for name, m := range spec.Maps {
			if m.Pinning != ebpf.PinByName {
				continue
			}
			if old, lerr := ebpf.LoadPinnedMap(filepath.Join(pinDir, name), nil); lerr == nil {
				compatible := old.Type() == m.Type && old.KeySize() == m.KeySize && old.ValueSize() == m.ValueSize &&
					old.MaxEntries() == m.MaxEntries
				_ = old.Close()
				if !compatible {
					_ = os.Remove(filepath.Join(pinDir, name))
				}
			}
		}
		err = spec.LoadAndAssign(&o.egressObjects, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("loading the egress programs: %w", err)
	}
	return o, nil
}

// NodeID is a node's id in the programs' maps: a 64-bit FNV-1a hash of its
// name, so every node computes the same id with no coordination, and 0 is
// never one (0 means "no owner").
func NodeID(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	if id := h.Sum64(); id != 0 {
		return id
	}
	return 1
}

// words is a's bytes as the programs hold them: copied from the packet
// into __u32 words, so each word is network order in memory.
func words(a netip.Addr) (w [4]uint32, family uint8) {
	if a.Is4() {
		b := a.As4()
		w[0] = binary.NativeEndian.Uint32(b[:])
		return w, familyV4
	}
	b := a.As16()
	for i := range w {
		w[i] = binary.NativeEndian.Uint32(b[4*i:])
	}
	return w, familyV6
}

// unwords is words' inverse.
func unwords(w [4]uint32, v6 bool) netip.Addr {
	if !v6 {
		var b [4]byte
		binary.NativeEndian.PutUint32(b[:], w[0])
		return netip.AddrFrom4(b)
	}
	var b [16]byte
	for i := range w {
		binary.NativeEndian.PutUint32(b[4*i:], w[i])
	}
	return netip.AddrFrom16(b)
}

func addrKey(a netip.Addr) egressAddrKey {
	w, f := words(a)
	return egressAddrKey{Addr: w, Family: f}
}

func portKey(a netip.Addr, proto uint8, port uint16) egressPortKey {
	w, f := words(a)
	return egressPortKey{Addr: w, Family: f, Proto: proto, Port: port}
}

func sliceKey(a netip.Addr, index uint16) egressSliceKey {
	w, f := words(a)
	return egressSliceKey{Addr: w, Family: f, Index: index}
}
