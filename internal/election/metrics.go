// Copyright 2024 Acnodal Inc.
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

package election

import (
	"github.com/prometheus/client_golang/prometheus"

	purelbv1 "purelb.io/pkg/apis/purelb/v1"
)

const subsystem = "election"

var (
	// leaseHealthy is 1 if this node's lease is healthy and being renewed,
	// 0 if renewals are failing or the node is shutting down.
	leaseHealthy = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "lease_healthy",
		Help:      "1 if this node's lease is healthy, 0 otherwise",
	})

	// leaseRenewals counts successful lease renewals.
	leaseRenewals = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "lease_renewals_total",
		Help:      "Total number of successful lease renewals",
	})

	// leaseRenewalFailures counts failed lease renewal attempts.
	leaseRenewalFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "lease_renewal_failures_total",
		Help:      "Total number of failed lease renewal attempts",
	})

	// winnerChanges counts the number of times a winner changed for any service.
	// Labels: service (namespace/name)
	winnerChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "winner_changes_total",
		Help:      "Total number of winner changes per service",
	}, []string{"service"})

	// memberCount tracks the current number of active members in the election.
	memberCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "member_count",
		Help:      "Current number of active members in the election",
	})

	// subnetCount tracks the number of unique subnets tracked across all members.
	subnetCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "subnet_count",
		Help:      "Number of unique subnets tracked across all members",
	})

	// localSubnetCount tracks the number of subnets on this node.
	localSubnetCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "local_subnet_count",
		Help:      "Number of subnets on this node",
	})
)

func init() {
	prometheus.MustRegister(leaseHealthy)
	prometheus.MustRegister(leaseRenewals)
	prometheus.MustRegister(leaseRenewalFailures)
	prometheus.MustRegister(winnerChanges)
	prometheus.MustRegister(memberCount)
	prometheus.MustRegister(subnetCount)
	prometheus.MustRegister(localSubnetCount)
}

// RecordLeaseHealthy sets the lease health metric.
func RecordLeaseHealthy(healthy bool) {
	if healthy {
		leaseHealthy.Set(1)
	} else {
		leaseHealthy.Set(0)
	}
}

// RecordLeaseRenewal increments the successful renewal counter.
func RecordLeaseRenewal() {
	leaseRenewals.Inc()
}

// RecordLeaseRenewalFailure increments the failed renewal counter.
func RecordLeaseRenewalFailure() {
	leaseRenewalFailures.Inc()
}

// RecordWinnerChange increments the winner change counter for a service.
func RecordWinnerChange(service string) {
	winnerChanges.WithLabelValues(service).Inc()
}

// RecordMemberCount sets the current member count.
func RecordMemberCount(count int) {
	memberCount.Set(float64(count))
}

// RecordSubnetCount sets the total subnet count.
func RecordSubnetCount(count int) {
	subnetCount.Set(float64(count))
}

// RecordLocalSubnetCount sets the local subnet count.
func RecordLocalSubnetCount(count int) {
	localSubnetCount.Set(float64(count))
}
