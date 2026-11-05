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
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// keyCNI decides the CNI ADDs waiting for their pods (§8.1).
const keyCNI = "agent:cni"

// cniHold is the longest an ADD waits for a selected pod's slice and
// classifier: the measured claim p99 (D17).
const cniHold = 25 * time.Second

var (
	cniRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "cni_requests_total",
		Help: "CNI plugin requests the agent answered, by operation and result (ready, held, pass, error).",
	}, []string{"op", "result"})
	cniAddSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "cni_add_duration_seconds",
		Help:    "How long the agent took to answer a CNI ADD: the wait for a selected pod's slice and classifier.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 30},
	})
)

func init() {
	prometheus.MustRegister(cniRequests, cniAddSeconds)
}

// cniSandbox is a pod sandbox as the plugin reported it: its ADD's
// addresses, until its DEL. Replaced, never changed.
type cniSandbox struct {
	namespace, name, uid string
	ips                  []netip.Addr
	deleted              bool
}

// waiter is an ADD waiting for its pod. verdict is the queue goroutine's
// latest decision, done closed once it is final.
type waiter struct {
	namespace, name, uid string
	ips                  []netip.Addr
	verdict              atomic.Pointer[cniVerdict]
	closed               atomic.Bool
	done                 chan struct{}
}

// cniVerdict is what an ADD answers now: a result, or an error that fails
// it; neither while there is nothing to answer yet.
type cniVerdict struct {
	result, err string
	final       bool
	// probe asks the plugin for the stage probe (ADD).
	probe bool
}

func (w *waiter) set(v *cniVerdict) {
	w.verdict.Store(v)
	if v != nil && v.final && w.closed.CompareAndSwap(false, true) {
		close(w.done)
	}
}

// serveCNI answers the plugin on the agent's socket while the session
// runs (D-idle: no socket while egress is off).
func (a *Agent) serveCNI(s *session) {
	l, err := cnisock.Listen(a.sockPath)
	if err != nil {
		logging.Info(a.logger, "op", "egressCNI", "node", a.node, "socket", a.sockPath, "error", err,
			"msg", "the CNI plugin can't reach the agent: new pods are judged by the snapshot")
		return
	}
	go func() {
		<-s.stop
		_ = l.Close()
	}()
	srv := cnisock.Server{Min: cnisock.MinVersion, Max: cnisock.Version, Handle: a.handleCNI}
	if err := srv.Serve(l); err != nil {
		logging.Info(a.logger, "op", "egressCNI", "node", a.node, "error", err)
	}
}

// handleCNI answers one plugin request, on the socket's goroutine: what
// it learns goes to the queue goroutine through the sandbox and waiter
// maps and the work queue.
func (a *Agent) handleCNI(req cnisock.Request) cnisock.Response {
	var ips []netip.Addr
	for _, s := range req.IPs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return a.cniAnswer(req, &cniVerdict{err: "bad pod address " + s})
		}
		ips = append(ips, ip)
	}
	if req.ContainerID == "" || req.Namespace == "" || req.Name == "" {
		return a.cniAnswer(req, &cniVerdict{err: "the request names no sandbox or pod"})
	}
	sb := &cniSandbox{namespace: req.Namespace, name: req.Name, uid: req.UID, ips: ips}
	switch req.Op {
	case cnisock.OpAdd:
		return a.cniAdd(req, sb)
	case cnisock.OpDel:
		// DEL always succeeds (D26): the pod's addresses leave the plan,
		// and with them its flows (podDeleted).
		sb.deleted = true
		a.sandboxes.Store(req.ContainerID, sb)
		a.add(keyPods, false)
		return a.cniAnswer(req, &cniVerdict{result: cnisock.ResultReady})
	case cnisock.OpProbe:
		return a.probeReport(req, ips)
	case cnisock.OpCheck:
		if v, ok := a.sandboxes.Load(req.ContainerID); ok && !v.(*cniSandbox).deleted {
			return a.cniAnswer(req, &cniVerdict{result: cnisock.ResultReady})
		}
		return a.cniAnswer(req, &cniVerdict{err: "the agent has no record of this sandbox"})
	}
	return a.cniAnswer(req, &cniVerdict{err: "unknown operation " + req.Op})
}

// cniAdd records the sandbox and waits up to cniHold for its pod's
// verdict (§8.1).
func (a *Agent) cniAdd(req cnisock.Request, sb *cniSandbox) cnisock.Response {
	start := time.Now()
	defer func() { cniAddSeconds.Observe(time.Since(start).Seconds()) }()
	a.sandboxes.Store(req.ContainerID, sb)
	if req.Namespace == metav1.NamespaceSystem || req.Namespace == a.namespace {
		// Never held (D35); the agent attaches the pod once it knows it.
		a.add(keyPods, false)
		return a.cniAnswer(req, &cniVerdict{result: cnisock.ResultPass})
	}
	w := &waiter{namespace: req.Namespace, name: req.Name, uid: req.UID, ips: sb.ips, done: make(chan struct{})}
	a.waiters.Store(req.ContainerID, w)
	defer a.waiters.Delete(req.ContainerID)
	a.add(keyPods, false)
	a.add(keyCNI, false)
	t := time.NewTimer(a.cniHold)
	defer t.Stop()
	select {
	case <-w.done:
	case <-t.C:
	}
	v := w.verdict.Load()
	if v == nil {
		v = &cniVerdict{err: "the pod's egress isn't in place yet; kubelet retries"}
	}
	return a.cniAnswer(req, v)
}

// cniAnswer is v as a response, counted and logged.
func (a *Agent) cniAnswer(req cnisock.Request, v *cniVerdict) cnisock.Response {
	result := v.result
	if v.err != "" {
		result = "error"
	}
	cniRequests.WithLabelValues(req.Op, result).Inc()
	kv := []interface{}{"op", "egressCNI", "node", a.node, "request", req.Op, "pod", req.Namespace + "/" + req.Name,
		"sandbox", req.ContainerID, "result", result}
	if v.err != "" {
		logging.Info(a.logger, append(kv, "error", v.err)...)
		return cnisock.Response{Error: v.err}
	}
	if v.probe {
		logging.Info(a.logger, append(kv, "msg", "asking the plugin for the stage probe")...)
		return cnisock.Response{Result: v.result, Probe: true}
	}
	if result == cnisock.ResultHeld {
		logging.Info(a.logger, append(kv, "msg", "its slice isn't ready: its traffic leaving the cluster is dropped until it is")...)
	} else {
		logging.Debug(a.logger, kv...)
	}
	return cnisock.Response{Result: v.result}
}

// syncCNI decides each waiting ADD from the pod-node state and the pod
// devices the datapath last tagged. Queue goroutine only.
func (a *Agent) syncCNI() {
	s, pn := a.on, a.podnode
	if s == nil || !s.synced.Load() || pn == nil {
		return
	}
	var tagged map[netip.Addr]datapath.PodDev
	if t := a.tagged.Load(); t != nil {
		tagged = *t
	}
	a.syncRecords(s, pn)
	c := a.capability.Load()
	a.waiters.Range(func(k, v any) bool {
		w := v.(*waiter)
		st := pn.pods[w.namespace+"/"+w.name]
		if a.stageGated(st, k.(string)) {
			return true // decided again when the stage is known or installed
		}
		w.set(a.askProbe(k.(string), st, cniDecide(s, pn, c, tagged, w)))
		return true
	})
}

// cniDecide is w's verdict (§8.1): ready once a selected pod's slot and
// classifier are installed; held or passed meanwhile, as its policies'
// failure policy says; nil until its device carries what the plan wants.
func cniDecide(s *session, pn *podNode, c *purelbv2.EgressCapability, tagged map[netip.Addr]datapath.PodDev,
	w *waiter) *cniVerdict {
	key := w.namespace + "/" + w.name
	obj, ok, _ := s.pods.GetByKey(key)
	if pod, isPod := obj.(*corev1.Pod); !ok || !isPod || (w.uid != "" && string(pod.UID) != w.uid) {
		if pn.failClosedNS[w.namespace] {
			return nil // not seen yet, and may be one a FailClosed policy selects
		}
		return &cniVerdict{result: cnisock.ResultPass, final: true}
	}
	st, ok := pn.pods[key]
	if !ok || !st.selected {
		return &cniVerdict{result: cnisock.ResultReady, final: true}
	}
	if c == nil {
		return nil // the datapath's first probe is still running
	}
	if !c.EgressCapable {
		if st.hold {
			return &cniVerdict{err: "this node can't run egress, and a FailClosed policy selects the pod (D29)", final: true}
		}
		return &cniVerdict{result: cnisock.ResultPass, final: true}
	}
	for _, ip := range w.ips {
		if d, ok := tagged[ip]; !ok || d.PodSet != st.podSet || d.Hold != st.hold {
			return nil
		}
	}
	switch {
	case st.hold:
		// Held until its slot is Active; for good when it has no pod-set
		// (the slot budget is exhausted).
		return &cniVerdict{result: cnisock.ResultHeld, final: !st.pending}
	case st.pending:
		return &cniVerdict{result: cnisock.ResultPass}
	}
	return &cniVerdict{result: cnisock.ResultReady, final: true}
}

// sandboxInput is what the plugin reported, for planPodNode: the
// addresses of pods with a live sandbox, and the pods whose sandboxes were
// all deleted, each matched to its pod by UID (a pod recreated under the
// same name is another). Records of deleted sandboxes whose addresses
// their pod no longer has, or of pods the API no longer has, are dropped.
// Queue goroutine only.
func (a *Agent) sandboxInput(pods []*corev1.Pod) (map[string][]netip.Addr, map[string]bool) {
	byUID, byName := map[string]*corev1.Pod{}, map[string]*corev1.Pod{}
	for _, p := range pods {
		byUID[string(p.UID)], byName[p.Namespace+"/"+p.Name] = p, p
	}
	ips, ended := map[string][]netip.Addr{}, map[string]bool{}
	a.sandboxes.Range(func(k, v any) bool {
		sb := v.(*cniSandbox)
		name := sb.namespace + "/" + sb.name
		pod, ok := byUID[sb.uid]
		if sb.uid == "" {
			pod, ok = byName[name]
		}
		if sb.deleted && (!ok || !slices.ContainsFunc(podIPs(pod), func(ip netip.Addr) bool { return slices.Contains(sb.ips, ip) })) {
			a.sandboxes.Delete(k)
			return true
		}
		switch {
		case !ok:
			// A live sandbox of a pod not seen yet: its informer event
			// plans it.
		case sb.deleted:
			if _, live := ips[name]; !live {
				ended[name] = true
			}
		default:
			ips[name] = append(ips[name], sb.ips...)
			delete(ended, name)
		}
		return true
	})
	return ips, ended
}
