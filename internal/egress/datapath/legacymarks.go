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
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Legacy iptables' getsockopt interface (linux/netfilter_ipv4/ip_tables.h,
// linux/netfilter_ipv6/ip6_tables.h): the same numbers in both families.
const (
	iptSoGetInfo    = 64 // IPT_SO_GET_INFO, IP6T_SO_GET_INFO
	iptSoGetEntries = 65 // IPT_SO_GET_ENTRIES, IP6T_SO_GET_ENTRIES
	xtTableNameLen  = 32 // XT_TABLE_MAXNAMELEN
	xtNumHooks      = 5  // NF_INET_NUMHOOKS
	xtEntryHdr      = 32 // struct xt_entry_match / xt_entry_target header
)

// legacyFamily is one family's legacy iptables layout: struct ipt_entry
// or ip6t_entry, and where its offsets are.
type legacyFamily struct {
	name               string
	domain, level      int
	tables             string // the /proc/net file naming its tables
	entrySize          int
	targetOff, nextOff int
}

var legacyFamilies = []legacyFamily{
	{name: "ip", domain: unix.AF_INET, level: unix.SOL_IP, tables: "ip_tables_names",
		entrySize: 112, targetOff: 88, nextOff: 90},
	{name: "ip6", domain: unix.AF_INET6, level: unix.SOL_IPV6, tables: "ip6_tables_names",
		entrySize: 168, targetOff: 140, nextOff: 142},
}

// legacySees is where each hook's built-in chain sees packets: every
// legacy table's prerouting runs ahead of classify (raw -300, mangle
// -150, nat -100).
var legacySees = [xtNumHooks]uint8{
	unix.NF_INET_PRE_ROUTING:  SeesBefore,
	unix.NF_INET_LOCAL_IN:     SeesLocal,
	unix.NF_INET_FORWARD:      SeesAfter,
	unix.NF_INET_LOCAL_OUT:    SeesLocal,
	unix.NF_INET_POST_ROUTING: SeesAfter,
}

var legacyHooks = [xtNumHooks]string{"PREROUTING", "INPUT", "FORWARD", "OUTPUT", "POSTROUTING"}

// LegacyMarks lists the legacy iptables (x_tables) rules that use the
// packet or conntrack mark, in this thread's network namespace. Only the
// tables that already exist are read: asking for another would create it
// (the kernel loads its module on demand). None without ip_tables.
func LegacyMarks() ([]MarkUse, error) {
	var out []MarkUse
	for _, f := range legacyFamilies {
		names, err := legacyTables(f)
		if err != nil {
			return nil, err
		}
		for _, t := range names {
			info, blob, err := legacyEntries(f, t)
			if err != nil {
				return nil, fmt.Errorf("reading legacy %s table %s: %w", f.name, t, err)
			}
			uses, err := legacyUses(f, t, info, blob)
			if err != nil {
				return nil, fmt.Errorf("parsing legacy %s table %s: %w", f.name, t, err)
			}
			out = append(out, uses...)
		}
	}
	return out, nil
}

// legacyTables are family f's existing tables.
func legacyTables(f legacyFamily) ([]string, error) {
	b, err := os.ReadFile("/proc/thread-self/net/" + f.tables)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // no ip_tables / ip6_tables
	}
	if err != nil {
		return nil, err
	}
	var names []string
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		if n := strings.TrimSpace(s.Text()); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// xtInfo is struct ipt_getinfo / ip6t_getinfo.
type xtInfo struct {
	Name       [xtTableNameLen]byte
	ValidHooks uint32
	HookEntry  [xtNumHooks]uint32
	Underflow  [xtNumHooks]uint32
	NumEntries uint32
	Size       uint32
}

// legacyEntries reads table t's info and its entries.
func legacyEntries(f legacyFamily, t string) (xtInfo, []byte, error) {
	var info xtInfo
	fd, err := unix.Socket(f.domain, unix.SOCK_RAW, unix.IPPROTO_RAW)
	if err != nil {
		return info, nil, err
	}
	defer unix.Close(fd)
	copy(info.Name[:], t)
	if err := getsockopt(fd, f.level, iptSoGetInfo, unsafe.Pointer(&info), unsafe.Sizeof(info)); err != nil {
		return info, nil, err
	}
	// struct ipt_get_entries: name, size, then the entries 8-aligned.
	const hdr = 40
	buf := make([]byte, hdr+int(info.Size))
	copy(buf, t)
	binary.NativeEndian.PutUint32(buf[xtTableNameLen:], info.Size)
	if err := getsockopt(fd, f.level, iptSoGetEntries, unsafe.Pointer(&buf[0]), uintptr(len(buf))); err != nil {
		return info, nil, err
	}
	return info, buf[hdr:], nil
}

func getsockopt(fd, level, opt int, p unsafe.Pointer, n uintptr) error {
	l := uint32(n)
	if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(level), uintptr(opt),
		uintptr(p), uintptr(unsafe.Pointer(&l)), 0); e != 0 {
		return e
	}
	return nil
}

// legacyUses walks table t's entries: each built-in chain from its hook
// entry, each user chain from its ERROR header; a standard target with a
// verdict >= 0 jumps to that offset.
func legacyUses(f legacyFamily, t string, info xtInfo, blob []byte) ([]MarkUse, error) {
	g := newRuleGraph()
	hookAt := map[uint32]int{}
	for h := range xtNumHooks {
		if info.ValidHooks&(1<<h) != 0 {
			hookAt[info.HookEntry[h]] = h
		}
	}
	type jump struct {
		from string
		to   uint32
	}
	var jumps []jump
	chainAt := map[uint32]string{} // an entry's offset: its chain
	chain := ""
	for off := 0; off < len(blob); {
		if len(blob) < off+f.entrySize {
			return nil, fmt.Errorf("entry at %d is cut short", off)
		}
		e := blob[off:]
		tOff := int(binary.NativeEndian.Uint16(e[f.targetOff:]))
		next := int(binary.NativeEndian.Uint16(e[f.nextOff:]))
		if next <= 0 || len(e) < next || tOff < f.entrySize || tOff+xtEntryHdr > next {
			return nil, fmt.Errorf("entry at %d has bad offsets", off)
		}
		if h, ok := hookAt[uint32(off)]; ok {
			chain = fmt.Sprintf("legacy %s %s %s", f.name, t, legacyHooks[h])
			g.base(chain, legacySees[h])
		}
		tName, tRev, tData := xtElem(e[tOff:next])
		if tName == "ERROR" {
			// A user chain's header (or the table's end): its rules follow.
			chain = fmt.Sprintf("legacy %s %s %s", f.name, t, cString(tData))
			off += next
			continue
		}
		chainAt[uint32(off)] = chain
		u := MarkUse{Where: chain}
		for m := f.entrySize; m+xtEntryHdr <= tOff; {
			size := int(binary.NativeEndian.Uint16(e[m:]))
			if size < xtEntryHdr || m+size > tOff {
				return nil, fmt.Errorf("entry at %d has a bad match", off)
			}
			name, rev, data := xtElem(e[m : m+size])
			xtUse(name, uint32(rev), false, data, &u)
			m += size
		}
		if tName == "" && len(tData) >= 4 {
			if v := int32(binary.NativeEndian.Uint32(tData)); v >= 0 {
				jumps = append(jumps, jump{from: chain, to: uint32(v)})
			}
		} else {
			xtUse(tName, uint32(tRev), true, tData, &u)
		}
		g.use(u)
		off += next
	}
	for _, j := range jumps {
		if to, ok := chainAt[j.to]; ok && to != j.from {
			g.edge(j.from, to)
		}
	}
	return g.uses(), nil
}

// xtElem is an xt_entry_match or xt_entry_target: its name, revision and
// data.
func xtElem(b []byte) (string, uint8, []byte) {
	return cString(b[2:31]), b[31], b[xtEntryHdr:]
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
