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
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// svcRow holds one row of the services table (one row per IP per service).
type svcRow struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Pool       string `json:"pool"`
	PoolType   string `json:"poolType"`
	Announcing string `json:"announcing"` // "node/iface" or ""
	SharingKey string `json:"sharingKey"`
	Status     string `json:"status"`
}

// sharedIPGroup describes services sharing an IP via a sharing key.
type sharedIPGroup struct {
	IP         string   `json:"ip"`
	SharingKey string   `json:"sharingKey"`
	Services   []string `json:"services"` // "namespace/name (ports)"
}

type servicesSummary struct {
	Services    []svcRow        `json:"services"`
	SharedIPs   []sharedIPGroup `json:"sharedIPs,omitempty"`
	TotalSvc    int             `json:"totalServices"`
	TotalIPs    int             `json:"totalIPs"`
	Problems    int             `json:"problems"`
}

func newServicesCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var pool string
	var node string
	var ip string
	var problems bool

	cmd := &cobra.Command{
		Use:   "services",
		Short: "Show PureLB-managed services with IPs, announcers, and status",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runServices(cmd.Context(), c, format, pool, node, ip, problems)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&pool, "pool", "", "Filter to services from a specific ServiceGroup")
	cmd.Flags().StringVar(&node, "node", "", "Filter to services announced by a specific node")
	cmd.Flags().StringVar(&ip, "ip", "", "Find the service owning a specific IP address")
	cmd.Flags().BoolVar(&problems, "problems", false, "Show only services with issues")

	return cmd
}

func runServices(ctx context.Context, c *clients, format outputFormat, filterPool, filterNode, filterIP string, problemsOnly bool) error {
	// Fetch services
	svcList, err := c.core.CoreV1().Services("").List(ctx, metav1.ListOptions{ResourceVersion: "0", FieldSelector: svcFieldSelector})
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}

	// Fetch leases for announcer health check
	leaseList, err := c.core.CoordinationV1().Leases(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing leases: %w", err)
	}
	healthyNodes := buildHealthyNodeSet(leaseList.Items)

	// Resolve dummy interface name once before the loop. Previously this
	// was called per-service inside the loop, making a fresh API call each time.
	dummyIface := dummyInterfaceName(ctx, c)

	var rows []svcRow
	// Track sharing: sharingKey -> IP -> []services with ports
	type sharingEntry struct {
		svcName string
		ports   string
	}
	sharingMap := map[string]map[string][]sharingEntry{} // key -> ip -> entries

	for _, svc := range svcList.Items {
		ann := svc.Annotations
		if ann == nil {
			continue
		}

		// Only PureLB-managed services
		if ann[annotationAllocatedBy] != brandPureLB {
			// Check for pending LB services (type=LoadBalancer but not yet allocated)
			if svc.Spec.Type == v1.ServiceTypeLoadBalancer && ann[annotationServiceGroup] != "" {
				row := svcRow{
					Namespace: svc.Namespace,
					Name:      svc.Name,
					IP:        "<pending>",
					Pool:      ann[annotationServiceGroup],
					Status:    "PENDING",
				}
				if filterPool == "" || row.Pool == filterPool {
					rows = append(rows, row)
				}
			}
			continue
		}

		poolName := ann[annotationAllocatedFrom]
		poolType := ann[annotationPoolType]
		sharingKey := ann[annotationSharing]

		// Parse announcing annotations for both families (local pools only;
		// remote pools derive the interface from the LBNodeAgent CR).
		announcers := map[string]announcement{} // ip -> announcement
		for _, suffix := range []string{"-IPv4", "-IPv6"} {
			for _, a := range parseAnnouncingAnnotation(ann[annotationAnnouncing+suffix]) {
				announcers[a.IP] = a
			}
		}

		// Build port string for sharing display
		portStrs := []string{}
		for _, p := range svc.Spec.Ports {
			portStrs = append(portStrs, fmt.Sprintf("%s/%d", p.Protocol, p.Port))
		}
		portsDisplay := strings.Join(portStrs, ",")

		ingresses := svc.Status.LoadBalancer.Ingress
		if len(ingresses) == 0 {
			// Allocated but no ingress IPs yet
			row := svcRow{
				Namespace:  svc.Namespace,
				Name:       svc.Name,
				IP:         "<pending>",
				Pool:       poolName,
				PoolType:   poolType,
				SharingKey: sharingKey,
				Status:     "PENDING",
			}
			rows = append(rows, row)
			continue
		}

		for _, ingress := range ingresses {
			ipStr := ingress.IP
			if ipStr == "" {
				continue
			}

			// Determine announcer and status
			announcing := ""
			status := "OK"

			if a, ok := announcers[ipStr]; ok {
				announcing = a.Node + "/" + a.Interface
				if !healthyNodes[a.Node] {
					status = "ANNOUNCER UNHEALTHY"
				}
			} else if poolType == poolTypeRemote {
				// Remote pools: all nodes announce on the dummy interface.
				// Derive the name from the LBNodeAgent CR instead of an
				// annotation to avoid a write storm from multiple agents.
				announcing = dummyIface
			} else {
				status = "NO ANNOUNCER"
			}

			row := svcRow{
				Namespace:  svc.Namespace,
				Name:       svc.Name,
				IP:         ipStr,
				Pool:       poolName,
				PoolType:   poolType,
				Announcing: announcing,
				SharingKey: sharingKey,
				Status:     status,
			}

			// Apply filters
			if filterPool != "" && poolName != filterPool {
				continue
			}
			if filterNode != "" && !strings.HasPrefix(announcing, filterNode+"/") {
				continue
			}
			if filterIP != "" && ipStr != filterIP {
				continue
			}
			if problemsOnly && status == "OK" {
				continue
			}

			rows = append(rows, row)

			// Track sharing groups
			if sharingKey != "" {
				if sharingMap[sharingKey] == nil {
					sharingMap[sharingKey] = map[string][]sharingEntry{}
				}
				nsName := svc.Namespace + "/" + svc.Name
				sharingMap[sharingKey][ipStr] = append(sharingMap[sharingKey][ipStr], sharingEntry{
					svcName: nsName,
					ports:   portsDisplay,
				})
			}
		}
	}

	// Sort rows by namespace, name, IP
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].IP < rows[j].IP
	})

	// Build shared IP groups (only groups with >1 service on same IP)
	var sharedGroups []sharedIPGroup
	for key, ipMap := range sharingMap {
		for ipStr, entries := range ipMap {
			if len(entries) > 1 {
				var svcs []string
				for _, e := range entries {
					svcs = append(svcs, fmt.Sprintf("%s (%s)", e.svcName, e.ports))
				}
				sharedGroups = append(sharedGroups, sharedIPGroup{
					IP:         ipStr,
					SharingKey: key,
					Services:   svcs,
				})
			}
		}
	}

	// Count problems
	problemCount := 0
	for _, r := range rows {
		if r.Status != "OK" {
			problemCount++
		}
	}

	if format != outputTable {
		return printStructured(format, servicesSummary{
			Services:  rows,
			SharedIPs: sharedGroups,
			TotalSvc:  countUniqueSvcs(rows),
			TotalIPs:  len(rows),
			Problems:  problemCount,
		})
	}

	// Table output
	tw := tableWriter(os.Stdout)
	fmt.Fprintf(tw, "NAMESPACE\tSERVICE\tIPS\tPOOL\tTYPE\tANNOUNCING\tSHARING\tSTATUS\n")

	for _, r := range rows {
		announcing := r.Announcing
		if announcing == "" {
			announcing = "-"
		}
		sharing := r.SharingKey
		if sharing == "" {
			sharing = "-"
		}
		poolType := r.PoolType
		if poolType == "" {
			poolType = "-"
		}

		statusMarker := r.Status
		if r.Status != "OK" && r.Status != "PENDING" {
			statusMarker = r.Status + "  ***"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Namespace, r.Name, r.IP, r.Pool, poolType, announcing, sharing, statusMarker)
	}
	tw.Flush()

	// Shared IP groups
	if len(sharedGroups) > 0 {
		fmt.Println()
		fmt.Println("Shared IPs:")
		for _, g := range sharedGroups {
			fmt.Printf("  %s (sharing key: %s): %s\n", g.IP, g.SharingKey, strings.Join(g.Services, ", "))
		}
	}

	// Summary
	fmt.Printf("\n%d service(s), %d IP(s), %d problem(s)\n",
		countUniqueSvcs(rows), len(rows), problemCount)

	return nil
}

// buildHealthyNodeSet returns a set of node names with valid (non-expired) PureLB leases.
func buildHealthyNodeSet(leases []coordinationv1.Lease) map[string]bool {
	healthy := map[string]bool{}
	now := time.Now()

	for _, lease := range leases {
		name := lease.Name
		if !strings.HasPrefix(name, leasePrefix) {
			continue
		}
		nodeName := name[len(leasePrefix):]

		if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
			continue
		}

		renewTime := lease.Spec.RenewTime.Time
		duration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
		expiry := renewTime.Add(duration)

		if now.Before(expiry) {
			healthy[nodeName] = true
		}
	}

	return healthy
}

func countUniqueSvcs(rows []svcRow) int {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Namespace+"/"+r.Name] = true
	}
	return len(seen)
}
