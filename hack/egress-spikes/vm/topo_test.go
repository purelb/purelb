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
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// requireVM skips unless the spike runs where it may create namespaces
// and load programs (EGRESS_SPIKES=required in the VM harness).
func requireVM(t *testing.T) {
	t.Helper()
	if os.Getenv("EGRESS_SPIKES") != "required" {
		t.Skip("egress spikes run in the VM harness: hack/egress-spikes/vm/vm-test.sh")
	}
}

// node is one network namespace standing in for a host.
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
	nl, err := netlink.NewHandleAt(h, unix.NETLINK_ROUTE) // not every family is loaded in the VM
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

// veth links a (name an) to b (name bn) and brings both ends up.
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
	a.Flags |= 0x02 // IFA_F_NODAD: tests should not wait for DAD
	require.NoError(t, n.nl.AddrAdd(l, a), "%s: %s %s", n.name, dev, cidr)
}

func route(t *testing.T, n *node, r *netlink.Route) {
	t.Helper()
	require.NoError(t, n.nl.RouteAdd(r), "%s: route %v", n.name, r)
}

func defaultRoute(t *testing.T, n *node, gw string) {
	t.Helper()
	ip := net.ParseIP(gw)
	r := &netlink.Route{Gw: ip}
	if ip.To4() == nil {
		r.Family = netlink.FAMILY_V6
	}
	route(t, n, r)
}

// attach puts prog on dev's tcx hook inside node n and closes it at cleanup.
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

// leWord reinterprets 4 network-order bytes as the little-endian uint32 the
// kernel stores (the BPF side compares raw words).
func leWord(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func family(s string) uint8 {
	if netip.MustParseAddr(s).Is4() {
		return 2
	}
	return 10
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

func stats(t *testing.T, m *ebpf.Map) string {
	names := []string{"steered", "steer_no_announcer", "to_tunnel", "owner_local", "no_owner",
		"transit", "transit_forged", "transit_ttl", "reply_local", "reply_not_owner",
		"pkt_type_host", "pkt_type_other", "pod_tagged", "pod_spoof", "transit_no_sender"}
	var b strings.Builder
	for i, n := range names {
		if v := stat(t, m, uint32(i)); v > 0 {
			fmt.Fprintf(&b, " %s=%d", n, v)
		}
	}
	return b.String()
}

// kstats summarises a node's kernel drop and forwarding counters, and the
// rx/tx of the named links, for spike failure messages.
func kstats(t *testing.T, n *node, links ...string) string {
	var b strings.Builder
	_ = n.in(func() error {
		for _, f := range []string{"/proc/net/snmp", "/proc/net/snmp6", "/proc/net/netstat"} {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			lines := strings.Split(string(raw), "\n")
			for i := 0; i+1 < len(lines); i++ {
				h, v := strings.Fields(lines[i]), strings.Fields(lines[i+1])
				if len(h) > 1 && len(h) == len(v) && h[0] == v[0] {
					for j := 1; j < len(h); j++ {
						if v[j] != "0" && !strings.HasPrefix(h[j], "Rto") && !strings.HasPrefix(h[j], "Max") && h[j] != "Forwarding" && h[j] != "DefaultTTL" {
							fmt.Fprintf(&b, " %s%s=%s", strings.TrimSuffix(h[0], ":"), h[j], v[j])
						}
					}
					i++
				} else if len(h) == 2 && strings.Contains(h[0], "6") {
					if h[1] != "0" && !strings.Contains(h[0], "Octets") {
						fmt.Fprintf(&b, " %s=%s", h[0], h[1])
					}
				}
			}
		}
		return nil
	})
	for _, name := range links {
		if l, err := n.nl.LinkByName(name); err == nil && l.Attrs().Statistics != nil {
			s := l.Attrs().Statistics
			fmt.Fprintf(&b, " [%s rx=%d tx=%d rxdrop=%d txdrop=%d]", name, s.RxPackets, s.TxPackets, s.RxDropped, s.TxDropped)
		}
	}
	return b.String()
}
