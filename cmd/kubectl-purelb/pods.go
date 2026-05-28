// Copyright 2026 Acnodal Inc.
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

package main

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// purelbPods groups pods in the purelb-system namespace by which PureLB
// component they implement. Identification is by container name, not by
// label, so it works for both install methods: manifest installs label pods
// `component=lbnodeagent`/`component=allocator` while Helm installs use
// `app.kubernetes.io/component=…`. Container names are baked into both
// pod templates and stay the same across install methods.
type purelbPods struct {
	allocator   []v1.Pod // pods with a container named "allocator"
	lbnodeagent []v1.Pod // pods with a container named "lbnodeagent"
	withK8GoBGP []v1.Pod // subset of lbnodeagent that also have a "k8gobgp" sidecar
}

// categorizePureLBPods scans a PodList and groups pods by container composition.
// A nil or empty input returns the zero value (all slices nil).
func categorizePureLBPods(pods *v1.PodList) purelbPods {
	var out purelbPods
	if pods == nil {
		return out
	}
	for _, pod := range pods.Items {
		var hasAllocator, hasLBNodeAgent, hasK8GoBGP bool
		for _, c := range pod.Spec.Containers {
			switch c.Name {
			case "allocator":
				hasAllocator = true
			case "lbnodeagent":
				hasLBNodeAgent = true
			case "k8gobgp":
				hasK8GoBGP = true
			}
		}
		if hasAllocator {
			out.allocator = append(out.allocator, pod)
		}
		if hasLBNodeAgent {
			out.lbnodeagent = append(out.lbnodeagent, pod)
			if hasK8GoBGP {
				out.withK8GoBGP = append(out.withK8GoBGP, pod)
			}
		}
	}
	return out
}

// listAndCategorizePureLBPods lists all pods in the purelb-system namespace
// and categorizes them. Used by commands that don't already have a snapshot
// to share with other rendering code.
func listAndCategorizePureLBPods(ctx context.Context, c *clients) (purelbPods, error) {
	pods, err := c.core.CoreV1().Pods(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return purelbPods{}, fmt.Errorf("listing pods in %s: %w", purelbNamespace, err)
	}
	return categorizePureLBPods(pods), nil
}
