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
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type nodeLeaseInfo struct {
	Name       string   `json:"name"`
	Subnets    []string `json:"subnets"`
	RenewedAgo string   `json:"renewedAgo"`
	ExpiresIn  string   `json:"expiresIn"`
	Healthy    bool     `json:"healthy"`
	Announcing int      `json:"announcing"`
}

type subnetCoverage struct {
	Subnet string   `json:"subnet"`
	Nodes  []string `json:"nodes"`
	Pools  []string `json:"pools"` // ServiceGroup ranges this subnet covers
}

type uncoveredRange struct {
	ServiceGroup string `json:"serviceGroup"`
	Pool         string `json:"pool"`
	Subnet       string `json:"subnet"`
}

type drainResult struct {
	Service   string `json:"service"`
	IP        string `json:"ip"`
	Current   string `json:"currentWinner"`
	NewWinner string `json:"newWinner"`
	Subnet    string `json:"subnet"`
	Warning   string `json:"warning,omitempty"`
}

type electionSummary struct {
	Nodes      []nodeLeaseInfo  `json:"nodes"`
	Coverage   []subnetCoverage `json:"subnetCoverage"`
	Uncovered  []uncoveredRange `json:"uncoveredRanges,omitempty"`
	DrainSim   []drainResult    `json:"drainSimulation,omitempty"`
	DrainNode  string           `json:"drainNode,omitempty"`
}

func newElectionCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var node string
	var check bool
	var simulateDrain string

	cmd := &cobra.Command{
		Use:   "election",
		Short: "Show election state, node leases, subnet coverage, and drain simulation",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runElection(cmd.Context(), c, format, node, check, simulateDrain)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&node, "node", "", "Show details for a single node")
	cmd.Flags().BoolVar(&check, "check", false, "Show only problems")
	cmd.Flags().StringVar(&simulateDrain, "simulate-drain", "", "Simulate draining a node and show service migration")

	return cmd
}

func runElection(ctx context.Context, c *clients, format outputFormat, filterNode string, checkOnly bool, drainNode string) error {
	now := time.Now()

	// Fetch leases
	leaseList, err := c.core.CoordinationV1().Leases(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing leases: %w", err)
	}

	// Parse leases into node info
	// Also build subnet -> nodes map
	var nodes []nodeLeaseInfo
	subnetNodes := map[string][]string{} // subnet CIDR -> node names
	nodeSubnets := map[string][]string{} // node name -> subnets

	for _, lease := range leaseList.Items {
		if !strings.HasPrefix(lease.Name, leasePrefix) {
			continue
		}
		nodeName := lease.Name[len(leasePrefix):]
		if filterNode != "" && nodeName != filterNode {
			continue
		}

		subnets := parseSubnetsAnnotation(lease.GetAnnotations()[subnetsAnnotation])
		healthy := isLeaseHealthy(&lease, now)

		renewedAgo := "-"
		expiresIn := "-"
		if lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil {
			age := now.Sub(lease.Spec.RenewTime.Time)
			renewedAgo = formatDuration(age)
			expiry := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
			if now.Before(expiry) {
				expiresIn = "in " + formatDuration(expiry.Sub(now))
			} else {
				expiresIn = "EXPIRED " + formatDuration(now.Sub(expiry)) + " ago"
			}
		}

		nodes = append(nodes, nodeLeaseInfo{
			Name:       nodeName,
			Subnets:    subnets,
			RenewedAgo: renewedAgo,
			ExpiresIn:  expiresIn,
			Healthy:    healthy,
		})

		nodeSubnets[nodeName] = subnets
		for _, s := range subnets {
			subnetNodes[s] = append(subnetNodes[s], nodeName)
		}
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	// Count announcing IPs per node from services
	svcList, err := c.core.CoreV1().Services("").List(ctx, metav1.ListOptions{ResourceVersion: "0", FieldSelector: svcFieldSelector})
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}

	announceCount := map[string]int{}
	// Also collect service IPs for drain simulation (only local pool services participate in election)
	type svcIPInfo struct {
		nsName string
		ip     string
	}
	var localSvcIPs []svcIPInfo

	for _, svc := range svcList.Items {
		ann := svc.Annotations
		if ann == nil || ann[annotationAllocatedBy] != brandPureLB {
			continue
		}
		for _, suffix := range []string{"-IPv4", "-IPv6"} {
			for _, a := range parseAnnouncingAnnotation(ann[annotationAnnouncing+suffix]) {
				if a.Node != "" {
					announceCount[a.Node]++
				}
			}
		}
		// Local pool services participate in election
		if ann[annotationPoolType] == poolTypeLocal {
			for _, ingress := range svc.Status.LoadBalancer.Ingress {
				if ingress.IP != "" {
					localSvcIPs = append(localSvcIPs, svcIPInfo{
						nsName: svc.Namespace + "/" + svc.Name,
						ip:     ingress.IP,
					})
				}
			}
		}
	}

	for i := range nodes {
		nodes[i].Announcing = announceCount[nodes[i].Name]
	}

	// Build subnet coverage: which ServiceGroup pool ranges are covered by which subnets
	sgList, err := c.dynamic.Resource(gvrServiceGroups).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing ServiceGroups: %w", err)
	}

	type poolSubnet struct {
		sgName string
		pool   string
		subnet string
	}
	var allPoolSubnets []poolSubnet

	for _, sg := range sgList.Items {
		spec, _, _ := unstructured.NestedMap(sg.Object, "spec")
		if spec == nil {
			continue
		}
		// Only local pools participate in election (remote pools are on kube-lb0, all nodes)
		if localSpec, ok := spec["local"]; ok {
			for _, ps := range extractPoolSubnets(sg.GetName(), localSpec) {
				allPoolSubnets = append(allPoolSubnets, ps)
			}
		}
	}

	// Build coverage and uncovered lists
	var coverage []subnetCoverage
	coveredSubnets := map[string]bool{}

	// All subnets seen in leases
	for subnet, nodeList := range subnetNodes {
		sort.Strings(nodeList)
		var pools []string
		for _, ps := range allPoolSubnets {
			if ps.subnet == subnet {
				pools = append(pools, fmt.Sprintf("%s %s", ps.sgName, ps.pool))
				coveredSubnets[ps.subnet] = true
			}
		}
		coverage = append(coverage, subnetCoverage{
			Subnet: subnet,
			Nodes:  nodeList,
			Pools:  pools,
		})
	}
	sort.Slice(coverage, func(i, j int) bool { return coverage[i].Subnet < coverage[j].Subnet })

	var uncovered []uncoveredRange
	for _, ps := range allPoolSubnets {
		if !coveredSubnets[ps.subnet] {
			uncovered = append(uncovered, uncoveredRange{
				ServiceGroup: ps.sgName,
				Pool:         ps.pool,
				Subnet:       ps.subnet,
			})
		}
	}

	// Drain simulation
	var drainResults []drainResult
	if drainNode != "" {
		for _, si := range localSvcIPs {
			ip := net.ParseIP(si.ip)
			if ip == nil {
				continue
			}
			// Find candidates: nodes with a subnet containing this IP
			var candidates []string
			for nodeName, subs := range nodeSubnets {
				if subnetContainsIP(subs, ip) {
					candidates = append(candidates, nodeName)
				}
			}

			currentWinner := electionWinner(si.ip, candidates)

			// Remove drain node from candidates
			var newCandidates []string
			for _, c := range candidates {
				if c != drainNode {
					newCandidates = append(newCandidates, c)
				}
			}

			newWinner := electionWinner(si.ip, newCandidates)

			// Find matching subnet
			matchedSubnet := ""
			for _, s := range nodeSubnets[currentWinner] {
				_, ipnet, err := net.ParseCIDR(s)
				if err == nil && ipnet.Contains(ip) {
					matchedSubnet = s
					break
				}
			}

			dr := drainResult{
				Service:   si.nsName,
				IP:        si.ip,
				Current:   currentWinner,
				NewWinner: newWinner,
				Subnet:    matchedSubnet,
			}
			if newWinner == "" {
				dr.Warning = "NO CANDIDATES"
			}
			drainResults = append(drainResults, dr)
		}
	}

	summary := electionSummary{
		Nodes:     nodes,
		Coverage:  coverage,
		Uncovered: uncovered,
		DrainSim:  drainResults,
		DrainNode: drainNode,
	}

	if format != outputTable {
		return printStructured(format, summary)
	}

	// Check-only mode: show only problems
	if checkOnly {
		hasProblems := false
		for _, n := range nodes {
			if !n.Healthy {
				fmt.Printf("UNHEALTHY: node %s lease expired (%s)\n", n.Name, n.ExpiresIn)
				hasProblems = true
			}
		}
		for _, u := range uncovered {
			fmt.Printf("UNCOVERED: %s range %s (subnet %s) - no nodes with this subnet\n",
				u.ServiceGroup, u.Pool, u.Subnet)
			hasProblems = true
		}
		if !hasProblems {
			fmt.Printf("All %d node(s) healthy, %d subnet(s) covered, no problems detected\n",
				len(nodes), len(coverage))
		}
		return nil
	}

	// Full table output
	tw := tableWriter(os.Stdout)
	fmt.Fprintf(tw, "NODE\tSUBNETS\tRENEWED\tEXPIRES\tHEALTHY\tANNOUNCING\n")
	for _, n := range nodes {
		subnets := strings.Join(n.Subnets, ",")
		if subnets == "" {
			subnets = "(none)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%d IPs\n",
			n.Name, subnets, n.RenewedAgo, n.ExpiresIn, n.Healthy, n.Announcing)
	}
	tw.Flush()

	// Subnet coverage
	fmt.Printf("\nMembers: %d, Subnets: %d unique\n", len(nodes), len(coverage))
	fmt.Println("Subnet Coverage:")
	for _, sc := range coverage {
		poolStr := ""
		if len(sc.Pools) > 0 {
			poolStr = fmt.Sprintf("  (covers: %s)", strings.Join(sc.Pools, ", "))
		}
		fmt.Printf("  %s -> %s%s\n", sc.Subnet, strings.Join(sc.Nodes, ", "), poolStr)
	}

	if len(uncovered) > 0 {
		fmt.Println("\nUncovered Pool Ranges:")
		for _, u := range uncovered {
			fmt.Printf("  %s: %s (subnet %s) - NO NODES WITH THIS SUBNET\n",
				u.ServiceGroup, u.Pool, u.Subnet)
		}
	}

	// Drain simulation output
	if drainNode != "" {
		fmt.Printf("\nSimulating removal of %s from elections...\n\n", drainNode)
		tw2 := tableWriter(os.Stdout)
		fmt.Fprintf(tw2, "SERVICE\tIP\tCURRENT\tNEW WINNER\tSUBNET\n")
		noAnnouncerCount := 0
		moveCount := 0
		for _, dr := range drainResults {
			newWinner := dr.NewWinner
			if newWinner == "" {
				newWinner = "(NONE)"
			}
			warning := ""
			if dr.Warning != "" {
				warning = "  *** " + dr.Warning + " ***"
				noAnnouncerCount++
			}
			if dr.Current == drainNode {
				moveCount++
			}
			fmt.Fprintf(tw2, "%s\t%s\t%s -> \t%s\t%s%s\n",
				dr.Service, dr.IP, dr.Current, newWinner, dr.Subnet, warning)
		}
		tw2.Flush()

		if noAnnouncerCount > 0 {
			fmt.Printf("\nWARNING: %d IP(s) will have NO announcer after drain\n", noAnnouncerCount)
		}
		fmt.Printf("%d service(s) will move\n", moveCount)
	}

	return nil
}

func isLeaseHealthy(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return false
	}
	expiry := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return now.Before(expiry)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

// extractPoolSubnets pulls subnet strings from a local spec for coverage mapping.
func extractPoolSubnets(sgName string, specRaw interface{}) []struct{ sgName, pool, subnet string } {
	spec, ok := specRaw.(map[string]interface{})
	if !ok {
		return nil
	}
	type ps = struct{ sgName, pool, subnet string }
	var result []ps

	for _, key := range []string{"v4pools", "v6pools", "v4pool", "v6pool"} {
		raw, ok := spec[key]
		if !ok {
			continue
		}

		// Handle array (v4pools/v6pools)
		if pools, ok := raw.([]interface{}); ok {
			for _, pRaw := range pools {
				p, ok := pRaw.(map[string]interface{})
				if !ok {
					continue
				}
				pool, _ := p["pool"].(string)
				subnet, _ := p["subnet"].(string)
				if subnet != "" {
					result = append(result, ps{sgName: sgName, pool: pool, subnet: subnet})
				}
			}
			continue
		}

		// Handle singular (v4pool/v6pool)
		if p, ok := raw.(map[string]interface{}); ok {
			pool, _ := p["pool"].(string)
			subnet, _ := p["subnet"].(string)
			if subnet != "" {
				result = append(result, ps{sgName: sgName, pool: pool, subnet: subnet})
			}
		}
	}

	return result
}
