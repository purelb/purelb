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

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// TestInstallNamespace_FlagWins: an explicit flag or PURELB_NAMESPACE takes
// precedence over the ServiceAccount projection, so an operator can override
// a wrong or absent projection without rebuilding anything.
func TestInstallNamespace_FlagWins(t *testing.T) {
	assert.Equal(t, "purelb-system", InstallNamespace(log.NewNopLogger(), "purelb-system"))
}

// TestInstallNamespace_TrimsWhitespace: the projected file carries no
// trailing-newline guarantee, and an untrimmed value would compare unequal to
// every ServiceGroup namespace -- which, once the value is used for scoping,
// silently matches nothing.
func TestInstallNamespace_TrimsWhitespace(t *testing.T) {
	assert.Equal(t, "purelb-system", InstallNamespace(log.NewNopLogger(), "  purelb-system\n"))
}

// TestInstallNamespace_EmptyIsNotFatal: with no flag and no projection the
// result is empty and the process continues. Callers must degrade rather than
// refuse to start -- guessing wrong here is worse than admitting ignorance,
// and refusing to start turns a packaging gap into an outage.
func TestInstallNamespace_EmptyIsNotFatal(t *testing.T) {
	// The SA path does not exist outside a cluster, so this exercises the
	// fallback failure branch.
	assert.Equal(t, "", InstallNamespace(log.NewNopLogger(), "   "))
}

func sg(namespace, name string) *purelbv2.ServiceGroup {
	return &purelbv2.ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
}

func names(groups []*purelbv2.ServiceGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Namespace+"/"+g.Name)
	}
	return out
}

// TestScopeToInstallNamespace_KeepsOnlyInstallNamespace is the ordinary case:
// ServiceGroups elsewhere are ignored and reported, not silently discarded.
func TestScopeToInstallNamespace_KeepsOnlyInstallNamespace(t *testing.T) {
	c := &Controller{logger: log.NewNopLogger(), installNamespace: "purelb-system"}
	kept, dropped := c.scopeToInstallNamespace([]*purelbv2.ServiceGroup{
		sg("purelb-system", "default"),
		sg("tenant-a", "stray"),
		sg("purelb-system", "tenant-a-pool"),
	})
	assert.Equal(t, []string{"purelb-system/default", "purelb-system/tenant-a-pool"}, names(kept))
	assert.Equal(t, []string{"tenant-a/stray"}, names(dropped),
		"out-of-scope groups must be carried for reporting, not dropped on the floor")
}

// TestScopeToInstallNamespace_UnknownNamespaceFiltersNothing: with no install
// namespace, filtering would discard every ServiceGroup and every Service
// would then fail `unknown pool "default"` while the allocator reported
// healthy. Honouring a misplaced group is the lesser failure.
func TestScopeToInstallNamespace_UnknownNamespaceFiltersNothing(t *testing.T) {
	c := &Controller{logger: log.NewNopLogger(), installNamespace: ""}
	in := []*purelbv2.ServiceGroup{sg("tenant-a", "one"), sg("tenant-b", "two")}
	kept, dropped := c.scopeToInstallNamespace(in)
	assert.Equal(t, names(in), names(kept), "must fail open when the namespace is unknown")
	assert.Empty(t, dropped)
}

// TestScopeToInstallNamespace_RefusesToDropEverything: nobody installs PureLB
// and then puts all of their ServiceGroups somewhere else, so dropping 100%
// means the configured namespace is wrong, not the cluster.
func TestScopeToInstallNamespace_RefusesToDropEverything(t *testing.T) {
	c := &Controller{logger: log.NewNopLogger(), installNamespace: "typo-system"}
	in := []*purelbv2.ServiceGroup{sg("purelb-system", "default"), sg("purelb-system", "other")}
	kept, dropped := c.scopeToInstallNamespace(in)
	assert.Equal(t, names(in), names(kept), "must keep everything rather than empty the pool table")
	assert.Empty(t, dropped)
}

// TestScopeToInstallNamespace_NoGroupsIsNotSuspect: an empty cluster is a
// legitimate state and must not trip the refuse-to-drop-everything branch.
func TestScopeToInstallNamespace_NoGroupsIsNotSuspect(t *testing.T) {
	c := &Controller{logger: log.NewNopLogger(), installNamespace: "purelb-system"}
	kept, dropped := c.scopeToInstallNamespace(nil)
	assert.Empty(t, kept)
	assert.Empty(t, dropped)
}
