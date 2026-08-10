// Copyright 2020-2026 Acnodal Inc.
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

package allocator

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Provenance values reported alongside a resolved pool. viaAmbiguous is
// distinct from viaDefault so that a Service which landed on "default" only
// because its namespace could not resolve one is visible without the caller
// re-deriving why.
const (
	viaAnnotation = "annotation"
	viaNamespace  = "namespace"
	viaDefault    = "default"
	viaAmbiguous  = "default (ambiguous binding)"
)

// Sentinel errors. The metric taxonomy in stats.go tags failures by reason,
// but the only failure sites are a single `if err != nil` in Allocate and one
// in SetBalancer -- without sentinels the caller would have to string-match,
// which is exactly how the verbatim `unknown pool %q` text gets broken.
var (
	// ErrUnknownPool means no ServiceGroup of that name produced a pool.
	ErrUnknownPool = errors.New("unknown pool")
	// ErrNoUsablePool means the ServiceGroup exists but failed to parse, so
	// it has no pool. Distinct from ErrUnknownPool because telling an
	// operator their ServiceGroup does not exist when it plainly does sends
	// them looking in the wrong place.
	ErrNoUsablePool = errors.New("no usable pool")
	// ErrNamespaceDenied means the boundary refused this combination.
	ErrNamespaceDenied = errors.New("namespace denied")
	// ErrAmbiguousDefault means several ServiceGroups serve the namespace and
	// none or more than one is marked as its default.
	ErrAmbiguousDefault = errors.New("ambiguous namespace default")
)

// allocationFailureReason maps an allocation error to a bounded metric
// label. The sentinels above exist so this can classify without string
// matching; anything unrecognised lands in "other" rather than minting a
// new series, because the error text can contain a Service's own
// annotation values.
func allocationFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrNamespaceDenied):
		return "namespace_denied"
	case errors.Is(err, ErrAmbiguousDefault):
		return "ambiguous_namespace_default"
	case errors.Is(err, ErrNoUsablePool):
		return "no_usable_pool"
	case errors.Is(err, ErrUnknownPool):
		return "unknown_pool"
	default:
		return "other"
	}
}

// recordAllocationFailure counts a failed allocation by reason.
//
// The pool label is the requested ServiceGroup only when the Service named
// one, and even then it is bounded by the annotation being a ServiceGroup
// name; when nothing was named there is no pool to attribute the failure
// to, so it is recorded as "<none>". Never the raw error text, which would
// let a Service mint unbounded Prometheus series.
func recordAllocationFailure(svc *v1.Service, err error) {
	pool := svc.Annotations[purelbv2.DesiredGroupAnnotation]
	if pool == "" {
		pool = "<none>"
	}
	allocationRejected.WithLabelValues(pool, allocationFailureReason(err)).Inc()
}

// isRetryableAllocationErr reports whether re-running the same allocation
// later could plausibly succeed.
//
// Only transport-level failures against an external IPAM sidecar qualify.
// Everything the allocator decides for itself -- exhaustion, sharing
// conflicts, namespace boundaries, unparseable addresses -- is a property
// of the request or the config and will fail identically on every retry.
func isRetryableAllocationErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true
	}
	return false
}

// resolve returns the pool svc must allocate from and the provenance of the
// choice.
//
// Precedence: an explicit purelb.io/service-group annotation, then the
// namespace's default, then the pool named "default" -- which is exactly the
// behaviour that predates this feature, so a cluster with no spec.namespaces
// anywhere is unaffected.
func (st *allocatorState) resolve(svc *v1.Service) (Pool, string, error) {
	ns := svc.Namespace
	b, bound := st.nsToPools[ns]

	// An annotation may name any ELIGIBLE ServiceGroup, including one that
	// is not the namespace's default. That is how a tenant reaches its BGP
	// pool when L2 is the default.
	if name, has := svc.Annotations[purelbv2.DesiredGroupAnnotation]; has {
		p, err := st.checked(ns, name)
		return p, viaAnnotation, err
	}

	if bound && b.def != "" {
		p, err := st.checked(ns, b.def)
		return p, viaNamespace, err
	}

	// Several ServiceGroups serve this namespace and none is marked as its
	// default. Enforcing => deny, because falling through to "default" would
	// breach the boundary the namespace just declared. Not enforcing => fall
	// through, which is what this namespace did before any binding existed,
	// so an additive config cannot break it.
	if bound && b.enforce {
		return nil, viaNamespace, fmt.Errorf("%w: %s", ErrAmbiguousDefault, b.defErr)
	}
	via := viaDefault
	if bound {
		via = viaAmbiguous
	}
	p, err := st.checked(ns, defaultPoolName)
	return p, via, err
}

// checked resolves one ServiceGroup by name and applies both halves of the
// boundary.
//
// Both halves derive from the NAMESPACE, not from the requested
// ServiceGroup's own EnforceNamespaces. Reading exclusion off the target
// would leave a half-open fence: enforcing on a tenant's L2 pool would leave
// its BGP pool reachable from every other namespace in the cluster.
func (st *allocatorState) checked(ns, name string) (Pool, error) {
	pool, has := st.pools[name]
	if !has {
		// Distinguish "no such ServiceGroup" from "it exists but produced no
		// pool"; the index binds from the spec, so a name here can belong to
		// a group that failed parseGroups.
		if _, exists := st.sgByName[name]; exists {
			return nil, fmt.Errorf("%w: ServiceGroup %q failed to parse", ErrNoUsablePool, name)
		}
		return nil, fmt.Errorf("%w %q", ErrUnknownPool, name)
	}

	// Exclusion: a ServiceGroup serving any confined namespace refuses
	// Services from outside the namespaces it serves.
	if st.confinedGroups[name] && !purelbv2.ServesNamespace(st.sgByName[name], ns) {
		return nil, fmt.Errorf("%w: ServiceGroup %q serves a confined namespace and does not serve %q",
			ErrNamespaceDenied, name, ns)
	}

	// Confinement: a confined namespace may use only the ServiceGroups that
	// serve it.
	if b, bound := st.nsToPools[ns]; bound && b.enforce && !slices.Contains(b.eligible, name) {
		return nil, fmt.Errorf("%w: namespace %q may only use %v, not ServiceGroup %q",
			ErrNamespaceDenied, ns, b.eligible, name)
	}

	return pool, nil
}
