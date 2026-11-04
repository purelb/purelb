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
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The eBPF stage under load, against spike 10's gates: no more than +150 ns of
// CPU per small packet, and no more than 50 MB of slab growth over a 10-minute
// soak. Both read /proc/stat or /proc/meminfo for the whole machine, so they run
// only in a VM with nothing else in it: hack/bpf-vm/stage-perf.sh.

var (
	stagePerf = flag.Bool("egress-perf", false, "run the eBPF stage's load and soak tests (VM only: hack/bpf-vm/stage-perf.sh)")
	stageSoak = flag.Duration("egress-soak", 10*time.Minute, "how long the soak watches each path's slab")
)

const (
	perfRuns = 5
	// Past the flows' own TIME_WAIT and UDP timeouts, so that what they hold at
	// any moment is in the soak's first sample.
	soakWarmup  = 90 * time.Second
	soakMaxSlab = 50 << 10 // kB
	// New flows, each family. A UDP flow that was answered holds its port for
	// 120 s, so at 4,096 ports per server and port the slice fills above about
	// 30 a second; this stays well below, as a full slice's failures would hide
	// any others.
	soakTCPGap   = 20 * time.Millisecond
	soakUDPGap   = 100 * time.Millisecond
	soakDeadline = 5 * time.Second
)

func perfOrSkip(t *testing.T) {
	t.Helper()
	if !*stagePerf {
		t.Skip("run in a VM by hack/bpf-vm/stage-perf.sh")
	}
	netnsOrSkip(t)
}

// cpuSample is /proc/stat's first line: busy excludes idle and iowait.
type cpuSample struct{ busy, total uint64 }

func readCPU(t *testing.T) cpuSample {
	t.Helper()
	f, err := os.Open("/proc/stat")
	require.NoError(t, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	require.True(t, sc.Scan())
	var c cpuSample
	for i, v := range strings.Fields(sc.Text())[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		require.NoError(t, err)
		c.total += n
		if i != 3 && i != 4 {
			c.busy += n
		}
	}
	return c
}

// cores is how many CPUs were busy, on average, between a and b.
func cores(a, b cpuSample) float64 {
	if b.total == a.total {
		return 0
	}
	return float64(b.busy-a.busy) / float64(b.total-a.total) * float64(runtime.NumCPU())
}

func minMed(v []float64) string {
	s := slices.Sorted(slices.Values(v))
	return fmt.Sprintf("min %.4g median %.4g", s[0], s[len(s)/2])
}

func maxMed(v []float64) string {
	s := slices.Sorted(slices.Values(v))
	return fmt.Sprintf("max %.2f median %.2f", s[len(s)-1], s[len(s)/2])
}

// mmsghdr is struct mmsghdr on 64-bit Linux.
type mmsghdr struct {
	hdr unix.Msghdr
	len uint32
	_   [4]byte
}

// blast sends 18-byte datagrams (64-byte frames) on c for d, 64 per sendmmsg.
func blast(c *net.UDPConn, d time.Duration) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	payload := make([]byte, 18)
	iov := make([]unix.Iovec, 64)
	msgs := make([]mmsghdr, 64)
	for i := range msgs {
		iov[i].Base = &payload[0]
		iov[i].SetLen(len(payload))
		msgs[i].hdr.Iov = &iov[i]
		msgs[i].hdr.SetIovlen(1)
	}
	var serr error
	for start := time.Now(); time.Since(start) < d && serr == nil; {
		if err := rc.Write(func(fd uintptr) bool {
			_, _, e := unix.Syscall6(unix.SYS_SENDMMSG, fd, uintptr(unsafe.Pointer(&msgs[0])), uintptr(len(msgs)), 0, 0, 0)
			switch e {
			case unix.EAGAIN:
				return false
			case 0:
			default:
				serr = e
			}
			return true
		}); err != nil {
			return err
		}
	}
	runtime.KeepAlive(payload)
	runtime.KeepAlive(iov)
	return serr
}

// udpLoad runs one UDP flow, once answered, as a flood of small packets toward
// the server (out) or toward the pod (not out), dropped at the receiver's link
// by a program that counts nothing, so that the path between is what the CPU is
// spent on. CPU nanoseconds and Mpps per run.
func udpLoad(t *testing.T, s *stageTopo, out bool) (mpps, nsPkt []float64) {
	ua := net.UDPAddrFromAddrPort(netip.AddrPortFrom(s.server, 9100))
	var sc, c *net.UDPConn
	require.NoError(t, s.srv.in(func() (err error) {
		sc, err = net.ListenUDP("udp", ua)
		return err
	}))
	require.NoError(t, s.pod.in(func() (err error) {
		c, err = net.DialUDP("udp", nil, ua)
		return err
	}))
	defer c.Close()
	_, err := c.Write([]byte("hi"))
	require.NoError(t, err)
	b := make([]byte, 64)
	require.NoError(t, sc.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, peer, err := sc.ReadFromUDP(b)
	require.NoError(t, err, "the first packet didn't reach the server: %v", s.counts())
	_, err = sc.WriteToUDP([]byte("ok"), peer)
	require.NoError(t, err)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, err = c.Read(b)
	require.NoError(t, err, "the answer didn't reach the pod: %v", s.counts())

	sender, recv, dev := c, s.srv, "eth0"
	if !out {
		// The same five-tuple the flow has, sending. A reply's source port is
		// the server's own, so nothing else can hold it.
		require.NoError(t, sc.Close())
		require.NoError(t, s.srv.in(func() (err error) {
			sender, err = net.DialUDP("udp", ua, peer)
			return err
		}))
		defer sender.Close()
		recv = s.pod
	} else {
		defer sc.Close()
	}
	drop, err := ebpf.NewProgram(&ebpf.ProgramSpec{Type: ebpf.SchedCLS, License: "Apache-2.0",
		Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, tcxDrop), asm.Return()}})
	require.NoError(t, err)
	defer drop.Close()
	recv.attach(t, dev, drop, ebpf.AttachTCXIngress)
	rx := func() uint64 { return recv.link(t, dev).Attrs().Statistics.RxPackets }
	for run := -2; run < perfRuns; run++ { // the first two warm up
		rx0, c0 := rx(), readCPU(t)
		start := time.Now()
		err := blast(sender, 2*time.Second)
		el := time.Since(start)
		c1 := readCPU(t)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		n := rx() - rx0
		require.NotZero(t, n, "nothing arrived: %v", s.counts())
		if run >= 0 {
			mpps = append(mpps, float64(n)/el.Seconds()/1e6)
			nsPkt = append(nsPkt, cores(c0, c1)*el.Seconds()*1e9/float64(n))
		}
	}
	return mpps, nsPkt
}

// tcpServe answers each connection: first byte 'U', discard what it sends; else
// send until it closes.
func tcpServe(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			b := make([]byte, 64<<10)
			if _, err := io.ReadFull(c, b[:1]); err != nil {
				return
			}
			if b[0] == 'U' {
				_, _ = io.Copy(io.Discard, c)
				return
			}
			for {
				if _, err := c.Write(b); err != nil {
					return
				}
			}
		}()
	}
}

// tcpLoad is one transfer of 2 s, upload or download: Gbit/s and busy CPUs.
func tcpLoad(t *testing.T, s *stageTopo, dir byte, hp string) (gbps, busy float64) {
	var c net.Conn
	require.NoError(t, s.pod.in(func() (err error) {
		c, err = net.DialTimeout("tcp", hp, 3*time.Second)
		return err
	}), "%v", s.counts())
	defer c.Close()
	_, err := c.Write([]byte{dir})
	require.NoError(t, err)
	buf := make([]byte, 64<<10)
	if dir == 'D' { // the sender ramps up before the window
		require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
		_, err := io.ReadFull(c, buf)
		require.NoError(t, err)
	}
	require.NoError(t, c.SetDeadline(time.Now().Add(7*time.Second)))
	c0 := readCPU(t)
	start := time.Now()
	var n int64
	for time.Since(start) < 2*time.Second {
		var k int
		if dir == 'U' {
			k, err = c.Write(buf)
		} else {
			k, err = c.Read(buf)
		}
		n += int64(k)
		require.NoError(t, err, "after %d bytes", n)
	}
	el := time.Since(start)
	return float64(n*8) / el.Seconds() / 1e9, cores(c0, readCPU(t))
}

// The stage on one node, direct, against the same topology just forwarding. The
// node has no nft and no conntrack in either, so the difference is the stage's.
func TestPerfEBPFStageLoad(t *testing.T) {
	perfOrSkip(t)
	for _, v6 := range []bool{false, true} {
		for _, mode := range []string{"plain", "stage"} {
			name := map[bool]string{false: "IPv4", true: "IPv6"}[v6] + "/" + mode
			t.Run(name, func(t *testing.T) {
				s := newBareStageTopo(t, v6, 0, 0)
				if mode == "stage" {
					s.attach(t)
				}
				for _, out := range []bool{true, false} {
					dir := map[bool]string{true: "pod->server", false: "server->pod"}[out]
					t.Run("udp "+dir, func(t *testing.T) {
						mpps, ns := udpLoad(t, s, out)
						t.Logf("STAGEPERF %s | udp 64B %s | CPU ns/pkt %s | Mpps %s", name, dir, minMed(ns), maxMed(mpps))
					})
				}
				hp := net.JoinHostPort(s.server.String(), "8000")
				var ln net.Listener
				require.NoError(t, s.srv.in(func() (err error) {
					ln, err = net.Listen("tcp", hp)
					return err
				}))
				defer ln.Close()
				go tcpServe(ln)
				for _, d := range []byte{'U', 'D'} {
					var g, cp []float64
					for range perfRuns {
						gb, busy := tcpLoad(t, s, d, hp)
						g, cp = append(g, gb), append(cp, busy/gb)
					}
					t.Logf("STAGEPERF %s | tcp %c | Gbit/s %s | CPUs per Gbit/s %s", name, d, maxMed(g), minMed(cp))
				}
				if mode == "stage" {
					t.Logf("STAGEPERF %s | counters %v", name, s.counts())
				}
			})
		}
	}
}

func memInfo(t *testing.T, field string) int {
	t.Helper()
	b, err := os.ReadFile("/proc/meminfo")
	require.NoError(t, err)
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, field+":"); ok {
			n, err := strconv.Atoi(strings.Fields(v)[0])
			require.NoError(t, err)
			return n
		}
	}
	require.Fail(t, "not in /proc/meminfo", field)
	return 0
}

// slabTop names the n slab caches holding the most memory.
func slabTop(t *testing.T, n int) string {
	t.Helper()
	b, err := os.ReadFile("/proc/slabinfo")
	require.NoError(t, err)
	type cache struct {
		name string
		kb   int
	}
	var cs []cache
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || strings.HasPrefix(l, "#") || f[0] == "slabinfo" {
			continue
		}
		objs, _ := strconv.Atoi(f[2])
		size, _ := strconv.Atoi(f[3])
		cs = append(cs, cache{f[0], objs * size / 1024})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].kb > cs[j].kb })
	var out []string
	for _, c := range cs[:min(n, len(cs))] {
		out = append(out, fmt.Sprintf("%s=%dkB", c.name, c.kb))
	}
	return strings.Join(out, " ")
}

// soakPath keeps a bulk TCP flow each way and a stream of new TCP and UDP flows
// running in both families for the warm-up and then the soak, with the NAT table
// kept as the agent keeps it, and fails if the slab grows past the gate or the
// traffic stops. srvs are the servers, one per family.
func soakPath(t *testing.T, pod, srv *host, o *Objects, servers []netip.Addr) {
	stop := make(chan struct{})
	var bulk, flows, failed atomic.Int64
	flowErrs := make(chan error, 5)
	for _, a := range servers {
		tcpHP, udpHP := net.JoinHostPort(a.String(), "8080"), net.JoinHostPort(a.String(), "8053")
		tcpEcho(t, srv, tcpHP, nil)
		udpEcho(t, srv, udpHP)
		var c net.Conn
		require.NoError(t, pod.in(func() (err error) {
			c, err = net.DialTimeout("tcp", tcpHP, soakDeadline)
			return err
		}))
		t.Cleanup(func() { _ = c.Close() })
		go func() { // the echo sends each write back
			b := make([]byte, 64<<10)
			for {
				if _, err := c.Write(b); err != nil {
					return
				}
			}
		}()
		go func() {
			b := make([]byte, 64<<10)
			for {
				n, err := c.Read(b)
				if err != nil {
					return
				}
				bulk.Add(int64(n))
			}
		}()
		for _, nw := range []string{"tcp", "udp"} {
			hp := map[string]string{"tcp": tcpHP, "udp": udpHP}[nw]
			gap := map[string]time.Duration{"tcp": soakTCPGap, "udp": soakUDPGap}[nw]
			go func() {
				for tick := time.NewTicker(gap); ; {
					select {
					case <-stop:
						tick.Stop()
						return
					case <-tick.C:
					}
					if err := pod.in(func() error {
						c, err := net.DialTimeout(nw, hp, soakDeadline)
						if err != nil {
							return err
						}
						defer c.Close()
						_ = c.SetDeadline(time.Now().Add(soakDeadline))
						if _, err := c.Write([]byte("hi\n")); err != nil {
							return err
						}
						_, err = c.Read(make([]byte, 128))
						return err
					}); err != nil {
						failed.Add(1)
						select {
						case flowErrs <- fmt.Errorf("%s %s: %w", nw, hp, err):
						default:
						}
					}
					flows.Add(1)
				}
			}()
		}
	}
	defer close(stop)

	gc := func() {
		now, err := MonotonicNow()
		require.NoError(t, err)
		_, err = o.NatGC(now)
		require.NoError(t, err)
	}
	wait := func(d time.Duration) {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(2 * time.Second) {
			gc()
		}
	}
	wait(soakWarmup)
	slab0, unrec0 := memInfo(t, "Slab"), memInfo(t, "SUnreclaim")
	t.Logf("STAGESOAK baseline: Slab=%dkB SUnreclaim=%dkB bulk=%dMB flows=%d failed=%d top: %s",
		slab0, unrec0, bulk.Load()>>20, flows.Load(), failed.Load(), slabTop(t, 3))
	var last int64
	for elapsed := time.Duration(0); elapsed < *stageSoak; {
		step := min(time.Minute, *stageSoak-elapsed)
		last = bulk.Load()
		wait(step)
		elapsed += step
		require.Greater(t, bulk.Load(), last, "the bulk flows stopped: %v", nonzeroCounts(o))
		t.Logf("STAGESOAK %v: Slab=%dkB (%+dkB) SUnreclaim=%dkB (%+dkB) bulk=%dMB flows=%d failed=%d", elapsed,
			memInfo(t, "Slab"), memInfo(t, "Slab")-slab0, memInfo(t, "SUnreclaim"), memInfo(t, "SUnreclaim")-unrec0,
			bulk.Load()>>20, flows.Load(), failed.Load())
	}
	t.Logf("STAGESOAK end: top: %s; counters %v", slabTop(t, 5), nonzeroCounts(o))
	assert.LessOrEqual(t, memInfo(t, "Slab")-slab0, soakMaxSlab, "slab growth, kB")
	assert.Zero(t, nonzeroCounts(o)["nat_insert_failed"], "the slice never fills at this rate")
	assert.Less(t, failed.Load()*100, flows.Load(), "over 1%% of the new flows failed")
	for len(flowErrs) > 0 {
		t.Logf("a new flow failed: %v", <-flowErrs)
	}
}

func nonzeroCounts(o *Objects) map[string][2]uint64 {
	c, _ := o.Counts()
	return nonzero(c)
}

// No path of the stage leaks: direct, and through both tunnel modes and underlay
// families, where 6.6.15's bpf_redirect_neigh leak was found (CVE-2025-40183).
func TestPerfEBPFStageSoak(t *testing.T) {
	perfOrSkip(t)
	servers := []netip.Addr{netip.MustParseAddr("172.30.10.1"), netip.MustParseAddr("fdaa::1")}
	t.Run("direct", func(t *testing.T) {
		s := newStageTopo(t, false, 0, 1000)
		soakPath(t, s.pod, s.srv, s.o, servers)
	})
	for _, c := range []tunCase{{under6: false, l2: false}, {under6: true, l2: true}} {
		t.Run("tunnel "+c.String(), func(t *testing.T) {
			pod, srv, objA, _ := newStageTunnel(t, c)
			soakPath(t, pod, srv, objA, servers)
		})
	}
}

// pod_reply runs only on a packet to one of this node's own slices (a held
// address, slice owned here): other uplink traffic leaves pl_announce before it.
// One run each, the median of many, over what pl_announce takes on a packet to
// no egress address.
func TestPodReplyCost(t *testing.T) {
	once := func(p *ebpf.Program, pkt []byte) time.Duration {
		var ds []time.Duration
		for range 2000 {
			_, d, err := p.Benchmark(pkt, 1, nil)
			require.NoError(t, err)
			ds = append(ds, d)
		}
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			s := newStageH(t) // skips where BPF can't load
			require.NoError(t, s.o.SetAddress(f.egress, Address{Held: true, SliceSize: 4096, Base: 1024, Table: 7900}))
			require.NoError(t, s.o.SetSlice(f.egress, sliceIndex, self))
			base := once(s.o.PlAnnounceEth, packet(f, f.server, s.pod[f.v6], protoUDP, ports(443, 40000)))
			t.Logf("pl_announce, uplink miss (the baseline): %v", base)
			for _, x := range []struct {
				name  string
				proto uint8
				out   func(sport uint16) []byte
				in    func(port uint16) []byte
			}{
				{"TCP", protoTCP,
					func(sport uint16) []byte { return s.pkt(f, protoTCP, tcpSeg(sport, 443, flagSYN)) },
					func(port uint16) []byte { return packet(f, f.server, f.egress, protoTCP, tcpSeg(443, port, flagACK)) }},
				{"UDP", protoUDP,
					func(sport uint16) []byte { return s.pkt(f, protoUDP, ports(sport, 443)) },
					func(port uint16) []byte { return reply(f, port) }},
			} {
				_, data, _ := s.out(x.out(40000 + uint16(x.proto)))
				_, port := s.srcOf(f, data)
				before := s.count("nat_reply", f.v6)
				t.Logf("pl_announce + pod_reply, established %s: +%v", x.name, once(s.o.PlAnnounceEth, x.in(port))-base)
				require.Equal(t, before+2000, s.count("nat_reply", f.v6), "the path timed is the reply's")
			}
		})
	}
}
