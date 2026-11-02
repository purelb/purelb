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
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"golang.org/x/sys/unix"

	"purelb.io/internal/egress"
	"purelb.io/internal/egress/datapath"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// kernelHelpers are each policy helper's kernel helpers on their
// well-known ports (D23).
var kernelHelpers = map[purelbv2.EgressHelper][]datapath.Helper{
	"FTP":  {{Name: "ftp", Proto: unix.IPPROTO_TCP, Port: 21}},
	"SIP":  {{Name: "sip", Proto: unix.IPPROTO_UDP, Port: 5060}, {Name: "sip", Proto: unix.IPPROTO_TCP, Port: 5060}},
	"H323": {{Name: "Q.931", Proto: unix.IPPROTO_TCP, Port: 1720}, {Name: "RAS", Proto: unix.IPPROTO_UDP, Port: 1719}},
}

// stagePlan is the pod-node stage's nft program for pn (design 6.3): each
// pod-set's policies in rank order, each policy's destinations and the
// slot its new flows take per family, and every slot's SNAT. Nodes
// classify by source address until the stage probe (Phase 6) can tell
// where pl_pod_tag's mark survives. The datapath adds the cluster
// networks and each slot's MSS. Queue goroutine only.
func stagePlan(pn *podNode, in pnInput, markShift uint8) *datapath.PodStage {
	ps := &datapath.PodStage{MarkShift: markShift, SourceIP: true, Pods: map[netip.Addr]uint8{},
		PodSets: map[uint8][]string{}, Policies: map[string]datapath.Policy{}, Slots: map[uint8]datapath.Slot{}}
	policies := map[egress.PolicyRef]*pnPolicy{}
	for _, p := range in.policies {
		policies[p.ref] = p
	}

	// The slots, and their reserved ports (D19) and helpers (D23).
	endpointsOf := map[uint8]string{}
	for k, id := range pn.slotIDs {
		endpointsOf[id] = k.endpoints
	}
	for _, id := range slices.Sorted(maps.Keys(pn.slots)) {
		s := pn.slots[id]
		slot := datapath.Slot{Addr: s.address}
		if s.identity == "" {
			k := slotKeyOf(pn, id)
			slot.First, slot.Last = uint16(k.index*s.sliceSize), uint16((k.index+1)*s.sliceSize-1)
		} else if p := policies[s.policy]; p != nil {
			for _, h := range p.spec.Helpers {
				slot.Helpers = append(slot.Helpers, kernelHelpers[h]...)
			}
		}
		ps.Slots[id] = slot
	}
	for _, r := range in.spec.Reserved {
		ref, ok := in.owners[r.Endpoints]
		p := policies[ref]
		addr, err := netip.ParseAddr(r.Address)
		if !ok || p == nil || err != nil {
			continue
		}
		pod := reservedPod(p, r.Pod, in, addr.Is4())
		if !pod.IsValid() {
			continue
		}
		for id, eps := range endpointsOf {
			if eps == r.Endpoints && pn.slots[id].identity == "" {
				s := ps.Slots[id]
				s.Reserved = append(s.Reserved, datapath.ReservedPort{Pod: pod, Proto: protoNumber(r.Protocol), Port: uint16(r.Port)})
				ps.Slots[id] = s
			}
		}
	}

	// The policies: one per policy, a PerPod policy one per pod.
	names := map[string]string{} // policy[#identity]: chain name
	for _, id := range slices.Sorted(maps.Keys(pn.sets)) {
		set := pn.sets[id]
		var chains []string
		for i, ref := range set.policies {
			p := policies[ref]
			if p == nil {
				continue
			}
			key := ref.String() + "#" + set.identities[i]
			name, ok := names[key]
			if !ok {
				name = fmt.Sprintf("p%d", len(names))
				names[key] = name
				ps.Policies[name] = stagePolicy(p, set.identities[i], pn)
			}
			chains = append(chains, name)
		}
		ps.PodSets[id] = chains
	}
	for _, st := range pn.pods {
		if st.podSet == 0 {
			continue
		}
		for _, ip := range st.ips {
			ps.Pods[ip] = st.podSet
		}
	}
	return ps
}

// stagePolicy is p, for the pod with PerPod identity (or every pod, ""),
// per family it holds an address in.
func stagePolicy(p *pnPolicy, identity string, pn *podNode) datapath.Policy {
	var out datapath.Policy
	for _, v4 := range []bool{true, false} {
		var addr netip.Addr
		for _, a := range p.addrs {
			if a.Is4() == v4 {
				addr = a
				break
			}
		}
		if !addr.IsValid() {
			continue // it doesn't cover this family
		}
		f := &datapath.PolicyFamily{Include: destinations(p.spec.Destinations.Include, v4),
			Exclude: destinations(p.spec.Destinations.Exclude, v4), Whole: identity != "", FailOpen: !p.failClosed()}
		if identity == "" {
			f.Address = addr
		}
		// New flows take its Active slot in the family: the slice of its
		// address, or its pod's PerPod address.
		for _, id := range slices.Sorted(maps.Keys(pn.slots)) {
			s := pn.slots[id]
			if s.policy == p.ref && s.identity == identity && s.address.Is4() == v4 && s.state == "Active" {
				f.Slot = id
				if identity != "" {
					f.Address = s.address
				}
				break
			}
		}
		if v4 {
			out.V4 = f
		} else {
			out.V6 = f
		}
	}
	return out
}

// destinations is ds in one family: each entry's networks of the family,
// with its ports; an entry with none of the family is left out.
func destinations(ds []purelbv2.EgressDestination, v4 bool) []datapath.Destination {
	var out []datapath.Destination
	for _, d := range ds {
		var dd datapath.Destination
		for _, c := range d.CIDRs {
			if p, err := netip.ParsePrefix(c); err == nil && p.Addr().Is4() == v4 {
				dd.Prefixes = append(dd.Prefixes, p.Masked())
			}
		}
		if len(dd.Prefixes) == 0 {
			continue
		}
		for _, r := range d.Ports {
			last := r.Port
			if r.EndPort != nil {
				last = *r.EndPort
			}
			dd.Ports = append(dd.Ports, datapath.PortRange{Proto: protoNumber(r.Protocol), First: uint16(r.Port), Last: uint16(last)})
		}
		out = append(out, dd)
	}
	return out
}

// reservedPod is the address, in the family, of the pod on this node p
// names identity; invalid if none.
func reservedPod(p *pnPolicy, identity string, in pnInput, v4 bool) netip.Addr {
	for _, pod := range in.pods {
		if p.identity(pod) == identity && p.selects(pod, in) {
			for _, ip := range podIPs(pod) {
				if ip.Is4() == v4 {
					return ip
				}
			}
		}
	}
	return netip.Addr{}
}

// slotKeyOf is slot id's key.
func slotKeyOf(pn *podNode, id uint8) slotKey {
	for k, v := range pn.slotIDs {
		if v == id {
			return k
		}
	}
	return slotKey{}
}
