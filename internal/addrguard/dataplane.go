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
	"errors"
	"fmt"
	"net/netip"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	"purelb.io/internal/kernelversion"
)

// dataplane writes the per-VIP state into the BPF maps. It is an interface
// so the Service bookkeeping can be tested without privileges.
type dataplane interface {
	putVIP(netip.Addr) error
	delVIP(netip.Addr) error
	putPort(netip.Addr, portSpec) error
	delPort(netip.Addr, portSpec) error
}

// nopDataplane stands in when the program couldn't be loaded: the guard
// keeps its bookkeeping (so the unguarded count is right) and fails open.
type nopDataplane struct{}

func (nopDataplane) putVIP(netip.Addr) error            { return nil }
func (nopDataplane) delVIP(netip.Addr) error            { return nil }
func (nopDataplane) putPort(netip.Addr, portSpec) error { return nil }
func (nopDataplane) delPort(netip.Addr, portSpec) error { return nil }

// mapDataplane writes the real maps.
type mapDataplane struct {
	objs *addrguardObjects
}

func (d *mapDataplane) putVIP(a netip.Addr) error {
	// The value is the VIP's per-CPU drop counters; a fresh VIP starts at 0.
	zero := make([]addrguardVipCounters, ebpf.MustPossibleCPU())
	if a.Is4() {
		return d.objs.AgVip4.Update(vip4Key(a), zero, ebpf.UpdateAny)
	}
	return d.objs.AgVip6.Update(vip6Key(a), zero, ebpf.UpdateAny)
}

func (d *mapDataplane) delVIP(a netip.Addr) error {
	var err error
	if a.Is4() {
		err = d.objs.AgVip4.Delete(vip4Key(a))
	} else {
		err = d.objs.AgVip6.Delete(vip6Key(a))
	}
	return ignoreMissing(err)
}

func (d *mapDataplane) putPort(a netip.Addr, p portSpec) error {
	return d.objs.AgPorts.Update(portKey(a, p.proto, p.port), uint8(1), ebpf.UpdateAny)
}

func (d *mapDataplane) delPort(a netip.Addr, p portSpec) error {
	return ignoreMissing(d.objs.AgPorts.Delete(portKey(a, p.proto, p.port)))
}

// putSpecial guards a kernel-generated broadcast or anycast address with no
// allowed ports. It never overwrites an existing key: if a VIP has the same
// address, the VIP's entry (and its ports) stands.
func (d *mapDataplane) putSpecial(a netip.Addr) (bool, error) {
	zero := make([]addrguardVipCounters, ebpf.MustPossibleCPU())
	var err error
	if a.Is4() {
		err = d.objs.AgVip4.Update(vip4Key(a), zero, ebpf.UpdateNoExist)
	} else {
		err = d.objs.AgVip6.Update(vip6Key(a), zero, ebpf.UpdateNoExist)
	}
	if errors.Is(err, ebpf.ErrKeyExist) {
		return false, nil
	}
	return err == nil, err
}

// setConfig writes the mode and the allowed non-port protocols.
func (d *mapDataplane) setConfig(s *Spec) error {
	mode := uint32(modeEnforce)
	if s.Monitor {
		mode = modeMonitor
	}
	if err := d.objs.AgConfig.Update(uint32(0), addrguardGuardConfig{Mode: mode}, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("writing mode: %w", err)
	}
	var allowed [256]bool
	for _, p := range s.AllowedProtocols {
		allowed[p] = true
	}
	for p := range allowed {
		v := uint8(0)
		if allowed[p] {
			v = 1
		}
		if err := d.objs.AgProtos.Update(uint32(p), v, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("writing allowed protocol %d: %w", p, err)
		}
	}
	return nil
}

func ignoreMissing(err error) error {
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil
	}
	return err
}

// loadProgram is load, replaceable in tests that check when the program is
// loaded.
var loadProgram = load

// load checks the kernel and loads the program and maps. On kernels older
// than 6.6 it refuses outright (VP gate G2): no partial guard.
func load() (*addrguardObjects, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return nil, fmt.Errorf("reading kernel version: %w", err)
	}
	release := unix.ByteSliceToString(uts.Release[:])
	v, err := kernelversion.Parse(release)
	if err != nil {
		return nil, err
	}
	if !v.AtLeast(kernelversion.AddressGuardMin) {
		return nil, fmt.Errorf("kernel %s is older than %s", release, kernelversion.AddressGuardMin)
	}
	var objs addrguardObjects
	if err := loadAddrguardObjects(&objs, nil); err != nil {
		if errors.Is(err, unix.EPERM) {
			return nil, fmt.Errorf("loading the BPF program was not permitted (is the BPF capability missing?): %w", err)
		}
		return nil, fmt.Errorf("loading the BPF program: %w", err)
	}
	return &objs, nil
}

// worstCaseMapBytes is the maps' footprint if every map were full. Hash
// maps are NO_PREALLOC, so real use is proportional to the VIPs and ports
// configured; this is the ceiling the pod's memory limit must allow for.
func worstCaseMapBytes() uint64 {
	spec, err := loadAddrguard()
	if err != nil {
		return 0
	}
	cpus := uint64(ebpf.MustPossibleCPU())
	var total uint64
	for _, m := range spec.Maps {
		per := uint64(m.KeySize + m.ValueSize)
		if m.Type == ebpf.PerCPUHash || m.Type == ebpf.PerCPUArray {
			per = uint64(m.KeySize) + uint64(m.ValueSize)*cpus
		}
		total += per * uint64(m.MaxEntries)
	}
	return total
}
