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
	"os"
	"path/filepath"
	"strings"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"

	"purelb.io/internal/bpfpin"
)

// DeleteHardening removes the hardening table, if there is one (D26).
func DeleteHardening(c *nftables.Conn) error {
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable}
	// Adding first makes the delete valid when the table doesn't exist.
	c.AddTable(t)
	c.DelTable(t)
	return c.Flush()
}

// DeleteTunnel removes the tunnel device, if there is one; the links
// attached to it go with it.
func DeleteTunnel(h *netlink.Handle) error {
	l, err := h.LinkByName(TunnelName)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return h.LinkDel(l)
}

// TunnelExists reports whether the tunnel device exists, for finding an
// earlier agent's datapath while egress is off.
func TunnelExists(h *netlink.Handle) bool {
	_, err := h.LinkByName(TunnelName)
	return err == nil
}

// RemovePins removes what egress pinned in pinDir (D12): its links, which
// detaches them, and its maps. Other pins there (the guard's) are left.
func RemovePins(pinDir string) error {
	if !bpfpin.Usable(pinDir) {
		return nil
	}
	var errs []error
	for _, prefix := range []string{"pl_announce-", "pl_steer-", "pl_pod_tag-"} {
		if _, err := bpfpin.Sweep(pinDir, prefix, func(string) bool { return false }); err != nil {
			errs = append(errs, err)
		}
	}
	spec, err := loadEgress()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for name := range spec.Maps {
		if err := os.Remove(filepath.Join(pinDir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Pinned reports whether pinDir holds anything egress pinned.
func Pinned(pinDir string) bool {
	if !bpfpin.Usable(pinDir) {
		return false
	}
	entries, err := os.ReadDir(pinDir)
	if err != nil || len(entries) == 0 {
		return false
	}
	spec, err := loadEgress()
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if _, isMap := spec.Maps[n]; isMap || strings.HasPrefix(n, "pl_announce-") || strings.HasPrefix(n, "pl_steer-") {
			return true
		}
	}
	return false
}
