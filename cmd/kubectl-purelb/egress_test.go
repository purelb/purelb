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
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"purelb.io/internal/egress"
	"purelb.io/internal/egress/diagreport"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func TestParseDst(t *testing.T) {
	for in, want := range map[string]flowDst{
		"203.0.113.50:443/tcp": {netip.MustParseAddr("203.0.113.50"), 443, "TCP"},
		"[2001:db8::1]:53/udp": {netip.MustParseAddr("2001:db8::1"), 53, "UDP"},
		"203.0.113.50":         {netip.MustParseAddr("203.0.113.50"), 0, "TCP"},
		"2001:db8::1/icmpv6":   {netip.MustParseAddr("2001:db8::1"), 0, "ICMPV6"},
	} {
		got, err := parseDst(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := parseDst("not-an-address")
	assert.Error(t, err)
	d, _ := parseDst("[2001:db8::1]:53/udp")
	assert.Equal(t, "[2001:db8::1]:53/UDP", d.String())
}

func TestDestinationsMatch(t *testing.T) {
	dests := purelbv2.EgressDestinations{
		Include: []purelbv2.EgressDestination{
			{CIDRs: []string{"198.51.100.0/24", "2001:db8::/32"}, Ports: []purelbv2.EgressPortRange{{Protocol: "TCP", Port: 443}}},
			{CIDRs: []string{"203.0.113.0/24"}, Ports: []purelbv2.EgressPortRange{{Port: 1000, EndPort: ptr.To(int32(2000))}}},
			{CIDRs: []string{"192.0.2.0/24"}},
		},
		Exclude: []purelbv2.EgressDestination{{CIDRs: []string{"192.0.2.128/25"}}},
	}
	for in, want := range map[string]bool{
		"198.51.100.1:443/tcp":  true,
		"198.51.100.1:443/udp":  false,
		"198.51.100.1:80/tcp":   false,
		"[2001:db8::9]:443/tcp": true,
		"[2001:db9::9]:443/tcp": false,
		"203.0.113.9:1500/udp":  true,
		"203.0.113.9:2001/udp":  false,
		"192.0.2.1:22/tcp":      true,
		"192.0.2.200:22/tcp":    false,
	} {
		d, err := parseDst(in)
		require.NoError(t, err)
		assert.Equal(t, want, destinationsMatch(dests, d), in)
	}
}

func TestSelectsPod(t *testing.T) {
	ref := egress.PolicyRef{Kind: egress.KindEgress, Namespace: "voice", Name: "sip"}
	cref := egress.PolicyRef{Kind: egress.KindClusterEgress, Name: "all"}
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "a", UID: types.UID("u1")}}
	members := map[string]*purelbv2.EgressMembers{
		egress.MembersName(ref, 0):  {Spec: purelbv2.EgressMembersSpec{Nodes: []purelbv2.EgressMemberNode{{Node: "n", Pods: []purelbv2.EgressMemberPod{{Name: "b", UID: "u2"}}}}}},
		egress.MembersName(ref, 1):  {Spec: purelbv2.EgressMembersSpec{Nodes: []purelbv2.EgressMemberNode{{Node: "n", Pods: []purelbv2.EgressMemberPod{{Name: "a", UID: "u1"}}}}}},
		egress.MembersName(cref, 0): {Spec: purelbv2.EgressMembersSpec{Nodes: []purelbv2.EgressMemberNode{{Node: "n", Pods: []purelbv2.EgressMemberPod{{Namespace: "voice", Name: "a", UID: "u1"}}}}}},
	}
	assert.True(t, selectsPod(members, policyOf{ref: ref}, pod), "in the second object")
	assert.True(t, selectsPod(members, policyOf{ref: cref}, pod), "a ClusterEgress's pods carry their namespace")
	assert.False(t, selectsPod(members, policyOf{ref: egress.PolicyRef{Kind: egress.KindEgress, Namespace: "x", Name: "y"}}, pod))
	replaced := pod.DeepCopy()
	replaced.UID = "u9"
	assert.False(t, selectsPod(members, policyOf{ref: ref}, replaced), "the same name recreated is another pod")
}

// The first by rank that covers the destination wins, in its family.
func TestRunTrace(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.10")
	ref := egress.PolicyRef{Kind: egress.KindEgress, Namespace: "voice", Name: "sip"}
	low := egress.PolicyRef{Kind: egress.KindEgress, Namespace: "voice", Name: "low"}
	epName := egress.EndpointsName(ref, addr)
	dests := func(cidr string) purelbv2.EgressPolicy {
		return purelbv2.EgressPolicy{FailurePolicy: "FailClosed", Destinations: purelbv2.EgressDestinations{
			Include: []purelbv2.EgressDestination{{CIDRs: []string{cidr}}}}}
	}
	member := func(r egress.PolicyRef) *purelbv2.EgressMembers {
		return &purelbv2.EgressMembers{TypeMeta: metav1.TypeMeta{APIVersion: "purelb.io/v2", Kind: "EgressMembers"},
			ObjectMeta: metav1.ObjectMeta{Name: egress.MembersName(r, 0), Namespace: purelbNamespace},
			Spec: purelbv2.EgressMembersSpec{Nodes: []purelbv2.EgressMemberNode{{Node: "n1",
				Pods: []purelbv2.EgressMemberPod{{Name: "a", UID: "u1"}}}}}}
	}
	gvk := func(k string) metav1.TypeMeta { return metav1.TypeMeta{APIVersion: "purelb.io/v2", Kind: k} }
	c := newFakeClients([]runtime.Object{&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "a", UID: "u1"}, Spec: v1.PodSpec{NodeName: "n1"}}})
	seed := func(gvr schema.GroupVersionResource, ns string, obj any) {
		t.Helper()
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		require.NoError(t, err)
		res := c.dynamic.Resource(gvr)
		u := &unstructured.Unstructured{Object: m}
		if ns != "" {
			_, err = res.Namespace(ns).Create(t.Context(), u, metav1.CreateOptions{})
		} else {
			_, err = res.Create(t.Context(), u, metav1.CreateOptions{})
		}
		require.NoError(t, err)
	}
	seed(gvrEgresses, "voice", &purelbv2.Egress{TypeMeta: gvk("Egress"), ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip", CreationTimestamp: metav1.NewTime(time.Unix(100, 0))},
		Spec: purelbv2.EgressSpec{Priority: 100, EgressPolicy: dests("203.0.113.0/24")}})
	seed(gvrEgresses, "voice", &purelbv2.Egress{TypeMeta: gvk("Egress"), ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "low", CreationTimestamp: metav1.NewTime(time.Unix(100, 0))},
		Spec: purelbv2.EgressSpec{Priority: 1, EgressPolicy: dests("0.0.0.0/0")}})
	seed(gvrEgressMembers, purelbNamespace, member(ref))
	seed(gvrEgressMembers, purelbNamespace, member(low))
	seed(gvrEgressEndpoints, purelbNamespace, &purelbv2.EgressEndpoints{TypeMeta: gvk("EgressEndpoints"), ObjectMeta: metav1.ObjectMeta{Name: epName, Namespace: purelbNamespace},
		Spec:   purelbv2.EgressEndpointsSpec{Address: addr.String()},
		Status: purelbv2.EgressEndpointsStatus{ObservedAnnouncers: []string{"n2"}}})
	seed(gvrEgressNodeStates, "", &purelbv2.EgressNodeState{TypeMeta: gvk("EgressNodeState"), ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:   purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{{Endpoints: epName, Address: addr.String(), Index: 1, State: "Active", SliceSize: 4096}}},
		Status: purelbv2.EgressNodeStateStatus{Capability: &purelbv2.EgressCapability{PodNodeStage: "Netfilter"}}})
	d, _ := parseDst("203.0.113.5:443/tcp")
	r, err := runTrace(t.Context(), c, "voice", "a", d, false)
	require.NoError(t, err)
	assert.Equal(t, ref.String(), r.Winner, "priority first")
	assert.Equal(t, addr.String(), r.Address)
	assert.Equal(t, "4096-8191", r.Ports)
	assert.Equal(t, "Netfilter", r.Stage)
	assert.Equal(t, "tunnel", r.Route, "the pod's node doesn't hold it")
	assert.Equal(t, []string{"n2"}, r.Holders)
	require.Len(t, r.Policies, 2)
	assert.True(t, r.Policies[0].Matches)

	// Only the low one covers this, and it has no slice here.
	d, _ = parseDst("198.51.100.5:443/tcp")
	r, err = runTrace(t.Context(), c, "voice", "a", d, false)
	require.NoError(t, err)
	assert.Equal(t, low.String(), r.Winner)
	assert.Empty(t, r.Address)
	assert.NotEmpty(t, r.Notes)
}

func TestFillLive(t *testing.T) {
	rep := &diagreport.Report{Stage: "EBPF", Addresses: []diagreport.Address{
		{Address: "192.0.2.10", Held: true, SliceSize: 4096, Slices: []diagreport.Slice{{Index: 1, Owner: diagreport.NodeID("n1")}},
			Traffic: diagreport.Traffic{OutPod: diagreport.Count{Packets: 7}}},
	}}
	l := traceLive{Node: "n1"}
	fillLive(&l, rep, "192.0.2.10", "4096-8191")
	assert.Equal(t, "EBPF", l.Stage)
	assert.True(t, l.Held)
	assert.Equal(t, "n1", l.SliceOwner)
	assert.Equal(t, uint64(7), l.Traffic.OutPod.Packets)
	l = traceLive{Node: "n1"}
	fillLive(&l, rep, "192.0.2.99", "0-1")
	assert.NotEmpty(t, l.Error)
}

func TestContainerImage(t *testing.T) {
	spec := v1.PodSpec{Containers: []v1.Container{{Name: "k8gobgp", Image: "gobgp"}, {Name: "lbnodeagent", Image: "agent"}}}
	assert.Equal(t, "agent", containerImage(spec, "lbnodeagent"), "not the sidecar ahead of it")
	assert.Empty(t, containerImage(spec, "none"))
}

func TestCleanupPod(t *testing.T) {
	p := cleanupPod("n1", "img:tag", "/var/lib/rancher/k3s/agent/etc/cni/net.d", "/opt/cni/bin")
	assert.Equal(t, "n1", p.Spec.NodeName)
	assert.Equal(t, purelbNamespace, p.Namespace)
	ct := p.Spec.Containers[0]
	assert.Equal(t, v1.PullAlways, ct.ImagePullPolicy)
	assert.Equal(t, []string{"egress-cleanup", "--node=n1"}, ct.Args)
	assert.True(t, *ct.SecurityContext.Privileged)
	var hosts []string
	for _, v := range p.Spec.Volumes {
		hosts = append(hosts, v.HostPath.Path)
	}
	assert.Contains(t, hosts, "/var/lib/rancher/k3s/agent/etc/cni/net.d", "the CNI's actual paths")
	assert.Contains(t, hosts, "/sys/fs/bpf/purelb")
}

func TestNodeChecks(t *testing.T) {
	levels := func(n diagreport.NodeChecks) map[string]int {
		out := map[string]int{}
		for _, c := range nodeChecks("n", n) {
			out[c.Status]++
		}
		return out
	}
	good := diagreport.NodeChecks{Kernel: "6.12", KernelConfigKnown: true, BPFFS: true, Forwarding4: "1", Forwarding6: "1",
		Interfaces: []diagreport.Interface{{Name: "eth0", ArpAnnounce: "2", AcceptRA: "2"}},
		Chains:     []diagreport.Chain{{Interface: "eth0", Guard: 1, Announce: 2}}}
	assert.Zero(t, levels(good)["FAIL"]+levels(good)["WARN"])

	bad := good
	bad.KernelConfigMissing = []string{"GENEVE"}
	bad.Forwarding4 = "0"
	bad.Chains = []diagreport.Chain{{Interface: "eth0", Guard: 2, Announce: 1}}
	assert.Equal(t, 3, levels(bad)["FAIL"])

	warn := good
	warn.KernelConfigKnown = false
	warn.BPFFS = false
	warn.Forwarding6 = "0"
	warn.Interfaces = []diagreport.Interface{{Name: "eth0", ArpAnnounce: "0", AcceptRA: "1"}}
	assert.Equal(t, 4, levels(warn)["WARN"], "no accept_ra warning without IPv6 forwarding")
}

func TestNodeConditionLevel(t *testing.T) {
	cond := func(typ, reason string) metav1.Condition { return metav1.Condition{Type: typ, Reason: reason} }
	assert.Equal(t, "WARN", nodeConditionLevel(cond("DatapathReady", "ArpAnnounce")))
	assert.Equal(t, "WARN", nodeConditionLevel(cond("UnderlayEncrypted", "UnderlayUnencrypted")))
	assert.Equal(t, "FAIL", nodeConditionLevel(cond("DatapathReady", "ApplyFailed")))
	assert.Equal(t, "FAIL", nodeConditionLevel(cond("DatapathCompatible", "ChainMissing")))
}
