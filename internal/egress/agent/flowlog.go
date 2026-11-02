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
	"errors"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var (
	flowEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "flow_log_events_total",
		Help: "Egress flows the flow log wrote, by conntrack event (new, destroy).",
	}, []string{"event"})
	flowOverflows = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "flow_log_overflows_total",
		Help: "Times the flow log's conntrack event socket overflowed and the kernel dropped events (how many isn't told).",
	})
)

func init() {
	prometheus.MustRegister(flowEvents, flowOverflows)
}

// flowWait bounds one read of the event socket, and so how long the flow
// log takes to stop.
const flowWait = time.Second

// flowView is what the flow log names flows by: each slot's address and
// policy, and each pod address's pod. Read-only once stored.
type flowView struct {
	slots map[uint8]flowSlot
	pods  map[netip.Addr]string
}

type flowSlot struct {
	addr   netip.Addr
	policy string
}

// newFlowView is pn's view for the flow log. Queue goroutine.
func newFlowView(pn *podNode, pods []dpPod) *flowView {
	v := &flowView{slots: map[uint8]flowSlot{}, pods: map[netip.Addr]string{}}
	for id, s := range pn.slots {
		v.slots[id] = flowSlot{addr: s.address, policy: s.policy.String()}
	}
	for _, p := range pods {
		for _, ip := range []netip.Addr{p.v4, p.v6} {
			if ip.IsValid() {
				v.pods[ip] = p.name
			}
		}
	}
	return v
}

// flowLog is the running flow log: stop ends it, done closes once it has.
type flowLog struct {
	stop, done chan struct{}
}

// syncFlowLog starts the flow log while egress and spec.egress.flowLog
// are on, and stops it otherwise. Queue goroutine only.
func (a *Agent) syncFlowLog() {
	want := a.on != nil && a.on.spec.FlowLog
	switch {
	case want && a.flows == nil:
		ev, err := datapath.OpenCtEvents(flowWait)
		if err != nil {
			logging.Info(a.logger, "op", "egressFlow", "node", a.node, "error", err, "msg", "the flow log can't start")
			return
		}
		f := &flowLog{stop: make(chan struct{}), done: make(chan struct{})}
		a.flows = f
		go a.runFlowLog(ev, f)
		logging.Info(a.logger, "op", "egressFlow", "node", a.node, "msg", "flow log on")
	case !want && a.flows != nil:
		close(a.flows.stop)
		<-a.flows.done
		a.flows = nil
		logging.Info(a.logger, "op", "egressFlow", "node", a.node, "msg", "flow log off")
	}
}

// runFlowLog writes each egress flow's NEW and DESTROY event, read from
// ev, until f.stop closes. Its own goroutine; it reads only atomics.
func (a *Agent) runFlowLog(ev *datapath.CtEvents, f *flowLog) {
	defer close(f.done)
	defer ev.Close()
	for {
		select {
		case <-f.stop:
			return
		default:
		}
		events, err := ev.Read()
		if errors.Is(err, datapath.ErrCtOverflow) {
			flowOverflows.Inc()
			logging.Info(a.logger, "op", "egressFlow", "node", a.node, "msg", err.Error())
			continue
		}
		if err != nil {
			logging.Info(a.logger, "op", "egressFlow", "node", a.node, "error", err, "msg", "reading conntrack events; the flow log stops")
			return
		}
		for _, e := range events {
			a.logFlow(e)
		}
	}
}

// logFlow writes e when its ct mark carries a slot in the mark byte (D18);
// every other flow on the node is skipped.
func (a *Agent) logFlow(e datapath.CtEvent) {
	shift := uint8(a.markByte.Load())
	id := uint8(e.Mark >> shift)
	if id == 0 || id > slotBudget {
		return
	}
	v := a.flowView.Load()
	var s flowSlot
	pod := ""
	if v != nil {
		s, pod = v.slots[id], v.pods[e.Src.Addr()]
	}
	event := "destroy"
	if e.New {
		event = "new"
	}
	kv := []any{"op", "egressFlow", "node", a.node, "event", event, "proto", protoName(e.Proto), "pod", pod,
		"src", e.Src.String(), "dst", e.Dst.String(), "egress", e.Nat.String(), "slot", id, "policy", s.policy}
	if e.Counted {
		kv = append(kv, "packets", e.Packets, "bytes", e.Bytes)
	}
	if !e.Start.IsZero() && !e.Stop.IsZero() {
		kv = append(kv, "duration", e.Stop.Sub(e.Start).String())
	}
	logging.Info(a.logger, append(kv, "msg", "egress flow")...)
	flowEvents.WithLabelValues(event).Inc()
}

// protoName is an IP protocol's name, its number otherwise.
func protoName(p uint8) any {
	switch p {
	case 1:
		return "icmp"
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 58:
		return "icmpv6"
	case 132:
		return "sctp"
	}
	return p
}
