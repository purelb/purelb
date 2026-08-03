// Copyright 2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// defaultDummyInterface is the dummy interface name assumed when no
// LBNodeAgent sets one (matches the CRD default).
const defaultDummyInterface = "kube-lb0"

// Announcement configuration states for a node. These mirror the states the
// node agent itself reports via purelb_lbnodeagent_selector_state, but are
// derived from the API so the plugin needs no access to node metrics ports.
const (
	// configStateConfigured: an LBNodeAgent with a local spec governs this node.
	configStateConfigured = "configured"
	// configStateDeselected: LBNodeAgents exist but none selects this node, so
	// it announces nothing and advertises no subnets.
	configStateDeselected = "deselected"
	// configStateRemote: the governing LBNodeAgent has no local spec, so this
	// node announces remote addresses only.
	configStateRemote = "remote"
	// configStateInvalid: the governing localInterface is not a valid regex, so
	// the agent rejects the config and announces nothing.
	configStateInvalid = "invalid"
	// configStateNoConfig: no LBNodeAgent resources exist at all.
	configStateNoConfig = "no-config"
)

// decodeLBNodeAgents converts dynamic-client objects into typed LBNodeAgents
// so the plugin can call purelbv2.AgentsForNode — the very function the node
// agents use to pick their configuration — instead of reimplementing selector
// precedence here, where it could drift. Objects that fail to decode are
// skipped rather than failing the whole command.
func decodeLBNodeAgents(list *unstructured.UnstructuredList) []*purelbv2.LBNodeAgent {
	if list == nil {
		return nil
	}
	agents := make([]*purelbv2.LBNodeAgent, 0, len(list.Items))
	for i := range list.Items {
		agent := &purelbv2.LBNodeAgent{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, agent); err != nil {
			continue
		}
		agents = append(agents, agent)
	}
	return agents
}

// agentName renders an LBNodeAgent's namespace/name for display.
func agentName(a *purelbv2.LBNodeAgent) string {
	if a == nil {
		return ""
	}
	if a.Namespace == "" {
		return a.Name
	}
	return a.Namespace + "/" + a.Name
}

// nodeConfig describes which LBNodeAgent governs a node, and what that means
// for announcement.
type nodeConfig struct {
	State string `json:"state"`
	// Agent is the resource providing the local spec (or the
	// highest-precedence match when none has one).
	Agent string `json:"agent,omitempty"`
	// Ignored lists lower-precedence matches, which the agent logs and events.
	Ignored []string `json:"ignored,omitempty"`
	// Contested is true when more than one *specific* selector matched, so
	// precedence fell through to namespace/name order — arbitrary from an
	// operator's point of view and worth a warning. A specific selector
	// overriding a catch-all is NOT contested: that is the documented
	// default-plus-override pattern and resolves unambiguously.
	Contested bool   `json:"contested,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// announces reports whether a node in this state announces local addresses.
func (c nodeConfig) announces() bool {
	return c.State == configStateConfigured
}

// resolveNodeConfig determines the announcement configuration for one node,
// mirroring the node agent's own resolution: filter by nodeSelector with
// AgentsForNode (specific selectors beat catch-alls, then namespace/name
// order), then take the first match carrying a local spec.
func resolveNodeConfig(agents []*purelbv2.LBNodeAgent, nodeLabels map[string]string) nodeConfig {
	if len(agents) == 0 {
		return nodeConfig{State: configStateNoConfig, Reason: "no LBNodeAgent resources exist"}
	}

	matched := purelbv2.AgentsForNode(agents, nodeLabels)
	if len(matched) == 0 {
		return nodeConfig{
			State:  configStateDeselected,
			Reason: "no LBNodeAgent nodeSelector matches this node",
		}
	}

	cfg := nodeConfig{}
	for _, extra := range matched[1:] {
		cfg.Ignored = append(cfg.Ignored, agentName(extra))
	}

	// Classify selectors exactly as AgentsForNode does, so "contested" means
	// the same thing here as the precedence rule it describes.
	specific := 0
	for _, m := range matched {
		if !purelbv2.IsCatchAll(m.Spec.NodeSelector) {
			specific++
		}
	}
	cfg.Contested = specific > 1

	local := purelbv2.FirstLocalAgent(matched)
	if local == nil {
		cfg.State = configStateRemote
		cfg.Agent = agentName(matched[0])
		cfg.Reason = "matching LBNodeAgent has no local spec"
		return cfg
	}

	cfg.Agent = agentName(local)
	if invalid, _ := localInterfaceIssues(local.Spec.Local.LocalInterface); invalid {
		cfg.State = configStateInvalid
		cfg.Reason = fmt.Sprintf("localInterface %q is not a valid regex", local.Spec.Local.LocalInterface)
		return cfg
	}

	cfg.State = configStateConfigured
	return cfg
}

// localInterfaceIssues classifies a localInterface value. "default" (and the
// empty value, which the agent treats as "default") selects the default-route
// interface. Anything else is a regex which both the announcer and the
// election match UNANCHORED, so "eth" also matches "veth1a2b3c" and would pull
// pod interfaces into subnet detection.
func localInterfaceIssues(pattern string) (invalid, unanchored bool) {
	if pattern == "" || pattern == "default" {
		return false, false
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return true, false
	}
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		return false, true
	}
	return false, false
}

// dummyInterfaces returns the distinct dummy interface names configured across
// the given agents, in deterministic order. Callers previously read item [0]
// of the LBNodeAgent list, which is arbitrary once more than one resource
// exists.
func dummyInterfaces(agents []*purelbv2.LBNodeAgent) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, a := range agents {
		if a.Spec.Local == nil {
			continue
		}
		name := a.Spec.Local.DummyInterface
		if name == "" {
			name = defaultDummyInterface
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return []string{defaultDummyInterface}
	}
	sort.Strings(names)
	return names
}

// dummyInterfaceForNode returns the dummy interface that applies to one node,
// resolved with the same precedence the node agent uses.
func dummyInterfaceForNode(agents []*purelbv2.LBNodeAgent, nodeLabels map[string]string) string {
	if local := purelbv2.FirstLocalAgent(purelbv2.AgentsForNode(agents, nodeLabels)); local != nil {
		if name := local.Spec.Local.DummyInterface; name != "" {
			return name
		}
	}
	return defaultDummyInterface
}

// matchesNode reports whether a single agent's nodeSelector selects a node.
func matchesNode(agent *purelbv2.LBNodeAgent, nodeLabels map[string]string) bool {
	return len(purelbv2.AgentsForNode([]*purelbv2.LBNodeAgent{agent}, nodeLabels)) > 0
}

// preferredNodesForService returns the nodes the announcer biases toward for
// this service, mirroring buildPreferredNodes in internal/local: only when the
// node-affinity annotation asks for it, never for shared IPs (where one
// address serves several services with different endpoints), and counting an
// endpoint whose node is known and which is Ready or Serving. Returns nil when
// affinity does not apply, which makes the caller's election plain.
func preferredNodesForService(ann map[string]string, slices []discoveryv1.EndpointSlice) []string {
	if ann[annotationNodeAffinity] != nodeAffinityServiceEndpoints {
		return nil
	}
	if _, shared := ann[annotationSharing]; shared {
		return nil
	}
	seen := map[string]struct{}{}
	var preferred []string
	for i := range slices {
		for _, ep := range slices[i].Endpoints {
			if ep.NodeName == nil {
				continue
			}
			ready := ep.Conditions.Ready != nil && *ep.Conditions.Ready
			serving := ep.Conditions.Serving != nil && *ep.Conditions.Serving
			if !ready && !serving {
				continue
			}
			if _, dup := seen[*ep.NodeName]; dup {
				continue
			}
			seen[*ep.NodeName] = struct{}{}
			preferred = append(preferred, *ep.NodeName)
		}
	}
	return preferred
}

// predictWinner reproduces the announcer's election for one address:
// WinnerWithPreference narrows the candidates to the preferred set when that
// intersection is non-empty, otherwise it falls back to the full set. Ignoring
// affinity here made the plugin predict the wrong winner for every
// affinity-enabled service and report a false split-brain.
func predictWinner(ip string, candidates, preferred []string) (winner string, affinityApplied, affinityFellBack bool) {
	if len(candidates) == 0 {
		return "", false, false
	}
	if len(preferred) > 0 {
		prefSet := make(map[string]struct{}, len(preferred))
		for _, n := range preferred {
			prefSet[n] = struct{}{}
		}
		var narrowed []string
		for _, c := range candidates {
			if _, ok := prefSet[c]; ok {
				narrowed = append(narrowed, c)
			}
		}
		if len(narrowed) > 0 {
			return electionWinner(ip, narrowed), true, false
		}
		// Preferred nodes exist but none can serve this address.
		return electionWinner(ip, candidates), false, true
	}
	return electionWinner(ip, candidates), false, false
}
