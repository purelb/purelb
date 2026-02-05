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

package k8s

import (
	"fmt"
	"net/http"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const subsystem = "k8s_client"

var (
	updates = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "updates_total",
		Help:      "Number of k8s object updates that have been processed.",
	})

	updateErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "update_errors_total",
		Help:      "Number of k8s object updates that failed for some reason.",
	})

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

// RunMetrics runs the metrics server. It doesn't ever return.
func RunMetrics(metricsHost string, metricsPort int) {
	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(fmt.Sprintf("%s:%d", metricsHost, metricsPort), nil)
}
