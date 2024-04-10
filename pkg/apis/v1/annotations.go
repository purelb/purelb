// Copyright 2020 Acnodal Inc.
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

package v1

const (
	// Annotations that the user can set to drive PureLB.

	// SharingAnnotation is the key for the annotation that indicates
	// whether a service can share its IP address with other
	// services. If two or more services have the same value in their
	// SharingAnnotation, and if they use different ports, then they can
	// share their IP address.
	SharingAnnotation string = "purelb.io/allow-shared-ip"

	// DesiredAddressAnnotation specifies an IP address (or a
	// comma-separated pair of addresses in the case of dual-stack) that
	// PureLB should use for this Service. If it's not present then
	// PureLB will allocate the next available address from the
	// specified service-group (or the default if none is specified).
	DesiredAddressAnnotation string = "purelb.io/addresses"

	// DesiredGroupAnnotation is the key for the annotation that
	// indicates the pool from which the user would like PureLB to
	// allocate this service's IP address.
	DesiredGroupAnnotation string = "purelb.io/service-group"

	// AllowLocalAnnotation tells PureLB to allow this Service
	// to implement "Local" ExternalTrafficPolicy. We usually don't
	// allow this, because it means that PureLB might announce an IP
	// address from a node that has no Pod belonging to this Service. At
	// least one user has found that "Local" is useful for them, though,
	// so this annotation overrides that policy.
	AllowLocalAnnotation string = "purelb.io/allow-local"

	// Annotations that PureLB sets that might be useful to users.

	// BrandAnnotation is the key for the PureLB "brand" annotation.
	// It's set when PureLB allocates an IP address for a service.
	BrandAnnotation string = "purelb.io/allocated-by"

	// Brand is the brand name of this product. It's the value of the
	// BrandAnnotation annotation.
	Brand string = "PureLB"

	// PoolAnnotation is the key for the annotation that indicates from
	// which pool the IP address was allocated. Pools are configured by
	// the PureLB ServiceGroup custom resource.
	PoolAnnotation string = "purelb.io/allocated-from"

	// AnnounceAnnotation is the key for the annotation that indicates
	// which node/intf is announcing this service's IP address. The IP
	// family name will be appended because in a dual-stack service we
	// might announce different IP addresses on different hosts.
	AnnounceAnnotation string = "purelb.io/announcing"
)
