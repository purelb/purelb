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
// See the License for the sp

package allocator

import (
	purelbv2 "purelb.io/pkg/apis/purelb/v2"

	"github.com/prometheus/client_golang/prometheus"
)

const subsystem = "address_pool"

var (
	labelNames = []string{"pool"}

	poolCapacity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "size",
		Help:      "Number of addresses in the pool",
	}, labelNames)

	poolActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "addresses_in_use",
		Help:      "Number of addresses allocated from the pool",
	}, labelNames)

	allocationRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "allocation_rejected_total",
		Help:      "Number of allocation requests rejected due to sharing constraints or exhaustion",
	}, []string{"pool", "reason"})

	multipoolAllocations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "multipool_allocations_total",
		Help:      "Total multi-pool allocations performed",
	}, labelNames)

	multipoolPartial = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "multipool_partial_total",
		Help:      "Multi-pool allocations where some ranges were exhausted or had no active nodes",
	}, labelNames)

	balancePoolsAllocations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "balance_pools_allocations_total",
		Help:      "Total balancePools allocations performed",
	}, labelNames)

	// sgStatusWritesTotal tracks ServiceGroup .status subresource write
	// outcomes. Defends against the v0.16.4-class silent-RBAC-failure
	// pattern: if servicegroups/status RBAC isn't granted, the
	// "forbidden" series will tick non-zero and operators can alert on
	// it via `rate(sg_status_writes_total{outcome!="success"}[5m]) > 0`.
	sgStatusWritesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: "allocator",
		Name:      "sg_status_writes_total",
		Help:      "ServiceGroup status subresource write outcomes (success|conflict|forbidden|other).",
	}, []string{"outcome"})

	// sidecarRPCTotal counts external-IPAM sidecar RPCs by socket,
	// method, and gRPC status code. The connectivity signal for sidecar
	// pools: alert on `code!="OK"` rate (no separate connected gauge).
	sidecarRPCTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: "allocator",
		Name:      "sidecar_rpc_total",
		Help:      "External-IPAM sidecar RPCs by socket, method, and gRPC status code.",
	}, []string{"socket", "method", "code"})

	// sidecarRPCDuration is the latency histogram for sidecar RPCs.
	sidecarRPCDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: "allocator",
		Name:      "sidecar_rpc_duration_seconds",
		Help:      "External-IPAM sidecar RPC latency by socket and method.",
	}, []string{"socket", "method"})
)

func init() {
	prometheus.MustRegister(poolCapacity)
	prometheus.MustRegister(poolActive)
	prometheus.MustRegister(allocationRejected)
	prometheus.MustRegister(multipoolAllocations)
	prometheus.MustRegister(multipoolPartial)
	prometheus.MustRegister(balancePoolsAllocations)
	prometheus.MustRegister(sgStatusWritesTotal)
	prometheus.MustRegister(sidecarRPCTotal)
	prometheus.MustRegister(sidecarRPCDuration)
}
