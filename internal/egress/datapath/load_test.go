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
	"net/netip"
	"os"
	"syscall"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// load loads the programs as the agent does, with CAP_BPF and
// CAP_NET_ADMIN only when the test runs that way (the bpf-test job and the
// VM matrix). EGRESS_BPF_TESTS=required turns a skip into a failure.
func load(t *testing.T) *Objects {
	t.Helper()
	o, err := Load("")
	if errors.Is(err, syscall.EPERM) || errors.Is(err, ebpf.ErrNotSupported) {
		if os.Getenv("EGRESS_BPF_TESTS") == "required" {
			t.Fatalf("BPF tests required but not permitted: %v", err)
		}
		t.Skipf("needs CAP_BPF and CAP_NET_ADMIN: %v", err)
	}
	var ve *ebpf.VerifierError
	if errors.As(err, &ve) {
		t.Fatalf("verifier: %+v", ve)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o
}

// Every program loads under the agent's capabilities.
func TestLoad(t *testing.T) {
	load(t)
}

// RecoverNodes reads back what SetNode wrote, both families, and removes
// underlay addresses whose node is gone or no longer has them.
func TestRecoverNodes(t *testing.T) {
	o := load(t)
	a := Node{V4: netip.MustParseAddr("10.0.0.2"), V6: netip.MustParseAddr("fd00::2"), MAC: TunnelMAC(NodeID("a"))}
	b := Node{V4: netip.MustParseAddr("10.0.0.3"), MAC: TunnelMAC(NodeID("b"))}
	require.NoError(t, o.SetNode(NodeID("a"), a))
	require.NoError(t, o.SetNode(NodeID("b"), b))
	// Orphans: an underlay address of a node not in the map, and one its
	// node no longer has.
	require.NoError(t, o.SetNode(NodeID("gone"), Node{V6: netip.MustParseAddr("fd00::9")}))
	require.NoError(t, o.Nodes.Delete(NodeID("gone")))
	require.NoError(t, o.NodeByUnderlay.Put(&egressUnderlayKey{Addr: [4]uint32{binary.NativeEndian.Uint32([]byte{10, 0, 0, 8})}},
		NodeID("b")))

	nodes, err := o.RecoverNodes()
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	assert.Equal(t, a.V4, nodes[NodeID("a")].V4)
	assert.Equal(t, a.V6, nodes[NodeID("a")].V6)
	assert.Equal(t, a.MAC.String(), nodes[NodeID("a")].MAC.String())
	assert.Equal(t, b.V4, nodes[NodeID("b")].V4)
	assert.False(t, nodes[NodeID("b")].V6.IsValid())

	var k egressUnderlayKey
	var id uint64
	have := map[netip.Addr]uint64{}
	it := o.NodeByUnderlay.Iterate()
	for it.Next(&k, &id) {
		have[unwords(k.Addr, k.V6 != 0)] = id
	}
	require.NoError(t, it.Err())
	assert.Equal(t, map[netip.Addr]uint64{a.V4: NodeID("a"), a.V6: NodeID("a"), b.V4: NodeID("b")}, have)
}
