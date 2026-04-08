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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	// Get dummy interface name from LBNodeAgent (using already-fetched lbnaList)
	dummyInterface := "kube-lb0"
	if len(lbnaList.Items) > 0 {
		if di, _, _ := unstructured.NestedString(lbnaList.Items[0].Object, "spec", "local", "dummyInterface"); di != "" {
			dummyInterface = di
		}
	}

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
	if len(lbnaList.Items) == 0 {
		checks = append(checks, checkResult{"WARN", "No LBNodeAgent configured"})
	} else {
		for _, lbna := range lbnaList.Items {
			checks = append(checks, checkResult{"PASS", fmt.Sprintf("LBNodeAgent %q: configured", lbna.GetName())})
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
				// Check interface match
				found := false
				for _, iface := range importInterfaces {
					if iface == dummyInterface {
						found = true
						break
					}
				}
				if found {
					checks = append(checks, checkResult{"PASS", fmt.Sprintf("BGPConfiguration %q: netlinkImport enabled, watching %v (matches LBNodeAgent dummyInterface %q)", cfgName, importInterfaces, dummyInterface)})
				} else {
					checks = append(checks, checkResult{"FAIL", fmt.Sprintf("BGPConfiguration %q: netlinkImport interfaces %v do not include LBNodeAgent dummyInterface %q", cfgName, importInterfaces, dummyInterface)})
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

				// Check if nodeSelector matches any nodes
				selectorRaw, hasSelector := n["nodeSelector"]
				if hasSelector && selectorRaw != nil && nodeList != nil {
					matchCount := 0
					selector, ok := selectorRaw.(map[string]interface{})
					if ok {
						matchLabels, _ := selector["matchLabels"].(map[string]interface{})
						if matchLabels != nil {
							for _, node := range nodeList.Items {
								if labelsMatch(node.Labels, matchLabels) {
									matchCount++
								}
							}
						}
					}
					if matchCount == 0 {
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
func labelsMatch(nodeLabels map[string]string, matchLabels map[string]interface{}) bool {
	for k, v := range matchLabels {
		expected, _ := v.(string)
		if nodeLabels[k] != expected {
			return false
		}
	}
	return true
}
