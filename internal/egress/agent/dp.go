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
	"net/netip"
	"slices"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// keyDatapath recomputes the datapath's desired state.
const keyDatapath = "agent:datapath"

// datapathRetry is how long a failed datapath apply waits to try again.
const datapathRetry = 30 * time.Second

// dpWant is the datapath's desired state, built on the queue goroutine and
// applied whole on the datapath goroutine.
type dpWant struct {
	spec  purelbv2.LBNodeAgentEgressSpec
	self  string
	nodes []dpNode
}

// dpNode is one node's underlay: its InternalIPs (D3).
type dpNode struct {
	name   string
	v4, v6 netip.Addr
}

// dpState is what the datapath goroutine owns: the loaded programs, the
// tunnel and the links attached to it.
type dpState struct {
	nl     *netlink.Handle
	pinDir string
	objs   *datapath.Objects
	mode   string
	port   uint16
	tun    netlink.Link
	links  map[string]link.Link
	nodes  map[uint64]datapath.Node
}

// syncDatapath hands the datapath its desired state: this node's egress
// spec and every node's underlay addresses.
func (a *Agent) syncDatapath() {
	s := a.on
	if s == nil || !s.synced.Load() {
		return
	}
	w := dpWant{spec: s.spec, self: a.node}
	for _, obj := range s.nodes.List() {
		n := obj.(*corev1.Node)
		u := underlay(s.nodes, n.Name)
		if u == nil {
			continue
		}
		d := dpNode{name: n.Name}
		d.v4, _ = netip.ParseAddr(u.IPv4)
		d.v6, _ = netip.ParseAddr(u.IPv6)
		w.nodes = append(w.nodes, d)
	}
	slices.SortFunc(w.nodes, func(x, y dpNode) int {
		if x.name < y.name {
			return -1
		}
		if x.name > y.name {
			return 1
		}
		return 0
	})
	a.runner.submit(func() {
		c, err := a.apply(w)
		if err != nil {
			logging.Info(a.logger, "op", "egressDatapath", "node", a.node, "error", err,
				"msg", "the egress datapath is not ready; retrying")
			time.AfterFunc(datapathRetry, func() { a.add(keyDatapath, false) })
		}
		a.capability.Store(c)
		a.add(keyNode, false)
	})
}

// applyDatapath brings the kernel in line with w and reports this node's
// capability: egress-capable once the datapath is up. Datapath goroutine
// only.
func (a *Agent) applyDatapath(w dpWant) (*purelbv2.EgressCapability, error) {
	c := probeCapability(w.spec)
	if !c.EgressCapable {
		return c, fmt.Errorf("kernel %s is older than egress needs", c.Kernel)
	}
	c.EgressCapable = false
	st := a.dp
	if st.nl == nil {
		h, err := netlink.NewHandle()
		if err != nil {
			return c, err
		}
		st.nl = h
	}
	if st.objs == nil {
		if bpfpin.Usable(bpfpin.Dir) {
			st.pinDir = bpfpin.Dir
		}
		objs, err := datapath.Load(st.pinDir)
		if err != nil {
			return c, err
		}
		st.objs, st.links, st.nodes = objs, map[string]link.Link{}, map[uint64]datapath.Node{}
	}

	self := datapath.NodeID(w.self)
	port := uint16(6082)
	if w.spec.TunnelPort != 0 {
		port = uint16(w.spec.TunnelPort)
	}
	mode, tun, err := datapath.EnsureTunnel(st.nl, w.spec.TunnelEncapsulation, port, datapath.TunnelMAC(self))
	if err != nil {
		return c, err
	}
	if st.tun == nil || st.tun.Attrs().Index != tun.Attrs().Index {
		// A new device: links to the old one went with it.
		for name, l := range st.links {
			_ = l.Close()
			delete(st.links, name)
		}
	}
	st.mode, st.port, st.tun = mode, port, tun
	c.TunnelEncapsulation = mode

	// Every node first, so no tunnel packet from a node is taken for a
	// forged one once pl_announce_tun is attached (design 6.5).
	var peers6, underlay []netip.Addr
	want := map[uint64]datapath.Node{}
	selfNode := datapath.Node{}
	for _, n := range w.nodes {
		id := datapath.NodeID(n.name)
		dn := datapath.Node{V4: n.v4, V6: n.v6, MAC: datapath.TunnelMAC(id)}
		if n.name == w.self {
			selfNode = dn
			continue
		}
		want[id] = dn
		for _, ip := range []netip.Addr{n.v4, n.v6} {
			if ip.IsValid() {
				underlay = append(underlay, ip)
			}
		}
		if n.v6.IsValid() {
			peers6 = append(peers6, n.v6)
		}
	}
	var errs []error
	for id, n := range st.nodes {
		if _, ok := want[id]; !ok || !sameNode(n, want[id]) {
			errs = append(errs, st.objs.DeleteNode(id, n))
			delete(st.nodes, id)
		}
	}
	for id, n := range want {
		if _, ok := st.nodes[id]; ok {
			continue
		}
		if err := st.objs.SetNode(id, n); err != nil {
			errs = append(errs, err)
			continue
		}
		st.nodes[id] = n
	}
	errs = append(errs, datapath.SyncThrows(st.nl, underlay))

	shift := uint8(0)
	switch w.spec.MarkField {
	case "0x0000ff00":
		shift = 8
	case "0x00ff0000":
		shift = 16
	case "0xff000000":
		shift = 24
	}
	errs = append(errs, st.objs.SetConfig(datapath.Config{Self: self, TunIfindex: tun.Attrs().Index,
		TunMTU: uint32(datapath.TunnelMTU(st.nl, mode, peers6)), TunMAC: datapath.TunnelMAC(self),
		L2: mode == datapath.ModeL2, MarkShift: shift,
		Underlay4: selfNode.V4.IsValid(), Underlay6: selfNode.V6.IsValid()}))
	if err := errors.Join(errs...); err != nil {
		return c, err
	}

	for _, x := range []struct {
		name string
		prog *ebpf.Program
		hook ebpf.AttachType
	}{
		{"pl_announce-" + datapath.TunnelName, st.objs.PlAnnounceTun, ebpf.AttachTCXIngress},
		{"pl_steer-" + datapath.TunnelName, st.objs.PlSteer, ebpf.AttachTCXEgress},
	} {
		if _, ok := st.links[x.name]; ok {
			continue
		}
		l, err := datapath.AttachTCX(st.pinDir, x.name, tun.Attrs().Index, x.prog, x.hook, 0)
		if err != nil {
			return c, fmt.Errorf("attaching %s: %w", x.name, err)
		}
		st.links[x.name] = l
	}
	c.EgressCapable = true
	logging.Debug(a.logger, "op", "egressDatapath", "node", a.node, "tunnel", mode, "port", port,
		"nodes", len(st.nodes), "pinned", st.pinDir != "", "msg", "datapath applied")
	return c, nil
}

func sameNode(a, b datapath.Node) bool {
	return a.V4 == b.V4 && a.V6 == b.V6 && a.MAC.String() == b.MAC.String()
}
