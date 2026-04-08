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
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"golang.org/x/term"
)

func newDashboardCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Consolidated monitoring view (status + services + pools + gobgp rib)",
		Long: `Runs status, services, pools, and gobgp global rib in a single loop,
fetching each resource once per tick instead of independently. This reduces
API server load by ~50% compared to running the four commands separately.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runDashboard(cmd.Context(), c, format, interval)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().DurationVar(&interval, "interval", 1*time.Second, "Refresh interval (e.g. 1s, 5s, 500ms)")

	return cmd
}

func runDashboard(ctx context.Context, c *clients, format outputFormat, interval time.Duration) error {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	var lastErr string

	// Run once immediately before starting the ticker.
	if err := dashboardTick(ctx, c, format, isTTY); err != nil {
		fmt.Fprintf(os.Stderr, "fetch error: %v\n", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := dashboardTick(ctx, c, format, isTTY); err != nil {
				// Deduplicate consecutive identical errors.
				msg := err.Error()
				if msg != lastErr {
					fmt.Fprintf(os.Stderr, "fetch error: %v\n", err)
					lastErr = msg
				}
				continue
			}
			lastErr = ""
		}
	}
}

// dashboardTick performs one fetch-and-render cycle.
func dashboardTick(ctx context.Context, c *clients, format outputFormat, isTTY bool) error {
	snap, err := fetchSnapshot(ctx, c)
	if err != nil {
		return err
	}

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

	// gobgp global rib — uses pre-fetched pods from the snapshot.
	fmt.Println()
	fmt.Println("GoBGP Global RIB")
	fmt.Println("=================")
	var buf bytes.Buffer
	gobgpCmd := []string{"gobgp", "-u", "/var/run/gobgp/gobgp.sock", "global", "rib"}
	if err := execInK8GoBGPWithPods(ctx, c, snap.pods, "", gobgpCmd, &buf, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "gobgp: %v\n", err)
	}
	fmt.Print(buf.String())

	return nil
}
