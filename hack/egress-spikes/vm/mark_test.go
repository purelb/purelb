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
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// Spike 3: does the pl_pod_tag mark survive the CNI's path into netfilter?
//
// Run on a real cluster node, as root, by hack/egress-spikes/cluster/
// spike3.sh, against one throwaway pod: pl_pod_tag goes at the head of the
// pod's host-side device's tcx ingress chain and marks the pod's packets
// with slot 3; nft counts packets carrying that mark at prerouting (-150,
// where classification runs), forward and postrouting. The ratio of counted
// to tagged packets is the survival rate. The table and the link are removed
// at the end.
//
//	SPIKE3_DEV      host-side pod device (required)
//	SPIKE3_POD4/6   the pod's IPv4/IPv6 address (for the source check)
//	SPIKE3_SECONDS  how long to count (default 20)
func TestSpike3MarkOnNode(t *testing.T) {
	dev := os.Getenv("SPIKE3_DEV")
	if dev == "" {
		t.Skip("run on a cluster node by hack/egress-spikes/cluster/spike3.sh")
	}
	secs, _ := strconv.Atoi(os.Getenv("SPIKE3_SECONDS"))
	if secs == 0 {
		secs = 20
	}
	l, err := netlink.LinkByName(dev)
	require.NoError(t, err)

	// SPIKE3_ATTACH=netkit-peer attaches at the netkit peer hook of the
	// host-side primary (the design's netkit attach point) instead of tcx
	// ingress on a veth.
	at := ebpf.AttachTCXIngress
	if os.Getenv("SPIKE3_ATTACH") == "netkit-peer" {
		at = ebpf.AttachNetkitPeer
	}
	spec, err := loadCore()
	require.NoError(t, err)
	spec.Programs["pl_pod_tag"].AttachType = at
	var o coreObjects
	require.NoError(t, spec.LoadAndAssign(&o, nil))
	defer o.Close()
	pd := corePodDev{Slot: 3}
	if v := os.Getenv("SPIKE3_POD4"); v != "" {
		pd.V4 = ip16(v)[0]
	}
	if v := os.Getenv("SPIKE3_POD6"); v != "" {
		pd.V6 = ip16(v)
	}
	require.NoError(t, o.PodDevs.Put(uint32(l.Attrs().Index), pd))

	// Who else is on this chain, and where do we land?
	before, _ := link.QueryPrograms(link.QueryOptions{Target: l.Attrs().Index, Attach: at})
	var tl link.Link
	if at == ebpf.AttachNetkitPeer {
		tl, err = link.AttachNetkit(link.NetkitOptions{Program: o.PlPodTag, Interface: l.Attrs().Index,
			Attach: at, Anchor: link.Head()})
	} else {
		tl, err = link.AttachTCX(link.TCXOptions{Program: o.PlPodTag, Interface: l.Attrs().Index,
			Attach: at, Anchor: link.Head()})
	}
	require.NoError(t, err)
	defer tl.Close()
	after, _ := link.QueryPrograms(link.QueryOptions{Target: l.Attrs().Index, Attach: at})
	t.Logf("SPIKE3 %s %s chain: %d program(s) before, %d after (ours at head)", dev, at, progs(before), progs(after))
	info, err := o.PlPodTag.Info()
	require.NoError(t, err)
	ours, _ := info.ID()
	t.Logf("SPIKE3 ORDER start %s", chainOrder(l.Attrs().Index, at, ours))
	// The chain again at the end of the window: spike3.sh can restart the
	// CNI DaemonSet meanwhile (SPIKE3_RESTART_DS) to see whether the CNI
	// re-attaches ahead of us, replaces us, or leaves the order alone.
	defer func() { t.Logf("SPIKE3 ORDER end %s", chainOrder(l.Attrs().Index, at, ours)) }()

	c, err := nftables.New()
	require.NoError(t, err)
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "purelb_spike3"})
	defer func() { c.DelTable(tbl); _ = c.Flush() }()
	hooks := []struct {
		name string
		hook *nftables.ChainHook
		prio *nftables.ChainPriority
	}{
		{"prerouting", nftables.ChainHookPrerouting, nftables.ChainPriorityRef(-150)},
		{"forward", nftables.ChainHookForward, nftables.ChainPriorityFilter},
		{"postrouting", nftables.ChainHookPostrouting, nftables.ChainPriorityRef(90)},
	}
	// Per family: NFPROTO 2 is IPv4, 10 is IPv6. Each hook counts the
	// tagged packets of each family; "fromPod" counts every packet from the
	// pod's own address, tagged or not, at prerouting -- the denominator.
	fams := []struct {
		name  string
		proto byte
	}{{"v4", 2}, {"v6", 10}}
	for _, h := range hooks {
		ch := c.AddChain(&nftables.Chain{Name: h.name, Table: tbl, Type: nftables.ChainTypeFilter,
			Hooknum: h.hook, Priority: h.prio})
		for _, f := range fams {
			c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, UserData: []byte(f.name), Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{f.proto}},
				&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
				&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(0xff), Xor: le32(0)},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: le32(3)},
				&expr.Counter{},
			}})
		}
	}
	// fromPod counts the pod's packets at prerouting; fwdFromPod only the
	// forwarded ones (not the pod's packets to the node itself, e.g. IPv6
	// neighbour discovery), for SPIKE3_DECIDE.
	for _, sc := range []struct {
		name string
		hook *nftables.ChainHook
	}{{"fromPod", nftables.ChainHookPrerouting}, {"fwdFromPod", nftables.ChainHookForward}} {
		src := c.AddChain(&nftables.Chain{Name: sc.name, Table: tbl, Type: nftables.ChainTypeFilter,
			Hooknum: sc.hook, Priority: nftables.ChainPriorityRef(-151)})
		for _, f := range []struct {
			name, env    string
			proto        byte
			offset, size uint32
		}{{"v4", "SPIKE3_POD4", 2, 12, 4}, {"v6", "SPIKE3_POD6", 10, 8, 16}} {
			a := os.Getenv(f.env)
			if a == "" {
				continue
			}
			c.AddRule(&nftables.Rule{Table: tbl, Chain: src, UserData: []byte(f.name), Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{f.proto}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: f.offset, Len: f.size},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: netip.MustParseAddr(a).AsSlice()},
				&expr.Counter{},
			}})
		}
	}
	require.NoError(t, c.Flush())
	counters := func() map[string]uint64 { // "<chain>/<family>"
		m := map[string]uint64{}
		for _, name := range []string{"prerouting", "forward", "postrouting", "fromPod", "fwdFromPod"} {
			rules, err := c.GetRules(tbl, &nftables.Chain{Name: name, Table: tbl})
			require.NoError(t, err)
			for _, r := range rules {
				for _, e := range r.Exprs {
					if ctr, ok := e.(*expr.Counter); ok {
						m[name+"/"+string(r.UserData)] = ctr.Packets
					}
				}
			}
		}
		return m
	}

	if os.Getenv("SPIKE3_DECIDE") != "" {
		// The active probe as a method selector: poll every 50 ms and report,
		// per family, the first observation that decides the classification
		// method. pl_pod_tag's counter (all families) says the pod sent; a
		// forwarded pod packet with the tag at prerouting says mark, one
		// without says source IP; SPIKE3_DECIDE packets tagged with none
		// forwarded through netfilter says neither (the eBPF stage).
		k, _ := strconv.Atoi(os.Getenv("SPIKE3_DECIDE"))
		start := time.Now()
		done := map[string]bool{}
		var first time.Duration // when pl_pod_tag first counted a pod packet
		for time.Since(start) < time.Duration(secs)*time.Second && len(done) < 2 {
			m, tg := counters(), stat(t, o.Stats, 12)
			if tg > 0 && first == 0 {
				first = time.Since(start)
			}
			for _, f := range []string{"v4", "v6"} {
				if done[f] {
					continue
				}
				d := ""
				switch {
				case m["fwdFromPod/"+f] > 0 && m["prerouting/"+f] > 0:
					d = "mark"
				case m["fwdFromPod/"+f] > 0:
					d = "source-ip"
				case int(tg) >= k:
					d = "neither (eBPF stage)"
				}
				if d != "" {
					done[f] = true
					t.Logf("SPIKE3 DECIDE %s %d ms after the pod's first packet: bpf_tagged(all)=%d forwarded_from_pod=%d tagged_at_prerouting=%d from_pod_at_prerouting=%d => %s",
						f, (time.Since(start) - first).Milliseconds(), tg, m["fwdFromPod/"+f], m["prerouting/"+f], m["fromPod/"+f], d)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(time.Duration(secs)*time.Second - time.Since(start))
	} else {
		time.Sleep(time.Duration(secs) * time.Second)
	}

	tagged := stat(t, o.Stats, 12)
	spoof := stat(t, o.Stats, 13)
	counts := counters()
	for _, f := range fams {
		from := counts["fromPod/"+f.name]
		pct := func(n uint64) float64 {
			if from == 0 {
				return 0
			}
			return 100 * float64(n) / float64(from)
		}
		verdict := "survives"
		switch {
		case counts["prerouting/"+f.name] > 0:
		case from > 0:
			verdict = "OVERWRITTEN (pod packets reach netfilter without the tag)"
		default:
			verdict = "NO PACKETS (none from the pod's address reached prerouting)"
		}
		t.Logf("SPIKE3 RESULT %s dev=%s tagged(all families)=%d spoof_dropped=%d pod_%s_at_prerouting=%d marked_at_prerouting=%d (%.0f%%) forward=%d postrouting=%d pod_forwarded=%d => mark %s",
			f.name, dev, tagged, spoof, f.name, from, counts["prerouting/"+f.name], pct(counts["prerouting/"+f.name]),
			counts["forward/"+f.name], counts["postrouting/"+f.name], counts["fwdFromPod/"+f.name], verdict)
	}
}

// chainOrder lists the device's tcx ingress chain head first, marking ours.
func chainOrder(ifindex int, at ebpf.AttachType, ours ebpf.ProgramID) string {
	r, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: at})
	if err != nil {
		return "query failed: " + err.Error()
	}
	s := "rev=" + strconv.FormatUint(r.Revision, 10) + " chain=["
	for i, p := range r.Programs {
		if i > 0 {
			s += " "
		}
		s += strconv.FormatUint(uint64(p.ID), 10)
		if p.ID == ours {
			s += "(ours)"
		}
	}
	return s + "]"
}

func progs(r *link.QueryResult) int {
	if r == nil {
		return -1
	}
	return len(r.Programs)
}
