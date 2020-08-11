// Copyright 2017 Google Inc.
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
	"purelb.io/internal/local"
	"purelb.io/internal/logging"
	"purelb.io/internal/node"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	mlLabels = "app=purelb,component=node"
)

var announcing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "purelb",
	Subsystem: "node_local",
	Name:      "announced",
	Help:      "Services being announced from this node. This is desired state, it does not guarantee that the routing protocols have converged.",
}, []string{
	"service",
	"node",
	"ip",
})

func main() {
	prometheus.MustRegister(announcing)

	logger := logging.Init()

	var (
		config      = flag.String("config", "config", "Kubernetes ConfigMap containing configuration")
		configNS    = flag.String("config-ns", os.Getenv("PURELB_ML_NAMESPACE"), "config file namespace (only needed when running outside of k8s)")
		kubeconfig  = flag.String("kubeconfig", "", "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host        = flag.String("host", os.Getenv("PURELB_HOST"), "HTTP host address")
		myNode      = flag.String("node-name", os.Getenv("PURELB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		port        = flag.Int("port", 80, "HTTP listening port")
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
	ctrl, err := node.NewController(
		*myNode,
		announcing,
		local.NewAnnouncer(logger, *myNode),
	)
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create controller")
		os.Exit(1)
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "purelb-node",
		ConfigMapName: *config,
		ConfigMapNS:   *configNS,
		NodeName:      *myNode,
		Logger:        logger,
		Kubeconfig:    *kubeconfig,

		MetricsHost:   *host,
		MetricsPort:   *port,
		ReadEndpoints: true,

		ServiceChanged: ctrl.ServiceChanged,
		ConfigChanged:  ctrl.SetConfig,
		NodeChanged:    ctrl.SetNode,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	election, err := election.New(&election.Config{
		Namespace: *configNS,
		NodeName: *myNode,
		Labels: mlLabels,
		BindAddr: "0.0.0.0",
		BindPort: 7946,
		Secret: []byte(os.Getenv("ML_SECRET")),
		Logger: &logger,
		StopCh: stopCh,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create election client")
		os.Exit(1)
	}

	ctrl.Election = &election

	iplist, err := client.GetPodsIPs(*configNS, mlLabels)
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to get PodsIPs")
		os.Exit(1)
	}
	err = election.Join(iplist, client)
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to join election")
		os.Exit(1)
	}

	// the k8s client doesn't return until it's time to shut down
	if err := client.Run(stopCh); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
