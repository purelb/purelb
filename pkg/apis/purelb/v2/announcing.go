// Copyright 2020-2026 Acnodal Inc.
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

package v2

import (
	"net/netip"
	"slices"
	"strings"
)

// The purelb.io/announcing-<family> annotation records which node announces
// each of a Service's LoadBalancer IPs. Its wire form is space-separated
// "node,interface,ip" entries, one per announced IP:
//
//	node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11
//
// The IP is the slot key: for any given IP exactly one node is the election
// winner and owns that IP's entry. Node names are DNS-1123 labels and IP
// strings are comma-free, but Linux interface names may legally contain
// commas, so an entry is split on its FIRST comma (node) and LAST comma (ip),
// leaving everything between as the interface.

// Announcement is one slot of an announcing-<family> annotation. It is a
// helper type, not a Kubernetes API object, so it is excluded from deepcopy
// generation (netip.Addr is a value type and copies by assignment).
//
// +k8s:deepcopy-gen=false
type Announcement struct {
	Node      string
	Interface string
	IP        netip.Addr
}

// Announcements is a decoded announcing-<family> annotation value.
//
// +k8s:deepcopy-gen=false
type Announcements []Announcement

// ParseAnnouncements decodes an annotation value. It is lenient: entries that
// are not well-formed "node,interface,ip" with a parseable, unzoned IP are
// dropped rather than causing an error, because a malformed annotation must
// not stall reconciliation. Dropped entries do not survive a subsequent
// Encode. Returns nil for the empty string.
func ParseAnnouncements(value string) Announcements {
	if value == "" {
		return nil
	}
	var out Announcements
	for _, field := range strings.Fields(value) {
		first := strings.IndexByte(field, ',')
		last := strings.LastIndexByte(field, ',')
		// Need at least two distinct commas for node,interface,ip. This also
		// drops the legacy one-field ("kube-lb0") and two-field
		// ("node,iface") forms, whose IP does not parse.
		if first < 0 || first == last {
			continue
		}
		node := field[:first]
		iface := field[first+1 : last]
		addr, err := netip.ParseAddr(field[last+1:])
		if err != nil || addr.Zone() != "" || node == "" || iface == "" {
			continue
		}
		out = append(out, Announcement{Node: node, Interface: iface, IP: addr.Unmap()})
	}
	return out
}

// Encode renders the canonical wire form: entries sorted by (IP, node,
// interface), reduced to one slot per IP. The output is a pure function of the
// slot set, so two nodes holding the same set produce byte-identical strings
// (the property that lets an unchanged reconcile skip the API write), and
// Encode(ParseAnnouncements(s)) == s for any canonical s.
func (a Announcements) Encode() string {
	if len(a) == 0 {
		return ""
	}
	s := slices.Clone(a)
	slices.SortFunc(s, func(x, y Announcement) int {
		if c := x.IP.Compare(y.IP); c != 0 {
			return c
		}
		if c := strings.Compare(x.Node, y.Node); c != 0 {
			return c
		}
		return strings.Compare(x.Interface, y.Interface)
	})

	var b strings.Builder
	var last netip.Addr
	for i, e := range s {
		if i > 0 && e.IP == last {
			continue // one slot per IP; keep the first after the total sort
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(e.Node)
		b.WriteByte(',')
		b.WriteString(e.Interface)
		b.WriteByte(',')
		b.WriteString(e.IP.String())
		last = e.IP
	}
	return b.String()
}

// Reconcile returns the slots this node should write after winning the
// election for mine.IP. mine's slot replaces any existing entry for the same
// IP (the winner takes the slot from a previous holder), and only entries
// whose IP is still in keep are retained (keep is the Service's current set of
// LoadBalancer ingress IPs, so a slot for an IP the Service no longer has is
// swept away). Entries for other IPs that are still in keep are left
// untouched -- they belong to whichever node won those IPs.
//
// This node only ever asserts its own slot and drops slots for departed IPs;
// it never rewrites another node's slot for a live IP, so concurrent writers
// on different IPs do not contend.
func (a Announcements) Reconcile(mine Announcement, keep []netip.Addr) Announcements {
	mine.IP = mine.IP.Unmap()
	keepSet := make(map[netip.Addr]struct{}, len(keep))
	for _, ip := range keep {
		keepSet[ip.Unmap()] = struct{}{}
	}

	out := make(Announcements, 0, len(a)+1)
	for _, e := range a {
		if _, ok := keepSet[e.IP]; !ok {
			continue // swept: IP no longer announced by this Service
		}
		if e.IP == mine.IP {
			continue // this node now owns this slot
		}
		out = append(out, e)
	}
	return append(out, mine)
}
