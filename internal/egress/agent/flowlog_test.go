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
	"bytes"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"purelb.io/internal/egress/datapath"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// flowLines are the flow log's lines in buf, decoded.
func flowLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !strings.Contains(l, `"egressFlow"`) || !strings.Contains(l, `"egress flow"`) {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m))
		delete(m, "level")
		delete(m, "op")
		delete(m, "node")
		delete(m, "msg")
		out = append(out, m)
	}
	return out
}

// Only a flow whose ct mark carries a slot in the mark byte is written,
// named by its pod and policy; counts and duration only where the kernel
// gave them.
func TestLogFlow(t *testing.T) {
	var buf bytes.Buffer
	a := New(log.NewJSONLogger(log.NewSyncWriter(&buf)), "node-a", "purelb-system")
	pod := netip.MustParseAddr("10.244.1.5")
	a.flowView.Store(&flowView{slots: map[uint8]flowSlot{4: {addr: netip.MustParseAddr("192.0.2.1"), policy: "Egress/voice/sip"}},
		pods: map[netip.Addr]string{pod: "voice/a"}})
	a.markByte.Store(16)
	ev := func(isNew bool, mark uint32) datapath.CtEvent {
		return datapath.CtEvent{New: isNew, Proto: 6, Src: netip.AddrPortFrom(pod, 40000),
			Dst: netip.MustParseAddrPort("203.0.113.7:443"), Nat: netip.MustParseAddrPort("192.0.2.1:20480"), Mark: mark}
	}
	newFlows := testutil.ToFloat64(flowEvents.WithLabelValues("new"))
	a.logFlow(ev(true, 0x00040123))
	a.logFlow(ev(true, 0x00000004))  // another byte's
	a.logFlow(ev(true, 0x00ff0000))  // classified not egress
	a.logFlow(ev(true, 0x00800000))  // a reply flag's value: never a slot
	a.logFlow(ev(false, 0x00000000)) // unmarked
	e := ev(false, 0x00040000)
	e.Counted, e.Packets, e.Bytes = true, 6, 624
	e.Start, e.Stop = time.Unix(1, 0), time.Unix(3, 500_000_000)
	a.logFlow(e)
	assert.Equal(t, []map[string]any{
		{"event": "new", "proto": "tcp", "pod": "voice/a", "src": "10.244.1.5:40000", "dst": "203.0.113.7:443",
			"egress": "192.0.2.1:20480", "slot": 4.0, "policy": "Egress/voice/sip"},
		{"event": "destroy", "proto": "tcp", "pod": "voice/a", "src": "10.244.1.5:40000", "dst": "203.0.113.7:443",
			"egress": "192.0.2.1:20480", "slot": 4.0, "policy": "Egress/voice/sip", "packets": 6.0, "bytes": 624.0,
			"duration": "2.5s"},
	}, flowLines(t, &buf))
	assert.Equal(t, 1.0, testutil.ToFloat64(flowEvents.WithLabelValues("new"))-newFlows)
}

// The flow log runs while egress and flowLog are on: a slot-marked flow
// created and deleted is written twice; turned off, it stops.
func TestNetnsFlowLog(t *testing.T) {
	_ = sandbox(t)
	var buf bytes.Buffer
	a := New(log.NewJSONLogger(log.NewSyncWriter(&buf)), "node-a", "purelb-system")
	a.flowView.Store(&flowView{slots: map[uint8]flowSlot{4: {policy: "Egress/voice/sip"}},
		pods: map[netip.Addr]string{netip.MustParseAddr("10.244.1.5"): "voice/a"}})
	a.on = &session{spec: purelbv2.LBNodeAgentEgressSpec{Enabled: true, FlowLog: true}}
	a.syncFlowLog()
	require.NotNil(t, a.flows)
	ctFlow(t, "10.244.1.5", 1000, 4)
	ctFlow(t, "10.244.1.9", 1001, 0xff) // not egress
	require.NoError(t, netlink.ConntrackTableFlush(netlink.ConntrackTable))
	// Polled here: the lines come from the flow log's goroutine.
	var lines []map[string]any
	for end := time.Now().Add(5 * time.Second); len(lines) < 2 && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
		lines = flowLines(t, &buf)
	}
	require.Len(t, lines, 2)
	assert.Equal(t, "new", lines[0]["event"])
	assert.Equal(t, "destroy", lines[1]["event"])
	for _, l := range lines {
		assert.Equal(t, "voice/a", l["pod"])
		assert.Equal(t, "10.244.1.5:1000", l["src"])
		assert.Equal(t, "Egress/voice/sip", l["policy"])
	}
	a.on.spec.FlowLog = false
	a.syncFlowLog()
	assert.Nil(t, a.flows)
	assert.Contains(t, buf.String(), `"flow log off"`)
}
