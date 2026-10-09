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

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// agentMetricsText is an agent's guard metrics, in the exposition format.
func agentMetricsText(loaded, restart, down int, attached map[string]string, extra ...string) []byte {
	var b strings.Builder
	b.WriteString("# TYPE purelb_address_guard_loaded gauge\n")
	b.WriteString("purelb_address_guard_loaded " + itoa(loaded) + "\n")
	b.WriteString("# TYPE purelb_address_guard_restart_required gauge\n")
	b.WriteString("purelb_address_guard_restart_required " + itoa(restart) + "\n")
	b.WriteString("# TYPE purelb_address_guard_standing_down gauge\n")
	b.WriteString("purelb_address_guard_standing_down " + itoa(down) + "\n")
	b.WriteString("# TYPE purelb_address_guard_attached gauge\n")
	for iface, hook := range attached {
		b.WriteString(`purelb_address_guard_attached{hook="` + hook + `",interface="` + iface + `"} 1` + "\n")
	}
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	return []byte(b.String())
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestGuardStateFrom(t *testing.T) {
	eth := map[string]string{"eth1": "tcx"}

	s := guardStateFrom("n1", "enforce", agentMetricsText(1, 0, 0, map[string]string{"eth1": "tcx", "eth0": "xdp"},
		"# TYPE purelb_address_guard_packets_total counter",
		`purelb_address_guard_packets_total{action="drop",family="ipv4",reason="port_denied"} 12`,
		`purelb_address_guard_packets_total{action="drop",family="ipv6",reason="icmp_denied"} 3`,
		`purelb_address_guard_packets_total{action="pass",family="ipv4",reason="port_allowed"} 900`,
		"# TYPE purelb_address_guard_chain_position gauge",
		`purelb_address_guard_chain_position{interface="eth1"} 2`,
		"# TYPE purelb_address_guard_vip_packets_total counter",
		`purelb_address_guard_vip_packets_total{action="drop",ip="10.255.7.100"} 12`,
		`purelb_address_guard_vip_packets_total{action="drop",ip="2001:db8::1"} 9`,
		`purelb_address_guard_vip_packets_total{action="would_drop",ip="2001:db8::1"} 5`,
		"# TYPE purelb_address_guard_unattached_interfaces gauge",
		`purelb_address_guard_unattached_interfaces{interface="eth2"} 1`,
		"# TYPE purelb_address_guard_failed_vips gauge",
		`purelb_address_guard_failed_vips{effect="withheld"} 2`,
		`purelb_address_guard_failed_vips{effect="incomplete"} 0`,
	), nil)
	assert.Equal(t, guardStateEnforcing, s.State)
	assert.True(t, s.Loaded)
	assert.Equal(t, map[string]string{"eth0": "xdp", "eth1": "tcx"}, s.Attached)
	assert.Equal(t, 15.0, s.Drops, "drops only, not passes")
	assert.Equal(t, map[string]int{"eth1": 2}, s.ChainPosition, "tcx only: eth0 is XDP")
	assert.Equal(t, "2001:db8::1", s.TopVIP, "the top VIP is the per-address total over every action")
	assert.Equal(t, 14.0, s.TopVIPPackets)
	assert.Equal(t, []string{"eth2"}, s.Unattached)
	assert.Equal(t, map[string]float64{"withheld": 2}, s.FailedVIPs, "non-zero effects only")

	assert.Equal(t, guardStateMonitoring, guardStateFrom("n1", "monitor", agentMetricsText(1, 0, 0, eth), nil).State)
	assert.Equal(t, guardOff, guardStateFrom("n1", guardOff, agentMetricsText(0, 0, 0, nil), nil).State,
		"not configured, nothing loaded")

	// Enabled after the agent started: configured, but not loaded.
	s = guardStateFrom("n1", "enforce", agentMetricsText(0, 1, 0, nil,
		"# TYPE purelb_address_guard_unguarded_vips gauge",
		`purelb_address_guard_unguarded_vips{reason="restart_pending"} 4`,
		`purelb_address_guard_unguarded_vips{reason="not_loaded"} 0`), nil)
	assert.Equal(t, guardStateRestartRequired, s.State)
	assert.Equal(t, map[string]float64{"restart_pending": 4}, s.UnguardedVIPs, "non-zero reasons only")

	// Disabled after the agent started: still loaded, configured off.
	assert.Equal(t, guardStateRestartRequired, guardStateFrom("n1", guardOff, agentMetricsText(1, 1, 0, nil), nil).State)

	assert.Equal(t, guardStateStandingDown, guardStateFrom("n1", "enforce", agentMetricsText(0, 0, 1, nil), nil).State)
	assert.Equal(t, guardStateNotLoaded, guardStateFrom("n1", "enforce", agentMetricsText(0, 0, 0, nil), nil).State,
		"fail-open with the program not running")
	assert.Equal(t, guardStateNotAttached, guardStateFrom("n1", "enforce", agentMetricsText(1, 0, 0, nil), nil).State)

	s = guardStateFrom("n1", "enforce", nil, errors.New("connection refused"))
	assert.Equal(t, guardStateUnknown, s.State)
	assert.Equal(t, "connection refused", s.Error)

	s = guardStateFrom("n1", "enforce", []byte("# TYPE purelb_lbnodeagent_up gauge\npurelb_lbnodeagent_up 1\n"), nil)
	assert.Equal(t, guardStateUnknown, s.State)
	assert.Contains(t, s.Error, "no address guard metrics")
}

func agentPod(name, node string) v1.Pod {
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "purelb-system"},
		Spec: v1.PodSpec{NodeName: node, Containers: []v1.Container{{
			Name:  "lbnodeagent",
			Ports: []v1.ContainerPort{{Name: "monitoring", ContainerPort: 7472}},
		}}},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}
}

func TestCollectGuardStates(t *testing.T) {
	pods := []v1.Pod{agentPod("a", "node-c"), agentPod("b", "node-a"), agentPod("c", "node-b")}
	nodes := &v1.NodeList{Items: []v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-c"}},
	}}
	agents := decodeLBNodeAgents(lbnaListOf(makeLBNA("default", guardSpec(map[string]interface{}{}, nil))))
	fetch := func(_ context.Context, pod v1.Pod) ([]byte, error) {
		if pod.Spec.NodeName == "node-b" {
			return nil, errors.New("timeout")
		}
		return agentMetricsText(1, 0, 0, map[string]string{"eth1": "tcx"}), nil
	}
	states := collectGuardStates(context.Background(), fetch, pods, nodes, agents)
	require.Len(t, states, 3)
	assert.Equal(t, []string{"node-a", "node-b", "node-c"}, []string{states[0].Node, states[1].Node, states[2].Node})
	assert.Equal(t, guardStateEnforcing, states[0].State)
	assert.Equal(t, "enforce", states[0].Mode, "the configured mode comes from the LBNodeAgent")
	assert.Equal(t, "timeout", states[1].Error, "one unreachable agent doesn't hide the others")
	assert.NoError(t, noStateRead(states))

	// During a rollout a node has a terminating pod and a new one: one row,
	// read from the new pod. A node whose only pod isn't running says so,
	// without a read.
	old := agentPod("old", "node-a")
	old.DeletionTimestamp = &metav1.Time{}
	pending := agentPod("p", "node-d")
	pending.Status.Phase = v1.PodPending
	var read []string
	fetch = func(_ context.Context, pod v1.Pod) ([]byte, error) {
		read = append(read, pod.Name)
		return agentMetricsText(1, 0, 0, map[string]string{"eth1": "tcx"}), nil
	}
	states = collectGuardStates(context.Background(), fetch, []v1.Pod{old, agentPod("new", "node-a"), pending}, nodes, agents)
	require.Len(t, states, 2)
	assert.Equal(t, guardStateEnforcing, states[0].State)
	assert.Equal(t, "lbnodeagent pod p is pending", states[1].Error)
	assert.Equal(t, []string{"new"}, read)

	// Every read failed: the command fails.
	assert.ErrorContains(t, noStateRead([]guardNodeState{{Node: "a", Error: "forbidden"}}), "from any node")
}

// More agents than workers: every one is read, each within its own
// timeout, however long the queue.
func TestCollectGuardStatesManyNodes(t *testing.T) {
	var pods []v1.Pod
	for i := range 1000 {
		pods = append(pods, agentPod(fmt.Sprintf("p%d", i), fmt.Sprintf("node-%04d", i)))
	}
	var inFlight, peak atomic.Int32
	fetch := func(_ context.Context, pod v1.Pod) ([]byte, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		return agentMetricsText(1, 0, 0, map[string]string{"eth1": "tcx"}), nil
	}
	states := collectGuardStates(context.Background(), fetch, pods, nil, nil)
	require.Len(t, states, 1000)
	for _, s := range states {
		require.Empty(t, s.Error, s.Node)
	}
	assert.LessOrEqual(t, peak.Load(), int32(guardFetchWorkers), "at most guardFetchWorkers reads at once")
}

func TestGuardLiveSummary(t *testing.T) {
	note, warnings := guardLiveSummary(nil)
	assert.Empty(t, note, "not read: nothing to add")
	assert.Empty(t, warnings)

	note, warnings = guardLiveSummary([]guardNodeState{
		{Node: "a", Mode: "enforce", State: guardStateRestartRequired, RestartRequired: true},
		{Node: "b", Mode: "enforce", State: guardStateStandingDown, StandingDown: true},
		{Node: "c", Mode: "enforce", State: guardStateEnforcing},
	})
	assert.Equal(t, " (1 restart required; 1 standing down)", note)
	assert.Equal(t, []string{
		"1 node(s) need an lbnodeagent restart for the address guard",
		"1 node(s) standing down: address guard not working",
	}, warnings)

	// An unreadable agent matters only where the guard is configured.
	note, warnings = guardLiveSummary([]guardNodeState{
		{Node: "a", Mode: guardOff, State: guardStateUnknown, Error: "forbidden"},
	})
	assert.Empty(t, note)
	assert.Empty(t, warnings)
	note, _ = guardLiveSummary([]guardNodeState{
		{Node: "a", Mode: "enforce", State: guardStateUnknown, Error: "forbidden: ... pods/proxy"},
	})
	assert.Equal(t, " (live state unavailable on 1 node(s): forbidden: ... pods/proxy)", note)
}

func TestAgentMetricsPort(t *testing.T) {
	pod := agentPod("a", "n")
	pod.Spec.Containers[0].Ports[0].ContainerPort = 9472
	assert.Equal(t, "9472", agentMetricsPort(pod), "the named monitoring port")
	assert.Equal(t, "7472", agentMetricsPort(v1.Pod{}), "the default when none is named")
}

func TestProxyErrorNamesThePermission(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/proxy"}, "a", errors.New("no"))
	assert.ErrorContains(t, proxyError(forbidden, "purelb-system"), "needs get on pods/proxy in purelb-system (see rbac-sample.yaml)")
	other := errors.New("connection refused")
	assert.Equal(t, other, proxyError(other, "purelb-system"))
	assert.NoError(t, proxyError(nil, "purelb-system"))
}

func TestChainSummary(t *testing.T) {
	tcx := map[string]string{"eth0": "tcx", "eth1": "tcx"}
	assert.Equal(t, "-", chainSummary(nil, nil), "nothing attached")
	assert.Equal(t, "-", chainSummary(map[string]string{"eth0": "xdp"}, nil), "XDP has no chain")
	assert.Equal(t, "1,1", chainSummary(tcx, map[string]int{"eth1": 1, "eth0": 1}), "ATTACHED order: eth0, eth1")
	assert.Equal(t, "1,3", chainSummary(tcx, map[string]int{"eth0": 1, "eth1": 3}))
	assert.Equal(t, "-,2", chainSummary(map[string]string{"eth0": "xdp", "eth1": "tcx"}, map[string]int{"eth1": 2}))
	assert.Equal(t, "?,?", chainSummary(tcx, nil), "on tcx, but the agent reports no position: an older agent")
}
