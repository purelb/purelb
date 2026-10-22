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

// Package perf holds the measurement helpers of Phase 0 spike 10 (the
// egress performance budgets), shared by the spikes and natspike test
// packages. Spike code, not production code. Run with vm-perf.sh.
package perf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"golang.org/x/sys/unix"
)

var enabled = flag.Bool("spike10", false, "run the spike 10 performance measurements")

// Require skips unless the test binary runs in the VM harness with -spike10.
func Require(t *testing.T) {
	t.Helper()
	if os.Getenv("EGRESS_SPIKES") != "required" || !*enabled {
		t.Skip("spike 10 runs in the VM harness: hack/egress-spikes/vm/perf/vm-perf.sh")
	}
}

// Runs is the number of runs every measurement is repeated; results are
// reported as min / median over them.
const Runs = 5

// ---- BPF_PROG_TEST_RUN -------------------------------------------------------

// progRunAttr is the BPF_PROG_TEST_RUN part of union bpf_attr.
type progRunAttr struct {
	progFd      uint32
	retval      uint32
	dataSizeIn  uint32
	dataSizeOut uint32
	dataIn      uint64
	dataOut     uint64
	repeat      uint32
	duration    uint32
	ctxSizeIn   uint32
	ctxSizeOut  uint32
	ctxIn       uint64
	ctxOut      uint64
	flags       uint32
	cpu         uint32
	batchSize   uint32
	_           uint32
}

const bpfProgTestRun = 10

// TestRun runs prog through BPF_PROG_TEST_RUN repeat times on one skb built
// from data, with the optional struct __sk_buff ctx (see SkbCtx). It returns
// the last return value and the kernel's per-iteration time in ns (the
// same number ebpf.Program.Benchmark returns, which cannot pass a context).
// When out is not nil the packet after the last run is copied into it.
func TestRun(prog *ebpf.Program, data, ctx []byte, repeat uint32, out []byte) (ret, ns uint32, err error) {
	for try := 0; try < 10; try++ {
		a := progRunAttr{progFd: uint32(prog.FD()), dataSizeIn: uint32(len(data)), repeat: repeat}
		a.dataIn = uint64(uintptr(unsafe.Pointer(&data[0])))
		if out != nil {
			a.dataOut = uint64(uintptr(unsafe.Pointer(&out[0])))
			a.dataSizeOut = uint32(len(out))
		}
		if ctx != nil {
			a.ctxIn = uint64(uintptr(unsafe.Pointer(&ctx[0])))
			a.ctxSizeIn = uint32(len(ctx))
		}
		_, _, e := unix.Syscall(unix.SYS_BPF, bpfProgTestRun, uintptr(unsafe.Pointer(&a)), unsafe.Sizeof(a))
		runtime.KeepAlive(data)
		runtime.KeepAlive(ctx)
		runtime.KeepAlive(out)
		if e == unix.EINTR {
			continue // a signal interrupted the loop: run it again
		}
		if e != 0 {
			return 0, 0, fmt.Errorf("BPF_PROG_TEST_RUN: %w", e)
		}
		return a.retval, a.duration, nil
	}
	return 0, 0, errors.New("BPF_PROG_TEST_RUN: interrupted 10 times")
}

// SkbCtx returns a struct __sk_buff (192 bytes) for TestRun with mark,
// ingress_ifindex and ifindex set; everything else is zero. ifindex 0 (or 1)
// leaves the skb on the namespace's loopback device.
func SkbCtx(mark, ingressIfindex, ifindex uint32) []byte {
	b := make([]byte, 192)
	binary.LittleEndian.PutUint32(b[8:], mark)
	binary.LittleEndian.PutUint32(b[36:], ingressIfindex)
	binary.LittleEndian.PutUint32(b[40:], ifindex)
	return b
}

// Trivial loads a tc program that only returns TCX_NEXT: the fixed cost of
// one test-run iteration (timer, bh disable, dispatch) for SingleShot.
func Trivial() (*ebpf.Program, error) {
	return ebpf.NewProgram(&ebpf.ProgramSpec{
		Type:         ebpf.SchedCLS,
		Instructions: asm.Instructions{asm.Mov.Imm32(asm.R0, -1), asm.Return()},
		License:      "Dual BSD/GPL",
	})
}

// Dropper loads a tc program that drops everything (TCX_DROP): a packet
// sink that keeps the receiving stack out of packet-rate measurements.
func Dropper() (*ebpf.Program, error) {
	return ebpf.NewProgram(&ebpf.ProgramSpec{
		Type:         ebpf.SchedCLS,
		Instructions: asm.Instructions{asm.Mov.Imm32(asm.R0, 2), asm.Return()},
		License:      "Dual BSD/GPL",
	})
}

// Repeat is the address guard's method (TestBPFNonVIPBudget): runs runs of
// repeat iterations on one skb, each run's per-iteration time. Only valid for
// paths that leave the packet as they found it.
func Repeat(prog *ebpf.Program, data, ctx []byte, repeat uint32, runs int) ([]float64, uint32, error) {
	var v []float64
	var ret uint32
	for range runs {
		r, ns, err := TestRun(prog, data, ctx, repeat, nil)
		if err != nil {
			return nil, 0, err
		}
		ret = r
		v = append(v, float64(ns))
	}
	return v, ret, nil
}

// Batched is for paths that change the packet in a way that a bounded
// number of iterations tolerates (a TTL decrement): calls test runs of
// repeat iterations, each on a fresh skb; the mean per-iteration time.
func Batched(prog *ebpf.Program, data, ctx []byte, repeat uint32, calls int) (float64, uint32, error) {
	var sum float64
	var ret uint32
	for range calls {
		r, ns, err := TestRun(prog, data, ctx, repeat, nil)
		if err != nil {
			return 0, 0, err
		}
		ret = r
		sum += float64(ns)
	}
	return sum / float64(calls), ret, nil
}

// SingleShot is for paths that rewrite the packet (NAT, a cleared mark, a
// pushed header): calls test runs of one iteration, each on a fresh skb
// built from gen(i), interleaved with the trivial program on the same
// packet. It returns the median of prog's times minus the median of the
// trivial program's (the test-run fixed cost), and that fixed cost. ret is
// the last return value.
func SingleShot(prog, trivial *ebpf.Program, gen func(i int) []byte, ctx []byte, calls int) (net, fixed float64, ret uint32, err error) {
	p := make([]float64, 0, calls)
	z := make([]float64, 0, calls)
	for i := range calls {
		d := gen(i)
		_, nz, err := TestRun(trivial, d, ctx, 1, nil)
		if err != nil {
			return 0, 0, 0, err
		}
		r, np, err := TestRun(prog, d, ctx, 1, nil)
		if err != nil {
			return 0, 0, 0, err
		}
		ret = r
		p = append(p, float64(np))
		z = append(z, float64(nz))
	}
	return Median(p) - Median(z), Median(z), ret, nil
}

// Median of v (v is reordered).
func Median(v []float64) float64 {
	sort.Float64s(v)
	n := len(v)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// MinMed formats min / median of v.
func MinMed(v []float64) string {
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return fmt.Sprintf("min=%.4g med=%.4g runs=%s", c[0], Median(c), fmtRuns(v))
}

// MaxMed formats max / median of v (throughput: higher is better).
func MaxMed(v []float64) string {
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return fmt.Sprintf("min=%.4g med=%.4g max=%.4g runs=%s", c[0], Median(c), c[len(c)-1], fmtRuns(v))
}

func fmtRuns(v []float64) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = strconv.FormatFloat(x, 'g', 4, 64)
	}
	return "[" + strings.Join(s, " ") + "]"
}

// Memlock is the kernel's memory accounting for a BPF map or program fd
// (fdinfo "memlock"; from 6.4 the map's actual usage, before that its
// preallocated maximum).
func Memlock(fd int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "memlock:"); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("no memlock in fdinfo")
}

// ---- packets -------------------------------------------------------------------

// Eth prepends an Ethernet header (the EtherType from the IP version).
func Eth(ip []byte) []byte {
	e := make([]byte, 14, 14+len(ip))
	copy(e, []byte{0x02, 0, 0, 0, 0, 1, 0x02, 0, 0, 0, 0, 2})
	et := uint16(unix.ETH_P_IP)
	if ip[0]>>4 == 6 {
		et = unix.ETH_P_IPV6
	}
	binary.BigEndian.PutUint16(e[12:], et)
	return append(e, ip...)
}

// L4 is a transport header plus payload: TCP (an ACK, 20 bytes), UDP or an
// ICMP / ICMPv6 echo request. Checksums are left zero: no program under test
// verifies them, and the rewriting ones update them incrementally.
func L4(proto uint8, sport, dport uint16, payload int) []byte {
	switch proto {
	case unix.IPPROTO_TCP:
		b := make([]byte, 20+payload)
		binary.BigEndian.PutUint16(b[0:], sport)
		binary.BigEndian.PutUint16(b[2:], dport)
		b[12] = 5 << 4
		b[13] = 0x10 // ACK
		return b
	case unix.IPPROTO_UDP:
		b := make([]byte, 8+payload)
		binary.BigEndian.PutUint16(b[0:], sport)
		binary.BigEndian.PutUint16(b[2:], dport)
		binary.BigEndian.PutUint16(b[4:], uint16(len(b)))
		return b
	}
	b := make([]byte, 8+payload) // echo: type, code, csum, id, seq
	b[0] = 8
	if proto == unix.IPPROTO_ICMPV6 {
		b[0] = 128
	}
	binary.BigEndian.PutUint16(b[4:], sport)
	return b
}

// Frag: 0 a whole datagram, 1 a first fragment, 2 a later fragment.
const (
	Whole = iota
	First
	Later
)

// IPv4 carries l4 in an IPv4 packet (ttl, fragment kind frag, IP ID id).
func IPv4(src, dst [4]byte, proto uint8, l4 []byte, ttl uint8, frag int, id uint16) []byte {
	p := make([]byte, 20+len(l4))
	p[0], p[8], p[9] = 0x45, ttl, proto
	binary.BigEndian.PutUint16(p[2:], uint16(len(p)))
	binary.BigEndian.PutUint16(p[4:], id)
	switch frag {
	case First:
		binary.BigEndian.PutUint16(p[6:], 0x2000)
	case Later:
		binary.BigEndian.PutUint16(p[6:], 185) // offset 1480
	}
	copy(p[12:], src[:])
	copy(p[16:], dst[:])
	var s uint32
	for i := 0; i < 20; i += 2 {
		s += uint32(binary.BigEndian.Uint16(p[i:]))
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	binary.BigEndian.PutUint16(p[10:], ^uint16(s))
	copy(p[20:], l4)
	return p
}

// IPv6 carries l4 in an IPv6 packet (hop limit hop). hbh adds an 8-byte
// Hop-by-Hop header; frag adds a Fragment Header (ID id) of that kind.
func IPv6(src, dst [16]byte, proto uint8, l4 []byte, hop uint8, hbh bool, frag int, id uint32) []byte {
	var ext []byte
	next := proto
	if frag != Whole {
		fh := make([]byte, 8)
		fh[0] = next
		if frag == First {
			binary.BigEndian.PutUint16(fh[2:], 1) // offset 0, M
		} else {
			binary.BigEndian.PutUint16(fh[2:], 1448) // offset 181*8
		}
		binary.BigEndian.PutUint32(fh[4:], id)
		ext = fh
		next = unix.IPPROTO_FRAGMENT
	}
	if hbh {
		h := []byte{next, 0, 1, 4, 0, 0, 0, 0}
		ext = append(h, ext...)
		next = 0
	}
	p := make([]byte, 40, 40+len(ext)+len(l4))
	p[0], p[6], p[7] = 0x60, next, hop
	binary.BigEndian.PutUint16(p[4:], uint16(len(ext)+len(l4)))
	copy(p[8:], src[:])
	copy(p[24:], dst[:])
	return append(append(p, ext...), l4...)
}

// ---- CPU and the stream / packet-rate loads ---------------------------------

// CPU is a /proc/stat sample, VM-wide.
type CPU struct{ busy, total uint64 }

// ReadCPU samples /proc/stat's aggregate line (busy excludes idle and iowait).
func ReadCPU() CPU {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return CPU{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	fs := strings.Fields(sc.Text())
	var c CPU
	for i, s := range fs[1:] {
		v, _ := strconv.ParseUint(s, 10, 64)
		c.total += v
		if i != 3 && i != 4 { // idle, iowait
			c.busy += v
		}
	}
	return c
}

// Cores is the average number of busy CPUs between a and b.
func Cores(a, b CPU) float64 {
	if b.total == a.total {
		return 0
	}
	return float64(b.busy-a.busy) / float64(b.total-a.total) * float64(runtime.NumCPU())
}

// Serve answers each connection on ln: the first byte 'U' means the client
// uploads (discard what it sends), 'D' that it downloads (send until it
// closes).
func Serve(ln net.Listener) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 1)
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				if b[0] == 'U' {
					_, _ = io.Copy(io.Discard, c)
					return
				}
				buf := make([]byte, 64<<10)
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()
}

// Stream runs one TCP transfer for d over a connection from dial ('U'
// upload or 'D' download, see Serve) and returns Gbit/s and busy CPUs.
func Stream(dial func() (net.Conn, error), dir byte, d time.Duration) (gbps, cores float64, err error) {
	c, err := dial()
	if err != nil {
		return 0, 0, err
	}
	defer c.Close()
	if _, err := c.Write([]byte{dir}); err != nil {
		return 0, 0, err
	}
	buf := make([]byte, 64<<10)
	if dir == 'D' { // let the sender ramp up before the window
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			return 0, 0, err
		}
	}
	_ = c.SetDeadline(time.Now().Add(d + 5*time.Second))
	c0 := ReadCPU()
	start := time.Now()
	var n int64
	for time.Since(start) < d {
		var k int
		if dir == 'U' {
			k, err = c.Write(buf)
		} else {
			k, err = c.Read(buf)
		}
		n += int64(k)
		if err != nil {
			return 0, 0, fmt.Errorf("after %d bytes: %w", n, err)
		}
	}
	el := time.Since(start)
	c1 := ReadCPU()
	return float64(n*8) / el.Seconds() / 1e9, Cores(c0, c1), nil
}

// mmsghdr is struct mmsghdr on 64-bit Linux.
type mmsghdr struct {
	hdr unix.Msghdr
	len uint32
	_   [4]byte
}

// Blast sends size-byte UDP datagrams on the connected socket c for d,
// 64 per sendmmsg call, and returns how many it sent.
func Blast(c *net.UDPConn, size int, d time.Duration) (int64, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	payload := make([]byte, size)
	const batch = 64
	iov := make([]unix.Iovec, batch)
	msgs := make([]mmsghdr, batch)
	for i := range msgs {
		iov[i].Base = &payload[0]
		iov[i].SetLen(size)
		msgs[i].hdr.Iov = &iov[i]
		msgs[i].hdr.SetIovlen(1)
	}
	var sent int64
	var serr error
	start := time.Now()
	for time.Since(start) < d && serr == nil {
		werr := rc.Write(func(fd uintptr) bool {
			n, _, e := unix.Syscall6(unix.SYS_SENDMMSG, fd, uintptr(unsafe.Pointer(&msgs[0])), batch, 0, 0, 0)
			if e == unix.EAGAIN {
				return false
			}
			if e != 0 {
				serr = e
				return true
			}
			sent += int64(n)
			return true
		})
		if werr != nil {
			return sent, werr
		}
	}
	runtime.KeepAlive(payload)
	runtime.KeepAlive(iov)
	return sent, serr
}

// ---- conntrack -------------------------------------------------------------------

// CTStats sums /proc/net/stat/nf_conntrack (per CPU, hex) of the calling
// thread's namespace by column name. Kernels built without
// CONFIG_NF_CONNTRACK_PROCFS (Ubuntu's) have no such file: the map is then
// empty, with "unavailable" set.
func CTStats() (map[string]uint64, error) {
	b, err := os.ReadFile("/proc/net/stat/nf_conntrack")
	if errors.Is(err, os.ErrNotExist) {
		return map[string]uint64{"unavailable": 1}, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	names := strings.Fields(lines[0])
	m := map[string]uint64{}
	for _, l := range lines[1:] {
		for i, f := range strings.Fields(l) {
			v, _ := strconv.ParseUint(f, 16, 64)
			if i < len(names) && names[i] != "entries" { // entries is global, repeated per CPU
				m[names[i]] += v
			} else if i < len(names) {
				m[names[i]] = v
			}
		}
	}
	return m, nil
}

// CTCount reads nf_conntrack_count in the calling thread's namespace.
func CTCount() (int, error) {
	b, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_count")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// CTDump dumps the conntrack table of family (unix.AF_INET or AF_INET6)
// in the calling thread's namespace through ctnetlink, with the kernel-side
// CTA_MARK / CTA_MARK_MASK filter when mask is not 0, and returns the
// number of entries the kernel sent and the time it took. Entries are only
// counted, not parsed.
func CTDump(family uint8, mark, mask uint32) (int, time.Duration, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return 0, 0, err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, 0, err
	}
	msg := make([]byte, 16+4)
	binary.LittleEndian.PutUint16(msg[4:], 1<<8|1) // NFNL_SUBSYS_CTNETLINK, IPCTNL_MSG_CT_GET
	binary.LittleEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.LittleEndian.PutUint32(msg[8:], 1)
	msg[16] = family // nfgenmsg: family, version 0, res_id 0
	if mask != 0 {
		for _, a := range []struct {
			typ uint16
			v   uint32
		}{{8, mark}, {21, mask}} { // CTA_MARK, CTA_MARK_MASK
			at := make([]byte, 8)
			binary.LittleEndian.PutUint16(at[0:], 8)
			binary.LittleEndian.PutUint16(at[2:], a.typ)
			binary.BigEndian.PutUint32(at[4:], a.v)
			msg = append(msg, at...)
		}
	}
	binary.LittleEndian.PutUint32(msg[0:], uint32(len(msg)))
	buf := make([]byte, 1<<20)
	start := time.Now()
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, 0, err
	}
	n := 0
	for {
		k, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return n, time.Since(start), err
		}
		for off := 0; off+16 <= k; {
			l := int(binary.LittleEndian.Uint32(buf[off:]))
			typ := binary.LittleEndian.Uint16(buf[off+4:])
			switch typ {
			case unix.NLMSG_DONE:
				return n, time.Since(start), nil
			case unix.NLMSG_ERROR:
				if e := int32(binary.LittleEndian.Uint32(buf[off+16:])); e != 0 {
					return n, time.Since(start), fmt.Errorf("ctnetlink dump: %w", unix.Errno(-e))
				}
			default:
				n++
			}
			if l < 16 {
				return n, time.Since(start), errors.New("ctnetlink dump: short message")
			}
			off += (l + 3) &^ 3
		}
	}
}

// Slab returns the active objects and the object size of a slab cache from
// /proc/slabinfo.
func Slab(name string) (active, objsize int, err error) {
	b, err := os.ReadFile("/proc/slabinfo")
	if err != nil {
		return 0, 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 3 && f[0] == name {
			active, _ = strconv.Atoi(f[1])
			objsize, _ = strconv.Atoi(f[3])
			return active, objsize, nil
		}
	}
	return 0, 0, fmt.Errorf("no slab cache %s", name)
}

// MemLine summarises /proc/meminfo (kB) for spotting memory growth.
func MemLine() string {
	var b strings.Builder
	for _, f := range []string{"MemAvailable", "Slab", "SUnreclaim", "Percpu", "KernelStack", "Shmem", "AnonPages"} {
		fmt.Fprintf(&b, " %s=%d", f, MemInfo(f))
	}
	return b.String()
}

// SlabTop lists the n slab caches using the most memory (/proc/slabinfo:
// name objects x size).
func SlabTop(n int) string {
	b, err := os.ReadFile("/proc/slabinfo")
	if err != nil {
		return err.Error()
	}
	type c struct {
		name      string
		objs, siz int
	}
	var cs []c
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || strings.HasPrefix(l, "#") || f[0] == "slabinfo" {
			continue
		}
		o, _ := strconv.Atoi(f[2])
		z, _ := strconv.Atoi(f[3])
		cs = append(cs, c{f[0], o, z})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].objs*cs[i].siz > cs[j].objs*cs[j].siz })
	var s strings.Builder
	for i := 0; i < n && i < len(cs); i++ {
		fmt.Fprintf(&s, " %s=%dx%d(%dkB)", cs[i].name, cs[i].objs, cs[i].siz, cs[i].objs*cs[i].siz/1024)
	}
	return s.String()
}

// MemInfo returns a /proc/meminfo field in kB.
func MemInfo(field string) int {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, field+":"); ok {
			n, _ := strconv.Atoi(strings.Fields(v)[0])
			return n
		}
	}
	return -1
}

// ---- routing-rule lookups ------------------------------------------------------

// FibProg loads a tc program that calls bpf_fib_lookup (no flags: the
// RPDB rules apply, as on a forwarded packet; the lookup mark is 0, a packet
// that is not egress) for dst from ingress device ifindex, and returns its
// result code. src and dst are 4 or 16 bytes.
func FibProg(ifindex int, src, dst []byte) (*ebpf.Program, error) {
	fam := int64(unix.AF_INET)
	if len(dst) == 16 {
		fam = unix.AF_INET6
	}
	in := asm.Instructions{asm.Mov.Reg(asm.R6, asm.R1), asm.Mov.Imm(asm.R0, 0)}
	for off := int16(-64); off < 0; off += 8 {
		in = append(in, asm.StoreMem(asm.RFP, off, asm.R0, asm.DWord))
	}
	in = append(in,
		asm.StoreImm(asm.RFP, -64, fam, asm.Byte),
		asm.StoreImm(asm.RFP, -64+8, int64(ifindex), asm.Word),
	)
	for i := 0; i < len(dst); i += 4 {
		in = append(in,
			asm.StoreImm(asm.RFP, int16(-64+16+i), int64(int32(binary.LittleEndian.Uint32(src[i:]))), asm.Word),
			asm.StoreImm(asm.RFP, int16(-64+32+i), int64(int32(binary.LittleEndian.Uint32(dst[i:]))), asm.Word))
	}
	in = append(in,
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -64),
		asm.Mov.Imm(asm.R3, 64),
		asm.Mov.Imm(asm.R4, 0),
		asm.FnFibLookup.Call(),
		asm.Return(),
	)
	return ebpf.NewProgram(&ebpf.ProgramSpec{Type: ebpf.SchedCLS, Instructions: in, License: "Dual BSD/GPL"})
}
