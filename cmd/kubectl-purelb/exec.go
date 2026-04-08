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
	"io"
	"os"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

func newGoBGPCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var node string

	cmd := &cobra.Command{
		Use:   "gobgp [args...]",
		Short: "Run gobgp CLI inside the k8gobgp sidecar",
		Long:  "Execute gobgp commands inside the k8gobgp container. Automatically connects via the unix socket.",
		Example: `  # Show BGP neighbors
  kubectl purelb gobgp neighbor

  # Show global RIB
  kubectl purelb gobgp global rib

  # Show adj-out for a specific peer
  kubectl purelb gobgp neighbor 192.168.151.1 adj-out

  # Target a specific node (multi-node clusters)
  kubectl purelb gobgp --node node-a neighbor`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parse --node flag manually since DisableFlagParsing is on
			filteredArgs, targetNode := extractNodeFlag(args)

			c, err := newClients(flags)
			if err != nil {
				return err
			}

			execCmd := append([]string{"gobgp", "-u", "/var/run/gobgp/gobgp.sock"}, filteredArgs...)
			return execInK8GoBGP(cmd.Context(), c, targetNode, execCmd)
		},
	}

	// Flag is documented in help but parsed manually due to DisableFlagParsing
	_ = node

	return cmd
}

func newIPCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var node string

	cmd := &cobra.Command{
		Use:   "ip [args...]",
		Short: "Run ip command inside the k8gobgp sidecar",
		Long:  "Execute ip commands inside the k8gobgp container for network debugging.",
		Example: `  # Show addresses on kube-lb0
  kubectl purelb ip addr show kube-lb0

  # Show BGP routes in kernel
  kubectl purelb ip route show proto bgp

  # Show all interfaces
  kubectl purelb ip link show

  # Target a specific node
  kubectl purelb ip --node node-a addr show kube-lb0`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			filteredArgs, targetNode := extractNodeFlag(args)

			c, err := newClients(flags)
			if err != nil {
				return err
			}

			execCmd := append([]string{"ip"}, filteredArgs...)
			return execInK8GoBGP(cmd.Context(), c, targetNode, execCmd)
		},
	}

	_ = node

	return cmd
}

// extractNodeFlag pulls --node <name> from args since DisableFlagParsing
// means cobra won't parse it for us.
func extractNodeFlag(args []string) (filtered []string, node string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--node" && i+1 < len(args) {
			node = args[i+1]
			i++ // skip value
		} else {
			filtered = append(filtered, args[i])
		}
	}
	return
}

// execInK8GoBGP runs a command in k8gobgp sidecar(s). If targetNode is empty,
// runs on all nodes and labels output. If targetNode is set, runs on that node only.
func execInK8GoBGP(ctx context.Context, c *clients, targetNode string, command []string) error {
	pods, err := c.core.CoreV1().Pods(purelbNamespace).List(ctx, metav1.ListOptions{
		ResourceVersion: "0",
		LabelSelector:   "app=purelb,component=lbnodeagent",
	})
	if err != nil {
		return fmt.Errorf("listing lbnodeagent pods: %w", err)
	}

	// Collect eligible pods
	var targets []v1.Pod
	for _, pod := range pods.Items {
		if pod.Status.Phase != v1.PodRunning {
			continue
		}
		hasK8GoBGP := false
		for _, container := range pod.Spec.Containers {
			if container.Name == "k8gobgp" {
				hasK8GoBGP = true
				break
			}
		}
		if !hasK8GoBGP {
			continue
		}
		if targetNode != "" && pod.Spec.NodeName != targetNode {
			continue
		}
		targets = append(targets, pod)
	}

	if len(targets) == 0 {
		if targetNode != "" {
			return fmt.Errorf("no k8gobgp sidecar found on node %s", targetNode)
		}
		return fmt.Errorf("no running lbnodeagent pod with k8gobgp sidecar found")
	}

	multiNode := len(targets) > 1

	for i, pod := range targets {
		if multiNode {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("=== %s ===\n", pod.Spec.NodeName)
		}

		if err := execOnePod(ctx, c, &pod, command); err != nil {
			fmt.Fprintf(os.Stderr, "Error on %s: %v\n", pod.Spec.NodeName, err)
		}
	}

	return nil
}

// execInK8GoBGPWithPods is like execInK8GoBGP but uses a pre-fetched pod list
// (e.g. from a clusterSnapshot) to avoid an extra Pods.List API call.
// Output is written to the provided writer instead of os.Stdout.
func execInK8GoBGPWithPods(ctx context.Context, c *clients, pods *v1.PodList, targetNode string, command []string, stdout, stderr io.Writer) error {
	if pods == nil {
		return fmt.Errorf("no pod list provided")
	}

	var targets []v1.Pod
	for _, pod := range pods.Items {
		if pod.Status.Phase != v1.PodRunning {
			continue
		}
		hasK8GoBGP := false
		for _, container := range pod.Spec.Containers {
			if container.Name == "k8gobgp" {
				hasK8GoBGP = true
				break
			}
		}
		if !hasK8GoBGP {
			continue
		}
		if targetNode != "" && pod.Spec.NodeName != targetNode {
			continue
		}
		targets = append(targets, pod)
	}

	if len(targets) == 0 {
		if targetNode != "" {
			return fmt.Errorf("no k8gobgp sidecar found on node %s", targetNode)
		}
		return fmt.Errorf("no running lbnodeagent pod with k8gobgp sidecar found")
	}

	multiNode := len(targets) > 1
	for i, pod := range targets {
		if multiNode {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintf(stdout, "=== %s ===\n", pod.Spec.NodeName)
		}
		if err := execOnePodToWriter(ctx, c, &pod, command, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "Error on %s: %v\n", pod.Spec.NodeName, err)
		}
	}
	return nil
}

// execOnePodToWriter executes a command in a pod, writing output to the
// provided writers instead of os.Stdout/os.Stderr.
func execOnePodToWriter(ctx context.Context, c *clients, pod *v1.Pod, command []string, stdout, stderr io.Writer) error {
	req := c.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Container: "k8gobgp",
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating exec: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: stderr,
	})
}

func execOnePod(ctx context.Context, c *clients, pod *v1.Pod, command []string) error {
	req := c.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Container: "k8gobgp",
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating exec: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
}
