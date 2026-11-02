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

package datapath

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// podIfindex is lo: a test run's ifindex must be a real device.
const podIfindex = 1

var (
	pod4      = netip.MustParseAddr("10.244.1.5")
	pod6      = netip.MustParseAddr("fd04:0:0:1::5")
	cluster4  = netip.MustParsePrefix("10.96.0.0/12")
	cluster6  = netip.MustParsePrefix("fd05::/112")
	inCluster = map[bool]netip.Addr{false: netip.MustParseAddr("10.96.0.10"), true: netip.MustParseAddr("fd05::a")}
)

// runOn runs prog on pkt arriving on ifindex and returns its verdict and
// mark.
func (h *harness) runOn(prog *ebpf.Program, ifindex uint32, pkt []byte) (uint32, uint32) {
	h.t.Helper()
	var in, out skbCtx
	binary.NativeEndian.PutUint32(in[40:], ifindex)
	data := make([]byte, len(pkt)+256)
	ret, err := prog.Run(&ebpf.RunOptions{Data: pkt, DataOut: data, Context: in[:], ContextOut: out[:]})
	require.NoError(h.t, err)
	return ret, out.mark()
}

func (h *harness) tag(pkt []byte) (uint32, uint32) {
	h.t.Helper()
	return h.runOn(h.o.PlPodTag, podIfindex, pkt)
}

func podAddr(f family) netip.Addr {
	if f.v6 {
		return pod6
	}
	return pod4
}

// A pod's own traffic is tagged with its pod-set id; anything else it
// sources is dropped (D20); an unknown device or a non-IP frame passes.
func TestPodTagSourceCheck(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.o.SetPodDev(podIfindex, PodDev{V4: pod4, V6: pod6, PodSet: 7}))
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			v, mark := h.tag(packet(f, podAddr(f), f.server, protoUDP, ports(5000, 443)))
			assert.Equal(t, tcxNext, v)
			assert.Equal(t, uint32(7), mark)

			before := h.count("source_check", f.v6)
			v, _ = h.tag(packet(f, f.egress, f.server, protoUDP, ports(5000, 443)))
			assert.Equal(t, uint32(tcxDrop), v, "sourcing an egress address")
			assert.Equal(t, before+1, h.count("source_check", f.v6))

		})
	}
	require.NoError(t, h.o.DeletePodDev(podIfindex))
	v, mark := h.tag(packet(families[0], egress4, server4, protoUDP, ports(5000, 443)))
	assert.Equal(t, tcxNext, v, "not a known pod device")
	assert.Zero(t, mark)
	require.NoError(t, h.o.SetPodDev(podIfindex, PodDev{V4: pod4, V6: pod6, PodSet: 7}))
	v, mark = h.tag(frame(0x0806, nil, make([]byte, 28)))
	assert.Equal(t, tcxNext, v, "ARP")
	assert.Zero(t, mark)

	// A pod with no address in a family may not use it.
	require.NoError(t, h.o.SetPodDev(podIfindex, PodDev{V4: pod4, PodSet: 7}))
	v, _ = h.tag(packet(families[1], pod6, server6, protoUDP, ports(5000, 443)))
	assert.Equal(t, uint32(tcxDrop), v)

	// Unselected: checked, not tagged.
	require.NoError(t, h.o.SetPodDev(podIfindex, PodDev{V4: pod4}))
	v, mark = h.tag(packet(families[0], pod4, server4, protoUDP, ports(5000, 443)))
	assert.Equal(t, tcxNext, v)
	assert.Zero(t, mark)
}

// Link-local and unspecified IPv6 sources pass for neighbour discovery and
// multicast listener reports (behind a Hop-by-Hop header) only, untagged.
func TestPodTagNeighbourDiscovery(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.o.SetPodDev(podIfindex, PodDev{V4: pod4, V6: pod6, PodSet: 7, Hold: true}))
	ll := netip.MustParseAddr("fe80::5")
	allNodes := netip.MustParseAddr("ff02::1")
	icmp6 := func(typ uint8) []byte { return []byte{typ, 0, 0, 0, 0, 0, 0, 0} }
	// Hop-by-Hop with Router Alert, then an MLDv2 report.
	hbh := append([]byte{protoICMPv6, 0, 5, 2, 0, 0, 1, 0}, icmp6(143)...)

	for name, pkt := range map[string][]byte{
		"NS from link-local": frame(0x86dd, nil, ipv6(ll, allNodes, protoICMPv6, icmp6(135))),
		"DAD NS from ::":     frame(0x86dd, nil, ipv6(netip.IPv6Unspecified(), allNodes, protoICMPv6, icmp6(135))),
		"RS":                 frame(0x86dd, nil, ipv6(ll, allNodes, protoICMPv6, icmp6(133))),
		"MLDv2 report (HBH)": frame(0x86dd, nil, ipv6(ll, netip.MustParseAddr("ff02::16"), 0, hbh)),
	} {
		v, mark := h.tag(pkt)
		assert.Equal(t, tcxNext, v, name)
		assert.Zero(t, mark, name)
	}
	for name, pkt := range map[string][]byte{
		"UDP from link-local":  frame(0x86dd, nil, ipv6(ll, server6, protoUDP, ports(5000, 443))),
		"echo from link-local": frame(0x86dd, nil, ipv6(ll, server6, protoICMPv6, echo(128, 1))),
	} {
		v, _ := h.tag(pkt)
		assert.Equal(t, uint32(tcxDrop), v, name)
	}
}

// A held pod's traffic reaches the cluster networks only (FailClosed).
func TestPodTagHold(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.o.SetPodDev(podIfindex, PodDev{V4: pod4, V6: pod6, PodSet: 7, Hold: true}))
	require.NoError(t, h.o.SyncCluster([]netip.Prefix{cluster4, cluster6}))
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			v, mark := h.tag(packet(f, podAddr(f), inCluster[f.v6], protoUDP, ports(5000, 53)))
			assert.Equal(t, tcxNext, v, "to a cluster network")
			assert.Equal(t, uint32(7), mark)

			before := h.count("fail_closed", f.v6)
			v, _ = h.tag(packet(f, podAddr(f), f.server, protoUDP, ports(5000, 443)))
			assert.Equal(t, uint32(tcxDrop), v, "leaving the cluster")
			assert.Equal(t, before+1, h.count("fail_closed", f.v6))
		})
	}
	require.NoError(t, h.o.SetPodDev(podIfindex, PodDev{V4: pod4, V6: pod6, PodSet: 7}))
	v, _ := h.tag(packet(families[0], pod4, server4, protoUDP, ports(5000, 443)))
	assert.Equal(t, tcxNext, v, "released")
}

// The pod devices and cluster networks read back as written.
func TestPodDevsAndClusterSync(t *testing.T) {
	o := load(t)
	require.NoError(t, o.SetPodDev(3, PodDev{V4: pod4, V6: pod6, PodSet: 2, Hold: true}))
	require.NoError(t, o.SetPodDev(4, PodDev{V6: pod6}))
	got, err := o.RecoverPodDevs()
	require.NoError(t, err)
	assert.Equal(t, map[int]PodDev{3: {V4: pod4, V6: pod6, PodSet: 2, Hold: true}, 4: {V6: pod6}}, got)
	require.NoError(t, o.DeletePodDev(3))
	require.NoError(t, o.DeletePodDev(3), "deleting a missing one is no error")

	read := func() []string {
		ps, err := o.ClusterNetworks()
		require.NoError(t, err)
		var out []string
		for _, p := range ps {
			out = append(out, p.String())
		}
		return out
	}
	require.NoError(t, o.SyncCluster([]netip.Prefix{cluster4, cluster6, netip.MustParsePrefix("10.244.1.77/16")}))
	assert.Equal(t, []string{"10.96.0.0/12", "10.244.0.0/16", "fd05::/112"}, read(), "masked")
	require.NoError(t, o.SyncCluster([]netip.Prefix{cluster6}))
	assert.Equal(t, []string{"fd05::/112"}, read())
}

// PodDevices finds a pod's host-side device from the addresses in the
// device's peer namespace, both families, without entering it; link-local
// addresses and devices without a peer namespace are left out.
func TestNetnsPodDevices(t *testing.T) {
	netnsOrSkip(t)
	node, pod := newHost(t, "node"), newHost(t, "pod")
	require.NoError(t, node.nl.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "vpod0"}, PeerName: "eth0",
		PeerNamespace: netlink.NsFd(pod.ns)}))
	require.NoError(t, node.nl.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}}))
	eth0, err := pod.nl.LinkByName("eth0")
	require.NoError(t, err)
	for _, a := range []string{"10.244.1.5/24", "fd04:0:0:1::5/64"} {
		addr, err := netlink.ParseAddr(a)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, pod.nl.AddrAdd(eth0, addr))
	}
	require.NoError(t, pod.nl.LinkSetUp(eth0)) // gives it a link-local address too
	vpod, err := node.nl.LinkByName("vpod0")
	require.NoError(t, err)
	require.NoError(t, node.nl.LinkSetUp(vpod))

	var devs map[netip.Addr]PodDevice
	require.Eventually(t, func() bool {
		require.NoError(t, node.in(func() error {
			devs, err = PodDevices(node.nl)
			return err
		}))
		return len(devs) == 2
	}, 5*time.Second, 100*time.Millisecond, "got %v", devs)
	want := PodDevice{Ifindex: vpod.Attrs().Index, Name: "vpod0", Kind: "veth"}
	assert.Equal(t, map[netip.Addr]PodDevice{pod4: want, pod6: want}, devs)
}
