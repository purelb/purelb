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

package agent

import (
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// The node's conditions (design 4.6): each True when healthy, the first
// failure reported, and a condition keeps its transition time while its
// status holds.
func TestNodeConditions(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	a := New(log.NewNopLogger(), "n1", "purelb-system")
	a.now = func() time.Time { return now }
	ok := &purelbv2.EgressCapability{Kernel: "6.12.111"}
	get := func(cs []metav1.Condition, typ string) string {
		for _, c := range cs {
			if c.Type == typ {
				return string(c.Status) + " " + c.Reason
			}
		}
		return ""
	}

	cs := a.nodeConditions(ok, nil)
	for _, typ := range []string{condDatapathReady, condDatapathCompatible, condUnderlayEncrypted,
		condPeersReachable, condForwardPath} {
		assert.Equal(t, "True "+typ, get(cs, typ))
	}
	start := cs[0].LastTransitionTime

	now = now.Add(time.Minute)
	cs = a.nodeConditions(&purelbv2.EgressCapability{Kernel: "6.1.0"}, &dpHealth{applyErr: "boom",
		noGateway: []string{"eth1 IPv6"}, encryption: []string{"wg0"},
		unreachable: []string{"n2 (10.0.0.2)"}, probed: 2})
	assert.Equal(t, "False KernelTooOld", get(cs, condDatapathCompatible))
	assert.Equal(t, "False ApplyFailed", get(cs, condDatapathReady), "the apply's failure first")
	assert.Equal(t, "False UnderlayUnencrypted", get(cs, condUnderlayEncrypted))
	assert.Equal(t, "False PeerUnreachable", get(cs, condPeersReachable))
	assert.Equal(t, "True "+condForwardPath, get(cs, condForwardPath), "one peer of two answers")

	cs = a.nodeConditions(ok, &dpHealth{noGateway: []string{"eth1 IPv6"}, arpAnnounce: []string{"eth1=0"},
		unreachable: []string{"n2 (10.0.0.2)", "n3 (10.0.0.3)"}, probed: 2})
	assert.Equal(t, "False NoGateway", get(cs, condDatapathReady))
	assert.Equal(t, "False TunnelPortBlocked", get(cs, condForwardPath), "no peer answers")
	cs = a.nodeConditions(ok, &dpHealth{arpAnnounce: []string{"eth1=0"}})
	assert.Equal(t, "False ArpAnnounce", get(cs, condDatapathReady))
	for _, c := range cs {
		if c.Type == condUnderlayEncrypted {
			assert.NotEqual(t, start, c.LastTransitionTime, "it changed status")
		}
	}
	now = now.Add(time.Minute)
	before := a.nodeConditions(ok, &dpHealth{arpAnnounce: []string{"eth1=0"}})
	assert.Equal(t, cs, before, "nothing changed: same conditions, same times")
}
