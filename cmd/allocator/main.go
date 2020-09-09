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

	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	"purelb.io/internal/allocator"
)

func main() {
	logger := logging.Init()

	var (
		port       = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
		kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file (only needed when running outside of k8s)")
	)
	flag.Parse()

	c, _ := allocator.NewController(logger, allocator.New())

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "purelb-allocator",
		Logger:        logger,
		Kubeconfig:    *kubeconfig,

		ServiceChanged: c.SetBalancer,
		ConfigChanged:  c.SetConfig,
		Synced:         c.MarkSynced,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	c.SetClient(client)

	go k8s.RunMetrics("", *port)

	if err := client.Run(nil); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
