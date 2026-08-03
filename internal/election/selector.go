// Copyright 2020-2026 Acnodal Inc.
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

package election

import (
	"fmt"
	"net"
	"regexp"
	"sort"

	"github.com/go-kit/log"

	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// lastSelectedInterfaces tracks the previously selected interface
// names so we can log at info level on first selection or change,
// debug on repeats. Like lastSubnets, it is safe only because the
// election closure is the single caller (Start happens-before the
// renewLoop goroutine).
var lastSelectedInterfaces string

// InterfaceSelector describes which interfaces feed the election's
// subnet detection. It is derived from the same LBNodeAgent Local spec
// the announcer uses, so the lease advertises exactly the subnets the
// announcer can announce on (the coherence invariant).
//
// The zero value (&InterfaceSelector{}) selects nothing: the node
// advertises no subnets and is a candidate for nothing. This is
// distinct from a nil *InterfaceSelector, which means "no
// configuration" and falls back to default-interface detection.
type InterfaceSelector struct {
	// UseDefault includes the default-route interface(s), i.e.
	// localInterface "default" (or "", which is treated as "default").
	UseDefault bool

	// Regex is the compiled localInterface pattern when it is not
	// "default". Matching is UNANCHORED, identical to the announcer's
	// findLocal: "eth" matches "veth0".
	Regex *regexp.Regexp

	// Interfaces are additional exact interface names (spec.interfaces),
	// tried in addition to whatever UseDefault/Regex select.
	Interfaces []string

	// ExcludeName is the dummy interface name (e.g. "kube-lb0"). It
	// carries remote VIPs whose subnets must not enter the election.
	ExcludeName string
}

// SelectorFromConfig derives an InterfaceSelector from cfg using the
// same agent the announcer picks (FirstLocalAgent). Returns (nil, nil)
// when no agent has a Local spec — remote-only clusters keep today's
// default-interface lease behavior. Returns an error when the
// localInterface regex does not compile; the announcer fails its
// SetConfig on the same input, so the caller must store an empty
// selector to stay coherent with an announcer that announces nothing.
func SelectorFromConfig(cfg *purelbv2.Config) (*InterfaceSelector, error) {
	agent := purelbv2.FirstLocalAgent(cfg.Agents)
	if agent == nil {
		return nil, nil
	}
	spec := agent.Spec.Local

	sel := &InterfaceSelector{
		Interfaces:  spec.Interfaces,
		ExcludeName: spec.DummyInterface,
	}

	// "" can only appear when an object bypassed kubebuilder defaulting;
	// treat it as "default" (the announcer does the same) rather than
	// compiling it into a match-everything regex.
	if spec.LocalInterface == "default" || spec.LocalInterface == "" {
		sel.UseDefault = true
		return sel, nil
	}

	regex, err := regexp.Compile(spec.LocalInterface)
	if err != nil {
		return nil, fmt.Errorf("error compiling regex %q: %w", spec.LocalInterface, err)
	}
	sel.Regex = regex
	return sel, nil
}

// GetSelectedSubnets returns the subnets the election should advertise
// for sel. nil sel falls back to default-interface detection
// (byte-identical to the pre-config behavior); an empty sel selects
// nothing. Everything funnels through GetLocalSubnets so
// deduplication, sorting, IPv6 bad-flag filtering, missing-interface
// tolerance and log throttling are shared with the historical path.
func GetSelectedSubnets(sel *InterfaceSelector, logger log.Logger) ([]string, error) {
	if sel == nil {
		return GetLocalSubnets(nil, true, logger)
	}
	if !sel.UseDefault && sel.Regex == nil && len(sel.Interfaces) == 0 {
		return []string{}, nil
	}

	names := selectedInterfaceNames(sel)

	// Log the selected names (not just the resulting subnets) so an
	// under-anchored regex that catches veth/bridge interfaces is
	// visible at info level.
	if logger != nil {
		formatted := fmt.Sprintf("%v(useDefault=%t)", names, sel.UseDefault)
		if formatted != lastSelectedInterfaces {
			lastSelectedInterfaces = formatted
			logging.Info(logger, "op", "getSelectedSubnets", "interfaces", formatted,
				"msg", "interface selection changed")
		} else {
			logging.Debug(logger, "op", "getSelectedSubnets", "interfaces", formatted,
				"msg", "interface selection unchanged")
		}
	}

	return GetLocalSubnets(names, sel.UseDefault, logger)
}

// selectedInterfaceNames computes the explicit interface-name set for
// sel: the exact names from Interfaces (minus ExcludeName) plus the
// regex matches over the system's interfaces (minus ExcludeName and
// loopback — loopback is excluded from regex enumeration only, so an
// explicit "lo" entry still works, which keeps CI tests viable). The
// result is deduplicated and sorted: ifindex enumeration order is
// boot-dependent, and deterministic order keeps logs and scans stable
// across reboots.
func selectedInterfaceNames(sel *InterfaceSelector) []string {
	nameSet := make(map[string]struct{})

	for _, name := range sel.Interfaces {
		if name == "" || name == sel.ExcludeName {
			continue
		}
		nameSet[name] = struct{}{}
	}

	if sel.Regex != nil {
		if interfaces, err := net.Interfaces(); err == nil {
			candidates := make([]string, 0, len(interfaces))
			for _, intf := range interfaces {
				if intf.Flags&net.FlagLoopback != 0 {
					continue
				}
				candidates = append(candidates, intf.Name)
			}
			for _, name := range matchInterfaceNames(sel.Regex, sel.ExcludeName, candidates) {
				nameSet[name] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// matchInterfaceNames returns the names matching regex, excluding
// exclude. Matching is unanchored (regexp.Match), identical to the
// announcer's findLocal semantics.
func matchInterfaceNames(regex *regexp.Regexp, exclude string, names []string) []string {
	matched := make([]string, 0, len(names))
	for _, name := range names {
		if name == exclude {
			continue
		}
		if regex.Match([]byte(name)) {
			matched = append(matched, name)
		}
	}
	return matched
}
