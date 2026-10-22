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

// Topology helpers, copied from ../topo_test.go (package spikes) so this
// package builds on its own.

import (
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func requireVM(t *testing.T) {
	t.Helper()
	if os.Getenv("EGRESS_SPIKES") != "required" {
		t.Skip("egress spikes run in the VM harness: SPIKES_PKG=natspike hack/egress-spikes/vm/vm-test.sh")
	}
}

type node struct {
	name string
	h    netns.NsHandle
	nl   *netlink.Handle
}

func newNode(t *testing.T, name string) *node {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer orig.Close()
	h, err := netns.New()
	require.NoError(t, err)
	require.NoError(t, netns.Set(orig))
	nl, err := netlink.NewHandleAt(h, unix.NETLINK_ROUTE)
	require.NoError(t, err)
	n := &node{name: name, h: h, nl: nl}
	t.Cleanup(func() { nl.Close(); h.Close() })
	lo, err := nl.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, nl.LinkSetUp(lo))
	return n
}

// in runs fn with the calling OS thread switched into the node's namespace.
func (n *node) in(fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // the thread is discarded, never unlocked
		if err := netns.Set(n.h); err != nil {
			errc <- err
			return
		}
		errc <- fn()
	}()
	return <-errc
}

func (n *node) sysctl(t *testing.T, key, val string) {
	t.Helper()
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	require.NoError(t, n.in(func() error { return os.WriteFile(path, []byte(val), 0o644) }), "%s: %s", n.name, key)
}

func (n *node) sysctlGet(t *testing.T, key string) string {
	t.Helper()
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	var v []byte
	require.NoError(t, n.in(func() error {
		var err error
		v, err = os.ReadFile(path)
		return err
	}), "%s: %s", n.name, key)
	return strings.TrimSpace(string(v))
}

// snmpNode returns the node's /proc/net/snmp counters keyed like
// "IpInAddrErrors" (read through thread-self: n.in switches one thread).
func snmpNode(t *testing.T, n *node) map[string]int64 {
	t.Helper()
	m := map[string]int64{}
	require.NoError(t, n.in(func() error {
		raw, err := os.ReadFile("/proc/thread-self/net/snmp")
		if err != nil {
			return err
		}
		lines := strings.Split(string(raw), "\n")
		for i := 0; i+1 < len(lines); i += 2 {
			h, v := strings.Fields(lines[i]), strings.Fields(lines[i+1])
			if len(h) < 2 || len(h) != len(v) {
				continue
			}
			p := strings.TrimSuffix(h[0], ":")
			for j := 1; j < len(h); j++ {
				x, _ := strconv.ParseInt(v[j], 10, 64)
				m[p+h[j]] = x
			}
		}
		return nil
	}))
	return m
}

func veth(t *testing.T, a *node, an string, b *node, bn string) {
	t.Helper()
	require.NoError(t, a.nl.LinkAdd(&netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: an},
		PeerName:      bn,
		PeerNamespace: netlink.NsFd(int(b.h)),
	}))
	up(t, a, an)
	up(t, b, bn)
}

func up(t *testing.T, n *node, name string) {
	t.Helper()
	l, err := n.nl.LinkByName(name)
	require.NoError(t, err, "%s: %s", n.name, name)
	require.NoError(t, n.nl.LinkSetUp(l))
}

func ifindex(t *testing.T, n *node, name string) int {
	t.Helper()
	l, err := n.nl.LinkByName(name)
	require.NoError(t, err, "%s: %s", n.name, name)
	return l.Attrs().Index
}

func addr(t *testing.T, n *node, dev, cidr string) {
	t.Helper()
	l, err := n.nl.LinkByName(dev)
	require.NoError(t, err)
	a, err := netlink.ParseAddr(cidr)
	require.NoError(t, err)
	a.Flags |= 0x02 // IFA_F_NODAD
	require.NoError(t, n.nl.AddrAdd(l, a), "%s: %s %s", n.name, dev, cidr)
}

func defaultRoute(t *testing.T, n *node, gw string) {
	t.Helper()
	ip := net.ParseIP(gw)
	r := &netlink.Route{Gw: ip}
	if ip.To4() == nil {
		r.Family = netlink.FAMILY_V6
	}
	require.NoError(t, n.nl.RouteAdd(r), "%s: route %v", n.name, r)
}

func attach(t *testing.T, n *node, dev string, prog *ebpf.Program, at ebpf.AttachType) {
	t.Helper()
	idx := ifindex(t, n, dev)
	var l link.Link
	require.NoError(t, n.in(func() error {
		var err error
		l, err = link.AttachTCX(link.TCXOptions{Program: prog, Interface: idx, Attach: at, Anchor: link.Tail()})
		return err
	}), "%s: attach %s to %s", n.name, prog, dev)
	t.Cleanup(func() { l.Close() })
}

// ip16 returns the address as the four network-order words the maps use.
func ip16(s string) [4]uint32 {
	a := netip.MustParseAddr(s)
	var w [4]uint32
	if a.Is4() {
		b := a.As4()
		w[0] = leWord(b[:])
		return w
	}
	b := a.As16()
	for i := 0; i < 4; i++ {
		w[i] = leWord(b[i*4 : i*4+4])
	}
	return w
}

func leWord(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func stat(t *testing.T, m *ebpf.Map, i uint32) uint64 {
	t.Helper()
	var per []uint64
	require.NoError(t, m.Lookup(i, &per))
	var sum uint64
	for _, v := range per {
		sum += v
	}
	return sum
}
