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

// svcIP pairs a service name with one of its allocated IPs.
type svcIP struct {
	name string
	ip   net.IP
}

// poolRangeInfo holds parsed data for a single address range within a ServiceGroup.
type poolRangeInfo struct {
	ServiceGroup string
	PoolType     string // "local", "remote", "netbox"
	Pool         string // raw pool string
	Subnet       string
	Aggregation  string
	Family       string // "IPv4" or "IPv6"
	Range        ipRange
	Size         uint64
	Used         int
	Services     []string // namespace/name of services using IPs from this range
}

// poolsSummary holds the aggregate data for JSON/YAML output.
type poolsSummary struct {
	ServiceGroups []sgSummary `json:"serviceGroups"`
	TotalV4       uint64      `json:"totalIPv4"`
	UsedV4        int         `json:"usedIPv4"`
	TotalV6       uint64      `json:"totalIPv6"`
	UsedV6        int         `json:"usedIPv6"`
	NetboxPools   int         `json:"netboxPools"`
	NetboxUsed    int         `json:"netboxAllocated"`
}

type sgSummary struct {
	Name   string      `json:"name"`
	Type   string      `json:"type"`
	Ranges []rangeStat `json:"ranges,omitempty"`
	// Netbox fields
	NetboxURL    string `json:"netboxURL,omitempty"`
	NetboxTenant string `json:"netboxTenant,omitempty"`
	Allocated    int    `json:"allocated,omitempty"`
}

type rangeStat struct {
	Pool     string   `json:"pool"`
	Subnet   string   `json:"subnet"`
	Family   string   `json:"family"`
	Size     uint64   `json:"size"`
	Used     int      `json:"used"`
	Avail    uint64   `json:"available"`
	Percent  float64  `json:"utilPercent"`
	Services []string `json:"services,omitempty"`
}

func newPoolsCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string
	var serviceGroup string
	var showServices bool

	cmd := &cobra.Command{
		Use:   "pools",
		Short: "Show ServiceGroup pool capacity and utilization",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}

			c, err := newClients(flags)
			if err != nil {
				return err
			}

			return runPools(cmd.Context(), c, format, serviceGroup, showServices)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&serviceGroup, "service-group", "", "Filter to a single ServiceGroup")
	cmd.Flags().BoolVar(&showServices, "show-services", false, "List services allocated from each pool")

	return cmd
}

func runPools(ctx context.Context, c *clients, format outputFormat, filterSG string, showServices bool) error {
	// Fetch all ServiceGroups
	sgList, err := c.dynamic.Resource(gvrServiceGroups).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing ServiceGroups: %w", err)
	}

	// Fetch all services across all namespaces that PureLB manages
	svcList, err := c.core.CoreV1().Services("").List(ctx, metav1.ListOptions{ResourceVersion: "0", FieldSelector: svcFieldSelector})
	if err != nil {
		return fmt.Errorf("listing Services: %w", err)
	}

	// Build map: pool name -> list of (svcName, ip)
	poolServices := map[string][]svcIP{}
	for _, svc := range svcList.Items {
		ann := svc.Annotations
		if ann == nil || ann[annotationAllocatedBy] != brandPureLB {
			continue
		}
		poolName := ann[annotationAllocatedFrom]
		if poolName == "" {
			continue
		}
		nsName := svc.Namespace + "/" + svc.Name
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			ip := net.ParseIP(ingress.IP)
			if ip != nil {
				poolServices[poolName] = append(poolServices[poolName], svcIP{name: nsName, ip: ip})
			}
		}
	}

	// Parse each ServiceGroup into poolRangeInfo entries
	var ranges []poolRangeInfo
	var netboxPools []sgSummary

	for _, sg := range sgList.Items {
		sgName := sg.GetName()
		if filterSG != "" && sgName != filterSG {
			continue
		}

		spec, _, _ := unstructured.NestedMap(sg.Object, "spec")
		if spec == nil {
			continue
		}

		if _, ok := spec["local"]; ok {
			ranges = append(ranges, parsePoolSpec(sgName, "local", spec["local"], poolServices[sgName])...)
		}
		if _, ok := spec["remote"]; ok {
			ranges = append(ranges, parsePoolSpec(sgName, "remote", spec["remote"], poolServices[sgName])...)
		}
		if nb, ok := spec["netbox"]; ok {
			nbMap, _ := nb.(map[string]interface{})
			url, _ := nbMap["url"].(string)
			tenant, _ := nbMap["tenant"].(string)
			allocated := len(poolServices[sgName])
			netboxPools = append(netboxPools, sgSummary{
				Name:         sgName,
				Type:         "netbox",
				NetboxURL:    url,
				NetboxTenant: tenant,
				Allocated:    allocated,
			})
		}
	}

	// Structured output
	if format != outputTable {
		summary := buildPoolsSummary(ranges, netboxPools)
		return printStructured(format, summary)
	}

	// Table output
	tw := tableWriter(os.Stdout)
	fmt.Fprintf(tw, "SERVICEGROUP\tTYPE\tPOOL RANGE\tSUBNET\tFAMILY\tSIZE\tUSED\tAVAIL\tUTIL%%\n")

	var totalV4, totalV6 uint64
	var usedV4, usedV6 int

	for _, r := range ranges {
		var avail uint64
		if uint64(r.Used) <= r.Size {
			avail = r.Size - uint64(r.Used)
		}
		pct := float64(0)
		if r.Size > 0 {
			pct = float64(r.Used) / float64(r.Size) * 100
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%.0f%%\n",
			r.ServiceGroup, r.PoolType, r.Pool, r.Subnet, r.Family,
			r.Size, r.Used, avail, pct)

		if showServices && len(r.Services) > 0 {
			for _, svc := range r.Services {
				fmt.Fprintf(tw, "\t\t  %s\t\t\t\t\t\t\n", svc)
			}
		}

		if r.Family == "IPv4" {
			totalV4 += r.Size
			usedV4 += r.Used
		} else {
			totalV6 += r.Size
			usedV6 += r.Used
		}
	}

	for _, nb := range netboxPools {
		fmt.Fprintf(tw, "%s\tnetbox\t(managed by netbox)\t%s/%s\t-\t-\t%d\t-\t-\n",
			nb.Name, nb.NetboxURL, nb.NetboxTenant, nb.Allocated)
	}

	tw.Flush()

	// Summary line
	fmt.Println()
	parts := []string{}
	if totalV4 > 0 {
		pct := float64(usedV4) / float64(totalV4) * 100
		parts = append(parts, fmt.Sprintf("%d/%d IPv4 used (%.0f%%)", usedV4, totalV4, pct))
	}
	if totalV6 > 0 {
		pct := float64(usedV6) / float64(totalV6) * 100
		parts = append(parts, fmt.Sprintf("%d/%d IPv6 used (%.0f%%)", usedV6, totalV6, pct))
	}
	if len(netboxPools) > 0 {
		totalNB := 0
		for _, nb := range netboxPools {
			totalNB += nb.Allocated
		}
		parts = append(parts, fmt.Sprintf("%d Netbox pool(s): %d allocated (capacity managed externally)", len(netboxPools), totalNB))
	}
	fmt.Printf("Totals: %d ServiceGroup(s), %d range(s), %s\n",
		len(sgList.Items), len(ranges), strings.Join(parts, " | "))

	return nil
}

// parsePoolSpec extracts pool ranges from a local or remote spec map.
func parsePoolSpec(sgName, poolType string, specRaw interface{}, svcs []svcIP) []poolRangeInfo {
	spec, ok := specRaw.(map[string]interface{})
	if !ok {
		return nil
	}

	var ranges []poolRangeInfo

	for _, key := range []string{"v4pools", "v6pools"} {
		poolsRaw, ok := spec[key]
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
			aggregation, _ := p["aggregation"].(string)

			ipr, err := newIPRange(poolStr)
			if err != nil {
				continue
			}

			// Count services with IPs in this range
			var used int
			var svcNames []string
			for _, s := range svcs {
				if ipr.contains(s.ip) {
					used++
					svcNames = append(svcNames, s.name)
				}
			}

			ranges = append(ranges, poolRangeInfo{
				ServiceGroup: sgName,
				PoolType:     poolType,
				Pool:         poolStr,
				Subnet:       subnet,
				Aggregation:  aggregation,
				Family:       family,
				Range:        ipr,
				Size:         ipr.size(),
				Used:         used,
				Services:     svcNames,
			})
		}
	}

	// Also check singular v4pool/v6pool fields
	for _, key := range []string{"v4pool", "v6pool"} {
		pRaw, ok := spec[key]
		if !ok {
			continue
		}
		p, ok := pRaw.(map[string]interface{})
		if !ok {
			continue
		}

		family := "IPv4"
		if key == "v6pool" {
			family = "IPv6"
		}

		poolStr, _ := p["pool"].(string)
		subnet, _ := p["subnet"].(string)
		aggregation, _ := p["aggregation"].(string)

		ipr, err := newIPRange(poolStr)
		if err != nil {
			continue
		}

		var used int
		var svcNames []string
		for _, s := range svcs {
			if ipr.contains(s.ip) {
				used++
				svcNames = append(svcNames, s.name)
			}
		}

		ranges = append(ranges, poolRangeInfo{
			ServiceGroup: sgName,
			PoolType:     poolType,
			Pool:         poolStr,
			Subnet:       subnet,
			Aggregation:  aggregation,
			Family:       family,
			Range:        ipr,
			Size:         ipr.size(),
			Used:         used,
			Services:     svcNames,
		})
	}

	return ranges
}

func buildPoolsSummary(ranges []poolRangeInfo, netboxPools []sgSummary) poolsSummary {
	// Group ranges by ServiceGroup
	sgMap := map[string]*sgSummary{}
	var sgOrder []string

	for _, r := range ranges {
		sg, ok := sgMap[r.ServiceGroup]
		if !ok {
			sg = &sgSummary{Name: r.ServiceGroup, Type: r.PoolType}
			sgMap[r.ServiceGroup] = sg
			sgOrder = append(sgOrder, r.ServiceGroup)
		}

		var avail uint64
		if uint64(r.Used) <= r.Size {
			avail = r.Size - uint64(r.Used)
		}
		pct := float64(0)
		if r.Size > 0 {
			pct = float64(r.Used) / float64(r.Size) * 100
		}

		sg.Ranges = append(sg.Ranges, rangeStat{
			Pool:     r.Pool,
			Subnet:   r.Subnet,
			Family:   r.Family,
			Size:     r.Size,
			Used:     r.Used,
			Avail:    avail,
			Percent:  pct,
			Services: r.Services,
		})
	}

	summary := poolsSummary{
		NetboxPools: len(netboxPools),
	}

	for _, name := range sgOrder {
		summary.ServiceGroups = append(summary.ServiceGroups, *sgMap[name])
	}
	summary.ServiceGroups = append(summary.ServiceGroups, netboxPools...)

	for _, r := range ranges {
		if r.Family == "IPv4" {
			summary.TotalV4 += r.Size
			summary.UsedV4 += r.Used
		} else {
			summary.TotalV6 += r.Size
			summary.UsedV6 += r.Used
		}
	}
	for _, nb := range netboxPools {
		summary.NetboxUsed += nb.Allocated
	}

	return summary
}
