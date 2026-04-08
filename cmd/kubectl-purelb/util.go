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
	"crypto/sha256"
	"fmt"
	"math"
	"math/big"
	"net"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PureLB annotation constants (copied from pkg/apis/purelb/v2/annotations.go
// to avoid importing netlink-dependent package).
const (
	annotationAllocatedBy   = "purelb.io/allocated-by"
	annotationAllocatedFrom = "purelb.io/allocated-from"
	annotationPoolType      = "purelb.io/pool-type"
	annotationAnnouncing    = "purelb.io/announcing"
	annotationServiceGroup  = "purelb.io/service-group"
	annotationAddresses     = "purelb.io/addresses"
	annotationSharing       = "purelb.io/allow-shared-ip"
	annotationSkipDAD       = "purelb.io/skip-ipv6-dad"
	annotationMultiPool     = "purelb.io/multi-pool"
	annotationReEvaluate    = "purelb.io/re-evaluate"
	annotationAllowLocal    = "purelb.io/allow-local"

	brandPureLB = "PureLB"

	poolTypeLocal  = "local"
	poolTypeRemote = "remote"
)

// Election constants (copied from internal/election/).
const (
	leasePrefix       = "purelb-node-"
	subnetsAnnotation = "purelb.io/subnets"
)

// PureLB system namespace default.
const purelbNamespace = "purelb-system"

// svcFieldSelector limits Services.List to LoadBalancer type, reducing
// response payload and API server work via server-side filtering.
const svcFieldSelector = "spec.type=LoadBalancer"

// Address family constants (AF_INET=2, AF_INET6=10, matching netlink/nl).
const (
	familyV4 = 2
	familyV6 = 10
)

// dummyInterfaceName returns the dummy interface name from the LBNodeAgent CR,
// defaulting to "kube-lb0" if not configured or not found.
func dummyInterfaceName(ctx context.Context, c *clients) string {
	lbnaList, _ := c.dynamic.Resource(gvrLBNodeAgents).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if lbnaList != nil && len(lbnaList.Items) > 0 {
		di, _, _ := unstructured.NestedString(lbnaList.Items[0].Object, "spec", "local", "dummyInterface")
		if di != "" {
			return di
		}
	}
	return "kube-lb0"
}

// resolveDummyInterface extracts the dummy interface name from a pre-fetched
// LBNodeAgent list, defaulting to "kube-lb0". Use this when the list is already
// available (e.g., from a clusterSnapshot) to avoid an extra API call.
func resolveDummyInterface(lbnaList *unstructured.UnstructuredList) string {
	if lbnaList != nil && len(lbnaList.Items) > 0 {
		di, _, _ := unstructured.NestedString(lbnaList.Items[0].Object, "spec", "local", "dummyInterface")
		if di != "" {
			return di
		}
	}
	return "kube-lb0"
}

// addrFamily returns the address family of an IP address.
func addrFamily(ip net.IP) int {
	if ip.To4() != nil {
		return familyV4
	}
	if ip.To16() != nil {
		return familyV6
	}
	return 0
}

// electionWinner determines which node should announce the given IP address,
// using the same SHA256 hash algorithm as internal/election/election.go.
func electionWinner(key string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	sorted := make([]string, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		hi := sha256.Sum256([]byte(sorted[i] + "#" + key))
		hj := sha256.Sum256([]byte(sorted[j] + "#" + key))
		return bytes.Compare(hi[:], hj[:]) < 0
	})
	return sorted[0]
}

// parseSubnetsAnnotation splits the comma-separated subnets annotation value.
func parseSubnetsAnnotation(annotation string) []string {
	if annotation == "" {
		return []string{}
	}
	return strings.Split(annotation, ",")
}

// subnetContainsIP returns true if any of the given subnets (CIDR notation) contains the IP.
func subnetContainsIP(subnets []string, ip net.IP) bool {
	for _, subnet := range subnets {
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil {
			continue
		}
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// parseAnnouncingAnnotation parses the "node,iface,ip[ node2,iface2,ip2]" format.
type announcement struct {
	Node      string
	Interface string
	IP        string
}

func parseAnnouncingAnnotation(value string) []announcement {
	if value == "" {
		return nil
	}
	var result []announcement
	for _, entry := range strings.Fields(value) {
		parts := strings.SplitN(entry, ",", 3)
		switch len(parts) {
		case 3:
			// Local format: "node,iface,ip"
			result = append(result, announcement{
				Node:      parts[0],
				Interface: parts[1],
				IP:        parts[2],
			})
		case 1:
			// Remote format: just the interface name (e.g., "kube-lb0")
			result = append(result, announcement{
				Interface: parts[0],
			})
		}
	}
	return result
}

// ipRange represents a range of IP addresses (reimplemented from pkg/apis/purelb/v2/iprange.go
// without netlink dependency).
type ipRange struct {
	from net.IP
	to   net.IP
}

// newIPRange parses a CIDR or from-to range string.
func newIPRange(raw string) (ipRange, error) {
	if strings.Contains(raw, "-") {
		return parseFromTo(raw)
	}
	return parseCIDR(raw)
}

func parseCIDR(cidr string) (ipRange, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ipRange{}, fmt.Errorf("invalid CIDR %q", cidr)
	}
	// Calculate the last address in the CIDR
	first := ip.Mask(ipnet.Mask)
	last := make(net.IP, len(first))
	for i := range first {
		last[i] = first[i] | ^ipnet.Mask[i]
	}
	return ipRange{from: first, to: last}, nil
}

func parseFromTo(rawrange string) (ipRange, error) {
	parts := strings.SplitN(rawrange, "-", 2)
	if len(parts) != 2 {
		return ipRange{}, fmt.Errorf("invalid IP range %q", rawrange)
	}
	from := net.ParseIP(strings.TrimSpace(parts[0]))
	if from == nil {
		return ipRange{}, fmt.Errorf("invalid start IP in range %q", rawrange)
	}
	to := net.ParseIP(strings.TrimSpace(parts[1]))
	if to == nil {
		return ipRange{}, fmt.Errorf("invalid end IP in range %q", rawrange)
	}
	return ipRange{from: from, to: to}, nil
}

// size returns the count of addresses in this range.
func (r ipRange) size() uint64 {
	from16 := r.from.To16()
	to16 := r.to.To16()
	if from16 == nil || to16 == nil {
		return 0
	}

	fromInt := new(big.Int).SetBytes(from16)
	toInt := new(big.Int).SetBytes(to16)
	diff := new(big.Int).Sub(toInt, fromInt)
	diff.Add(diff, big.NewInt(1)) // inclusive range

	if !diff.IsUint64() {
		return math.MaxUint64
	}
	return diff.Uint64()
}

// contains returns true if the IP is within this range.
func (r ipRange) contains(ip net.IP) bool {
	ip16 := ip.To16()
	from16 := r.from.To16()
	to16 := r.to.To16()
	if ip16 == nil || from16 == nil || to16 == nil {
		return false
	}
	return bytes.Compare(ip16, from16) >= 0 && bytes.Compare(ip16, to16) <= 0
}

// family returns the address family of this range.
func (r ipRange) family() int {
	return addrFamily(r.from)
}

// String returns a human-readable representation.
func (r ipRange) String() string {
	return fmt.Sprintf("%s-%s", r.from, r.to)
}
