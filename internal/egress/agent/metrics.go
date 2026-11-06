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

package agent

import (
	"net/netip"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var (
	reasonPacketsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "datapath_packets_total"),
		"Packets the egress programs handled, by reason, action (pass or drop) and family.",
		[]string{"reason", "action", "family"}, nil)
	packetsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "packets_total"),
		"Packets of an egress address on this node, by direction (out, return) and role (pod_node, announcer). Addresses of Redacted groups are left out.",
		[]string{"ip", "direction", "role"}, nil)
	bytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "bytes_total"),
		"Bytes of an egress address on this node, by direction (out, return) and role (pod_node, announcer). Addresses of Redacted groups are left out.",
		[]string{"ip", "direction", "role"}, nil)
	peersDownDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "tunnel_peers_down"),
		"Tunnel peers not answering this node's probes. kubectl purelb egress dump names them.",
		nil, nil)
	orphanPinsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, "egress", "orphan_pins"),
		"Objects still pinned on this node after egress was turned off and its datapath removed.",
		nil, nil)
)

// trafficLabels names each datapath.Traffic kind's direction and role.
var trafficLabels = [datapath.TrafficKinds][2]string{
	datapath.TrafficOutPod:          {"out", "pod_node"},
	datapath.TrafficReturnPod:       {"return", "pod_node"},
	datapath.TrafficOutAnnouncer:    {"out", "announcer"},
	datapath.TrafficReturnAnnouncer: {"return", "announcer"},
}

// dpMetrics is what the datapath goroutine last read for the metrics:
// scrapes never touch the maps, which that goroutine may close.
type dpMetrics struct {
	reasons   map[string][2]uint64
	traffic   map[netip.Addr]datapath.Traffic
	peersDown int
}

var (
	currentDPMetrics atomic.Pointer[dpMetrics]
	// egressIdle: egress is off and teardown has run, so anything pinned is
	// left over. False until then: pins are normal while an agent starts.
	egressIdle atomic.Bool
)

func init() {
	prometheus.MustRegister(dpCollector{})
}

type dpCollector struct{}

func (dpCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- reasonPacketsDesc
	ch <- packetsDesc
	ch <- bytesDesc
	ch <- peersDownDesc
	ch <- orphanPinsDesc
}

func (dpCollector) Collect(ch chan<- prometheus.Metric) {
	if egressIdle.Load() {
		ch <- prometheus.MustNewConstMetric(orphanPinsDesc, prometheus.GaugeValue, float64(datapath.PinnedCount(bpfpin.Dir)))
	}
	m := currentDPMetrics.Load()
	if m == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(peersDownDesc, prometheus.GaugeValue, float64(m.peersDown))
	for reason, c := range m.reasons {
		action := datapath.ReasonAction(reason)
		ch <- prometheus.MustNewConstMetric(reasonPacketsDesc, prometheus.CounterValue, float64(c[0]), reason, action, "ipv4")
		ch <- prometheus.MustNewConstMetric(reasonPacketsDesc, prometheus.CounterValue, float64(c[1]), reason, action, "ipv6")
	}
	for addr, t := range m.traffic {
		for kind, l := range trafficLabels {
			ch <- prometheus.MustNewConstMetric(packetsDesc, prometheus.CounterValue, float64(t.Packets[kind]), addr.String(), l[0], l[1])
			ch <- prometheus.MustNewConstMetric(bytesDesc, prometheus.CounterValue, float64(t.Bytes[kind]), addr.String(), l[0], l[1])
		}
	}
}

// publishMetrics reads the maps for the next scrapes. A read that fails keeps
// the last values. Datapath goroutine only.
func (a *Agent) publishMetrics(st *dpState, redacted map[netip.Addr]bool, peersDown int) {
	if st.objs == nil {
		currentDPMetrics.Store(nil)
		return
	}
	reasons, err := st.objs.Counts()
	if err != nil {
		logging.Debug(a.logger, "op", "egressMetrics", "node", a.node, "error", err, "msg", "reading the reason counters")
		return
	}
	traffic, err := st.objs.TrafficCounts()
	if err != nil {
		logging.Debug(a.logger, "op", "egressMetrics", "node", a.node, "error", err, "msg", "reading the traffic counters")
		return
	}
	if st.table != nil && st.table.Pods != nil {
		slots := map[uint8]netip.Addr{}
		for id, slot := range st.table.Pods.Slots {
			slots[id] = slot.Addr
		}
		out, err := datapath.SlotTraffic(st.nft, slots)
		if err != nil {
			logging.Debug(a.logger, "op", "egressMetrics", "node", a.node, "error", err, "msg", "reading the slot counters")
			return
		}
		for addr, t := range out {
			have := traffic[addr]
			have.Packets[datapath.TrafficOutPod] += t.Packets[datapath.TrafficOutPod]
			have.Bytes[datapath.TrafficOutPod] += t.Bytes[datapath.TrafficOutPod]
			traffic[addr] = have
		}
	}
	for addr := range redacted {
		delete(traffic, addr)
	}
	currentDPMetrics.Store(&dpMetrics{reasons: reasons, traffic: traffic, peersDown: peersDown})
}

// groupRedacted reports whether g hides nodes, slices and per-address metrics.
func groupRedacted(g *purelbv2.ServiceGroup) bool {
	return g != nil && g.Spec.Egress != nil && g.Spec.Egress.Tenancy != nil && g.Spec.Egress.Tenancy.StatusDetail == "Redacted"
}
