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

package k8s

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-kit/log"

	"purelb.io/internal/logging"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const subsystem = "k8s_client"

var (
	updates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "updates_total",
		Help:      "Number of k8s object updates that have been processed.",
	}, []string{"kind"})

	updateErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "update_errors_total",
		Help:      "Number of k8s object updates that failed for some reason.",
	}, []string{"kind"})

	// crdStale is 1 for an installed PureLB CRD whose release stamp
	// differs from this binary's release (see watchCRDVersions).
	crdStale = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Name:      "crd_stale",
		Help:      "1 if the installed CRD is from another release than this binary, else 0.",
	}, []string{"crd"})

	configLoaded = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "config_loaded_bool",
		Help:      "1 if the PureLB configuration was successfully loaded at least once.",
	})
)

func init() {
	prometheus.MustRegister(updates)
	prometheus.MustRegister(updateErrors)
	prometheus.MustRegister(configLoaded)
	prometheus.MustRegister(crdStale)
}

// metricsAddr joins host and port into a listen address. An IPv6 host
// must be bracketed, or the listener fails with "too many colons".
func metricsAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// RunMetrics runs the metrics and health server. It only returns if the
// listener fails, which it logs: the error used to be discarded, so a
// metrics endpoint that never came up was indistinguishable from an idle
// one.
//
// Health shares the metrics listener rather than taking a port of its
// own. Both PureLB manifests already declare 7472, and the probe spec is
// then identical in the BGP and no-BGP variants -- the k8gobgp sidecar's
// 7474 does not exist in a no-BGP install.
func RunMetrics(logger log.Logger, metricsHost string, metricsPort int, health *Client) {
	addr := metricsAddr(metricsHost, metricsPort)
	err := http.ListenAndServe(addr, serveMux(logger, health))
	logging.Info(logger, "op", "metrics", "action", "listen", "addr", addr, "error", err)
}

// serveMux builds the handler RunMetrics serves.
//
// An explicit mux rather than http.DefaultServeMux: the default is
// process-global, so anything a dependency registers on it would be
// published on this port too, and a test cannot stand up the real
// routing twice in one process.
func serveMux(logger log.Logger, health *Client) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	// Liveness failures are logged at Info: they are rare and a restart
	// follows, so the reason belongs in the log the operator reads after
	// it. Readiness can stay false for as long as the cause lasts, so it
	// logs at Debug rather than once per probe period forever.
	mux.HandleFunc("/healthz", healthHandler(logger, "healthz", health.Live, true))
	mux.HandleFunc("/readyz", healthHandler(logger, "readyz", health.Ready, false))
	return mux
}

// livenessStuckAfter bounds how long the service-sync goroutine may spend
// inside a single sync() before /healthz fails and kubelet restarts the
// container.
//
// Every operation that goroutine performs is already bounded: apiserver
// and sidecar-IPAM calls go through APIContext/poolContext, which cap a
// single call at 10s. This is an order of magnitude above that, so it
// fires for a genuine wedge -- a blocked syscall, an unbounded wait --
// and never for a slow-but-live reconcile.
const livenessStuckAfter = 120 * time.Second

// healthHandler serves one health endpoint: 200 when check returns nil,
// 503 with the reason otherwise.
//
// The reason goes in the response body because kubelet copies it into the
// Unhealthy event, which is the first place an operator looks.
func healthHandler(logger log.Logger, name string, check func() error, logFailure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		err := check()
		if err == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		if logFailure {
			logging.Info(logger, "op", name, "result", "fail", "error", err)
		} else {
			logging.Debug(logger, "op", name, "result", "fail", "error", err)
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}

// Live reports whether the service-sync goroutine is still turning over.
// It backs /healthz, and also the startupProbe -- see Ready for why that
// probe must not use /readyz.
//
// Being stuck inside one work item is deliberately the only liveness
// condition. Cache sync and apiserver reachability are readiness
// concerns, and lease health and guard stand-down are neither:
// restarting the container fixes none of them, it only adds churn on top
// of whatever is already wrong.
func (c *Client) Live() error {
	started := c.syncStartedAt.Load()
	if started == 0 {
		// No work item in flight. The goroutine blocks in queue.Get()
		// for as long as the cluster is quiet, which on a healthy
		// cluster is indefinitely -- so idle is always live. Stamping a
		// "last progress" time instead would make a quiet cluster
		// indistinguishable from a wedged one.
		return nil
	}
	if stuck := time.Since(time.Unix(0, started)); stuck > livenessStuckAfter {
		return fmt.Errorf("service-sync goroutine stuck in one work item for %s", stuck.Round(time.Second))
	}
	return nil
}

// Ready reports whether this process can act on Services. It backs
// /readyz. Cache sync is the whole condition.
//
// It is deliberately shallow. Readiness gates DaemonSet and Deployment
// rollouts, so every condition added here becomes a condition that can
// wedge an upgrade. Gating it on election lease health and address-guard
// stand-down was tried and reverted: with the guard unable to load
// cluster-wide -- which is its designed fail-closed state -- no agent
// ever reported Ready, so no rollout could complete, including the one
// that would fix it. The e2e suite caught it as two 900s rollout
// timeouts.
//
// Those conditions belong in alerts, where they already are:
// purelb_address_guard_standing_down and purelb_election_lease_healthy
// both have PrometheusRules. k8gobgp reached the same conclusion for BGP
// session state and says so in controllers/gobgpd_health.go.
//
// Also deliberately not "config loaded": a cluster with no ServiceGroups
// yet is a valid state, and gating readiness on config would leave a
// fresh install NotReady forever.
func (c *Client) Ready() error {
	if !c.cachesSynced.Load() {
		return errors.New("informer caches not synced")
	}
	return nil
}
