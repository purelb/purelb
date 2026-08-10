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
// serves all of them. Exported because the kubectl plugin has to classify
// ServiceGroups exactly as the allocator does when it explains a decision;
// two implementations of this rule would drift.
func IsNamespaceCatchAll(sg *ServiceGroup) bool {
	return sg == nil || len(sg.Spec.Namespaces) == 0
}
