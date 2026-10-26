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
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Informer indexes for the nodes' reports.
const (
	// idxFault indexes EgressNodeStates by the addresses of their
	// addressFaults.
	idxFault = "fault"
	// idxAddress indexes EgressEndpoints by their address.
	idxAddress = "address"
)

func faultIndex(obj interface{}) ([]string, error) {
	ns, ok := obj.(*purelbv2.EgressNodeState)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(ns.Status.AddressFaults))
	for _, f := range ns.Status.AddressFaults {
		out = append(out, f.Address)
	}
	return out, nil
}

func addressIndex(obj interface{}) ([]string, error) {
	if ep, ok := obj.(*purelbv2.EgressEndpoints); ok {
		return []string{ep.Spec.Address}, nil
	}
	return nil, nil
}

// cniEgressKinds are the CNI egress-gateway kinds whose policies may
// select the same pods as PureLB's (design 10.1), by API group.
var cniEgressKinds = map[string]string{
	"cilium.io":             "ciliumegressgatewaypolicies",
	"crd.projectcalico.org": "egressgatewaypolicies",
	"projectcalico.org":     "egressgatewaypolicies",
	"crd.antrea.io":         "egresses",
}

// cniEgressInterval is how often the API is checked for those kinds.
const cniEgressInterval = 10 * time.Minute

// watchCNIEgress keeps r.cniEgress current: the CNI egress-gateway kinds
// the cluster serves, found by API discovery (no extra RBAC). A change
// reconciles every policy.
func (r *Reconciler) watchCNIEgress(stop <-chan struct{}, d discovery.DiscoveryInterface) {
	t := time.NewTicker(cniEgressInterval)
	defer t.Stop()
	for {
		found := cniEgressServed(d)
		if old := r.cniEgress.Load(); old == nil || !slices.Equal(*old, found) {
			r.cniEgress.Store(&found)
			if len(found) > 0 {
				logging.Info(r.logger, "op", "egressCNI", "kinds", strings.Join(found, ", "),
					"msg", "the cluster serves CNI egress gateway kinds whose policies may select the same pods")
			}
			if r.synced.Load() {
				for _, k := range r.allPolicies() {
					r.enqueue(k, false)
				}
			}
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// cniEgressServed is the CNI egress-gateway kinds the API serves, as
// "resource.group", sorted.
func cniEgressServed(d discovery.DiscoveryInterface) []string {
	groups, err := d.ServerGroups()
	if err != nil {
		return nil
	}
	var out []string
	for _, g := range groups.Groups {
		res, ok := cniEgressKinds[g.Name]
		if !ok {
			continue
		}
		for _, v := range g.Versions {
			list, err := d.ServerResourcesForGroupVersion(v.GroupVersion)
			if err != nil {
				continue
			}
			if slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == res }) {
				out = append(out, res+"."+g.Name)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// finding is one node-reported problem across the policy's addresses.
type finding struct {
	cond, reason string
	addrs, nodes map[string]bool
}

// nodeConditions folds what the nodes report about the policy's addresses
// into its conditions (§4.8): each address's holders (its observed
// announcers, and the nodes reporting a fault with it) and its pod nodes
// (its slices' owners). Under Redacted the messages name no node.
func (r *Reconciler) nodeConditions(run *run, eps map[string]*purelbv2.EgressEndpoints) {
	found := map[string]*finding{}
	add := func(cond, reason, addr, node string) {
		k := cond + "/" + reason
		f := found[k]
		if f == nil {
			f = &finding{cond: cond, reason: reason, addrs: map[string]bool{}, nodes: map[string]bool{}}
			found[k] = f
		}
		f.addrs[addr] = true
		if node != "" {
			f.nodes[node] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(eps)) {
		ep := eps[name]
		addr := ep.Spec.Address
		holders := map[string]bool{}
		for _, n := range ep.Status.ObservedAnnouncers {
			holders[n] = true
		}
		objs, _ := r.nodeStateIdx.ByIndex(idxFault, addr)
		for _, o := range objs {
			ns := o.(*purelbv2.EgressNodeState)
			holders[ns.Name] = true
			for _, f := range ns.Status.AddressFaults {
				if f.Address != addr {
					continue
				}
				switch f.Reason {
				case "DADFailed":
					add(condAnnounced, f.Reason, addr, ns.Name)
					add(condAddressUnique, f.Reason, addr, ns.Name)
				case "AddressAddFailed":
					add(condAnnounced, f.Reason, addr, ns.Name)
				case "GuardMapFull", "GuardStandingDown":
					add(condGuarded, f.Reason, addr, ns.Name)
				}
			}
		}
		nodes := maps.Clone(holders)
		for _, n := range sliceNodes(ep) {
			nodes[n] = true
		}
		modes := map[string]bool{}
		for _, n := range slices.Sorted(maps.Keys(nodes)) {
			ns, err := r.nodeStates.Get(n)
			if err != nil {
				continue
			}
			cs := ns.Status.Conditions
			if c := apimeta.FindStatusCondition(cs, "DatapathReady"); holders[n] && c != nil &&
				c.Status == metav1.ConditionFalse && c.Reason == "NoGateway" {
				add(condDatapathReady, c.Reason, addr, n)
			}
			if c := apimeta.FindStatusCondition(cs, "DatapathCompatible"); c != nil && c.Status == metav1.ConditionFalse {
				add(condDatapathCompatible, c.Reason, addr, n)
			}
			if c := apimeta.FindStatusCondition(cs, "ForwardPath"); c != nil && c.Status == metav1.ConditionFalse {
				add(condDatapathCompatible, c.Reason, addr, n)
			}
			if c := apimeta.FindStatusCondition(cs, "UnderlayEncrypted"); c != nil && c.Status == metav1.ConditionFalse {
				add(condUnderlay, c.Reason, addr, n)
			}
			if ns.Status.Capability != nil && ns.Status.Capability.TunnelEncapsulation != "" {
				modes[ns.Status.Capability.TunnelEncapsulation] = true
			}
		}
		if len(modes) > 1 {
			add(condDatapathCompatible, "TunnelModeMismatch", addr, "")
		}
	}
	hide := redacted(run.group)
	for _, k := range slices.Sorted(maps.Keys(found)) {
		f := found[k]
		msg := strings.Join(slices.Sorted(maps.Keys(f.addrs)), ", ")
		switch {
		case f.reason == "TunnelModeMismatch":
			msg += ": its nodes' tunnels use different encapsulations"
		case hide:
			msg += fmt.Sprintf(": %d nodes", len(f.nodes))
		default:
			msg += ": nodes " + strings.Join(slices.Sorted(maps.Keys(f.nodes)), ", ")
		}
		run.conds.fail(f.cond, f.reason, "%s", msg)
	}
	if kinds := r.cniEgress.Load(); kinds != nil && len(*kinds) > 0 {
		run.conds.fail(condCNIEgressPolicy, "CNIEgressPolicy",
			"the cluster serves CNI egress gateway kinds (%s): a policy of theirs may select the same pods",
			strings.Join(*kinds, ", "))
		if c := apimeta.FindStatusCondition(run.p.status.Conditions, condCNIEgressPolicy); c == nil || c.Status != metav1.ConditionFalse {
			r.event(run.p.ref, nil, run.group, false, true, "CNIEgressPolicy",
				"the cluster serves CNI egress gateway kinds (%s): a policy of theirs may select the same pods",
				strings.Join(*kinds, ", "))
		}
	}
}
