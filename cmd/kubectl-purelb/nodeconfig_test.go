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
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/utils/ptr"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// makeLBNA builds an unstructured LBNodeAgent the way the API server returns
// one, so the decode path is exercised rather than bypassed.
func makeLBNA(name string, spec map[string]interface{}) *unstructured.Unstructured {
	lbna := &unstructured.Unstructured{}
	lbna.SetGroupVersionKind(schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "LBNodeAgent"})
	lbna.SetName(name)
	lbna.SetNamespace("purelb-system")
	lbna.Object["spec"] = spec
	return lbna
}

func localSpec(localInterface string) map[string]interface{} {
	return map[string]interface{}{
		"local": map[string]interface{}{
			"localInterface": localInterface,
			"dummyInterface": "kube-lb0",
		},
	}
}

func lbnaListOf(items ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	for _, i := range items {
		list.Items = append(list.Items, *i)
	}
	return list
}

func TestDecodeLBNodeAgents(t *testing.T) {
	list := lbnaListOf(
		makeLBNA("default", localSpec("default")),
		makeLBNA("scoped", map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"role": "edge"},
			},
			"local": map[string]interface{}{"localInterface": "^eth1$"},
		}),
	)

	agents := decodeLBNodeAgents(list)
	if len(agents) != 2 {
		t.Fatalf("decoded %d agents, want 2", len(agents))
	}
	if agents[0].Name != "default" || agents[0].Spec.Local == nil {
		t.Errorf("first agent decoded wrong: %+v", agents[0])
	}
	if agents[1].Spec.NodeSelector == nil || agents[1].Spec.NodeSelector.MatchLabels["role"] != "edge" {
		t.Errorf("nodeSelector did not decode: %+v", agents[1].Spec.NodeSelector)
	}
	if got := agentName(agents[1]); got != "purelb-system/scoped" {
		t.Errorf("agentName = %q, want purelb-system/scoped", got)
	}

	if agents := decodeLBNodeAgents(nil); agents != nil {
		t.Errorf("nil list should decode to nil, got %v", agents)
	}
}

func TestResolveNodeConfig(t *testing.T) {
	catchAll := decodeLBNodeAgents(lbnaListOf(makeLBNA("default", localSpec("default"))))
	scoped := decodeLBNodeAgents(lbnaListOf(makeLBNA("edge", map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"role": "edge"},
		},
		"local": map[string]interface{}{"localInterface": "default"},
	})))
	remoteOnly := decodeLBNodeAgents(lbnaListOf(makeLBNA("remote", map[string]interface{}{})))
	badRegex := decodeLBNodeAgents(lbnaListOf(makeLBNA("broken", localSpec("["))))

	tests := []struct {
		name      string
		agents    []*purelbv2.LBNodeAgent
		labels    map[string]string
		wantState string
	}{
		{"no resources at all", nil, nil, configStateNoConfig},
		{"catch-all selects every node", catchAll, map[string]string{"any": "value"}, configStateConfigured},
		{"scoped selects a matching node", scoped, map[string]string{"role": "edge"}, configStateConfigured},
		{"scoped deselects a non-matching node", scoped, map[string]string{"role": "worker"}, configStateDeselected},
		{"resource without local spec", remoteOnly, nil, configStateRemote},
		{"invalid localInterface regex", badRegex, nil, configStateInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveNodeConfig(tt.agents, tt.labels).State; got != tt.wantState {
				t.Errorf("state = %q, want %q", got, tt.wantState)
			}
		})
	}
}

// TestResolveNodeConfigPrecedence pins that the plugin reports the same
// winner the node agent uses: a specific selector beats a catch-all
// regardless of name order, and the losers are listed as ignored.
func TestResolveNodeConfigPrecedence(t *testing.T) {
	agents := decodeLBNodeAgents(lbnaListOf(
		makeLBNA("aaa-catchall", localSpec("default")),
		makeLBNA("zzz-scoped", map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"role": "edge"},
			},
			"local": map[string]interface{}{"localInterface": "^eth1$"},
		}),
	))

	cfg := resolveNodeConfig(agents, map[string]string{"role": "edge"})
	if cfg.Agent != "purelb-system/zzz-scoped" {
		t.Errorf("governing agent = %q, want the specific selector to win", cfg.Agent)
	}
	if len(cfg.Ignored) != 1 || cfg.Ignored[0] != "purelb-system/aaa-catchall" {
		t.Errorf("ignored = %v, want the catch-all listed", cfg.Ignored)
	}

	// A node the specific selector does not match falls back to the catch-all
	// with nothing ignored.
	cfg = resolveNodeConfig(agents, map[string]string{"role": "worker"})
	if cfg.Agent != "purelb-system/aaa-catchall" || len(cfg.Ignored) != 0 {
		t.Errorf("non-matching node: agent=%q ignored=%v", cfg.Agent, cfg.Ignored)
	}
}

func TestLocalInterfaceIssues(t *testing.T) {
	tests := []struct {
		pattern    string
		invalid    bool
		unanchored bool
	}{
		{"default", false, false},
		{"", false, false}, // agent treats empty as "default"
		{"^eth1$", false, false},
		{"^(eth0|eth1)$", false, false},
		{"eth", false, true},   // matches veth1a2b3c too
		{"^eth1", false, true}, // half-anchored
		{"eth1$", false, true}, // half-anchored
		{"[", true, false},     // does not compile
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			invalid, unanchored := localInterfaceIssues(tt.pattern)
			if invalid != tt.invalid || unanchored != tt.unanchored {
				t.Errorf("localInterfaceIssues(%q) = (%v,%v), want (%v,%v)",
					tt.pattern, invalid, unanchored, tt.invalid, tt.unanchored)
			}
		})
	}
}

// TestDummyInterfacesDeterministic covers the bug where callers read list
// item [0]: with several resources the answer depended on list order, and a
// second distinct name was invisible.
func TestDummyInterfacesDeterministic(t *testing.T) {
	withDummy := func(name, dummy string) *unstructured.Unstructured {
		return makeLBNA(name, map[string]interface{}{
			"local": map[string]interface{}{"localInterface": "default", "dummyInterface": dummy},
		})
	}

	agents := decodeLBNodeAgents(lbnaListOf(withDummy("b", "kube-lb1"), withDummy("a", "kube-lb0")))
	got := dummyInterfaces(agents)
	if len(got) != 2 || got[0] != "kube-lb0" || got[1] != "kube-lb1" {
		t.Errorf("dummyInterfaces = %v, want sorted [kube-lb0 kube-lb1]", got)
	}

	// Reversed input order must give the same answer.
	reversed := decodeLBNodeAgents(lbnaListOf(withDummy("a", "kube-lb0"), withDummy("b", "kube-lb1")))
	if got2 := dummyInterfaces(reversed); got2[0] != got[0] || got2[1] != got[1] {
		t.Errorf("order-dependent result: %v vs %v", got, got2)
	}

	if got := dummyInterfaces(nil); len(got) != 1 || got[0] != defaultDummyInterface {
		t.Errorf("no agents should default to %q, got %v", defaultDummyInterface, got)
	}
}

func TestDummyInterfaceForNode(t *testing.T) {
	agents := decodeLBNodeAgents(lbnaListOf(
		makeLBNA("default", map[string]interface{}{
			"local": map[string]interface{}{"localInterface": "default", "dummyInterface": "kube-lb0"},
		}),
		makeLBNA("edge", map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"role": "edge"},
			},
			"local": map[string]interface{}{"localInterface": "default", "dummyInterface": "edge-lb0"},
		}),
	))

	if got := dummyInterfaceForNode(agents, map[string]string{"role": "edge"}); got != "edge-lb0" {
		t.Errorf("edge node dummy = %q, want edge-lb0", got)
	}
	if got := dummyInterfaceForNode(agents, map[string]string{"role": "worker"}); got != "kube-lb0" {
		t.Errorf("worker node dummy = %q, want kube-lb0", got)
	}
}

func TestAnnounceState(t *testing.T) {
	healthy := map[string]bool{"node-a": true}
	announcers := map[string]announcement{
		"10.0.0.1": {Node: "node-a", Interface: "eth0", IP: "10.0.0.1"},
		"10.0.0.2": {Node: "node-dead", Interface: "eth0", IP: "10.0.0.2"},
	}

	tests := []struct {
		name     string
		ip       string
		poolType string
		want     string
	}{
		{"announced by a healthy node", "10.0.0.1", poolTypeLocal, svcStatusOK},
		{"announced by a dead node", "10.0.0.2", poolTypeLocal, svcStatusAnnouncerUnhealthy},
		{"local pool with no announcer", "10.0.0.3", poolTypeLocal, svcStatusNoAnnouncer},
		{"remote pool needs no annotation", "10.0.0.3", poolTypeRemote, svcStatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := announceState(announcers, tt.ip, tt.poolType, healthy); got != tt.want {
				t.Errorf("announceState = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveNodeConfigContested separates the two multi-match cases: a
// specific selector beating a catch-all resolves unambiguously, while two
// specific selectors are decided only by namespace/name order.
func TestResolveNodeConfigContested(t *testing.T) {
	specific := func(name, key, value string) *unstructured.Unstructured {
		return makeLBNA(name, map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{key: value},
			},
			"local": map[string]interface{}{"localInterface": "default"},
		})
	}

	labels := map[string]string{"role": "edge", "tier": "front"}

	override := decodeLBNodeAgents(lbnaListOf(
		makeLBNA("catchall", localSpec("default")),
		specific("scoped", "role", "edge"),
	))
	cfg := resolveNodeConfig(override, labels)
	if cfg.Contested {
		t.Errorf("specific-over-catch-all must not be contested: %+v", cfg)
	}
	if len(cfg.Ignored) != 1 {
		t.Errorf("expected the catch-all to be listed as overridden, got %v", cfg.Ignored)
	}

	collision := decodeLBNodeAgents(lbnaListOf(
		specific("aaa-by-role", "role", "edge"),
		specific("zzz-by-tier", "tier", "front"),
	))
	cfg = resolveNodeConfig(collision, labels)
	if !cfg.Contested {
		t.Errorf("two specific selectors must be contested: %+v", cfg)
	}
	if cfg.Agent != "purelb-system/aaa-by-role" {
		t.Errorf("winner = %q, want the name-sorted first", cfg.Agent)
	}

	// An empty selector is a catch-all, so it must not make a node contested.
	withEmpty := decodeLBNodeAgents(lbnaListOf(
		makeLBNA("empty-selector", map[string]interface{}{
			"nodeSelector": map[string]interface{}{},
			"local":        map[string]interface{}{"localInterface": "default"},
		}),
		specific("scoped", "role", "edge"),
	))
	if cfg := resolveNodeConfig(withEmpty, labels); cfg.Contested {
		t.Errorf("empty selector is a catch-all, must not contest: %+v", cfg)
	}
}

func epSlice(svcName string, entries ...struct {
	node          string
	ready         bool
	serving       bool
	nodeNameIsNil bool
}) discoveryv1.EndpointSlice {
	slice := discoveryv1.EndpointSlice{}
	slice.Labels = map[string]string{"kubernetes.io/service-name": svcName}
	for _, e := range entries {
		ep := discoveryv1.Endpoint{
			Conditions: discoveryv1.EndpointConditions{
				Ready:   ptr.To(e.ready),
				Serving: ptr.To(e.serving),
			},
		}
		if !e.nodeNameIsNil {
			node := e.node
			ep.NodeName = &node
		}
		slice.Endpoints = append(slice.Endpoints, ep)
	}
	return slice
}

type epEntry = struct {
	node          string
	ready         bool
	serving       bool
	nodeNameIsNil bool
}

func TestPreferredNodesForService(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{
		epSlice("web",
			epEntry{node: "node-a", ready: true, serving: true},
			epEntry{node: "node-b", ready: false, serving: false}, // neither: skipped
			epEntry{node: "node-c", ready: false, serving: true},  // serving only: kept
			epEntry{node: "node-a", ready: true, serving: true},   // duplicate node
			epEntry{nodeNameIsNil: true, ready: true, serving: true},
		),
	}

	t.Run("no annotation means no affinity", func(t *testing.T) {
		if got := preferredNodesForService(map[string]string{}, slices); got != nil {
			t.Errorf("expected nil without the annotation, got %v", got)
		}
	})

	t.Run("ready or serving, deduplicated, nil node skipped", func(t *testing.T) {
		got := preferredNodesForService(
			map[string]string{annotationNodeAffinity: nodeAffinityServiceEndpoints}, slices)
		if len(got) != 2 || got[0] != "node-a" || got[1] != "node-c" {
			t.Errorf("preferred = %v, want [node-a node-c]", got)
		}
	})

	t.Run("shared IPs opt out", func(t *testing.T) {
		got := preferredNodesForService(map[string]string{
			annotationNodeAffinity: nodeAffinityServiceEndpoints,
			annotationSharing:      "shared-key",
		}, slices)
		if got != nil {
			t.Errorf("shared services must not use affinity, got %v", got)
		}
	})
}

// TestPredictWinner covers the bug where inspect predicted the plain-hash
// winner for affinity services and then reported a false
// "DOES NOT MATCH ANNOUNCER".
func TestPredictWinner(t *testing.T) {
	candidates := []string{"node-a", "node-b", "node-c"}

	t.Run("no affinity uses the plain hash", func(t *testing.T) {
		got, applied, fellBack := predictWinner("10.0.0.1", candidates, nil)
		if got != electionWinner("10.0.0.1", candidates) {
			t.Errorf("winner = %q, want the plain-hash winner", got)
		}
		if applied || fellBack {
			t.Errorf("flags = (%v,%v), want (false,false)", applied, fellBack)
		}
	})

	t.Run("affinity narrows the candidate set", func(t *testing.T) {
		got, applied, fellBack := predictWinner("10.0.0.1", candidates, []string{"node-c"})
		if got != "node-c" {
			t.Errorf("winner = %q, want node-c (the only preferred candidate)", got)
		}
		if !applied || fellBack {
			t.Errorf("flags = (%v,%v), want (true,false)", applied, fellBack)
		}
	})

	t.Run("preferred node that cannot serve the subnet falls back", func(t *testing.T) {
		got, applied, fellBack := predictWinner("10.0.0.1", candidates, []string{"node-z"})
		if got != electionWinner("10.0.0.1", candidates) {
			t.Errorf("winner = %q, want the plain-hash winner on fallback", got)
		}
		if applied || !fellBack {
			t.Errorf("flags = (%v,%v), want (false,true)", applied, fellBack)
		}
	})

	t.Run("no candidates", func(t *testing.T) {
		if got, _, _ := predictWinner("10.0.0.1", nil, []string{"node-a"}); got != "" {
			t.Errorf("winner = %q, want empty", got)
		}
	})
}
