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
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// bgpSessionRow holds one row of the sessions table.
type bgpSessionRow struct {
	Node       string `json:"node"`
	RouterID   string `json:"routerID"`
	Neighbor   string `json:"neighbor"`
	PeerASN    int64  `json:"peerASN"`
	LocalASN   int64  `json:"localASN"`
	State      string `json:"state"`
	Uptime     string `json:"uptime"`
	PrefixSent int64  `json:"prefixesSent"`
	PrefixRecv int64  `json:"prefixesReceived"`
	LastError  string `json:"lastError,omitempty"`
	Selector   string `json:"selector,omitempty"`
}

type bgpSessionsSummary struct {
	GlobalASN  int64           `json:"globalASN"`
	RouterIDMode string       `json:"routerIDMode"`
	Sessions   []bgpSessionRow `json:"sessions"`
	Problems   []string        `json:"problems,omitempty"`
}

func newBGPCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bgp",
		Short: "BGP session and data plane visibility",
		Long:  "Show BGP neighbor sessions, route advertisements, and the netlink import/export pipeline. Data comes from BGPNodeStatus CRDs written by k8gobgp.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		newBGPSessionsCmd(flags),
		newBGPDataplaneCmd(flags),
	)

	return cmd
}

func newBGPSessionsCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var node string
	var check bool

	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Show BGP neighbor session state across all nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runBGPSessions(cmd.Context(), c, format, node, check)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&node, "node", "", "Show BGP state for a single node")
	cmd.Flags().BoolVar(&check, "check", false, "Show only problems")

	return cmd
}

func runBGPSessions(ctx context.Context, c *clients, format outputFormat, filterNode string, checkOnly bool) error {
	now := time.Now()

	// Fetch BGPNodeStatuses
	bgpnsList, err := c.dynamic.Resource(gvrBGPNodeStatuses).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing BGPNodeStatuses: %w\nBGPNodeStatus CRD may not be installed — upgrade k8gobgp to 0.2.3+ for BGP visibility", err)
	}
	if len(bgpnsList.Items) == 0 {
		fmt.Println("No BGPNodeStatus resources found.")
		fmt.Println("Either k8gobgp is not deployed, nodeStatus.enabled is false, or k8gobgp version < 0.2.3")
		return nil
	}

	// Fetch BGPConfiguration for global config and nodeSelector info
	bgpConfigList, _ := c.dynamic.Resource(gvrBGPConfigurations).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})

	globalASN := int64(0)
	routerIDMode := "auto-detect"
	// neighbor address -> selector description
	neighborSelectors := map[string]string{}

	if bgpConfigList != nil && len(bgpConfigList.Items) > 0 {
		cfg := bgpConfigList.Items[0]
		globalASN, _, _ = unstructured.NestedInt64(cfg.Object, "spec", "global", "asn")
		rid, _, _ := unstructured.NestedString(cfg.Object, "spec", "global", "routerID")
		if rid != "" {
			routerIDMode = "explicit: " + rid
		}

		// Extract nodeSelector per neighbor for display
		neighbors, _, _ := unstructured.NestedSlice(cfg.Object, "spec", "neighbors")
		for _, nRaw := range neighbors {
			n, ok := nRaw.(map[string]interface{})
			if !ok {
				continue
			}
			config, _ := n["config"].(map[string]interface{})
			addr, _ := config["neighborAddress"].(string)
			if sel, ok := n["nodeSelector"]; ok && sel != nil {
				neighborSelectors[addr] = formatNodeSelector(sel)
			}
		}
	}

	var rows []bgpSessionRow
	var problems []string

	for _, bgpns := range bgpnsList.Items {
		nodeName, _, _ := unstructured.NestedString(bgpns.Object, "status", "nodeName")
		if filterNode != "" && nodeName != filterNode {
			continue
		}

		routerID, _, _ := unstructured.NestedString(bgpns.Object, "status", "routerID")

		// Check staleness
		lastUpdatedStr, _, _ := unstructured.NestedString(bgpns.Object, "status", "lastUpdated")
		if lastUpdatedStr != "" {
			lastUpdated, err := time.Parse(time.RFC3339, lastUpdatedStr)
			if err == nil {
				heartbeat, _, _ := unstructured.NestedInt64(bgpns.Object, "status", "heartbeatSeconds")
				if heartbeat == 0 {
					heartbeat = 60
				}
				staleThreshold := time.Duration(heartbeat*3) * time.Second
				if now.Sub(lastUpdated) > staleThreshold {
					problems = append(problems, fmt.Sprintf("%s: BGPNodeStatus stale (last updated %s ago, threshold %s)",
						nodeName, formatDuration(now.Sub(lastUpdated)), formatDuration(staleThreshold)))
				}
			}
		}

		neighbors, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "neighbors")
		if len(neighbors) == 0 {
			// Node has no neighbors — still show it
			rows = append(rows, bgpSessionRow{
				Node:     nodeName,
				RouterID: routerID,
				Neighbor: "(none)",
				State:    "-",
			})
			continue
		}

		for _, nRaw := range neighbors {
			n, ok := nRaw.(map[string]interface{})
			if !ok {
				continue
			}

			addr, _ := n["address"].(string)
			peerASN, _ := n["peerASN"].(int64)
			localASN, _ := n["localASN"].(int64)
			state, _ := n["state"].(string)
			prefSent, _ := n["prefixesSent"].(int64)
			prefRecv, _ := n["prefixesReceived"].(int64)
			lastError, _ := n["lastError"].(string)
			desc, _ := n["description"].(string)
			_ = desc

			uptime := "-"
			if sessionUpSince, ok := n["sessionUpSince"].(string); ok && sessionUpSince != "" {
				t, err := time.Parse(time.RFC3339, sessionUpSince)
				if err == nil {
					uptime = formatDuration(now.Sub(t))
				}
			}

			selector := neighborSelectors[addr]

			row := bgpSessionRow{
				Node:       nodeName,
				RouterID:   routerID,
				Neighbor:   addr,
				PeerASN:    peerASN,
				LocalASN:   localASN,
				State:      state,
				Uptime:     uptime,
				PrefixSent: prefSent,
				PrefixRecv: prefRecv,
				LastError:  lastError,
				Selector:   selector,
			}
			rows = append(rows, row)

			if state != "Established" {
				msg := fmt.Sprintf("%s <-> %s (AS %d): not established (state: %s)",
					nodeName, addr, peerASN, state)
				if lastError != "" {
					msg += " error: " + lastError
				}
				problems = append(problems, msg)
			}
		}
	}

	summary := bgpSessionsSummary{
		GlobalASN:    globalASN,
		RouterIDMode: routerIDMode,
		Sessions:     rows,
		Problems:     problems,
	}

	if format != outputTable {
		return printStructured(format, summary)
	}

	if checkOnly {
		if len(problems) == 0 {
			established := 0
			for _, r := range rows {
				if r.State == "Established" {
					established++
				}
			}
			fmt.Printf("All %d BGP session(s) established, no problems detected\n", established)
		} else {
			fmt.Println("Problems:")
			for _, p := range problems {
				fmt.Printf("  %s\n", p)
			}
		}
		return nil
	}

	// Full table
	fmt.Printf("BGP Global: ASN %d, Router ID: %s\n\n", globalASN, routerIDMode)

	tw := tableWriter(os.Stdout)
	fmt.Fprintf(tw, "NODE\tROUTER ID\tNEIGHBOR\tPEER ASN\tSTATE\tUPTIME\tPREFIXES\tSELECTOR\n")

	for _, r := range rows {
		prefixes := fmt.Sprintf("%d/%d", r.PrefixSent, r.PrefixRecv)
		if r.State == "-" {
			prefixes = "-"
		}
		peerASN := fmt.Sprintf("%d", r.PeerASN)
		if r.PeerASN == 0 {
			peerASN = "-"
		}

		marker := ""
		if r.State != "Established" && r.State != "-" {
			marker = "  *** DOWN ***"
		}

		selector := r.Selector
		if selector == "" {
			selector = "-"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s%s\n",
			r.Node, r.RouterID, r.Neighbor, peerASN,
			r.State, r.Uptime, prefixes, selector, marker)
	}
	tw.Flush()

	if len(problems) > 0 {
		fmt.Println("\nProblems:")
		for _, p := range problems {
			fmt.Printf("  %s\n", p)
		}
	}

	return nil
}

// formatNodeSelector renders a nodeSelector map into a compact string.
func formatNodeSelector(sel interface{}) string {
	sMap, ok := sel.(map[string]interface{})
	if !ok {
		return ""
	}
	matchLabels, _ := sMap["matchLabels"].(map[string]interface{})
	if matchLabels == nil {
		return ""
	}
	var parts []string
	for k, v := range matchLabels {
		vs, _ := v.(string)
		parts = append(parts, k+"="+vs)
	}
	return strings.Join(parts, ",")
}

func newBGPDataplaneCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var node string
	var check bool
	var vrf string
	var importOnly bool
	var exportOnly bool

	cmd := &cobra.Command{
		Use:   "dataplane",
		Short: "Show the full route data plane: kube-lb0 -> netlinkImport -> RIB -> advertisements -> netlinkExport",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runBGPDataplane(cmd.Context(), c, format, node, check, vrf, importOnly, exportOnly)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&node, "node", "", "Show data plane for a single node")
	cmd.Flags().BoolVar(&check, "check", false, "Show only problems")
	cmd.Flags().StringVar(&vrf, "vrf", "", "Filter to a specific VRF")
	cmd.Flags().BoolVar(&importOnly, "import-only", false, "Show only netlink import pipeline")
	cmd.Flags().BoolVar(&exportOnly, "export-only", false, "Show only netlink export pipeline")

	return cmd
}

func runBGPDataplane(ctx context.Context, c *clients, format outputFormat, filterNode string, checkOnly bool, filterVRF string, importOnly, exportOnly bool) error {
	return runBGPDataplaneImpl(ctx, c, format, filterNode, checkOnly, filterVRF, importOnly, exportOnly)
}
