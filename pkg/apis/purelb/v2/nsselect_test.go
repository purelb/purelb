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
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sgWithNamespaces(namespaces []string, enforce bool) *ServiceGroup {
	return &ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: "sg"},
		Spec: ServiceGroupSpec{
			Namespaces:        namespaces,
			EnforceNamespaces: enforce,
		},
	}
}

func TestIsNamespaceCatchAll(t *testing.T) {
	tests := []struct {
		name string
		sg   *ServiceGroup
		want bool
	}{
		// The backwards-compatibility invariant: every ServiceGroup written
		// before spec.namespaces existed has no entries, and all of them
		// must keep serving every namespace.
		{"nil ServiceGroup", nil, true},
		{"nil slice", sgWithNamespaces(nil, false), true},
		{"empty slice", sgWithNamespaces([]string{}, false), true},
		{"one namespace", sgWithNamespaces([]string{"tenant-a"}, false), false},
		{"several namespaces", sgWithNamespaces([]string{"a", "b"}, false), false},
		// enforceNamespaces does not change what "catch-all" means.
		{"empty but enforcing", sgWithNamespaces(nil, true), true},
		{"listed and enforcing", sgWithNamespaces([]string{"a"}, true), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsNamespaceCatchAll(tc.sg))
		})
	}
}

// TestServesNamespace pins the rule shared between the allocator
// (internal/allocator/poolselect.go) and the kubectl plugin. Two
// implementations of it would drift, which is why it is exported.
func TestServesNamespace(t *testing.T) {
	tests := []struct {
		name      string
		sg        *ServiceGroup
		namespace string
		want      bool
	}{
		// A nil ServiceGroup must never be read as "serves nothing".
		{"nil serves anything", nil, "anything", true},
		{"catch-all serves a named namespace", sgWithNamespaces(nil, false), "tenant-a", true},
		{"catch-all serves the empty namespace", sgWithNamespaces(nil, false), "", true},
		{"listed namespace", sgWithNamespaces([]string{"tenant-a"}, false), "tenant-a", true},
		{"unlisted namespace", sgWithNamespaces([]string{"tenant-a"}, false), "tenant-b", false},
		{"one of several", sgWithNamespaces([]string{"a", "b", "c"}, false), "b", true},
		// A duplicate entry is still just set membership, not a weighting.
		{"duplicate entry", sgWithNamespaces([]string{"a", "a"}, false), "a", true},
		// A scoped group must not serve the empty namespace by accident.
		{"scoped does not serve empty", sgWithNamespaces([]string{"a"}, false), "", false},
		// EnforceNamespaces is deliberately ignored here: whether the list
		// is a boundary or merely a default is the caller's question. These
		// two cases pin that contract -- the answers must match the
		// non-enforcing ones above.
		{"enforcing, listed", sgWithNamespaces([]string{"tenant-a"}, true), "tenant-a", true},
		{"enforcing, unlisted", sgWithNamespaces([]string{"tenant-a"}, true), "tenant-b", false},
		{"enforcing catch-all still serves all", sgWithNamespaces(nil, true), "tenant-z", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ServesNamespace(tc.sg, tc.namespace))
		})
	}
}
