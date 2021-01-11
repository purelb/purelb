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

const (
	mlLabels = "app=purelb,component=lbnodeagent"
)

func main() {
	logger := logging.Init()

	var (
		memberlistNS = flag.String("memberlist-ns", os.Getenv("PURELB_ML_NAMESPACE"), "memberlist namespace (only needed when running outside of k8s)")
		kubeconfig   = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host         = flag.String("host", os.Getenv("PURELB_HOST"), "HTTP host address for Prometheus metrics")
		myNode       = flag.String("node-name", os.Getenv("PURELB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		port         = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
	)
	flag.Parse()

	if *myNode == "" {
		logger.Log("op", "startup", "error", "must specify --node-name or PURELB_NODE_NAME", "msg", "missing configuration")
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	go func() {
		c1 := make(chan os.Signal)
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
		ProcessName:   "purelb-lbnodeagent",
		NodeName:      *myNode,
		Logger:        logger,
		Kubeconfig:    *kubeconfig,
		ReadEndpoints: true,

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

	election, err := election.New(&election.Config{
		Namespace: *memberlistNS,
		NodeName:  *myNode,
		BindAddr:  "0.0.0.0",
		BindPort:  7934,
		Secret:    []byte(os.Getenv("ML_GROUP")),
		Logger:    &logger,
		StopCh:    stopCh,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create election client")
		os.Exit(1)
	}

	ctrl.SetElection(&election)

	iplist, err := client.GetPodsIPs(*memberlistNS, mlLabels)
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to get PodsIPs")
		os.Exit(1)
	}
	err = election.Join(iplist, client)
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to join election")
		os.Exit(1)
	}

	go k8s.RunMetrics(*host, *port)

	// the k8s client doesn't return until it's time to shut down
	if err := client.Run(stopCh); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
