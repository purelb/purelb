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
	"fmt"
	"maps"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
)

// podTagPrefix names pl_pod_tag's links, pinned as pl_pod_tag-<device>.
const podTagPrefix = "pl_pod_tag-"

// dpPod is one pod as pl_pod_tag treats it; failClosed: a FailClosed
// policy selects it (holdFirst).
type dpPod struct {
	name       string // namespace/name, for logs
	v4, v6     netip.Addr
	podSet     uint8
	hold       bool
	failClosed bool
}

// dpPods is the pod-node state's pods for the datapath, sorted by name.
func dpPods(pn *podNode) []dpPod {
	out := make([]dpPod, 0, len(pn.pods))
	for _, name := range slices.Sorted(maps.Keys(pn.pods)) {
		st := pn.pods[name]
		p := dpPod{name: name, podSet: st.podSet, hold: st.hold, failClosed: st.failClosed}
		for _, ip := range st.ips {
			if ip.Is4() && !p.v4.IsValid() {
				p.v4 = ip
			} else if ip.Is6() && !p.v6.IsValid() {
				p.v6 = ip
			}
		}
		if p.v4.IsValid() || p.v6.IsValid() {
			out = append(out, p)
		}
	}
	return out
}

// seedPodSets is the plan a restarted agent starts from: the pod-set ids
// the pinned pod_devs map gives this node's pods, so a pod keeps its id
// across the restart (D12). A conflicting recovery (two pod-sets claiming
// one id) is left out.
func seedPodSets(in pnInput, recovered map[netip.Addr]uint8) *podNode {
	first := planPodNode(in, nil)
	prev := &podNode{setIDs: map[string]uint8{}, sets: map[uint8]podSet{}}
	for _, st := range first.pods {
		if st.podSet == 0 {
			continue
		}
		set := first.sets[st.podSet]
		for _, ip := range st.ips {
			id, ok := recovered[ip]
			if !ok || id == 0 {
				continue
			}
			if have, ok := prev.setIDs[set.key]; ok && have != id {
				continue
			}
			if other, ok := prev.sets[id]; ok && other.key != set.key {
				continue
			}
			prev.setIDs[set.key], prev.sets[id] = id, set
		}
	}
	return prev
}

// recoveredPodSets reads the pinned pod_devs map an earlier agent left:
// each pod address's pod-set id.
func recoveredPodSets(pinDir string) map[netip.Addr]uint8 {
	out := map[netip.Addr]uint8{}
	if !bpfpin.Usable(pinDir) {
		return out
	}
	devs, err := datapath.PinnedPodDevs(pinDir)
	if err != nil {
		return out
	}
	for _, d := range devs {
		for _, ip := range []netip.Addr{d.V4, d.V6} {
			if ip.IsValid() && d.PodSet != 0 {
				out[ip] = d.PodSet
			}
		}
	}
	return out
}

// clusterAPI is the cluster networks the API shows (design 6.3): every
// Node's podCIDRs and addresses, the Service CIDRs, the API server's
// addresses, and the agent's excludedNetworks and podNetworks; with the
// fixed link-local, multicast and broadcast networks. The datapath adds the
// pod ranges this node's routes show.
func (a *Agent) clusterAPI(s *session) []netip.Prefix {
	set := map[netip.Prefix]bool{}
	add := func(cidr string) {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			set[p.Masked()] = true
		} else if ip, err := netip.ParseAddr(cidr); err == nil {
			set[netip.PrefixFrom(ip, ip.BitLen())] = true
		}
	}
	for _, p := range datapath.FixedClusterNetworks {
		set[p] = true
	}
	for _, obj := range s.nodes.List() {
		n := obj.(*corev1.Node)
		for _, c := range n.Spec.PodCIDRs {
			add(c)
		}
		for _, addr := range n.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
				add(addr.Address)
			}
		}
	}
	if s.serviceCIDRs != nil {
		for _, obj := range s.serviceCIDRs.List() {
			for _, c := range obj.(*networkingv1.ServiceCIDR).Spec.CIDRs {
				add(c)
			}
		}
	}
	for _, obj := range s.apiServer.List() {
		for _, ep := range obj.(*discoveryv1.EndpointSlice).Endpoints {
			for _, addr := range ep.Addresses {
				add(addr)
			}
		}
	}
	for _, c := range s.spec.ExcludedNetworks {
		add(c)
	}
	for _, c := range s.spec.PodNetworks {
		add(c)
	}
	out := slices.Collect(maps.Keys(set))
	slices.SortFunc(out, func(x, y netip.Prefix) int {
		if c := x.Addr().Compare(y.Addr()); c != 0 {
			return c
		}
		return x.Bits() - y.Bits()
	})
	return out
}

// nodeAddrs are every Node's InternalIPs and ExternalIPs: a route through
// one reaches a cluster network (design 6.3).
func nodeAddrs(s *session) map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	for _, obj := range s.nodes.List() {
		for _, addr := range obj.(*corev1.Node).Status.Addresses {
			if addr.Type != corev1.NodeInternalIP && addr.Type != corev1.NodeExternalIP {
				continue
			}
			if ip, err := netip.ParseAddr(addr.Address); err == nil {
				out[ip] = true
			}
		}
	}
	return out
}

// holdFirst is an agent start's first step for its pods (design 8.1): the
// pods a FailClosed policy selects that no earlier agent programmed (no
// pinned entry has their address: started in the CNI boot window, or
// while no agent ran, after a reboot) get pl_pod_tag's hold before
// anything classifies; syncPodDevs lifts it, after the table, where the
// pod's slot is ready. Datapath goroutine only.
func (a *Agent) holdFirst(st *dpState, w dpWant) error {
	have, err := st.objs.RecoverPodDevs()
	if err != nil {
		return fmt.Errorf("reading the pod devices: %w", err)
	}
	programmed := map[netip.Addr]bool{}
	for _, d := range have {
		for _, ip := range []netip.Addr{d.V4, d.V6} {
			if ip.IsValid() {
				programmed[ip] = true
			}
		}
	}
	var held []string
	pods := slices.Clone(w.pods)
	for i, p := range pods {
		if p.failClosed && !p.hold && !programmed[p.v4] && !programmed[p.v6] {
			pods[i].hold = true
			held = append(held, p.name)
		}
	}
	if len(held) == 0 {
		return nil
	}
	logging.Info(a.logger, "op", "egressPods", "node", a.node, "pods", held,
		"msg", "holding FailClosed pods no earlier agent programmed until their classifier is in place")
	w.pods = pods
	return a.syncPodDevs(st, w)
}

// syncPodDevs programs pl_pod_tag (D2): the cluster networks, then each
// local pod's device entry, then the program on each pod's host-side
// device, the devices found from the pods' addresses
// (datapath.PodDevices). A device no pod is known on yet is left alone.
// Until the pod-node state is planned (an agent restart), what the kernel
// holds is kept (D12). Datapath goroutine only.
func (a *Agent) syncPodDevs(st *dpState, w dpWant) error {
	routes, err := datapath.ClusterRoutes(st.nl, w.nodeAddrs)
	if err != nil {
		return fmt.Errorf("reading the routes: %w", err)
	}
	cluster := append(slices.Clone(w.cluster), routes...)
	if err := st.objs.SyncCluster(cluster); err != nil {
		return fmt.Errorf("writing the cluster networks: %w", err)
	}
	if !w.podsKnown {
		return nil
	}
	devs, err := datapath.PodDevices(st.nl)
	if err != nil {
		return fmt.Errorf("finding the pod devices: %w", err)
	}
	type entry struct {
		dev datapath.PodDevice
		pd  datapath.PodDev
	}
	want := map[int]entry{}
	for _, p := range w.pods {
		dev, ok := devs[p.v4]
		if !ok {
			dev, ok = devs[p.v6]
		}
		if !ok {
			continue // not wired yet: the next apply follows
		}
		want[dev.Ifindex] = entry{dev: dev, pd: datapath.PodDev{V4: p.v4, V6: p.v6, PodSet: p.podSet, Hold: p.hold, MAC: dev.MAC}}
	}
	var errs []error
	have, err := st.objs.RecoverPodDevs()
	if err != nil {
		return fmt.Errorf("reading the pod devices: %w", err)
	}
	set := map[int]bool{}
	for ifindex, e := range want {
		if have[ifindex] != e.pd {
			if err := st.objs.SetPodDev(ifindex, e.pd); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		set[ifindex] = true
	}
	for ifindex := range have {
		if _, ok := want[ifindex]; !ok {
			errs = append(errs, st.objs.DeletePodDev(ifindex))
		}
	}

	wantLinks := map[string]datapath.PodDevice{}
	st.podBySrc = false
	for _, e := range want {
		wantLinks[podTagPrefix+e.dev.Name] = e.dev
		if e.dev.Kind == "netkit" {
			st.podBySrc = true // no ingress device at the NIC (measured)
		}
	}
	for name, l := range st.links {
		if !strings.HasPrefix(name, podTagPrefix) {
			continue
		}
		if d, ok := wantLinks[name]; ok && st.linkIfs[name] == d.Ifindex && podTagAtHead(st, d) {
			continue
		} else if ok && st.linkIfs[name] == d.Ifindex {
			a.reattaching(name, d.Ifindex)
		}
		if err := detach(l); err != nil && !errors.Is(err, ebpf.ErrNotSupported) {
			// The device is gone with its link: nothing to detach.
			logging.Debug(a.logger, "op", "egressPods", "node", a.node, "link", name, "error", err)
		}
		delete(st.links, name)
		delete(st.linkIfs, name)
	}
	for _, name := range slices.Sorted(maps.Keys(wantLinks)) {
		if _, ok := st.links[name]; ok {
			continue
		}
		d := wantLinks[name]
		var l link.Link
		var err error
		if d.Kind == "netkit" {
			l, err = datapath.AttachNetkitPeer(st.pinDir, name, d.Ifindex, st.objs.PlPodTag)
		} else {
			l, err = datapath.AttachTCX(st.pinDir, name, d.Ifindex, st.objs.PlPodTag, ebpf.AttachTCXIngress, nil)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("attaching %s: %w", name, err))
			continue
		}
		st.links[name], st.linkIfs[name] = l, d.Ifindex
	}
	if st.pinDir != "" {
		if _, err := bpfpin.Sweep(st.pinDir, podTagPrefix, func(path string) bool {
			_, ok := st.links[filepath.Base(path)]
			return ok
		}); err != nil {
			errs = append(errs, err)
		}
	}
	// What a CNI ADD may count on: the device's entry and its program.
	st.tagged = map[netip.Addr]datapath.PodDev{}
	for ifindex, e := range want {
		if _, ok := st.links[podTagPrefix+e.dev.Name]; ok && set[ifindex] {
			for _, ip := range []netip.Addr{e.pd.V4, e.pd.V6} {
				if ip.IsValid() {
					st.tagged[ip] = e.pd
				}
			}
		}
	}
	logging.Debug(a.logger, "op", "egressPods", "node", a.node, "devices", len(want), "tagged", len(wantLinks),
		"cluster", len(cluster), "msg", "pod devices programmed")
	return errors.Join(errs...)
}
