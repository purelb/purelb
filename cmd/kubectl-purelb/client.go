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

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// GVRs for PureLB and BGP CRDs (accessed via dynamic client to avoid
// importing purelb.io/pkg/apis/purelb/v2 which has Linux-only netlink deps).
var (
	gvrServiceGroups = schema.GroupVersionResource{
		Group:    "purelb.io",
		Version:  "v2",
		Resource: "servicegroups",
	}
	gvrLBNodeAgents = schema.GroupVersionResource{
		Group:    "purelb.io",
		Version:  "v2",
		Resource: "lbnodeagents",
	}
	gvrBGPConfigurations = schema.GroupVersionResource{
		Group:    "bgp.purelb.io",
		Version:  "v1",
		Resource: "configs",
	}
	gvrBGPNodeStatuses = schema.GroupVersionResource{
		Group:    "bgp.purelb.io",
		Version:  "v1",
		Resource: "bgpnodestatuses",
	}
)

// clients holds all the K8s clients the plugin needs.
type clients struct {
	// core provides access to core K8s resources (Services, Pods, Nodes, Leases, Events, EndpointSlices).
	core kubernetes.Interface

	// dynamic provides access to all CRDs (ServiceGroups, LBNodeAgents, BGPConfiguration, BGPNodeStatus).
	dynamic dynamic.Interface

	// config is the raw REST config, available if needed.
	config *rest.Config

	// namespace is the resolved namespace (from --namespace flag or kubeconfig context).
	namespace string
}

// newClients builds all clients from the genericclioptions config flags.
func newClients(flags *genericclioptions.ConfigFlags) (*clients, error) {
	config, err := flags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("building REST config: %w", err)
	}

	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating core client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	ns, _, err := flags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return nil, fmt.Errorf("resolving namespace: %w", err)
	}

	return &clients{
		core:      coreClient,
		dynamic:   dynClient,
		config:    config,
		namespace: ns,
	}, nil
}
