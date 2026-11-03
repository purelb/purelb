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
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/egress"
	"purelb.io/internal/egress/datapath"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// stagePlan turns the pod-node state into the nft program: slots with
// their slice's ports, reserved ports and helpers; each policy per family
// it holds an address in, with its Active slot; each pod-set's policies;
// each pod's addresses.
func TestStagePlan(t *testing.T) {
	web := pnEgress("voice", "web", 100, func(p *pnPolicy) {
		p.spec.FailurePolicy = "FailOpen"
		p.spec.Destinations = purelbv2.EgressDestinations{
			Include: []purelbv2.EgressDestination{
				{CIDRs: []string{"203.0.113.0/24", "198.51.100.7/32"}, Ports: []purelbv2.EgressPortRange{{Protocol: "TCP", Port: 443},
					{Protocol: "UDP", Port: 8000, EndPort: ptr[int32](8099)}}},
				{CIDRs: []string{"2001:db8:f::/48"}},
			},
			Exclude: []purelbv2.EgressDestination{{CIDRs: []string{"203.0.113.9/32"}}},
		}
		p.addrs = []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")}
	})
	db := pnEgress("db", "db", 100, func(p *pnPolicy) {
		p.spec.Affinity = &purelbv2.EgressAffinity{Mode: "PerPod"}
		p.spec.Helpers = []purelbv2.EgressHelper{"FTP", "H323"}
		p.spec.Destinations.Include = []purelbv2.EgressDestination{{CIDRs: []string{"0.0.0.0/0", "::/0"}}}
		p.addrs = []netip.Addr{netip.MustParseAddr("192.0.2.10")}
	})
	db0 := pnPod("db", "db-0", appA, "10.1.0.9")
	db0.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: ptr(true)}}
	// a lists its IPv6 address first: the reserved port takes the one in
	// the address's family.
	in := pnInput{pods: []*corev1.Pod{pnPod("voice", "a", appA, "fd01::5", "10.1.0.5"), pnPod("voice", "b", appA, "10.1.0.6"), db0},
		policies: []*pnPolicy{web, db},
		owners:   map[string]egress.PolicyRef{"e-web": web.ref, "e-web6": web.ref, "e-db": db.ref},
		spec: purelbv2.EgressNodeStateSpec{
			Slices: []purelbv2.EgressNodeSlice{
				{Endpoints: "e-web", Address: "192.0.2.1", Index: 5, State: "Active", SliceSize: 4096},
				{Endpoints: "e-web6", Address: "2001:db8::1", Index: 6, State: "Pending", SliceSize: 4096},
				{Endpoints: "e-db", Address: "192.0.2.10", Index: 0, State: "Active", Identity: "db-0"},
			},
			Reserved: []purelbv2.EgressNodeReserved{{Endpoints: "e-web", Address: "192.0.2.1", Protocol: "UDP", Port: 5060, Pod: "a"}},
		}}
	pn := planPodNode(in, nil)
	ps := stagePlan(pn, in, 8, true)
	assert.True(t, ps.SourceIP, "source-IP classification until the stage probe")
	assert.Equal(t, uint8(8), ps.MarkShift)

	id := func(eps string) uint8 {
		for k, v := range pn.slotIDs {
			if k.endpoints == eps {
				return v
			}
		}
		t.Fatalf("no slot for %s", eps)
		return 0
	}
	w4, w6, d := id("e-web"), id("e-web6"), id("e-db")
	assert.Equal(t, map[uint8]datapath.Slot{
		w4: {Addr: netip.MustParseAddr("192.0.2.1"), First: 20480, Last: 24575,
			Reserved: []datapath.ReservedPort{{Pod: netip.MustParseAddr("10.1.0.5"), Proto: unix.IPPROTO_UDP, Port: 5060}}},
		w6: {Addr: netip.MustParseAddr("2001:db8::1"), First: 24576, Last: 28671},
		d: {Addr: netip.MustParseAddr("192.0.2.10"), Helpers: []datapath.Helper{{Name: "ftp", Proto: unix.IPPROTO_TCP, Port: 21},
			{Name: "Q.931", Proto: unix.IPPROTO_TCP, Port: 1720}, {Name: "RAS", Proto: unix.IPPROTO_UDP, Port: 1719}}},
	}, ps.Slots)

	a, b := pn.pods["voice/a"].podSet, pn.pods["voice/b"].podSet
	require.NotEqual(t, a, b, "a holds a reserved port: its own pod-set")
	require.Len(t, ps.PodSets[a], 1)
	assert.Equal(t, ps.PodSets[a], ps.PodSets[b], "one chain for the policy")
	webP := ps.Policies[ps.PodSets[a][0]]
	assert.Equal(t, &datapath.PolicyFamily{
		Include: []datapath.Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("198.51.100.7/32")},
			Ports: []datapath.PortRange{{Proto: unix.IPPROTO_TCP, First: 443, Last: 443}, {Proto: unix.IPPROTO_UDP, First: 8000, Last: 8099}}}},
		Exclude: []datapath.Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}}},
		Slot:    w4, FailOpen: true, Address: netip.MustParseAddr("192.0.2.1"),
	}, webP.V4)
	assert.Equal(t, &datapath.PolicyFamily{
		Include:  []datapath.Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:f::/48")}}},
		FailOpen: true, Address: netip.MustParseAddr("2001:db8::1"),
	}, webP.V6, "no Active slot in IPv6 yet")

	dbID := pn.pods["db/db-0"].podSet
	dbP := ps.Policies[ps.PodSets[dbID][0]]
	assert.Equal(t, &datapath.PolicyFamily{Include: []datapath.Destination{{Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
		Slot: d, Whole: true, Address: netip.MustParseAddr("192.0.2.10")}, dbP.V4, "PerPod: its pod's whole address")
	assert.Nil(t, dbP.V6, "no IPv6 address: the policy doesn't cover IPv6")

	assert.Equal(t, map[netip.Addr]uint8{netip.MustParseAddr("10.1.0.5"): a, netip.MustParseAddr("fd01::5"): a,
		netip.MustParseAddr("10.1.0.6"): b, netip.MustParseAddr("10.1.0.9"): dbID}, ps.Pods)

	// The same input plans the same program (the datapath rewrites the
	// table only when it changes).
	assert.Equal(t, ps, stagePlan(planPodNode(in, pn), in, 8, true))
}
