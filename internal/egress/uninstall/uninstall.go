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

// Package uninstall is the Helm pre-delete hook's subcommand (design
// 8.10): it disables egress on every node and waits for each to report its
// teardown complete (D26), so uninstalling never leaves a node with the
// CNI plugin chained and nothing to answer it.
package uninstall

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/go-kit/log"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"purelb.io/internal/logging"
	"purelb.io/pkg/generated/clientset/versioned"
)

// Command is the subcommand the hook runs.
const Command = "egress-uninstall"

// disablePatch turns egress off in an LBNodeAgent.
var disablePatch = []byte(`{"spec":{"egress":{"enabled":false}}}`)

// Main runs the hook; it exits non-zero, failing the uninstall, when a
// node is still programmed at the timeout (user decision 2026-11-03).
func Main(args []string) {
	logger := logging.Init()
	fs := flag.NewFlagSet(Command, flag.ExitOnError)
	timeout := fs.Duration("timeout", 4*time.Minute, "how long to wait for every node to tear egress down")
	_ = fs.Parse(args)
	cfg, err := rest.InClusterConfig()
	if err == nil {
		var purelb versioned.Interface
		if purelb, err = versioned.NewForConfig(cfg); err == nil {
			err = Run(context.Background(), logger, purelb, *timeout, 2*time.Second)
		}
	}
	if err != nil {
		logging.Info(logger, "op", "egressUninstall", "error", err)
		os.Exit(1)
	}
}

// Run disables egress on every LBNodeAgent, then waits, up to timeout,
// polling every poll, until no EgressNodeState reports egress programmed.
func Run(ctx context.Context, logger log.Logger, purelb versioned.Interface, timeout, poll time.Duration) error {
	agents, err := purelb.PurelbV2().LBNodeAgents(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing the LBNodeAgents: %w", err)
	}
	for _, a := range agents.Items {
		if a.Spec.Egress == nil || !a.Spec.Egress.Enabled {
			continue
		}
		if _, err := purelb.PurelbV2().LBNodeAgents(a.Namespace).Patch(ctx, a.Name, types.MergePatchType, disablePatch,
			metav1.PatchOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("disabling egress in LBNodeAgent %s/%s: %w", a.Namespace, a.Name, err)
		}
		logging.Info(logger, "op", "egressUninstall", "lbnodeagent", a.Namespace+"/"+a.Name, "msg", "egress disabled")
	}
	deadline := time.Now().Add(timeout)
	var waiting, logged []string
	for {
		states, err := purelb.PurelbV2().EgressNodeStates().List(ctx, metav1.ListOptions{})
		switch {
		case k8serrors.IsNotFound(err):
			return nil // no egress CRDs: nothing was ever programmed
		case err != nil:
			logging.Info(logger, "op", "egressUninstall", "error", err, "msg", "listing the nodes' egress state; retrying")
		default:
			waiting = waiting[:0]
			for _, s := range states.Items {
				if s.Status.Programmed != "" {
					waiting = append(waiting, s.Name)
				}
			}
			slices.Sort(waiting)
			if len(waiting) == 0 {
				logging.Info(logger, "op", "egressUninstall", "msg", "every node has torn egress down")
				return nil
			}
			if !slices.Equal(waiting, logged) {
				logging.Info(logger, "op", "egressUninstall", "nodes", waiting, "msg", "waiting for these nodes to tear egress down")
				logged = slices.Clone(waiting)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s these nodes still have egress programmed: %v. Their agents must run to remove the CNI plugin "+
				"from the chain and egress's kernel state: bring them up and uninstall again, or uninstall with --no-hooks and clean "+
				"those nodes up by hand", timeout, waiting)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
