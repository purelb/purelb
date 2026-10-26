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
	// nft is the nftables connection (tests make their own); hardening
	// what was last written through it.
	nft       *nftables.Conn
	hardening *datapath.Hardening
	// pools numbers the pool interfaces (ifindex: n, table 7900 + n);
	// reports is what was last logged about each.
	pools   map[int]uint32
	reports map[int]poolReport
	// addrs are the address entries in the maps.
	addrs map[netip.Addr]*datapath.AddressEntries
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
			acks := w.hold.acks
			a.acks.Store(&acks)
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
		st.objs, st.links, st.nodes, st.pools, st.addrs = objs, map[string]link.Link{}, nodes, pools, addrs
		st.linkIfs = map[string]int{}
		st.reports = map[int]poolReport{}
	}

	self := datapath.NodeID(w.self)
	port := uint16(6082)
	if w.spec.TunnelPort != 0 {
		port = uint16(w.spec.TunnelPort)
	}
	// The hardening chains before the tunnel opens its port (design 6.12).
	if err := a.syncHardening(st, w, port); err != nil {
		return c, err
	}
	mode, tun, err := datapath.EnsureTunnel(st.nl, w.spec.TunnelEncapsulation, port, datapath.TunnelMAC(self))
	if err != nil {
		return c, err
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
	errs = append(errs, datapath.SyncThrows(st.nl, underlay))
	errs = append(errs, a.syncPools(st, w))
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
	c.EgressCapable = true
	logging.Debug(a.logger, "op", "egressDatapath", "node", a.node, "tunnel", mode, "port", port,
		"nodes", len(st.nodes), "pinned", st.pinDir != "", "msg", "datapath applied")
	return c, nil
}

// syncHardening writes the hardening chains unless the kernel already
// holds them as wanted; something else may have removed them (another
// firewall flushing the ruleset), which is logged.
func (a *Agent) syncHardening(st *dpState, w dpWant, port uint16) error {
	h := datapath.Hardening{TunnelPort: port, Egress: w.hold.egress, ServicePorts: w.hold.services}
	for _, n := range w.nodes {
		for _, ip := range []netip.Addr{n.v4, n.v6} {
			if ip.IsValid() {
				h.Nodes = append(h.Nodes, ip)
			}
		}
	}
	if st.nft == nil {
		c, err := nftables.New()
		if err != nil {
			return fmt.Errorf("opening an nftables connection: %w", err)
		}
		st.nft = c
	}
	intact, err := datapath.HardeningIntact(st.nft, h)
	if err != nil {
		return fmt.Errorf("reading the hardening chains: %w", err)
	}
	if intact {
		st.hardening = &h
		return nil
	}
	if st.hardening != nil && reflect.DeepEqual(*st.hardening, h) {
		logging.Info(a.logger, "op", "egressHardening", "node", a.node,
			"msg", "the hardening chains were changed outside the agent; restoring them")
	}
	if err := datapath.SyncHardening(st.nft, h); err != nil {
		return fmt.Errorf("writing the hardening chains: %w", err)
	}
	st.hardening = &h
	return nil
}

// syncPools numbers w's pool interfaces, keeping the numbers they have,
// mirrors their tables, and reports what each lacks when that changes.
// The numbering changes table-first: a new interface's table is filled
// before pool_ifs names it, a removed one leaves pool_ifs before its
// table is emptied.
func (a *Agent) syncPools(st *dpState, w dpWant) error {
	var errs []error
	next := map[int]uint32{}
	used := map[uint32]bool{}
	for ifindex, n := range st.pools {
		if _, ok := w.pools[ifindex]; !ok {
			if err := st.objs.DeletePoolInterface(ifindex); err != nil {
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
	status, err := datapath.SyncPoolTables(st.nl, next)
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
