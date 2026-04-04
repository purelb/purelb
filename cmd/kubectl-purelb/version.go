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

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type versionInfo struct {
	Plugin      string           `json:"plugin"`
	Allocator   componentVersion `json:"allocator"`
	LBNodeAgent componentVersion `json:"lbnodeagent"`
	K8GoBGP     componentVersion `json:"k8gobgp,omitempty"`
	CRDs        []string         `json:"crds"`
	Consistent  bool             `json:"consistent"`
}

type componentVersion struct {
	Image   string `json:"image"`
	Pods    int    `json:"pods"`
	Running int    `json:"running"`
}

func newVersionCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show plugin and PureLB component versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runVersion(cmd.Context(), c, format)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")

	return cmd
}

func runVersion(ctx context.Context, c *clients, format outputFormat) error {
	info := versionInfo{
		Plugin: fmt.Sprintf("%s (commit %s)", version, commit),
	}

	// Fetch pods
	pods, _ := c.core.CoreV1().Pods(purelbNamespace).List(ctx, metav1.ListOptions{})
	if pods != nil {
		for _, pod := range pods.Items {
			if pod.Labels["component"] == "allocator" {
				for _, container := range pod.Spec.Containers {
					if container.Name == "allocator" {
						info.Allocator.Image = container.Image
					}
				}
				info.Allocator.Pods++
				if pod.Status.Phase == v1.PodRunning {
					info.Allocator.Running++
				}
			}
			if pod.Labels["component"] == "lbnodeagent" {
				for _, container := range pod.Spec.Containers {
					if container.Name == "lbnodeagent" {
						info.LBNodeAgent.Image = container.Image
						info.LBNodeAgent.Pods++
						if isPodContainerRunning(pod, "lbnodeagent") {
							info.LBNodeAgent.Running++
						}
					}
					if container.Name == "k8gobgp" {
						info.K8GoBGP.Image = container.Image
						info.K8GoBGP.Pods++
						if isPodContainerRunning(pod, "k8gobgp") {
							info.K8GoBGP.Running++
						}
					}
				}
			}
		}
	}

	// Check CRDs
	crdList, _ := c.core.Discovery().ServerResourcesForGroupVersion("purelb.io/v2")
	if crdList != nil {
		for _, r := range crdList.APIResources {
			info.CRDs = append(info.CRDs, fmt.Sprintf("purelb.io/v2 %s", r.Kind))
		}
	}
	bgpCRDList, _ := c.core.Discovery().ServerResourcesForGroupVersion("bgp.purelb.io/v1")
	if bgpCRDList != nil {
		for _, r := range bgpCRDList.APIResources {
			info.CRDs = append(info.CRDs, fmt.Sprintf("bgp.purelb.io/v1 %s", r.Kind))
		}
	}

	// Version consistency check
	info.Consistent = true
	if info.Allocator.Running < info.Allocator.Pods {
		info.Consistent = false
	}
	if info.LBNodeAgent.Running < info.LBNodeAgent.Pods {
		info.Consistent = false
	}
	if info.K8GoBGP.Pods > 0 && info.K8GoBGP.Running < info.K8GoBGP.Pods {
		info.Consistent = false
	}

	if format != outputTable {
		return printStructured(format, info)
	}

	fmt.Printf("Plugin:      %s\n", info.Plugin)
	fmt.Printf("Allocator:   %s (%d pod(s), %d Running)\n", info.Allocator.Image, info.Allocator.Pods, info.Allocator.Running)
	fmt.Printf("LBNodeAgent: %s (%d pod(s), %d Running)\n", info.LBNodeAgent.Image, info.LBNodeAgent.Pods, info.LBNodeAgent.Running)
	if info.K8GoBGP.Pods > 0 {
		fmt.Printf("k8gobgp:     %s (%d sidecar(s), %d Running)\n", info.K8GoBGP.Image, info.K8GoBGP.Pods, info.K8GoBGP.Running)
	}
	if len(info.CRDs) > 0 {
		fmt.Printf("CRDs:        %s\n", joinCRDs(info.CRDs))
	}
	fmt.Println()
	if info.Consistent {
		fmt.Println("Version Consistency: OK")
	} else {
		fmt.Println("Version Consistency: DEGRADED (not all pods running)")
	}

	return nil
}

func isPodContainerRunning(pod v1.Pod, containerName string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName && cs.Ready {
			return true
		}
	}
	return false
}

func joinCRDs(crds []string) string {
	if len(crds) <= 3 {
		return fmt.Sprintf("%s", crds)
	}
	return fmt.Sprintf("%d resources across purelb.io/v2 and bgp.purelb.io/v1", len(crds))
}
