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

	"purelb.io/internal/acnodal"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	"purelb.io/internal/node"

	"github.com/prometheus/client_golang/prometheus"
)

var announcing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "purelb",
	Subsystem: "node_acnodal",
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
		kubeconfig  = flag.String("kubeconfig", "", "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host        = flag.String("host", os.Getenv("PURELB_HOST"), "HTTP host address for Prometheus metrics")
		myNode      = flag.String("node-name", os.Getenv("PURELB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		port        = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
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
		acnodal.NewAnnouncer(logger, *myNode),
	)
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create controller")
		os.Exit(1)
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "purelb-node",
		NodeName:      *myNode,
		Logger:        logger,
		Kubeconfig:    *kubeconfig,
		ReadEndpoints: true,

		ServiceChanged: ctrl.ServiceChanged,
		ConfigChanged:  ctrl.SetConfig,
		NodeChanged:    ctrl.SetNode,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	go k8s.RunMetrics(*host, *port)

	if err := client.Run(stopCh); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
