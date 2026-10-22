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

package spikes

import (
	"errors"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
)

// Spike 2: every prototype program loads under the agent's exact
// capabilities. The harness runs this test with only CAP_BPF and
// CAP_NET_ADMIN (no CAP_PERFMON, no CAP_SYS_ADMIN), on every matrix kernel.
func TestSpike2LoadCore(t *testing.T) {
	requireVM(t)
	var o coreObjects
	if err := loadCoreObjects(&o, nil); err != nil {
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
		{"pl_steer", o.PlSteer}, {"pl_announce", o.PlAnnounce}, {"pl_tun_in", o.PlTunIn}, {"pl_pod_tag", o.PlPodTag},
		{"pl_announce_all", o.PlAnnounceAll},
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
