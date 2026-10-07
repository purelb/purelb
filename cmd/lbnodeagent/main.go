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

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"purelb.io/internal/addrguard"
	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
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
	Help:      "Interface selector state (1 = active): default, configured, deselected, invalid, guardUnavailable",
}, []string{"state"})

// selectorStates are the values selector_state reports. guardUnavailable
// means the address guard is configured fail-closed and isn't working, so
// the node announces nothing whatever its configuration says.
var selectorStates = []string{"default", "configured", "deselected", "invalid", "guardUnavailable"}

func init() {
	prometheus.MustRegister(selectorState)
}

func recordSelectorState(active string) {
	for _, state := range selectorStates {
		value := 0.0
		if state == active {
			value = 1.0
		}
		selectorState.WithLabelValues(state).Set(value)
	}
}

// selectorReporter publishes selector_state, folding in the address guard:
// while a fail-closed guard isn't working the node announces nothing, and
// says so, whatever the last config delivery made of it. Config delivery
// (CR-controller goroutine) and the guard's attacher both publish through
// it; the state they share is atomic.
type selectorReporter struct {
	standingDown func() bool // nil: never
	base         atomic.Pointer[string]
}

// set records the state a config delivery arrived at and publishes the
// effective one, which it returns.
func (r *selectorReporter) set(state string) string {
	r.base.Store(&state)
	return r.publish()
}

// publish republishes the effective state, and returns it.
func (r *selectorReporter) publish() string {
	state := "default"
	if b := r.base.Load(); b != nil {
		state = *b
	}
	if r.standingDown != nil && r.standingDown() {
		state = "guardUnavailable"
	}
	recordSelectorState(state)
	return state
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

// nodeClient is the subset of *k8s.Client that config delivery uses.
// Narrowed to an interface so the delivery logic can be exercised without
// a live API server.
type nodeClient interface {
	Clientset() kubernetes.Interface
	Errorf(obj runtime.Object, kind, msg string, args ...interface{})
}

// configSetter is the subset of *controller that config delivery uses.
type configSetter interface {
	SetConfig(cfg *purelbv2.Config) k8s.SyncState
}

// newConfigChanged builds the ConfigChanged callback. It wraps
// ctrl.SetConfig with nodeSelector evaluation and keeps the election
// selector coherent with the announcer: the lease must never advertise a
// subnet the announcer cannot announce on. Convergence after a config
// change is two-wave: wave 1 reprocesses services against the stale lease
// subnets; wave 2 follows the next lease renewal (<=2.5s) plus informer
// propagation.
//
// getClient is a getter rather than the client itself because main builds
// the k8s client with this callback already in hand -- the client variable
// is still nil at construction time. That is safe because deliveries only
// happen from inside client.Run(), which happens-after the assignment.
func newConfigChanged(
	logger log.Logger,
	getClient func() nodeClient,
	ctrl configSetter,
	myNode string,
	selector *atomic.Pointer[election.InterfaceSelector],
	report *selectorReporter,
) func(*purelbv2.Config) k8s.SyncState {
	return func(cfg *purelbv2.Config) k8s.SyncState {
		client := getClient()

		// Fetch our Node's labels fresh per delivery — no stored label
		// state. Like a Pod's nodeSelector, label changes take effect
		// when config is next delivered (CR event, informer resync, or
		// pod restart), not continuously.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		node, err := client.Clientset().CoreV1().Nodes().Get(ctx, myNode, metav1.GetOptions{})
		cancel()
		if err != nil {
			// SyncStateError makes the CR controller requeue this
			// delivery with backoff; config is not applied.
			logging.Info(logger, "op", "configChanged", "error", err,
				"msg", "failed to get node for nodeSelector evaluation, delivery will be retried")
			return k8s.SyncStateError
		}

		// Log locally as well as in the allocator. An operator debugging a
		// VIP that stopped being announced looks at the node agent, and a
		// dropped `remote` group is exactly why it would have stopped.
		for _, g := range cfg.DroppedGroups {
			logging.Info(logger, "op", "configChanged", "event", "serviceGroupOutOfScope",
				"sg", g.Namespace+"/"+g.Name, "isRemote", fmt.Sprintf("%t", g.Spec.Remote != nil),
				"msg", "ignored: ServiceGroups are read only from the PureLB install namespace")
		}

		for _, agent := range cfg.Agents {
			logging.Debug(logger, "op", "configChanged", "agent", agent.Namespace+"/"+agent.Name,
				"hasNodeSelector", fmt.Sprintf("%t", agent.Spec.NodeSelector != nil))
		}

		// AgentsForNode drops agents whose selector will not convert, and
		// documents that reporting them is the caller's job. Do that here:
		// otherwise a typo in matchExpressions removes an LBNodeAgent from
		// consideration with no log, no event and no metric, which looks
		// exactly like the CR not existing.
		preFilter := len(cfg.Agents)
		for _, agent := range cfg.Agents {
			if purelbv2.IsCatchAll(agent.Spec.NodeSelector) {
				continue
			}
			if _, err := metav1.LabelSelectorAsSelector(agent.Spec.NodeSelector); err != nil {
				logging.Info(logger, "op", "configChanged", "event", "invalidNodeSelector",
					"agent", agent.Namespace+"/"+agent.Name, "error", err,
					"msg", "nodeSelector could not be evaluated; this LBNodeAgent matches no node")
				client.Errorf(agent, "InvalidNodeSelector",
					"nodeSelector could not be evaluated (%s); this LBNodeAgent applies to no node", err)
			}
		}

		cfg.Agents = purelbv2.AgentsForNode(cfg.Agents, node.Labels)

		// Report the agent whose configuration is actually applied, which is
		// the first one with a Local spec, not simply the highest-precedence
		// match. The announcer and the election selector both resolve through
		// FirstLocalAgent, so an LBNodeAgent carrying only a nodeSelector
		// sorts to the front, gets skipped, and naming it here would event
		// "ignored" against the CR that is genuinely in force.
		if len(cfg.Agents) > 1 {
			winner := purelbv2.FirstLocalAgent(cfg.Agents)
			if winner == nil {
				logging.Info(logger, "op", "configChanged", "event", "noLocalSpec", "node", myNode,
					"matching", fmt.Sprintf("%d", len(cfg.Agents)),
					"msg", "LBNodeAgents match this node but none has a local spec; announcing nothing")
			}
			for _, ignored := range cfg.Agents {
				if winner == nil || ignored == winner {
					continue
				}
				logging.Info(logger, "op", "configChanged", "msg", "multiple LBNodeAgents match this node, using highest precedence with a local spec",
					"node", myNode, "using", winner.Namespace+"/"+winner.Name, "ignoring", ignored.Namespace+"/"+ignored.Name)
				client.Errorf(ignored, "ConfigIgnored",
					"LBNodeAgent %s/%s takes precedence on node %s", winner.Namespace, winner.Name, myNode)
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
					"invalid configuration: node %s is announcing nothing until this is fixed", myNode)
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
		state = report.set(state)
		logging.Info(logger, "op", "configChanged", "selectorState", state,
			"node", myNode, "matchingAgents", fmt.Sprintf("%d", len(cfg.Agents)),
			"msg", "config delivery evaluated")

		return ret
	}
}

// readGuardEnabled reads, directly from the API, whether the address guard
// is configured for myNode right now. The agent loads the guard program
// only then, and decides before any informer or goroutine runs, so the
// program never changes under the goroutines that use it.
func readGuardEnabled(logger log.Logger, kubeconfig, myNode string) (bool, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return false, fmt.Errorf("building client config: %w", err)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	cr, err := versioned.NewForConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("creating custom resource client: %w", err)
	}
	listAgents := func(ctx context.Context) ([]*purelbv2.LBNodeAgent, error) {
		// Every namespace, as the CR informer lists them.
		list, err := cr.PurelbV2().LBNodeAgents("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		agents := make([]*purelbv2.LBNodeAgent, 0, len(list.Items))
		for i := range list.Items {
			agents = append(agents, &list.Items[i])
		}
		return agents, nil
	}
	nodeLabels := func(ctx context.Context) (map[string]string, error) {
		node, err := core.CoreV1().Nodes().Get(ctx, myNode, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return node.Labels, nil
	}
	return guardEnabledAtStartup(logger, listAgents, nodeLabels, 30*time.Second, 2*time.Second)
}

// guardEnabledAtStartup resolves the address guard configuration the way
// every config delivery does (addrguard.Enabled), retrying for up to
// timeout: the agent can't run without the API anyway, and exiting lets the
// kubelet restart it.
func guardEnabledAtStartup(
	logger log.Logger,
	listAgents func(context.Context) ([]*purelbv2.LBNodeAgent, error),
	nodeLabels func(context.Context) (map[string]string, error),
	timeout, interval time.Duration,
) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		enabled, err := func() (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			agents, err := listAgents(ctx)
			if err != nil {
				return false, fmt.Errorf("listing LBNodeAgents: %w", err)
			}
			labels, err := nodeLabels(ctx)
			if err != nil {
				return false, fmt.Errorf("reading this node's labels: %w", err)
			}
			return addrguard.Enabled(agents, labels), nil
		}()
		if err == nil || time.Now().After(deadline) {
			return enabled, err
		}
		logging.Info(logger, "op", "startup", "error", err,
			"msg", "could not read the address guard configuration, retrying")
		time.Sleep(interval)
	}
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

	// The address guard program is loaded only if the guard is configured
	// for this node, and only at startup: decide before anything runs.
	guardEnabled, err := readGuardEnabled(logger, *kubeconfig, *myNode)
	if err != nil {
		logging.Info(logger, "op", "startup", "error", err, "msg", "could not read the address guard configuration")
		os.Exit(1)
	}

	// Set up controller
	ctrl, err := NewController(
		logger,
		*myNode,
		guardEnabled,
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

	// A fail-closed address guard that isn't working takes this node out
	// of the election (no subnets below) and the announcer withdraws
	// everything (local.NewAnnouncer is given guard.StandingDown).
	report := &selectorReporter{standingDown: ctrl.guard.StandingDown}
	configChanged := newConfigChanged(logger, func() nodeClient { return client }, ctrl, *myNode, &selector, report)

	client, err = k8s.New(&k8s.Config{
		ProcessName:        "purelb-lbnodeagent",
		NodeName:           *myNode,
		Logger:             logger,
		Kubeconfig:         *kubeconfig,
		Namespace:          *namespace,
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
			// identical to the historical behavior. A node whose
			// fail-closed address guard isn't working advertises no
			// subnets, so no node -- this one included -- elects it.
			if ctrl.guard.StandingDown() {
				return []string{}, nil
			}
			return election.GetSelectedSubnets(selector.Load(), logger)
		},
	})
	if err != nil {
		logging.Info(logger, "op", "startup", "error", err, "msg", "failed to create election")
		os.Exit(1)
	}

	ctrl.SetElection(elect)

	// The guard calls this from its attacher goroutine whenever standing
	// down changes. Re-sync every Service, so the announcer withdraws or
	// re-announces now rather than at the next resync; the lease's subnets
	// follow at its next renewal.
	ctrl.guard.SetStandDownHook(func(down bool) {
		state := report.publish()
		logging.Info(logger, "op", "addressGuard", "standingDown", down, "selectorState", state,
			"msg", "re-syncing services")
		client.ForceSync()
	})

	// Start the election (creates lease, starts informer)
	if err := elect.Start(); err != nil {
		logging.Info(logger, "op", "startup", "error", err, "msg", "failed to start election")
		os.Exit(1)
	}

	go k8s.RunMetrics(logger, *host, *port)

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
