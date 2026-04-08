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
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func newDashboardCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var interval time.Duration
	var gobgpInterval time.Duration

	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Consolidated monitoring view (status + services + pools + gobgp rib)",
		Long: `Runs status, services, pools, and gobgp global rib in a single loop,
fetching each resource once per tick instead of independently. This reduces
API server load by ~50% compared to running the four commands separately.

The gobgp global rib is refreshed at a separate, slower cadence (default 5s)
because each exec call involves SPDY negotiation across every node. Cached
output is displayed between refreshes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runDashboard(cmd.Context(), c, format, interval, gobgpInterval)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().DurationVar(&interval, "interval", 1*time.Second, "Refresh interval (e.g. 1s, 5s, 500ms)")
	cmd.Flags().DurationVar(&gobgpInterval, "gobgp-interval", 5*time.Second, "GoBGP RIB refresh interval (e.g. 5s, 10s)")

	return cmd
}

func runDashboard(ctx context.Context, c *clients, format outputFormat, interval, gobgpInterval time.Duration) error {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	var lastErr string
	var cachedGoBGP string
	var lastGoBGPRefresh time.Time

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Force gobgp refresh on the first tick.
	refreshGoBGP := true

	for {
		snap, err := fetchSnapshot(ctx, c)
		if err != nil {
			msg := err.Error()
			if msg != lastErr {
				fmt.Fprintf(os.Stderr, "fetch error: %v\n", err)
				lastErr = msg
			}
		} else {
			lastErr = ""

			// Refresh gobgp output if the interval has elapsed.
			if refreshGoBGP || time.Since(lastGoBGPRefresh) >= gobgpInterval {
				cachedGoBGP = fetchGoBGPOutput(ctx, c, snap)
				lastGoBGPRefresh = time.Now()
				refreshGoBGP = false
			}

			renderDashboard(snap, format, isTTY, cachedGoBGP)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// renderDashboard outputs one complete dashboard frame.
func renderDashboard(snap *clusterSnapshot, format outputFormat, isTTY bool, gobgpOutput string) {
	if isTTY {
		fmt.Print("\033[2J\033[H")
	} else {
		fmt.Println("---")
	}

	if err := renderStatus(snap, format); err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
	}

	fmt.Println()
	if err := renderServices(snap, format, "", "", "", false); err != nil {
		fmt.Fprintf(os.Stderr, "services: %v\n", err)
	}

	fmt.Println()
	if err := renderPools(snap, format, "", false); err != nil {
		fmt.Fprintf(os.Stderr, "pools: %v\n", err)
	}

	fmt.Println()
	fmt.Println("GoBGP Global RIB")
	fmt.Println("=================")
	if gobgpOutput != "" {
		fmt.Print(gobgpOutput)
	} else {
		fmt.Println("(no k8gobgp sidecars found)")
	}
}

// fetchGoBGPOutput runs gobgp global rib across all nodes using the
// pre-fetched pod list from the snapshot. Returns the captured output,
// or an empty string on error.
func fetchGoBGPOutput(ctx context.Context, c *clients, snap *clusterSnapshot) string {
	var buf bytes.Buffer
	gobgpCmd := []string{"gobgp", "-u", "/var/run/gobgp/gobgp.sock", "global", "rib"}
	if err := execInK8GoBGPWithPods(ctx, c, snap.pods, "", gobgpCmd, &buf, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "gobgp: %v\n", err)
		return ""
	}
	return buf.String()
}
