// Copyright 2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// The address guard's live state is only in each node agent's metrics:
// whether the program is loaded, where it is attached, and whether a
// restart is pending are not in the API. This is the one place the plugin
// reads agent metrics, through the API server's pod proxy.

// Guard states, most urgent first.
const (
	guardStateUnknown         = "unknown"
	guardStateRestartRequired = "restart required"
	guardStateStandingDown    = "standing down"
	guardStateNotLoaded       = "not loaded"
	guardStateNotAttached     = "not attached"
	guardStateEnforcing       = "enforcing"
	guardStateMonitoring      = "monitoring"
)

const defaultAgentMetricsPort = "7472"

// guardNodeState is the address guard as one node's agent reports it.
// Counters are cumulative since that agent started.
type guardNodeState struct {
	Node     string            `json:"node"`
	Mode     string            `json:"mode"` // configured: off, enforce or monitor
	State    string            `json:"state"`
	Loaded   bool              `json:"loaded"`
	Attached map[string]string `json:"attached,omitempty"` // interface -> hook
	// ChainPosition is where the guard runs in each tcx chain: 1 is first.
	// Above 1, other programs run before it. No XDP entries: one slot.
	ChainPosition   map[string]int     `json:"chainPosition,omitempty"`
	RestartRequired bool               `json:"restartRequired"`
	StandingDown    bool               `json:"standingDown"`
	UnguardedVIPs   map[string]float64 `json:"unguardedVIPs,omitempty"` // reason -> VIPs, non-zero only
	AttachErrors    float64            `json:"attachErrors"`
	Drops           float64            `json:"drops"`
	WouldDrops      float64            `json:"wouldDrops"`
	TopVIP          string             `json:"topVIP,omitempty"`
	TopVIPPackets   float64            `json:"topVIPPackets,omitempty"`
	Error           string             `json:"error,omitempty"`
}

// metricsFetcher returns one agent pod's metrics in the text exposition
// format.
type metricsFetcher func(ctx context.Context, pod v1.Pod) ([]byte, error)

func newGuardCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output, node string

	cmd := &cobra.Command{
		Use:   "guard",
		Short: "Address guard state on each node, read live from the node agents",
		Long: `Show what the address guard is doing on each node: the configured mode,
whether the program is loaded, the interfaces and hooks it is attached to,
whether a restart is pending or the node has stood down, and what it has
dropped. Packet counts are cumulative since each agent started.

The state is read from each lbnodeagent's metrics through the API server's
pod proxy, which needs get on pods/proxy in the PureLB namespace (see
rbac-sample.yaml).`,
		Example: `  kubectl purelb guard
  kubectl purelb guard --node node-a -o json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runGuard(cmd.Context(), c, node, format)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&node, "node", "", "Only this node")
	return cmd
}

func runGuard(ctx context.Context, c *clients, node string, format outputFormat) error {
	pods, err := listAndCategorizePureLBPods(ctx, c)
	if err != nil {
		return err
	}
	nodes, err := c.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	lbna, err := c.dynamic.Resource(gvrLBNodeAgents).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return fmt.Errorf("listing LBNodeAgents: %w", err)
	}
	agentPods := pods.lbnodeagent
	if node != "" {
		agentPods = nil
		for _, p := range pods.lbnodeagent {
			if p.Spec.NodeName == node {
				agentPods = append(agentPods, p)
			}
		}
		if len(agentPods) == 0 {
			return fmt.Errorf("no lbnodeagent pod on node %q", node)
		}
	}
	states := collectGuardStates(ctx, proxyMetricsFetcher(c), agentPods, nodes, decodeLBNodeAgents(lbna))
	if format != outputTable {
		return printStructured(format, states)
	}
	renderGuardTable(states)
	return nil
}

// proxyMetricsFetcher reads an agent's metrics through the API server's pod
// proxy.
func proxyMetricsFetcher(c *clients) metricsFetcher {
	return func(ctx context.Context, pod v1.Pod) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		body, err := c.core.CoreV1().Pods(pod.Namespace).
			ProxyGet("http", pod.Name, agentMetricsPort(pod), "metrics", nil).DoRaw(ctx)
		return body, proxyError(err, pod.Namespace)
	}
}

// proxyError names the missing permission when the pod proxy is forbidden,
// which is the common case: many clusters keep pods/proxy for admins.
func proxyError(err error, namespace string) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("forbidden: reading agent metrics needs get on pods/proxy in %s (see rbac-sample.yaml)", namespace)
	}
	return err
}

// agentMetricsPort is the lbnodeagent container's metrics port: the one
// named "monitoring", as both the manifests and the Helm chart name it.
func agentMetricsPort(pod v1.Pod) string {
	for _, ct := range pod.Spec.Containers {
		if ct.Name != "lbnodeagent" {
			continue
		}
		for _, p := range ct.Ports {
			if p.Name == "monitoring" {
				return strconv.Itoa(int(p.ContainerPort))
			}
		}
	}
	return defaultAgentMetricsPort
}

// collectGuardStates reads every agent's metrics concurrently; each
// goroutine writes only its own slot.
func collectGuardStates(ctx context.Context, fetch metricsFetcher, agentPods []v1.Pod,
	nodes *v1.NodeList, agents []*purelbv2.LBNodeAgent) []guardNodeState {
	labels := map[string]map[string]string{}
	if nodes != nil {
		for _, n := range nodes.Items {
			labels[n.Name] = n.Labels
		}
	}
	states := make([]guardNodeState, len(agentPods))
	done := make(chan struct{}, len(agentPods))
	for i, pod := range agentPods {
		go func() {
			defer func() { done <- struct{}{} }()
			mode := resolveNodeConfig(agents, labels[pod.Spec.NodeName]).Guard
			body, err := fetch(ctx, pod)
			states[i] = guardStateFrom(pod.Spec.NodeName, mode, body, err)
		}()
	}
	for range agentPods {
		<-done
	}
	sort.Slice(states, func(a, b int) bool { return states[a].Node < states[b].Node })
	return states
}

// guardStateFrom turns one agent's metrics into its guard state.
func guardStateFrom(node, mode string, body []byte, fetchErr error) guardNodeState {
	s := guardNodeState{Node: node, Mode: mode, State: guardStateUnknown}
	if fetchErr != nil {
		s.Error = fetchErr.Error()
		return s
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		s.Error = fmt.Sprintf("parsing agent metrics: %v", err)
		return s
	}
	if _, ok := fams["purelb_address_guard_loaded"]; !ok {
		s.Error = "the agent reports no address guard metrics (an lbnodeagent older than the address guard?)"
		return s
	}

	s.Loaded = guardGauge(fams, "purelb_address_guard_loaded") == 1
	s.RestartRequired = guardGauge(fams, "purelb_address_guard_restart_required") == 1
	s.StandingDown = guardGauge(fams, "purelb_address_guard_standing_down") == 1
	for _, m := range guardMetrics(fams, "purelb_address_guard_attached") {
		if m.GetGauge().GetValue() == 1 {
			if s.Attached == nil {
				s.Attached = map[string]string{}
			}
			s.Attached[metricLabel(m, "interface")] = metricLabel(m, "hook")
		}
	}
	for _, m := range guardMetrics(fams, "purelb_address_guard_chain_position") {
		if s.ChainPosition == nil {
			s.ChainPosition = map[string]int{}
		}
		s.ChainPosition[metricLabel(m, "interface")] = int(m.GetGauge().GetValue())
	}
	for _, m := range guardMetrics(fams, "purelb_address_guard_unguarded_vips") {
		if v := m.GetGauge().GetValue(); v > 0 {
			if s.UnguardedVIPs == nil {
				s.UnguardedVIPs = map[string]float64{}
			}
			s.UnguardedVIPs[metricLabel(m, "reason")] = v
		}
	}
	for _, m := range guardMetrics(fams, "purelb_address_guard_attach_errors_total") {
		s.AttachErrors += m.GetCounter().GetValue()
	}
	for _, m := range guardMetrics(fams, "purelb_address_guard_packets_total") {
		switch metricLabel(m, "action") {
		case "drop":
			s.Drops += m.GetCounter().GetValue()
		case "would_drop":
			s.WouldDrops += m.GetCounter().GetValue()
		}
	}
	for _, m := range guardMetrics(fams, "purelb_address_guard_vip_packets_total") {
		if v := m.GetCounter().GetValue(); v > s.TopVIPPackets {
			s.TopVIP, s.TopVIPPackets = metricLabel(m, "ip"), v
		}
	}

	switch {
	case s.RestartRequired:
		s.State = guardStateRestartRequired
	case s.StandingDown:
		s.State = guardStateStandingDown
	case mode == guardOff:
		s.State = guardOff
	case !s.Loaded:
		s.State = guardStateNotLoaded
	case len(s.Attached) == 0:
		s.State = guardStateNotAttached
	case mode == "monitor":
		s.State = guardStateMonitoring
	default:
		s.State = guardStateEnforcing
	}
	return s
}

func guardMetrics(fams map[string]*dto.MetricFamily, name string) []*dto.Metric {
	if f, ok := fams[name]; ok {
		return f.GetMetric()
	}
	return nil
}

func guardGauge(fams map[string]*dto.MetricFamily, name string) float64 {
	for _, m := range guardMetrics(fams, name) {
		return m.GetGauge().GetValue()
	}
	return 0
}

func metricLabel(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func renderGuardTable(states []guardNodeState) {
	w := tableWriter(os.Stdout)
	fmt.Fprintln(w, "NODE\tMODE\tSTATE\tLOADED\tATTACHED\tCHAIN\tDROPS\tTOP VIP")
	restart := false
	for _, s := range states {
		loaded, attached, chain, drops, top := "-", "-", "-", "-", "-"
		if s.Error == "" {
			loaded = map[bool]string{true: "yes", false: "no"}[s.Loaded]
			if len(s.Attached) > 0 {
				var links []string
				for iface, hook := range s.Attached {
					links = append(links, iface+"/"+hook)
				}
				sort.Strings(links)
				attached = strings.Join(links, ",")
			}
			chain = chainSummary(s.Attached, s.ChainPosition)
			drops = strconv.FormatFloat(s.Drops, 'f', 0, 64)
			if s.WouldDrops > 0 {
				drops += fmt.Sprintf(" (+%.0f would)", s.WouldDrops)
			}
			if s.TopVIP != "" {
				top = fmt.Sprintf("%s (%.0f)", s.TopVIP, s.TopVIPPackets)
			}
		}
		state := s.State
		if s.State != guardStateEnforcing && s.State != guardStateMonitoring && s.State != guardOff {
			state = strings.ToUpper(state)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Node, s.Mode, state, loaded, attached, chain, drops, top)
		restart = restart || s.RestartRequired
	}
	_ = w.Flush()
	for _, s := range states {
		if s.Error != "" {
			fmt.Printf("%s: %s\n", s.Node, s.Error)
		}
		for reason, n := range s.UnguardedVIPs {
			fmt.Printf("%s: %.0f VIP(s) not filtered (%s)\n", s.Node, n, reason)
		}
		if chainUnreported(s.Attached, s.ChainPosition) {
			fmt.Printf("%s: the agent doesn't report tcx chain position (an lbnodeagent older than chain reporting)\n", s.Node)
		}
		for _, iface := range sortedKeys(s.ChainPosition) {
			if n := s.ChainPosition[iface] - 1; n > 0 {
				fmt.Printf("%s: %d tcx program(s) run before the guard on %s and can drop or redirect VIP traffic it never sees (bpftool net show dev %s on the node)\n",
					s.Node, n, iface, iface)
			}
		}
		if s.AttachErrors > 0 {
			fmt.Printf("%s: %.0f attach error(s): an interface has no hook\n", s.Node, s.AttachErrors)
		}
	}
	if restart {
		fmt.Printf("The guard was enabled or disabled after lbnodeagent started; restart it to apply:\n"+
			"  kubectl -n %s rollout restart daemonset/lbnodeagent\n", purelbNamespace)
	}
}

// chainSummary is the CHAIN column: the guard's position in each attached
// link's tcx chain, in the ATTACHED column's order (1 = runs first). "-" for
// an XDP link (one slot, no chain), "?" when the agent doesn't report it.
func chainSummary(attached map[string]string, pos map[string]int) string {
	if len(attached) == 0 {
		return "-"
	}
	ifaces := make([]string, 0, len(attached))
	for iface := range attached {
		ifaces = append(ifaces, iface)
	}
	sort.Strings(ifaces)
	var out []string
	for _, iface := range ifaces {
		switch p, ok := pos[iface]; {
		case attached[iface] != "tcx":
			out = append(out, "-")
		case !ok:
			out = append(out, "?")
		default:
			out = append(out, strconv.Itoa(p))
		}
	}
	return strings.Join(out, ",")
}

// chainUnreported is a tcx attachment with no chain position: an agent that
// predates chain reporting.
func chainUnreported(attached map[string]string, pos map[string]int) bool {
	for iface, hook := range attached {
		if _, ok := pos[iface]; hook == "tcx" && !ok {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// guardLiveSummary is what status adds to its address guard line, and the
// warnings it raises, from the live states. An unreadable state matters only
// where the guard is configured.
func guardLiveSummary(states []guardNodeState) (note string, warnings []string) {
	restart, down, unknown := 0, 0, 0
	var firstErr string
	for _, s := range states {
		switch {
		case s.Error != "":
			if s.Mode != guardOff {
				unknown++
				if firstErr == "" {
					firstErr = s.Error
				}
			}
		case s.RestartRequired:
			restart++
		case s.StandingDown:
			down++
		}
	}
	var parts []string
	if restart > 0 {
		parts = append(parts, fmt.Sprintf("%d restart required", restart))
		warnings = append(warnings, fmt.Sprintf("%d node(s) need an lbnodeagent restart for the address guard", restart))
	}
	if down > 0 {
		parts = append(parts, fmt.Sprintf("%d standing down", down))
		warnings = append(warnings, fmt.Sprintf("%d node(s) standing down: address guard not working", down))
	}
	if unknown > 0 {
		parts = append(parts, fmt.Sprintf("live state unavailable on %d node(s): %s", unknown, firstErr))
	}
	if len(parts) > 0 {
		note = " (" + strings.Join(parts, "; ") + ")"
	}
	return note, warnings
}
