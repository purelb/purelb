// Copyright 2017 Google Inc.
// Copyright 2020 Acnodal Inc.
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

package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
)

func main() {
	logger := logging.Init()

	var (
		namespace  = flag.String("namespace", os.Getenv("PURELB_NAMESPACE"), "namespace for PureLB resources (from downward API)")
		kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host       = flag.String("host", os.Getenv("PURELB_HOST"), "HTTP host address for Prometheus metrics")
		myNode     = flag.String("node-name", os.Getenv("PURELB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		port       = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
	)
	flag.Parse()

	if *myNode == "" {
		logger.Log("op", "startup", "error", "must specify --node-name or PURELB_NODE_NAME", "msg", "missing configuration")
		os.Exit(1)
	}
	if *namespace == "" {
		logger.Log("op", "startup", "error", "must specify --namespace or PURELB_NAMESPACE", "msg", "missing configuration")
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	go func() {
		c1 := make(chan os.Signal, 1)
		signal.Notify(c1, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
		<-c1
		logger.Log("op", "shutdown", "msg", "starting shutdown")
		signal.Stop(c1)
		close(stopCh)
	}()
	defer logger.Log("op", "shutdown", "msg", "done")

	// Set up controller
	ctrl, err := NewController(
		logger,
		*myNode,
	)
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create controller")
		os.Exit(1)
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:        "purelb-lbnodeagent",
		NodeName:           *myNode,
		Logger:             logger,
		Kubeconfig:         *kubeconfig,
		ReadEndpointSlices: true,

		ServiceChanged: ctrl.ServiceChanged,
		ServiceDeleted: ctrl.DeleteBalancer,
		ConfigChanged:  ctrl.SetConfig,
		Shutdown:       ctrl.Shutdown,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	ctrl.SetClient(client)

	// Create the lease-based election
	elect, err := election.New(election.Config{
		Namespace:      *namespace,
		NodeName:       *myNode,
		Client:         client.Clientset(),
		Logger:         logger,
		StopCh:         stopCh,
		OnMemberChange: client.ForceSync,
		GetLocalSubnets: func() ([]string, error) {
			// TODO: This will be populated from LBNodeAgent config in Milestone 4
			// For now, use default interface detection
			return election.GetLocalSubnets([]string{}, true, logger)
		},
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create election")
		os.Exit(1)
	}

	ctrl.SetElection(elect)

	// Start the election (creates lease, starts informer)
	if err := elect.Start(); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to start election")
		os.Exit(1)
	}

	go k8s.RunMetrics(*host, *port)

	// the k8s client doesn't return until it's time to shut down
	if err := client.Run(stopCh); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
