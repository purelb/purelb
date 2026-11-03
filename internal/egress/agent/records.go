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

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// reasonUnrecorded is the Warning event on a pod whose sandbox started
// without the plugin.
const reasonUnrecorded = "EgressSandboxUnrecorded"

var cniUnrecorded = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "cni_unrecorded_pods",
	Help: "Pods on this node whose sandbox started without the CNI plugin (before it was chained, or in the CNI boot window): their first packets weren't held or classified.",
})

func init() {
	prometheus.MustRegister(cniUnrecorded)
}

// syncRecords reports this node's pod sandboxes the plugin has no record
// of (design 8.1): pods that predate the plugin, and any started in the
// CNI boot window. A pod counts once its address is on a device the
// datapath found: while a rebooted node recreates its sandboxes, the pods'
// status keeps their old addresses. Each is reported once a session (an
// Info log and a Warning event on the pod) and counted while it runs; the
// datapath attaches it as it does every pod. The plugin's records are read
// when a pod has none the socket told of: at the session's start, and for
// sandboxes started while no agent answered. Queue goroutine only.
func (a *Agent) syncRecords(s *session, pn *podNode) {
	var tagged map[netip.Addr]datapath.PodDev
	if t := a.tagged.Load(); t != nil {
		tagged = *t
	}
	var wired []*corev1.Pod
	present := map[types.UID]bool{}
	for _, o := range s.pods.List() {
		pod := o.(*corev1.Pod)
		present[pod.UID] = true
		if pod.Spec.NodeName != a.node || !onNode(pod) || pod.Spec.HostNetwork || pod.DeletionTimestamp != nil {
			continue
		}
		if slices.ContainsFunc(podIPs(pod), func(ip netip.Addr) bool { _, ok := tagged[ip]; return ok }) {
			wired = append(wired, pod)
		}
	}
	missing := a.unrecorded(wired)
	if slices.ContainsFunc(missing, func(p *corev1.Pod) bool { return !s.reported[p.UID] }) && a.loadRecords() {
		a.add(keyPods, false) // their sandboxes' addresses
		missing = a.unrecorded(missing)
	}
	for _, pod := range missing {
		if s.reported[pod.UID] {
			continue
		}
		s.reported[pod.UID] = true
		name := pod.Namespace + "/" + pod.Name
		logging.Info(a.logger, "op", "egressCNI", "node", a.node, "pod", name, "uid", pod.UID, "ips", podIPs(pod),
			"selected", pn.pods[name].selected, "msg",
			"the pod's sandbox started without the CNI plugin: its first packets weren't held or classified; egress applies to it from now on")
		if a.events != nil {
			a.events.Eventf(pod, corev1.EventTypeWarning, reasonUnrecorded,
				"The pod's sandbox started without PureLB's CNI plugin (before the plugin was in the CNI chain, or in the CNI boot window): "+
					"its first packets weren't held or classified. PureLB egress applies to it from now on.")
		}
	}
	for uid := range s.reported {
		if !present[uid] {
			delete(s.reported, uid)
		}
	}
	cniUnrecorded.Set(float64(len(missing)))
}

// unrecorded are the pods none of whose addresses a live sandbox the
// plugin reported has: matched by UID (by name when the plugin had none).
func (a *Agent) unrecorded(pods []*corev1.Pod) []*corev1.Pod {
	type key struct {
		uid string
		ip  netip.Addr
	}
	have := map[key]bool{}
	a.sandboxes.Range(func(_, v any) bool {
		sb := v.(*cniSandbox)
		if !sb.deleted {
			id := sb.uid
			if id == "" {
				id = sb.namespace + "/" + sb.name
			}
			for _, ip := range sb.ips {
				have[key{id, ip}] = true
			}
		}
		return true
	})
	var out []*corev1.Pod
	for _, pod := range pods {
		if !slices.ContainsFunc(podIPs(pod), func(ip netip.Addr) bool {
			return have[key{string(pod.UID), ip}] || have[key{pod.Namespace + "/" + pod.Name, ip}]
		}) {
			out = append(out, pod)
		}
	}
	return out
}

// loadRecords adds the sandboxes the plugin recorded to those the socket
// told of, reporting whether there were any it didn't know. Queue
// goroutine only.
func (a *Agent) loadRecords() bool {
	recs, err := cnisock.ReadRecords(a.recordDir)
	if err != nil {
		logging.Info(a.logger, "op", "egressCNI", "node", a.node, "records", a.recordDir, "error", err,
			"msg", "reading the CNI plugin's sandbox records")
		return false
	}
	added := false
	for id, r := range recs {
		var ips []netip.Addr
		for _, s := range r.IPs {
			if ip, err := netip.ParseAddr(s); err == nil {
				ips = append(ips, ip)
			}
		}
		// A sandbox the socket told of stays as it said (a DEL since).
		if _, loaded := a.sandboxes.LoadOrStore(id, &cniSandbox{namespace: r.Namespace, name: r.Name, uid: r.UID, ips: ips}); !loaded {
			added = true
		}
	}
	logging.Debug(a.logger, "op", "egressCNI", "node", a.node, "records", len(recs), "added", added, "msg", "sandbox records read")
	return added
}
