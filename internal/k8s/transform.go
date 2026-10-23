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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// SlimPod returns an informer transform that keeps only what pod
// selection needs, since pods are by far the largest objects an informer
// here caches: name, namespace, UID, resource version, labels, owner
// references, deletion timestamp, the annotations named in keep,
// spec.nodeName, spec.hostNetwork, status.phase and status.podIPs.
// Everything else is dropped, managed fields included. Objects other than
// pods pass through untouched.
func SlimPod(keep ...string) cache.TransformFunc {
	return func(obj interface{}) (interface{}, error) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return obj, nil
		}
		var ann map[string]string
		for _, k := range keep {
			if v, ok := pod.Annotations[k]; ok {
				if ann == nil {
					ann = map[string]string{}
				}
				ann[k] = v
			}
		}
		return &corev1.Pod{
			TypeMeta: pod.TypeMeta,
			ObjectMeta: metav1.ObjectMeta{
				Name:              pod.Name,
				Namespace:         pod.Namespace,
				UID:               pod.UID,
				ResourceVersion:   pod.ResourceVersion,
				Labels:            pod.Labels,
				Annotations:       ann,
				OwnerReferences:   pod.OwnerReferences,
				DeletionTimestamp: pod.DeletionTimestamp,
			},
			Spec: corev1.PodSpec{
				NodeName:    pod.Spec.NodeName,
				HostNetwork: pod.Spec.HostNetwork,
			},
			Status: corev1.PodStatus{
				Phase:  pod.Status.Phase,
				PodIPs: pod.Status.PodIPs,
			},
		}, nil
	}
}
