// Copyright 2026 Acnodal Inc.
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

package egress

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func TestPodExcluded(t *testing.T) {
	yes := true
	sts := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
		{Kind: "StatefulSet", Name: "db", Controller: &yes}}}}
	host := &corev1.Pod{Spec: corev1.PodSpec{HostNetwork: true}}
	out := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{purelbv2.EgressOptOutAnnotation: "false"}}}

	assert.Equal(t, "", PodExcluded(sts, "Allowed", true))
	assert.Equal(t, ExcludedHostNetwork, PodExcluded(host, "Allowed", false))
	assert.Equal(t, ExcludedOptedOut, PodExcluded(out, "Allowed", false))
	assert.Equal(t, "", PodExcluded(out, "Denied", false), "Denied ignores the annotation")
	assert.Equal(t, ExcludedNotSTS, PodExcluded(&corev1.Pod{}, "", true))
	assert.Equal(t, "db", StatefulSetOf(sts))
}

func TestNamespaceInScope(t *testing.T) {
	sg := &purelbv2.ServiceGroup{ObjectMeta: metav1.ObjectMeta{Name: "pci"}, Spec: purelbv2.ServiceGroupSpec{Namespaces: []string{"pay"}}}
	b := purelbv2.BindNamespaces([]*purelbv2.ServiceGroup{sg})
	ns := func(name string, admitted bool) *corev1.Namespace {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if admitted {
			n.Labels = map[string]string{AdmittedLabel: "true"}
		}
		return n
	}
	assert.True(t, NamespaceInScope(ns("pay", false), false, "pci", sg, b))
	assert.False(t, NamespaceInScope(ns("pay", false), true, "pci", sg, b), "not admitted")
	assert.True(t, NamespaceInScope(ns("pay", true), true, "pci", sg, b))
	assert.False(t, NamespaceInScope(ns("web", true), false, "pci", sg, b), "not one of the group's")
}

func TestEffectivePriority(t *testing.T) {
	ceiling := int32(500)
	sg := &purelbv2.ServiceGroup{Spec: purelbv2.ServiceGroupSpec{Egress: &purelbv2.ServiceGroupEgressSpec{
		Tenancy: &purelbv2.EgressTenancy{PriorityCeiling: &ceiling}}}}
	p, clamped := EffectivePriority(900, false, sg)
	assert.Equal(t, int32(500), p)
	assert.True(t, clamped)
	p, clamped = EffectivePriority(100, false, sg)
	assert.Equal(t, int32(100), p)
	assert.False(t, clamped)
	p, _ = EffectivePriority(2000, true, sg)
	assert.Equal(t, int32(2000), p, "a ClusterEgress isn't clamped")
	p, _ = EffectivePriority(900, false, nil)
	assert.Equal(t, int32(900), p)
}
