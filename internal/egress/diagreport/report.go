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

// Package diagreport is what `lbnodeagent diag` prints and `kubectl purelb
// egress` reads. It builds on every platform the plugin does.
package diagreport

import "hash/fnv"

// Version is the report's format.
const Version = 1

// Report is one node's egress datapath, read from the kernel (D21).
type Report struct {
	Version int    `json:"version"`
	Node    string `json:"node"`
	// Stage is the pod-node stage: EBPF or Netfilter.
	Stage string `json:"stage"`
	// Pinned: the datapath's maps are pinned; without them nothing below
	// but Table can be read.
	Pinned bool `json:"pinned"`
	// Table: the nft table exists.
	Table      bool        `json:"table"`
	Addresses  []Address   `json:"addresses,omitempty"`
	Nodes      []Node      `json:"nodes,omitempty"`
	PodDevices []PodDevice `json:"podDevices,omitempty"`
	Slots      []Slot      `json:"slots,omitempty"`
	NATFlows   int         `json:"natFlows"`
	// Reasons counts packets by reason, IPv4 then IPv6; zeros left out.
	Reasons map[string][2]uint64 `json:"reasons,omitempty"`
	Errors  []string             `json:"errors,omitempty"`
	// Checks are what validate --live reports.
	Checks NodeChecks `json:"checks"`
}

// NodeChecks are the node's settings egress depends on.
type NodeChecks struct {
	Kernel string `json:"kernel"`
	// KernelConfigKnown: /proc/config.gz exists; KernelConfigMissing lists the
	// options egress needs that it doesn't have.
	KernelConfigKnown   bool     `json:"kernelConfigKnown"`
	KernelConfigMissing []string `json:"kernelConfigMissing,omitempty"`
	BPFFS               bool     `json:"bpffs"`
	Forwarding4         string   `json:"forwarding4"`
	Forwarding6         string   `json:"forwarding6"`
	// Interfaces are the pool interfaces.
	Interfaces []Interface `json:"interfaces,omitempty"`
	Chains     []Chain     `json:"chains,omitempty"`
}

// Interface is one pool interface's settings.
type Interface struct {
	Name        string `json:"name"`
	ArpAnnounce string `json:"arpAnnounce"`
	AcceptRA    string `json:"acceptRA"`
}

// Chain is where the guard and pl_announce run on an interface's tcx ingress
// chain: positions from 1, 0 when not attached.
type Chain struct {
	Interface string `json:"interface"`
	Guard     int    `json:"guard"`
	Announce  int    `json:"announce"`
}

// Address is what the node's programs know of one egress address.
type Address struct {
	Address string `json:"address"`
	Held    bool   `json:"held"`
	// WholeOwner is the NodeID owning every port (PerPod), else 0.
	WholeOwner uint64         `json:"wholeOwner,omitempty"`
	SliceSize  uint32         `json:"sliceSize,omitempty"`
	Base       uint16         `json:"base,omitempty"`
	Table      uint32         `json:"table,omitempty"`
	Slices     []Slice        `json:"slices,omitempty"`
	Reserved   []ReservedPort `json:"reserved,omitempty"`
	Services   []ServicePort  `json:"services,omitempty"`
	Traffic    Traffic        `json:"traffic"`
}

// Slice is one slice's owner, a NodeID.
type Slice struct {
	Index uint16 `json:"index"`
	Owner uint64 `json:"owner"`
}

// ReservedPort is a reserved port's owner, a NodeID.
type ReservedPort struct {
	Proto uint8  `json:"proto"`
	Port  uint16 `json:"port"`
	Owner uint64 `json:"owner"`
}

// ServicePort is a Service port of a shared address.
type ServicePort struct {
	Proto uint8  `json:"proto"`
	Port  uint16 `json:"port"`
}

// Traffic is an address's packets and bytes on this node, by role.
type Traffic struct {
	OutPod          Count `json:"outPod"`
	ReturnPod       Count `json:"returnPod"`
	OutAnnouncer    Count `json:"outAnnouncer"`
	ReturnAnnouncer Count `json:"returnAnnouncer"`
}

// Count is packets and bytes.
type Count struct {
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

// Node is a node the programs know.
type Node struct {
	ID uint64 `json:"id"`
	V4 string `json:"v4,omitempty"`
	V6 string `json:"v6,omitempty"`
}

// PodDevice is a pod's host-side device.
type PodDevice struct {
	Ifindex int    `json:"ifindex"`
	V4      string `json:"v4,omitempty"`
	V6      string `json:"v6,omitempty"`
	PodSet  uint8  `json:"podSet,omitempty"`
	Hold    bool   `json:"hold,omitempty"`
}

// Slot is one of this node's slots: the address and ports its pods leave by.
// First and Last are zero for a whole address (PerPod).
type Slot struct {
	ID      uint8  `json:"id"`
	Address string `json:"address"`
	First   uint16 `json:"first,omitempty"`
	Last    uint16 `json:"last,omitempty"`
}

// NodeID is a node's id in the programs' maps: its name's FNV-1a hash, never 0.
func NodeID(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	if id := h.Sum64(); id != 0 {
		return id
	}
	return 1
}
