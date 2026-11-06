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

package diag

import (
	"compress/gzip"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/egress/diagreport"
)

func TestKernelConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.gz")
	f, err := os.Create(path)
	require.NoError(t, err)
	z := gzip.NewWriter(f)
	var lines []string
	for _, o := range kernelOptions {
		if o != "GENEVE" && o != "NFT_NAT" {
			lines = append(lines, "CONFIG_"+o+"=y")
		}
	}
	lines = append(lines, "CONFIG_NFT_NAT=m", "# CONFIG_GENEVE is not set")
	_, err = z.Write([]byte(strings.Join(lines, "\n")))
	require.NoError(t, err)
	require.NoError(t, z.Close())
	require.NoError(t, f.Close())

	known, missing := kernelConfig(path)
	assert.True(t, known)
	assert.Equal(t, []string{"GENEVE"}, missing, "a module counts")

	known, _ = kernelConfig(filepath.Join(t.TempDir(), "none"))
	assert.False(t, known)
}

// Read reads every map it uses from what an agent pins: a map OpenPinned
// doesn't open would panic here, not in a test of one reader.
func TestReadPinned(t *testing.T) {
	if !bpfpin.Usable("/sys/fs/bpf") {
		t.Skip("needs a bpffs at /sys/fs/bpf")
	}
	dir, err := os.MkdirTemp("/sys/fs/bpf", "egress-diag-test-")
	if err != nil {
		t.Skipf("needs write access to the bpffs: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	o, err := datapath.Load(dir)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("needs CAP_BPF: %v", err)
	}
	require.NoError(t, err)
	addr := netip.MustParseAddr("2001:db8::7")
	require.NoError(t, o.SetAddress(addr, datapath.Address{Held: true, SliceSize: 4096, Base: 1024}))
	require.NoError(t, o.SetSlice(addr, 3, diagreport.NodeID("n1")))
	require.NoError(t, o.SetNode(diagreport.NodeID("n1"), datapath.Node{V4: netip.MustParseAddr("192.0.2.1")}))
	require.NoError(t, o.Close())

	r := Read(dir, "n1")
	assert.Empty(t, r.Errors)
	assert.True(t, r.Pinned)
	assert.Equal(t, "n1", r.Node)
	require.Len(t, r.Addresses, 1)
	assert.Equal(t, addr.String(), r.Addresses[0].Address)
	assert.Equal(t, []diagreport.Slice{{Index: 3, Owner: diagreport.NodeID("n1")}}, r.Addresses[0].Slices)
	require.Len(t, r.Nodes, 1)
	assert.Equal(t, "192.0.2.1", r.Nodes[0].V4)
	assert.True(t, r.Checks.BPFFS)
}
