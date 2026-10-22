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
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The pod-node eBPF stage's NAT, end to end, with no conntrack:
//
//   pod --veth-- node --ul-- server
//
// The node holds the egress address on its uplink "ul". pl_pod_out on the
// uplink's tcx egress (design 6.4: host NIC, after routing) translates the
// selected pod's forwarded packets to the egress address and a port in the
// node's slice; pl_pod_reply on the uplink's tcx ingress (where 6.4 calls
// it from pl_announce) reverses replies and ICMP errors. The server has no
// route to the pod network, so every reply the pod gets went through the
// reverse NAT. accept_local stays at its default (0) everywhere.

const (
	sliceStart = 20480
	sliceSize  = 4096
	tcpPort    = 8080
	udpPort    = 9090
	closedPort = 9999
	echoID     = 0x4242

	natOut = 1
	natIn  = 2

	stNatOut      = 15
	stNatNew      = 16
	stNatReply    = 17
	stNatICMPErr  = 18
	stNatFragDrop = 19
	stNatNoPort   = 20
	stNatMiss     = 21
)

var stateName = map[uint8]string{1: "NEW", 2: "ESTABLISHED", 3: "FIN", 4: "CLOSED"}

type natFamily struct {
	name, pod, egress, server string
	af                        uint8
}

var natFamilies = []natFamily{
	{"ipv4", "10.244.1.5", "172.30.10.110", "172.30.10.1", 2},
	{"ipv6", "fd44:1::5", "fdaa::110", "fdaa::1", 10},
}

func TestSpike2NatPath(t *testing.T) {
	requireVM(t)
	for _, f := range natFamilies {
		t.Run(f.name, func(t *testing.T) { runNat(t, f) })
	}
	// Control: the same program on the pod veth's tcx ingress (the earlier
	// prototype's attach point), also without accept_local.
	for _, f := range natFamilies {
		t.Run("veth-ingress-control/"+f.name, func(t *testing.T) { runNatControl(t, f) })
	}
}

// natTopo builds pod -- node -- server with the NAT maps filled, and no
// program attached. accept_local is left at its default and checked to be 0.
func natTopo(t *testing.T) (pod, nd, srv *node, o *natObjects) {
	pod, nd, srv = newNode(t, "pod"), newNode(t, "node"), newNode(t, "server")
	veth(t, pod, "eth0", nd, "vpod")
	veth(t, nd, "ul", srv, "eth0")

	addr(t, pod, "eth0", "10.244.1.5/24")
	addr(t, pod, "eth0", "fd44:1::5/64")
	addr(t, nd, "vpod", "10.244.1.1/24")
	addr(t, nd, "vpod", "fd44:1::1/64")
	defaultRoute(t, pod, "10.244.1.1")
	defaultRoute(t, pod, "fd44:1::1")
	addr(t, nd, "ul", "172.30.10.2/24")
	addr(t, nd, "ul", "fdaa::2/64")
	addr(t, nd, "ul", "172.30.10.110/32") // the egress addresses
	addr(t, nd, "ul", "fdaa::110/128")
	addr(t, srv, "eth0", "172.30.10.1/24")
	addr(t, srv, "eth0", "fdaa::1/64")

	nd.sysctl(t, "net.ipv4.ip_forward", "1")
	nd.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	for _, dev := range []string{"all", "default", "vpod", "ul"} {
		require.Equal(t, "0", nd.sysctlGet(t, "net.ipv4.conf."+dev+".accept_local"), "accept_local on %s", dev)
	}

	o = &natObjects{}
	require.NoError(t, loadNatObjects(o, nil))
	t.Cleanup(func() { o.Close() })
	require.NoError(t, o.PodNat.Put(uint32(ifindex(t, nd, "vpod")), natPodCfg{
		V4: ip16("10.244.1.5")[0], V6: ip16("fd44:1::5"),
		Eg4: ip16("172.30.10.110")[0], Eg6: ip16("fdaa::110"),
		Start: sliceStart, Size: sliceSize}))
	for _, e := range []struct {
		a  string
		af uint8
	}{{"172.30.10.110", 2}, {"fdaa::110", 10}} {
		require.NoError(t, o.EgressAddrs.Put(natAddrKey{Addr: ip16(e.a), Family: e.af},
			natAddrCfg{Base: 1024, Shift: 12, Announced: 1}))
	}
	return pod, nd, srv, o
}

// runNatControl: pl_pod_out on the pod veth's tcx ingress, before routing,
// without accept_local. Only TCP is tried: IPv4 is expected to be dropped
// as a martian (the node's own address as the source), IPv6 to pass.
func runNatControl(t *testing.T, f natFamily) {
	pod, nd, srv, o := natTopo(t)
	attach(t, nd, "vpod", o.PlPodOut, ebpf.AttachTCXIngress)
	attach(t, nd, "ul", o.PlPodReply, ebpf.AttachTCXIngress)
	hp := net.JoinHostPort(f.server, strconv.Itoa(tcpPort))
	var ln net.Listener
	require.NoError(t, srv.in(func() error {
		var err error
		ln, err = net.Listen("tcp", hp)
		return err
	}))
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	k0, m0 := snmpNode(t, nd), martianSrc(t, nd)
	var tc net.Conn
	err := pod.in(func() error {
		var err error
		tc, err = net.DialTimeout("tcp", hp, 2*time.Second)
		return err
	})
	if tc != nil {
		tc.Close()
	}
	k1, m1 := snmpNode(t, nd), martianSrc(t, nd)
	t.Logf("SPIKE2NAT RESULT %s veth-ingress-control: TCP connect without accept_local: %s; node IpInAddrErrors +%d, rt_cache in_martian_src +%d;%s",
		f.name, errOK(err), k1["IpInAddrErrors"]-k0["IpInAddrErrors"], m1-m0, natStats(t, o.Stats))
	if f.af == 2 {
		require.Error(t, err, "IPv4 at veth ingress without accept_local was expected to fail")
		require.NotZero(t, m1-m0, "the translated SYN must be dropped as a martian source")
		require.NotZero(t, stat(t, o.Stats, stNatOut), "the SYN was translated before it was dropped")
	} else {
		require.NoError(t, err, "IPv6 at veth ingress needs nothing")
	}
}

// martianSrc sums the in_martian_src column of /proc/net/stat/rt_cache
// (per-CPU hex counters; kernel-wide, but only this test routes at the time).
func martianSrc(t *testing.T, n *node) int64 {
	t.Helper()
	var sum int64
	require.NoError(t, n.in(func() error {
		raw, err := os.ReadFile("/proc/thread-self/net/stat/rt_cache")
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		col := -1
		for i, h := range strings.Fields(lines[0]) {
			if h == "in_martian_src" {
				col = i
			}
		}
		if col < 0 {
			return fmt.Errorf("rt_cache: no in_martian_src column")
		}
		for _, l := range lines[1:] {
			if f := strings.Fields(l); len(f) > col {
				v, _ := strconv.ParseInt(f[col], 16, 64)
				sum += v
			}
		}
		return nil
	}))
	return sum
}

func errOK(err error) string {
	if err == nil {
		return "connected"
	}
	return err.Error()
}

func runNat(t *testing.T, f natFamily) {
	pod, nd, srv, o := natTopo(t)
	attach(t, nd, "ul", o.PlPodOut, ebpf.AttachTCXEgress)
	attach(t, nd, "ul", o.PlPodReply, ebpf.AttachTCXIngress)

	inSlice := func(p uint16) bool { return p >= sliceStart && p < sliceStart+sliceSize }
	diag := func() string { return natStats(t, o.Stats) }
	result := func(c, format string, args ...any) {
		t.Logf("SPIKE2NAT RESULT %s %s PASS: %s", f.name, c, fmt.Sprintf(format, args...))
	}
	hp := func(port int) string { return net.JoinHostPort(f.server, strconv.Itoa(port)) }

	// ---- TCP: the server sees the egress address and a slice port; data
	// round-trips; the FIN moves the flow's state.
	var ln net.Listener
	require.NoError(t, srv.in(func() error {
		var err error
		ln, err = net.Listen("tcp", hp(tcpPort))
		return err
	}))
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				fmt.Fprintf(c, "%s\n", c.RemoteAddr())
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	var tc net.Conn
	err := pod.in(func() error {
		var err error
		tc, err = net.DialTimeout("tcp", hp(tcpPort), 5*time.Second)
		return err
	})
	require.NoError(t, err, "tcp: pod could not reach the server;%s", diag())
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReader(tc)
	line, err := rd.ReadString('\n')
	require.NoError(t, err, "tcp: reading the server's report;%s", diag())
	seen, err := netip.ParseAddrPort(strings.TrimSpace(line))
	require.NoError(t, err)
	require.Equal(t, f.egress, seen.Addr().String(), "tcp: server must see the egress address")
	require.True(t, inSlice(seen.Port()), "tcp: source port %d outside the slice", seen.Port())
	const msg = "data through the PureLB-owned NAT"
	_, err = fmt.Fprintf(tc, "%s\n", msg)
	require.NoError(t, err)
	echo, err := rd.ReadString('\n')
	require.NoError(t, err, "tcp: echo;%s", diag())
	require.Equal(t, msg+"\n", echo)
	// Bulk: 4 MiB each way through the echo, so the NIC-egress program sees
	// GSO packets from the pod (veth TSO) and GRO'd replies.
	const bulk = 4 << 20
	out0 := stat(t, o.Stats, stNatOut)
	werr := make(chan error, 1)
	go func() {
		b := make([]byte, 64<<10)
		var err error
		for sent := 0; sent < bulk && err == nil; sent += len(b) {
			_, err = tc.Write(b)
		}
		werr <- err
	}()
	_ = tc.SetDeadline(time.Now().Add(20 * time.Second))
	got, err := io.CopyN(io.Discard, rd, bulk)
	require.NoError(t, err, "tcp bulk: %d of %d bytes echoed;%s", got, bulk, diag())
	require.NoError(t, <-werr)
	bulkPkts := stat(t, o.Stats, stNatOut) - out0
	require.NoError(t, tc.Close())
	var tcpState uint8
	require.Eventually(t, func() bool {
		tcpState = natState(t, o.Nat, f, 6, tcpPort, seen.Port())
		return tcpState == 3 || tcpState == 4
	}, 3*time.Second, 50*time.Millisecond, "tcp: the NAT entry must reach FIN/CLOSED after close")
	result("tcp", "server saw %s (slice %d-%d), %d-byte echo round-tripped, %d MiB bulk echoed through %d translated skbs, NAT state after close=%s",
		seen, sliceStart, sliceStart+sliceSize-1, len(msg)+1, bulk>>20, bulkPkts, stateName[tcpState])

	// ---- UDP: an exchange, the server reporting the source it saw.
	var uc net.PacketConn
	require.NoError(t, srv.in(func() error {
		var err error
		uc, err = net.ListenPacket("udp", hp(udpPort))
		return err
	}))
	defer uc.Close()
	var srvUDP atomic.Int64
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := uc.ReadFrom(buf)
			if err != nil {
				return
			}
			srvUDP.Add(1)
			_, _ = uc.WriteTo(append([]byte(from.String()+"|"), buf[:n]...), from)
		}
	}()
	var pc net.Conn
	require.NoError(t, pod.in(func() error {
		var err error
		pc, err = net.Dial("udp", hp(udpPort))
		return err
	}))
	defer pc.Close()
	_ = pc.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = pc.Write([]byte("udp-hello"))
	require.NoError(t, err)
	buf := make([]byte, 65536)
	n, err := pc.Read(buf)
	require.NoError(t, err, "udp: no reply;%s", diag())
	from, payload, ok := strings.Cut(string(buf[:n]), "|")
	require.True(t, ok)
	require.Equal(t, "udp-hello", payload)
	useen, err := netip.ParseAddrPort(from)
	require.NoError(t, err)
	require.Equal(t, f.egress, useen.Addr().String(), "udp: server must see the egress address")
	require.True(t, inSlice(useen.Port()), "udp: source port %d outside the slice", useen.Port())
	result("udp", "server saw %s, reply de-NATed to the pod (%q)", useen, payload)

	// ---- ICMP echo: the identifier is translated into the slice.
	proto, req, rep := "ip4:icmp", byte(8), byte(0)
	if f.af == 10 {
		proto, req, rep = "ip6:ipv6-icmp", 128, 129
	}
	var sraw, praw net.PacketConn
	require.NoError(t, srv.in(func() error {
		var err error
		sraw, err = net.ListenPacket(proto, f.server)
		return err
	}))
	defer sraw.Close()
	type echoSeen struct {
		from string
		id   uint16
	}
	seenc := make(chan echoSeen, 4)
	go func() {
		b := make([]byte, 1500)
		for {
			n, a, err := sraw.ReadFrom(b)
			if err != nil {
				return
			}
			if n >= 8 && b[0] == req {
				seenc <- echoSeen{a.String(), uint16(b[4])<<8 | uint16(b[5])}
			}
		}
	}()
	require.NoError(t, pod.in(func() error {
		var err error
		praw, err = net.ListenPacket(proto, f.pod)
		return err
	}))
	defer praw.Close()
	ping := []byte{req, 0, 0, 0, echoID >> 8, echoID & 0xff, 0, 1, 'p', 'u', 'r', 'e', 'l', 'b'}
	if f.af == 2 { // the kernel fills the ICMPv6 checksum on raw sockets
		cs := icmpChecksum(ping)
		ping[2], ping[3] = byte(cs>>8), byte(cs)
	}
	_, err = praw.WriteTo(ping, &net.IPAddr{IP: net.ParseIP(f.server)})
	require.NoError(t, err)
	_ = praw.SetReadDeadline(time.Now().Add(5 * time.Second))
	var replyFrom string
	for {
		n, a, err := praw.ReadFrom(buf)
		require.NoError(t, err, "icmp echo: no reply at the pod;%s", diag())
		if n >= 8 && buf[0] == rep && uint16(buf[4])<<8|uint16(buf[5]) == echoID {
			replyFrom = a.String()
			break
		}
	}
	var es echoSeen
	select {
	case es = <-seenc:
	case <-time.After(2 * time.Second):
		t.Fatalf("icmp echo: the server saw no request")
	}
	require.Equal(t, f.egress, es.from, "icmp echo: server must see the egress address")
	require.True(t, inSlice(es.id), "icmp echo: identifier %d outside the slice", es.id)
	require.Equal(t, f.server, replyFrom)
	result("icmp-echo", "server saw request from %s id=%d (pod sent id=%d); pod got the reply from %s with id=%d",
		es.from, es.id, echoID, replyFrom, echoID)

	// ---- ICMP error: UDP to a closed port; the port-unreachable quotes the
	// translated packet, and the pod's connected socket must still match it.
	var ec net.Conn
	require.NoError(t, pod.in(func() error {
		var err error
		ec, err = net.Dial("udp", hp(closedPort))
		return err
	}))
	defer ec.Close()
	errBefore := stat(t, o.Stats, stNatICMPErr)
	_ = ec.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = ec.Write([]byte("anyone?"))
	require.NoError(t, err)
	_, err = ec.Read(buf)
	require.True(t, errors.Is(err, syscall.ECONNREFUSED),
		"icmp error: the pod's socket must see ECONNREFUSED, got %v;%s", err, diag())
	nerr := stat(t, o.Stats, stNatICMPErr) - errBefore
	require.NotZero(t, nerr)
	result("icmp-error", "port unreachable de-NATed (outer + quoted header): pod socket got %q; nat_icmp_err=%d",
		syscall.ECONNREFUSED.Error(), nerr)

	// ---- Fragments: dropped and counted at pl_pod_out.
	fragBefore, rcvBefore := stat(t, o.Stats, stNatFragDrop), srvUDP.Load()
	_, err = pc.Write(make([]byte, 3000)) // over the 1500 MTU: the pod fragments it
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)
	nfrag := stat(t, o.Stats, stNatFragDrop) - fragBefore
	require.GreaterOrEqual(t, nfrag, uint64(2), "fragments must be dropped and counted;%s", diag())
	require.Equal(t, rcvBefore, srvUDP.Load(), "the server must not receive the fragmented datagram")
	result("fragments", "3000-byte UDP datagram: %d fragments dropped and counted, server received none", nfrag)

	// ---- No conntrack: the translation never touched it.
	ct := "nf_conntrack not loaded"
	require.NoError(t, nd.in(func() error {
		b, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_count")
		if err == nil {
			ct = strings.TrimSpace(string(b))
		}
		return nil
	}))
	if ct != "nf_conntrack not loaded" {
		require.Equal(t, "0", ct, "the node's conntrack table must stay empty")
	}
	flows := countNat(t, o.Nat)
	result("no-conntrack", "node nf_conntrack_count=%s; PureLB NAT map holds %d entries (%d flows, both directions)",
		ct, flows, flows/2)

	// ---- GC: TCP-state-aware expiry from user space.
	now := monotonic(t)
	g1, err := gcNat(o.Nat, now+uint64(11*time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, g1, "at +11s only the closed TCP flow expires")
	require.Zero(t, natState(t, o.Nat, f, 6, tcpPort, seen.Port()))
	g2, err := gcNat(o.Nat, now+uint64(120*time.Second))
	require.NoError(t, err)
	require.Zero(t, countNat(t, o.Nat), "at +120s every flow has expired")
	result("gc", "+11s expired %d flow (TCP after FIN), +120s expired %d (UDP, ICMP), map empty;%s", g1, g2, diag())
}

// natState returns the flow state of the translated tuple, 0 if absent.
func natState(t *testing.T, m *ebpf.Map, f natFamily, proto uint8, rport, xport uint16) uint8 {
	k := natNatKey{Raddr: ip16(f.server), Laddr: ip16(f.egress), Rport: rport, Lport: xport,
		Proto: proto, Family: f.af, Dir: natIn}
	var v natNatVal
	if err := m.Lookup(k, &v); err != nil {
		require.ErrorIs(t, err, ebpf.ErrKeyNotExist)
		return 0
	}
	return v.State
}

func countNat(t *testing.T, m *ebpf.Map) int {
	var k natNatKey
	var v natNatVal
	n := 0
	it := m.Iterate()
	for it.Next(&k, &v) {
		n++
	}
	require.NoError(t, it.Err())
	return n
}

// natTimeout is the idle timeout per protocol and state: closing TCP goes
// quickly, established TCP lives long, everything else is short.
func natTimeout(proto, state uint8) time.Duration {
	switch {
	case proto == 6 && (state == 3 || state == 4):
		return 10 * time.Second
	case proto == 6 && state == 2:
		return 2 * time.Hour
	case state == 2:
		return 60 * time.Second
	default:
		return 30 * time.Second
	}
}

// gcNat removes every NAT_IN entry idle past its timeout at now (CLOCK_MONOTONIC
// ns, the clock of bpf_ktime_get_ns) together with its NAT_OUT reverse
// entry. It returns the number of flows removed.
func gcNat(m *ebpf.Map, now uint64) (int, error) {
	var k natNatKey
	var v natNatVal
	var dead []natNatKey
	it := m.Iterate()
	for it.Next(&k, &v) {
		if k.Dir != natIn || now-v.Ts < uint64(natTimeout(k.Proto, v.State)) {
			continue
		}
		out := natNatKey{Raddr: k.Raddr, Laddr: v.Addr, Rport: k.Rport, Lport: v.Port,
			Proto: k.Proto, Family: k.Family, Dir: natOut}
		dead = append(dead, k, out)
	}
	if err := it.Err(); err != nil {
		return 0, err
	}
	for _, d := range dead {
		if err := m.Delete(d); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return 0, err
		}
	}
	return len(dead) / 2, nil
}

func monotonic(t *testing.T) uint64 {
	var ts unix.Timespec
	require.NoError(t, unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts))
	return uint64(ts.Nano())
}

func icmpChecksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

func natStats(t *testing.T, m *ebpf.Map) string {
	names := map[uint32]string{stNatOut: "nat_out", stNatNew: "nat_new", stNatReply: "nat_reply",
		stNatICMPErr: "nat_icmp_err", stNatFragDrop: "nat_frag_drop", stNatNoPort: "nat_no_port", stNatMiss: "nat_miss"}
	var b strings.Builder
	for i := uint32(stNatOut); i <= stNatMiss; i++ {
		if v := stat(t, m, i); v > 0 {
			fmt.Fprintf(&b, " %s=%d", names[i], v)
		}
	}
	return b.String()
}
