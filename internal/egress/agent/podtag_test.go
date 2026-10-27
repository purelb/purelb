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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// podNetns makes a pod: a network namespace with eth0 holding addrs, its
// host side vethName in this one. It returns the host side's ifindex.
func podNetns(t *testing.T, vethName string, addrs ...string) int {
	t.Helper()
	here, err := netns.Get()
	require.NoError(t, err)
	defer here.Close()
	pod, err := netns.New()
	require.NoError(t, err)
	require.NoError(t, netns.Set(here))
	t.Cleanup(func() { _ = pod.Close() })
	require.NoError(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: vethName}, PeerName: "eth0",
		PeerNamespace: netlink.NsFd(pod)}))
	h, err := netlink.NewHandleAt(pod, unix.NETLINK_ROUTE)
	require.NoError(t, err)
	defer h.Close()
	eth0, err := h.LinkByName("eth0")
	require.NoError(t, err)
	for _, a := range addrs {
		addr, err := netlink.ParseAddr(a)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, h.AddrAdd(eth0, addr))
	}
	require.NoError(t, h.LinkSetUp(eth0))
	host, err := netlink.LinkByName(vethName)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(host))
	return host.Attrs().Index
}

// The real apply tags a pod's host-side device: its pod_devs entry and
// pl_pod_tag, pinned; the cluster networks from the API and the routes; a
// restart keeps both until the pods are planned, then takes the link over
// in place; a pod gone loses both.
func TestNetnsApplyDatapathPods(t *testing.T) {
	dir := sandbox(t)
	eth0, err := netlink.LinkByName("eth0")
	require.NoError(t, err)
	require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth0.Attrs().Index, Gw: net.ParseIP("10.0.0.254")}))
	flannel := dummy(t, "flannel.1")
	require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: flannel, Dst: cidrNet("10.244.2.0/24")}))
	veth := podNetns(t, "vpod0", "10.244.1.5/24", "fd04:0:0:1::5/64")
	pod4, pod6 := netip.MustParseAddr("10.244.1.5"), netip.MustParseAddr("fd04:0:0:1::5")
	pinned := func() bool {
		_, err := os.Stat(filepath.Join(dir, "pl_pod_tag-vpod0"))
		return err == nil
	}

	a := newNetnsAgent(dir)
	w := want(node("node-b", "10.0.0.2", "fd00::2"))
	w.cluster = []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")}
	w.podsKnown = true
	w.pods = []dpPod{{name: "voice/a", v4: pod4, v6: pod6, podSet: 3, hold: true},
		{name: "voice/unwired", v4: netip.MustParseAddr("10.244.1.9")}}
	apply(t, a, w)
	tag := progID(t, a.dp.objs.PlPodTag)
	assert.Equal(t, []ebpf.ProgramID{tag}, chain(t, veth, ebpf.AttachTCXIngress))
	assert.True(t, pinned())
	devs, err := a.dp.objs.RecoverPodDevs()
	require.NoError(t, err)
	assert.Len(t, devs, 1, "a pod with no device yet has no entry")
	assert.Equal(t, uint8(3), devs[veth].PodSet)
	assert.True(t, devs[veth].Hold)
	cluster, err := a.dp.objs.ClusterNetworks()
	require.NoError(t, err)
	assert.Contains(t, cluster, netip.MustParsePrefix("10.96.0.0/12"), "from the API")
	assert.Contains(t, cluster, netip.MustParsePrefix("10.244.2.0/24"), "from the routes")
	assert.NotContains(t, cluster, netip.MustParsePrefix("10.0.0.0/24"), "the uplink's subnet")
	assert.Equal(t, map[netip.Addr]uint8{pod4: 3, pod6: 3}, recoveredPodSets(dir))

	// A restart: until the pods are planned the kernel keeps what it has;
	// then the pinned link is taken over in place.
	id, _ := pinnedLink(t, filepath.Join(dir, "pl_pod_tag-vpod0"))
	closeAgent(a)
	b := newNetnsAgent(dir)
	early := w
	early.pods, early.podsKnown = nil, false
	apply(t, b, early)
	assert.Equal(t, []ebpf.ProgramID{tag}, chain(t, veth, ebpf.AttachTCXIngress),
		"the earlier agent's program, on the shared pinned maps")
	devs, err = b.dp.objs.RecoverPodDevs()
	require.NoError(t, err)
	assert.Len(t, devs, 1, "kept")
	apply(t, b, w)
	again, _ := pinnedLink(t, filepath.Join(dir, "pl_pod_tag-vpod0"))
	assert.Equal(t, id, again, "the same link")
	assert.Equal(t, []ebpf.ProgramID{progID(t, b.dp.objs.PlPodTag)}, chain(t, veth, ebpf.AttachTCXIngress),
		"now running this agent's program")

	// Gone: entry, link and pin.
	w.pods = nil
	apply(t, b, w)
	assert.Empty(t, chain(t, veth, ebpf.AttachTCXIngress))
	assert.False(t, pinned())
	devs, err = b.dp.objs.RecoverPodDevs()
	require.NoError(t, err)
	assert.Empty(t, devs)
	closeAgent(b)
}

// The datapath's desired state carries this node's pods, planned, and the
// cluster networks the API shows: podCIDRs, node addresses, Service CIDRs,
// the API server, excludedNetworks, podNetworks and the fixed ones.
func TestDatapathGetsPodsAndCluster(t *testing.T) {
	const ep = "e-voice.sip-0a0b0c0d"
	ens := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode},
		Spec: purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{
			{Endpoints: ep, Address: "192.0.2.1", Index: 2, State: "Pending", SliceSize: 4096}}}}
	ea := &purelbv2.EgressAddress{ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: ep,
		Labels: map[string]string{egress.LabelKind: egress.KindEgress, egress.LabelNamespace: "voice", egress.LabelName: "sip"}},
		Spec: purelbv2.EgressAddressSpec{State: "Active", Address: "192.0.2.1", Family: "IPv4", ServiceGroup: "default",
			PoolType: "Local", Placement: "SingleAnnouncer"}}
	eg := &purelbv2.Egress{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip", UID: "uid-sip"},
		Spec: purelbv2.EgressSpec{PodSelector: &metav1.LabelSelector{MatchLabels: appA}}}
	n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.2.0/24", "fd04:0:0:2::/64"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.2"},
			{Type: corev1.NodeExternalIP, Address: "198.51.100.2"}, {Type: corev1.NodeHostName, Address: "n2"}}}}
	cidrs := &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"},
		Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12", "fd05::/112"}}}
	api := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes",
		Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.200"}}}}
	f := newFixture(t, true, testNodeObj, n2, cidrs, api, ens, ea, eg, pnPod("voice", "a-1", appA, "10.1.0.5", "fd01::5"))
	f.kube.Resources = append(f.kube.Resources, &metav1.APIResourceList{GroupVersion: networkingv1.SchemeGroupVersion.String(),
		APIResources: []metav1.APIResource{{Name: "servicecidrs"}}})
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true, ExcludedNetworks: []string{"203.0.113.0/24"},
		PodNetworks: []string{"fd99::/64"}}))

	var w dpWant
	require.Eventually(t, func() bool {
		select {
		case w = <-f.wants:
		default:
		}
		return w.podsKnown && len(w.pods) == 1
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "voice/a-1", w.pods[0].name)
	assert.Equal(t, netip.MustParseAddr("10.1.0.5"), w.pods[0].v4)
	assert.Equal(t, netip.MustParseAddr("fd01::5"), w.pods[0].v6)
	assert.NotZero(t, w.pods[0].podSet)
	assert.True(t, w.pods[0].hold, "FailClosed, its slot Pending")
	for _, p := range []string{"10.244.2.0/24", "fd04:0:0:2::/64", "10.0.0.1/32", "10.0.0.2/32", "198.51.100.2/32", "fd00::1/128",
		"10.96.0.0/12", "fd05::/112", "192.0.2.200/32", "203.0.113.0/24", "fd99::/64", "169.254.0.0/16", "ff00::/8"} {
		assert.Contains(t, w.cluster, netip.MustParsePrefix(p))
	}
	assert.True(t, w.nodeAddrs[netip.MustParseAddr("198.51.100.2")])
}

// A restarted agent's first plan keeps the pod-set id the pinned maps give
// its pod's address.
func TestSeedPodSets(t *testing.T) {
	p := pnEgress("voice", "p", 100, nil)
	in := pnIn("Active", []*corev1.Pod{pnPod("voice", "a", appA, "10.1.0.5")}, p)
	assert.Equal(t, uint8(2), planPodNode(in, nil).pods["voice/a"].podSet, "slot 1, pod-set 2 when fresh")
	pn := planPodNode(in, seedPodSets(in, map[netip.Addr]uint8{netip.MustParseAddr("10.1.0.5"): 9}))
	assert.Equal(t, uint8(9), pn.pods["voice/a"].podSet)
	_, slotOn9 := pn.slots[9]
	assert.False(t, slotOn9, "the slot takes another id")
}
