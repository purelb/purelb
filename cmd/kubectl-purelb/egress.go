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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/utils/ptr"

	"purelb.io/internal/egress"
	"purelb.io/internal/egress/diagreport"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func egressGVR(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "purelb.io", Version: "v2", Resource: resource}
}

var (
	gvrEgresses         = egressGVR("egresses")
	gvrClusterEgresses  = egressGVR("clusteregresses")
	gvrEgressEndpoints  = egressGVR("egressendpoints")
	gvrEgressMembers    = egressGVR("egressmembers")
	gvrEgressNodeStates = egressGVR("egressnodestates")
)

// egressFinalizer is the finalizer the allocator puts on policies and
// EgressEndpoints (D5).
const egressFinalizer = "purelb.io/egress"

// diagBinary is the agent's binary in its image.
const diagBinary = "/ko-app/lbnodeagent"

func newEgressCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Trace, dump and repair egress",
	}
	cmd.AddCommand(newEgressTraceCmd(flags), newEgressDumpCmd(flags), newEgressCleanupCmd(flags), newEgressReleaseCmd(flags))
	return cmd
}

// listTyped lists a resource as T.
func listTyped[T any](ctx context.Context, c *clients, gvr schema.GroupVersionResource, namespace string) ([]T, error) {
	res := c.dynamic.Resource(gvr)
	var list *unstructured.UnstructuredList
	var err error
	if namespace != "" {
		list, err = res.Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		list, err = res.List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", gvr.Resource, err)
	}
	out := make([]T, 0, len(list.Items))
	for i := range list.Items {
		var v T
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &v); err != nil {
			return nil, fmt.Errorf("reading %s %s: %w", gvr.Resource, list.Items[i].GetName(), err)
		}
		out = append(out, v)
	}
	return out, nil
}

// flowDst is a trace's destination.
type flowDst struct {
	addr  netip.Addr
	port  uint16
	proto string
}

// parseDst reads 203.0.113.50:443/tcp, [2001:db8::1]:443/udp or a bare
// address; the protocol defaults to TCP.
func parseDst(s string) (flowDst, error) {
	d := flowDst{proto: "TCP"}
	if addr, proto, ok := strings.Cut(s, "/"); ok {
		s, d.proto = addr, strings.ToUpper(proto)
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		d.addr, d.port = ap.Addr().Unmap(), ap.Port()
		return d, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return d, fmt.Errorf("bad destination %q: want address[:port][/protocol]", s)
	}
	d.addr = a.Unmap()
	return d, nil
}

// destinationsMatch reports whether a flow to d is covered: inside an include
// and no exclude, as the datapath's policy chains decide (D24).
func destinationsMatch(dests purelbv2.EgressDestinations, d flowDst) bool {
	in := func(rules []purelbv2.EgressDestination) bool {
		for _, r := range rules {
			if destinationRuleMatches(r, d) {
				return true
			}
		}
		return false
	}
	return in(dests.Include) && !in(dests.Exclude)
}

func destinationRuleMatches(r purelbv2.EgressDestination, d flowDst) bool {
	hit := false
	for _, c := range r.CIDRs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(d.addr) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	if len(r.Ports) == 0 {
		return true
	}
	for _, p := range r.Ports {
		last := p.Port
		if p.EndPort != nil {
			last = *p.EndPort
		}
		if (p.Protocol == "" || strings.EqualFold(p.Protocol, d.proto)) && int32(d.port) >= p.Port && int32(d.port) <= last {
			return true
		}
	}
	return false
}

// tracePolicy is one policy selecting the pod.
type tracePolicy struct {
	Policy   string `json:"policy"`
	Priority int32  `json:"priority"`
	Matches  bool   `json:"matchesDestination"`
}

// traceLive is what one node's datapath holds for the flow.
type traceLive struct {
	Node       string              `json:"node"`
	Role       string              `json:"role"`
	Stage      string              `json:"stage,omitempty"`
	Held       bool                `json:"held,omitempty"`
	SliceOwner string              `json:"sliceOwner,omitempty"`
	Traffic    *diagreport.Traffic `json:"traffic,omitempty"`
	Error      string              `json:"error,omitempty"`
}

// traceResult is the answer to "what happens to this flow".
type traceResult struct {
	Pod         string        `json:"pod"`
	Node        string        `json:"node"`
	Destination string        `json:"destination"`
	Policies    []tracePolicy `json:"policies,omitempty"`
	// Winner is the policy supplying the flow's slot; none means the CNI's
	// normal path.
	Winner        string   `json:"winner,omitempty"`
	FailurePolicy string   `json:"failurePolicy,omitempty"`
	Address       string   `json:"address,omitempty"`
	Ports         string   `json:"ports,omitempty"`
	SliceState    string   `json:"sliceState,omitempty"`
	Stage         string   `json:"stage,omitempty"`
	Holders       []string `json:"holders,omitempty"`
	// Route is direct, tunnel or none.
	Route string      `json:"route,omitempty"`
	Notes []string    `json:"notes,omitempty"`
	Live  []traceLive `json:"live,omitempty"`
}

// policyOf is a policy reduced to what trace needs.
type policyOf struct {
	ref      egress.PolicyRef
	spec     purelbv2.EgressPolicy
	group    string
	priority int32
	created  time.Time
	uid      string
}

// allPolicies lists Egresses and ClusterEgresses.
func allPolicies(ctx context.Context, c *clients) ([]policyOf, error) {
	es, err := listTyped[purelbv2.Egress](ctx, c, gvrEgresses, "")
	if err != nil {
		return nil, err
	}
	cs, err := listTyped[purelbv2.ClusterEgress](ctx, c, gvrClusterEgresses, "")
	if err != nil {
		return nil, err
	}
	out := make([]policyOf, 0, len(es)+len(cs))
	for i := range es {
		e := &es[i]
		out = append(out, policyOf{ref: egress.PolicyRef{Kind: egress.KindEgress, Namespace: e.Namespace, Name: e.Name},
			spec: e.Spec.EgressPolicy, group: e.Spec.ServiceGroup, priority: e.Spec.Priority,
			created: e.CreationTimestamp.Time, uid: string(e.UID)})
	}
	for i := range cs {
		e := &cs[i]
		out = append(out, policyOf{ref: egress.PolicyRef{Kind: egress.KindClusterEgress, Name: e.Name},
			spec: e.Spec.EgressPolicy, group: e.Spec.ServiceGroup, priority: e.Spec.Priority,
			created: e.CreationTimestamp.Time, uid: string(e.UID)})
	}
	return out, nil
}

// selectsPod reports whether any of the policy's EgressMembers lists the pod.
func selectsPod(members map[string]*purelbv2.EgressMembers, p policyOf, pod *v1.Pod) bool {
	for i := 0; ; i++ {
		m, ok := members[egress.MembersName(p.ref, i)]
		if !ok {
			return false
		}
		for _, n := range m.Spec.Nodes {
			for _, mp := range n.Pods {
				ns := mp.Namespace
				if ns == "" {
					ns = p.ref.Namespace
				}
				if ns == pod.Namespace && mp.Name == pod.Name && mp.UID == string(pod.UID) {
					return true
				}
			}
		}
	}
}

func newEgressTraceCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output, dst string
	var live bool
	cmd := &cobra.Command{
		Use:   "trace <namespace/pod> --dst <address[:port][/protocol]>",
		Short: "Show what egress does with a pod's flow",
		Long: `Show which policy gives a pod's flow its address, the address and ports it
leaves by, the nodes holding that address and whether the flow goes direct or
through the tunnel. Destinations inside the cluster's own networks never use
egress, whatever a policy says.

It is computed from API objects. With --live it also reads the pod's node's and
the holders' datapaths, through kubectl exec, which needs create on pods/exec in
the PureLB namespace (see rbac-sample.yaml).`,
		Example: `  kubectl purelb egress trace voice/sip-0 --dst 203.0.113.50:443/tcp
  kubectl purelb egress trace voice/sip-0 --dst [2001:db8::50]:53/udp --live -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			ns, name, ok := strings.Cut(args[0], "/")
			if !ok {
				return fmt.Errorf("want <namespace/pod>, got %q", args[0])
			}
			d, err := parseDst(dst)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			r, err := runTrace(cmd.Context(), c, ns, name, d, live)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(format, r)
			}
			renderTrace(r)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&dst, "dst", "", "Destination: address[:port][/protocol]")
	cmd.Flags().BoolVar(&live, "live", false, "Also read the nodes' datapaths through kubectl exec")
	_ = cmd.MarkFlagRequired("dst")
	return cmd
}

func runTrace(ctx context.Context, c *clients, namespace, name string, d flowDst, live bool) (*traceResult, error) {
	pod, err := c.core.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting the pod: %w", err)
	}
	r := &traceResult{Pod: namespace + "/" + name, Node: pod.Spec.NodeName, Destination: d.String()}
	if pod.Spec.HostNetwork {
		r.Notes = append(r.Notes, "a host-network pod: egress doesn't apply")
		return r, nil
	}
	policies, err := allPolicies(ctx, c)
	if err != nil {
		return nil, err
	}
	memberList, err := listTyped[purelbv2.EgressMembers](ctx, c, gvrEgressMembers, purelbNamespace)
	if err != nil {
		return nil, err
	}
	members := map[string]*purelbv2.EgressMembers{}
	for i := range memberList {
		members[memberList[i].Name] = &memberList[i]
	}
	var selecting []policyOf
	for _, p := range policies {
		if selectsPod(members, p, pod) {
			selecting = append(selecting, p)
		}
	}
	slices.SortFunc(selecting, func(a, b policyOf) int {
		if egress.Before(egress.Rank{Priority: a.priority, Created: a.created, UID: a.uid},
			egress.Rank{Priority: b.priority, Created: b.created, UID: b.uid}) {
			return -1
		}
		return 1
	})
	var winner *policyOf
	for i := range selecting {
		p := &selecting[i]
		m := destinationsMatch(p.spec.Destinations, d)
		r.Policies = append(r.Policies, tracePolicy{Policy: p.ref.String(), Priority: p.priority, Matches: m})
		if m && winner == nil {
			winner = p
		}
	}
	if winner == nil {
		r.Notes = append(r.Notes, "no policy covers this flow: it takes the CNI's normal path")
		return r, nil
	}
	r.Winner, r.FailurePolicy = winner.ref.String(), winner.spec.FailurePolicy
	if err := traceAddress(ctx, c, r, winner, d, pod.Spec.NodeName); err != nil {
		return nil, err
	}
	if live && r.Address != "" {
		traceLiveNodes(ctx, c, r)
	}
	return r, nil
}

// traceAddress fills in the address, slice, holders and route on the pod's node.
func traceAddress(ctx context.Context, c *clients, r *traceResult, p *policyOf, d flowDst, node string) error {
	states, err := listTyped[purelbv2.EgressNodeState](ctx, c, gvrEgressNodeStates, "")
	if err != nil {
		return err
	}
	var state *purelbv2.EgressNodeState
	for i := range states {
		if states[i].Name == node {
			state = &states[i]
		}
	}
	if state == nil {
		r.Notes = append(r.Notes, "no EgressNodeState for "+node+": egress isn't enabled there")
		return nil
	}
	if state.Status.Capability != nil {
		r.Stage = state.Status.Capability.PodNodeStage
	}
	eps, err := listTyped[purelbv2.EgressEndpoints](ctx, c, gvrEgressEndpoints, purelbNamespace)
	if err != nil {
		return err
	}
	for i := range eps {
		ep := &eps[i]
		addr, err := netip.ParseAddr(ep.Spec.Address)
		if err != nil || addr.Is4() != d.addr.Is4() || ep.Name != egress.EndpointsName(p.ref, addr) {
			continue
		}
		for _, s := range state.Spec.Slices {
			if s.Endpoints != ep.Name {
				continue
			}
			r.Address, r.SliceState = ep.Spec.Address, s.State
			if s.Identity != "" || s.SliceSize == 0 {
				r.Ports = "1-65535"
			} else {
				r.Ports = fmt.Sprintf("%d-%d", s.Index*s.SliceSize, (s.Index+1)*s.SliceSize-1)
			}
		}
		if r.Address == "" {
			continue
		}
		r.Holders = ep.Status.ObservedAnnouncers
		switch {
		case len(r.Holders) == 0:
			r.Route = "none"
			r.Notes = append(r.Notes, "no node announces the address")
		case slices.Contains(r.Holders, node):
			r.Route = "direct"
		default:
			r.Route = "tunnel"
		}
		return nil
	}
	r.Notes = append(r.Notes, fmt.Sprintf("%s has no slice of %s's address in this family yet", node, r.Winner))
	return nil
}

// runDiag reads one agent pod's datapath.
func runDiag(ctx context.Context, c *clients, pod *v1.Pod) (*diagreport.Report, error) {
	var stdout, stderr bytes.Buffer
	if err := execContainer(ctx, c, pod, "lbnodeagent", []string{diagBinary, "diag"}, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var r diagreport.Report
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return nil, fmt.Errorf("reading the report: %w", err)
	}
	return &r, nil
}

func traceLiveNodes(ctx context.Context, c *clients, r *traceResult) {
	pods, err := listAndCategorizePureLBPods(ctx, c)
	if err != nil {
		r.Notes = append(r.Notes, "live: "+err.Error())
		return
	}
	nodes := []string{r.Node}
	for _, h := range r.Holders {
		if h != r.Node {
			nodes = append(nodes, h)
		}
	}
	for _, n := range nodes {
		role := "holder"
		if n == r.Node {
			role = "pod node"
		}
		l := traceLive{Node: n, Role: role}
		for i := range pods.lbnodeagent {
			if p := &pods.lbnodeagent[i]; p.Spec.NodeName == n {
				if rep, err := runDiag(ctx, c, p); err != nil {
					l.Error = err.Error()
				} else {
					fillLive(&l, rep, r.Address, r.Ports)
				}
				break
			}
		}
		r.Live = append(r.Live, l)
	}
}

// fillLive picks the flow's address out of a node's report, naming the owner
// of the slice its ports are in by node when the report's nodes include it.
func fillLive(l *traceLive, rep *diagreport.Report, addr, ports string) {
	l.Stage = rep.Stage
	for i := range rep.Addresses {
		a := &rep.Addresses[i]
		if a.Address != addr {
			continue
		}
		l.Held, l.Traffic = a.Held, &a.Traffic
		first, _, _ := strings.Cut(ports, "-")
		port, _ := strconv.Atoi(first)
		for _, s := range a.Slices {
			if a.SliceSize != 0 && int(s.Index) == port/int(a.SliceSize) {
				l.SliceOwner = fmt.Sprintf("%#x", s.Owner)
				if s.Owner == diagreport.NodeID(l.Node) {
					l.SliceOwner = l.Node
				}
			}
		}
		return
	}
	l.Error = "the datapath doesn't have " + addr
}

func (d flowDst) String() string {
	if d.port == 0 {
		return d.addr.String() + "/" + d.proto
	}
	return netip.AddrPortFrom(d.addr, d.port).String() + "/" + d.proto
}

func renderTrace(r *traceResult) {
	w := tableWriter(os.Stdout)
	fmt.Fprintf(w, "Pod:\t%s on %s\n", r.Pod, r.Node)
	fmt.Fprintf(w, "Destination:\t%s\n", r.Destination)
	for _, p := range r.Policies {
		m := "doesn't cover this destination"
		if p.Matches {
			m = "covers this destination"
		}
		fmt.Fprintf(w, "Policy:\t%s, priority %d: %s\n", p.Policy, p.Priority, m)
	}
	if r.Winner != "" {
		fmt.Fprintf(w, "Winner:\t%s (failurePolicy %s)\n", r.Winner, r.FailurePolicy)
	}
	if r.Address != "" {
		fmt.Fprintf(w, "Address:\t%s ports %s (%s)\n", r.Address, r.Ports, r.SliceState)
		fmt.Fprintf(w, "Stage:\t%s\n", r.Stage)
		fmt.Fprintf(w, "Holders:\t%s\n", strings.Join(r.Holders, ", "))
		fmt.Fprintf(w, "Route:\t%s\n", r.Route)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "Note:\t%s\n", n)
	}
	for _, l := range r.Live {
		if l.Error != "" {
			fmt.Fprintf(w, "Live %s (%s):\t%s\n", l.Node, l.Role, l.Error)
			continue
		}
		fmt.Fprintf(w, "Live %s (%s):\tstage %s, held %v, slice owner %s", l.Node, l.Role, l.Stage, l.Held, l.SliceOwner)
		if l.Traffic != nil {
			fmt.Fprintf(w, ", out %d/%d pkts, return %d/%d pkts (pod node/announcer)", l.Traffic.OutPod.Packets,
				l.Traffic.OutAnnouncer.Packets, l.Traffic.ReturnPod.Packets, l.Traffic.ReturnAnnouncer.Packets)
		}
		fmt.Fprintln(w)
	}
	_ = w.Flush()
}

// egressDump is one node's dump: its datapath and what it reports in the API.
type egressDump struct {
	Node       string                        `json:"node"`
	Conditions []metav1.Condition            `json:"conditions,omitempty"`
	Faults     []purelbv2.EgressAddressFault `json:"addressFaults,omitempty"`
	Datapath   *diagreport.Report            `json:"datapath,omitempty"`
	Error      string                        `json:"error,omitempty"`
}

func newEgressDumpCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var output, node string
	cmd := &cobra.Command{
		Use:   "dump",
		Short: "Dump each node's egress datapath, read live through kubectl exec",
		Long: `Show each node's egress conditions and faults, and its datapath as the kernel
has it: addresses and their slice owners, slots, pod devices, NAT flows, traffic
and packet counts by reason.

It runs lbnodeagent diag through kubectl exec, which needs create on pods/exec in
the PureLB namespace (see rbac-sample.yaml).`,
		Example: `  kubectl purelb egress dump
  kubectl purelb egress dump --node node-a -o json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(output)
			if err != nil {
				return err
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			dumps, err := runDump(cmd.Context(), c, node)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(format, dumps)
			}
			renderDump(dumps)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: table, json, yaml")
	cmd.Flags().StringVar(&node, "node", "", "Only this node")
	return cmd
}

func runDump(ctx context.Context, c *clients, node string) ([]egressDump, error) {
	pods, err := listAndCategorizePureLBPods(ctx, c)
	if err != nil {
		return nil, err
	}
	states, err := listTyped[purelbv2.EgressNodeState](ctx, c, gvrEgressNodeStates, "")
	if err != nil {
		return nil, err
	}
	var out []egressDump
	for _, p := range agentPerNode(pods.lbnodeagent) {
		if node != "" && p.Spec.NodeName != node {
			continue
		}
		d := egressDump{Node: p.Spec.NodeName}
		for i := range states {
			if states[i].Name == d.Node {
				d.Conditions, d.Faults = states[i].Status.Conditions, states[i].Status.AddressFaults
			}
		}
		if rep, err := runDiag(ctx, c, &p); err != nil {
			d.Error = err.Error()
		} else {
			d.Datapath = rep
		}
		out = append(out, d)
	}
	if node != "" && len(out) == 0 {
		return nil, fmt.Errorf("no lbnodeagent pod on node %q", node)
	}
	return out, nil
}

func renderDump(dumps []egressDump) {
	for _, d := range dumps {
		fmt.Printf("Node %s\n", d.Node)
		for _, c := range d.Conditions {
			if c.Status == metav1.ConditionFalse {
				fmt.Printf("  %s=False %s: %s\n", c.Type, c.Reason, c.Message)
			}
		}
		for _, f := range d.Faults {
			fmt.Printf("  fault %s %s: %s\n", f.Address, f.Reason, f.Message)
		}
		if d.Error != "" {
			fmt.Printf("  datapath: %s\n", d.Error)
			continue
		}
		rep := d.Datapath
		if !rep.Pinned && !rep.Table {
			fmt.Println("  no egress datapath")
			continue
		}
		fmt.Printf("  stage %s, %d NAT flows, %d pod devices\n", rep.Stage, rep.NATFlows, len(rep.PodDevices))
		names := map[uint64]string{}
		for _, ds := range dumps {
			names[diagreport.NodeID(ds.Node)] = ds.Node
		}
		w := tableWriter(os.Stdout)
		fmt.Fprintln(w, "  ADDRESS\tHELD\tSLICES\tOUT POD\tRETURN POD\tOUT ANNOUNCER\tRETURN ANNOUNCER")
		for _, a := range rep.Addresses {
			var owners []string
			for _, s := range a.Slices {
				n, ok := names[s.Owner]
				if !ok {
					n = fmt.Sprintf("%#x", s.Owner)
				}
				owners = append(owners, fmt.Sprintf("%d:%s", s.Index, n))
			}
			fmt.Fprintf(w, "  %s\t%v\t%s\t%d\t%d\t%d\t%d\n", a.Address, a.Held, strings.Join(owners, " "), a.Traffic.OutPod.Packets,
				a.Traffic.ReturnPod.Packets, a.Traffic.OutAnnouncer.Packets, a.Traffic.ReturnAnnouncer.Packets)
		}
		_ = w.Flush()
		for _, s := range rep.Slots {
			fmt.Printf("  slot %d: %s ports %d-%d\n", s.ID, s.Address, s.First, s.Last)
		}
		for _, e := range rep.Errors {
			fmt.Printf("  error: %s\n", e)
		}
	}
}

func newEgressCleanupCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var node string
	var all, force bool
	cmd := &cobra.Command{
		Use:   "cleanup (--node <node> | --all)",
		Short: "Break-glass: remove egress from nodes whose agent can't",
		Long: `Remove everything egress put on a node: the CNI chain entry and plugin, its
programs, pins, nft table, routing rules and tunnel, and the plugin's files. Live
egress flows are reset.

Use it when disabling egress on the LBNodeAgent didn't finish, for instance
because the agent is gone. It runs a privileged pod on each node, so it needs
create and delete on pods in the PureLB namespace. It refuses while an
LBNodeAgent still enables egress, since the agent would put it all back, unless
given --force.`,
		Example: `  kubectl purelb egress cleanup --node node-a
  kubectl purelb egress cleanup --all`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if (node == "") == !all {
				return fmt.Errorf("give exactly one of --node or --all")
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runCleanup(cmd.Context(), c, node, force)
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Only this node")
	cmd.Flags().BoolVar(&all, "all", false, "Every node")
	cmd.Flags().BoolVar(&force, "force", false, "Clean up even while an LBNodeAgent enables egress")
	return cmd
}

func runCleanup(ctx context.Context, c *clients, node string, force bool) error {
	agents, err := c.dynamic.Resource(gvrLBNodeAgents).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing LBNodeAgents: %w", err)
	}
	for _, a := range decodeLBNodeAgents(agents) {
		if a.Spec.Egress != nil && a.Spec.Egress.Enabled && !force {
			return fmt.Errorf("LBNodeAgent %s enables egress, so its agents would put it back: disable it first, or use --force", a.Name)
		}
	}
	ds, err := c.core.AppsV1().DaemonSets(purelbNamespace).Get(ctx, "lbnodeagent", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("finding the agent's image: %w", err)
	}
	image := containerImage(ds.Spec.Template.Spec, "lbnodeagent")
	if image == "" {
		return fmt.Errorf("the lbnodeagent DaemonSet has no lbnodeagent container")
	}
	confDir, binDir := "/etc/cni/net.d", "/opt/cni/bin"
	if cni, err := c.core.AppsV1().DaemonSets(purelbNamespace).Get(ctx, "purelb-cni", metav1.GetOptions{}); err == nil {
		for _, v := range cni.Spec.Template.Spec.Volumes {
			if v.HostPath != nil && v.Name == "cni-conf" {
				confDir = v.HostPath.Path
			}
			if v.HostPath != nil && v.Name == "cni-bin" {
				binDir = v.HostPath.Path
			}
		}
	}
	nodes := []string{node}
	if node == "" {
		list, err := c.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("listing nodes: %w", err)
		}
		nodes = nil
		for _, n := range list.Items {
			nodes = append(nodes, n.Name)
		}
	}
	failed := 0
	for _, n := range nodes {
		fmt.Printf("%s: ", n)
		if err := cleanupNode(ctx, c, n, image, confDir, binDir); err != nil {
			fmt.Printf("FAILED: %v\n", err)
			failed++
			continue
		}
		fmt.Println("egress removed")
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d nodes failed", failed, len(nodes))
	}
	return nil
}

// containerImage is the image of the named container: the sidecars come first
// in some installs.
func containerImage(spec v1.PodSpec, name string) string {
	for _, c := range spec.Containers {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// cleanupPod is the privileged pod that runs egress-cleanup on node.
func cleanupPod(node, image, confDir, binDir string) *v1.Pod {
	mounts := []struct{ name, host, at string }{
		{"bpf", "/sys/fs/bpf/purelb", "/sys/fs/bpf"}, {"cni-conf", confDir, "/host/cni/net.d"},
		{"cni-bin", binDir, "/host/cni/bin"}, {"var-lib-purelb", "/var/lib/purelb", "/var/lib/purelb"},
		{"run-purelb", "/run/purelb", "/run/purelb"},
	}
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "purelb-egress-cleanup-", Namespace: purelbNamespace,
			Labels: map[string]string{"app": "purelb", "component": "egress-cleanup"}},
		Spec: v1.PodSpec{
			NodeName: node, HostNetwork: true, RestartPolicy: v1.RestartPolicyNever,
			Tolerations: []v1.Toleration{{Operator: v1.TolerationOpExists}},
			Containers: []v1.Container{{Name: "cleanup", Image: image, ImagePullPolicy: v1.PullAlways,
				Args:            []string{"egress-cleanup", "--node=" + node},
				SecurityContext: &v1.SecurityContext{Privileged: ptr.To(true), RunAsUser: ptr.To(int64(0)), RunAsNonRoot: ptr.To(false)},
			}},
		},
	}
	for _, m := range mounts {
		p.Spec.Volumes = append(p.Spec.Volumes, v1.Volume{Name: m.name,
			VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: m.host}}})
		p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, v1.VolumeMount{Name: m.name, MountPath: m.at})
	}
	return p
}

func cleanupNode(ctx context.Context, c *clients, node, image, confDir, binDir string) error {
	pods := c.core.CoreV1().Pods(purelbNamespace)
	pod, err := pods.Create(ctx, cleanupPod(node, image, confDir, binDir), metav1.CreateOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = pods.Delete(context.WithoutCancel(ctx), pod.Name, metav1.DeleteOptions{}) }()
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		cur, err := pods.Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch cur.Status.Phase {
		case v1.PodSucceeded:
			return nil
		case v1.PodFailed:
			logs, _ := pods.GetLogs(pod.Name, &v1.PodLogOptions{}).DoRaw(ctx)
			return fmt.Errorf("%s", strings.TrimSpace(string(logs)))
		}
	}
	return fmt.Errorf("pod %s didn't finish in 3 minutes", pod.Name)
}

func newEgressReleaseCmd(flags *genericclioptions.ConfigFlags) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "release-finalizers (<namespace/name> | <name> | --all)",
		Short: "Break-glass: remove egress finalizers the allocator can't",
		Long: `Remove the purelb.io/egress finalizer from a policy (<namespace/name> for an
Egress, <name> for a ClusterEgress), and from its EgressEndpoints, or from all of
them with --all. Use it when the allocator is gone and they can't be deleted:
without the allocator's teardown, quarantine and any external IPAM's release are
skipped.`,
		Example: `  kubectl purelb egress release-finalizers voice/sip
  kubectl purelb egress release-finalizers --all`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 0) != all {
				return fmt.Errorf("give exactly one of a policy or --all")
			}
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			c, err := newClients(flags)
			if err != nil {
				return err
			}
			return runRelease(cmd.Context(), c, target)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Every policy and EgressEndpoints")
	return cmd
}

func runRelease(ctx context.Context, c *clients, target string) error {
	policies, err := allPolicies(ctx, c)
	if err != nil {
		return err
	}
	eps, err := listTyped[purelbv2.EgressEndpoints](ctx, c, gvrEgressEndpoints, purelbNamespace)
	if err != nil {
		return err
	}
	matched, released := false, 0
	release := func(gvr schema.GroupVersionResource, ns, name string, finalizers []string) error {
		if !slices.Contains(finalizers, egressFinalizer) {
			return nil
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"finalizers": slices.DeleteFunc(slices.Clone(finalizers), func(f string) bool { return f == egressFinalizer })}})
		if err != nil {
			return err
		}
		res := c.dynamic.Resource(gvr)
		if ns != "" {
			_, err = res.Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		} else {
			_, err = res.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		}
		if err != nil {
			return fmt.Errorf("releasing %s %s: %w", gvr.Resource, name, err)
		}
		fmt.Printf("%s %s: released\n", gvr.Resource, name)
		released++
		return nil
	}
	es, err := listTyped[purelbv2.Egress](ctx, c, gvrEgresses, "")
	if err != nil {
		return err
	}
	cs, err := listTyped[purelbv2.ClusterEgress](ctx, c, gvrClusterEgresses, "")
	if err != nil {
		return err
	}
	finalizers := map[string][]string{}
	for i := range es {
		finalizers[es[i].Namespace+"/"+es[i].Name] = es[i].Finalizers
	}
	for i := range cs {
		finalizers[cs[i].Name] = cs[i].Finalizers
	}
	for _, p := range policies {
		key := p.ref.Name
		if p.ref.Kind == egress.KindEgress {
			key = p.ref.Namespace + "/" + p.ref.Name
		}
		if target != "" && target != key {
			continue
		}
		matched = true
		gvr := gvrClusterEgresses
		if p.ref.Kind == egress.KindEgress {
			gvr = gvrEgresses
		}
		if err := release(gvr, p.ref.Namespace, p.ref.Name, finalizers[key]); err != nil {
			return err
		}
	}
	for i := range eps {
		ep := &eps[i]
		if target != "" && !endpointsOf(ep, policies, target) {
			continue
		}
		matched = true
		if err := release(gvrEgressEndpoints, ep.Namespace, ep.Name, ep.Finalizers); err != nil {
			return err
		}
	}
	if target != "" && !matched {
		return fmt.Errorf("no policy %q", target)
	}
	fmt.Printf("%d finalizers released\n", released)
	return nil
}

// endpointsOf reports whether ep is an EgressEndpoints of the policy target
// names (namespace/name, or name for a ClusterEgress).
func endpointsOf(ep *purelbv2.EgressEndpoints, policies []policyOf, target string) bool {
	addr, err := netip.ParseAddr(ep.Spec.Address)
	if err != nil {
		return false
	}
	for _, p := range policies {
		key := p.ref.Name
		if p.ref.Kind == egress.KindEgress {
			key = p.ref.Namespace + "/" + p.ref.Name
		}
		if key == target && ep.Name == egress.EndpointsName(p.ref, addr) {
			return true
		}
	}
	return false
}
