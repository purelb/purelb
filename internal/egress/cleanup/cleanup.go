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

// Package cleanup is `lbnodeagent egress-cleanup`, the break-glass removal
// of egress from a node whose agent can't do it (D26), run by
// `kubectl purelb egress cleanup` in a privileged pod on that node.
package cleanup

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/agent"
	"purelb.io/internal/egress/cniinstall"
	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/logging"
)

// Command is the lbnodeagent subcommand.
const Command = "egress-cleanup"

// Main removes everything egress put on this node, in D26's order: the
// chain entry, the plugin binary, the kernel state, the plugin's files.
func Main(args []string) {
	logger := logging.Init()
	fs := flag.NewFlagSet(Command, flag.ExitOnError)
	confDir := fs.String("conf-dir", "/host/cni/net.d", "the node's CNI configuration directory, as mounted")
	binDir := fs.String("bin-dir", "/host/cni/bin", "the node's CNI binary directory, as mounted")
	node := fs.String("node", os.Getenv("PURELB_NODE_NAME"), "this node's name")
	_ = fs.Parse(args)

	var errs []error
	// First, so a purelb-cni still running has nothing to put back: with no
	// desired state it leaves the chain alone.
	if err := os.Remove(cnisock.DesiredPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := cniinstall.RemovePlugin(logger, *confDir, *binDir); err != nil {
		errs = append(errs, fmt.Errorf("removing the CNI plugin: %w", err))
	}
	if err := agent.Cleanup(logger, *node, bpfpin.Dir); err != nil {
		errs = append(errs, fmt.Errorf("removing the datapath: %w", err))
	}
	for _, path := range []string{cnisock.SnapshotPath, cnisock.StatusPath, cnisock.RecordDir} {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		logging.Info(logger, "op", "egressCleanup", "node", *node, "error", err)
		os.Exit(1)
	}
	logging.Info(logger, "op", "egressCleanup", "node", *node, "msg", "egress removed from this node")
}
