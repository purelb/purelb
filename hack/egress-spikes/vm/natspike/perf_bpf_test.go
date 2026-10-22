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
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"purelb.io/egress-spikes/vm/perf"
)

// Spike 10: BPF_PROG_TEST_RUN cost of the eBPF pod-node stage (pl_pod_out,
// pl_pod_reply), per path and family, under CAP_BPF + CAP_NET_ADMIN only.
// Translating paths rewrite the packet, so they are measured one
// iteration per fresh skb (perf.SingleShot); the miss paths with the
// address guard's method (1M iterations, five runs). Then the slice is
// filled flow by flow until the bounded probe (NAT_PROBES = 64, odd step from the flow hash) fails.

// natProbes mirrors NAT_PROBES in bpf/egress_nat.c (for the log only).
const natProbes = 64

const (
	tcxNext   = 0xFFFFFFFF
	tcxDrop   = 2
	podIf     = 5 // the selected pod's host-side ifindex (skb->ingress_ifindex)
	natCalls  = 20_000
	newPerRun = 512
)

func a4(s string) [4]byte   { return netip.MustParseAddr(s).As4() }
func a16(s string) [16]byte { return netip.MustParseAddr(s).As16() }

func frame(src, dst string, proto uint8, sport, dport uint16) []byte {
	l4 := perf.L4(proto, sport, dport, 0)
	if netip.MustParseAddr(dst).Is4() {
		return perf.Eth(perf.IPv4(a4(src), a4(dst), proto, l4, 64, perf.Whole, 0))
	}
	return perf.Eth(perf.IPv6(a16(src), a16(dst), proto, l4, 64, false, perf.Whole, 0))
}

func newNat(t *testing.T) *natObjects {
	t.Helper()
	o := &natObjects{}
	require.NoError(t, loadNatObjects(o, nil))
	require.NoError(t, o.PodNat.Put(uint32(podIf), natPodCfg{
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
	return o
}

// xport finds the translated port of the flow from pod port sport.
func xport(t *testing.T, o *natObjects, af, proto uint8, sport uint16) uint16 {
	var k natNatKey
	var v natNatVal
	it := o.Nat.Iterate()
	for it.Next(&k, &v) {
		if k.Dir == natIn && k.Family == af && k.Proto == proto && v.Port == sport {
			return k.Lport
		}
	}
	require.NoError(t, it.Err())
	t.Fatalf("no NAT entry for pod port %d", sport)
	return 0
}

func TestPerfNatBPF(t *testing.T) {
	perf.Require(t)
	triv, err := perf.Trivial()
	require.NoError(t, err)
	defer triv.Close()
	podCtx := perf.SkbCtx(0, podIf, 0)

	for _, f := range natFamilies {
		o := newNat(t)
		log := func(prog, path, method string, vals []float64, extra string) {
			t.Logf("SPIKE10 BPF %s | %s | %s | %s | ns/pkt %s%s", prog, f.name, path, method, perf.MinMed(vals), extra)
		}
		check := func(what string, ret, want uint32, st uint32, before uint64) {
			if ret != want || (st != 0 && stat(t, o.Stats, st) == before) {
				t.Errorf("SPIKE10 BPF WRONG-PATH %s %s: ret=%#x want %#x, counter %d before %d now %d",
					f.name, what, ret, want, st, before, stat(t, o.Stats, st))
			}
		}
		repeat := func(prog string, p *ebpf.Program, path string, data, ctx []byte, want uint32) {
			vals, ret, err := perf.Repeat(p, data, ctx, 1_000_000, perf.Runs)
			require.NoError(t, err)
			check(path, ret, want, 0, 0)
			log(prog, path, "repeat", vals, "")
		}
		single := func(prog string, p *ebpf.Program, path string, gen func(int) []byte, ctx []byte, calls int, want, st uint32, fresh func() *natObjects) {
			var vals, fixed []float64
			for range perf.Runs {
				pp := p
				if fresh != nil {
					pp = fresh().PlPodOut
				}
				before := stat(t, o.Stats, st)
				v, z, ret, err := perf.SingleShot(pp, triv, gen, ctx, calls)
				require.NoError(t, err)
				if fresh == nil {
					check(path, ret, want, st, before)
				}
				vals = append(vals, v)
				fixed = append(fixed, z)
			}
			log(prog, path, "single", vals, fmt.Sprintf(" (test-run fixed cost subtracted: median %.0f ns)", perf.Median(fixed)))
		}

		other := "192.0.2.99"
		if f.af == 10 {
			other = "2001:db8::99"
		}
		pod := frame(f.pod, f.server, unix.IPPROTO_TCP, 40000, 443)
		podU := frame(f.pod, f.server, unix.IPPROTO_UDP, 40001, 53)
		// Misses: every packet on the NIC's egress, and on the uplink's
		// ingress, pays these.
		repeat("pl_pod_out", o.PlPodOut, "not a selected pod (lookup miss)", pod, perf.SkbCtx(0, 7, 0), tcxNext)
		repeat("pl_pod_out", o.PlPodOut, "node-originated (no ingress device)", pod, nil, tcxNext)
		repeat("pl_pod_reply", o.PlPodReply, "uplink miss (not a translated flow)", frame(f.server, other, unix.IPPROTO_TCP, 443, 40000), nil, tcxNext)

		// Established flows: create, then translate fresh copies.
		for _, d := range [][]byte{pod, podU} {
			_, _, err := perf.TestRun(o.PlPodOut, d, podCtx, 1, nil)
			require.NoError(t, err)
		}
		same := func(d []byte) func(int) []byte { return func(int) []byte { return d } }
		single("pl_pod_out", o.PlPodOut, "established TCP (translate)", same(pod), podCtx, natCalls, tcxNext, stNatOut, nil)
		single("pl_pod_out", o.PlPodOut, "established UDP (translate)", same(podU), podCtx, natCalls, tcxNext, stNatOut, nil)
		xp := xport(t, o, f.af, unix.IPPROTO_TCP, 40000)
		reply := frame(f.server, f.egress, unix.IPPROTO_TCP, 443, xp)
		single("pl_pod_reply", o.PlPodReply, "established TCP reply (reverse translate)", same(reply), nil, natCalls, tcxNext, stNatReply, nil)
		repeat("pl_pod_out", o.PlPodOut, "method check: not a selected pod", pod, perf.SkbCtx(0, 7, 0), tcxNext)

		// New flows: port probing and two inserts. Fresh maps per run, 512
		// flows each (slice at most 12.5% full).
		gen := func(i int) []byte { return frame(f.pod, f.server, unix.IPPROTO_TCP, uint16(1024+i), 443) }
		single("pl_pod_out", nil, "new TCP flow (probe + 2 inserts), slice <= 12.5% full", gen, podCtx, newPerRun, tcxNext, stNatNew,
			func() *natObjects { n := newNat(t); t.Cleanup(func() { n.Close() }); return n })

		// Fill one slice (4096 ports) flow by flow until the probe fails.
		n := newNat(t)
		first, ok, fails := -1, 0, 0
		var bucket []float64
		for i := 0; i < 2*sliceSize; i++ {
			d := gen(i)
			_, z, err := perf.TestRun(triv, d, podCtx, 1, nil)
			require.NoError(t, err)
			ret, ns, err := perf.TestRun(n.PlPodOut, d, podCtx, 1, nil)
			require.NoError(t, err)
			bucket = append(bucket, float64(ns)-float64(z))
			if ret == tcxDrop {
				fails++
				if first < 0 {
					first = i
				}
			} else {
				ok++
			}
			if (i+1)%512 == 0 {
				t.Logf("SPIKE10 NATFILL ebpf %s flows=%d mapped=%d failed=%d cost-median-ns=%.0f (last 512 flows)",
					f.name, i+1, ok, fails, perf.Median(bucket))
				bucket = bucket[:0]
			}
		}
		t.Logf("SPIKE10 NATFILL ebpf %s RESULT first-failure-at-flow=%d mapped=%d of %d attempts (slice %d ports, %d probes) no_port=%d nat-entries=%d",
			f.name, first+1, ok, 2*sliceSize, sliceSize, natProbes, stat(t, n.Stats, stNatNoPort), countNat(t, n.Nat))
		ml, err := perf.Memlock(n.Nat.FD())
		require.NoError(t, err)
		info, _ := n.Nat.Info()
		t.Logf("SPIKE10 MAP nat nat(%s) entries=%d max_entries=%d key=%d value=%d memlock=%d", f.name, countNat(t, n.Nat), info.MaxEntries, info.KeySize, info.ValueSize, ml)
		n.Close()
		o.Close()
	}
}

// TestPerfMapMemory logs the NAT map's kernel memory accounting (fdinfo
// memlock) empty and filled to its 131,072 max_entries (65,536 flows).
func TestPerfMapMemory(t *testing.T) {
	perf.Require(t)
	o := newNat(t)
	defer o.Close()
	empty, err := perf.Memlock(o.Nat.FD())
	require.NoError(t, err)
	var ks []natNatKey
	var vs []natNatVal
	for i := 0; i < 131072; i++ {
		ks = append(ks, natNatKey{Raddr: [4]uint32{uint32(i/4096 + 1)}, Lport: uint16(i % 4096), Proto: 6, Family: 2, Dir: uint8(1 + i%2)})
		vs = append(vs, natNatVal{Port: 1})
	}
	_, err = o.Nat.BatchUpdate(ks, vs, nil)
	require.NoError(t, err)
	full, err := perf.Memlock(o.Nat.FD())
	require.NoError(t, err)
	info, _ := o.Nat.Info()
	t.Logf("SPIKE10 MAPMEM nat nat type=%s max_entries=%d key=%d value=%d memlock-empty=%d memlock-full=%d",
		info.Type, info.MaxEntries, info.KeySize, info.ValueSize, empty, full)
	for _, m := range []struct {
		name string
		m    *ebpf.Map
	}{{"pod_nat", o.PodNat}, {"egress_addrs", o.EgressAddrs}, {"slices", o.Slices}, {"scratch", o.Scratch}} {
		ml, err := perf.Memlock(m.m.FD())
		require.NoError(t, err)
		info, _ := m.m.Info()
		t.Logf("SPIKE10 MAPMEM nat %s type=%s max_entries=%d memlock-empty=%d", m.name, info.Type, info.MaxEntries, ml)
	}
}
