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

package v2

import "slices"

// ServesNamespace reports whether a Service in namespace may allocate from
// sg, considering spec.namespaces only.
//
// A ServiceGroup with no Namespaces entries serves every namespace. That is
// the behaviour of every ServiceGroup written before the field existed, so
// absence must never be read as "serves nothing".
//
// This deliberately ignores EnforceNamespaces. Whether the list is a boundary
// or merely a default is the caller's question; this answers only whether the
// namespace is in it.
func ServesNamespace(sg *ServiceGroup, namespace string) bool {
	if sg == nil || IsNamespaceCatchAll(sg) {
		return true
	}
	return slices.Contains(sg.Spec.Namespaces, namespace)
}

// IsNamespaceCatchAll reports whether sg names no namespace and therefore
// serves all of them.
//
// Exported so that anything deciding whether a group may serve a namespace
// decides it here rather than re-reading spec.namespaces; two implementations
// of "absent means all" would drift, and the drift would be silent.
//
// Note that `kubectl purelb validate` deliberately does NOT use these
// functions, despite asking a question that sounds identical. It reports
// whether a namespace is bound by two or more groups without exactly one
// naming itself the default -- and for that, a catch-all group is not a
// binding at all. ServesNamespace answers true for every catch-all, so using
// it there would make the "default" group eligible for every namespace and
// report ambiguity for configurations that have none.
func IsNamespaceCatchAll(sg *ServiceGroup) bool {
	return sg == nil || len(sg.Spec.Namespaces) == 0
}

// NamespaceBinding is the ServiceGroups that list one namespace.
//
// +k8s:deepcopy-gen=false
type NamespaceBinding struct {
	// Eligible is every ServiceGroup listing the namespace, sorted.
	Eligible []string
	// Enforce is whether any of them sets EnforceNamespaces: the
	// namespace is confined to them.
	Enforce bool
	// Defaults are those of them that set NamespaceDefault, sorted.
	Defaults []string
}

// NamespaceBindings indexes ServiceGroups by the namespaces they list.
//
// The allocator (for Services and egress policies) and the node agent (for
// a ClusterEgress's pods) both decide namespace scope from this, so the
// two can't disagree about which pods a policy may select.
//
// +k8s:deepcopy-gen=false
type NamespaceBindings struct {
	// Namespaces are the bindings of every listed namespace.
	Namespaces map[string]NamespaceBinding
	// Confined are the ServiceGroups serving a confined namespace.
	Confined map[string]bool
}

// BindNamespaces indexes groups by the namespaces they list. The caller
// passes only the groups that may bind: a group whose spec is broken must
// not deny a namespace on the strength of a YAML error elsewhere.
func BindNamespaces(groups []*ServiceGroup) NamespaceBindings {
	idx := map[string]NamespaceBinding{}
	for _, g := range groups {
		// Dedupe: +listType=set is deliberately not used (under server-side
		// apply it gives per-element ownership, so an entry deleted from Git
		// would silently persist). The API server therefore accepts
		// `namespaces: [a, a]`, and without this a single ServiceGroup with a
		// copy-pasted entry would appear twice in Eligible, look like two
		// groups competing, and deny its own namespace.
		for _, ns := range slices.Compact(slices.Sorted(slices.Values(g.Spec.Namespaces))) {
			b := idx[ns]
			b.Eligible = append(b.Eligible, g.Name)
			b.Enforce = b.Enforce || g.Spec.EnforceNamespaces
			if g.Spec.NamespaceDefault {
				b.Defaults = append(b.Defaults, g.Name)
			}
			idx[ns] = b
		}
	}
	// Exclusion is derived from namespace confinement, not from a group's own
	// EnforceNamespaces. Reading the target's own flag would fence a tenant's
	// L2 pool while leaving its BGP pool reachable from every namespace in
	// the cluster.
	confined := map[string]bool{}
	for ns, b := range idx {
		// Sorted for stable event text and inspect output only; it never
		// selects anything, so the build stays order-independent.
		slices.Sort(b.Eligible)
		slices.Sort(b.Defaults)
		idx[ns] = b
		if b.Enforce {
			for _, name := range b.Eligible {
				confined[name] = true
			}
		}
	}
	if len(idx) == 0 {
		return NamespaceBindings{}
	}
	return NamespaceBindings{Namespaces: idx, Confined: confined}
}

// Fence is why a ServiceGroup may not serve a namespace.
type Fence int

const (
	// Unfenced: the group may serve the namespace.
	Unfenced Fence = iota
	// FenceExcluded: the group serves a confined namespace and doesn't
	// list this one.
	FenceExcluded
	// FenceConfined: the namespace is confined to other groups.
	FenceConfined
)

// Fenced applies both halves of the namespace boundary to the ServiceGroup
// called name (sg, which may be nil) serving namespace ns. Both derive from
// the namespace, not from the group's own EnforceNamespaces.
func (b NamespaceBindings) Fenced(name string, sg *ServiceGroup, ns string) Fence {
	if b.Confined[name] && !ServesNamespace(sg, ns) {
		return FenceExcluded
	}
	if nb, bound := b.Namespaces[ns]; bound && nb.Enforce && !slices.Contains(nb.Eligible, name) {
		return FenceConfined
	}
	return Unfenced
}
