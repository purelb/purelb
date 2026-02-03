// Copyright 2020 Acnodal Inc.
// Copyright 2024 Acnodal Inc.
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
	// Metrics
	// ============================================================================

	// MetricsNamespace is the Prometheus metrics namespace for PureLB.
	MetricsNamespace string = "purelb"
)
