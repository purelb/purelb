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
	"net"
	"net/http"
	"strconv"

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
}

// metricsAddr joins host and port into a listen address. An IPv6 host
// must be bracketed, or the listener fails with "too many colons".
func metricsAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// RunMetrics runs the metrics server. It only returns if the listener
// fails, which it logs: the error used to be discarded, so a metrics
// endpoint that never came up was indistinguishable from an idle one.
func RunMetrics(logger log.Logger, metricsHost string, metricsPort int) {
	addr := metricsAddr(metricsHost, metricsPort)
	http.Handle("/metrics", promhttp.Handler())
	err := http.ListenAndServe(addr, nil)
	logging.Info(logger, "op", "metrics", "action", "listen", "addr", addr, "error", err)
}
