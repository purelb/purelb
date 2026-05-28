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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// bgpState is the cluster's BGP operational state. All three states are
// legitimate operating modes — NotEnabled is not a failure, it just means
// the user installed PureLB without the k8gobgp sidecar (a supported
// configuration documented for L2/ARP-only deployments).
type bgpState int

const (
	bgpStateNotEnabled    bgpState = iota // no k8gobgp sidecar — gobgp.enabled=false or no-bgp manifest variant
	bgpStateNotConfigured                 // sidecar present, no BGPConfiguration applied (or sidecar still starting up)
	bgpStateActive                        // sidecar present, BGPConfiguration applied, ≥1 neighbor configured
)

// bgpInfo summarizes the cluster's BGP state with all numbers the
// existing renderers need (counts, established peers, import failures).
type bgpInfo struct {
	state            bgpState
	nodeCount        int // number of BGPNodeStatus rows
	peersEstablished int
	peersTotal       int
	importFailures   int
}

// detectBGPState classifies the cluster's BGP state from a BGPNodeStatus
// list and a categorized pod set.
//
// bgpns may be nil — that's the normal case when the BGP CRDs aren't
// installed (no-bgp manifest variant or older PureLB versions). In that
// case we fall back to checking pod presence: no k8gobgp container means
// NotEnabled; container present means NotConfigured (transient startup).
//
// Classification rules:
//  1. peersTotal > 0 → Active (sub-stats already captured)
//  2. bgpns has rows, peersTotal == 0 → NotConfigured (rows written, no peers)
//  3. bgpns is nil OR empty, withK8GoBGP container present → NotConfigured (startup race)
//  4. bgpns is nil OR empty, withK8GoBGP container absent → NotEnabled
func detectBGPState(bgpns *unstructured.UnstructuredList, pods purelbPods) bgpInfo {
	info := bgpInfo{}

	if bgpns != nil {
		info.nodeCount = len(bgpns.Items)
		for _, ns := range bgpns.Items {
			neighbors, _, _ := unstructured.NestedSlice(ns.Object, "status", "neighbors")
			for _, nRaw := range neighbors {
				n, ok := nRaw.(map[string]interface{})
				if !ok {
					continue
				}
				info.peersTotal++
				state, _ := n["state"].(string)
				if state == "Established" {
					info.peersEstablished++
				}
			}
			addrs, _, _ := unstructured.NestedSlice(ns.Object, "status", "netlinkImport", "importedAddresses")
			for _, aRaw := range addrs {
				a, ok := aRaw.(map[string]interface{})
				if !ok {
					continue
				}
				inRIB, _ := a["inRIB"].(bool)
				if !inRIB {
					info.importFailures++
				}
			}
		}
	}

	switch {
	case info.peersTotal > 0:
		info.state = bgpStateActive
	case info.nodeCount > 0:
		// BGPNodeStatus rows exist but no neighbors: sidecar is running
		// and has written status, but no BGPConfiguration applied.
		info.state = bgpStateNotConfigured
	case len(pods.withK8GoBGP) > 0:
		// No rows but the sidecar container exists: either the manager
		// hasn't written status yet (startup race), or it's running
		// unconfigured. Either way, NotConfigured per the design.
		info.state = bgpStateNotConfigured
	default:
		info.state = bgpStateNotEnabled
	}

	return info
}

// statusSummary returns the canonical short string used on the BGP line of
// the status one-liner.
func (i bgpInfo) statusSummary() string {
	switch i.state {
	case bgpStateNotEnabled:
		return "not enabled"
	case bgpStateNotConfigured:
		return "not configured"
	case bgpStateActive:
		parts := []string{fmt.Sprintf("%d/%d peers established", i.peersEstablished, i.peersTotal)}
		if i.importFailures > 0 {
			parts = append(parts, fmt.Sprintf("%d import failure(s)", i.importFailures))
		} else {
			parts = append(parts, "netlinkImport OK")
		}
		return strings.Join(parts, " | ")
	}
	return "unknown"
}

// sentence returns the canonical longer-form sentence used by bgp sessions /
// bgp dataplane / dashboard / inspect / gobgp / ip when reporting
// NotEnabled or NotConfigured. Active returns an empty string — callers in
// that case render their full table or output instead.
func (i bgpInfo) sentence() string {
	switch i.state {
	case bgpStateNotEnabled:
		return "BGP not enabled."
	case bgpStateNotConfigured:
		return "BGP not configured."
	}
	return ""
}
