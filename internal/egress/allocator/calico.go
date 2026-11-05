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
	"context"
	"net/netip"
	"reflect"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"purelb.io/internal/logging"
)

// Calico's eBPF dataplane treats a pod's traffic by its IP pool (design 6.1,
// resolution 20): with natOutgoing on it reaches netfilter, with it off it
// leaves in BPF, past netfilter. A node has one pod stage, so the selected
// pods on a node must agree with it. Read only, and only where the API group
// exists.
const (
	calicoGroupVersion = "crd.projectcalico.org/v1"
	calicoInterval     = time.Minute
)

var (
	calicoPoolsGVR = schema.GroupVersionResource{Group: "crd.projectcalico.org", Version: "v1", Resource: "ippools"}
	calicoFelixGVR = schema.GroupVersionResource{Group: "crd.projectcalico.org", Version: "v1", Resource: "felixconfigurations"}
)

// calicoPool is one Calico IP pool.
type calicoPool struct {
	cidr        netip.Prefix
	natOutgoing bool
}

// calicoState is what matters of Calico here: only its eBPF dataplane's
// pools can disagree with a stage.
type calicoState struct {
	bpf   bool
	pools []calicoPool
}

// natOutgoing is the setting of the longest pool holding ip; ok false outside
// every pool.
func (c *calicoState) natOutgoing(ip netip.Addr) (nat, ok bool) {
	best := -1
	for _, p := range c.pools {
		if p.cidr.Contains(ip) && p.cidr.Bits() > best {
			best, nat, ok = p.cidr.Bits(), p.natOutgoing, true
		}
	}
	return nat, ok
}

// watchCalico keeps r.calico current. A change reconciles every policy.
func (r *Reconciler) watchCalico(stop <-chan struct{}, d discovery.DiscoveryInterface, dyn dynamic.Interface) {
	t := time.NewTicker(calicoInterval)
	defer t.Stop()
	for {
		found, err := readCalico(d, dyn)
		if err != nil {
			logging.Info(r.logger, "op", "egressCalico", "error", err, "msg", "reading Calico's IP pools; retrying")
		} else if old := r.calico.Load(); !reflect.DeepEqual(old, found) {
			r.calico.Store(found)
			logging.Debug(r.logger, "op", "egressCalico", "bpf", found != nil && found.bpf, "msg", "Calico's IP pools changed")
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

// readCalico reads Calico's eBPF setting and IP pools; nil where the API
// group isn't served, or its dataplane isn't eBPF.
func readCalico(d discovery.DiscoveryInterface, dyn dynamic.Interface) (*calicoState, error) {
	if _, err := d.ServerResourcesForGroupVersion(calicoGroupVersion); err != nil {
		return nil, nil // no Calico CRDs: not Calico, or none of this API
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	felix, err := dyn.Resource(calicoFelixGVR).Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		return nil, nil // no default configuration: Felix's defaults, not eBPF
	}
	if on, _, _ := unstructured.NestedBool(felix.Object, "spec", "bpfEnabled"); !on {
		return nil, nil
	}
	list, err := dyn.Resource(calicoPoolsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := &calicoState{bpf: true}
	for _, item := range list.Items {
		cidr, _, _ := unstructured.NestedString(item.Object, "spec", "cidr")
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			continue
		}
		nat, _, _ := unstructured.NestedBool(item.Object, "spec", "natOutgoing")
		out.pools = append(out.pools, calicoPool{p.Masked(), nat})
	}
	slices.SortFunc(out.pools, func(x, y calicoPool) int { return x.cidr.Addr().Compare(y.cidr.Addr()) })
	return out, nil
}

// natConflicts are the nodes whose selected pods can't all be served: pods in
// pools that disagree, or in a pool that disagrees with the node's stage
// (natOutgoing on needs netfilter, off the eBPF stage).
func (r *Reconciler) natConflicts(run *run) map[string]bool {
	c := r.calico.Load()
	if c == nil {
		return nil
	}
	out := map[string]bool{}
	for node, members := range run.sel.nodes {
		stage := ""
		if ns, err := r.nodeStates.Get(node); err == nil && ns.Status.Capability != nil {
			stage = ns.Status.Capability.PodNodeStage
		}
		var on, off bool
		for _, m := range members {
			obj, ok, _ := r.pods.GetByKey(m.namespace + "/" + m.name)
			if !ok {
				continue
			}
			pod, isPod := obj.(*corev1.Pod)
			if !isPod {
				continue
			}
			for _, pip := range pod.Status.PodIPs {
				ip, err := netip.ParseAddr(pip.IP)
				if err != nil {
					continue
				}
				if nat, ok := c.natOutgoing(ip); ok {
					on, off = on || nat, off || !nat
				}
			}
		}
		if (on && off) || (on && stage == "EBPF") || (off && stage == "Netfilter") {
			out[node] = true
		}
	}
	return out
}
