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
}

// health builds the datapath's health from its last apply. Datapath
// goroutine only.
func health(st *dpState, w dpWant, err error) *dpHealth {
	h := &dpHealth{encryption: st.encryption}
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
	if v, err := kernelversion.Parse(c.Kernel); err != nil || !v.AtLeast(kernelversion.EgressMin) {
		set(condDatapathCompatible, "KernelTooOld", fmt.Sprintf("kernel %s is older than egress needs", c.Kernel))
	} else {
		set(condDatapathCompatible, "", "")
	}
	if h == nil {
		h = &dpHealth{}
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

// faultList is a.faults in address order.
func (a *Agent) faultList() []purelbv2.EgressAddressFault {
	addrs := make([]netip.Addr, 0, len(a.faults))
	for addr := range a.faults {
		addrs = append(addrs, addr)
	}
	slices.SortFunc(addrs, func(x, y netip.Addr) int { return x.Compare(y) })
	out := make([]purelbv2.EgressAddressFault, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, a.faults[addr])
	}
	return out
}
