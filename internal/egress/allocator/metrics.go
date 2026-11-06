// Copyright 2026 Acnodal Inc.
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

package allocator

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	listers "purelb.io/pkg/generated/listers/purelb/v2"
)

// The alerts' inputs (user decision 2026-11-05): counts by reason, so a
// series never names a node or a policy; EgressNodeState and the policies'
// status say which.
var (
	nodeConditionsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "node_conditions_false"),
		"Nodes with an EgressNodeState condition False, by condition and reason.",
		[]string{"condition", "reason"}, nil)
	addressFaultsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "node_address_faults"),
		"Faults nodes report on egress addresses (EgressNodeState status.addressFaults), by reason.",
		[]string{"reason"}, nil)
	policyConditionsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "policy_conditions_false"),
		"Egress and ClusterEgress policies with a condition False, by condition and reason.",
		[]string{"condition", "reason"}, nil)
)

// conditionKey is one series of a conditions gauge.
type conditionKey struct{ condition, reason string }

// statusListers read the objects the gauges count. Set once they're synced;
// listers are safe from the HTTP goroutine.
type statusListers struct {
	egresses        listers.EgressLister
	clusterEgresses listers.ClusterEgressLister
	nodeStates      listers.EgressNodeStateLister
}

var currentStatus atomic.Pointer[statusListers]

func init() {
	prometheus.MustRegister(statusCollector{})
}

type statusCollector struct{}

func (statusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- nodeConditionsDesc
	ch <- addressFaultsDesc
	ch <- policyConditionsDesc
}

func (statusCollector) Collect(ch chan<- prometheus.Metric) {
	l := currentStatus.Load()
	if l == nil {
		return // egress's CRDs aren't installed, or aren't synced
	}
	nodes, faults, policies := map[conditionKey]int{}, map[string]int{}, map[conditionKey]int{}
	if nss, err := l.nodeStates.List(labels.Everything()); err == nil {
		for _, ns := range nss {
			countFalse(nodes, ns.Status.Conditions)
			for _, f := range ns.Status.AddressFaults {
				faults[f.Reason]++
			}
		}
	}
	if es, err := l.egresses.List(labels.Everything()); err == nil {
		for _, e := range es {
			countFalse(policies, e.Status.Conditions)
		}
	}
	if cs, err := l.clusterEgresses.List(labels.Everything()); err == nil {
		for _, c := range cs {
			countFalse(policies, c.Status.Conditions)
		}
	}
	for k, n := range nodes {
		ch <- prometheus.MustNewConstMetric(nodeConditionsDesc, prometheus.GaugeValue, float64(n), k.condition, k.reason)
	}
	for r, n := range faults {
		ch <- prometheus.MustNewConstMetric(addressFaultsDesc, prometheus.GaugeValue, float64(n), r)
	}
	for k, n := range policies {
		ch <- prometheus.MustNewConstMetric(policyConditionsDesc, prometheus.GaugeValue, float64(n), k.condition, k.reason)
	}
}

func countFalse(into map[conditionKey]int, conds []metav1.Condition) {
	for _, c := range conds {
		if c.Status == metav1.ConditionFalse {
			into[conditionKey{c.Type, c.Reason}]++
		}
	}
}
