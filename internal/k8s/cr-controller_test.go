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

package k8s

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func newServiceGroup(name string) *purelbv2.ServiceGroup {
	return &purelbv2.ServiceGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: purelbv2.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceDefault,
		},
	}
}

// TestSGUpdateNeedsReconcile covers the Generation-aware UpdateFunc
// filter that prevents the allocator's own status writes (which don't
// bump .metadata.generation) from triggering full reconcile cycles over
// every service.
func TestSGUpdateNeedsReconcile(t *testing.T) {
	withGen := func(name string, gen int64) *purelbv2.ServiceGroup {
		sg := newServiceGroup(name)
		sg.Generation = gen
		return sg
	}

	cases := []struct {
		name     string
		old, new interface{}
		want     bool
	}{
		{
			name: "same generation (status-only update) -> skip",
			old:  withGen("sg", 3), new: withGen("sg", 3),
			want: false,
		},
		{
			name: "generation bumped (spec change) -> reconcile",
			old:  withGen("sg", 3), new: withGen("sg", 4),
			want: true,
		},
		{
			name: "old not a ServiceGroup -> reconcile (fail safe)",
			old:  "not-an-sg", new: withGen("sg", 3),
			want: true,
		},
		{
			name: "new not a ServiceGroup -> reconcile (fail safe)",
			old:  withGen("sg", 3), new: nil,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sgUpdateNeedsReconcile(tc.old, tc.new); got != tc.want {
				t.Errorf("sgUpdateNeedsReconcile = %v, want %v", got, tc.want)
			}
		})
	}
}
