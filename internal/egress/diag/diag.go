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

// Package diag is `lbnodeagent diag`: it prints this node's egress datapath
// as JSON, for `kubectl purelb egress dump` and `trace --live` (D21). It
// runs next to the agent through kubectl exec, and only reads.
package diag

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/nftables"

	"purelb.io/internal/bpfpin"
	"purelb.io/internal/egress/datapath"
	"purelb.io/internal/egress/diagreport"
)

// Command is the lbnodeagent subcommand.
const Command = "diag"

// Main prints the report. A datapath it can't read is reported in it.
func Main(args []string) {
	fs := flag.NewFlagSet(Command, flag.ExitOnError)
	pinDir := fs.String("pin-dir", bpfpin.Dir, "where the datapath's maps are pinned")
	_ = fs.Parse(args)
	r := Read(*pinDir, os.Getenv("PURELB_NODE_NAME"))
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Read reads the datapath pinned in pinDir, and the nft table.
func Read(pinDir, node string) diagreport.Report {
	r := diagreport.Report{Version: diagreport.Version, Node: node, Stage: "Netfilter", Checks: readChecks(pinDir)}
	fail := func(what string, err error) {
		if err != nil {
			r.Errors = append(r.Errors, what+": "+err.Error())
		}
	}
	var tableSlots map[uint8]datapath.SlotNAT
	var tableTraffic map[netip.Addr]datapath.Traffic
	if c, err := nftables.New(); err != nil {
		fail("nft", err)
	} else {
		// A missing table is an error from the kernel: what a node with egress
		// off has.
		tbl, err := c.ListTableOfFamily(datapath.NftTable, nftables.TableFamilyINet)
		r.Table = err == nil && tbl != nil
		if r.Table {
			tableSlots, err = datapath.TableSlots(c)
			fail("nft table", err)
			addrs := map[uint8]netip.Addr{}
			for id, slot := range tableSlots {
				addrs[id] = slot.Addr
			}
			tableTraffic, err = datapath.SlotTraffic(c, addrs)
			fail("nft counters", err)
		}
	}
	if datapath.PinnedEBPFStage(pinDir) {
		r.Stage = "EBPF"
	}
	o, err := datapath.OpenPinned(pinDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fail("pinned maps", err)
		}
		r.Slots = slotsOf(tableSlots)
		return r
	}
	defer func() { _ = o.Close() }()
	r.Pinned = true
	readMaps(&r, o, tableTraffic, fail)
	if r.Stage == "EBPF" {
		s, err := o.RecoverPodSlots()
		fail("slots", err)
		r.Slots = slotsOf(s)
		flows, err := o.NatFlows()
		fail("NAT flows", err)
		r.NATFlows = len(flows)
	} else {
		r.Slots = slotsOf(tableSlots)
	}
	return r
}

// readMaps fills in r from the maps. tableTraffic is what the nft table counts:
// a netfilter-stage node's pods' traffic, which no program sees.
func readMaps(r *diagreport.Report, o *datapath.Objects, tableTraffic map[netip.Addr]datapath.Traffic, fail func(string, error)) {
	traffic, err := o.TrafficCounts()
	fail("traffic", err)
	addrs, err := o.RecoverAddresses()
	fail("addresses", err)
	for addr, e := range addrs {
		a := diagreport.Address{Address: addr.String(), Held: e.Held, WholeOwner: e.WholeOwner, SliceSize: e.SliceSize,
			Base: e.Base, Table: e.Table}
		for i, owner := range e.Slices {
			a.Slices = append(a.Slices, diagreport.Slice{Index: i, Owner: owner})
		}
		slices.SortFunc(a.Slices, func(x, y diagreport.Slice) int { return int(x.Index) - int(y.Index) })
		for k, owner := range e.Reserved {
			a.Reserved = append(a.Reserved, diagreport.ReservedPort{Proto: k.Proto, Port: k.Port, Owner: owner})
		}
		for k := range e.Services {
			a.Services = append(a.Services, diagreport.ServicePort{Proto: k.Proto, Port: k.Port})
		}
		t := traffic[addr]
		t.Packets[datapath.TrafficOutPod] += tableTraffic[addr].Packets[datapath.TrafficOutPod]
		t.Bytes[datapath.TrafficOutPod] += tableTraffic[addr].Bytes[datapath.TrafficOutPod]
		count := func(kind int) diagreport.Count {
			return diagreport.Count{Packets: t.Packets[kind], Bytes: t.Bytes[kind]}
		}
		a.Traffic = diagreport.Traffic{OutPod: count(datapath.TrafficOutPod), ReturnPod: count(datapath.TrafficReturnPod),
			OutAnnouncer: count(datapath.TrafficOutAnnouncer), ReturnAnnouncer: count(datapath.TrafficReturnAnnouncer)}
		r.Addresses = append(r.Addresses, a)
	}
	slices.SortFunc(r.Addresses, func(x, y diagreport.Address) int {
		return netip.MustParseAddr(x.Address).Compare(netip.MustParseAddr(y.Address))
	})
	nodes, err := o.ReadNodes()
	fail("nodes", err)
	for id, n := range nodes {
		r.Nodes = append(r.Nodes, diagreport.Node{ID: id, V4: addrString(n.V4), V6: addrString(n.V6)})
	}
	devs, err := o.RecoverPodDevs()
	fail("pod devices", err)
	for ifindex, d := range devs {
		r.PodDevices = append(r.PodDevices, diagreport.PodDevice{Ifindex: ifindex, V4: addrString(d.V4), V6: addrString(d.V6),
			PodSet: d.PodSet, Hold: d.Hold})
	}
	slices.SortFunc(r.PodDevices, func(x, y diagreport.PodDevice) int { return x.Ifindex - y.Ifindex })
	reasons, err := o.Counts()
	fail("reasons", err)
	for k, v := range reasons {
		if v[0] != 0 || v[1] != 0 {
			if r.Reasons == nil {
				r.Reasons = map[string][2]uint64{}
			}
			r.Reasons[k] = v
		}
	}
}

func slotsOf(m map[uint8]datapath.SlotNAT) []diagreport.Slot {
	var out []diagreport.Slot
	for id, s := range m {
		out = append(out, diagreport.Slot{ID: id, Address: s.Addr.String(), First: s.First, Last: s.Last})
	}
	slices.SortFunc(out, func(x, y diagreport.Slot) int { return int(x.ID) - int(y.ID) })
	return out
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

// kernelOptions are the kernel options egress needs.
var kernelOptions = []string{"NF_TABLES", "NF_TABLES_INET", "NFT_NAT", "NFT_FIB_IPV4", "NFT_FIB_IPV6", "NFT_CT",
	"NF_CONNTRACK_MARK", "NF_CT_NETLINK", "IP_MULTIPLE_TABLES", "IPV6_MULTIPLE_TABLES", "GENEVE", "BPF_SYSCALL",
	"DEBUG_INFO_BTF"}

func readChecks(pinDir string) diagreport.NodeChecks {
	n := diagreport.NodeChecks{Forwarding4: sysctl("ipv4", "all", "forwarding"), Forwarding6: sysctl("ipv6", "all", "forwarding"),
		BPFFS: bpfpin.Usable(pinDir)}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		n.Kernel = strings.TrimSpace(string(b))
	}
	n.KernelConfigKnown, n.KernelConfigMissing = kernelConfig("/proc/config.gz")
	if pools, err := datapath.PinnedPoolInterfaces(pinDir); err == nil {
		for ifindex := range pools {
			if i, err := net.InterfaceByIndex(ifindex); err == nil {
				n.Interfaces = append(n.Interfaces, diagreport.Interface{Name: i.Name,
					ArpAnnounce: sysctl("ipv4", i.Name, "arp_announce"), AcceptRA: sysctl("ipv6", i.Name, "accept_ra")})
			}
		}
		slices.SortFunc(n.Interfaces, func(x, y diagreport.Interface) int { return strings.Compare(x.Name, y.Name) })
	}
	n.Chains = datapath.ChainOrders(pinDir)
	return n
}

func sysctl(family, iface, name string) string {
	b, err := os.ReadFile(filepath.Join("/proc/sys/net", family, "conf", iface, name))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

// kernelConfig reads the options egress needs from a gzipped kernel config.
func kernelConfig(path string) (bool, []string) {
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	z, err := gzip.NewReader(f)
	if err != nil {
		return false, nil
	}
	b, err := io.ReadAll(z)
	if err != nil {
		return false, nil
	}
	return true, missingOptions(string(b))
}

func missingOptions(config string) []string {
	have := map[string]bool{}
	for _, line := range strings.Split(config, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && (v == "y" || v == "m") {
			have[strings.TrimPrefix(k, "CONFIG_")] = true
		}
	}
	var missing []string
	for _, o := range kernelOptions {
		if !have[o] {
			missing = append(missing, o)
		}
	}
	return missing
}
