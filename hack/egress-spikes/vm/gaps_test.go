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

// Gap tests for Phase 0 spikes 1, 6 and 7: the questions the design
// (docs/egress-design.md) still marks OPEN against those spikes.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type gapFam struct {
	name         string // IPv4 | IPv6
	egress, peer string
}

var gapFams = []gapFam{{"IPv4", "172.30.10.110", "172.30.10.1"}, {"IPv6", "fdaa::110", "fdaa::1"}}

func yes(b bool, y, n string) string {
	if b {
		return y
	}
	return n
}

// ---- Spike 6 gap 1: out_src with "ct direction reply" ----------------------

const (
	outEstRel    = "established-related+ND" // the form spike 6 verified
	outReply     = "direction-reply+ND"     // the design's out_src (§6.11)
	outReplyOnly = "direction-reply-noND"   // to show the ND rule is still needed
)

// outSrc installs the D20 output drop of packets sourced from an egress
// address, with the exemptions of mode.
func outSrc(t *testing.T, n *node, mode string, eg4, eg6 []string) {
	t.Helper()
	c, err := nftables.New(nftables.WithNetNSFd(int(n.h)))
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "purelb_gap_out"})
	s4, s6 := egressSets(t, c, tbl, eg4, eg6)
	ch := c.AddChain(&nftables.Chain{Name: "out_src", Table: tbl, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter})
	add := func(label string, e []expr.Any) {
		c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, UserData: []byte(label), Exprs: e})
	}
	// icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } ip6 hoplimit 255 accept
	if mode != outReplyOnly {
		add("nd", cat(mNDType(ndTypes(t, c, tbl)), mHop255(), counted(vAccept)))
	}
	if mode == outEstRel {
		add("ct", cat(mCtState(expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED), counted(vAccept)))
	} else {
		add("ct", cat(mCtReply(), counted(vAccept))) // ct direction reply accept
	}
	add("drop4", cat(mAddrIn(false, true, s4), counted(vDrop))) // ip saddr @egress4 drop
	add("drop6", cat(mAddrIn(true, true, s6), counted(vDrop)))  // ip6 saddr @egress6 drop
	require.NoError(t, c.Flush(), "%s: out_src", n.name)
}

// TestSpike6GapOutSrcReply checks the design's out_src exemption
// ("ct direction reply" and ND) against the form spike 6 verified
// ("ct state established,related" and ND): (a) neighbour discovery for the
// egress address stays REACHABLE, (b) replies to connections that came in
// to the address leave, (c) connections a local process originates from
// the address are dropped. (d) and (e) are where the forms can differ: a
// flow from the address that predates the rule, and a local process
// connecting to the address itself.
func TestSpike6GapOutSrcReply(t *testing.T) {
	requireVM(t)
	all := map[string]map[string]bool{}
	for _, mode := range []string{outEstRel, outReply, outReplyOnly} {
		t.Run(mode, func(t *testing.T) {
			for fam, r := range runOutSrc(t, mode) {
				all[mode+"/"+fam] = r
			}
		})
	}
	for _, f := range gapFams {
		a, b := all[outEstRel+"/"+f.name], all[outReply+"/"+f.name]
		if a == nil || b == nil {
			continue
		}
		var same, diff []string
		for _, k := range []string{"a-nud", "b-tcp", "b-udp", "b-icmp", "c-tcp-sent", "c-udp-sent", "c-icmp-sent", "d-predating-sent", "e-self-sent"} {
			if a[k] == b[k] {
				same = append(same, k)
			} else {
				diff = append(diff, fmt.Sprintf("%s(estrel=%v,reply=%v)", k, a[k], b[k]))
			}
		}
		t.Logf("SPIKE6GAP RESULT out_src compare %s: same on %v; differ on %v", f.name, same, diff)
	}
}

func runOutSrc(t *testing.T, mode string) map[string]map[string]bool {
	nd, srv := newNode(t, "node"), newNode(t, "server")
	veth(t, nd, "pool", srv, "eth0")
	addr(t, nd, "pool", "172.30.10.2/24")
	addr(t, nd, "pool", "fdaa::2/64")
	addr(t, nd, "pool", "172.30.10.110/32") // the egress address on the pool interface
	addrDeprecated(t, nd, "pool", "fdaa::110/128")
	addr(t, srv, "eth0", "172.30.10.1/24")
	addr(t, srv, "eth0", "fdaa::1/64")
	fastNUD(t, srv, "eth0")
	fastNUD(t, nd, "pool")
	ctOn(t, nd) // conntrack runs on a real node before PureLB's rules exist

	lines := make(chan string, 256)
	pre := map[string]net.Conn{}
	for _, f := range gapFams {
		listenTCP(t, nd, jh(f.egress, 9000), func(c net.Conn) {
			fmt.Fprintf(c, "hello\n")
			_, _ = io.Copy(c, c)
		})
		pong := listenUDP(t, nd, jh(f.egress, 9001))
		go func() {
			buf := make([]byte, 2048)
			for {
				_, ra, err := pong.ReadFromUDP(buf)
				if err != nil {
					return
				}
				_, _ = pong.WriteToUDP([]byte("pong"), ra)
			}
		}()
		listenTCP(t, srv, jh(f.peer, 9100), func(c net.Conn) {
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				lines <- f.name + " " + sc.Text()
			}
		})
		sink := listenUDP(t, srv, jh(f.peer, 9101))
		go func() {
			buf := make([]byte, 2048)
			for {
				nr, _, err := sink.ReadFromUDP(buf)
				if err != nil {
					return
				}
				lines <- "udp " + f.name + " " + string(buf[:nr])
			}
		}()
		// (d) an outbound flow from the egress address, opened before the rule.
		c, err := dialTCP(nd, f.egress, jh(f.peer, 9100), 3*time.Second)
		require.NoError(t, err, "%s: pre-rule flow from the egress address", f.name)
		t.Cleanup(func() { c.Close() })
		fmt.Fprintf(c, "before\n")
		require.True(t, expectLine(lines, f.name+" before", 3*time.Second), "%s: pre-rule flow", f.name)
		pre[f.name] = c
	}

	outSrc(t, nd, mode, []string{"172.30.10.110"}, []string{"fdaa::110"})

	out := map[string]map[string]bool{}
	for _, f := range gapFams {
		r := map[string]bool{}
		ev := map[string]string{}
		// (b) replies to connections that came in to the egress address.
		if c, err := dialTCP(srv, "", jh(f.egress, 9000), 3*time.Second); err == nil {
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			br := bufio.NewReader(c)
			l1, e1 := br.ReadString('\n')
			fmt.Fprintf(c, "x\n")
			l2, e2 := br.ReadString('\n')
			r["b-tcp"] = e1 == nil && e2 == nil && l1 == "hello\n" && l2 == "x\n"
			ev["b-tcp"] = fmt.Sprintf("%v/%v", e1, e2)
			c.Close()
		} else {
			ev["b-tcp"] = err.Error()
		}
		p, err := udpAsk(srv, "", jh(f.egress, 9001), "q", 2*time.Second)
		r["b-udp"] = err == nil && p == "pong"
		ev["b-udp"] = fmt.Sprint(err)
		_, err = echo(srv, "", f.egress, 0x4242, 1, 2*time.Second)
		r["b-icmp"] = err == nil
		ev["b-icmp"] = fmt.Sprint(err)

		// (a) the peer's neighbour entry for the address: force STALE, send a
		// datagram (no upper-layer confirmation), and watch NUD probe it.
		st, _ := neigh(t, srv, "eth0", f.egress)
		var seq []string
		final, sent, ucast, answered := 0, 0, 0, 0
		probes := ndProbes(t, srv, "eth0", f.egress)
		if err := forceStale(t, srv, "eth0", f.egress); err == nil {
			_, _ = udpAsk(srv, "", jh(f.egress, 9001), "nud", 500*time.Millisecond)
			seq, final = watchNeigh(t, srv, "eth0", f.egress, 3*time.Second)
		} else {
			seq = []string{err.Error()}
		}
		sent, ucast, answered = probes()
		r["a-nud"] = final == netlink.NUD_REACHABLE && ucast > 0 && answered > 0
		ev["a-nud"] = fmt.Sprintf("before=%s after-stale=%s probes=%d unicast=%d answered=%d", nudName(st), strings.Join(seq, ">"), sent, ucast, answered)

		// (c) connections a local process originates from the address.
		c, err := dialTCP(nd, f.egress, jh(f.peer, 9100), 2*time.Second)
		r["c-tcp-sent"] = err == nil
		ev["c-tcp-sent"] = fmt.Sprint(err)
		if c != nil {
			c.Close()
		}
		_, err = udpAsk(nd, f.egress, jh(f.peer, 9101), "out", 100*time.Millisecond)
		r["c-udp-sent"] = expectLine(lines, "udp "+f.name+" out", time.Second)
		ev["c-udp-sent"] = fmt.Sprint(err)
		_, err = echo(nd, f.egress, f.peer, 0x4343, 1, time.Second)
		r["c-icmp-sent"] = err == nil
		ev["c-icmp-sent"] = fmt.Sprint(err)

		// (d) the pre-rule flow sends again, in its original direction.
		_, err = fmt.Fprintf(pre[f.name], "after\n")
		r["d-predating-sent"] = expectLine(lines, f.name+" after", 2*time.Second)
		ev["d-predating-sent"] = fmt.Sprint(err)

		// (e) a local process connecting to the egress address itself: the
		// kernel picks the address as the source.
		c, err = dialTCP(nd, "", jh(f.egress, 9000), 2*time.Second)
		r["e-self-sent"] = err == nil
		ev["e-self-sent"] = fmt.Sprint(err)
		if c != nil {
			c.Close()
		}

		var b strings.Builder
		for _, k := range []string{"a-nud", "b-tcp", "b-udp", "b-icmp", "c-tcp-sent", "c-udp-sent", "c-icmp-sent", "d-predating-sent", "e-self-sent"} {
			fmt.Fprintf(&b, " %s=%v", k, r[k])
			if !r[k] {
				fmt.Fprintf(&b, "(%s)", ev[k])
			} else if k == "a-nud" {
				fmt.Fprintf(&b, "(%s)", ev[k])
			}
		}
		t.Logf("SPIKE6GAP RESULT out_src mode=%s %s:%s", mode, f.name, b.String())
		out[f.name] = r
	}
	_, ctrs := nftCounters(t, nd, "purelb_gap_out", "out_src")
	t.Logf("SPIKE6GAP RESULT out_src mode=%s counters:%s", mode, ctrs)

	for _, f := range gapFams {
		r := out[f.name]
		if mode == outReplyOnly && f.name == "IPv6" {
			require.False(t, r["a-nud"], "IPv6 without the ND exemption: NUD was expected to fail (NA from the egress address dropped)")
			continue
		}
		for _, k := range []string{"a-nud", "b-tcp", "b-udp", "b-icmp"} {
			require.True(t, r[k], "%s %s: %s must pass", mode, f.name, k)
		}
		for _, k := range []string{"c-tcp-sent", "c-udp-sent", "c-icmp-sent"} {
			require.False(t, r[k], "%s %s: %s must be dropped", mode, f.name, k)
		}
	}
	return out
}

// ---- Spike 6 gap 2: the ND exemption on announce_in ------------------------

// announceIn installs the D32 announce_in chain (§6.11), without the
// Service-port accepts (no Service here). Three leading rules only count:
// ND to an egress address, and its conntrack state.
func announceIn(t *testing.T, n *node, ndExempt bool, eg4, eg6 []string) {
	t.Helper()
	c, err := nftables.New(nftables.WithNetNSFd(int(n.h)))
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "purelb_gap_ann"})
	s4, s6 := egressSets(t, c, tbl, eg4, eg6)
	ch := c.AddChain(&nftables.Chain{Name: "announce_in", Table: tbl, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-150)})
	add := func(label string, e []expr.Any) {
		c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, UserData: []byte(label), Exprs: e})
	}
	ndTo := func() []expr.Any { return cat(mAddrIn(true, false, s6), mNDType(ndTypes(t, c, tbl))) }
	add("nd-to-egress", cat(ndTo(), counted(nil)))
	add("nd-to-egress-ct-untracked", cat(ndTo(), mCtState(expr.CtStateBitUNTRACKED), counted(nil)))
	add("nd-to-egress-ct-invalid", cat(ndTo(), mCtState(expr.CtStateBitINVALID), counted(nil)))
	if ndExempt {
		add("nd-accept", cat(mNDType(ndTypes(t, c, tbl)), mHop255(), counted(vAccept)))
	}
	newInv := mCtState(expr.CtStateBitNEW | expr.CtStateBitINVALID)
	add("drop4", cat(newInv, mAddrIn(false, false, s4), counted(vDrop)))
	add("drop6", cat(newInv, mAddrIn(true, false, s6), counted(vDrop)))
	add("drop-eg0", cat(newInv, mIifname("purelb-eg0"), counted(vDrop)))
	require.NoError(t, c.Flush(), "%s: announce_in", n.name)
}

// TestSpike6GapAnnounceInND checks that the announce_in drop of new and
// invalid packets to an egress address leaves neighbour discovery for the
// address working on the announcer, with and without the chain's ND
// exemption: (A) the gateway's unicast NUD probe of the address is
// answered, and (B) the announcer resolves the gateway with a solicitation
// sourced from the address (as for transit redirected out of the pool
// interface, and replies), so the gateway's advertisement comes back to
// the egress address.
func TestSpike6GapAnnounceInND(t *testing.T) {
	requireVM(t)
	for _, exempt := range []bool{false, true} {
		t.Run(fmt.Sprintf("nd-exempt=%v", exempt), func(t *testing.T) {
			ann, gw := newNode(t, "announcer"), newNode(t, "gateway")
			veth(t, ann, "pool", gw, "eth0")
			addr(t, ann, "pool", "172.30.10.2/24")
			addr(t, ann, "pool", "fdaa::2/64")
			addr(t, ann, "pool", "172.30.10.110/32")
			addrDeprecated(t, ann, "pool", "fdaa::110/128")
			addr(t, gw, "eth0", "172.30.10.1/24")
			addr(t, gw, "eth0", "fdaa::1/64")
			fastNUD(t, ann, "pool")
			fastNUD(t, gw, "eth0")
			announceIn(t, ann, exempt, []string{"172.30.10.110"}, []string{"fdaa::110"})

			recv := make(chan string, 64)
			for _, f := range gapFams {
				u := listenUDP(t, gw, jh(f.peer, 9200))
				go func() {
					buf := make([]byte, 2048)
					for {
						nr, _, err := u.ReadFromUDP(buf)
						if err != nil {
							return
						}
						recv <- f.name + " " + string(buf[:nr])
					}
				}()
			}
			ok := map[string][2]bool{}
			for _, f := range gapFams {
				// (B) the announcer sends from the egress address to a gateway
				// it has not resolved yet.
				st0, _ := neigh(t, ann, "pool", f.peer)
				probesB := ndProbes(t, ann, "pool", f.peer)
				_, err := udpAsk(ann, f.egress, jh(f.peer, 9200), "from-egress", 50*time.Millisecond)
				gotB := expectLine(recv, f.name+" from-egress", 3*time.Second)
				stB, _ := neigh(t, ann, "pool", f.peer)
				bSent, _, bAns := probesB()

				// (A) the gateway sends to the egress address (a new flow, which
				// announce_in drops), then NUD re-probes the address.
				_, _ = udpAsk(gw, "", jh(f.egress, 9), "x", 300*time.Millisecond)
				st1, _ := neigh(t, gw, "eth0", f.egress)
				var seq []string
				final, aSent, aUcast, aAns := 0, 0, 0, 0
				probesA := ndProbes(t, gw, "eth0", f.egress)
				if err := forceStale(t, gw, "eth0", f.egress); err == nil {
					_, _ = udpAsk(gw, "", jh(f.egress, 9), "x", 50*time.Millisecond)
					seq, final = watchNeigh(t, gw, "eth0", f.egress, 3*time.Second)
				} else {
					seq = []string{err.Error()}
				}
				aSent, aUcast, aAns = probesA()
				gotA := final == netlink.NUD_REACHABLE && aUcast > 0 && aAns > 0
				ok[f.name] = [2]bool{gotA, gotB}
				t.Logf("SPIKE6GAP RESULT announce_in nd-exempt=%v %s: (A) gateway NUD of the egress address %s (entry %s; after STALE: %s; probes sent=%d unicast=%d answered=%d); (B) announcer resolves the gateway from the egress address: datagram %s (entry before %s, after %s; probes sent=%d answered=%d; send err %v)",
					exempt, f.name, yes(gotA, "REACHABLE", "BROKEN"), nudName(st1), strings.Join(seq, ">"), aSent, aUcast, aAns,
					yes(gotB, "delivered", "NOT delivered"), nudName(st0), nudName(stB), bSent, bAns, err)
			}
			m, ctrs := nftCounters(t, ann, "purelb_gap_ann", "announce_in")
			t.Logf("SPIKE6GAP RESULT announce_in nd-exempt=%v counters:%s", exempt, ctrs)
			require.NotZero(t, m["drop4"], "the chain must be in force: the gateway's new IPv4 flow to the egress address was not dropped")
			require.NotZero(t, m["drop6"], "the chain must be in force: the gateway's new IPv6 flow to the egress address was not dropped")
			require.NotZero(t, m["nd-to-egress"], "no NS or NA to the egress address crossed the chain: the test exercised nothing")
			require.Equal(t, [2]bool{true, true}, ok["IPv4"], "IPv4: ARP does not cross the inet hooks")
			if exempt {
				require.Equal(t, [2]bool{true, true}, ok["IPv6"], "IPv6 with the ND exemption")
			}
		})
	}
}

// ---- Spike 7 gaps 3 and 4: ICMP echo and NAT66 through the slice SNAT ------

// natTopo is spike 7's topology, dual stack: the router is a pod node that
// holds the egress addresses on its uplink. The main table blackholes the
// server, so every packet (new or established, either family) must carry
// the slot to reach it.
func natTopo(t *testing.T) (cli, rtr, srv *node) {
	cli, rtr, srv = newNode(t, "client"), newNode(t, "router"), newNode(t, "server")
	veth(t, cli, "eth0", rtr, "vpod")
	veth(t, rtr, "up0", srv, "eth0")
	addr(t, cli, "eth0", "10.244.1.5/24")
	addr(t, cli, "eth0", "fd44:1::5/64")
	addr(t, rtr, "vpod", "10.244.1.1/24")
	addr(t, rtr, "vpod", "fd44:1::1/64")
	addr(t, rtr, "up0", "172.30.10.2/24")
	addr(t, rtr, "up0", "fdaa::2/64")
	addr(t, rtr, "up0", "172.30.10.110/32")
	addr(t, rtr, "up0", "fdaa::110/128")
	addr(t, srv, "eth0", "172.30.10.1/24")
	addr(t, srv, "eth0", "fdaa::1/64")
	defaultRoute(t, cli, "10.244.1.1")
	defaultRoute(t, cli, "fd44:1::1")
	rtr.sysctl(t, "net.ipv4.ip_forward", "1")
	rtr.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	rtr.sysctl(t, "net.ipv4.conf.all.rp_filter", "1")
	rtr.sysctl(t, "net.ipv4.conf.up0.src_valid_mark", "1")
	for _, s := range []string{"172.30.10.1/32", "fdaa::1/128"} {
		_, d, _ := net.ParseCIDR(s)
		route(t, rtr, &netlink.Route{Dst: d, Type: unix.RTN_BLACKHOLE})
		route(t, rtr, &netlink.Route{Dst: d, LinkIndex: ifindex(t, rtr, "up0"), Table: 100})
	}
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		r := netlink.NewRule()
		r.Family, r.Priority, r.Table = fam, 50, 100
		r.Mark, r.Mask = 3, ptr(uint32(0xff))
		require.NoError(t, rtr.nl.RuleAdd(r))
	}
	newFlowMark(t, rtr, "vpod", 3)
	nftEgress(t, rtr, []snatSlot{
		{slot: 3, addr: "172.30.10.110", portLo: sliceLo, portHi: sliceHi},
		{slot: 3, addr: "fdaa::110", portLo: sliceLo, portHi: sliceHi},
	})
	return cli, rtr, srv
}

func inSlice(p uint16) bool { return p >= sliceLo && p <= sliceHi }

// TestSpike7GapICMPEcho pings through the per-slot constant SNAT
// ("snat to <addr>:20480-24575") and reports the echo identifier the server
// sees. 0x1234 is outside the slice; 0x5100 (20736) is inside it.
func TestSpike7GapICMPEcho(t *testing.T) {
	requireVM(t)
	cli, _, srv := natTopo(t)
	for _, f := range gapFams {
		s0 := snmp(t, srv)
		stop := sniffEcho(t, srv, "eth0")
		type res struct {
			id, seq, replyID uint16
			err              error
		}
		var rs []res
		for _, c := range []struct{ id, seq uint16 }{{0x1234, 1}, {0x1234, 2}, {0x5100, 3}} {
			rid, err := echo(cli, "", f.peer, c.id, c.seq, 2*time.Second)
			rs = append(rs, res{c.id, c.seq, rid, err})
		}
		seen := stop()
		t.Logf("SPIKE7GAP RESULT icmp %s: echo requests on the server's wire %v; server:%s", f.name, seen,
			deltas(s0, snmp(t, srv), "IcmpInEchos", "IcmpOutEchoReps", "IcmpInCsumErrors", "Icmp6InEchos", "Icmp6OutEchoReplies", "Icmp6InCsumErrors"))
		for _, r := range rs {
			i := slices.IndexFunc(seen, func(s echoSeen) bool { return s.seq == r.seq })
			var s echoSeen
			if i >= 0 {
				s = seen[i]
			}
			t.Logf("SPIKE7GAP RESULT icmp %s: pod echo id=%d seq=%d -> server saw src=%v id=%d in-slice[%d-%d]=%v; reply reached the pod=%v with id=%d",
				f.name, r.id, r.seq, s.src, s.id, sliceLo, sliceHi, i >= 0 && inSlice(s.id), r.err == nil, r.replyID)
			require.NoError(t, r.err, "%s seq %d: the echo reply must reach the pod", f.name, r.seq)
			require.Equal(t, r.id, r.replyID, "%s seq %d: the pod must see its own identifier", f.name, r.seq)
			require.GreaterOrEqual(t, i, 0, "%s seq %d: the server saw no request", f.name, r.seq)
			require.Equal(t, f.egress, s.src.String(), "%s seq %d: server must see the egress address", f.name, r.seq)
			require.True(t, inSlice(s.id), "%s seq %d: echo id %d must be inside the slice", f.name, r.seq, s.id)
			require.True(t, s.csumOK, "%s seq %d: the translated echo request must carry a valid checksum", f.name, r.seq)
		}
	}
}

// TestSpike7GapNAT66 checks the per-slot constant SNAT for both families
// over TCP and UDP on the direct path (the spike 1 tunnel test covers TCP
// NAT66 through the tunnel): the server must see the egress address and a
// port inside the slice, and replies must return. Several datagrams and a
// stream on one flow also exercise the ct-mark restore, since the main
// table blackholes the server.
func TestSpike7GapNAT66(t *testing.T) {
	requireVM(t)
	cli, _, srv := natTopo(t)
	t.Logf("SPIKE7GAP RESULT nat66: table with ip and ip6 per-slot 'snat to addr:%d-%d' loaded", sliceLo, sliceHi)
	for _, f := range gapFams {
		// UDP: the server echoes the source it sees.
		u := listenUDP(t, srv, jh(f.peer, 9400))
		go func() {
			buf := make([]byte, 2048)
			for {
				_, ra, err := u.ReadFromUDPAddrPort(buf)
				if err != nil {
					return
				}
				_, _ = u.WriteToUDPAddrPort([]byte(ra.String()), ra)
			}
		}()
		var c *net.UDPConn
		require.NoError(t, cli.in(func() error {
			var err error
			c, err = net.ListenUDP("udp", &net.UDPAddr{})
			return err
		}))
		ra := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(jh(f.peer, 9400)))
		var udpSeen []netip.AddrPort
		for i := 0; i < 3; i++ {
			_, err := c.WriteToUDP([]byte("q"), ra)
			require.NoError(t, err)
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 128)
			nr, _, err := c.ReadFromUDP(buf)
			require.NoError(t, err, "%s UDP datagram %d: no reply (de-NAT or ct-mark restore failed)", f.name, i+1)
			udpSeen = append(udpSeen, netip.MustParseAddrPort(string(buf[:nr])))
		}
		c.Close()

		// TCP: the server reports the source, then counts a 4 MiB stream.
		got := make(chan int64, 1)
		listenTCP(t, srv, jh(f.peer, 9401), func(c net.Conn) {
			fmt.Fprintf(c, "%s\n", c.RemoteAddr())
			n, _ := io.Copy(io.Discard, c)
			got <- n
		})
		tc, err := dialTCP(cli, "", jh(f.peer, 9401), 3*time.Second)
		require.NoError(t, err, "%s TCP connect", f.name)
		_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
		line, err := bufio.NewReader(tc).ReadString('\n')
		require.NoError(t, err)
		const total = 4 << 20
		buf := make([]byte, 64<<10)
		for sent := 0; sent < total; {
			n, err := tc.Write(buf)
			require.NoError(t, err, "%s TCP stream", f.name)
			sent += n
		}
		tc.Close()
		tcpSeen := netip.MustParseAddrPort(strings.TrimSpace(line))
		streamed := <-got

		t.Logf("SPIKE7GAP RESULT nat66 %s: UDP server saw %v (3 datagrams, replies returned); TCP server saw %v, %d of %d bytes received",
			f.name, udpSeen, tcpSeen, streamed, total)
		for _, s := range udpSeen {
			require.Equal(t, f.egress, s.Addr().String(), "%s UDP source", f.name)
			require.True(t, inSlice(s.Port()), "%s UDP port %d inside the slice", f.name, s.Port())
			require.Equal(t, udpSeen[0], s, "%s UDP: one flow keeps one mapping", f.name)
		}
		require.Equal(t, f.egress, tcpSeen.Addr().String(), "%s TCP source", f.name)
		require.True(t, inSlice(tcpSeen.Port()), "%s TCP port %d inside the slice", f.name, tcpSeen.Port())
		require.Equal(t, int64(total), streamed, "%s TCP stream", f.name)
	}
}

// ---- Spike 1 gap 5: fragments through the tunnel ---------------------------

// fragNet is spike 1's L3 tunnel (with the Ethernet push) with up to two
// announcers. The announcers hold the egress addresses on a dummy
// interface (routed-pool placement) and reach the server over a bridge in
// the server's namespace, one bridge port per announcer.
type fragNet struct {
	pod, a, srv    *node
	anns           []*node
	objA           *coreObjects
	objAnn         []*coreObjects
	egress, server string
	inner6         bool
}

func geneveL3(t *testing.T, n *node, mtu int) {
	t.Helper()
	require.NoError(t, n.nl.LinkAdd(&netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: "purelb-eg0", MTU: mtu},
		FlowBased: true, Dport: tunPort, InnerProtoInherit: true}), "%s: purelb-eg0", n.name)
	up(t, n, "purelb-eg0")
}

func peerOf(s string) corePeer {
	return corePeer{Addr: ip16(s), V6: b2u8(netip.MustParseAddr(s).Is6())}
}

func newFragNet(t *testing.T, inner6, under6 bool, nAnn, tunMTU, routeMTU int, routeLock bool) *fragNet {
	f := &fragNet{pod: newNode(t, "pod"), a: newNode(t, "nodeA"), srv: newNode(t, "server"), inner6: inner6}
	pod, a, srv := f.pod, f.a, f.srv
	f.egress, f.server = "172.30.10.110", "172.30.10.1"
	if inner6 {
		f.egress, f.server = "fdaa::110", "fdaa::1"
	}

	veth(t, pod, "eth0", a, "vpod")
	addr(t, pod, "eth0", "10.244.1.5/24")
	addr(t, pod, "eth0", "fd44:1::5/64")
	addr(t, a, "vpod", "10.244.1.1/24")
	addr(t, a, "vpod", "fd44:1::1/64")
	defaultRoute(t, pod, "10.244.1.1")
	defaultRoute(t, pod, "fd44:1::1")

	require.NoError(t, srv.nl.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, MulticastSnooping: ptr(false)}))
	up(t, srv, "br0")
	addr(t, srv, "br0", "172.30.10.1/24")
	addr(t, srv, "br0", "fdaa::1/64")
	_ = srv.in(func() error { // br_netfilter is loaded in the VM: keep the bridge out of netfilter
		for _, k := range []string{"iptables", "ip6tables"} {
			_ = os.WriteFile("/proc/sys/net/bridge/bridge-nf-call-"+k, []byte("0"), 0o644)
		}
		return nil
	})
	br, err := srv.nl.LinkByName("br0")
	require.NoError(t, err)

	a.sysctl(t, "net.ipv4.ip_forward", "1")
	a.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
	geneveL3(t, a, tunMTU)

	var peers []corePeer
	for i := 0; i < nAnn; i++ {
		x := newNode(t, fmt.Sprintf("ann%d", i))
		ul := fmt.Sprintf("ul%d", i)
		veth(t, a, ul, x, "ul")
		aU4, aU6 := fmt.Sprintf("10.0.%d.1", i), fmt.Sprintf("fd00:%d::1", i)
		xU4, xU6 := fmt.Sprintf("10.0.%d.2", i), fmt.Sprintf("fd00:%d::2", i)
		addr(t, a, ul, aU4+"/24")
		addr(t, a, ul, aU6+"/64")
		addr(t, x, "ul", xU4+"/24")
		addr(t, x, "ul", xU6+"/64")
		port := fmt.Sprintf("p%d", i)
		veth(t, x, "pool", srv, port)
		pl, err := srv.nl.LinkByName(port)
		require.NoError(t, err)
		require.NoError(t, srv.nl.LinkSetMaster(pl, br))
		addr(t, x, "pool", fmt.Sprintf("172.30.10.%d/24", 2+i))
		addr(t, x, "pool", fmt.Sprintf("fdaa::%d/64", 2+i))
		require.NoError(t, x.nl.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eg"}}))
		up(t, x, "eg")
		addr(t, x, "eg", "172.30.10.110/32")
		addr(t, x, "eg", "fdaa::110/128")
		x.sysctl(t, "net.ipv4.ip_forward", "1")
		x.sysctl(t, "net.ipv6.conf.all.forwarding", "1")
		geneveL3(t, x, tunMTU)

		o := newCore(t)
		// TunMtu: pl_announce answers a reply that would not fit the tunnel
		// over an IPv6 underlay with Packet Too Big / frag-needed (1b).
		require.NoError(t, o.Cfg.Put(uint32(0), coreConfig{SelfNode: uint32(2 + i),
			TunIfindex: uint32(ifindex(t, x, "purelb-eg0")), PoolIfindex: uint32(ifindex(t, x, "pool")), PushEth: 1,
			TunMtu: uint32(tunMTU)}))
		aU, xU := aU4, xU4
		if under6 {
			aU, xU = aU6, xU6
		}
		for _, e := range []string{"172.30.10.110", "fdaa::110"} {
			k := coreAddrKey{Addr: ip16(e), Family: family(e)}
			require.NoError(t, o.EgressAddrs.Put(k, coreAddrCfg{Base: base, Shift: shift, Announced: 1}))
			require.NoError(t, o.Slices.Put(coreSliceKey{Addr: ip16(e), Family: family(e), Index: sliceLo >> shift}, uint32(nodeAID)))
		}
		require.NoError(t, o.Nodes.Put(uint32(nodeAID), peerOf(aU)))
		require.NoError(t, o.NodeByAddr.Put(peerOf(aU), uint32(nodeAID)))
		attach(t, x, "purelb-eg0", o.PlTunIn, ebpf.AttachTCXIngress)
		peers = append(peers, peerOf(xU))
		f.anns, f.objAnn = append(f.anns, x), append(f.objAnn, o)
	}

	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		r := netlink.NewRule()
		r.Family, r.Priority, r.Table = fam, 50, 100
		r.Mark, r.Mask = slot, ptr(uint32(0xff))
		require.NoError(t, a.nl.RuleAdd(r))
		dst, throw := "0.0.0.0/0", "10.0.0.0/16"
		if fam == netlink.FAMILY_V6 {
			dst, throw = "::/0", "fd00::/16"
		}
		_, d, _ := net.ParseCIDR(dst)
		_, th, _ := net.ParseCIDR(throw)
		route(t, a, &netlink.Route{Dst: d, LinkIndex: ifindex(t, a, "purelb-eg0"), Table: 100, MTU: routeMTU, MTULock: routeLock})
		route(t, a, &netlink.Route{Dst: th, Type: unix_RTN_THROW, Table: 100})
	}
	nftEgress(t, a, []snatSlot{
		{slot: slot, addr: "172.30.10.110", portLo: sliceLo, portHi: sliceHi},
		{slot: slot, addr: "fdaa::110", portLo: sliceLo, portHi: sliceHi},
	})
	f.objA = newCore(t)
	require.NoError(t, f.objA.Cfg.Put(uint32(0), coreConfig{SelfNode: nodeAID, TunIfindex: uint32(ifindex(t, a, "purelb-eg0"))}))
	var st coreSteer
	st.N = uint32(nAnn)
	copy(st.Peers[:], peers)
	require.NoError(t, f.objA.SteerSlots.Put(uint32(slot), st))
	for _, e := range []string{"172.30.10.110", "fdaa::110"} {
		k := coreAddrKey{Addr: ip16(e), Family: family(e)}
		require.NoError(t, f.objA.EgressAddrs.Put(k, coreAddrCfg{Base: base, Shift: shift}))
		require.NoError(t, f.objA.Slices.Put(coreSliceKey{Addr: ip16(e), Family: family(e), Index: sliceLo >> shift}, uint32(nodeAID)))
	}
	require.NoError(t, f.objA.PodDevs.Put(uint32(ifindex(t, a, "vpod")), corePodDev{Slot: slot,
		V4: ip16("10.244.1.5")[0], V6: ip16("fd44:1::5")}))
	attach(t, a, "vpod", f.objA.PlPodTag, ebpf.AttachTCXIngress)
	attach(t, a, "purelb-eg0", f.objA.PlSteer, ebpf.AttachTCXEgress)

	// The server's addresses on br0 stay tentative until the bridge has
	// carrier (IPv6 does not answer NS for a tentative address, even one
	// added NODAD). Wait for that before any test traffic.
	start := time.Now()
	for {
		l, err := srv.nl.LinkByName("br0")
		require.NoError(t, err)
		as, err := srv.nl.AddrList(l, netlink.FAMILY_ALL)
		require.NoError(t, err)
		tentative := 0
		for _, ad := range as {
			if ad.Flags&unix.IFA_F_TENTATIVE != 0 {
				tentative++
			}
		}
		if tentative == 0 && l.Attrs().OperState == netlink.OperUp {
			break
		}
		require.Less(t, time.Since(start), 5*time.Second, "server br0: %d tentative addresses, oper %s", tentative, l.Attrs().OperState)
		time.Sleep(20 * time.Millisecond)
	}
	// Steady state: each announcer has resolved the server, as a holder
	// has resolved its gateway, so the measurement is not a first-packet
	// neighbour-resolution transient.
	for i, x := range f.anns {
		for _, pair := range [][2]string{{fmt.Sprintf("172.30.10.%d", 2+i), "172.30.10.1"}, {fmt.Sprintf("fdaa::%d", 2+i), "fdaa::1"}} {
			var err error
			for try := 0; try < 10; try++ {
				if _, err = echo(x, pair[0], pair[1], 0x7777, uint16(try), 500*time.Millisecond); err == nil {
					break
				}
			}
			require.NoError(t, err, "%s: ping %s from %s", x.name, pair[1], pair[0])
		}
	}
	t.Logf("server br0 ready, announcers resolved the server, after %s", time.Since(start).Round(time.Millisecond))
	return f
}

type udpGot struct {
	seq uint32
	n   int
}

// collectUDP counts the datagrams (reassembled, if fragmented) the server
// receives on port.
func (f *fragNet) collectUDP(t *testing.T, port int) <-chan udpGot {
	ch := make(chan udpGot, 4096)
	u := listenUDP(t, f.srv, jh(f.server, port))
	go func() {
		buf := make([]byte, 65536)
		for {
			nr, _, err := u.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if nr >= 4 {
				ch <- udpGot{binary.BigEndian.Uint32(buf), nr}
			}
		}
	}()
	return ch
}

func drainUDP(ch <-chan udpGot, quiet time.Duration) map[uint32]int {
	m := map[uint32]int{}
	for {
		select {
		case g := <-ch:
			m[g.seq] = g.n
		case <-time.After(quiet):
			return m
		}
	}
}

// podSocket opens an unconnected UDP socket in the pod; df sets
// IP(V6)_PMTUDISC_DO, so the kernel neither fragments locally nor sends
// a datagram larger than the path MTU it knows.
func (f *fragNet) podSocket(t *testing.T, df bool) *net.UDPConn {
	t.Helper()
	ip := "10.244.1.5"
	if f.inner6 {
		ip = "fd44:1::5"
	}
	var c *net.UDPConn
	require.NoError(t, f.pod.in(func() error {
		var err error
		c, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(ip)})
		return err
	}))
	t.Cleanup(func() { c.Close() })
	if df {
		rc, err := c.SyscallConn()
		require.NoError(t, err)
		var serr error
		require.NoError(t, rc.Control(func(fd uintptr) {
			if f.inner6 {
				serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_DO)
			} else {
				serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DO)
			}
		}))
		require.NoError(t, serr)
	}
	return c
}

func (f *fragNet) send(c *net.UDPConn, port int, seq uint32, size int) error {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, seq)
	_, err := c.WriteToUDP(b, &net.UDPAddr{IP: net.ParseIP(f.server), Port: port})
	return err
}

// routeMTU returns the MTU of n's route to dst (with mark, as ip route get
// mark ...), which shows a learned PMTU exception; 0 when none.
func routeMTU(n *node, dst string, mark uint32) string {
	rs, err := n.nl.RouteGetWithOptions(net.ParseIP(dst), &netlink.RouteGetOptions{Mark: mark})
	if err != nil {
		return err.Error()
	}
	if len(rs) == 0 {
		return "none"
	}
	return fmt.Sprint(rs[0].MTU)
}

func errStr(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

var (
	podICMP  = []string{"IcmpInDestUnreachs", "Icmp6InPktTooBigs"}
	nodeICMP = []string{"IcmpOutDestUnreachs", "Icmp6OutPktTooBigs", "IcmpInDestUnreachs", "Icmp6InPktTooBigs",
		"IpFragCreates", "IpFragOKs", "IpFragFails", "Ip6FragCreates", "Ip6FragOKs", "Ip6FragFails", "IpOutDiscards", "Ip6OutDiscards",
		"IpReasmReqds", "IpReasmOKs", "Ip6ReasmReqds", "Ip6ReasmOKs"}
	reasm = []string{"IpReasmReqds", "IpReasmOKs", "IpReasmFails", "Ip6ReasmReqds", "Ip6ReasmOKs", "Ip6ReasmFails",
		"IpInAddrErrors", "Ip6InAddrErrors", "IpInDiscards", "Ip6InDiscards", "IpExtInNoRoutes", "Ip6InNoRoutes"}
)

// TestSpike1GapFragments sends UDP datagrams larger than the path MTU from
// the pod through the tunnel, inner and underlay IPv4 and IPv6:
//
//	steer: two announcers, tunnel MTU 1400. Per datagram, which announcers
//	       received its fragments (captured on each announcer's
//	       purelb-eg0); every datagram must reach one announcer, and the
//	       server must receive and reassemble every one (pl_tun_in forwards
//	       later fragments through the frags map).
//	reply: one announcer. The server sends datagrams larger than the path
//	       MTU back to the pod's flow, fragmented by the server. pl_announce
//	       must tunnel every fragment to the slice owner (frags map) and the
//	       pod must reassemble them. fit: the server's route MTU is 1400, so
//	       the fragments fit the tunnel. oversize: no route MTU, so
//	       1500-byte fragments reach the announcer. Over an IPv4 underlay
//	       they enter the tunnel and the outer packet must be fragmented
//	       (Geneve sends IPv4 outer packets without DF, design 6.7), with no
//	       Packet Too Big. Over an IPv6 underlay pl_announce answers each
//	       one with ICMP frag-needed (IPv4 server) or ICMPv6 Packet Too Big
//	       (IPv6 server) carrying the tunnel MTU (1400); the server must
//	       learn that path MTU, and a second batch must reach the pod.
//	pmtu:  one announcer, tunnel MTU 1500 on a 1500 underlay, so a
//	       1500-byte inner packet does not fit once encapsulated; without a
//	       route MTU, with the design's route mtu (underlay - overhead), and
//	       with that MTU locked. What reaches the server and what PMTU
//	       signal reaches the pod.
func TestSpike1GapFragments(t *testing.T) {
	requireVM(t)
	for _, inner6 := range []bool{false, true} {
		for _, under6 := range []bool{false, true} {
			name := fmt.Sprintf("inner%s/underlay%s", yes(inner6, "6", "4"), yes(under6, "6", "4"))
			t.Run("steer/"+name, func(t *testing.T) { runFragSteer(t, name, inner6, under6) })
			for _, v := range []string{"fit", "oversize"} {
				t.Run("reply/"+name+"/"+v, func(t *testing.T) { runFragReply(t, name, inner6, under6, v) })
			}
			for _, v := range []string{"no-route-mtu", "route-mtu", "route-mtu-lock"} {
				t.Run("pmtu/"+name+"/"+v, func(t *testing.T) { runFragPMTU(t, name, inner6, under6, v) })
			}
		}
	}
}

func runFragSteer(t *testing.T, name string, inner6, under6 bool) {
	const port, flows, large, size = 9300, 4, 3, 3000
	f := newFragNet(t, inner6, under6, 2, 1400, 0, false)
	got := f.collectUDP(t, port)
	stops := []func() []capPkt{capture(t, f.anns[0], "purelb-eg0"), capture(t, f.anns[1], "purelb-eg0")}
	srv0, ann0, ann1 := snmp(t, f.srv), snmp(t, f.anns[0]), snmp(t, f.anns[1])

	socks := make([]*net.UDPConn, flows)
	for i := range socks {
		socks[i] = f.podSocket(t, false)
	}
	// Prime: an IPv6 pod learns the tunnel MTU from nodeA's Packet Too Big
	// (its 1500-byte fragments exceed the 1400 tunnel after reassembly).
	p0 := snmp(t, f.pod)
	primeErr := f.send(socks[0], port, 1, size)
	time.Sleep(300 * time.Millisecond)
	p1 := snmp(t, f.pod)
	pmtu := routeMTU(f.pod, f.server, 0)

	sent := 1
	for i, c := range socks {
		require.NoError(t, f.send(c, port, uint32(100+i), 64))
		for j := 0; j < large; j++ {
			require.NoError(t, f.send(c, port, uint32(1000+10*i+j), size))
			sent++
			time.Sleep(20 * time.Millisecond)
		}
	}
	time.Sleep(500 * time.Millisecond)
	caps := [][]capPkt{stops[0](), stops[1]()}
	rcv := drainUDP(got, 300*time.Millisecond)

	type dgram struct {
		anns  map[int]bool
		frags int
		sport uint16
	}
	frags := map[uint32]*dgram{}
	small := map[uint16]map[int]bool{}
	var fragPkts int
	for ai, list := range caps {
		for _, p := range list {
			if p.proto != unix.IPPROTO_UDP {
				continue
			}
			if !p.frag {
				if small[p.sport] == nil {
					small[p.sport] = map[int]bool{}
				}
				small[p.sport][ai] = true
				continue
			}
			fragPkts++
			d := frags[p.id]
			if d == nil {
				d = &dgram{anns: map[int]bool{}}
				frags[p.id] = d
			}
			d.anns[ai] = true
			d.frags++
			if p.sport != 0 {
				d.sport = p.sport
			}
		}
	}
	split, per := 0, [2]int{}
	fragFlow := map[uint16]map[int]bool{} // per flow (SNATed source port): announcers of its fragments
	for _, d := range frags {
		if len(d.anns) > 1 {
			split++
		}
		for ai := range d.anns {
			per[ai]++
			if d.sport != 0 {
				if fragFlow[d.sport] == nil {
					fragFlow[d.sport] = map[int]bool{}
				}
				fragFlow[d.sport][ai] = true
			}
		}
	}
	var flowsDesc []string
	sameAnn, diffAnn := 0, 0
	for sp, fa := range fragFlow {
		sa := small[sp]
		flowsDesc = append(flowsDesc, fmt.Sprintf("%d:unfragmented->%v,fragments->%v", sp, keys(sa), keys(fa)))
		if len(sa) == 1 && len(fa) == 1 && sa[firstKey(fa)] {
			sameAnn++
		} else {
			diffAnn++
		}
	}
	slices.Sort(flowsDesc)
	var largeGot, smallGot, badSize int
	for seq, n := range rcv {
		switch {
		case seq == 1:
		case n == size:
			largeGot++
		case n == 64:
			smallGot++
		default:
			badSize++
		}
	}
	srv1 := snmp(t, f.srv)
	fs := func(i uint32) uint64 { return stat(t, f.objAnn[0].Stats, i) + stat(t, f.objAnn[1].Stats, i) }
	first, followed, missing := fs(stFragTransitFirst), fs(stFragTransitFollowed), fs(stFragTransitMissing)
	reasmOK := (srv1["IpReasmOKs"] - srv0["IpReasmOKs"]) + (srv1["Ip6ReasmOKs"] - srv0["Ip6ReasmOKs"])
	t.Logf("SPIKE1GAP RESULT frag-steer %s (b) fragmented datagrams seen at announcers=%d (fragments=%d), split across announcers=%d, datagrams per announcer=%v; flows whose fragments and unfragmented datagrams used the same announcer=%d, different=%d %v",
		name, len(frags), fragPkts, split, per, sameAnn, diffAnn, flowsDesc)
	t.Logf("SPIKE1GAP RESULT frag-steer %s (a) server received large(%d B)=%d/%d, prime=%v, small=%d/%d, wrong size=%d; server reassembled=%d; pl_tun_in transit=[%d %d]; frags map (both announcers): first-recorded=%d followed=%d missing-entry drops=%d; server:%s; ann0:%s; ann1:%s",
		name, size, largeGot, sent-1, rcv[1] == size, smallGot, flows, badSize, reasmOK, stat(t, f.objAnn[0].Stats, 5), stat(t, f.objAnn[1].Stats, 5),
		first, followed, missing,
		deltas(srv0, srv1, reasm...), deltas(ann0, snmp(t, f.anns[0]), reasm...), deltas(ann1, snmp(t, f.anns[1]), reasm...))
	t.Logf("SPIKE1GAP RESULT frag-steer %s prime: send=%s pod ICMP:%s; pod route MTU to the server after prime=%s; nodeA steered=%d",
		name, errStr(primeErr), deltas(p0, p1, podICMP...), pmtu, stat(t, f.objA.Stats, 0))
	t.Logf("SPIKE1GAP detail frag-steer %s: bpf ann0:%s ann1:%s; ann0 kernel:%s; ann1 kernel:%s",
		name, stats(t, f.objAnn[0].Stats), stats(t, f.objAnn[1].Stats), deltas(ann0, snmp(t, f.anns[0])), deltas(ann1, snmp(t, f.anns[1])))

	require.Equal(t, flows, smallGot, "control: every unfragmented datagram must cross the tunnel and reach the server")
	require.NotZero(t, len(frags), "no fragments reached the announcers")
	require.Zero(t, split, "the fragments of one datagram must reach one announcer")
	// Every fragmented datagram that reached an announcer recorded its
	// owner once and forwarded every later fragment through frags.
	require.Zero(t, missing, "later fragments with no frags entry")
	require.Equal(t, uint64(len(frags)), first, "first fragments recorded = fragmented datagrams captured at the announcers")
	require.Equal(t, uint64(fragPkts-len(frags)), followed, "later fragments forwarded = fragments captured - datagrams")
	require.Zero(t, badSize, "datagrams delivered with the wrong size")
	require.Equal(t, sent-1, largeGot, "every fragmented datagram must reach the server, reassembled")
	require.GreaterOrEqual(t, reasmOK, int64(largeGot), "the server must have reassembled the datagrams")
}

// Fragment and Packet Too Big counters (ST_FRAG_*, ST_PTB_* in
// bpf/egress_core.c).
const (
	stFragTransitFirst = 15 + iota
	stFragTransitFollowed
	stFragTransitMissing
	stFragReplyFirst
	stFragReplyFollowed
	stFragReplyMissing
	stPtb4Sent
	stPtb6Sent
	stPtbFailed
)

func fragStats(t *testing.T, m *ebpf.Map) string {
	names := []string{"transit_first", "transit_followed", "transit_missing", "reply_first", "reply_followed", "reply_missing"}
	var b strings.Builder
	for i, n := range names {
		fmt.Fprintf(&b, " %s=%d", n, stat(t, m, uint32(stFragTransitFirst+i)))
	}
	return b.String()
}

// linkTx reports a link's tx, tx error and tx drop counters.
func linkTx(n *node, dev string) [3]uint64 {
	l, err := n.nl.LinkByName(dev)
	if err != nil || l.Attrs().Statistics == nil {
		return [3]uint64{}
	}
	s := l.Attrs().Statistics
	return [3]uint64{s.TxPackets, s.TxErrors, s.TxDropped}
}

// fragSummary counts captured UDP packets: whole, first fragments and
// later fragments, and the largest length seen.
func fragSummary(ps []capPkt) string {
	var whole, firstF, later, maxLen int
	for _, p := range ps {
		if p.proto != unix.IPPROTO_UDP && !(p.frag && p.offset > 0) {
			continue
		}
		switch {
		case !p.frag:
			whole++
		case p.offset == 0:
			firstF++
		default:
			later++
		}
		maxLen = max(maxLen, p.length)
	}
	return fmt.Sprintf("whole=%d first-frags=%d later-frags=%d max-len=%d", whole, firstF, later, maxLen)
}

// runFragReply: the reply direction. The pod sends one small datagram to
// the server (the flow, SNATed into nodeA's slice); the server answers it
// with a small control datagram and n datagrams of size bytes, which it
// fragments itself. The server reaches the egress address through the
// announcer (host routes, as through a gateway), whose pl_announce returns
// every fragment to nodeA through the tunnel; nodeA's pl_tun_in admits them,
// conntrack reassembles and de-NATs them, and the pod reassembles them.
func runFragReply(t *testing.T, name string, inner6, under6 bool, variant string) {
	const port, n, size = 9320, 4, 3000
	f := newFragNet(t, inner6, under6, 1, 1400, 0, false)
	x, ox := f.anns[0], f.objAnn[0]
	attach(t, x, "pool", ox.PlAnnounce, ebpf.AttachTCXIngress)
	attach(t, f.a, "purelb-eg0", f.objA.PlTunIn, ebpf.AttachTCXIngress)
	mtu := 0
	if variant == "fit" {
		mtu = 1400 // fragments of at most 1400 bytes fit the 1500 underlay once encapsulated
	}
	for _, r := range [][2]string{{"172.30.10.110/32", "172.30.10.2"}, {"fdaa::110/128", "fdaa::2"}} {
		_, d, _ := net.ParseCIDR(r[0])
		route(t, f.srv, &netlink.Route{Dst: d, Gw: net.ParseIP(r[1]), MTU: mtu})
	}

	srvC := listenUDP(t, f.srv, jh(f.server, port))
	pc := f.podSocket(t, false)
	require.NoError(t, f.send(pc, port, 0, 64))
	buf := make([]byte, 65536)
	_ = srvC.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, from, err := srvC.ReadFromUDPAddrPort(buf)
	require.NoError(t, err, "forward control: the pod's datagram must reach the server; bpf nodeA:%s announcer:%s",
		stats(t, f.objA.Stats), stats(t, ox.Stats))
	require.Equal(t, f.egress, from.Addr().Unmap().String(), "the server must see the egress address")
	require.True(t, inSlice(from.Port()), "source port %d must be in nodeA's slice", from.Port())

	p0, a0, x0, s0 := snmp(t, f.pod), snmp(t, f.a), snmp(t, x), snmp(t, f.srv)
	tx0 := linkTx(x, "purelb-eg0")
	capOuter := capture(t, f.a, "ul0")        // outer packets nodeA receives
	capInner := capture(t, f.a, "purelb-eg0") // inner packets after decapsulation
	capPod := capture(t, f.pod, "eth0")       // what the pod receives
	capErr := sniffICMPErr(t, f.srv, "br0")   // ICMP errors the server receives

	reply := func(seq uint32, sz int) error {
		b := make([]byte, sz)
		binary.BigEndian.PutUint32(b, seq)
		_, err := srvC.WriteToUDPAddrPort(b, from)
		return err
	}
	require.NoError(t, reply(1, 64))
	for i := 0; i < n; i++ {
		require.NoError(t, reply(uint32(10+i), size))
		time.Sleep(20 * time.Millisecond)
	}
	got := map[uint32]int{}
	_ = pc.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	for {
		nr, _, err := pc.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if nr >= 4 {
			got[binary.BigEndian.Uint32(buf)] = nr
		}
	}
	outer, inner, atPod := capOuter(), capInner(), capPod()
	tx1 := linkTx(x, "purelb-eg0")
	p1, a1, x1 := snmp(t, f.pod), snmp(t, f.a), snmp(t, x)
	s1 := snmp(t, f.srv)
	errs := capErr()
	ptb4, ptb6, ptbFail := stat(t, ox.Stats, stPtb4Sent), stat(t, ox.Stats, stPtb6Sent), stat(t, ox.Stats, stPtbFailed)
	srvPMTU := routeMTU(f.srv, f.egress, 0)
	t.Logf("SPIKE1GAP RESULT frag-reply %s %s ptb: pl_announce frag-needed sent=%d packet-too-big sent=%d failed=%d; server received %d ICMP errors %v; server ICMP:%s; server route MTU to %s=%s",
		name, variant, ptb4, ptb6, ptbFail, len(errs), errs, deltas(s0, s1, "IcmpInDestUnreachs", "IcmpInErrors", "IcmpInCsumErrors", "Icmp6InPktTooBigs", "Icmp6InErrors", "Icmp6InCsumErrors"),
		f.egress, srvPMTU)

	large, badSize := 0, 0
	for i := 0; i < n; i++ {
		switch got[uint32(10+i)] {
		case size:
			large++
		case 0:
		default:
			badSize++
		}
	}
	podReasm := (p1["IpReasmOKs"] - p0["IpReasmOKs"]) + (p1["Ip6ReasmOKs"] - p0["Ip6ReasmOKs"])
	innerFrags := 0
	for _, p := range inner {
		if p.frag {
			innerFrags++
		}
	}
	annFirst, annFollowed, annMissing := stat(t, ox.Stats, stFragReplyFirst), stat(t, ox.Stats, stFragReplyFollowed), stat(t, ox.Stats, stFragReplyMissing)
	aFirst, aFollowed, aMissing := stat(t, f.objA.Stats, stFragReplyFirst), stat(t, f.objA.Stats, stFragReplyFollowed), stat(t, f.objA.Stats, stFragReplyMissing)
	t.Logf("SPIKE1GAP RESULT frag-reply %s %s (server route mtu %d): pod received control=%v large(%d B)=%d/%d, wrong size=%d, pod reassembled=%d; announcer pl_announce frags: first-recorded=%d followed=%d missing-entry drops=%d, to_tunnel=%d; nodeA pl_tun_in frags: first-recorded=%d followed=%d missing-entry drops=%d, reply_local=%d",
		name, variant, mtu, got[1] == 64, size, large, n, badSize, podReasm,
		annFirst, annFollowed, annMissing, stat(t, ox.Stats, 2), aFirst, aFollowed, aMissing, stat(t, f.objA.Stats, 8))
	t.Logf("SPIKE1GAP RESULT frag-reply %s %s tunnel: announcer purelb-eg0 tx/txerr/txdrop +%d/+%d/+%d; announcer kernel:%s; nodeA outer received (ul0): %s; nodeA inner received (purelb-eg0): %s; pod received: %s; nodeA kernel:%s; pod kernel:%s",
		name, variant, tx1[0]-tx0[0], tx1[1]-tx0[1], tx1[2]-tx0[2], deltas(x0, x1, nodeICMP...), fragSummary(outer), fragSummary(inner), fragSummary(atPod),
		deltas(a0, a1, reasm...), deltas(p0, p1, reasm...))
	t.Logf("SPIKE1GAP detail frag-reply %s %s: bpf announcer:%s |%s; nodeA:%s |%s; announcer kernel:%s; server kernel:%s",
		name, variant, stats(t, ox.Stats), fragStats(t, ox.Stats), stats(t, f.objA.Stats), fragStats(t, f.objA.Stats),
		deltas(x0, x1), deltas(s0, snmp(t, f.srv)))

	require.Equal(t, 64, got[1], "control: an unfragmented reply must return to the pod")
	require.Zero(t, annMissing, "announcer: later fragments with no frags entry")
	require.Equal(t, uint64(n), annFirst, "announcer: one first fragment recorded per datagram")
	require.NotZero(t, annFollowed, "announcer: later fragments must follow their first")
	require.Zero(t, ptbFail, "Packet Too Big synthesis failed")
	if variant == "oversize" && under6 {
		// 1b: the replies did not fit the IPv6 underlay; pl_announce answered
		// them with the tunnel MTU and the server learnt it.
		want := 1400
		if inner6 {
			require.NotZero(t, ptb6, "pl_announce must answer with Packet Too Big")
			require.Zero(t, ptb4)
		} else {
			require.NotZero(t, ptb4, "pl_announce must answer with frag-needed")
			require.Zero(t, ptb6)
		}
		require.NotEmpty(t, errs, "the server must receive the ICMP error")
		for _, e := range errs {
			require.Equal(t, f.egress, e.src.String(), "error source: the egress address %v", e)
			require.Equal(t, f.server, e.dst.String(), "error destination: the server %v", e)
			require.Equal(t, f.egress, e.quotedDst.String(), "quoted packet: the reply to the egress address %v", e)
			require.Equal(t, uint32(want), e.mtu, "MTU in the error %v", e)
			require.True(t, e.csum && e.ipCsum, "checksums %v", e)
			if inner6 {
				require.Equal(t, [2]byte{2, 0}, [2]byte{e.typ, e.code}, "Packet Too Big %v", e)
				require.Equal(t, 1280, e.length, "an ICMPv6 error fills the minimum MTU %v", e)
			} else {
				require.Equal(t, [2]byte{3, 4}, [2]byte{e.typ, e.code}, "frag-needed %v", e)
				require.Equal(t, 576, e.length, "an ICMP error stays within 576 bytes %v", e)
			}
		}
		require.Equal(t, uint64(len(errs)), ptb4+ptb6, "every error sent reached the server")
		require.Equal(t, fmt.Sprint(want), srvPMTU, "the server's path MTU to the egress address")
		// A second batch: the server now fragments to the learnt MTU, so it
		// fits the tunnel and must reach the pod.
		before := ptb4 + ptb6
		for i := 0; i < n; i++ {
			require.NoError(t, reply(uint32(100+i), size))
			time.Sleep(20 * time.Millisecond)
		}
		got2 := map[uint32]int{}
		_ = pc.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		for {
			nr, _, err := pc.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if nr >= 4 {
				got2[binary.BigEndian.Uint32(buf)] = nr
			}
		}
		large2 := 0
		for i := 0; i < n; i++ {
			if got2[uint32(100+i)] == size {
				large2++
			}
		}
		after := stat(t, ox.Stats, stPtb4Sent) + stat(t, ox.Stats, stPtb6Sent)
		t.Logf("SPIKE1GAP RESULT frag-reply %s %s: first batch delivered %d/%d (each datagram sent before the server learnt the MTU is lost); after the error, second batch delivered %d/%d, further errors sent=%d",
			name, variant, large, n, large2, n, after-before)
		require.Equal(t, n, large2, "after learning the MTU, every reply must reach the pod")
		require.Equal(t, before, after, "no further errors once the server fragments to the tunnel MTU")
		return
	}
	require.Zero(t, ptb4+ptb6, "no Packet Too Big unless an IPv6 underlay cannot carry the reply")
	require.Zero(t, aMissing, "nodeA: later fragments with no frags entry")
	require.Equal(t, uint64(n), aFirst, "nodeA: one first fragment recorded per datagram")
	require.Equal(t, annFollowed, aFollowed, "nodeA must receive every later fragment the announcer tunnelled")
	require.Equal(t, uint64(innerFrags)-aFirst, aFollowed, "nodeA: later fragments followed = inner fragments captured - datagrams")
	require.Zero(t, badSize, "replies delivered with the wrong size")
	require.Equal(t, n, large, "every fragmented reply must reach the pod, reassembled")
	require.GreaterOrEqual(t, podReasm, int64(n), "the pod must have reassembled the replies")
}

func keys(m map[int]bool) []int {
	var k []int
	for i := range m {
		k = append(k, i)
	}
	slices.Sort(k)
	return k
}

func firstKey(m map[int]bool) int {
	for k := range m {
		return k
	}
	return -1
}

func runFragPMTU(t *testing.T, name string, inner6, under6 bool, variant string) {
	const port = 9310
	over := 36 // L3 Geneve: outer IPv4 + UDP + Geneve
	if under6 {
		over = 56
	}
	rmtu, lock := 0, false
	switch variant {
	case "route-mtu":
		rmtu = 1500 - over
	case "route-mtu-lock":
		rmtu, lock = 1500-over, true
	}
	f := newFragNet(t, inner6, under6, 1, 1500, rmtu, lock)
	got := f.collectUDP(t, port)
	c := f.podSocket(t, true)
	hdr := 28
	if inner6 {
		hdr = 48
	}
	require.NoError(t, f.send(c, port, 1, 64))
	ctl := drainUDP(got, time.Second)
	require.Equal(t, 64, ctl[1], "control: a small datagram must cross the tunnel; bpf nodeA:%s announcer:%s; announcer:%s; server:%s",
		stats(t, f.objA.Stats), stats(t, f.objAnn[0].Stats), deltas(nil, snmp(t, f.anns[0])), deltas(nil, snmp(t, f.srv)))

	p0, a0, b0, s0 := snmp(t, f.pod), snmp(t, f.a), snmp(t, f.anns[0]), snmp(t, f.srv)
	eg0 := func() string {
		l, err := f.a.nl.LinkByName("purelb-eg0")
		if err != nil || l.Attrs().Statistics == nil {
			return "?"
		}
		s := l.Attrs().Statistics
		return fmt.Sprintf("tx=%d txerr=%d txdrop=%d", s.TxPackets, s.TxErrors, s.TxDropped)
	}
	e0 := eg0()
	csIn := udpCsums(t, f.anns[0], "purelb-eg0", port) // inner, decapsulated, before pl_tun_in
	csOut := udpCsums(t, f.srv, "p0", port)            // after transit, on the server's bridge port
	var sendRes []string
	for i := 0; i < 3; i++ {
		sendRes = append(sendRes, errStr(f.send(c, port, uint32(10+i), 1500-hdr)))
		time.Sleep(300 * time.Millisecond)
	}
	rcv := drainUDP(got, 300*time.Millisecond)
	inSeen, outSeen := csIn(), csOut()
	var delivered []bool
	for i := 0; i < 3; i++ {
		delivered = append(delivered, rcv[uint32(10+i)] == 1500-hdr)
	}
	t.Logf("SPIKE1GAP RESULT frag-pmtu %s %s (route mtu %d lock=%v): three 1500-byte DF datagrams: send=%v delivered=%v; pod ICMP:%s; pod route MTU to the server=%s; nodeA:%s; nodeA purelb-eg0 %s -> %s; nodeA route MTU (mark 3)=%s; announcer:%s",
		name, variant, rmtu, lock, sendRes, delivered, deltas(p0, snmp(t, f.pod), podICMP...), routeMTU(f.pod, f.server, 0),
		deltas(a0, snmp(t, f.a), nodeICMP...), e0, eg0(), routeMTU(f.a, f.server, slot), deltas(b0, snmp(t, f.anns[0]), reasm...))
	t.Logf("SPIKE1GAP RESULT frag-pmtu %s %s: inner UDP checksum on the bytes at announcer purelb-eg0 %v, at server port %v; server:%s",
		name, variant, inSeen, outSeen, deltas(s0, snmp(t, f.srv), "UdpInDatagrams", "UdpInCsumErrors", "Udp6InDatagrams", "Udp6InCsumErrors"))
	t.Logf("SPIKE1GAP detail frag-pmtu %s %s: bpf nodeA:%s announcer:%s; announcer kernel:%s; server kernel:%s",
		name, variant, stats(t, f.objA.Stats), stats(t, f.objAnn[0].Stats), deltas(b0, snmp(t, f.anns[0])), deltas(s0, snmp(t, f.srv)))
}

// ---- Spike 1 gaps 26a/26b: reordered fragments, IPv6 extension headers ----

// replyFlow attaches pl_announce (announcer's pool interface) and pl_tun_in
// (nodeA's purelb-eg0), routes the server to the egress addresses through
// the announcer, and opens one UDP flow from the pod to the server's port.
// It returns the server's socket, the pod's socket and the pod's address as
// the server sees it (the egress address and a port in nodeA's slice).
func (f *fragNet) replyFlow(t *testing.T, port int) (*net.UDPConn, *net.UDPConn, netip.AddrPort) {
	t.Helper()
	x, ox := f.anns[0], f.objAnn[0]
	attach(t, x, "pool", ox.PlAnnounce, ebpf.AttachTCXIngress)
	attach(t, f.a, "purelb-eg0", f.objA.PlTunIn, ebpf.AttachTCXIngress)
	for _, r := range [][2]string{{"172.30.10.110/32", "172.30.10.2"}, {"fdaa::110/128", "fdaa::2"}} {
		_, d, _ := net.ParseCIDR(r[0])
		route(t, f.srv, &netlink.Route{Dst: d, Gw: net.ParseIP(r[1])})
	}
	srvC := listenUDP(t, f.srv, jh(f.server, port))
	pc := f.podSocket(t, false)
	require.NoError(t, f.send(pc, port, 0, 64))
	buf := make([]byte, 65536)
	_ = srvC.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, from, err := srvC.ReadFromUDPAddrPort(buf)
	require.NoError(t, err, "the pod's datagram must reach the server; bpf nodeA:%s announcer:%s",
		stats(t, f.objA.Stats), stats(t, ox.Stats))
	require.Equal(t, f.egress, from.Addr().Unmap().String(), "the server must see the egress address")
	require.True(t, inSlice(from.Port()), "source port %d must be in nodeA's slice", from.Port())
	return srvC, pc, netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
}

// readSeqs reads datagrams on c for d and returns their lengths by the
// sequence number in their first four bytes.
func readSeqs(c *net.UDPConn, d time.Duration) map[uint32]int {
	got := map[uint32]int{}
	buf := make([]byte, 65536)
	_ = c.SetReadDeadline(time.Now().Add(d))
	for {
		nr, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return got
		}
		if nr >= 4 {
			got[binary.BigEndian.Uint32(buf)] = nr
		}
	}
}

// underlays returns nodeA's and the first announcer's underlay addresses.
func underlays(under6 bool) (a, x string) {
	if under6 {
		return "fd00::1", "fd00::2"
	}
	return "10.0.0.1", "10.0.0.2"
}

// fragDatagram builds one UDP datagram of 3000 payload bytes (sequence seq)
// from src:sport to dst:dport, in three fragments (1200-byte pieces).
func fragDatagram(src, dst string, sport, dport uint16, seq, id uint32) [][]byte {
	s, d := netip.MustParseAddr(src), netip.MustParseAddr(dst)
	payload := make([]byte, 3000)
	binary.BigEndian.PutUint32(payload, seq)
	u := udpDatagram(s, d, sport, dport, payload)
	if s.Is6() {
		return ipv6Packets(s, d, nil, nil, true, id, nil, nil, unix.IPPROTO_UDP, u, 1200)
	}
	return ipv4Packets(s, d, uint16(id), unix.IPPROTO_UDP, u, 1200)
}

// TestSpike1GapFragReorder (26a): a datagram's later fragment arrives
// before its first. Three fragments are delivered in the order [F2, F1, F3]:
//
//	reply:   from the server, through pl_announce on the announcer and
//	         pl_tun_in on nodeA, to the pod's flow;
//	transit: injected into the announcer's tunnel from nodeA's underlay
//	         address (as nodeA's purelb-eg0 sends), through pl_tun_in to
//	         the server.
//
// Then F2 alone is sent again (a sender repeating the fragment), and a new
// datagram is sent in order (a higher-layer retransmission). An in-order
// datagram first proves the injection path.
func TestSpike1GapFragReorder(t *testing.T) {
	requireVM(t)
	for _, inner6 := range []bool{false, true} {
		for _, under6 := range []bool{false, true} {
			name := fmt.Sprintf("inner%s/underlay%s", yes(inner6, "6", "4"), yes(under6, "6", "4"))
			t.Run("reply/"+name, func(t *testing.T) { runReorder(t, name, inner6, under6, true) })
			t.Run("transit/"+name, func(t *testing.T) { runReorder(t, name, inner6, under6, false) })
		}
	}
}

func runReorder(t *testing.T, name string, inner6, under6, reply bool) {
	const port = 9330
	f := newFragNet(t, inner6, under6, 1, 1400, 0, false)
	ox := f.objAnn[0]
	aU, xU := underlays(under6)
	var (
		send  func(pkts [][]byte, order []int) error
		recv  func() map[uint32]int
		build func(seq, id uint32) [][]byte
		dir   = uint32(stFragReplyFirst)
		where = "reply (pl_announce, then pl_tun_in on nodeA)"
	)
	if reply {
		_, pc, pod := f.replyFlow(t, port)
		send = func(p [][]byte, o []int) error { return sendRawIP(f.srv, f.egress, p, o) }
		recv = func() map[uint32]int { return readSeqs(pc, time.Second) }
		build = func(seq, id uint32) [][]byte {
			return fragDatagram(f.server, f.egress, port, pod.Port(), seq, id)
		}
	} else {
		dir, where = stFragTransitFirst, "transit (pl_tun_in on the announcer)"
		got := f.collectUDP(t, port)
		send = func(p [][]byte, o []int) error { return sendGeneve(f.a, aU, xU, false, nil, p, o) }
		recv = func() map[uint32]int { return drainUDP(got, time.Second) }
		build = func(seq, id uint32) [][]byte {
			return fragDatagram(f.egress, f.server, sliceLo+20, port, seq, id)
		}
	}
	// Fragment counters for this direction: announcer, then nodeA (reply only).
	snap := func() [6]uint64 {
		var c [6]uint64
		for i := uint32(0); i < 3; i++ {
			c[i] = stat(t, ox.Stats, dir+i)
			c[3+i] = stat(t, f.objA.Stats, dir+i)
		}
		return c
	}
	delta := func(a, b [6]uint64) string {
		return fmt.Sprintf("announcer first/followed/missing +%d/+%d/+%d, nodeA +%d/+%d/+%d",
			b[0]-a[0], b[1]-a[1], b[2]-a[2], b[3]-a[3], b[4]-a[4], b[5]-a[5])
	}

	c0 := snap()
	ctl := build(1, 0x1001)
	require.NoError(t, send(ctl, []int{0, 1, 2}))
	g := recv()
	c1 := snap()
	require.Equal(t, 3000, g[1], "control: an in-order datagram must be delivered; %s; bpf announcer:%s nodeA:%s",
		delta(c0, c1), stats(t, ox.Stats), stats(t, f.objA.Stats))

	dg := build(2, 0x1002)
	require.NoError(t, send(dg, []int{1, 0, 2}))
	g = recv()
	c2 := snap()
	reordered := g[2] == 3000

	require.NoError(t, send(dg, []int{1}))
	g = recv()
	c3 := snap()
	refrag := g[2] == 3000

	require.NoError(t, send(build(3, 0x1003), []int{0, 1, 2}))
	g = recv()
	c4 := snap()
	retrans := g[3] == 3000

	t.Logf("SPIKE1GAP RESULT frag-reorder %s %s: control in order delivered; [F2,F1,F3] delivered=%v (%s); F2 sent again: datagram recovered=%v (%s); new datagram in order delivered=%v (%s)",
		name, where, reordered, delta(c1, c2), refrag, delta(c2, c3), retrans, delta(c3, c4))
	require.False(t, reordered, "a later fragment arriving first is dropped, so the datagram is incomplete")
	require.Equal(t, uint64(1), c2[2]-c1[2], "the early later fragment is dropped and counted")
	require.Equal(t, uint64(1), c2[0]-c1[0], "its first fragment still records the owner")
	require.Equal(t, uint64(1), c2[1]-c1[1], "the later fragment after the first follows it")
	require.Equal(t, uint64(1), c3[1]-c2[1], "F2 sent again follows the recorded first fragment")
	require.True(t, retrans, "a new datagram in order must be delivered")
}

// TestSpike1GapIPv6ExtHeaders (26b): IPv6 packets with extension headers
// before the upper-layer header, classified by the parser's bounded walk:
//
//	unfragmented: Hop-by-Hop, Destination Options, UDP;
//	fragmented:   Hop-by-Hop, Fragment, then (first fragment only)
//	              Destination Options, UDP: three fragments.
//
// reply: from the server through pl_announce and nodeA's pl_tun_in to the
// pod; transit: injected into the announcer's tunnel from nodeA, through
// pl_tun_in to the server. IPv4 and IPv6 underlays.
func TestSpike1GapIPv6ExtHeaders(t *testing.T) {
	requireVM(t)
	for _, under6 := range []bool{false, true} {
		name := "inner6/underlay" + yes(under6, "6", "4")
		t.Run("reply/"+name, func(t *testing.T) { runExtHdr(t, name, under6, true) })
		t.Run("transit/"+name, func(t *testing.T) { runExtHdr(t, name, under6, false) })
	}
}

func runExtHdr(t *testing.T, name string, under6, reply bool) {
	const port = 9350
	f := newFragNet(t, true, under6, 1, 1400, 0, false)
	ox := f.objAnn[0]
	aU, xU := underlays(under6)
	var (
		send             func(pkts [][]byte) error
		recv             func() map[uint32]int
		src, dst         string
		sport, dport     uint16
		dir, stPass      = uint32(stFragReplyFirst), uint32(2) // to_tunnel
		where            = "reply (pl_announce, then pl_tun_in on nodeA)"
		nodeAPass, nodeA = uint32(8), true // reply_local
	)
	if reply {
		_, pc, pod := f.replyFlow(t, port)
		send = func(p [][]byte) error { return sendRawIP(f.srv, f.egress, p, []int{0, 1, 2}[:len(p)]) }
		recv = func() map[uint32]int { return readSeqs(pc, time.Second) }
		src, dst, sport, dport = f.server, f.egress, port, pod.Port()
	} else {
		dir, stPass, where, nodeA = stFragTransitFirst, 5, "transit (pl_tun_in on the announcer)", false
		got := f.collectUDP(t, port)
		send = func(p [][]byte) error { return sendGeneve(f.a, aU, xU, false, nil, p, []int{0, 1, 2}[:len(p)]) }
		recv = func() map[uint32]int { return drainUDP(got, time.Second) }
		src, dst, sport, dport = f.egress, f.server, sliceLo+30, port
	}
	s, d := netip.MustParseAddr(src), netip.MustParseAddr(dst)
	dgram := func(seq uint32, size int) []byte {
		b := make([]byte, size)
		binary.BigEndian.PutUint32(b, seq)
		return udpDatagram(s, d, sport, dport, b)
	}
	unfrag := ipv6Packets(s, d, []byte{0, 60}, [][]byte{padOpt(), padOpt()}, false, 0, nil, nil,
		unix.IPPROTO_UDP, dgram(11, 200), 0)
	frag := ipv6Packets(s, d, []byte{0}, [][]byte{padOpt()}, true, 0x2001, []byte{60}, [][]byte{padOpt()},
		unix.IPPROTO_UDP, dgram(12, 3000), 1200)
	require.Len(t, frag, 3)
	cnt := func() [5]uint64 {
		c := [5]uint64{stat(t, ox.Stats, stPass), stat(t, ox.Stats, dir), stat(t, ox.Stats, dir+1), stat(t, ox.Stats, dir+2)}
		if nodeA {
			c[4] = stat(t, f.objA.Stats, nodeAPass)
		}
		return c
	}
	c0 := cnt()
	require.NoError(t, send(unfrag))
	g := recv()
	c1 := cnt()
	gotU := g[11] == 200
	require.NoError(t, send(frag))
	g = recv()
	c2 := cnt()
	gotF := g[12] == 3000
	passName := yes(reply, "to_tunnel", "transit")
	t.Logf("SPIKE1GAP RESULT ipv6-ext %s %s: [HBH, DestOpts, UDP] delivered=%v (announcer %s +%d, nodeA reply_local +%d); [HBH, Frag, DestOpts, UDP] x3 delivered=%v (announcer %s +%d, frags first/followed/missing +%d/+%d/+%d, nodeA reply_local +%d); bpf announcer:%s nodeA:%s",
		name, where, gotU, passName, c1[0]-c0[0], c1[4]-c0[4], gotF, passName, c2[0]-c1[0], c2[1]-c1[1], c2[2]-c1[2], c2[3]-c1[3], c2[4]-c1[4],
		stats(t, ox.Stats), stats(t, f.objA.Stats))
	require.Equal(t, uint64(1), c1[0]-c0[0], "the unfragmented packet must be classified and forwarded")
	require.True(t, gotU, "the unfragmented packet must be delivered")
	require.Equal(t, uint64(3), c2[0]-c1[0], "every fragment must be classified and forwarded")
	require.Equal(t, [3]uint64{1, 2, 0}, [3]uint64{c2[1] - c1[1], c2[2] - c1[2], c2[3] - c1[3]},
		"frags: one first fragment recorded, two followed, none missing")
	require.True(t, gotF, "the fragmented datagram must be delivered")
	if nodeA {
		require.Equal(t, uint64(1), c1[4]-c0[4], "nodeA must admit the unfragmented reply")
		require.Equal(t, uint64(3), c2[4]-c1[4], "nodeA must admit every fragment")
	}
}
