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

// Package bpfpin keeps the node agent's BPF links attached across agent
// restarts and upgrades by pinning them in one bpffs directory (D12), so a
// chain such as [guard, pl_announce] keeps its order and never runs
// without the guard. Links are pinned only while egress is enabled;
// otherwise each link lives and dies with the agent, as before egress.
package bpfpin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

// Dir is the agent's pin directory as its container sees it: the egress
// install mounts the node's /sys/fs/bpf/purelb over /sys/fs/bpf. It can't
// mount it at its own path, because the container's /sys is sysfs, where
// the runtime can't create the mount point (measured on prox-purelb2:
// "mkdirat .../rootfs/sys/fs/bpf/purelb: no such file or directory").
// Without that mount, /sys/fs/bpf is an empty sysfs directory and Usable
// is false.
const Dir = "/sys/fs/bpf"

// Usable reports whether dir is a mounted bpffs, which pinning needs; a
// writable bpffs needs no CAP_SYS_ADMIN.
func Usable(dir string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false
	}
	return st.Type == unix.BPF_FS_MAGIC
}

// Adopt takes over the link pinned at path when it is still attached to
// ifindex, replacing its program with prog (bpf_link_update), so the link
// keeps its place in its chain and traffic never runs without it. It
// returns nil when nothing usable is pinned there; a pin that no longer
// applies (its interface gone or another one) is removed, which detaches
// it. The returned link stays pinned; Unpin it to stop pinning.
func Adopt(path string, ifindex int, prog *ebpf.Program) (link.Link, error) {
	l, err := link.LoadPinnedLink(path, nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading pinned link %s: %w", path, err)
	}
	info, err := l.Info()
	if err != nil || attachedTo(info) != ifindex {
		return nil, release(l)
	}
	if err := l.Update(prog); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("replacing the program of pinned link %s: %w", path, err)
	}
	return l, nil
}

// attachedTo is the interface a tcx or XDP link is attached to, 0 when
// it is defunct or neither.
func attachedTo(info *link.Info) int {
	if t := info.TCX(); t != nil {
		return int(t.Ifindex)
	}
	if x := info.XDP(); x != nil {
		return int(x.Ifindex)
	}
	return 0
}

// Sweep detaches every link pinned in dir under a name starting with
// prefix that keep rejects, such as a link to an interface no longer
// attached to, pinned by an earlier agent. It returns the paths it
// detached.
func Sweep(dir, prefix string, keep func(path string) bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var gone []string
	var errs []error
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if !strings.HasPrefix(e.Name(), prefix) || keep(path) {
			continue
		}
		l, err := link.LoadPinnedLink(path, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("loading pinned link %s: %w", path, err))
			continue
		}
		if err := release(l); err != nil {
			errs = append(errs, err)
			continue
		}
		gone = append(gone, path)
	}
	return gone, errors.Join(errs...)
}

// release unpins and detaches l. The explicit detach takes effect at once;
// on 6.6 a detach that waits for the last close is asynchronous (spike 4),
// so a chain query just after it could still show the link.
func release(l link.Link) error {
	err := l.Unpin()
	if derr := l.Detach(); derr != nil && err == nil {
		err = derr
	}
	_ = l.Close()
	return err
}
