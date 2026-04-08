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
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type inspectResult struct {
	Service      string             `json:"service"`
	Type         string             `json:"type"`
	IPFamilies   string             `json:"ipFamilies"`
	ETP          string             `json:"externalTrafficPolicy"`
	Status       string             `json:"status"`
	Allocations  []allocationInfo   `json:"allocations,omitempty"`
	Elections    []electionInfo     `json:"elections,omitempty"`
	Announcements []announcementInfo `json:"announcements,omitempty"`
	Sharing      *sharingInfo       `json:"sharing,omitempty"`
	Endpoints    endpointsInfo      `json:"endpoints"`
	BGP          []bgpRouteCheck    `json:"bgp,omitempty"`
	Diagnosis    *diagnosisInfo     `json:"diagnosis,omitempty"`
}

type allocationInfo struct {
	IP          string `json:"ip"`
	Pool        string `json:"pool"`
	PoolType    string `json:"poolType"`
	Range       string `json:"range,omitempty"`
	Subnet      string `json:"subnet,omitempty"`
}

type electionInfo struct {
	IP         string   `json:"ip"`
	Candidates []string `json:"candidates"`
	Subnet     string   `json:"subnet"`
	Winner     string   `json:"winner"`
	Matches    bool     `json:"matchesAnnouncer"`
}

type announcementInfo struct {
	IP        string `json:"ip"`
	Node      string `json:"node"`
	Interface string `json:"interface"`
	Healthy   bool   `json:"leaseHealthy"`
	RenewedAgo string `json:"renewedAgo,omitempty"`
}

type sharingInfo struct {
	Key      string   `json:"key"`
	CoTenants []string `json:"coTenants"` // "namespace/name (ports)"
}

type endpointsInfo struct {
	Ready int      `json:"ready"`
	Nodes []string `json:"nodes"`
}

type bgpRouteCheck struct {
	IP       string `json:"ip"`
	Node     string `json:"node"`
	InRIB    bool   `json:"inRIB"`
	Advertised bool  `json:"advertised"`
}

type diagnosisInfo struct {
	Reason  string   `json:"reason"`
	Details []string `json:"details"`
}

func newInspectCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "inspect <namespace/service>",
		Short: "Deep-dive into a single service: allocation, election, endpoints, BGP",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runInspect(cmd.Context(), c, format, args[0])
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")

	return cmd
}

func runInspect(ctx context.Context, c *clients, format outputFormat, svcArg string) error {
	// Parse namespace/name
	ns, name, err := parseSvcArg(svcArg, c.namespace)
	if err != nil {
		return err
	}

	// Fetch the service
	svc, err := c.core.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting service %s/%s: %w", ns, name, err)
	}

	now := time.Now()
	nsName := ns + "/" + name
	ann := svc.Annotations
	if ann == nil {
		ann = map[string]string{}
	}

	result := inspectResult{
		Service:    nsName,
		Type:       string(svc.Spec.Type),
		ETP:        string(svc.Spec.ExternalTrafficPolicy),
	}

	// IP families
	var families []string
	for _, f := range svc.Spec.IPFamilies {
		families = append(families, string(f))
	}
	ipFamilyPolicy := ""
	if svc.Spec.IPFamilyPolicy != nil {
		ipFamilyPolicy = string(*svc.Spec.IPFamilyPolicy)
	}
	if len(families) > 0 {
		result.IPFamilies = strings.Join(families, ", ")
		if ipFamilyPolicy != "" {
			result.IPFamilies += " (" + ipFamilyPolicy + ")"
		}
	}

	// Check if allocated
	ingresses := svc.Status.LoadBalancer.Ingress
	isAllocated := ann[annotationAllocatedBy] == brandPureLB && len(ingresses) > 0

	if !isAllocated {
		result.Status = "PENDING"
		result.Diagnosis = diagnosePending(ctx, c, svc)
	} else {
		result.Status = "Allocated"

		poolName := ann[annotationAllocatedFrom]
		poolType := ann[annotationPoolType]
		sharingKey := ann[annotationSharing]

		// Fetch ServiceGroups for range lookup
		sgList, _ := c.dynamic.Resource(gvrServiceGroups).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})

		// Fetch leases for election
		leaseList, _ := c.core.CoordinationV1().Leases(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
		nodeSubnets := map[string][]string{}
		healthyNodes := map[string]bool{}
		leaseRenewed := map[string]time.Time{}
		for _, lease := range leaseList.Items {
			if !strings.HasPrefix(lease.Name, leasePrefix) {
				continue
			}
			nodeName := lease.Name[len(leasePrefix):]
			subs := parseSubnetsAnnotation(lease.GetAnnotations()[subnetsAnnotation])
			nodeSubnets[nodeName] = subs
			healthyNodes[nodeName] = isLeaseHealthy(&lease, now)
			if lease.Spec.RenewTime != nil {
				leaseRenewed[nodeName] = lease.Spec.RenewTime.Time
			}
		}

		// Parse announcing annotations
		announcers := map[string]announcement{}
		for _, suffix := range []string{"-IPv4", "-IPv6"} {
			for _, a := range parseAnnouncingAnnotation(ann[annotationAnnouncing+suffix]) {
				announcers[a.IP] = a
			}
		}

		// Fetch BGPNodeStatus for remote pool route checks
		var bgpStatuses *unstructured.UnstructuredList
		if poolType == poolTypeRemote {
			bgpStatuses, _ = c.dynamic.Resource(gvrBGPNodeStatuses).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
		}

		for _, ingress := range ingresses {
			ipStr := ingress.IP
			if ipStr == "" {
				continue
			}
			ip := net.ParseIP(ipStr)

			// Allocation info
			alloc := allocationInfo{
				IP:       ipStr,
				Pool:     poolName,
				PoolType: poolType,
			}
			// Find which range contains this IP
			if sgList != nil {
				alloc.Range, alloc.Subnet = findRangeForIP(sgList, poolName, ip)
			}
			result.Allocations = append(result.Allocations, alloc)

			// Election (local pools only)
			if poolType == poolTypeLocal && ip != nil {
				var candidates []string
				matchedSubnet := ""
				for nodeName, subs := range nodeSubnets {
					if !healthyNodes[nodeName] {
						continue
					}
					if subnetContainsIP(subs, ip) {
						candidates = append(candidates, nodeName)
						// Find the matching subnet for display
						for _, s := range subs {
							_, ipnet, err := net.ParseCIDR(s)
							if err == nil && ipnet.Contains(ip) {
								matchedSubnet = s
							}
						}
					}
				}

				winner := electionWinner(ipStr, candidates)
				actualAnnouncer := ""
				if a, ok := announcers[ipStr]; ok {
					actualAnnouncer = a.Node
				}

				result.Elections = append(result.Elections, electionInfo{
					IP:         ipStr,
					Candidates: candidates,
					Subnet:     matchedSubnet,
					Winner:     winner,
					Matches:    winner == actualAnnouncer || (winner != "" && actualAnnouncer == ""),
				})
			}

			// Announcement info
			if a, ok := announcers[ipStr]; ok {
				ai := announcementInfo{
					IP:        ipStr,
					Node:      a.Node,
					Interface: a.Interface,
					Healthy:   healthyNodes[a.Node],
				}
				if t, ok := leaseRenewed[a.Node]; ok {
					ai.RenewedAgo = formatDuration(now.Sub(t))
				}
				result.Announcements = append(result.Announcements, ai)
			} else if poolType == poolTypeRemote {
				// Remote pools: annotation is just the interface name (all nodes announce)
				for _, a := range announcers {
					if a.Node == "" {
						result.Announcements = append(result.Announcements, announcementInfo{
							IP:        ipStr,
							Interface: a.Interface,
							Healthy:   true,
						})
						break
					}
				}
			}

			// BGP route check (remote pools)
			if poolType == poolTypeRemote && bgpStatuses != nil {
				for _, bgpns := range bgpStatuses.Items {
					nodeName, _, _ := unstructured.NestedString(bgpns.Object, "status", "nodeName")
					inRIB := checkRouteInBGPNodeStatus(&bgpns, ipStr)
					advertised := checkRouteAdvertised(&bgpns, ipStr)
					if inRIB || advertised {
						result.BGP = append(result.BGP, bgpRouteCheck{
							IP:         ipStr,
							Node:       nodeName,
							InRIB:      inRIB,
							Advertised: advertised,
						})
					}
				}
				// If no BGP entries found, add a "not found" entry
				if len(result.BGP) == 0 {
					result.BGP = append(result.BGP, bgpRouteCheck{
						IP:    ipStr,
						Node:  "(none)",
						InRIB: false,
					})
				}
			}
		}

		// Sharing info
		if sharingKey != "" {
			si := &sharingInfo{Key: sharingKey}
			// Find other services with the same sharing key
			allSvcs, _ := c.core.CoreV1().Services("").List(ctx, metav1.ListOptions{ResourceVersion: "0", FieldSelector: svcFieldSelector})
			if allSvcs != nil {
				for _, other := range allSvcs.Items {
					otherNsName := other.Namespace + "/" + other.Name
					if otherNsName == nsName {
						continue
					}
					if other.Annotations[annotationSharing] == sharingKey {
						var ports []string
						for _, p := range other.Spec.Ports {
							ports = append(ports, fmt.Sprintf("%s/%d", p.Protocol, p.Port))
						}
						si.CoTenants = append(si.CoTenants, fmt.Sprintf("%s (%s)", otherNsName, strings.Join(ports, ",")))
					}
				}
			}
			if len(si.CoTenants) > 0 {
				result.Sharing = si
			}
		}
	}

	// Endpoints
	epSlices, _ := c.core.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		ResourceVersion: "0",
		LabelSelector:   fmt.Sprintf("kubernetes.io/service-name=%s", name),
	})
	if epSlices != nil {
		result.Endpoints = countEndpoints(epSlices.Items)
	}

	if format != outputTable {
		return printStructured(format, result)
	}

	// Human-readable output
	fmt.Printf("Service: %s (%s, %s, ETP=%s)\n", result.Service, result.Type, result.IPFamilies, result.ETP)
	fmt.Println()

	if result.Diagnosis != nil {
		fmt.Printf("Status: %s - no IP allocated\n\n", result.Status)
		fmt.Println("Diagnosis:")
		for _, d := range result.Diagnosis.Details {
			fmt.Printf("  %s\n", d)
		}
		fmt.Printf("  >>> %s <<<\n", result.Diagnosis.Reason)
	} else {
		// Allocations
		fmt.Println("Allocation:")
		for _, a := range result.Allocations {
			rangeStr := ""
			if a.Range != "" {
				rangeStr = fmt.Sprintf(", range %s, subnet %s", a.Range, a.Subnet)
			}
			fmt.Printf("  %s: %s from %s (%s%s)\n", familyLabel(a.IP), a.IP, a.Pool, a.PoolType, rangeStr)
		}

		// Sharing
		if result.Sharing != nil {
			fmt.Printf("  Sharing: key %q, shared with:\n", result.Sharing.Key)
			for _, ct := range result.Sharing.CoTenants {
				fmt.Printf("    %s\n", ct)
			}
		} else {
			fmt.Println("  Sharing: none")
		}

		// Elections (local only)
		if len(result.Elections) > 0 {
			fmt.Println()
			for _, e := range result.Elections {
				match := "MATCHES ANNOUNCER"
				if !e.Matches {
					match = "DOES NOT MATCH ANNOUNCER"
				}
				fmt.Printf("Election (%s):\n", e.IP)
				fmt.Printf("  Candidates: %s (subnet %s)\n", strings.Join(e.Candidates, ", "), e.Subnet)
				fmt.Printf("  Winner: %s (SHA256 hash)  [%s]\n", e.Winner, match)
			}
		}

		// Announcements
		if len(result.Announcements) > 0 {
			fmt.Println()
			fmt.Println("Announcement:")
			for _, a := range result.Announcements {
				healthStr := "healthy"
				if !a.Healthy {
					healthStr = "UNHEALTHY"
				}
				fmt.Printf("  %s: %s / %s  (lease %s, renewed %s ago)\n",
					familyLabel(a.IP), a.Node, a.Interface, healthStr, a.RenewedAgo)
			}
		}

		// BGP
		if len(result.BGP) > 0 {
			fmt.Println()
			fmt.Println("BGP (remote pool):")
			for _, b := range result.BGP {
				ribStr := "Yes"
				if !b.InRIB {
					ribStr = "No"
				}
				advStr := "Yes"
				if !b.Advertised {
					advStr = "No"
				}
				fmt.Printf("  Route %s in RIB on %s: %s, Advertised: %s\n",
					b.IP, b.Node, ribStr, advStr)
			}
		}
	}

	// Endpoints
	fmt.Println()
	if result.Endpoints.Ready > 0 {
		fmt.Printf("Endpoints: %d ready (%s)\n", result.Endpoints.Ready, strings.Join(result.Endpoints.Nodes, ", "))
	} else {
		fmt.Println("Endpoints: 0 ready")
	}

	return nil
}

func parseSvcArg(arg, defaultNS string) (string, string, error) {
	parts := strings.SplitN(arg, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	if defaultNS == "" {
		return "", "", fmt.Errorf("no namespace specified: use namespace/service or --namespace flag")
	}
	return defaultNS, parts[0], nil
}

func familyLabel(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "unknown"
	}
	if ip.To4() != nil {
		return "IPv4"
	}
	return "IPv6"
}

// findRangeForIP finds which pool range in a ServiceGroup contains the given IP.
func findRangeForIP(sgList *unstructured.UnstructuredList, poolName string, ip net.IP) (string, string) {
	if ip == nil || sgList == nil {
		return "", ""
	}
	for _, sg := range sgList.Items {
		if sg.GetName() != poolName {
			continue
		}
		spec, _, _ := unstructured.NestedMap(sg.Object, "spec")
		if spec == nil {
			continue
		}
		for _, specKey := range []string{"local", "remote"} {
			poolSpec, ok := spec[specKey]
			if !ok {
				continue
			}
			ps, ok := poolSpec.(map[string]interface{})
			if !ok {
				continue
			}
			for _, key := range []string{"v4pools", "v6pools", "v4pool", "v6pool"} {
				raw, ok := ps[key]
				if !ok {
					continue
				}
				// Array pools
				if pools, ok := raw.([]interface{}); ok {
					for _, pRaw := range pools {
						p, ok := pRaw.(map[string]interface{})
						if !ok {
							continue
						}
						poolStr, _ := p["pool"].(string)
						subnet, _ := p["subnet"].(string)
						ipr, err := newIPRange(poolStr)
						if err == nil && ipr.contains(ip) {
							return poolStr, subnet
						}
					}
				}
				// Singular pool
				if p, ok := raw.(map[string]interface{}); ok {
					poolStr, _ := p["pool"].(string)
					subnet, _ := p["subnet"].(string)
					ipr, err := newIPRange(poolStr)
					if err == nil && ipr.contains(ip) {
						return poolStr, subnet
					}
				}
			}
		}
	}
	return "", ""
}

// diagnosePending tries to explain why a service is stuck in Pending.
func diagnosePending(ctx context.Context, c *clients, svc *v1.Service) *diagnosisInfo {
	ann := svc.Annotations
	if ann == nil {
		ann = map[string]string{}
	}

	diag := &diagnosisInfo{}
	requestedPool := ann[annotationServiceGroup]
	requestedIP := ann[annotationAddresses]

	// Check allocator pod
	pods, _ := c.core.CoreV1().Pods(purelbNamespace).List(ctx, metav1.ListOptions{
		ResourceVersion: "0",
		LabelSelector:   "app=purelb,component=allocator",
	})
	allocatorRunning := false
	if pods != nil {
		for _, p := range pods.Items {
			if p.Status.Phase == v1.PodRunning {
				allocatorRunning = true
				break
			}
		}
	}
	if !allocatorRunning {
		diag.Reason = "ALLOCATOR NOT RUNNING"
		diag.Details = append(diag.Details, "No running allocator pod found in "+purelbNamespace)
		return diag
	}

	if requestedPool == "" {
		requestedPool = "default"
	}
	diag.Details = append(diag.Details, fmt.Sprintf("Requested pool: %q (from annotation %s)", requestedPool, annotationServiceGroup))

	// Check if ServiceGroup exists
	sgList, _ := c.dynamic.Resource(gvrServiceGroups).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	var targetSG *unstructured.Unstructured
	if sgList != nil {
		for i, sg := range sgList.Items {
			if sg.GetName() == requestedPool {
				targetSG = &sgList.Items[i]
				break
			}
		}
	}

	if targetSG == nil {
		diag.Reason = fmt.Sprintf("SERVICEGROUP %q NOT FOUND", requestedPool)
		diag.Details = append(diag.Details, "Pool exists: No")
		return diag
	}
	diag.Details = append(diag.Details, "Pool exists: Yes")

	// Determine pool type
	spec, _, _ := unstructured.NestedMap(targetSG.Object, "spec")
	poolType := "unknown"
	if _, ok := spec["local"]; ok {
		poolType = "local"
	} else if _, ok := spec["remote"]; ok {
		poolType = "remote"
	} else if _, ok := spec["netbox"]; ok {
		poolType = "netbox"
	}
	diag.Details = append(diag.Details, fmt.Sprintf("Pool type: %s", poolType))

	// Calculate capacity
	if poolType == "local" || poolType == "remote" {
		svcs := parsePoolSpec(requestedPool, poolType, spec[poolType], nil)
		var totalSize uint64
		var totalUsed int
		for _, r := range svcs {
			totalSize += r.Size
			totalUsed += r.Used
		}
		diag.Details = append(diag.Details, fmt.Sprintf("Pool capacity: %d total, %d used, %d available",
			totalSize, totalUsed, totalSize-uint64(totalUsed)))

		if totalSize > 0 && uint64(totalUsed) >= totalSize {
			diag.Reason = fmt.Sprintf("POOL EXHAUSTED - no addresses available in %s", requestedPool)
			return diag
		}
	}

	// Check specific IP request
	if requestedIP != "" {
		diag.Details = append(diag.Details, fmt.Sprintf("Requested IP: %s", requestedIP))
		// Check if it's in the pool range
		ip := net.ParseIP(requestedIP)
		if ip != nil && targetSG != nil {
			rng, _ := findRangeForIP(sgList, requestedPool, ip)
			if rng == "" {
				diag.Reason = fmt.Sprintf("REQUESTED IP %s IS OUTSIDE POOL RANGE", requestedIP)
				return diag
			}
		}
	} else {
		diag.Details = append(diag.Details, "Requested IP: none (automatic allocation)")
	}

	sharingKey := ann[annotationSharing]
	if sharingKey != "" {
		diag.Details = append(diag.Details, fmt.Sprintf("Sharing key: %s", sharingKey))
	} else {
		diag.Details = append(diag.Details, "Sharing key: none")
	}

	diag.Reason = "UNKNOWN - allocator may be processing or service may have other issues"
	return diag
}

// countEndpoints counts ready endpoints and their nodes from EndpointSlices.
func countEndpoints(slices []discoveryv1.EndpointSlice) endpointsInfo {
	nodeSet := map[string]bool{}
	ready := 0
	for _, slice := range slices {
		for _, ep := range slice.Endpoints {
			isReady := ep.Conditions.Ready != nil && *ep.Conditions.Ready
			if isReady {
				ready++
				if ep.NodeName != nil {
					nodeSet[*ep.NodeName] = true
				}
			}
		}
	}
	var nodes []string
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return endpointsInfo{Ready: ready, Nodes: nodes}
}

// prefixMatchesIP returns true if a prefix string (e.g. "10.201.0.0/24") has
// the same IP portion as ipStr (e.g. "10.201.0.0"), regardless of mask length.
func prefixMatchesIP(prefix, ipStr string) bool {
	bareIP := strings.Split(prefix, "/")[0]
	return bareIP == ipStr
}

// checkRouteInBGPNodeStatus checks if an IP has a route in the BGPNodeStatus RIB,
// matching by bare IP regardless of prefix length (supports /32, /24, /128, etc).
func checkRouteInBGPNodeStatus(bgpns *unstructured.Unstructured, ipStr string) bool {
	// Check netlinkImport.importedAddresses
	importedAddrs, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "netlinkImport", "importedAddresses")
	for _, addrRaw := range importedAddrs {
		addr, ok := addrRaw.(map[string]interface{})
		if !ok {
			continue
		}
		addrStr, _ := addr["address"].(string)
		inRIB, _ := addr["inRIB"].(bool)
		if prefixMatchesIP(addrStr, ipStr) && inRIB {
			return true
		}
	}

	// Check rib.localRoutes
	localRoutes, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "rib", "localRoutes")
	for _, routeRaw := range localRoutes {
		route, ok := routeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		prefix, _ := route["prefix"].(string)
		if prefixMatchesIP(prefix, ipStr) {
			return true
		}
	}

	return false
}

// checkRouteAdvertised checks if a route for this IP is being advertised to any peer,
// matching by bare IP regardless of prefix length.
func checkRouteAdvertised(bgpns *unstructured.Unstructured, ipStr string) bool {
	localRoutes, _, _ := unstructured.NestedSlice(bgpns.Object, "status", "rib", "localRoutes")
	for _, routeRaw := range localRoutes {
		route, ok := routeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		prefix, _ := route["prefix"].(string)
		if prefixMatchesIP(prefix, ipStr) {
			advTo, _, _ := unstructured.NestedStringSlice(route, "advertisedTo")
			return len(advTo) > 0
		}
	}

	return false
}
