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

package spikes

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// ---- Spike 4: pinned links keep [guard, pl_announce] order ----------------
//
// Runs with only CAP_BPF + CAP_NET_ADMIN (the agent's set), in the VM's own
// namespace. A stand-in "guard" program is attached at the head of a dummy
// link's tcx ingress chain and pinned; pl_announce is attached after it and
// pinned. Then the agent "restarts" (every program, map and link fd closed),
// the links are reopened from their pins, and the programs are swapped in
// place as an upgrade would. The chain order must hold throughout.
func TestSpike4PinOrder(t *testing.T) {
	requireVM(t)
	const dev = "spkpin0"
	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: dev}}))
	l, err := netlink.LinkByName(dev)
	require.NoError(t, err)
	defer netlink.LinkDel(l)
	require.NoError(t, netlink.LinkSetUp(l))
	ifx := l.Attrs().Index

	dir := "/sys/fs/bpf/purelb-spike"
	require.NoError(t, os.MkdirAll(dir, 0o700))
	defer os.RemoveAll(dir)
	guardPin := filepath.Join(dir, fmt.Sprintf("guard-tcx-%d", ifx))
	annPin := filepath.Join(dir, fmt.Sprintf("announce-tcx-%d", ifx))

	order := func(stage string) []ebpf.ProgramID {
		res, err := link.QueryPrograms(link.QueryOptions{Target: ifx, Attach: ebpf.AttachTCXIngress})
		require.NoError(t, err, stage)
		var ids []ebpf.ProgramID
		for _, p := range res.Programs {
			ids = append(ids, p.ID)
		}
		return ids
	}
	id := func(p *ebpf.Program) ebpf.ProgramID {
		info, err := p.Info()
		require.NoError(t, err)
		i, _ := info.ID()
		return i
	}

	// First agent: attach and pin.
	var o1 coreObjects
	require.NoError(t, loadCoreObjects(&o1, nil))
	guard, err := link.AttachTCX(link.TCXOptions{Program: o1.PlPodTag, Interface: ifx,
		Attach: ebpf.AttachTCXIngress, Anchor: link.Head()})
	require.NoError(t, err)
	ann, err := link.AttachTCX(link.TCXOptions{Program: o1.PlAnnounce, Interface: ifx,
		Attach: ebpf.AttachTCXIngress, Anchor: link.AfterLink(guard)})
	require.NoError(t, err)
	require.NoError(t, guard.Pin(guardPin), "pin under CAP_BPF only")
	require.NoError(t, ann.Pin(annPin))
	want := []ebpf.ProgramID{id(o1.PlPodTag), id(o1.PlAnnounce)}
	require.Equal(t, want, order("attach"))
	t.Logf("SPIKE4 attached+pinned: order %v", order("attach"))

	// Agent exits: every fd closed. Pinned links keep the programs attached.
	require.NoError(t, guard.Close())
	require.NoError(t, ann.Close())
	o1.Close()
	require.Equal(t, want, order("after restart, before reopen"))
	t.Logf("SPIKE4 agent fds closed: still attached, order %v", order("closed"))

	// Next agent: reopen from pins, then upgrade in place.
	g2, err := link.LoadPinnedLink(guardPin, nil)
	require.NoError(t, err)
	a2, err := link.LoadPinnedLink(annPin, nil)
	require.NoError(t, err)
	var o2 coreObjects
	require.NoError(t, loadCoreObjects(&o2, nil))
	defer o2.Close()
	require.NoError(t, g2.Update(o2.PlPodTag), "bpf_link_update on a tcx link")
	require.NoError(t, a2.Update(o2.PlAnnounce))
	want2 := []ebpf.ProgramID{id(o2.PlPodTag), id(o2.PlAnnounce)}
	require.Equal(t, want2, order("after upgrade"))
	t.Logf("SPIKE4 reopened from pins and upgraded in place: order %v (new program ids)", order("upgraded"))

	// Uninstall: unpin and close detaches both.
	require.NoError(t, g2.Unpin())
	require.NoError(t, a2.Unpin())
	require.NoError(t, g2.Close())
	require.NoError(t, a2.Close())
	// On some kernels (6.6) the last link reference is released from a work
	// queue, so detach is asynchronous: uninstall must wait for it.
	start := time.Now()
	for len(order("after uninstall")) > 0 && time.Since(start) < 5*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	require.Empty(t, order("after uninstall"), "links still attached 5s after unpin+close")
	t.Logf("SPIKE4 PASS: order held across restart and upgrade; unpin+close detached after %s", time.Since(start).Round(time.Millisecond))
}

// ---- Spike 6: IPv6 forwarding knob, and ND under the output drop ----------

// TestSpike6PerDeviceForwarding answers whether per-device
// net.ipv6.conf.<dev>.forwarding=1 forwards IPv6 when all.forwarding=0.
func TestSpike6PerDeviceForwarding(t *testing.T) {
	requireVM(t)
	cli, rtr, srv := newNode(t, "client"), newNode(t, "router"), newNode(t, "server")
	veth(t, cli, "eth0", rtr, "in0")
	veth(t, rtr, "out0", srv, "eth0")
	addr(t, cli, "eth0", "fd10::2/64")
	addr(t, rtr, "in0", "fd10::1/64")
	addr(t, rtr, "out0", "fd20::1/64")
	addr(t, srv, "eth0", "fd20::2/64")
	defaultRoute(t, cli, "fd10::1")
	defaultRoute(t, srv, "fd20::1")

	try := func() error {
		var ln net.Listener
		require.NoError(t, srv.in(func() error {
			var err error
			ln, err = net.Listen("tcp", "[fd20::2]:9000")
			return err
		}))
		defer ln.Close()
		go func() {
			if c, err := ln.Accept(); err == nil {
				c.Close()
			}
		}()
		return cli.in(func() error {
			c, err := net.DialTimeout("tcp", "[fd20::2]:9000", 2*time.Second)
			if err == nil {
				c.Close()
			}
			return err
		})
	}

	rtr.sysctl(t, "net.ipv6.conf.all.forwarding", "0")
	rtr.sysctl(t, "net.ipv6.conf.in0.forwarding", "1")
	rtr.sysctl(t, "net.ipv6.conf.out0.forwarding", "1")
	perDev := try()
	rtr.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	global := try()
	require.NoError(t, global, "control: all.forwarding=1 must forward")
	if perDev == nil {
		t.Logf("SPIKE6 FORWARDING: per-device forwarding=1 with all.forwarding=0 FORWARDS on this kernel")
	} else {
		t.Logf("SPIKE6 FORWARDING: per-device forwarding=1 with all.forwarding=0 does NOT forward on this kernel (%v); all.forwarding=1 is required", perDev)
	}
}

// TestSpike6NDUnderOutputDrop checks that the D20 output-hook drop of
// packets sourced from an egress address breaks IPv6 neighbour discovery
// unless NS/NA (hop limit 255) and reply-direction traffic are exempted.
func TestSpike6NDUnderOutputDrop(t *testing.T) {
	requireVM(t)
	for _, exempt := range []bool{false, true} {
		t.Run(fmt.Sprintf("exempt=%v", exempt), func(t *testing.T) {
			nd, srv := newNode(t, "node"), newNode(t, "server")
			veth(t, nd, "pool", srv, "eth0")
			addr(t, nd, "pool", "fdaa::2/64")
			addr(t, nd, "pool", "fdaa::110/128") // the egress address on the pool interface
			addr(t, srv, "eth0", "fdaa::1/64")
			outputDrop(t, nd, "fdaa::110", exempt)

			var ln net.Listener
			require.NoError(t, nd.in(func() error {
				var err error
				ln, err = net.Listen("tcp", "[fdaa::110]:9000")
				return err
			}))
			defer ln.Close()
			go func() {
				if c, err := ln.Accept(); err == nil {
					c.Close()
				}
			}()
			err := srv.in(func() error {
				c, err := net.DialTimeout("tcp", "[fdaa::110]:9000", 3*time.Second)
				if err == nil {
					c.Close()
				}
				return err
			})
			if exempt {
				require.NoError(t, err, "with the ND and reply exemptions the server must reach the address")
				t.Logf("SPIKE6 ND exempt=true: neighbour resolution and the reply both pass")
			} else {
				require.Error(t, err, "without the exemption NA from the egress address is dropped")
				t.Logf("SPIKE6 ND exempt=false: unreachable as predicted (%v)", err)
			}
		})
	}
}

func outputDrop(t *testing.T, n *node, egress string, exempt bool) {
	t.Helper()
	c, err := nftables.New(nftables.WithNetNSFd(int(n.h)))
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "purelb_out"})
	ch := c.AddChain(&nftables.Chain{Name: "out", Table: tbl, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter})
	if exempt {
		// ct state established,related accept (replies of inbound flows)
		c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: []expr.Any{
			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: le32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: le32(0)},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: le32(0)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		}})
		// icmpv6 type {nd-neighbor-solicit, nd-neighbor-advert} ip6 hoplimit 255 accept
		for _, typ := range []byte{135, 136} {
			c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{typ}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 7, Len: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{255}},
				&expr.Verdict{Kind: expr.VerdictAccept},
			}})
		}
	}
	// ip6 saddr <egress> drop
	c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 8, Len: 16},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: netip.MustParseAddr(egress).AsSlice()},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}})
	require.NoError(t, c.Flush())
}

// ---- Spike 7: established flows keep their slot via the ct-mark vmap -------
//
// The router marks only a flow's first packet (ct state new). Every later
// packet must get the slot back from the ct mark through the constant-write
// vmap, or the slot rule does not route it and it is blackholed (the main
// table's route to the server is a blackhole). A completed multi-megabyte
// stream proves the restore runs on every established packet, on this
// kernel, with no register-to-register OR.
func TestSpike7NftRestore(t *testing.T) {
	requireVM(t)
	cli, rtr, srv := newNode(t, "client"), newNode(t, "router"), newNode(t, "server")
	veth(t, cli, "eth0", rtr, "vpod")
	veth(t, rtr, "up0", srv, "eth0")
	addr(t, cli, "eth0", "10.244.1.5/24")
	addr(t, rtr, "vpod", "10.244.1.1/24")
	addr(t, rtr, "up0", "172.30.10.2/24")
	addr(t, rtr, "up0", "172.30.10.110/32")
	addr(t, srv, "eth0", "172.30.10.1/24")
	defaultRoute(t, cli, "10.244.1.1")
	rtr.sysctl(t, "net.ipv4.ip_forward", "1")
	// Replies come back on up0 with the slot restored from the ct mark; the
	// reverse-path check must follow the mark into table 100, as on a pod
	// node that is itself the announcer (direct-path reply marking).
	rtr.sysctl(t, "net.ipv4.conf.all.rp_filter", "1")
	rtr.sysctl(t, "net.ipv4.conf.up0.src_valid_mark", "1")

	// main: server prefix blackholed; table 100 (slot 3): the real path.
	_, sp, _ := net.ParseCIDR("172.30.10.1/32")
	route(t, rtr, &netlink.Route{Dst: sp, Type: unix.RTN_BLACKHOLE})
	route(t, rtr, &netlink.Route{Dst: sp, LinkIndex: ifindex(t, rtr, "up0"), Table: 100})
	r := netlink.NewRule()
	r.Family, r.Priority, r.Table = netlink.FAMILY_V4, 50, 100
	r.Mark, r.Mask = 3, ptr(uint32(0xff))
	require.NoError(t, rtr.nl.RuleAdd(r))

	newFlowMark(t, rtr, "vpod", 3)
	nftEgress(t, rtr, []snatSlot{{slot: 3, addr: "172.30.10.110", portLo: sliceLo, portHi: sliceHi}})

	var ln net.Listener
	require.NoError(t, srv.in(func() error {
		var err error
		ln, err = net.Listen("tcp", "172.30.10.1:9000")
		return err
	}))
	defer ln.Close()
	got := make(chan int64, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer c.Close()
		fmt.Fprintf(c, "%s\n", c.RemoteAddr())
		n, _ := io.Copy(io.Discard, c)
		got <- n
	}()
	var c net.Conn
	require.NoError(t, cli.in(func() error {
		var err error
		c, err = net.DialTimeout("tcp", "172.30.10.1:9000", 3*time.Second)
		return err
	}))
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	require.NoError(t, err)
	const total = 16 << 20
	buf := make([]byte, 64<<10)
	var sent int64
	for sent < total {
		n, err := c.Write(buf)
		require.NoError(t, err, "established packets were not routed: ct-mark restore failed")
		sent += int64(n)
	}
	c.Close()
	require.Equal(t, int64(total), <-got, "the server must receive the whole stream")
	ap, err := netip.ParseAddrPort(line[:len(line)-1])
	require.NoError(t, err)
	require.Equal(t, "172.30.10.110", ap.Addr().String())
	t.Logf("SPIKE7 PASS: 16 MiB over one flow marked only on ct new; server saw %s (per-slot constant SNAT)", ap)
}

// newFlowMark marks a flow's first packet from iif with slot (constant
// mask-and-xor), in its own table at priority -160, ahead of the restore.
func newFlowMark(t *testing.T, n *node, iif string, slot uint32) {
	t.Helper()
	c, err := nftables.New(nftables.WithNetNSFd(int(n.h)))
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "purelb_spike_new"})
	ch := c.AddChain(&nftables.Chain{Name: "new", Table: tbl, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-160)})
	name := make([]byte, 16)
	copy(name, iif)
	c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: name},
		&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(expr.CtStateBitNEW), Xor: le32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: le32(0)},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(0xffffff00), Xor: le32(slot)},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	}})
	require.NoError(t, c.Flush())
}
