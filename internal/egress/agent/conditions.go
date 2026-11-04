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
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/kernelversion"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Node condition types (design 4.6); each is True when healthy.
const (
	condDatapathReady      = "DatapathReady"
	condDatapathCompatible = "DatapathCompatible"
	condUnderlayEncrypted  = "UnderlayEncrypted"
	condPeersReachable     = "TunnelPeersReachable"
	condForwardPath        = "ForwardPath"
)

// dpHealth is what the datapath goroutine last found, for the node's
// conditions. Read-only once stored.
type dpHealth struct {
	applyErr string
	// noGateway are pool interfaces lacking a default route in a family
	// placed on them ("eth1 IPv6"); arpAnnounce IPv4 pool interfaces whose
	// arp_announce isn't 2 ("eth1=0").
	noGateway, arpAnnounce []string
	// encryption is what encrypts node-to-node traffic outside the
	// tunnel: WireGuard links, and "xfrm" for IPsec states (D30).
	encryption []string
	// unreachable are the tunnel peers not answering probes, of probed.
	unreachable []string
	probed      int
	// podFaults are the pod-node stage's address faults: port-less
	// traffic on a slice (D4), missing helpers (D23).
	podFaults map[netip.Addr]purelbv2.EgressAddressFault
	// markConflict names the other rules using the mark byte (D18);
	// ebpf: the node runs the eBPF pod stage.
	ebpf bool
	// forwardBlocked are the families whose flows don't reach their SNAT
	// (D25).
	markConflict   string
	forwardBlocked []string
}

// health builds the datapath's health from its last apply. Datapath
// goroutine only.
func health(st *dpState, w dpWant, err error) *dpHealth {
	h := &dpHealth{encryption: st.encryption, podFaults: maps.Clone(st.podFaults),
		markConflict: st.markConflict, forwardBlocked: slices.Clone(st.forwardBlocked), ebpf: st.ebpf}
	if err != nil {
		h.applyErr = err.Error()
	}
	for ifindex, r := range st.reports {
		name := w.pools[ifindex].name
		if r.noGateway4 {
			h.noGateway = append(h.noGateway, name+" IPv4")
		}
		if r.noGateway6 {
			h.noGateway = append(h.noGateway, name+" IPv6")
		}
		if r.arpAnnounce < 2 {
			h.arpAnnounce = append(h.arpAnnounce, fmt.Sprintf("%s=%d", name, r.arpAnnounce))
		}
	}
	slices.Sort(h.noGateway)
	slices.Sort(h.arpAnnounce)
	return h
}

// underlayEncryption finds node-to-node encryption the tunnel bypasses
// (D30): WireGuard links, and IPsec (xfrm) states. Without an xfrm socket
// (no xfrm_user module) there is no IPsec to find.
func underlayEncryption(h *netlink.Handle) []string {
	var out []string
	if links, err := h.LinkList(); err == nil {
		for _, l := range links {
			if l.Type() == "wireguard" {
				out = append(out, l.Attrs().Name)
			}
		}
	}
	if x, err := netlink.NewHandle(unix.NETLINK_XFRM); err == nil {
		if states, err := x.XfrmStateList(netlink.FAMILY_ALL); err == nil && len(states) > 0 {
			out = append(out, "xfrm")
		}
		x.Close()
	}
	slices.Sort(out)
	return out
}

// nodeConditions brings a.conds in line with the capability and the
// datapath's health; a condition keeps its transition time while its
// status holds. Queue goroutine only.
func (a *Agent) nodeConditions(c *purelbv2.EgressCapability, h *dpHealth, pn *podNode) []metav1.Condition {
	set := func(typ, reason, msg string) {
		st := metav1.ConditionFalse
		if reason == "" {
			st, reason = metav1.ConditionTrue, typ
		}
		meta.SetStatusCondition(&a.conds, metav1.Condition{Type: typ, Status: st, Reason: reason,
			Message: truncate(msg, 1024), LastTransitionTime: metav1.NewTime(a.now())})
	}
	if h == nil {
		h = &dpHealth{}
	}
	if v, err := kernelversion.Parse(c.Kernel); err != nil || !v.AtLeast(kernelversion.EgressMin) {
		set(condDatapathCompatible, "KernelTooOld", fmt.Sprintf("kernel %s is older than egress needs", c.Kernel))
	} else if h.markConflict != "" {
		set(condDatapathCompatible, "MarkConflict", h.markConflict)
	} else if reason, msg := chainCondition(a.installer.Load(), a.now()); reason != "" {
		set(condDatapathCompatible, reason, msg)
	} else {
		set(condDatapathCompatible, "", "")
	}
	switch {
	case h.applyErr != "":
		set(condDatapathReady, "ApplyFailed", h.applyErr)
	case pn != nil && len(pn.short) > 0:
		set(condDatapathReady, "SlotsExhausted", fmt.Sprintf("all %d slots are in use; without one: %s",
			slotBudget, strings.Join(pn.short, ", ")))
	case len(h.noGateway) > 0:
		set(condDatapathReady, "NoGateway", "pool interfaces with no default route: "+strings.Join(h.noGateway, ", "))
	case len(h.arpAnnounce) > 0:
		set(condDatapathReady, "ArpAnnounce", "pool interfaces whose arp_announce isn't 2: "+strings.Join(h.arpAnnounce, ", "))
	default:
		set(condDatapathReady, "", "")
	}
	switch {
	case h.probed > 0 && len(h.unreachable) == h.probed:
		// No peer answers: this node's tunnel port, or its path to every
		// peer, is blocked (a firewall: D25).
		set(condForwardPath, "TunnelPortBlocked", "no tunnel peer answers probes: "+strings.Join(h.unreachable, ", "))
	case len(h.forwardBlocked) > 0 && h.ebpf:
		// The eBPF stage isn't where the node's pods' traffic leaves: the
		// CNI translated it first (Calico's natOutgoing: design 6.1), it
		// leaves by a device the stage isn't on, or the probe was wrong.
		set(condForwardPath, "PodStageBypassed", strings.Join(h.forwardBlocked, ", ")+
			": selected pods' SYNs leave their devices but none reaches the eBPF stage at the NIC, so their traffic isn't translated")
	case len(h.forwardBlocked) > 0:
		// The pods' new egress flows are marked but none reaches its SNAT:
		// a forward filter (Docker, firewalld), rp_filter, or a
		// NetworkPolicy denying every selected pod drops them (D25).
		set(condForwardPath, "ForwardBlocked", strings.Join(h.forwardBlocked, ", ")+
			": pods' new egress flows are marked but none reached its SNAT; a forward filter (Docker, firewalld), "+
			"rp_filter, or a NetworkPolicy denying the selected pods drops them")
	default:
		set(condForwardPath, "", "")
	}
	if len(h.unreachable) > 0 {
		set(condPeersReachable, "PeerUnreachable", "tunnel peers not answering probes: "+strings.Join(h.unreachable, ", "))
	} else {
		set(condPeersReachable, "", "")
	}
	if len(h.encryption) > 0 {
		set(condUnderlayEncrypted, "UnderlayUnencrypted", "node-to-node traffic is encrypted ("+
			strings.Join(h.encryption, ", ")+") but the egress tunnel bypasses it")
	} else {
		set(condUnderlayEncrypted, "", "")
	}
	return slices.Clone(a.conds)
}

// faultList is a.faults and the datapath's pod-node faults (h), in
// address order: an address can have one of each.
func (a *Agent) faultList(h *dpHealth) []purelbv2.EgressAddressFault {
	out := slices.Collect(maps.Values(a.faults))
	if h != nil {
		out = append(out, slices.Collect(maps.Values(h.podFaults))...)
	}
	slices.SortFunc(out, func(x, y purelbv2.EgressAddressFault) int {
		ax, _ := netip.ParseAddr(x.Address)
		ay, _ := netip.ParseAddr(y.Address)
		if c := ax.Compare(ay); c != 0 {
			return c
		}
		return strings.Compare(x.Reason, y.Reason)
	})
	return out
}
