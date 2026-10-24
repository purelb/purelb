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
	"syscall"
	"testing"

	"github.com/cilium/ebpf"
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
