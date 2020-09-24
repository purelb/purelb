// Copyright 2017 Google Inc.
// Copyright 2020 Acnodal Inc.
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

	"purelb.io/internal/allocator"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
)

func main() {
	logger := logging.Init()

	var (
		port       = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
		kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file (only needed when running outside of k8s)")
	)
	flag.Parse()

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
	c, _ := allocator.NewController(logger, allocator.New())

	client, err := k8s.New(&k8s.Config{
		ProcessName: "purelb-allocator",
		Logger:      logger,
		Kubeconfig:  *kubeconfig,

		ServiceChanged: c.SetBalancer,
		ServiceDeleted: c.DeleteBalancer,
		ConfigChanged:  c.SetConfig,
		Synced:         c.MarkSynced,
		Shutdown:       c.Shutdown,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	c.SetClient(client)

	go k8s.RunMetrics("", *port)

	// the k8s client doesn't return until it's time to shut down
	if err := client.Run(stopCh); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
