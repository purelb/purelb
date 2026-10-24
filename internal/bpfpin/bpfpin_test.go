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

package bpfpin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// The tests need a bpffs, CAP_BPF, CAP_NET_ADMIN and CAP_SYS_ADMIN (for
// the network namespace); PURELB_NETNS_TESTS=required turns a skip into a
// failure, as the bpf-test CI job and the VM matrix set it.
func skipOrFail(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	if os.Getenv("PURELB_NETNS_TESTS") == "required" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// tcProg is a tcx program that returns ret.
func tcProg(t *testing.T, ret int32) *ebpf.Program {
	t.Helper()
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Type:         ebpf.SchedCLS,
		License:      "Apache-2.0",
		Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, ret), asm.Return()},
	})
	if err != nil {
		skipOrFail(t, "loading a program: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// sandbox runs the test in a new network namespace with one dummy
// interface, and returns its ifindex and a pin directory in bpffs.
func sandbox(t *testing.T) (int, string) {
	t.Helper()
	if !Usable("/sys/fs/bpf") {
		skipOrFail(t, "no bpffs at /sys/fs/bpf")
	}
	dir, err := os.MkdirTemp("/sys/fs/bpf", "bpfpin-test-")
	if err != nil {
		skipOrFail(t, "creating a pin directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	runtime.LockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	ns, err := netns.New()
	if err != nil {
		runtime.UnlockOSThread()
		skipOrFail(t, "creating a network namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = netns.Set(orig)
		_ = ns.Close()
		_ = orig.Close()
		runtime.UnlockOSThread()
	})
	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}}))
	l, err := netlink.LinkByName("dummy0")
	require.NoError(t, err)
	return l.Attrs().Index, dir
}

// chain is the program IDs attached at tcx ingress of ifindex, in order.
func chain(t *testing.T, ifindex int) []ebpf.ProgramID {
	t.Helper()
	res, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: ebpf.AttachTCXIngress})
	require.NoError(t, err)
	var ids []ebpf.ProgramID
	for _, p := range res.Programs {
		ids = append(ids, p.ID)
	}
	return ids
}

func progID(t *testing.T, p *ebpf.Program) ebpf.ProgramID {
	t.Helper()
	info, err := p.Info()
	require.NoError(t, err)
	id, _ := info.ID()
	return id
}

// A pinned link outlives its file descriptor (an agent's exit); Adopt
// takes it over with a new program in place, so the chain keeps its
// order; unpinned and closed, it detaches.
func TestPinAdoptRelease(t *testing.T) {
	idx, dir := sandbox(t)
	oldProg, newProg, other := tcProg(t, 0), tcProg(t, 1), tcProg(t, 2)
	path := filepath.Join(dir, "guard-tcx-dummy0")

	first, err := link.AttachTCX(link.TCXOptions{Program: oldProg, Interface: idx, Attach: ebpf.AttachTCXIngress, Anchor: link.Head()})
	require.NoError(t, err)
	require.NoError(t, first.Pin(path))
	behind, err := link.AttachTCX(link.TCXOptions{Program: other, Interface: idx, Attach: ebpf.AttachTCXIngress, Anchor: link.Tail()})
	require.NoError(t, err)
	defer behind.Close()
	require.NoError(t, first.Close()) // the agent exits
	assert.Equal(t, []ebpf.ProgramID{progID(t, oldProg), progID(t, other)}, chain(t, idx), "still attached while pinned")

	l, err := Adopt(path, idx, newProg)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, []ebpf.ProgramID{progID(t, newProg), progID(t, other)}, chain(t, idx), "replaced in place")

	require.NoError(t, release(l))
	assert.Equal(t, []ebpf.ProgramID{progID(t, other)}, chain(t, idx), "detached")
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

// Nothing pinned: nothing to adopt. A pin on another interface is removed
// and detached rather than adopted.
func TestAdoptNothingOrStale(t *testing.T) {
	idx, dir := sandbox(t)
	p := tcProg(t, 0)
	l, err := Adopt(filepath.Join(dir, "none"), idx, p)
	require.NoError(t, err)
	assert.Nil(t, l)

	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy1"}}))
	d1, err := netlink.LinkByName("dummy1")
	require.NoError(t, err)
	path := filepath.Join(dir, "guard-tcx-dummy1")
	stale, err := link.AttachTCX(link.TCXOptions{Program: p, Interface: d1.Attrs().Index, Attach: ebpf.AttachTCXIngress})
	require.NoError(t, err)
	require.NoError(t, stale.Pin(path))
	require.NoError(t, stale.Close())

	l, err = Adopt(path, idx, p) // dummy1's pin, asked for dummy0
	require.NoError(t, err)
	assert.Nil(t, l)
	assert.Empty(t, chain(t, d1.Attrs().Index), "the stale pin was detached")
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

// Sweep detaches the pins under a prefix that keep rejects, and leaves the
// rest.
func TestSweep(t *testing.T) {
	idx, dir := sandbox(t)
	a, b, c := tcProg(t, 0), tcProg(t, 1), tcProg(t, 2)
	pin := func(p *ebpf.Program, name string) {
		l, err := link.AttachTCX(link.TCXOptions{Program: p, Interface: idx, Attach: ebpf.AttachTCXIngress, Anchor: link.Tail()})
		require.NoError(t, err)
		require.NoError(t, l.Pin(filepath.Join(dir, name)))
		require.NoError(t, l.Close())
	}
	pin(a, "guard-keep")
	pin(b, "guard-stale")
	pin(c, "other-stale")
	gone, err := Sweep(dir, "guard-", func(path string) bool { return filepath.Base(path) == "guard-keep" })
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(dir, "guard-stale")}, gone)
	assert.Equal(t, []ebpf.ProgramID{progID(t, a), progID(t, c)}, chain(t, idx))
	_, _ = Sweep(dir, "", func(string) bool { return false })
	assert.Empty(t, chain(t, idx))

	gone, err = Sweep(filepath.Join(dir, "missing"), "", func(string) bool { return false })
	assert.NoError(t, err)
	assert.Empty(t, gone)
}

// Usable is true only for a bpffs.
func TestUsable(t *testing.T) {
	assert.False(t, Usable(t.TempDir()), "a tmpfs or disk directory")
	assert.False(t, Usable("/nonexistent"))
}
