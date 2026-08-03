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
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type checkResult struct {
	Status  string `json:"status"` // PASS, WARN, FAIL
	Message string `json:"message"`
}

type validateSummary struct {
	Checks []checkResult `json:"checks"`
	Pass   int           `json:"pass"`
	Warn   int           `json:"warn"`
	Fail   int           `json:"fail"`
}

func newValidateCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var strict bool

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check ServiceGroup, LBNodeAgent, and BGPConfiguration consistency",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runValidate(cmd.Context(), c, format, strict)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().BoolVar(&strict, "strict", false, "Treat warnings as failures (for CI/CD)")

	return cmd
}

func runValidate(ctx context.Context, c *clients, format outputFormat, strict bool) error {
	var checks []checkResult

	// Fetch resources
	sgList, err := c.dynamic.Resource(gvrServiceGroups).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing ServiceGroups: %w", err)
	}

	lbnaList, err := c.dynamic.Resource(gvrLBNodeAgents).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing LBNodeAgents: %w", err)
	}

	bgpConfigList, _ := c.dynamic.Resource(gvrBGPConfigurations).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})

	leaseList, _ := c.core.CoordinationV1().Leases(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})

	nodeList, _ := c.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{ResourceVersion: "0"})

	// Build node subnets from leases
	nodeSubnets := map[string][]string{}
	if leaseList != nil {
		for _, lease := range leaseList.Items {
			if !strings.HasPrefix(lease.Name, leasePrefix) {
				continue
			}
			nodeName := lease.Name[len(leasePrefix):]
			subs := parseSubnetsAnnotation(lease.GetAnnotations()[subnetsAnnotation])
			nodeSubnets[nodeName] = subs
		}
	}
	allSubnets := map[string]bool{}
	for _, subs := range nodeSubnets {
		for _, s := range subs {
			allSubnets[s] = true
		}
	}

	// Dummy interface names configured across all LBNodeAgents. With
	// nodeSelector-scoped resources different nodes can use different names,
	// so BGP netlinkImport has to cover every one of them; reading only the
	// first resource in list order would check an arbitrary node's name.
	agents := decodeLBNodeAgents(lbnaList)
	dummyInterfaceNames := dummyInterfaces(agents)

	// ========== Pool checks ==========
	hasRemotePool := false
	type rangeEntry struct {
		sg     string
		pool   string
		ipr    ipRange
		family string
	}
	var allRanges []rangeEntry

	sgCount := 0
	for _, sg := range sgList.Items {
		sgName := sg.GetName()
		sgCount++
		spec, _, _ := unstructured.NestedMap(sg.Object, "spec")
		if spec == nil {
			checks = append(checks, checkResult{"FAIL", fmt.Sprintf("ServiceGroup %q: empty spec", sgName)})
			continue
		}

		for _, poolKey := range []string{"local", "remote"} {
			poolSpec, ok := spec[poolKey]
			if !ok {
				continue
			}
			if poolKey == "remote" {
				hasRemotePool = true
			}

			ps, ok := poolSpec.(map[string]interface{})
			if !ok {
				continue
			}

			// Check multiPool + balancePools conflict
			multiPool, _, _ := unstructured.NestedBool(ps, "multiPool")
			balancePools, _, _ := unstructured.NestedBool(ps, "balancePools")
			if multiPool && balancePools {
				checks = append(checks, checkResult{"FAIL", fmt.Sprintf("ServiceGroup %q: multiPool and balancePools are mutually exclusive", sgName)})
			}

			// Extract and validate ranges
			for _, key := range []string{"v4pools", "v6pools"} {
				poolsRaw, ok := ps[key]
				if !ok {
					continue
				}
				pools, ok := poolsRaw.([]interface{})
				if !ok {
					continue
				}
				family := "IPv4"
				if strings.HasPrefix(key, "v6") {
					family = "IPv6"
				}

				for _, pRaw := range pools {
					p, ok := pRaw.(map[string]interface{})
					if !ok {
						continue
					}
					poolStr, _ := p["pool"].(string)
					subnet, _ := p["subnet"].(string)

					ipr, err := newIPRange(poolStr)
					if err != nil {
						checks = append(checks, checkResult{"FAIL", fmt.Sprintf("ServiceGroup %q: invalid pool range %q: %v", sgName, poolStr, err)})
						continue
					}

					// Validate range contained by subnet
					if subnet != "" {
						_, ipnet, err := net.ParseCIDR(subnet)
						if err != nil {
							checks = append(checks, checkResult{"FAIL", fmt.Sprintf("ServiceGroup %q: invalid subnet %q", sgName, subnet)})
						} else {
							if !ipnet.Contains(ipr.from) || !ipnet.Contains(ipr.to) {
								checks = append(checks, checkResult{"FAIL", fmt.Sprintf("ServiceGroup %q: range %s not contained by subnet %s", sgName, poolStr, subnet)})
							}
						}

						// Check subnet coverage (local pools only)
						if poolKey == "local" && !allSubnets[subnet] {
							checks = append(checks, checkResult{"WARN", fmt.Sprintf("ServiceGroup %q: range %s (subnet %s) not covered by any node", sgName, poolStr, subnet)})
						}
					}

					allRanges = append(allRanges, rangeEntry{sg: sgName, pool: poolStr, ipr: ipr, family: family})
				}
			}
		}

		if _, ok := spec["netbox"]; ok {
			checks = append(checks, checkResult{"PASS", fmt.Sprintf("ServiceGroup %q: Netbox config present (URL reachability not checked)", sgName)})
		}

		if spec["local"] == nil && spec["remote"] == nil && spec["netbox"] == nil {
			checks = append(checks, checkResult{"FAIL", fmt.Sprintf("ServiceGroup %q: no local, remote, or netbox spec", sgName)})
		}
	}

	// Check for overlapping ranges between ServiceGroups
	for i := 0; i < len(allRanges); i++ {
		for j := i + 1; j < len(allRanges); j++ {
			a, b := allRanges[i], allRanges[j]
			if a.family != b.family {
				continue
			}
			// Check overlap: a.from <= b.to && b.from <= a.to
			if a.ipr.contains(b.ipr.from) || a.ipr.contains(b.ipr.to) ||
				b.ipr.contains(a.ipr.from) || b.ipr.contains(a.ipr.to) {
				if a.sg != b.sg {
					checks = append(checks, checkResult{"FAIL", fmt.Sprintf("Overlapping ranges: %s/%s and %s/%s", a.sg, a.pool, b.sg, b.pool)})
				}
			}
		}
	}
	if len(allRanges) > 1 {
		// Only report if we actually checked
		overlap := false
		for _, ch := range checks {
			if strings.Contains(ch.Message, "Overlapping") {
				overlap = true
				break
			}
		}
		if !overlap {
			checks = append(checks, checkResult{"PASS", "No overlapping pool ranges detected"})
		}
	}

	// ========== LBNodeAgent checks ==========
	if len(agents) == 0 {
		checks = append(checks, checkResult{"WARN", "No LBNodeAgent configured"})
	} else {
		for _, agent := range agents {
			name := agentName(agent)

			// A nodeSelector that matches nothing is dead configuration:
			// the resource exists but governs no node.
			matchCount := 0
			if nodeList != nil {
				for i := range nodeList.Items {
					if matchesNode(agent, nodeList.Items[i].Labels) {
						matchCount++
					}
				}
			}
			switch {
			case nodeList == nil:
				checks = append(checks, checkResult{"PASS", fmt.Sprintf("LBNodeAgent %q: configured", name)})
			case matchCount == 0:
				checks = append(checks, checkResult{"WARN", fmt.Sprintf(
					"LBNodeAgent %q: nodeSelector matches 0 of %d nodes (configuration has no effect)", name, len(nodeList.Items))})
			default:
				scope := "all nodes"
				if agent.Spec.NodeSelector != nil {
					scope = fmt.Sprintf("%d of %d nodes", matchCount, len(nodeList.Items))
				}
				checks = append(checks, checkResult{"PASS", fmt.Sprintf("LBNodeAgent %q: configured, selects %s", name, scope)})
			}

			// localInterface is a regex unless it is "default", and both the
			// announcer and the election match it UNANCHORED.
			if agent.Spec.Local != nil {
				pattern := agent.Spec.Local.LocalInterface
				invalid, unanchored := localInterfaceIssues(pattern)
				switch {
				case invalid:
					checks = append(checks, checkResult{"FAIL", fmt.Sprintf(
						"LBNodeAgent %q: localInterface %q is not a valid regex — selected nodes announce nothing", name, pattern)})
				case unanchored:
					checks = append(checks, checkResult{"WARN", fmt.Sprintf(
						"LBNodeAgent %q: localInterface %q is unanchored — it matches any interface name containing %q, including pod veth interfaces; anchor it as %q",
						name, pattern, pattern, "^"+pattern+"$")})
				}
			}
		}

		// Nodes that no LBNodeAgent selects announce nothing at all. This is
		// legitimate (deliberately excluding a node) but silent otherwise.
		if nodeList != nil {
			var orphans, overridden, contested []string
			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				cfg := resolveNodeConfig(agents, node.Labels)
				if cfg.State == configStateDeselected {
					orphans = append(orphans, node.Name)
				}
				if len(cfg.Ignored) == 0 {
					continue
				}
				detail := fmt.Sprintf("%s (using %s, over %s)", node.Name, cfg.Agent, strings.Join(cfg.Ignored, ", "))
				if cfg.Contested {
					contested = append(contested, detail)
				} else {
					overridden = append(overridden, detail)
				}
			}
			if len(orphans) > 0 {
				checks = append(checks, checkResult{"WARN", fmt.Sprintf(
					"%d node(s) selected by no LBNodeAgent, announcing nothing: %s", len(orphans), strings.Join(orphans, ", "))})
			}
			// A specific selector beating a catch-all is the documented
			// default-plus-override pattern: report which resource governs,
			// but do not warn — otherwise a correct configuration could never
			// validate clean, and --strict would fail CI on it.
			if len(overridden) > 0 {
				checks = append(checks, checkResult{"PASS", fmt.Sprintf(
					"%d node(s) using a scoped LBNodeAgent override: %s", len(overridden), strings.Join(overridden, "; "))})
			}
			// Two specific selectors matching one node is genuinely ambiguous:
			// the winner is decided only by namespace/name sort order.
			if len(contested) > 0 {
				checks = append(checks, checkResult{"WARN", fmt.Sprintf(
					"%d node(s) matched by multiple specific nodeSelectors, resolved by name order: %s",
					len(contested), strings.Join(contested, "; "))})
			}
		}
	}

	// ========== BGP checks ==========
	if bgpConfigList != nil && len(bgpConfigList.Items) > 0 {
		for _, bgpCfg := range bgpConfigList.Items {
			cfgName := bgpCfg.GetName()

			// Check netlinkImport
			importEnabled, _, _ := unstructured.NestedBool(bgpCfg.Object, "spec", "netlinkImport", "enabled")
			importInterfaces, _, _ := unstructured.NestedStringSlice(bgpCfg.Object, "spec", "netlinkImport", "interfaceList")

			if !importEnabled {
				checks = append(checks, checkResult{"WARN", fmt.Sprintf("BGPConfiguration %q: netlinkImport disabled (k8gobgp will advertise NO routes)", cfgName)})
			} else {
				// Every dummy interface in use must be watched, not just one:
				// nodeSelector-scoped LBNodeAgents may configure different
				// names on different nodes.
				watched := map[string]bool{}
				for _, iface := range importInterfaces {
					watched[iface] = true
				}
				var missing []string
				for _, name := range dummyInterfaceNames {
					if !watched[name] {
						missing = append(missing, name)
					}
				}
				if len(missing) == 0 {
					checks = append(checks, checkResult{"PASS", fmt.Sprintf("BGPConfiguration %q: netlinkImport enabled, watching %v (covers LBNodeAgent dummyInterface(s) %v)", cfgName, importInterfaces, dummyInterfaceNames)})
				} else {
					checks = append(checks, checkResult{"FAIL", fmt.Sprintf("BGPConfiguration %q: netlinkImport interfaces %v do not include LBNodeAgent dummyInterface(s) %v", cfgName, importInterfaces, missing)})
				}
			}

			// Check neighbors with nodeSelectors
			neighbors, _, _ := unstructured.NestedSlice(bgpCfg.Object, "spec", "neighbors")
			for _, nRaw := range neighbors {
				n, ok := nRaw.(map[string]interface{})
				if !ok {
					continue
				}
				config, ok := n["config"].(map[string]interface{})
				if !ok {
					continue
				}
				addr, _ := config["neighborAddress"].(string)
				peerASN, _ := config["peerAsn"].(int64)

				// Check if nodeSelector matches any nodes. Full label-selector
				// semantics: the previous matchLabels-only comparison silently
				// treated a matchExpressions-based selector as matching
				// everything, hiding a neighbor that reaches no node.
				selectorRaw, hasSelector := n["nodeSelector"]
				if hasSelector && selectorRaw != nil && nodeList != nil {
					count, err := countMatchingNodes(selectorRaw, nodeList.Items)
					switch {
					case err != nil:
						checks = append(checks, checkResult{"FAIL", fmt.Sprintf("BGPConfiguration %q: neighbor %s (AS %d) has an invalid nodeSelector: %v", cfgName, addr, peerASN, err)})
					case count == 0:
						checks = append(checks, checkResult{"FAIL", fmt.Sprintf("BGPConfiguration %q: neighbor %s (AS %d) nodeSelector matches 0 nodes", cfgName, addr, peerASN)})
					}
				}
			}
		}
	} else if hasRemotePool {
		checks = append(checks, checkResult{"FAIL", "Remote ServiceGroups exist but no BGPConfiguration is deployed"})
	}

	// ========== Summary ==========
	summary := validateSummary{Checks: checks}
	for _, ch := range checks {
		switch ch.Status {
		case "PASS":
			summary.Pass++
		case "WARN":
			summary.Warn++
		case "FAIL":
			summary.Fail++
		}
	}

	if format != outputTable {
		return printStructured(format, summary)
	}

	// Table output
	fmt.Printf("Checking %d ServiceGroup(s), %d LBNodeAgent(s)",
		sgCount, len(lbnaList.Items))
	if bgpConfigList != nil && len(bgpConfigList.Items) > 0 {
		fmt.Printf(", %d BGPConfiguration(s)", len(bgpConfigList.Items))
	}
	fmt.Println("...")
	fmt.Println()

	tw := tableWriter(os.Stdout)
	for _, ch := range checks {
		fmt.Fprintf(tw, "%s\t%s\n", ch.Status, ch.Message)
	}
	tw.Flush()

	fmt.Printf("\nResult: %d FAIL, %d WARN, %d PASS\n", summary.Fail, summary.Warn, summary.Pass)

	if strict && (summary.Fail > 0 || summary.Warn > 0) {
		return fmt.Errorf("validation failed (strict mode): %d failures, %d warnings", summary.Fail, summary.Warn)
	}
	if summary.Fail > 0 {
		return fmt.Errorf("validation failed: %d failures", summary.Fail)
	}

	return nil
}

// labelsMatch checks if a node's labels contain all the required matchLabels.
// countMatchingNodes evaluates an unstructured label selector against the
// given nodes with full Kubernetes semantics (matchLabels AND
// matchExpressions), so selectors the plugin cannot interpret surface as an
// error rather than silently matching everything.
func countMatchingNodes(selectorRaw interface{}, nodes []v1.Node) (int, error) {
	raw, ok := selectorRaw.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("nodeSelector is not an object")
	}
	labelSelector := &metav1.LabelSelector{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, labelSelector); err != nil {
		return 0, err
	}
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range nodes {
		if selector.Matches(labels.Set(nodes[i].Labels)) {
			count++
		}
	}
	return count, nil
}
