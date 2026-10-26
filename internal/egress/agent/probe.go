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
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"

	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Tunnel peer probes (design 6.8; user decision 2026-10-25): every
// probeInterval each live egress-capable peer is sent a probe in every
// underlay family both ends have; one that hasn't answered for probeLost
// intervals, after being probed that long, is unreachable.
const (
	probeInterval = 10 * time.Second
	probeLost     = 3
)

// probeTarget is a peer underlay address being probed: its node and when
// probing it began (CLOCK_MONOTONIC nanoseconds).
type probeTarget struct {
	node  string
	since uint64
}

// monotonic is CLOCK_MONOTONIC in nanoseconds, the clock of
// bpf_ktime_get_ns.
func monotonic() uint64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return uint64(ts.Nano())
}

// runProbes probes the tunnel peers until s stops, once the datapath is
// up. The targets go to the datapath goroutine, which reads the replies.
func (a *Agent) runProbes(s *session) {
	t := time.NewTicker(probeInterval)
	defer t.Stop()
	targets := map[netip.Addr]probeTarget{}
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		c := a.capability.Load()
		if c == nil || !c.EgressCapable || !s.synced.Load() {
			continue
		}
		port := 6082
		if p := a.config.Load(); p != nil && p.spec != nil && p.spec.TunnelPort != 0 {
			port = int(p.spec.TunnelPort)
		}
		self := underlay(s.nodes, a.node)
		if self == nil {
			continue
		}
		now := monotonic()
		next := map[netip.Addr]probeTarget{}
		live := a.elect.SteeringLive()
		for _, obj := range s.nodes.List() {
			n := obj.(*corev1.Node)
			if n.Name == a.node || n.Labels[purelbv2.EgressCapableLabel] != "true" || !slices.Contains(live, n.Name) {
				continue
			}
			peer := underlay(s.nodes, n.Name)
			if peer == nil {
				continue
			}
			for _, pair := range [][2]string{{self.IPv4, peer.IPv4}, {self.IPv6, peer.IPv6}} {
				from, err1 := netip.ParseAddr(pair[0])
				to, err2 := netip.ParseAddr(pair[1])
				if err1 != nil || err2 != nil {
					continue
				}
				tg, ok := targets[to]
				if !ok || tg.node != n.Name {
					tg = probeTarget{node: n.Name, since: now}
				}
				next[to] = tg
				pkt := datapath.ProbePacket(c.TunnelEncapsulation, from, to,
					datapath.TunnelMAC(datapath.NodeID(a.node)), datapath.TunnelMAC(datapath.NodeID(n.Name)))
				if err := a.sendProbe(from, to, port, pkt); err != nil {
					logging.Debug(a.logger, "op", "egressProbe", "node", a.node, "peer", n.Name, "address", to, "error", err)
				}
			}
		}
		targets = next
		a.probing.Store(&next) // not written again: the next round builds its own
	}
}

// sendUDPProbe sends pkt from this node's underlay address to a peer's
// tunnel port (the default sendProbe).
func sendUDPProbe(from, to netip.Addr, port int, pkt []byte) error {
	conn, err := net.DialUDP("udp", &net.UDPAddr{IP: from.AsSlice()}, &net.UDPAddr{IP: to.AsSlice(), Port: port})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(pkt)
	return err
}

// unreachable is the probed peers that haven't answered for probeLost
// intervals, after being probed that long, as "node (address)", and how
// many are probed. Datapath goroutine only.
func (a *Agent) unreachable(st *dpState) ([]string, int) {
	p := a.probing.Load()
	if p == nil || st.objs == nil {
		return nil, 0
	}
	seen, err := st.objs.ProbeReplies()
	if err != nil {
		return nil, 0
	}
	lost := uint64(probeLost * probeInterval)
	now := monotonic()
	var out []string
	for addr, tg := range *p {
		if now-tg.since < lost {
			continue // not probed long enough to judge
		}
		if last := seen[addr]; last == 0 || now-last > lost {
			out = append(out, fmt.Sprintf("%s (%s)", tg.node, addr))
		}
	}
	slices.Sort(out)
	return out, len(*p)
}
