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

package addrguard

import (
	"net/netip"

	"github.com/prometheus/client_golang/prometheus"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

const subsystem = "address_guard"

var (
	attachedVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "attached",
		Help:      "1 for each interface the address guard is attached to, by hook (tcx or xdp).",
	}, []string{"interface", "hook"})

	unattachedVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "unattached_interfaces",
		Help:      "1 for each interface the address guard should be attached to and isn't, because attaching failed: VIP traffic arriving there is not filtered.",
	}, []string{"interface"})

	reconcileDeferred = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "reconcile_deferred_total",
		Help:      "Reconciles put off to the next 30s check because a netlink dump (links or routes) stayed interrupted -- what it listed was changing -- or failed. Nothing changes meanwhile: no interface is detached and the node doesn't stand down.",
	}, []string{"dump"})

	attachErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "attach_errors_total",
		Help:      "Times the address guard could attach no hook to an interface, leaving VIP traffic arriving there unfiltered.",
	}, []string{"interface"})

	chainPositionVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "chain_position",
		Help:      "Where the address guard runs in each interface's tcx ingress chain: 1 is first. Above 1, other programs run before it and can drop or redirect VIP traffic it never sees. Not reported for XDP, which has one slot.",
	}, []string{"interface"})

	loadedGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: purelbv2.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "loaded",
		Help:      "1 if the address guard program is loaded on this node.",
	})

	packetsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, subsystem, "packets_total"),
		"Packets addressed to a guarded VIP, by verdict (pass, drop, or would_drop in monitor mode), reason and family.",
		[]string{"action", "reason", "family"}, nil)

	unreadDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, subsystem, "unread_packets_total"),
		"Frames the guard could not read, so dropped (would_drop in monitor mode) whatever their destination: truncated (shorter than the VLAN tag or IP header they claim), vlan_depth (more VLAN tags than QinQ's two).",
		[]string{"reason", "action"}, nil)

	vipPacketsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, subsystem, "vip_packets_total"),
		"Packets to one guarded address that were dropped, or would be dropped in monitor mode. Zero series are omitted.",
		[]string{"ip", "action"}, nil)

	standingDownDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, subsystem, "standing_down"),
		"1 if this node announces nothing because the guard is configured fail-closed and isn't working.",
		nil, nil)

	unguardedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, subsystem, "unguarded_vips"),
		"VIPs the configured address guard is not filtering on this node: not_loaded when the program isn't running, map_write_failed when a VIP's rules could not be written, restart_pending when the guard was enabled after lbnodeagent started.",
		[]string{"reason"}, nil)

	failedVIPsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, subsystem, "failed_vips"),
		"Under failurePolicy closed, VIPs whose rules could not be written to the guard's maps (a map full, or out of memory): withheld VIPs are not announced from this node; incomplete VIPs are filtered by rules that don't match their Services yet, so a Service port may be dropped. Retried on every Service sync.",
		[]string{"effect"}, nil)

	restartRequiredDesc = prometheus.NewDesc(
		prometheus.BuildFQName(purelbv2.MetricsNamespace, subsystem, "restart_required"),
		"1 if the address guard was enabled or disabled after lbnodeagent started on this node: it is loaded only at startup, so the change takes full effect when lbnodeagent is restarted.",
		nil, nil)
)

func init() {
	prometheus.MustRegister(attachedVec, unattachedVec, reconcileDeferred, attachErrors, chainPositionVec, loadedGauge)
}

var reasonNames = [reasonMax]string{
	reasonPortAllowed:  "port_allowed",
	reasonProtoAllowed: "proto_allowed",
	reasonICMP:         "icmp",
	reasonFragment:     "fragment",
	reasonPortDenied:   "port_denied",
	reasonProtoDenied:  "proto_denied",
	reasonICMPDenied:   "icmp_denied",
	reasonMalformed:    "malformed",
}

var familyNames = [2]string{famV4: "ipv4", famV6: "ipv6"}

var unreadNames = [unreadMax]string{
	unreadTruncated: "truncated",
	unreadVLANDepth: "vlan_depth",
}

// collector reports what lives in the BPF maps, reading them at scrape
// time. It touches no Guard state except atomics, so it is safe on the
// HTTP goroutine.
type collector struct{ g *Guard }

func registerCollector(g *Guard) {
	if g.objs != nil {
		loadedGauge.Set(1)
	}
	prometheus.MustRegister(collector{g})
}

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- packetsDesc
	ch <- unreadDesc
	ch <- vipPacketsDesc
	ch <- standingDownDesc
	ch <- unguardedDesc
	ch <- failedVIPsDesc
	ch <- restartRequiredDesc
}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	g := c.g
	standingDown := 0.0
	if g.StandingDown() {
		standingDown = 1
	}
	ch <- prometheus.MustNewConstMetric(standingDownDesc, prometheus.GaugeValue, standingDown)
	restartRequired := 0.0
	if g.restartRequired.Load() {
		restartRequired = 1
	}
	ch <- prometheus.MustNewConstMetric(restartRequiredDesc, prometheus.GaugeValue, restartRequired)

	// Fail-closed exposes nothing it can't filter -- the node stands down,
	// and a VIP whose rules couldn't be written stays filtered -- so those
	// count only under fail-open. A guard enabled after startup filters
	// nothing until the agent restarts, whatever the policy.
	var notLoaded, writeFailed, restartPending float64
	switch {
	case g.configured.Load() && !g.loadWanted:
		restartPending = float64(g.vipCount.Load())
	case g.configured.Load() && !g.closed.Load():
		if g.objs == nil {
			notLoaded = float64(g.vipCount.Load())
		}
		writeFailed = float64(g.failedCount.Load())
	}
	ch <- prometheus.MustNewConstMetric(unguardedDesc, prometheus.GaugeValue, notLoaded, "not_loaded")
	ch <- prometheus.MustNewConstMetric(unguardedDesc, prometheus.GaugeValue, writeFailed, "map_write_failed")
	ch <- prometheus.MustNewConstMetric(unguardedDesc, prometheus.GaugeValue, restartPending, "restart_pending")
	ch <- prometheus.MustNewConstMetric(failedVIPsDesc, prometheus.GaugeValue, float64(g.withheldCount.Load()), "withheld")
	ch <- prometheus.MustNewConstMetric(failedVIPsDesc, prometheus.GaugeValue, float64(g.incompleteCount.Load()), "incomplete")

	if g.objs == nil {
		return
	}
	for r := uint32(0); r < unreadMax; r++ {
		for _, monitor := range []bool{false, true} {
			var per []uint64
			if err := g.objs.AgUnread.Lookup(unreadIndex(r, monitor), &per); err != nil {
				continue
			}
			var n uint64
			for _, v := range per {
				n += v
			}
			ch <- prometheus.MustNewConstMetric(unreadDesc, prometheus.CounterValue, float64(n),
				unreadNames[r], map[bool]string{false: "drop", true: "would_drop"}[monitor])
		}
	}

	// Only the combinations the program can produce: pass with an allow
	// reason, drop and would_drop with a deny reason.
	for fam := uint32(0); fam < 2; fam++ {
		for r := uint32(0); r < reasonMax; r++ {
			actions := []uint32{actDrop, actWouldDrop}
			if r <= reasonFragment {
				actions = []uint32{actPass}
			}
			for _, act := range actions {
				var per []uint64
				if err := g.objs.AgReasons.Lookup(reasonIndex(act, r, fam), &per); err != nil {
					continue
				}
				var n uint64
				for _, v := range per {
					n += v
				}
				ch <- prometheus.MustNewConstMetric(packetsDesc, prometheus.CounterValue, float64(n),
					[]string{"pass", "drop", "would_drop"}[act], reasonNames[r], familyNames[fam])
			}
		}
	}

	emit := func(ip netip.Addr, per []addrguardVipCounters) {
		var c addrguardVipCounters
		for _, v := range per {
			c.Drop += v.Drop
			c.WouldDrop += v.WouldDrop
		}
		if c.Drop > 0 {
			ch <- prometheus.MustNewConstMetric(vipPacketsDesc, prometheus.CounterValue, float64(c.Drop), ip.String(), "drop")
		}
		if c.WouldDrop > 0 {
			ch <- prometheus.MustNewConstMetric(vipPacketsDesc, prometheus.CounterValue, float64(c.WouldDrop), ip.String(), "would_drop")
		}
	}
	var k4 [4]byte
	var k6 [16]byte
	var per []addrguardVipCounters
	it4 := g.objs.AgVip4.Iterate()
	for it4.Next(&k4, &per) {
		emit(netip.AddrFrom4(k4), per)
	}
	it6 := g.objs.AgVip6.Iterate()
	for it6.Next(&k6, &per) {
		emit(netip.AddrFrom16(k6), per)
	}
}
