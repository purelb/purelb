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

package natspike

import (
	"errors"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
)

// Spike 2 (completion): pl_pod_out, pl_pod_reply and pl_announce_all load
// under the agent's exact capabilities. vm-test.sh runs every
// TestSpike2Load* test with only CAP_BPF and CAP_NET_ADMIN.
func TestSpike2LoadNat(t *testing.T) {
	requireVM(t)
	var o natObjects
	if err := loadNatObjects(&o, nil); err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			for _, l := range ve.Log {
				t.Log("VERIFIER", l)
			}
		}
		t.Fatalf("load under CAP_BPF+CAP_NET_ADMIN: %v", err)
	}
	defer o.Close()
	for _, p := range []struct {
		name string
		prog *ebpf.Program
	}{
		{"pl_pod_out", o.PlPodOut}, {"pl_pod_reply", o.PlPodReply}, {"pl_announce_all", o.PlAnnounceAll},
	} {
		info, err := p.prog.Info()
		require.NoError(t, err)
		if n, ok := info.VerifiedInstructions(); ok {
			t.Logf("SPIKE2 LOADED %s verified_insns=%d", p.name, n)
		} else {
			t.Logf("SPIKE2 LOADED %s", p.name)
		}
	}
}
