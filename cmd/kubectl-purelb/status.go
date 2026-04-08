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
	"strings"
	"time"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type statusOverview struct {
	Components componentStatus `json:"components"`
	Pools      poolStatus      `json:"pools"`
	Election   electionStatus  `json:"election"`
	BGP        bgpStatus       `json:"bgp"`
	Services   svcStatus       `json:"services"`
	Overall    string          `json:"overall"`
	Warnings   []string        `json:"warnings,omitempty"`
}

type componentStatus struct {
	AllocatorReady   string `json:"allocator"`
	NodeAgentReady   string `json:"lbnodeagent"`
	K8GoBGPReady     string `json:"k8gobgp,omitempty"`
}

type poolStatus struct {
	Summary string `json:"summary"`
}

type electionStatus struct {
	Summary string `json:"summary"`
}

type bgpStatus struct {
	Summary string `json:"summary"`
}

type svcStatus struct {
	Summary string `json:"summary"`
}

func newStatusCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Single-pane cluster health overview",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runStatus(cmd.Context(), c, format)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")

	return cmd
}

func runStatus(ctx context.Context, c *clients, format outputFormat) error {
	var warnings []string

	// === Components ===
	pods, _ := c.core.CoreV1().Pods(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})

	allocatorTotal, allocatorReady := 0, 0
	nodeagentTotal, nodeagentReady := 0, 0
	gobgpTotal, gobgpReady := 0, 0

	if pods != nil {
		for _, pod := range pods.Items {
			labels := pod.Labels
			if labels["component"] == "allocator" {
				allocatorTotal++
				if pod.Status.Phase == v1.PodRunning {
					allocatorReady++
				}
			}
			if labels["component"] == "lbnodeagent" {
				nodeagentTotal++
				// Count container statuses
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.Name == "lbnodeagent" && cs.Ready {
						nodeagentReady++
					}
					if cs.Name == "k8gobgp" {
						gobgpTotal++
						if cs.Ready {
							gobgpReady++
						}
					}
				}
			}
		}
	}

	comp := componentStatus{
		AllocatorReady: fmt.Sprintf("%d/%d Running", allocatorReady, allocatorTotal),
		NodeAgentReady: fmt.Sprintf("%d/%d Running", nodeagentReady, nodeagentTotal),
	}
	if gobgpTotal > 0 {
		comp.K8GoBGPReady = fmt.Sprintf("%d/%d Running", gobgpReady, gobgpTotal)
	}
	if allocatorReady < allocatorTotal {
		warnings = append(warnings, "Allocator not fully ready")
	}
	if nodeagentReady < nodeagentTotal {
		warnings = append(warnings, "LBNodeAgent not fully ready")
	}

	// === Pools ===
	sgList, _ := c.dynamic.Resource(gvrServiceGroups).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	svcList, _ := c.core.CoreV1().Services("").List(ctx, metav1.ListOptions{ResourceVersion: "0", FieldSelector: svcFieldSelector})

	var totalV4, totalV6 uint64
	var usedV4, usedV6 int
	exhausted := 0

	if sgList != nil && svcList != nil {
		poolSvcs := map[string][]svcIP{}
		for _, svc := range svcList.Items {
			ann := svc.Annotations
			if ann == nil || ann[annotationAllocatedBy] != brandPureLB {
				continue
			}
			pool := ann[annotationAllocatedFrom]
			for _, ingress := range svc.Status.LoadBalancer.Ingress {
				ip := net.ParseIP(ingress.IP)
				if ip != nil {
					poolSvcs[pool] = append(poolSvcs[pool], svcIP{name: svc.Namespace + "/" + svc.Name, ip: ip})
				}
			}
		}

		for _, sg := range sgList.Items {
			spec, _, _ := unstructured.NestedMap(sg.Object, "spec")
			if spec == nil {
				continue
			}
			for _, key := range []string{"local", "remote"} {
				if poolSpec, ok := spec[key]; ok {
					ranges := parsePoolSpec(sg.GetName(), key, poolSpec, poolSvcs[sg.GetName()])
					for _, r := range ranges {
						if r.Family == "IPv4" {
							totalV4 += r.Size
							usedV4 += r.Used
						} else {
							totalV6 += r.Size
							usedV6 += r.Used
						}
						if r.Size > 0 && uint64(r.Used) >= r.Size {
							exhausted++
						}
					}
				}
			}
		}
	}

	poolParts := []string{}
	if totalV4 > 0 {
		poolParts = append(poolParts, fmt.Sprintf("%d/%d IPv4 (%.0f%%)", usedV4, totalV4, float64(usedV4)/float64(totalV4)*100))
	}
	if totalV6 > 0 {
		poolParts = append(poolParts, fmt.Sprintf("%d/%d IPv6 (%.0f%%)", usedV6, totalV6, float64(usedV6)/float64(totalV6)*100))
	}
	exhaustedStr := "no pools exhausted"
	if exhausted > 0 {
		exhaustedStr = fmt.Sprintf("%d pool(s) EXHAUSTED", exhausted)
		warnings = append(warnings, exhaustedStr)
	}
	poolParts = append(poolParts, exhaustedStr)

	// === Election ===
	leaseList, _ := c.core.CoordinationV1().Leases(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	now := time.Now()
	healthyCount, totalNodes := 0, 0
	subnetSet := map[string]bool{}

	if leaseList != nil {
		for _, lease := range leaseList.Items {
			if !strings.HasPrefix(lease.Name, leasePrefix) {
				continue
			}
			totalNodes++
			if isLeaseHealthy(&lease, now) {
				healthyCount++
			}
			for _, s := range parseSubnetsAnnotation(lease.GetAnnotations()[subnetsAnnotation]) {
				subnetSet[s] = true
			}
		}
	}
	if healthyCount < totalNodes {
		warnings = append(warnings, fmt.Sprintf("%d node(s) unhealthy", totalNodes-healthyCount))
	}

	// === BGP ===
	bgpnsList, _ := c.dynamic.Resource(gvrBGPNodeStatuses).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	bgpEstablished, bgpTotal := 0, 0
	importFailures := 0

	if bgpnsList != nil {
		for _, bgpns := range bgpnsList.Items {
			neighbors, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "neighbors")
			for _, nRaw := range neighbors {
				n, ok := nRaw.(map[string]interface{})
				if !ok {
					continue
				}
				bgpTotal++
				state, _ := n["state"].(string)
				if state == "Established" {
					bgpEstablished++
				}
			}
			// Check import failures
			addrs, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "netlinkImport", "importedAddresses")
			for _, aRaw := range addrs {
				a, ok := aRaw.(map[string]interface{})
				if !ok {
					continue
				}
				inRIB, _ := a["inRIB"].(bool)
				if !inRIB {
					importFailures++
				}
			}
		}
	}

	bgpSummary := "not deployed"
	if bgpTotal > 0 {
		bgpParts := []string{fmt.Sprintf("%d/%d sessions established", bgpEstablished, bgpTotal)}
		if importFailures > 0 {
			bgpParts = append(bgpParts, fmt.Sprintf("%d import failure(s)", importFailures))
			warnings = append(warnings, fmt.Sprintf("%d netlinkImport failure(s)", importFailures))
		} else {
			bgpParts = append(bgpParts, "netlinkImport OK")
		}
		bgpSummary = strings.Join(bgpParts, " | ")
		if bgpEstablished < bgpTotal {
			warnings = append(warnings, fmt.Sprintf("%d BGP session(s) down", bgpTotal-bgpEstablished))
		}
	}

	// === Services ===
	totalSvcs := 0
	totalIPs := 0
	svcProblems := 0

	if svcList != nil {
		for _, svc := range svcList.Items {
			ann := svc.Annotations
			if ann == nil || ann[annotationAllocatedBy] != brandPureLB {
				continue
			}
			totalSvcs++
			totalIPs += len(svc.Status.LoadBalancer.Ingress)
		}
		// Count pending LB services
		for _, svc := range svcList.Items {
			if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
				ann := svc.Annotations
				if ann != nil && ann[annotationServiceGroup] != "" && ann[annotationAllocatedBy] != brandPureLB {
					svcProblems++
				}
			}
		}
	}
	if svcProblems > 0 {
		warnings = append(warnings, fmt.Sprintf("%d service(s) pending", svcProblems))
	}

	// === Overall ===
	overall := "OK"
	if len(warnings) > 0 {
		overall = "WARNING"
	}

	overview := statusOverview{
		Components: comp,
		Pools:      poolStatus{Summary: strings.Join(poolParts, " | ")},
		Election:   electionStatus{Summary: fmt.Sprintf("%d/%d nodes healthy | %d subnets covered", healthyCount, totalNodes, len(subnetSet))},
		BGP:        bgpStatus{Summary: bgpSummary},
		Services:   svcStatus{Summary: fmt.Sprintf("%d services, %d IPs | %d problem(s)", totalSvcs, totalIPs, svcProblems)},
		Overall:    overall,
		Warnings:   warnings,
	}

	if format != outputTable {
		return printStructured(format, overview)
	}

	fmt.Println("PureLB Cluster Status")
	fmt.Println("=====================")
	compParts := []string{
		"Allocator " + comp.AllocatorReady,
		"LBNodeAgents " + comp.NodeAgentReady,
	}
	if comp.K8GoBGPReady != "" {
		compParts = append(compParts, "k8gobgp "+comp.K8GoBGPReady)
	}
	fmt.Printf("Components:  %s\n", strings.Join(compParts, " | "))
	fmt.Printf("Pools:       %s\n", overview.Pools.Summary)
	fmt.Printf("Election:    %s\n", overview.Election.Summary)
	fmt.Printf("BGP:         %s\n", overview.BGP.Summary)
	fmt.Printf("Services:    %s\n", overview.Services.Summary)
	fmt.Println()

	if overall == "OK" {
		fmt.Println("Overall: OK")
	} else {
		fmt.Printf("Overall: WARNING (%s)\n", strings.Join(warnings, "; "))
	}

	return nil
}

