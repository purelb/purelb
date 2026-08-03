// Copyright 2017 Google Inc.
// Copyright 2020-2026 Acnodal Inc.
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
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// selectorState reports which interface-selector state this node is
// in: 1 for the active state, 0 for the others. The four states are
// otherwise indistinguishable from the outside (config_loaded_bool
// keeps reporting the previous good config during an invalid-config
// outage).
var selectorState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: purelbv2.MetricsNamespace,
	Subsystem: "lbnodeagent",
	Name:      "selector_state",
	Help:      "Interface selector state (1 = active): default, configured, deselected, invalid",
}, []string{"state"})

func init() {
	prometheus.MustRegister(selectorState)
}

func recordSelectorState(active string) {
	for _, state := range []string{"default", "configured", "deselected", "invalid"} {
		value := 0.0
		if state == active {
			value = 1.0
		}
		selectorState.WithLabelValues(state).Set(value)
	}
}

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

	// selector bridges config delivery (CR-controller goroutine, via
	// configChanged below) to the election's renewLoop goroutine, which
	// reads it through the GetLocalSubnets closure. It is the only
	// shared state in this file; everything else is per-delivery locals.
	// nil means "no config yet / remote-only": the election falls back
	// to default-interface detection, today's behavior. An empty
	// selector means "advertise nothing".
	var selector atomic.Pointer[election.InterfaceSelector]

	// client is captured by configChanged before it is assigned. That is
	// safe because the k8s client only invokes callbacks from inside
	// client.Run(), which happens-after the assignment below.
	var client *k8s.Client

	// configChanged wraps ctrl.SetConfig with nodeSelector evaluation
	// and keeps the election selector coherent with the announcer: the
	// lease must never advertise a subnet the announcer cannot announce
	// on. Convergence after a config change is two-wave: wave 1
	// reprocesses services against the stale lease subnets; wave 2
	// follows the next lease renewal (≤2.5s) plus informer propagation.
	configChanged := func(cfg *purelbv2.Config) k8s.SyncState {
		// Fetch our Node's labels fresh per delivery — no stored label
		// state. Like a Pod's nodeSelector, label changes take effect
		// when config is next delivered (CR event, informer resync, or
		// pod restart), not continuously.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		node, err := client.Clientset().CoreV1().Nodes().Get(ctx, *myNode, metav1.GetOptions{})
		cancel()
		if err != nil {
			// SyncStateError makes the CR controller requeue this
			// delivery with backoff; config is not applied.
			logging.Info(logger, "op", "configChanged", "error", err,
				"msg", "failed to get node for nodeSelector evaluation, delivery will be retried")
			return k8s.SyncStateError
		}

		for _, agent := range cfg.Agents {
			logging.Debug(logger, "op", "configChanged", "agent", agent.Namespace+"/"+agent.Name,
				"hasNodeSelector", fmt.Sprintf("%t", agent.Spec.NodeSelector != nil))
		}

		preFilter := len(cfg.Agents)
		cfg.Agents = purelbv2.AgentsForNode(cfg.Agents, node.Labels)
		if len(cfg.Agents) > 1 {
			winner := cfg.Agents[0]
			for _, ignored := range cfg.Agents[1:] {
				logging.Info(logger, "op", "configChanged", "msg", "multiple LBNodeAgents match this node, using highest precedence",
					"node", *myNode, "using", winner.Namespace+"/"+winner.Name, "ignoring", ignored.Namespace+"/"+ignored.Name)
				client.Errorf(ignored, "ConfigIgnored",
					"LBNodeAgent %s/%s takes precedence on node %s", winner.Namespace, winner.Name, *myNode)
			}
		}

		// Announcer first: the selector is stored only after we know
		// whether the announcer accepted the config, so the lease can
		// never advertise what the announcer failed to configure.
		ret := ctrl.SetConfig(cfg)

		state := "default"
		switch {
		case ret == k8s.SyncStateError:
			// Invalid regex, dummy-interface failure, any announcer
			// config failure: the announcer announces nothing, so the
			// lease must advertise nothing.
			selector.Store(&election.InterfaceSelector{})
			state = "invalid"
			if offending := purelbv2.FirstLocalAgent(cfg.Agents); offending != nil {
				client.Errorf(offending, "ConfigError",
					"invalid configuration: node %s is announcing nothing until this is fixed", *myNode)
			}
		case preFilter > 0 && len(cfg.Agents) == 0:
			// Every CR's nodeSelector deselected this node: the
			// announcer is unconfigured, so advertise nothing. (A
			// default fallback here would win elections it cannot
			// serve — a blackhole.)
			selector.Store(&election.InterfaceSelector{})
			state = "deselected"
		default:
			sel, selErr := election.SelectorFromConfig(cfg)
			if selErr != nil {
				// Cannot happen when SetConfig succeeded (same regex,
				// same agent), but stay coherent if it ever does.
				selector.Store(&election.InterfaceSelector{})
				state = "invalid"
			} else {
				// sel is nil for remote-only clusters (no Local spec):
				// keep the default-detection lease, today's behavior.
				selector.Store(sel)
				if sel != nil {
					state = "configured"
				}
			}
		}
		recordSelectorState(state)
		logging.Info(logger, "op", "configChanged", "selectorState", state,
			"node", *myNode, "matchingAgents", fmt.Sprintf("%d", len(cfg.Agents)),
			"msg", "config delivery evaluated")

		return ret
	}

	client, err = k8s.New(&k8s.Config{
		ProcessName:        "purelb-lbnodeagent",
		NodeName:           *myNode,
		Logger:             logger,
		Kubeconfig:         *kubeconfig,
		ReadEndpointSlices: true,

		ServiceChanged: ctrl.ServiceChanged,
		ServiceDeleted: ctrl.DeleteBalancer,
		ConfigChanged:  configChanged,
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
			// Runs on the election's renewLoop goroutine every
			// LeaseDuration/2. Before the first config delivery the
			// selector is nil and this is default-interface detection,
			// identical to the historical behavior.
			return election.GetSelectedSubnets(selector.Load(), logger)
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
