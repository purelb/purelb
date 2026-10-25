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
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// udpListen receives datagrams on port in h; each payload names its test.
func udpListen(t *testing.T, h *host, port int) <-chan string {
	t.Helper()
	var pc net.PacketConn
	require.NoError(t, h.in(func() error {
		var err error
		pc, err = net.ListenPacket("udp", ":"+strconv.Itoa(port))
		return err
	}))
	t.Cleanup(func() { _ = pc.Close() })
	ch := make(chan string, 64)
	go func() {
		b := make([]byte, 128)
		for {
			n, _, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			ch <- string(b[:n])
		}
	}()
	return ch
}

func udpSend(t *testing.T, h *host, dst string, port int, payload string) {
	t.Helper()
	require.NoError(t, h.in(func() error {
		c, err := net.Dial("udp", net.JoinHostPort(dst, strconv.Itoa(port)))
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Write([]byte(payload))
		return err
	}))
}

// received reports whether payload arrives on ch within a short wait.
func received(ch <-chan string, payload string) bool {
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case p := <-ch:
			if p == payload {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// delivered sends payload until it arrives on ch, for up to 5 s: a fresh
// veth's IPv6 neighbour discovery waits for its link-local address's DAD.
// A drop is checked once, after delivered has shown the path works.
func delivered(t *testing.T, h *host, dst string, port int, ch <-chan string, payload string) bool {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		udpSend(t, h, dst, port, payload)
		if received(ch, payload) {
			return true
		}
	}
	return false
}

// dialable is tcpDial retried for up to 5 s, as delivered.
func dialable(h *host, dst string, port int) error {
	var err error
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		if err = tcpDial(h, dst, port); err == nil {
			return nil
		}
	}
	return err
}

func tcpListen(t *testing.T, h *host, port int) {
	t.Helper()
	var ln net.Listener
	require.NoError(t, h.in(func() error {
		var err error
		ln, err = net.Listen("tcp", ":"+strconv.Itoa(port))
		return err
	}))
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

func tcpDial(h *host, dst string, port int) error {
	return h.in(func() error {
		d := net.Dialer{Timeout: 300 * time.Millisecond}
		c, err := d.Dial("tcp", net.JoinHostPort(dst, strconv.Itoa(port)))
		if err == nil {
			_ = c.Close()
		}
		return err
	})
}

// TestNetnsHardening checks the hardening chains with real packets, both
// families. Node a and outsider c each reach b over a veth; b routes
// between them and holds egress addresses on lo.
func TestNetnsHardening(t *testing.T) {
	netnsOrSkip(t)
	a, b, c := newHost(t, "a"), newHost(t, "b"), newHost(t, "c")
	wire(t, a, "a0", b, "b0")
	wire(t, c, "c0", b, "b1")
	for _, x := range []struct {
		h         *host
		dev, cidr string
	}{
		{a, "a0", "10.0.1.1/24"}, {a, "a0", "fd01::1/64"}, {b, "b0", "10.0.1.2/24"}, {b, "b0", "fd01::2/64"},
		{c, "c0", "10.0.2.3/24"}, {c, "c0", "fd02::3/64"}, {b, "b1", "10.0.2.2/24"}, {b, "b1", "fd02::2/64"},
		{b, "lo", "10.9.9.9/32"}, {b, "lo", "fd09::9/128"},
	} {
		x.h.addr(t, x.dev, x.cidr)
	}
	c.route(t, &netlink.Route{Dst: cidr("0.0.0.0/0"), Gw: net.ParseIP("10.0.2.2")})
	c.route(t, &netlink.Route{Dst: cidr("::/0"), Gw: net.ParseIP("fd02::2")})
	for _, dst := range []string{"10.0.2.0/24", "10.9.9.9/32"} {
		a.route(t, &netlink.Route{Dst: cidr(dst), Gw: net.ParseIP("10.0.1.2")})
	}
	for _, dst := range []string{"fd02::/64", "fd09::9/128"} {
		a.route(t, &netlink.Route{Dst: cidr(dst), Gw: net.ParseIP("fd01::2")})
	}
	b.sysctl(t, "net.ipv4.ip_forward", "1")
	b.sysctl(t, "net.ipv6.conf.all.forwarding", "1")

	tunB := udpListen(t, b, 6082)
	tunA := udpListen(t, a, 6082)
	otherA := udpListen(t, a, 6083)
	tcpListen(t, b, 8080)
	tcpListen(t, b, 8081)
	// Without the chains everything below gets through, so each drop
	// after them is theirs.
	for _, f := range []struct{ name, bFromC, aFromC, eg string }{
		{"IPv4", "10.0.2.2", "10.0.1.1", "10.9.9.9"},
		{"IPv6", "fd02::2", "fd01::1", "fd09::9"},
	} {
		require.True(t, delivered(t, c, f.bFromC, 6082, tunB, "baseline outsider "+f.name), f.name)
		require.True(t, delivered(t, c, f.aFromC, 6082, tunA, "baseline forwarded "+f.name), f.name)
		require.NoError(t, dialable(c, f.eg, 8081), f.name)
	}

	nft, err := nftables.New(nftables.WithNetNSFd(int(b.ns)))
	require.NoError(t, err)
	// Something of an earlier agent's in the table: replaced with it.
	stale := nft.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable})
	nft.AddChain(&nftables.Chain{Name: "stale", Table: stale})
	require.NoError(t, nft.Flush())

	h := Hardening{TunnelPort: 6082,
		Nodes:  []netip.Addr{netip.MustParseAddr("10.0.1.1"), netip.MustParseAddr("fd01::1"), netip.MustParseAddr("10.0.1.2"), netip.MustParseAddr("fd01::2")},
		Egress: []netip.Addr{netip.MustParseAddr("10.9.9.9"), netip.MustParseAddr("fd09::9")},
		ServicePorts: []ServicePort{{Addr: netip.MustParseAddr("10.9.9.9"), Proto: unix.IPPROTO_TCP, Port: 8080},
			{Addr: netip.MustParseAddr("fd09::9"), Proto: unix.IPPROTO_TCP, Port: 8080}}}
	require.NoError(t, SyncHardening(nft, h))

	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: NftTable}
	chains, err := nft.ListChainsOfTableFamily(nftables.TableFamilyINet)
	require.NoError(t, err)
	rules := map[string]int{}
	for _, ch := range chains {
		if ch.Table.Name != NftTable {
			continue
		}
		r, err := nft.GetRules(tbl, ch)
		require.NoError(t, err)
		rules[ch.Name] = len(r)
	}
	assert.Equal(t, map[string]int{"announce_in": 5, "tun_fwd": 2, "tun_in": 2, "tun_raw": 2, "tun_raw_out": 2}, rules,
		"the stale chain is gone; each family has its rules")

	for _, f := range []struct {
		name                       string
		bFromA, bFromC, aFromC, eg string
	}{
		{"IPv4", "10.0.1.2", "10.0.2.2", "10.0.1.1", "10.9.9.9"},
		{"IPv6", "fd01::2", "fd02::2", "fd01::1", "fd09::9"},
	} {
		t.Run(f.name, func(t *testing.T) {
			// tun_in: the tunnel port takes nodes only.
			assert.True(t, delivered(t, a, f.bFromA, 6082, tunB, "node "+f.name), "from a node")
			udpSend(t, c, f.bFromC, 6082, "outsider "+f.name)
			assert.False(t, received(tunB, "outsider "+f.name), "from a non-node")
			// tun_fwd: nothing forwarded reaches a node's tunnel port; other
			// ports forward.
			udpSend(t, c, f.aFromC, 6082, "forwarded "+f.name)
			assert.False(t, received(tunA, "forwarded "+f.name), "forwarded to a node's tunnel port")
			assert.True(t, delivered(t, c, f.aFromC, 6083, otherA, "other "+f.name), "forwarded to another port")
			// announce_in: no new connection to an egress address but its
			// Service ports; the node's own addresses are untouched.
			assert.NoError(t, dialable(c, f.eg, 8080), "a Service port of a shared address")
			assert.Error(t, tcpDial(c, f.eg, 8081), "any other port of an egress address")
			assert.NoError(t, dialable(c, f.bFromC, 8081), "the node's own address")
		})
	}

	// HardeningIntact: true as written, false for other inputs and after a
	// rule, an element or the whole table is deleted.
	intact := func() bool {
		ok, err := HardeningIntact(nft, h)
		require.NoError(t, err)
		return ok
	}
	assert.True(t, intact())
	other := h
	other.Nodes = h.Nodes[:2]
	ok, err := HardeningIntact(nft, other)
	require.NoError(t, err)
	assert.False(t, ok, "another node set")
	in, err := nft.ListChain(tbl, "tun_in")
	require.NoError(t, err)
	r, err := nft.GetRules(tbl, in)
	require.NoError(t, err)
	require.NoError(t, nft.DelRule(r[0]))
	require.NoError(t, nft.Flush())
	assert.False(t, intact(), "a rule deleted")
	require.NoError(t, SyncHardening(nft, h))
	assert.True(t, intact())
	nodes4, err := nft.GetSetByName(tbl, "nodes4")
	require.NoError(t, err)
	require.NoError(t, nft.SetDeleteElements(nodes4, []nftables.SetElement{{Key: netip.MustParseAddr("10.0.1.1").AsSlice()}}))
	require.NoError(t, nft.Flush())
	assert.False(t, intact(), "an element deleted")
	nft.DelTable(tbl)
	require.NoError(t, nft.Flush())
	assert.False(t, intact(), "the table deleted")

	// Replaced whole: an address no longer egress takes connections.
	h.Egress, h.ServicePorts = nil, nil
	require.NoError(t, SyncHardening(nft, h))
	assert.True(t, intact())
	assert.NoError(t, dialable(c, "10.9.9.9", 8081))
	assert.NoError(t, dialable(c, "fd09::9", 8081))
}

// TestNetnsAnnounceInTunnel drops new connections arriving through the
// tunnel device, whatever their destination.
func TestNetnsAnnounceInTunnel(t *testing.T) {
	netnsOrSkip(t)
	b, c := newHost(t, "b"), newHost(t, "c")
	// A veth named as the tunnel stands in for it.
	wire(t, c, "c0", b, TunnelName)
	c.addr(t, "c0", "10.0.2.3/24")
	c.addr(t, "c0", "fd02::3/64")
	b.addr(t, TunnelName, "10.0.2.2/24")
	b.addr(t, TunnelName, "fd02::2/64")
	tcpListen(t, b, 8081)
	require.NoError(t, dialable(c, "10.0.2.2", 8081), "before the chains")
	require.NoError(t, dialable(c, "fd02::2", 8081), "before the chains")
	nft, err := nftables.New(nftables.WithNetNSFd(int(b.ns)))
	require.NoError(t, err)
	require.NoError(t, SyncHardening(nft, Hardening{TunnelPort: 6082}))
	assert.Error(t, tcpDial(c, "10.0.2.2", 8081))
	assert.Error(t, tcpDial(c, "fd02::2", 8081))
}

func tableRoutes(t *testing.T, h *host, table int) []string {
	t.Helper()
	var out []string
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := h.nl.RouteListFiltered(fam, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		require.NoError(t, err)
		for _, r := range routes {
			dst := "default"
			if r.Dst != nil && !isDefault(r) {
				dst = r.Dst.String()
			}
			if r.Gw != nil {
				dst += " via " + r.Gw.String()
			}
			out = append(out, dst)
		}
	}
	sort.Strings(out)
	return out
}

// TestNetnsPoolTables mirrors each pool interface's main-table routes
// into its table, both families, reports gateways, and empties the
// tables of interfaces that are no longer pool interfaces.
func TestNetnsPoolTables(t *testing.T) {
	netnsOrSkip(t)
	h := newHost(t, "h")
	for _, name := range []string{"eth1", "eth2"} {
		require.NoError(t, h.nl.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}))
		require.NoError(t, h.nl.LinkSetUp(h.link(t, name)))
	}
	h.addr(t, "eth1", "192.0.2.1/24")
	h.addr(t, "eth1", "2001:db8:1::1/64")
	h.addr(t, "eth2", "198.51.100.1/24")
	eth1, eth2 := h.link(t, "eth1").Attrs().Index, h.link(t, "eth2").Attrs().Index
	h.route(t, &netlink.Route{Dst: cidr("0.0.0.0/0"), Gw: net.ParseIP("192.0.2.254"), LinkIndex: eth1})
	// An RA-learned default counts as the IPv6 gateway (design 6.9).
	ra := &netlink.Route{Dst: cidr("::/0"), Gw: net.ParseIP("2001:db8:1::fe"), LinkIndex: eth1, Protocol: unix.RTPROT_RA}
	h.route(t, ra)
	// A table an earlier agent left with no interface now.
	h.route(t, &netlink.Route{Dst: cidr("203.0.113.0/24"), LinkIndex: eth2, Table: PoolTable(5)})

	status, err := SyncPoolTables(h.nl, map[int]uint32{eth1: 0, eth2: 1})
	require.NoError(t, err)
	assert.Equal(t, map[int]PoolStatus{eth1: {Gateway4: true, Gateway6: true}, eth2: {}}, status)
	assert.Equal(t, []string{"192.0.2.0/24", "2001:db8:1::/64", "default via 192.0.2.254", "default via 2001:db8:1::fe", "fe80::/64"},
		tableRoutes(t, h, PoolTable(0)))
	assert.Equal(t, []string{"198.51.100.0/24", "fe80::/64"}, tableRoutes(t, h, PoolTable(1)), "eth2 has no default: NoGateway")
	assert.Empty(t, tableRoutes(t, h, PoolTable(5)), "a table no interface owns is emptied")

	// A second sync changes nothing; a main-table change is mirrored.
	_, err = SyncPoolTables(h.nl, map[int]uint32{eth1: 0, eth2: 1})
	require.NoError(t, err)
	require.NoError(t, h.nl.RouteDel(ra))
	status, err = SyncPoolTables(h.nl, map[int]uint32{eth1: 0})
	require.NoError(t, err)
	assert.Equal(t, map[int]PoolStatus{eth1: {Gateway4: true}}, status)
	assert.Equal(t, []string{"192.0.2.0/24", "2001:db8:1::/64", "default via 192.0.2.254", "fe80::/64"},
		tableRoutes(t, h, PoolTable(0)))
	assert.Empty(t, tableRoutes(t, h, PoolTable(1)), "eth2 is no longer a pool interface")

	// arp_announce is the larger of the interface's and "all".
	require.NoError(t, h.in(func() error {
		v, err := ArpAnnounce("eth1")
		if err != nil {
			return err
		}
		if v != 0 {
			return fmt.Errorf("arp_announce %d, want 0", v)
		}
		return nil
	}))
	h.sysctl(t, "net.ipv4.conf.all.arp_announce", "2")
	require.NoError(t, h.in(func() error {
		v, err := ArpAnnounce("eth1")
		if err == nil && v != 2 {
			err = fmt.Errorf("arp_announce %d, want 2", v)
		}
		return err
	}))
}
