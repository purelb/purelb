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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// isHostRoute returns true for /32 (IPv4) or /128 (IPv6) prefixes.
func isHostRoute(prefix string) bool {
	return strings.HasSuffix(prefix, "/32") || strings.HasSuffix(prefix, "/128")
}

type importEntry struct {
	Node      string `json:"node"`
	Interface string `json:"interface"`
	Address   string `json:"address"`
	InRIB     bool   `json:"inRIB"`
}

type importInterfaceEntry struct {
	Node      string `json:"node"`
	Interface string `json:"interface"`
	Exists    bool   `json:"exists"`
	OperState string `json:"operState"`
}

type ribAdvertEntry struct {
	Node         string   `json:"node"`
	Prefix       string   `json:"prefix"`
	NextHop      string   `json:"nextHop"`
	AdvertisedTo []string `json:"advertisedTo,omitempty"`
	Service      string   `json:"service,omitempty"`
}

type exportEntry struct {
	Node      string `json:"node"`
	Prefix    string `json:"prefix"`
	FromPeer  string `json:"fromPeer,omitempty"`
	Table     string `json:"table"`
	Metric    int64  `json:"metric"`
	Installed bool   `json:"installed"`
	Reason    string `json:"reason,omitempty"`
}

type exportRuleEntry struct {
	Name    string `json:"name"`
	Metric  int64  `json:"metric"`
	TableID int64  `json:"tableID"`
}

type vrfEntry struct {
	Name           string `json:"name"`
	RD             string `json:"rd"`
	ImportedRoutes int64  `json:"importedRoutes"`
	ExportedRoutes int64  `json:"exportedRoutes"`
}

type dataplaneSummary struct {
	ImportEnabled    bool                   `json:"importEnabled"`
	ImportInterfaces []importInterfaceEntry `json:"importInterfaces,omitempty"`
	ImportAddresses  []importEntry          `json:"importAddresses,omitempty"`
	RIBRoutes        []ribAdvertEntry       `json:"ribRoutes,omitempty"`
	ExportEnabled    bool                   `json:"exportEnabled"`
	ExportRules      []exportRuleEntry      `json:"exportRules,omitempty"`
	ExportRoutes     []exportEntry          `json:"exportRoutes,omitempty"`
	VRFs             []vrfEntry             `json:"vrfs,omitempty"`
	CrossReference   []crossRefEntry        `json:"crossReference,omitempty"`
	Problems         []string               `json:"problems,omitempty"`
}

type crossRefEntry struct {
	IP      string `json:"ip"`
	Service string `json:"service"`
	Status  string `json:"status"` // "OK", "NOT ON kube-lb0", "NOT IN RIB", etc.
	Node    string `json:"node,omitempty"`
}

func runBGPDataplaneImpl(ctx context.Context, c *clients, format outputFormat, filterNode string, checkOnly bool, filterVRF string, importOnly, exportOnly bool) error {
	// Fetch BGPNodeStatuses
	bgpnsList, err := c.dynamic.Resource(gvrBGPNodeStatuses).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing BGPNodeStatuses: %w", err)
	}
	if len(bgpnsList.Items) == 0 {
		fmt.Println("No BGPNodeStatus resources found.")
		return nil
	}

	// Fetch BGPConfiguration for import/export config display
	bgpConfigList, _ := c.dynamic.Resource(gvrBGPConfigurations).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	configImportInterfaces := []string{}
	if bgpConfigList != nil && len(bgpConfigList.Items) > 0 {
		ifaces, _, _ := unstructured.NestedStringSlice(bgpConfigList.Items[0].Object, "spec", "netlinkImport", "interfaceList")
		configImportInterfaces = ifaces
	}

	dummyInterface := dummyInterfaceName(ctx, c)

	// Build service IP -> name map for cross-reference
	svcList, _ := c.core.CoreV1().Services("").List(ctx, metav1.ListOptions{ResourceVersion: "0", FieldSelector: svcFieldSelector})
	svcByIP := map[string]string{}  // ip -> "namespace/name"
	etpLocalIPs := map[string]bool{} // IPs from ETP Local services
	var remoteSvcIPs []string
	if svcList != nil {
		for _, svc := range svcList.Items {
			ann := svc.Annotations
			if ann == nil || ann[annotationAllocatedBy] != brandPureLB {
				continue
			}
			nsName := svc.Namespace + "/" + svc.Name
			for _, ingress := range svc.Status.LoadBalancer.Ingress {
				if ingress.IP != "" {
					svcByIP[ingress.IP] = nsName
					if ann[annotationPoolType] == poolTypeRemote {
						remoteSvcIPs = append(remoteSvcIPs, ingress.IP)
						if string(svc.Spec.ExternalTrafficPolicy) == "Local" {
							etpLocalIPs[ingress.IP] = true
						}
					}
				}
			}
		}
	}

	dp := dataplaneSummary{}
	var problems []string

	for _, bgpns := range bgpnsList.Items {
		nodeName, _, _ := unstructured.NestedString(bgpns.Object, "status", "nodeName")
		if filterNode != "" && nodeName != filterNode {
			continue
		}

		// === Netlink Import ===
		if !exportOnly {
			importEnabled, _, _ := unstructured.NestedBool(bgpns.Object, "status", "netlinkImport", "enabled")
			dp.ImportEnabled = dp.ImportEnabled || importEnabled

			// Interfaces
			ifaces, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "netlinkImport", "interfaces")
			for _, ifRaw := range ifaces {
				ifMap, ok := ifRaw.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := ifMap["name"].(string)
				exists, _ := ifMap["exists"].(bool)
				operState, _ := ifMap["operState"].(string)

				dp.ImportInterfaces = append(dp.ImportInterfaces, importInterfaceEntry{
					Node: nodeName, Interface: name, Exists: exists, OperState: operState,
				})
				if !exists {
					problems = append(problems, fmt.Sprintf("%s: interface %s does not exist", nodeName, name))
				}
			}

			// Imported addresses
			addrs, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "netlinkImport", "importedAddresses")
			for _, aRaw := range addrs {
				aMap, ok := aRaw.(map[string]interface{})
				if !ok {
					continue
				}
				addr, _ := aMap["address"].(string)
				iface, _ := aMap["interface"].(string)
				inRIB, _ := aMap["inRIB"].(bool)

				dp.ImportAddresses = append(dp.ImportAddresses, importEntry{
					Node: nodeName, Interface: iface, Address: addr, InRIB: inRIB,
				})
				if !inRIB && isHostRoute(addr) {
					// Only flag host routes as import failures. Aggregate addresses
					// (e.g., 10.255.1.100/24) are covered by the subnet route
					// (10.255.1.0/24) which IS in the RIB — not an error.
					problems = append(problems, fmt.Sprintf("%s: %s on %s but not in BGP RIB (import failure)", nodeName, addr, iface))
				}
			}
		}

		// === RIB Routes ===
		if !importOnly && !exportOnly {
			localRoutes, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "rib", "localRoutes")
			for _, rRaw := range localRoutes {
				rMap, ok := rRaw.(map[string]interface{})
				if !ok {
					continue
				}
				prefix, _ := rMap["prefix"].(string)
				nextHop, _ := rMap["nextHop"].(string)
				advTo, _, _ := unstructured.NestedStringSlice(rMap, "advertisedTo")

				// Match to service: exact IP match first, then check if any
				// service VIP falls within this aggregate route's subnet.
				svcName := ""
				ipOnly := strings.Split(prefix, "/")[0]
				if name, ok := svcByIP[ipOnly]; ok {
					svcName = name
				} else if !isHostRoute(prefix) {
					_, ipnet, err := net.ParseCIDR(prefix)
					if err == nil {
						for vip, name := range svcByIP {
							if ipnet.Contains(net.ParseIP(vip)) {
								svcName = name
								break
							}
						}
					}
				}

				dp.RIBRoutes = append(dp.RIBRoutes, ribAdvertEntry{
					Node: nodeName, Prefix: prefix, NextHop: nextHop,
					AdvertisedTo: advTo, Service: svcName,
				})
			}
		}

		// === Netlink Export ===
		if !importOnly {
			exportEnabled, _, _ := unstructured.NestedBool(bgpns.Object, "status", "netlinkExport", "enabled")
			dp.ExportEnabled = dp.ExportEnabled || exportEnabled

			if exportEnabled {
				// Rules
				rules, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "netlinkExport", "rules")
				for _, rRaw := range rules {
					rMap, ok := rRaw.(map[string]interface{})
					if !ok {
						continue
					}
					name, _ := rMap["name"].(string)
					metric, _ := rMap["metric"].(int64)
					tableID, _ := rMap["tableID"].(int64)
					dp.ExportRules = append(dp.ExportRules, exportRuleEntry{
						Name: name, Metric: metric, TableID: tableID,
					})
				}

				// Exported routes
				exportedRoutes, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "netlinkExport", "exportedRoutes")
				for _, eRaw := range exportedRoutes {
					eMap, ok := eRaw.(map[string]interface{})
					if !ok {
						continue
					}
					prefix, _ := eMap["prefix"].(string)
					table, _ := eMap["table"].(string)
					metric, _ := eMap["metric"].(int64)
					installed, _ := eMap["installed"].(bool)
					reason, _ := eMap["reason"].(string)

					dp.ExportRoutes = append(dp.ExportRoutes, exportEntry{
						Node: nodeName, Prefix: prefix, Table: table,
						Metric: metric, Installed: installed, Reason: reason,
					})
					if !installed {
						problems = append(problems, fmt.Sprintf("%s: %s received but not installed in kernel (%s)", nodeName, prefix, reason))
					}
				}
			}
		}

		// === VRFs ===
		vrfs, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "vrfs")
		for _, vRaw := range vrfs {
			vMap, ok := vRaw.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := vMap["name"].(string)
			if filterVRF != "" && name != filterVRF {
				continue
			}
			rd, _ := vMap["rd"].(string)
			imported, _ := vMap["importedRouteCount"].(int64)
			exported, _ := vMap["exportedRouteCount"].(int64)
			dp.VRFs = append(dp.VRFs, vrfEntry{
				Name: name, RD: rd, ImportedRoutes: imported, ExportedRoutes: exported,
			})
		}
	}

	// === Cross-reference remote pool services ===
	if !importOnly && !exportOnly {
		// Build set of addresses on kube-lb0 across all nodes
		// Key is the IP portion (without mask) so we match regardless of aggregation
		kubeLB0ByIP := map[string]string{}   // bare IP -> node
		kubeLB0Prefix := map[string]string{} // bare IP -> full prefix (e.g. "10.201.0.0/24")
		for _, ia := range dp.ImportAddresses {
			bareIP := strings.Split(ia.Address, "/")[0]
			kubeLB0ByIP[bareIP] = ia.Node
			kubeLB0Prefix[bareIP] = ia.Address
		}

		// Build RIB lookup: bare IP -> first matching RIB route
		ribByIP := map[string]*ribAdvertEntry{}
		for i, r := range dp.RIBRoutes {
			bareIP := strings.Split(r.Prefix, "/")[0]
			if _, exists := ribByIP[bareIP]; !exists {
				ribByIP[bareIP] = &dp.RIBRoutes[i]
			}
		}

		for _, ipStr := range remoteSvcIPs {
			svcName := svcByIP[ipStr]
			entry := crossRefEntry{IP: ipStr, Service: svcName}

			if node, ok := kubeLB0ByIP[ipStr]; ok {
				// On kube-lb0 — check if advertised (exact match or aggregate)
				advertised := false
				if r, ok := ribByIP[ipStr]; ok && len(r.AdvertisedTo) > 0 {
					advertised = true
					entry.Node = r.Node
				} else {
					// Check if a covering aggregate route is advertised
					ip := net.ParseIP(ipStr)
					for _, r := range dp.RIBRoutes {
						if r.Node != node || isHostRoute(r.Prefix) || len(r.AdvertisedTo) == 0 {
							continue
						}
						_, ipnet, err := net.ParseCIDR(r.Prefix)
						if err == nil && ipnet.Contains(ip) {
							advertised = true
							entry.Node = node
							break
						}
					}
				}
				if advertised {
					entry.Status = "OK"
					entry.Node = node
				} else {
					entry.Status = "IN RIB BUT NOT ADVERTISED"
					entry.Node = node
					problems = append(problems, fmt.Sprintf("%s (%s): in RIB on %s but not advertised", ipStr, svcName, node))
				}
			} else if etpLocalIPs[ipStr] {
				// ETP Local: VIP is only on endpoint nodes — not a problem
				entry.Status = "ETP Local (endpoint-only)"
			} else {
				entry.Status = "NOT ON kube-lb0"
				problems = append(problems, fmt.Sprintf("%s (%s): not on any node's %s", ipStr, svcName, dummyInterface))
			}

			dp.CrossReference = append(dp.CrossReference, entry)
		}
	}

	dp.Problems = problems

	if format != outputTable {
		return printStructured(format, dp)
	}

	if checkOnly {
		if len(problems) == 0 {
			fmt.Println("No data plane problems detected")
		} else {
			fmt.Println("Problems:")
			for _, p := range problems {
				fmt.Printf("  %s\n", p)
			}
		}
		return nil
	}

	// === Table output ===

	if !exportOnly {
		fmt.Println("=== Netlink Import ===")
		importConfig := "disabled"
		if dp.ImportEnabled {
			importConfig = fmt.Sprintf("enabled, interfaces=%v", configImportInterfaces)
		}
		fmt.Printf("Config: %s\n", importConfig)
		fmt.Printf("LBNodeAgent dummy interface: %s\n", dummyInterface)

		if len(dp.ImportInterfaces) > 0 || len(dp.ImportAddresses) > 0 {
			fmt.Println()
			tw := tableWriter(os.Stdout)
			fmt.Fprintf(tw, "NODE\tINTERFACE\tADDRESS\tIN RIB?\n")
			// Show interface status rows for interfaces with no addresses
			ifaceHasAddr := map[string]bool{}
			for _, ia := range dp.ImportAddresses {
				key := ia.Node + "/" + ia.Interface
				ifaceHasAddr[key] = true
				ribStr := "Yes"
				if !ia.InRIB {
					if isHostRoute(ia.Address) {
						ribStr = "NO  *** IMPORT FAILURE ***"
					} else {
						ribStr = "Yes (aggregate)"
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ia.Node, ia.Interface, ia.Address, ribStr)
			}
			for _, iface := range dp.ImportInterfaces {
				key := iface.Node + "/" + iface.Interface
				if !ifaceHasAddr[key] {
					existStr := "Up"
					if !iface.Exists {
						existStr = "NOT FOUND"
					} else if iface.OperState != "up" {
						existStr = iface.OperState
					}
					fmt.Fprintf(tw, "%s\t%s\t(no addresses)\t%s\n", iface.Node, iface.Interface, existStr)
				}
			}
			tw.Flush()
		}
	}

	if !importOnly && !exportOnly && len(dp.RIBRoutes) > 0 {
		fmt.Println()
		fmt.Println("=== BGP Advertisements ===")
		tw := tableWriter(os.Stdout)
		fmt.Fprintf(tw, "NODE\tROUTE\tADVERTISED TO\tSERVICE\n")
		for _, r := range dp.RIBRoutes {
			advTo := strings.Join(r.AdvertisedTo, ", ")
			if advTo == "" {
				advTo = "(not advertised)"
			}
			svc := r.Service
			if svc == "" {
				svc = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Node, r.Prefix, advTo, svc)
		}
		tw.Flush()
	}

	if !importOnly && dp.ExportEnabled {
		fmt.Println()
		fmt.Println("=== Netlink Export ===")
		fmt.Printf("Config: enabled\n")
		if len(dp.ExportRules) > 0 {
			fmt.Print("Rules: ")
			var ruleParts []string
			for _, r := range dp.ExportRules {
				ruleParts = append(ruleParts, fmt.Sprintf("%q (metric=%d, table=%d)", r.Name, r.Metric, r.TableID))
			}
			fmt.Println(strings.Join(ruleParts, ", "))
		}

		if len(dp.ExportRoutes) > 0 {
			fmt.Println()
			tw := tableWriter(os.Stdout)
			fmt.Fprintf(tw, "NODE\tRECEIVED ROUTE\tIN KERNEL?\tTABLE\tMETRIC\n")
			for _, e := range dp.ExportRoutes {
				installed := "Yes"
				if !e.Installed {
					installed = "NO  *** EXPORT FAILURE ***"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", e.Node, e.Prefix, installed, e.Table, e.Metric)
			}
			tw.Flush()
		}
	}

	if !importOnly && !exportOnly && len(dp.CrossReference) > 0 {
		fmt.Println()
		fmt.Println("=== Cross-reference (remote pool services) ===")
		for _, cr := range dp.CrossReference {
			marker := ""
			if cr.Status != "OK" {
				marker = "  *** " + cr.Status + " ***"
			}
			nodeInfo := ""
			if cr.Node != "" {
				nodeInfo = fmt.Sprintf(" on %s", cr.Node)
			}
			fmt.Printf("  %s  %-30s  %s%s%s\n", cr.IP, cr.Service, cr.Status, nodeInfo, marker)
		}
	}

	if len(dp.VRFs) > 0 {
		fmt.Println()
		fmt.Println("=== VRFs ===")
		for _, v := range dp.VRFs {
			fmt.Printf("  %s: RD %s, %d imported routes, %d exported routes\n",
				v.Name, v.RD, v.ImportedRoutes, v.ExportedRoutes)
		}
	}

	if len(problems) > 0 {
		fmt.Println()
		fmt.Println("=== Problems ===")
		for _, p := range problems {
			fmt.Printf("  %s\n", p)
		}
	}

	return nil
}
