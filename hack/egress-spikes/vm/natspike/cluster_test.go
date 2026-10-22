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

package natspike

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// TestSpike2NatOnNode runs the eBPF stage on a real cluster node, as root,
// for hack/egress-spikes/cluster/ebpf-stage.sh: pl_pod_out on the uplink's
// tcx egress and pl_pod_reply on its tcx ingress, at the head or the tail
// of the chain, so the CNI's own programs on the uplink run after or before
// ours. It logs both chains with program names, waits while the script
// sends traffic, and prints the NAT counters and table.
//
//	NAT_POD_DEV     host-side device of the pod (required)
//	NAT_UPLINK      the node's uplink
//	NAT_POD4/6      the pod's addresses
//	NAT_EG4/6       the egress addresses (already on the uplink)
//	NAT_ANCHOR      head | tail
//	NAT_SECONDS     how long to wait (default 20)
//	NAT_KEY         src: register the pod by address (pod_src), not by device
//	NAT_SLICE_START the slice's first port (default sliceStart)
//	NAT_SLICE_SIZE  the slice's size, a power of two (default sliceSize)
//	NAT_ATTACH      pod: pl_pod_out at the head of each pod device's tcx
//	                ingress (before the CNI's program) instead of the uplink
//	NAT_CHAINS      only list the tcx chains of these devices and return
//
// NAT_POD_DEV, NAT_POD4 and NAT_POD6 take space-separated lists, one entry
// per pod, so several pods on the node share the stage and the slice.
func TestSpike2NatOnNode(t *testing.T) {
	if devs := os.Getenv("NAT_CHAINS"); devs != "" {
		for _, d := range strings.Fields(devs) {
			l, err := netlink.LinkByName(d)
			require.NoError(t, err)
			t.Logf("NAT CHAIN %s egress: %s", d, chain(l.Attrs().Index, ebpf.AttachTCXEgress))
			t.Logf("NAT CHAIN %s ingress: %s", d, chain(l.Attrs().Index, ebpf.AttachTCXIngress))
		}
		return
	}
	podDevs := strings.Fields(os.Getenv("NAT_POD_DEV"))
	if len(podDevs) == 0 {
		t.Skip("run on a cluster node by hack/egress-spikes/cluster/ebpf-stage.sh")
	}
	pod4s, pod6s := strings.Fields(os.Getenv("NAT_POD4")), strings.Fields(os.Getenv("NAT_POD6"))
	require.Len(t, pod4s, len(podDevs))
	require.Len(t, pod6s, len(podDevs))
	secs, _ := strconv.Atoi(os.Getenv("NAT_SECONDS"))
	if secs == 0 {
		secs = 20
	}
	start, size := envU16(t, "NAT_SLICE_START", sliceStart), envU16(t, "NAT_SLICE_SIZE", sliceSize)
	ul, err := netlink.LinkByName(os.Getenv("NAT_UPLINK"))
	require.NoError(t, err)

	o := &natObjects{}
	require.NoError(t, loadNatObjects(o, nil))
	defer o.Close()
	var pls []netlink.Link
	for i, podDev := range podDevs {
		pl, err := netlink.LinkByName(podDev)
		require.NoError(t, err)
		pls = append(pls, pl)
		pc := natPodCfg{
			V4: ip16(pod4s[i])[0], V6: ip16(pod6s[i]),
			Eg4: ip16(os.Getenv("NAT_EG4"))[0], Eg6: ip16(os.Getenv("NAT_EG6")),
			Start: start, Size: size}
		if os.Getenv("NAT_KEY") == "src" {
			// netkit: the pod is found by its source address (pod_src), not by
			// the ingress device.
			require.NoError(t, o.PodSrc.Put(natAddrKey{Addr: ip16(pod4s[i]), Family: 2}, pc))
			require.NoError(t, o.PodSrc.Put(natAddrKey{Addr: ip16(pod6s[i]), Family: 10}, pc))
		} else {
			require.NoError(t, o.PodNat.Put(uint32(pl.Attrs().Index), pc))
		}
	}
	t.Logf("NAT SLICE start=%d size=%d pods=%d", start, size, len(podDevs))
	for _, e := range []struct {
		a  string
		af uint8
	}{{os.Getenv("NAT_EG4"), 2}, {os.Getenv("NAT_EG6"), 10}} {
		require.NoError(t, o.EgressAddrs.Put(natAddrKey{Addr: ip16(e.a), Family: e.af},
			natAddrCfg{Base: 1024, Shift: 12, Announced: 1}))
	}

	anchor := link.Head()
	if os.Getenv("NAT_ANCHOR") == "tail" {
		anchor = link.Tail()
	}
	type att struct {
		prog *ebpf.Program
		ifx  int
		at   ebpf.AttachType
	}
	atts := []att{{o.PlPodReply, ul.Attrs().Index, ebpf.AttachTCXIngress}}
	if os.Getenv("NAT_ATTACH") == "pod" {
		// Translate on the pod's host-side device, before the CNI's program.
		for _, pl := range pls {
			atts = append(atts, att{o.PlPodOut, pl.Attrs().Index, ebpf.AttachTCXIngress})
		}
	} else {
		atts = append(atts, att{o.PlPodOut, ul.Attrs().Index, ebpf.AttachTCXEgress})
	}
	for _, a := range atts {
		l, err := link.AttachTCX(link.TCXOptions{Program: a.prog, Interface: a.ifx, Attach: a.at, Anchor: anchor})
		require.NoError(t, err)
		defer l.Close()
	}
	t.Logf("NAT CHAIN %s egress: %s", ul.Attrs().Name, chain(ul.Attrs().Index, ebpf.AttachTCXEgress))
	t.Logf("NAT CHAIN %s ingress: %s", ul.Attrs().Name, chain(ul.Attrs().Index, ebpf.AttachTCXIngress))
	for _, pl := range pls {
		t.Logf("NAT CHAIN %s ingress: %s", pl.Attrs().Name, chain(pl.Attrs().Index, ebpf.AttachTCXIngress))
		t.Logf("NAT CHAIN %s egress: %s", pl.Attrs().Name, chain(pl.Attrs().Index, ebpf.AttachTCXEgress))
	}
	fmt.Println("NAT READY")

	time.Sleep(time.Duration(secs) * time.Second)

	t.Logf("NAT RESULT anchor=%s counters:%s entries=%d", os.Getenv("NAT_ANCHOR"), natStats(t, o.Stats), countNat(t, o.Nat))
	var k natNatKey
	var v natNatVal
	for it := o.Nat.Iterate(); it.Next(&k, &v); {
		t.Logf("NAT ENTRY dir=%d proto=%d fam=%d laddr=%s rport=%d lport=%d -> addr=%s port=%d state=%d",
			k.Dir, k.Proto, k.Family, addrStr(k.Laddr, k.Family), k.Rport, k.Lport, addrStr(v.Addr, k.Family), v.Port, v.State)
	}
}

// envU16 reads a uint16 from the environment, or returns def.
func envU16(t *testing.T, name string, def uint16) uint16 {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.ParseUint(s, 10, 16)
	require.NoError(t, err, name)
	return uint16(v)
}

// addrStr formats the four network-order words the maps use.
func addrStr(w [4]uint32, family uint8) string {
	var b [16]byte
	for i := 0; i < 4; i++ {
		b[i*4], b[i*4+1], b[i*4+2], b[i*4+3] = byte(w[i]), byte(w[i]>>8), byte(w[i]>>16), byte(w[i]>>24)
	}
	if family == 2 {
		return netip.AddrFrom4([4]byte(b[:4])).String()
	}
	return netip.AddrFrom16(b).String()
}

// chain lists a device's tcx chain head first, with program names.
func chain(ifindex int, at ebpf.AttachType) string {
	r, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: at})
	if err != nil {
		return "query failed: " + err.Error()
	}
	var s []string
	for _, p := range r.Programs {
		name := "?"
		if prog, err := ebpf.NewProgramFromID(p.ID); err == nil {
			if info, err := prog.Info(); err == nil {
				name = info.Name
			}
			prog.Close()
		}
		s = append(s, fmt.Sprintf("%d:%s", p.ID, name))
	}
	return "[" + strings.Join(s, " ") + "]"
}
