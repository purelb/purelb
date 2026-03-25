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
	"time"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
)

// parseDurationEnv parses a duration from an environment variable, returning
// the default if the env var is not set or cannot be parsed.
func parseDurationEnv(envVar string, defaultVal time.Duration) time.Duration {
	val := os.Getenv(envVar)
	if val == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return defaultVal
	}
	return d
}

func main() {
	logger := logging.Init()

	var (
		namespace  = flag.String("namespace", os.Getenv("PURELB_NAMESPACE"), "namespace for PureLB resources (from downward API)")
		kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host       = flag.String("host", os.Getenv("PURELB_HOST"), "HTTP host address for Prometheus metrics")
		myNode     = flag.String("node-name", os.Getenv("PURELB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		podUID     = flag.String("pod-uid", os.Getenv("PURELB_POD_UID"), "unique Pod UID for lease ownership (from downward API)")
		port       = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")

		// Lease configuration (optional, uses defaults if not set)
		leaseDuration = flag.Duration("lease-duration", parseDurationEnv("PURELB_LEASE_DURATION", election.DefaultLeaseDuration), "lease duration for leader election")
		renewDeadline = flag.Duration("renew-deadline", parseDurationEnv("PURELB_RENEW_DEADLINE", election.DefaultRenewDeadline), "renew deadline for lease renewal")
		retryPeriod   = flag.Duration("retry-period", parseDurationEnv("PURELB_RETRY_PERIOD", election.DefaultRetryPeriod), "retry period between renewal attempts")
	)
	flag.Parse()

	if *myNode == "" {
		logging.Info(logger, "op", "startup", "error", "must specify --node-name or PURELB_NODE_NAME", "msg", "missing configuration")
		os.Exit(1)
	}
	if *namespace == "" {
		logging.Info(logger, "op", "startup", "error", "must specify --namespace or PURELB_NAMESPACE", "msg", "missing configuration")
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	go func() {
		c1 := make(chan os.Signal, 1)
		signal.Notify(c1, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
		<-c1
		logging.Info(logger, "op", "shutdown", "msg", "signal received, initiating shutdown")
		signal.Stop(c1)
		close(stopCh)
	}()

	// Set up controller
	ctrl, err := NewController(
		logger,
		*myNode,
	)
	if err != nil {
		logging.Info(logger, "op", "startup", "error", err, "msg", "failed to create controller")
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
		// Note: Shutdown is handled explicitly in main() after client.Run() returns
		// to ensure proper ordering: mark unhealthy -> withdraw -> delete lease -> cleanup
	})
	if err != nil {
		logging.Info(logger, "op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	ctrl.SetClient(client)

	// Create the lease-based election
	elect, err := election.New(election.Config{
		Namespace:      *namespace,
		NodeName:       *myNode,
		InstanceID:     *podUID,
		Client:         client.Clientset(),
		LeaseDuration:  *leaseDuration,
		RenewDeadline:  *renewDeadline,
		RetryPeriod:    *retryPeriod,
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
		logging.Info(logger, "op", "startup", "error", err, "msg", "failed to create election")
		os.Exit(1)
	}

	ctrl.SetElection(elect)

	// Start the election (creates lease, starts informer)
	if err := elect.Start(); err != nil {
		logging.Info(logger, "op", "startup", "error", err, "msg", "failed to start election")
		os.Exit(1)
	}

	go k8s.RunMetrics(*host, *port)

	// the k8s client doesn't return until it's time to shut down
	if err := client.Run(stopCh); err != nil {
		logging.Info(logger, "op", "run", "error", err, "msg", "k8s client exited with error")
	}

	// Graceful shutdown sequence:
	// 1. Mark election unhealthy - Winner() returns "" for all queries
	// 2. Force sync to trigger address withdrawal on all services
	// 3. Wait for traffic to drain and GARP to propagate
	// 4. Stop lease renewals
	// 5. Delete our lease so other nodes see us gone
	// 6. Clean up local networking (dummy interface)
	logging.Info(logger, "op", "shutdown", "msg", "starting graceful shutdown sequence")

	// Step 1: Mark unhealthy - this causes Winner() to return ""
	elect.MarkUnhealthy()

	// Step 2: Force sync to trigger re-evaluation of all services
	// This will cause the announcer to withdraw addresses since Winner() now returns ""
	client.ForceSync()

	// Step 3: Wait for traffic to drain
	// This gives time for:
	// - Announcer to process the ForceSync and withdraw addresses
	// - GARP packets to propagate
	// - Upstream routers/switches to update their tables
	logging.Info(logger, "op", "shutdown", "msg", "waiting for traffic drain", "duration", "2s")
	time.Sleep(2 * time.Second)

	// Step 4: Stop lease renewals (no longer needed)
	elect.StopRenewals()

	// Step 5: Delete our lease so other nodes see us gone immediately
	if err := elect.DeleteOurLease(); err != nil {
		logging.Info(logger, "op", "shutdown", "error", err, "msg", "failed to delete lease")
	}

	// Step 6: Clean up local networking
	ctrl.Shutdown()

	logging.Info(logger, "op", "shutdown", "msg", "graceful shutdown complete")
}
