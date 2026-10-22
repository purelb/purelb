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
	"net"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"purelb.io/egress-spikes/vm/perf"
)

// Spike 10: throughput through the eBPF pod-node stage (pl_pod_out on the
// uplink's tcx egress, pl_pod_reply on its tcx ingress) against the same
// topology with plain forwarding, at 1,500 and 9,000 MTU, both families.
// One 2-vCPU VM over veth: relative numbers only. Neither mode has
// conntrack in the node (no nft at all), so the eBPF stage's cost is
// measured alone.
func TestPerfNatNet(t *testing.T) {
	perf.Require(t)
	for _, mtu := range []int{1500, 9000} {
		for _, mode := range []string{"plain", "ebpf"} {
			t.Run(fmt.Sprintf("mtu%d/%s", mtu, mode), func(t *testing.T) {
				pod, nd, srv, o := natTopo(t)
				for _, x := range []struct {
					n   *node
					dev string
				}{{pod, "eth0"}, {nd, "vpod"}, {nd, "ul"}, {srv, "eth0"}} {
					l, err := x.n.nl.LinkByName(x.dev)
					require.NoError(t, err)
					require.NoError(t, x.n.nl.LinkSetMTU(l, mtu))
				}
				if mode == "plain" {
					for _, r := range []struct{ dst, gw string }{{"10.244.1.0/24", "172.30.10.2"}, {"fd44:1::/64", "fdaa::2"}} {
						_, d, _ := net.ParseCIDR(r.dst)
						require.NoError(t, srv.nl.RouteAdd(&netlink.Route{Dst: d, Gw: net.ParseIP(r.gw)}))
					}
				} else {
					attach(t, nd, "ul", o.PlPodOut, ebpf.AttachTCXEgress)
					attach(t, nd, "ul", o.PlPodReply, ebpf.AttachTCXIngress)
				}
				for _, f := range natFamilies {
					cfg := fmt.Sprintf("mtu%d/%s %s", mtu, mode, f.name)
					hp := net.JoinHostPort(f.server, "8000")
					var ln net.Listener
					require.NoError(t, srv.in(func() error {
						var err error
						ln, err = net.Listen("tcp", hp)
						return err
					}))
					perf.Serve(ln)
					for _, dir := range []byte{'U', 'D'} {
						var g, cp []float64
						for range perf.Runs {
							gb, cores, err := perf.Stream(func() (net.Conn, error) {
								var c net.Conn
								err := pod.in(func() error {
									var err error
									c, err = net.DialTimeout("tcp", hp, 3*time.Second)
									return err
								})
								return c, err
							}, dir, 2*time.Second)
							require.NoError(t, err, "%s stream %c;%s", cfg, dir, natStats(t, o.Stats))
							g = append(g, gb)
							cp = append(cp, cores/gb)
						}
						d := map[byte]string{'U': "tcp pod->server", 'D': "tcp server->pod"}[dir]
						t.Logf("SPIKE10 NET ebpf-stage | %s | %s | Gbit/s %s", cfg, d, perf.MaxMed(g))
						t.Logf("SPIKE10 NET ebpf-stage | %s | %s | CPUs per Gbit/s %s", cfg, d, perf.MaxMed(cp))
					}
					ln.Close()
					m, ns := natPPS(t, pod, srv, f.server)
					t.Logf("SPIKE10 NET ebpf-stage | %s | udp 64B | Mpps %s", cfg, perf.MaxMed(m))
					t.Logf("SPIKE10 NET ebpf-stage | %s | udp 64B | CPU ns/pkt %s", cfg, perf.MinMed(ns))
					t.Logf("SPIKE10 MEM after %s%s slabtop:%s", cfg, perf.MemLine(), perf.SlabTop(2))
				}
			})
		}
	}
}

// natPPS is the spikes package's ppsRuns: one UDP flow the server answers
// once, then two warm-up and perf.Runs windows of 64-byte frames with
// sendmmsg, counted and dropped at the server's link.
func natPPS(t *testing.T, pod, srv *node, server string) (mpps, nsPkt []float64) {
	hp := net.JoinHostPort(server, "9100")
	ua, err := net.ResolveUDPAddr("udp", hp)
	require.NoError(t, err)
	var sc, c *net.UDPConn
	require.NoError(t, srv.in(func() error {
		var err error
		sc, err = net.ListenUDP("udp", ua)
		return err
	}))
	defer sc.Close()
	require.NoError(t, pod.in(func() error {
		var err error
		c, err = net.DialUDP("udp", nil, ua)
		return err
	}))
	defer c.Close()
	_, err = c.Write([]byte("hi"))
	require.NoError(t, err)
	_ = sc.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 64)
	_, peer, err := sc.ReadFromUDP(b)
	require.NoError(t, err, "udp: first packet did not reach the server")
	_, err = sc.WriteToUDP([]byte("ok"), peer)
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(b)
	require.NoError(t, err, "udp: the server's answer did not reach the pod")
	drop, err := perf.Dropper()
	require.NoError(t, err)
	defer drop.Close()
	idx := ifindex(t, srv, "eth0")
	var l link.Link
	require.NoError(t, srv.in(func() error {
		var err error
		l, err = link.AttachTCX(link.TCXOptions{Program: drop, Interface: idx, Attach: ebpf.AttachTCXIngress})
		return err
	}))
	defer l.Close()
	rx := func() uint64 {
		ln, err := srv.nl.LinkByName("eth0")
		require.NoError(t, err)
		return ln.Attrs().Statistics.RxPackets
	}
	for run := -2; run < perf.Runs; run++ { // runs -2 and -1 warm up
		rx0 := rx()
		c0 := perf.ReadCPU()
		start := time.Now()
		sent, err := perf.Blast(c, 18, 2*time.Second)
		el := time.Since(start)
		c1 := perf.ReadCPU()
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		n := rx() - rx0
		require.NotZero(t, n, "no packets reached the server (sent %d)", sent)
		if run < 0 {
			continue
		}
		mpps = append(mpps, float64(n)/el.Seconds()/1e6)
		nsPkt = append(nsPkt, perf.Cores(c0, c1)*el.Seconds()*1e9/float64(n))
	}
	return mpps, nsPkt
}
