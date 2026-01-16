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
// See the License for the sp

package allocator

import (
	purelbv1 "purelb.io/pkg/apis/purelb/v1"

	"github.com/prometheus/client_golang/prometheus"
)

const subsystem = "address_pool"

var (
	labelNames = []string{"pool"}

	poolCapacity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "size",
		Help:      "Number of addresses in the pool",
	}, labelNames)

	poolActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "addresses_in_use",
		Help:      "Number of addresses allocated from the pool",
	}, labelNames)

	allocationRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "allocation_rejected_total",
		Help:      "Number of allocation requests rejected due to sharing constraints or exhaustion",
	}, []string{"pool", "reason"})
)

func init() {
	prometheus.MustRegister(poolCapacity)
	prometheus.MustRegister(poolActive)
	prometheus.MustRegister(allocationRejected)
}
