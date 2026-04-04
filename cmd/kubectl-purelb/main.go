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
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// Set via ldflags at build time.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	flags := genericclioptions.NewConfigFlags(true)

	root := &cobra.Command{
		Use:   "kubectl-purelb",
		Short: "Operational visibility for PureLB LoadBalancer",
		Long:  "kubectl-purelb provides consolidated views of PureLB pool utilization, service announcements, election state, and BGP data plane health.",
		SilenceUsage: true,
	}

	// Bind kubeconfig / context / namespace flags to all commands
	flags.AddFlags(root.PersistentFlags())

	root.AddCommand(
		newStatusCmd(flags),
		newPoolsCmd(flags),
		newServicesCmd(flags),
		newElectionCmd(flags),
		newBGPCmd(flags),
		newInspectCmd(flags),
		newValidateCmd(flags),
		newVersionCmd(flags),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
