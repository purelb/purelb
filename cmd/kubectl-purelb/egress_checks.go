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

package main

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"purelb.io/internal/egress"
	"purelb.io/internal/egress/diagreport"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var (
	gvrCalicoPools = schema.GroupVersionResource{Group: "crd.projectcalico.org", Version: "v1", Resource: "ippools"}
)

// egressInUse reports whether any LBNodeAgent enables egress.
func egressInUse(agents []*purelbv2.LBNodeAgent) bool {
	for _, a := range agents {
		if a.Spec.Egress != nil && a.Spec.Egress.Enabled {
			return true
		}
	}
	return false
}

// egressSummary is the status line's numbers.
type egressSummary struct {
	Policies int `json:"policies"`
	NotReady int `json:"notReady"`
	Nodes    int `json:"nodes"`
	// Stages counts nodes by pod stage.
	Stages map[string]int `json:"stages,omitempty"`
	// Faults are the nodes' conditions False and address faults.
	Faults int `json:"faults"`
}

// readEgressSummary is nil where egress isn't installed or isn't readable (an
// install without it, or a plugin role older than it).
func readEgressSummary(ctx context.Context, c *clients) *egressSummary {
	es, err := listTyped[purelbv2.Egress](ctx, c, gvrEgresses, "")
	if err != nil {
		return nil
	}
	cs, err := listTyped[purelbv2.ClusterEgress](ctx, c, gvrClusterEgresses, "")
	if err != nil {
		return nil
	}
	states, err := listTyped[purelbv2.EgressNodeState](ctx, c, gvrEgressNodeStates, "")
	if err != nil {
		return nil
	}
	if len(es) == 0 && len(cs) == 0 && len(states) == 0 {
		return nil
	}
	s := &egressSummary{Policies: len(es) + len(cs), Nodes: len(states), Stages: map[string]int{}}
	notReady := func(st purelbv2.EgressStatus) {
		if !apimeta.IsStatusConditionTrue(st.Conditions, "Ready") {
			s.NotReady++
		}
	}
	for i := range es {
		notReady(es[i].Status)
	}
	for i := range cs {
		notReady(cs[i].Status)
	}
	for i := range states {
		st := &states[i].Status
		if st.Capability != nil && st.Capability.PodNodeStage != "" {
			s.Stages[st.Capability.PodNodeStage]++
		}
		s.Faults += len(st.AddressFaults)
		for _, c := range st.Conditions {
			if c.Status == metav1.ConditionFalse {
				s.Faults++
			}
		}
	}
	return s
}

// String is the status line.
func (s *egressSummary) String() string {
	var stages []string
	for k, n := range s.Stages {
		stages = append(stages, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(stages)
	line := fmt.Sprintf("%d policies, %d not ready | %d nodes", s.Policies, s.NotReady, s.Nodes)
	if len(stages) > 0 {
		line += " (" + strings.Join(stages, ", ") + ")"
	}
	return line + fmt.Sprintf(" | %d fault(s)", s.Faults)
}

// serviceEgress names the Egress policies using a Service's addresses
// (serviceRef, always in the policy's namespace).
func serviceEgress(ctx context.Context, c *clients, namespace, name string) []string {
	eps, err := listTyped[purelbv2.EgressEndpoints](ctx, c, gvrEgressEndpoints, purelbNamespace)
	if err != nil {
		return nil
	}
	es, err := listTyped[purelbv2.Egress](ctx, c, gvrEgresses, namespace)
	if err != nil {
		return nil
	}
	var out []string
	for i := range eps {
		ep := &eps[i]
		addr, err := netip.ParseAddr(ep.Spec.Address)
		if err != nil || ep.Spec.SharedWith == nil || ep.Spec.SharedWith.Name != name {
			continue
		}
		for j := range es {
			ref := egress.PolicyRef{Kind: egress.KindEgress, Namespace: namespace, Name: es[j].Name}
			if ep.Name == egress.EndpointsName(ref, addr) {
				out = append(out, ep.Spec.Address+" by "+ref.String())
			}
		}
	}
	sort.Strings(out)
	return out
}

// egressChecks are validate's: what the API shows, and with live each node's
// own view through lbnodeagent diag.
func egressChecks(ctx context.Context, c *clients, agents []*purelbv2.LBNodeAgent, live bool) []checkResult {
	if !egressInUse(agents) {
		return nil
	}
	var checks []checkResult
	add := func(level, format string, args ...any) {
		checks = append(checks, checkResult{level, fmt.Sprintf(format, args...)})
	}
	checks = append(checks, egressPolicyChecks(ctx, c)...)
	states, err := listTyped[purelbv2.EgressNodeState](ctx, c, gvrEgressNodeStates, "")
	if err != nil {
		add("WARN", "Egress: can't read EgressNodeStates: %v", reason(err))
	} else {
		for i := range states {
			for _, cond := range states[i].Status.Conditions {
				if cond.Status == metav1.ConditionFalse {
					add(nodeConditionLevel(cond), "Egress on %s: %s=False %s: %s", states[i].Name, cond.Type, cond.Reason, cond.Message)
				}
			}
		}
	}
	if ds, err := c.core.AppsV1().DaemonSets(purelbNamespace).Get(ctx, "purelb-cni", metav1.GetOptions{}); err != nil {
		add("FAIL", "Egress: the purelb-cni DaemonSet: %v", reason(err))
	} else if ds.Status.NumberReady != ds.Status.DesiredNumberScheduled {
		add("WARN", "Egress: purelb-cni is ready on %d of %d nodes", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
	} else {
		paths := []string{}
		for _, v := range ds.Spec.Template.Spec.Volumes {
			if v.HostPath != nil && strings.HasPrefix(v.Name, "cni-") {
				paths = append(paths, v.HostPath.Path)
			}
		}
		add("PASS", "Egress: purelb-cni is ready on %d nodes, using %s", ds.Status.NumberReady, strings.Join(paths, " and "))
	}
	if cm, err := c.core.CoreV1().ConfigMaps("kube-system").Get(ctx, "kube-proxy", metav1.GetOptions{}); err == nil {
		if strings.Contains(cm.Data["config.conf"], "mode: ipvs") {
			add("FAIL", "Egress: kube-proxy runs in IPVS mode, which egress doesn't support")
		}
	}
	checks = append(checks, bfdChecks(ctx, c)...)
	checks = append(checks, calicoChecks(ctx, c)...)
	if !live {
		add("PASS", "Egress: node checks skipped: --live runs them through kubectl exec")
		return checks
	}
	return append(checks, egressLiveChecks(ctx, c)...)
}

// nodeConditionLevel is how bad a node's False condition is: a host setting it
// reports, or something egress keeps working through, is a warning.
func nodeConditionLevel(c metav1.Condition) string {
	if c.Reason == "ArpAnnounce" || c.Type == "UnderlayEncrypted" || c.Type == "TunnelPeersReachable" {
		return "WARN"
	}
	return "FAIL"
}

func egressPolicyChecks(ctx context.Context, c *clients) []checkResult {
	var checks []checkResult
	report := func(kind, name string, st purelbv2.EgressStatus) {
		ready := apimeta.FindStatusCondition(st.Conditions, "Ready")
		if ready != nil && ready.Status == metav1.ConditionTrue {
			checks = append(checks, checkResult{"PASS", fmt.Sprintf("%s %s: Ready", kind, name)})
			return
		}
		var why []string
		for _, cond := range st.Conditions {
			if cond.Status == metav1.ConditionFalse && cond.Type != "Ready" {
				why = append(why, cond.Type+": "+cond.Reason)
			}
		}
		// A policy another outranks does what it was told, wherever it isn't
		// outranked: worth a look, but nothing is broken.
		level := "FAIL"
		if len(why) > 0 && !slices.ContainsFunc(why, func(w string) bool { return !strings.HasPrefix(w, "PolicyEffective:") }) {
			level = "WARN"
		}
		checks = append(checks, checkResult{level, fmt.Sprintf("%s %s: not Ready (%s)", kind, name, strings.Join(why, ", "))})
	}
	es, err := listTyped[purelbv2.Egress](ctx, c, gvrEgresses, "")
	if err != nil {
		return []checkResult{{"WARN", "Egress: can't read policies: " + reason(err)}}
	}
	for i := range es {
		report(egress.KindEgress, es[i].Namespace+"/"+es[i].Name, es[i].Status)
	}
	cs, err := listTyped[purelbv2.ClusterEgress](ctx, c, gvrClusterEgresses, "")
	if err != nil {
		return append(checks, checkResult{"WARN", "Egress: can't read ClusterEgresses: " + reason(err)})
	}
	for i := range cs {
		report(egress.KindClusterEgress, cs[i].Name, cs[i].Status)
	}
	return checks
}

// bfdChecks warn about BFD on a routed egress install: k8gobgp's BFD doesn't
// recover after a session drop and then flaps BGP (purelb/gobgp-netlink#65).
func bfdChecks(ctx context.Context, c *clients) []checkResult {
	list, err := c.dynamic.Resource(gvrBGPConfigurations).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil // no BGP in this install
	}
	var checks []checkResult
	for i := range list.Items {
		neighbors, _, _ := unstructured.NestedSlice(list.Items[i].Object, "spec", "neighbors")
		for _, n := range neighbors {
			m, ok := n.(map[string]any)
			if !ok {
				continue
			}
			if on, _, _ := unstructured.NestedBool(m, "bfd", "enabled"); on {
				checks = append(checks, checkResult{"WARN", fmt.Sprintf(
					"BGP config %s enables BFD: k8gobgp's BFD doesn't recover after a session drop and then flaps BGP (purelb/gobgp-netlink#65), so routed egress addresses can flap",
					list.Items[i].GetName())})
				return checks
			}
		}
	}
	return checks
}

// calicoChecks report natOutgoing on Calico's pools: on Calico's eBPF dataplane
// the selected pods' pools on a node must agree (design §10, resolution 20).
func calicoChecks(ctx context.Context, c *clients) []checkResult {
	list, err := c.dynamic.Resource(gvrCalicoPools).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
			return []checkResult{{"WARN", "Egress: can't read Calico's IP pools: " + reason(err)}}
		}
		return nil
	}
	var on, off []string
	for i := range list.Items {
		nat, _, _ := unstructured.NestedBool(list.Items[i].Object, "spec", "natOutgoing")
		if nat {
			on = append(on, list.Items[i].GetName())
		} else {
			off = append(off, list.Items[i].GetName())
		}
	}
	switch {
	case len(on) > 0 && len(off) > 0:
		return []checkResult{{"WARN", fmt.Sprintf(
			"Egress on Calico: natOutgoing is on for %s and off for %s: on Calico's eBPF dataplane, selected pods on a node must share the setting",
			strings.Join(on, ", "), strings.Join(off, ", "))}}
	case len(on)+len(off) > 0:
		return []checkResult{{"PASS", fmt.Sprintf("Egress on Calico: natOutgoing is %v on all %d IP pools", len(on) > 0, len(on)+len(off))}}
	}
	return nil
}

// egressLiveChecks read each node's own view.
func egressLiveChecks(ctx context.Context, c *clients) []checkResult {
	pods, err := listAndCategorizePureLBPods(ctx, c)
	if err != nil {
		return []checkResult{{"WARN", "Egress: listing the agents: " + reason(err)}}
	}
	var checks []checkResult
	for _, p := range agentPerNode(pods.lbnodeagent) {
		rep, err := runDiag(ctx, c, &p)
		if err != nil {
			checks = append(checks, checkResult{"WARN", fmt.Sprintf("Egress on %s: can't read the node: %v", p.Spec.NodeName, err)})
			continue
		}
		checks = append(checks, nodeChecks(p.Spec.NodeName, rep.Checks)...)
	}
	return checks
}

// nodeChecks turn one node's report into check results.
func nodeChecks(node string, n diagreport.NodeChecks) []checkResult {
	var checks []checkResult
	add := func(level, format string, args ...any) {
		checks = append(checks, checkResult{level, "Egress on " + node + ": " + fmt.Sprintf(format, args...)})
	}
	switch {
	case !n.KernelConfigKnown:
		add("WARN", "kernel %s has no /proc/config.gz: its config can't be checked here", n.Kernel)
	case len(n.KernelConfigMissing) > 0:
		add("FAIL", "kernel %s lacks %s", n.Kernel, strings.Join(n.KernelConfigMissing, ", "))
	default:
		add("PASS", "kernel %s has the config egress needs", n.Kernel)
	}
	if n.BPFFS {
		add("PASS", "bpffs is mounted")
	} else {
		add("WARN", "bpffs isn't mounted at /sys/fs/bpf/purelb: egress can't pin, so a restarted agent loses its state")
	}
	if n.Forwarding4 != "1" {
		add("FAIL", "net.ipv4.conf.all.forwarding is %s: egress needs 1", n.Forwarding4)
	}
	if n.Forwarding6 != "1" {
		add("WARN", "net.ipv6.conf.all.forwarding is %s: IPv6 egress needs 1", n.Forwarding6)
	}
	for _, i := range n.Interfaces {
		if i.ArpAnnounce != "2" {
			add("WARN", "%s: net.ipv4.conf.%s.arp_announce is %s: egress needs 2 on pool interfaces", i.Name, i.Name, i.ArpAnnounce)
		}
		if n.Forwarding6 == "1" && i.AcceptRA == "1" {
			add("WARN", "%s: accept_ra is 1 with IPv6 forwarding on, so router advertisements are ignored: use 2 if its default route comes from them", i.Name)
		}
	}
	for _, ch := range n.Chains {
		if ch.Guard != 0 && ch.Announce != 0 && ch.Guard > ch.Announce {
			add("FAIL", "%s: pl_announce runs ahead of the address guard", ch.Interface)
		}
	}
	if slices.ContainsFunc(n.Chains, func(ch diagreport.Chain) bool { return ch.Guard != 0 && ch.Announce != 0 && ch.Guard < ch.Announce }) {
		add("PASS", "the address guard runs ahead of pl_announce")
	}
	return checks
}
