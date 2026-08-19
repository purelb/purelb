// Copyright 2020-2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v2

import (
	"net"
	"strings"
)

const (
	// ============================================================================
	// User-settable annotations (on Services)
	// ============================================================================

	// SharingAnnotation enables IP address sharing between services.
	// If two or more services have the same value in their SharingAnnotation,
	// and if they use different ports, then they can share their IP address.
	SharingAnnotation string = "purelb.io/allow-shared-ip"

	// DesiredAddressAnnotation specifies a specific IP address (or a
	// comma-separated pair for dual-stack) that PureLB should use.
	// If not present, PureLB allocates the next available address from
	// the specified ServiceGroup (or default if none specified).
	DesiredAddressAnnotation string = "purelb.io/addresses"

	// DesiredGroupAnnotation specifies the ServiceGroup from which
	// to allocate this service's IP address.
	DesiredGroupAnnotation string = "purelb.io/service-group"

	// AllowLocalAnnotation allows ExternalTrafficPolicy=Local for local
	// addresses. Normally PureLB doesn't allow this because it means
	// PureLB might announce an IP from a node with no matching Pod.
	// This annotation overrides that policy.
	AllowLocalAnnotation string = "purelb.io/allow-local"

	// MultiPoolAnnotation enables or disables multi-pool allocation for
	// a service. When "true", the service gets one IP from each address
	// range (per family) that has active nodes. This overrides the
	// ServiceGroup's MultiPool setting if present.
	MultiPoolAnnotation string = "purelb.io/multi-pool"

	// ReEvaluateAnnotation is a one-shot trigger to force reprocessing of
	// a service. Set to "true" by the user; the allocator deletes it after
	// processing. Useful when subnets change without a config change.
	ReEvaluateAnnotation string = "purelb.io/re-evaluate"

	// NodeAffinityAnnotation, when set on a Service whose VIP comes from a
	// Local pool, biases election toward nodes that host a Ready or Serving
	// endpoint of this Service. The only supported value is
	// "service-endpoints" (NodeAffinityServiceEndpoints); any other value
	// is ignored. Has no effect on Remote-pool services (which already get
	// affinity via BGP-from-endpoint-nodes when ETP=Local). Has no effect
	// on services that share an IP via SharingAnnotation. When no preferred
	// node is eligible, election falls back silently to standard hash
	// selection (tracked via purelb_election_affinity_fallback_total).
	NodeAffinityAnnotation string = "purelb.io/node-affinity"

	// NodeAffinityServiceEndpoints is the only currently-supported value
	// for NodeAffinityAnnotation. String-valued (not boolean) so future
	// modes (e.g. topology-zone) can extend without inventing a second
	// annotation.
	NodeAffinityServiceEndpoints string = "service-endpoints"

	// ============================================================================
	// PureLB-set annotations (informational)
	// ============================================================================

	// BrandAnnotation is set when PureLB allocates an IP address for a service.
	BrandAnnotation string = "purelb.io/allocated-by"

	// Brand is the brand name value for BrandAnnotation.
	Brand string = "PureLB"

	// PoolAnnotation indicates from which ServiceGroup(s) the IP addresses
	// were allocated.
	PoolAnnotation string = "purelb.io/allocated-from"

	// PoolTypeAnnotation indicates the type of pool from which the address
	// was allocated. Values: "local" or "remote". This helps the announcer
	// determine which interface to use for announcement.
	PoolTypeAnnotation string = "purelb.io/pool-type"

	// PoolTypeLocal indicates the address is from a local pool and should
	// be announced on the local interface.
	PoolTypeLocal string = "local"

	// PoolTypeRemote indicates the address is from a remote pool and should
	// be announced on the dummy interface.
	PoolTypeRemote string = "remote"

	// AnnounceAnnotation indicates which node/interface is announcing
	// this service's IP address. The IP family name is appended (e.g.,
	// "-IPv4", "-IPv6") for dual-stack services.
	AnnounceAnnotation string = "purelb.io/announcing"

	// SkipIPv6DADAnnotation when set to "true" on a Service, skips
	// Duplicate Address Detection for IPv6 addresses. This is set by
	// the allocator when the ServiceGroup has skipIPv6DAD enabled.
	SkipIPv6DADAnnotation string = "purelb.io/skip-ipv6-dad"

	// ============================================================================
	// Lease annotations (coordination between nodes)
	// ============================================================================

	// LeasePrefix is prepended to node names to form lease names.
	LeasePrefix = "purelb-node-"

	// SubnetsAnnotation is the annotation key used on leases to store
	// the node's local subnets.
	SubnetsAnnotation = "purelb.io/subnets"

	// ============================================================================
	// Metrics
	// ============================================================================

	// MetricsNamespace is the Prometheus metrics namespace for PureLB.
	MetricsNamespace string = "purelb"

	// MaxAnnotationSubnets caps how many subnet entries we accept from a
	// single lease annotation. Peer leases are written by other nodes; a
	// buggy writer must not be able to bloat every node's election maps.
	MaxAnnotationSubnets = 256
)

// ParseSubnetsAnnotation parses the annotation value back into a slice
// of subnet strings. Returns an empty slice for empty input. Entries
// that are not valid CIDRs are dropped, and at most
// maxAnnotationSubnets entries are returned. Valid entries keep their
// original spelling — downstream consumers match them as exact strings
// against ServiceGroup subnet specs.
func ParseSubnetsAnnotation(annotation string) []string {
	if annotation == "" {
		return []string{}
	}
	entries := strings.Split(annotation, ",")
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			continue
		}
		result = append(result, entry)
		if len(result) == MaxAnnotationSubnets {
			break
		}
	}
	return result
}
