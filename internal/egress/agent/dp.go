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
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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
	// pools are this node's pool interfaces by ifindex: where its local
	// pools' egress addresses are placed (design 6.9).
	pools map[int]dpPool
	// hold is the egress addresses' entries and the acks they earn (D6).
	hold holdings
	// guardLinks are the interfaces the address guard is attached to and
	// guardProgs its programs: pl_announce's uplinks, right behind it.
	guardLinks []int
	guardProgs []ebpf.ProgramID
	// cluster are the cluster networks the API shows, nodeAddrs every
	// Node's addresses (for the route rule, design 6.3).
	cluster   []netip.Prefix
	nodeAddrs map[netip.Addr]bool
	// pods are this node's pods for pl_pod_tag, and stage the pod-node
	// stage's nft program; podsKnown is false until the pod-node state is
	// planned, which keeps what the kernel has (D12).
	pods      []dpPod
	stage     *datapath.PodStage
	podsKnown bool
	// steer is where each of stage's slots goes (D1, design 6.8).
	steer map[uint8]dpSteer
	// retired are the mark values no longer used that conntrack may
	// carry; draining this node's Draining slices by slot id, and
	// drainedNoSlot those with no slot, which have no flows (§5.6).
	retired       []uint8
	draining      map[uint8]purelbv2.EgressDrained
	drainedNoSlot []purelbv2.EgressDrained
	// specGen is the EgressNodeState spec generation the pod-node state
	// and the holdings were both computed from; 0 when they differ or the
	// state isn't whole (a slot left without an id). Installed, it is
	// acked (D6).
	specGen int64
}

// dpPool is one pool interface and the families placed on it.
type dpPool struct {
	name   string
	v4, v6 bool
}

// dpNode is one node's underlay: its InternalIPs (D3).
type dpNode struct {
	name   string
	v4, v6 netip.Addr
}

// dpState is what the datapath goroutine owns: the loaded programs, the
// tunnel and the links attached to it.
type dpState struct {
	nl *netlink.Handle
	// pinRoot is where links and maps are pinned when it is a bpffs
	// (bpfpin.Dir; tests use their own); pinDir is pinRoot once it was.
	pinRoot string
	pinDir  string
	objs    *datapath.Objects
	mode    string
	port    uint16
	tun     netlink.Link
	links   map[string]link.Link
	linkIfs map[string]int // each link's interface
	// unparsed are the uplinks pl_announce can't parse, as last logged.
	unparsed map[string]bool
	// encryption is the node-to-node encryption last found (D30).
	encryption []string
	nodes      map[uint64]datapath.Node
	// nft is the nftables connection (tests make their own); table what
	// was last written through it, its MSS from the tunnel in tableMode.
	nft       *nftables.Conn
	table     *datapath.Table
	tableMode string
	// helpers are the helpers found usable, by name, protocol and family.
	helpers map[helperKey]bool
	// portless are the port-less counts per address and when each last
	// rose; podFaults the pod-node faults they and missing helpers make.
	portless  map[netip.Addr]portlessSeen
	podFaults map[netip.Addr]purelbv2.EgressAddressFault
	// pools numbers the pool interfaces (ifindex: n, table 7900 + n);
	// reports is what was last logged about each.
	pools   map[int]uint32
	reports map[int]poolReport
	// addrs are the address entries in the maps.
	addrs map[netip.Addr]*datapath.AddressEntries
	// netkit are the netkit pod devices pl_pod_tag skips, as last logged.
	netkit map[string]bool
	// podIPs are the pods' addresses last applied; gone those of pods
	// since gone whose flows aren't deleted yet.
	podIPs map[netip.Addr]bool
	gone   map[netip.Addr]bool
	// shift is the mark byte in use (D18), from the pins until the
	// pod-node state is planned; movedFrom the one it left, whose flows
	// aren't reset yet; marks the other rules' mark uses as last read, and
	// markConflict their conflict with it.
	shift        uint8
	movedFrom    *uint8
	marks        *markRead
	markConflict string
	// prohibited are the slots routed to prohibit; fwd each family's
	// flow window, IPv4 then IPv6, and forwardBlocked the families
	// reported (D25).
	prohibited     map[uint8]bool
	fwd            [2]fwdWindow
	forwardBlocked []string
}

// poolReport is what a pool interface lacks: a default route in a family
// placed on it (NoGateway), or arp_announce 2 (IPv4).
type poolReport struct {
	noGateway4, noGateway6 bool
	arpAnnounce            int
}

// syncDatapath hands the datapath its desired state: this node's egress
// spec and every node's underlay addresses.
func (a *Agent) syncDatapath() {
	s := a.on
	if s == nil || !s.synced.Load() {
		return
	}
	w := dpWant{spec: s.spec, self: a.node, hold: a.syncHoldings(s)}
	w.pools = w.hold.pools
	a.shared = w.hold.shared
	a.syncPlacement(w.hold)
	if !maps.Equal(a.faults, w.hold.faults) {
		a.faults = w.hold.faults
		a.add(keyNode, false)
	}
	if a.guard != nil {
		w.guardLinks, w.guardProgs = a.guard.Interfaces(), a.guard.ProgramIDs()
	}
	w.cluster, w.nodeAddrs = a.clusterAPI(s), nodeAddrs(s)
	if pn := a.podnode; pn != nil {
		w.steer = a.steering(s, a.stageWant)
		w.pods, w.stage, w.podsKnown = a.podsWant, steeredStage(a.stageWant, w.steer), true
		w.retired = slices.Sorted(maps.Keys(pn.retired))
		if ns, err := s.nodeStates.Get(a.node); err == nil {
			// The holdings above read this same spec.
			w.draining, w.drainedNoSlot = drainWant(ns.Spec, pn)
			w.specGen = ackable(ns.Generation, pn)
		}
	}
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
		h := health(a.dp, w, err)
		h.unreachable, h.probed = a.unreachable(a.dp)
		a.health.Store(h)
		if err != nil {
			logging.Info(a.logger, "op", "egressDatapath", "node", a.node, "error", err,
				"msg", "the egress datapath is not ready; retrying")
			time.AfterFunc(datapathRetry, func() { a.add(keyDatapath, false) })
		} else {
			// Installed: the generations it covers may be acked, and the
			// addresses it holds placed.
			acks := slices.Clone(w.hold.acks)
			for i := range acks {
				acks[i].MTU = int32(holderMTU(a.dp, acks[i].Address))
			}
			a.acks.Store(&acks)
			if w.specGen != 0 {
				a.specAck.Store(w.specGen)
			}
			a.installedHeld(w)
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
		// Route only: with no family, NewHandle also opens xfrm and
		// netfilter sockets and fails where either is unavailable.
		h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return c, fmt.Errorf("opening a netlink socket: %w", err)
		}
		st.nl = h
	}
	if st.objs == nil {
		if bpfpin.Usable(st.pinRoot) {
			st.pinDir = st.pinRoot
		}
		objs, err := datapath.Load(st.pinDir)
		if err != nil {
			return c, err
		}
		// Pinned maps come back holding an earlier agent's nodes: start
		// from those, so the ones gone meanwhile are pruned below.
		nodes, err := objs.RecoverNodes()
		if err != nil {
			_ = objs.Close()
			return c, fmt.Errorf("reading the node map: %w", err)
		}
		// And its pool interface numbers, which addresses' tables use.
		pools, err := objs.RecoverPoolInterfaces()
		if err != nil {
			_ = objs.Close()
			return c, fmt.Errorf("reading the pool interface map: %w", err)
		}
		addrs, err := objs.RecoverAddresses()
		if err != nil {
			_ = objs.Close()
			return c, fmt.Errorf("reading the address maps: %w", err)
		}
		// And its mark byte: its table and flows use it.
		shift, err := objs.MarkShift()
		if err != nil {
			_ = objs.Close()
			return c, fmt.Errorf("reading the mark byte: %w", err)
		}
		st.shift = shift
		st.objs, st.links, st.nodes, st.pools, st.addrs = objs, map[string]link.Link{}, nodes, pools, addrs
		st.linkIfs = map[string]int{}
		st.reports = map[int]poolReport{}
		// And its pods' addresses: a pod gone while no agent ran still has
		// its flows deleted.
		st.podIPs, st.gone = map[netip.Addr]bool{}, map[netip.Addr]bool{}
		if devs, err := objs.RecoverPodDevs(); err == nil {
			for _, d := range devs {
				for _, ip := range []netip.Addr{d.V4, d.V6} {
					if ip.IsValid() {
						st.podIPs[ip] = true
					}
				}
			}
		}
	}

	self := datapath.NodeID(w.self)
	port := uint16(6082)
	if w.spec.TunnelPort != 0 {
		port = uint16(w.spec.TunnelPort)
	}
	// The cluster networks: the stage's sets and the target tables'
	// throws (design 6.3).
	routes, err := datapath.ClusterRoutes(st.nl, w.nodeAddrs)
	if err != nil {
		return c, fmt.Errorf("reading the routes: %w", err)
	}
	cluster := append(slices.Clone(w.cluster), routes...)
	if w.podsKnown {
		a.unflushLive(w)
	}
	a.syncMarkByte(st, w)
	c.MarkField = fieldName(st.shift)
	// The hardening chains before the tunnel opens its port (design 6.12),
	// the stage's MSS from the mode last resolved; again once the tunnel
	// resolves another.
	if err := a.syncTable(st, w, port, st.mode, cluster); err != nil {
		return c, err
	}
	mode, tun, err := datapath.EnsureTunnel(st.nl, w.spec.TunnelEncapsulation, port, datapath.TunnelMAC(self))
	if err != nil {
		return c, err
	}
	if mode != st.tableMode {
		if err := a.syncTable(st, w, port, mode, cluster); err != nil {
			return c, err
		}
	}
	if st.tun == nil || st.tun.Attrs().Index != tun.Attrs().Index {
		// A new device: links to the old one went with it.
		for name, l := range st.links {
			if st.tun != nil && st.linkIfs[name] == st.tun.Attrs().Index {
				_ = l.Close()
				delete(st.links, name)
				delete(st.linkIfs, name)
			}
		}
	}
	for name, l := range st.links {
		// Detached by something else: attached again below.
		if info, err := l.Info(); err != nil || info.TCX() == nil || int(info.TCX().Ifindex) != st.linkIfs[name] {
			logging.Info(a.logger, "op", "egressDatapath", "node", a.node, "link", name,
				"msg", "a datapath link was detached outside the agent; attaching it again")
			_ = l.Close()
			delete(st.links, name)
			delete(st.linkIfs, name)
		}
	}
	st.mode, st.port, st.tun = mode, port, tun
	c.TunnelEncapsulation = mode

	// Every node first, so no tunnel packet from a node is taken for a
	// forged one once pl_announce_tun is attached (design 6.5).
	var peers6 []netip.Addr
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
		if n.v6.IsValid() {
			peers6 = append(peers6, n.v6)
		}
	}
	var errs []error
	for id, n := range st.nodes {
		if _, ok := want[id]; !ok || !sameNode(n, want[id]) {
			// Kept on failure, so the next apply retries the delete.
			if err := st.objs.DeleteNode(id, n); err != nil {
				errs = append(errs, err)
				continue
			}
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
	st.encryption = underlayEncryption(st.nl)
	errs = append(errs, a.syncPools(st, w, cluster))
	// A local address forwards transit in its pool interface's table.
	addrs := maps.Clone(w.hold.addrs)
	for addr, ifindex := range w.hold.ifaces {
		n, ok := st.pools[ifindex]
		if !ok {
			errs = append(errs, fmt.Errorf("address %s: pool interface %d has no table", addr, ifindex))
			delete(addrs, addr)
			continue
		}
		e := *addrs[addr]
		e.Table = datapath.PoolTableBase + n
		addrs[addr] = &e
	}
	// One whose EgressEndpoints aren't read yet keeps what it has (D12).
	for addr := range w.hold.pending {
		if e, ok := st.addrs[addr]; ok {
			addrs[addr] = e
		}
	}
	errs = append(errs, syncAddresses(st, addrs))

	errs = append(errs, st.objs.SetConfig(datapath.Config{Self: self, TunIfindex: tun.Attrs().Index,
		TunMTU: uint32(datapath.TunnelMTU(st.nl, mode, peers6)), TunMAC: datapath.TunnelMAC(self),
		L2: mode == datapath.ModeL2, MarkShift: st.shift,
		Underlay4: selfNode.V4.IsValid(), Underlay6: selfNode.V6.IsValid(), Self4: selfNode.V4, Self6: selfNode.V6}))
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
		l, err := datapath.AttachTCX(st.pinDir, x.name, tun.Attrs().Index, x.prog, x.hook, nil)
		if err != nil {
			return c, fmt.Errorf("attaching %s: %w", x.name, err)
		}
		st.links[x.name], st.linkIfs[x.name] = l, tun.Attrs().Index
	}
	if err := a.syncUplinks(st, w); err != nil {
		return c, err
	}
	if err := a.syncPodDevs(st, w); err != nil {
		return c, err
	}
	// Until the pod-node state is planned (an agent restart), the rules,
	// tables and steering an earlier agent left are kept (D12).
	if w.podsKnown {
		if err := a.syncSteering(st, w, addrs, cluster, mode, tun); err != nil {
			return c, err
		}
		a.syncConntrack(st, w)
		a.resetMoved(st)
		a.syncForward(st)
	}
	c.EgressCapable = true
	logging.Debug(a.logger, "op", "egressDatapath", "node", a.node, "tunnel", mode, "port", port,
		"nodes", len(st.nodes), "pinned", st.pinDir != "", "msg", "datapath applied")
	return c, nil
}

// markShift places the 8-bit mark field the spec's markField names
// (D18).
func markShift(spec purelbv2.LBNodeAgentEgressSpec) uint8 {
	switch spec.MarkField {
	case "0x0000ff00":
		return 8
	case "0x00ff0000":
		return 16
	case "0xff000000":
		return 24
	}
	return 0
}

// helperKey is a helper in a family.
type helperKey struct {
	helper datapath.Helper
	v6     bool
}

// portlessSeen is an address's port-less count and when it last rose.
type portlessSeen struct {
	count uint64
	rose  time.Time
}

// portlessWindow is how long an address reports ProtocolUnsupported after
// its port-less count last rose (user decision 2026-10-30).
const portlessWindow = 10 * time.Minute

// sysModule is where loaded kernel modules are listed.
var sysModule = "/sys/module"

// syncTable writes egress's table: the hardening chains (design 6.12) and,
// once the pod-node state is planned, the pod-node stage (design 6.3),
// with each slot's MSS from the tunnel's path MTU in mode (D13) and only
// the helpers the kernel can run (D23). The table is rewritten when what
// is wanted changed, or when something else changed the kernel's (another
// firewall flushing the ruleset), which is logged. Until the pod-node
// state is planned (an agent restart), a table the kernel holds is kept
// (D12). The port-less counts are read first: a rewrite resets them.
func (a *Agent) syncTable(st *dpState, w dpWant, port uint16, mode string, cluster []netip.Prefix) error {
	if st.nft == nil {
		c, err := datapath.NftConn()
		if err != nil {
			return fmt.Errorf("opening an nftables connection: %w", err)
		}
		st.nft = c
	}
	if st.table != nil && st.table.Pods != nil {
		if err := a.readPortless(st, *st.table.Pods); err != nil {
			return err
		}
	}
	t := datapath.Table{Hardening: datapath.Hardening{TunnelPort: port, Egress: w.hold.egress, ServicePorts: w.hold.services}}
	for _, n := range w.nodes {
		for _, ip := range []netip.Addr{n.v4, n.v6} {
			if ip.IsValid() {
				t.Hardening.Nodes = append(t.Hardening.Nodes, ip)
			}
		}
	}
	faults := map[netip.Addr]purelbv2.EgressAddressFault{}
	if !w.podsKnown {
		if st.table == nil {
			if tbl, err := st.nft.ListTableOfFamily(datapath.NftTable, nftables.TableFamilyINet); err == nil && tbl != nil {
				return nil // an earlier agent's: kept until the plan
			}
		}
	} else if w.stage != nil {
		t.Pods = a.tableStage(st, w, mode, cluster, faults)
	}

	write := st.table == nil || !reflect.DeepEqual(*st.table, t)
	if !write {
		intact, err := datapath.TableIntact(st.nft, t)
		if err != nil {
			return fmt.Errorf("reading the egress table: %w", err)
		}
		if !intact {
			logging.Info(a.logger, "op", "egressTable", "node", a.node,
				"msg", "the egress table was changed outside the agent; restoring it")
			write = true
		}
	}
	if write {
		if err := datapath.SyncTable(st.nft, t); err != nil {
			return fmt.Errorf("writing the egress table: %w", err)
		}
		st.table = &t
		// The new table's counters start from zero.
		for addr, seen := range st.portless {
			seen.count = 0
			st.portless[addr] = seen
		}
		logging.Debug(a.logger, "op", "egressTable", "node", a.node, "podStage", t.Pods != nil, "msg", "egress table written")
	}
	st.tableMode = mode

	// ProtocolUnsupported while an address's port-less count rose in the
	// last portlessWindow; addresses no longer in the stage are forgotten.
	addrs := map[netip.Addr]bool{}
	if t.Pods != nil {
		for _, p := range t.Pods.Policies {
			for _, f := range []*datapath.PolicyFamily{p.V4, p.V6} {
				if f != nil && f.Address.IsValid() {
					addrs[f.Address] = true
				}
			}
		}
	}
	now := a.now()
	for addr, seen := range st.portless {
		if !addrs[addr] {
			delete(st.portless, addr)
			continue
		}
		if !seen.rose.IsZero() && now.Sub(seen.rose) < portlessWindow {
			faults[addr] = purelbv2.EgressAddressFault{Address: addr.String(), Reason: "ProtocolUnsupported",
				Message: "pods sent protocols without ports to this address's slice, which carries TCP, UDP, SCTP and ICMP echo only"}
		}
	}
	st.podFaults = faults
	return nil
}

// readPortless reads stage's port-less counters (D4), as written, and notes
// when each address's count rose.
func (a *Agent) readPortless(st *dpState, stage datapath.PodStage) error {
	drops, err := datapath.PortlessDrops(st.nft, stage)
	if err != nil {
		return fmt.Errorf("reading the port-less counters: %w", err)
	}
	if st.portless == nil {
		st.portless = map[netip.Addr]portlessSeen{}
	}
	now := a.now()
	for addr, n := range drops {
		seen := st.portless[addr]
		if n > seen.count {
			if seen.rose.IsZero() || now.Sub(seen.rose) >= portlessWindow {
				logging.Info(a.logger, "op", "egressPods", "node", a.node, "address", addr, "packets", n-seen.count,
					"msg", "pods sent a protocol without ports to a slice address, which can't carry it (D4)")
			}
			seen.rose = now
		}
		seen.count = n
		st.portless[addr] = seen
	}
	return nil
}

// tableStage is w's pod-node stage as the kernel can run it: with the
// cluster networks (as pl_pod_tag has them), each slot's MSS, and only the
// helpers that are usable, the others reported in faults.
func (a *Agent) tableStage(st *dpState, w dpWant, mode string, cluster []netip.Prefix,
	faults map[netip.Addr]purelbv2.EgressAddressFault) *datapath.PodStage {
	stage := *w.stage
	stage.Cluster = cluster
	stage.MarkShift = st.shift
	mtu4, mtu6 := pathMTU(st, w, mode)
	if st.helpers == nil {
		st.helpers = map[helperKey]bool{}
	}
	stage.Slots = map[uint8]datapath.Slot{}
	for id, s := range w.stage.Slots {
		mtu := slotMTU(s.Addr, mtu4, mtu6, w.steer[id].mtu)
		if s.Addr.Is6() && mtu > 0 && mtu < minMTU6 {
			faults[s.Addr] = purelbv2.EgressAddressFault{Address: s.Addr.String(), Reason: "PathMTUTooSmall",
				Message: fmt.Sprintf("the path MTU is %d, below IPv6's minimum of %d", mtu, minMTU6)}
			mtu = minMTU6
		}
		switch {
		case s.Addr.Is4() && mtu > 40:
			s.MSS = uint16(mtu - 40)
		case s.Addr.Is6() && mtu > 60:
			s.MSS = uint16(mtu - 60)
		}
		var usable []datapath.Helper
		for _, h := range s.Helpers {
			k := helperKey{helper: h, v6: s.Addr.Is6()}
			if !st.helpers[k] {
				if why := datapath.HelperUsable(st.nft, sysModule, h, k.v6); why != "" {
					faults[s.Addr] = purelbv2.EgressAddressFault{Address: s.Addr.String(), Reason: "HelpersUnsupported",
						Message: truncate(why, 1024)}
					continue
				}
				st.helpers[k] = true
			}
			usable = append(usable, h)
		}
		s.Helpers = usable
		stage.Slots[id] = s
	}
	return &stage
}

// minMTU6 is the smallest MTU IPv6 allows.
const minMTU6 = 1280

// pathMTU is the tunnel's path MTU to every peer in mode, per inner
// family (D13); 0 with no peer.
func pathMTU(st *dpState, w dpWant, mode string) (int, int) {
	var self datapath.Node
	var peers []datapath.Node
	for _, n := range w.nodes {
		if n.name == w.self {
			self = datapath.Node{V4: n.v4, V6: n.v6}
		} else {
			peers = append(peers, datapath.Node{V4: n.v4, V6: n.v6})
		}
	}
	if mode == "" {
		mode = datapath.ModeL3
	}
	return datapath.PathMTU(st.nl, mode, self, peers)
}

// slotMTU is a slot's path MTU (D13): the tunnel's in its family, capped
// by its address's pool MTU when known; 0 when neither is.
func slotMTU(addr netip.Addr, mtu4, mtu6, pool int) int {
	mtu := mtu4
	if addr.Is6() {
		mtu = mtu6
	}
	if pool > 0 && (mtu == 0 || pool < mtu) {
		mtu = pool
	}
	return mtu
}

// syncPools numbers w's pool interfaces, keeping the numbers they have,
// mirrors their tables with the cluster networks thrown, and reports what
// each lacks when that changes. The numbering changes table-first: a new
// interface's table is filled before pool_ifs names it, a removed one
// leaves pool_ifs before its table is emptied.
func (a *Agent) syncPools(st *dpState, w dpWant, cluster []netip.Prefix) error {
	var errs []error
	next := map[int]uint32{}
	used := map[uint32]bool{}
	for ifindex, n := range st.pools {
		if _, ok := w.pools[ifindex]; !ok {
			// Its src_valid_mark goes first: pool_ifs is the record of
			// where egress turned it on.
			err := a.resetSrcValidMark(st.nl, ifindex)
			if err == nil {
				err = st.objs.DeletePoolInterface(ifindex)
			}
			if err != nil {
				errs = append(errs, err)
			} else {
				continue
			}
		}
		next[ifindex], used[n] = n, true
	}
	var added []int
	for ifindex := range w.pools {
		if _, ok := next[ifindex]; !ok {
			added = append(added, ifindex)
		}
	}
	slices.Sort(added)
	for _, ifindex := range added {
		n := uint32(0)
		for n < datapath.MaxPoolInterfaces && used[n] {
			n++
		}
		if n == datapath.MaxPoolInterfaces {
			errs = append(errs, fmt.Errorf("more than %d pool interfaces", datapath.MaxPoolInterfaces))
			break
		}
		next[ifindex], used[n] = n, true
	}
	status, err := datapath.SyncPoolTables(st.nl, next, cluster)
	errs = append(errs, err)
	for _, ifindex := range added {
		if n, ok := next[ifindex]; ok {
			if err := st.objs.SetPoolInterface(ifindex, n); err != nil {
				errs = append(errs, err)
			}
		}
	}
	st.pools = next
	watch := map[int]bool{}
	for ifindex := range next {
		watch[ifindex] = true
	}
	a.pools.Store(&watch)

	for ifindex := range st.reports {
		if _, ok := w.pools[ifindex]; !ok {
			delete(st.reports, ifindex)
		}
	}
	for ifindex, p := range w.pools {
		s := status[ifindex]
		r := poolReport{noGateway4: p.v4 && !s.Gateway4, noGateway6: p.v6 && !s.Gateway6, arpAnnounce: 2}
		if p.v4 {
			if r.arpAnnounce, err = datapath.ArpAnnounce(p.name); err != nil {
				errs = append(errs, err)
			}
		}
		if old, ok := st.reports[ifindex]; ok && old == r {
			continue
		}
		st.reports[ifindex] = r
		if r.noGateway4 || r.noGateway6 || r.arpAnnounce < 2 {
			logging.Info(a.logger, "op", "egressPools", "node", a.node, "interface", p.name, "table", datapath.PoolTable(next[ifindex]),
				"noGatewayIPv4", r.noGateway4, "noGatewayIPv6", r.noGateway6, "arpAnnounce", r.arpAnnounce,
				"msg", "pool interface not ready for egress: NoGateway is a missing default route; arp_announce must be 2")
		} else {
			logging.Debug(a.logger, "op", "egressPools", "node", a.node, "interface", p.name, "table", datapath.PoolTable(next[ifindex]),
				"msg", "pool interface table mirrored")
		}
	}
	return errors.Join(errs...)
}

// syncAddresses makes the address maps hold want: each address's entry and
// its slices, reserved and Service ports, added or changed before the
// stale ones are removed; an address no longer wanted loses its entries
// before its addr_cfg entry. An entry that fails stays as it was in
// st.addrs, so the next apply tries it again.
func syncAddresses(st *dpState, want map[netip.Addr]*datapath.AddressEntries) error {
	var errs []error
	o := st.objs
	for addr, w := range want {
		have := st.addrs[addr]
		if have == nil {
			have = &datapath.AddressEntries{Slices: map[uint16]uint64{}, Reserved: map[datapath.PortKey]uint64{},
				Services: map[datapath.PortKey]bool{}}
			st.addrs[addr] = have
			have.SliceSize = ^uint32(0) // never equal: written below
		}
		if have.Address != w.Address {
			if err := o.SetAddress(addr, w.Address); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", addr, err))
				continue
			}
			have.Address = w.Address
		}
		for idx, owner := range w.Slices {
			if have.Slices[idx] != owner {
				if err := o.SetSlice(addr, idx, owner); err != nil {
					errs = append(errs, err)
					continue
				}
				have.Slices[idx] = owner
			}
		}
		for p, owner := range w.Reserved {
			if have.Reserved[p] != owner {
				if err := o.SetReserved(addr, p.Proto, p.Port, owner); err != nil {
					errs = append(errs, err)
					continue
				}
				have.Reserved[p] = owner
			}
		}
		for p := range w.Services {
			if !have.Services[p] {
				if err := o.SetServicePort(addr, p.Proto, p.Port); err != nil {
					errs = append(errs, err)
					continue
				}
				have.Services[p] = true
			}
		}
		errs = append(errs, pruneEntries(o, addr, have, w))
	}
	for addr, have := range st.addrs {
		if _, ok := want[addr]; ok {
			continue
		}
		if err := pruneEntries(o, addr, have, &datapath.AddressEntries{}); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := o.DeleteAddress(addr); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(st.addrs, addr)
	}
	return errors.Join(errs...)
}

// pruneEntries removes have's slices, reserved and Service ports that w
// doesn't have.
func pruneEntries(o *datapath.Objects, addr netip.Addr, have, w *datapath.AddressEntries) error {
	var errs []error
	for idx := range have.Slices {
		if _, ok := w.Slices[idx]; !ok {
			if err := o.DeleteSlice(addr, idx); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(have.Slices, idx)
		}
	}
	for p := range have.Reserved {
		if _, ok := w.Reserved[p]; !ok {
			if err := o.DeleteReserved(addr, p.Proto, p.Port); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(have.Reserved, p)
		}
	}
	for p := range have.Services {
		if !w.Services[p] {
			if err := o.DeleteServicePort(addr, p.Proto, p.Port); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(have.Services, p)
		}
	}
	return errors.Join(errs...)
}

func sameNode(a, b datapath.Node) bool {
	return a.V4 == b.V4 && a.V6 == b.V6 && a.MAC.String() == b.MAC.String()
}
