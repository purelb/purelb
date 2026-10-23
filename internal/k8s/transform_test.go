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

package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSlimPod(t *testing.T) {
	in := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns", UID: "u", ResourceVersion: "7",
			Labels:          map[string]string{"app": "a"},
			Annotations:     map[string]string{"purelb.io/egress": "false", "big": "drop me"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "s"}},
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
		},
		Spec: corev1.PodSpec{NodeName: "n1", HostNetwork: true, Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}, {IP: "fd00::1"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady}}},
	}
	out, err := SlimPod("purelb.io/egress")(in)
	require.NoError(t, err)
	p := out.(*corev1.Pod)
	assert.Equal(t, map[string]string{"purelb.io/egress": "false"}, p.Annotations)
	assert.Equal(t, in.Labels, p.Labels)
	assert.Equal(t, in.OwnerReferences, p.OwnerReferences)
	assert.Equal(t, "7", p.ResourceVersion)
	assert.Equal(t, "n1", p.Spec.NodeName)
	assert.True(t, p.Spec.HostNetwork)
	assert.Equal(t, in.Status.PodIPs, p.Status.PodIPs)
	assert.Equal(t, corev1.PodRunning, p.Status.Phase)
	assert.Empty(t, p.ManagedFields)
	assert.Empty(t, p.Spec.Containers)
	assert.Empty(t, p.Status.Conditions)

	svc := &corev1.Service{}
	got, err := SlimPod()(svc)
	require.NoError(t, err)
	assert.Same(t, svc, got, "other objects pass through")
}
