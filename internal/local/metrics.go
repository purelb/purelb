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

package local

import (
	"github.com/prometheus/client_golang/prometheus"

	purelbv1 "purelb.io/pkg/apis/purelb/v1"
)

const subsystem = "lbnodeagent"

var (
	// garpSent counts the total number of GARP packets sent.
	garpSent = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "garp_sent_total",
		Help:      "Total number of GARP (Gratuitous ARP) packets sent",
	})

	// garpErrors counts GARP send failures.
	garpErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "garp_errors_total",
		Help:      "Total number of GARP send failures",
	})

	// addressRenewalCount counts successful address renewals.
	addressRenewalCount = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "address_renewals_total",
		Help:      "Total number of address lifetime renewals",
	})

	// addressRenewalErrors counts address renewal failures.
	addressRenewalErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "address_renewal_errors_total",
		Help:      "Total number of address renewal failures",
	})

	// addressAdditions counts addresses added to interfaces.
	addressAdditions = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "address_additions_total",
		Help:      "Total number of addresses added to interfaces",
	})

	// addressWithdrawals counts addresses withdrawn from interfaces.
	addressWithdrawals = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "address_withdrawals_total",
		Help:      "Total number of addresses withdrawn from interfaces",
	})

	// electionWins counts how many times this node won an election.
	electionWins = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "election_wins_total",
		Help:      "Total number of election wins on this node",
	})

	// electionLosses counts how many times this node lost an election.
	electionLosses = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "election_losses_total",
		Help:      "Total number of election losses on this node",
	})
)

func init() {
	prometheus.MustRegister(garpSent)
	prometheus.MustRegister(garpErrors)
	prometheus.MustRegister(addressRenewalCount)
	prometheus.MustRegister(addressRenewalErrors)
	prometheus.MustRegister(addressAdditions)
	prometheus.MustRegister(addressWithdrawals)
	prometheus.MustRegister(electionWins)
	prometheus.MustRegister(electionLosses)
}

// RecordGARPSent increments the GARP sent counter.
func RecordGARPSent() {
	garpSent.Inc()
}

// RecordGARPError increments the GARP error counter.
func RecordGARPError() {
	garpErrors.Inc()
}

// RecordAddressRenewal increments the address renewal counter.
func RecordAddressRenewal() {
	addressRenewalCount.Inc()
}

// RecordAddressRenewalError increments the address renewal error counter.
func RecordAddressRenewalError() {
	addressRenewalErrors.Inc()
}

// RecordAddressAddition increments the address addition counter.
func RecordAddressAddition() {
	addressAdditions.Inc()
}

// RecordAddressWithdrawal increments the address withdrawal counter.
func RecordAddressWithdrawal() {
	addressWithdrawals.Inc()
}

// RecordElectionWin increments the election win counter.
func RecordElectionWin() {
	electionWins.Inc()
}

// RecordElectionLoss increments the election loss counter.
func RecordElectionLoss() {
	electionLosses.Inc()
}
