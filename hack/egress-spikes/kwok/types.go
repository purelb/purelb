package main

// Object shapes for the designs. Field names follow the design document
// (§4.5 EgressEndpoints, §4.6 EgressNodeState) so that serialized sizes are
// representative.

type sliceJSON struct {
	Node        string `json:"node"`
	Start       int    `json:"start"`
	End         int    `json:"end"`
	State       string `json:"state"`
	ActiveSince int64  `json:"activeSince,omitempty"`
}

type addrJSON struct {
	Address    string      `json:"address"`
	Family     string      `json:"family"`
	Pool       string      `json:"pool"`
	Interface  string      `json:"interface"`
	Placement  string      `json:"placement"`
	Announcers []string    `json:"announcers"`
	Slices     []sliceJSON `json:"slices"`
	Reserved   []any       `json:"reserved"`
}

type destJSON struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type policyJSON struct {
	UID           string   `json:"uid"`
	Helpers       []string `json:"helpers"`
	FailurePolicy string   `json:"failurePolicy"`
	Destinations  destJSON `json:"destinations"`
}

type endpointJSON struct {
	Pod    string   `json:"pod"`
	UID    string   `json:"uid"`
	Node   string   `json:"node"`
	PodIPs []string `json:"podIPs"`
}

// original §4.5: one object per policy; addresses + slices + endpoints.
type origSpec struct {
	Policy    policyJSON     `json:"policy"`
	Addresses []addrJSON     `json:"addresses"`
	Endpoints []endpointJSON `json:"endpoints"`
}

// decided D5: one object per (policy, address); addresses + slices only.
type decSpec struct {
	Policy    policyJSON `json:"policy"`
	Epoch     int64      `json:"epoch"`
	Addresses []addrJSON `json:"addresses"`
}

// decided D5: allocator-only pod list.
type membersSpec struct {
	PolicyUID string         `json:"policyUID"`
	Endpoints []endpointJSON `json:"endpoints"`
}

type claimJSON struct {
	EgressUID string `json:"egressUID"`
	Address   string `json:"address"`
	// TS is the claim time (unix ns); used only to measure claim latency.
	TS int64 `json:"ts"`
}

type observedJSON struct {
	EgressUID  string `json:"egressUID"`
	Generation int64  `json:"generation"`
}

type condJSON struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

var staticConds = []condJSON{
	{"DatapathReady", "True", "Programmed", "2026-10-01T00:00:00Z"},
	{"TunnelPeersReachable", "True", "AllPeersReachable", "2026-10-01T00:00:00Z"},
}

// original §4.6 status.
type origNodeStatus struct {
	Observed     []observedJSON `json:"observed"`
	Kernel       string         `json:"kernel"`
	PodNodeStage string         `json:"podNodeStage"`
	CNI          map[string]any `json:"cni"`
	KubeProxy    map[string]any `json:"kubeProxy"`
	PolicySlots  map[string]int `json:"policySlots"`
	Claims       []claimJSON    `json:"claims"`
	Announcing   []any          `json:"announcing"`
	AddressGuard map[string]any `json:"addressGuard"`
	Conditions   []condJSON     `json:"conditions"`
}

// decided D6/D21 status: one ack integer, capability, claims, conditions.
type decNodeStatus struct {
	ObservedEpoch int64          `json:"observedEpoch"`
	Capability    map[string]any `json:"capability"`
	Claims        []claimJSON    `json:"claims"`
	Conditions    []condJSON     `json:"conditions"`
}

// scoped: one slice table per (policy, address), watched only by that
// address's announcers (label spike.purelb.io/ann-<node>), with no epoch: its
// version is its metadata.generation.
type scSpec struct {
	Policy    policyJSON `json:"policy"`
	Addresses []addrJSON `json:"addresses"`
}

// scoped: a node's own slices, written by the allocator into its
// EgressNodeState.spec; the node watches only that object.
type nodeSliceJSON struct {
	Policy  int    `json:"policy"`
	Addr    int    `json:"addr"`
	Address string `json:"address"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	State   string `json:"state"`
}

type scNodeSpec struct {
	Slices []nodeSliceJSON `json:"slices"`
	// option A (D7 host-route mode): for each routed host-route address this
	// node holds a slice of, the advertiser set computed by the allocator.
	Advertisers []advJSON `json:"advertisers,omitempty"`
}

type advJSON struct {
	Address string   `json:"address"`
	Nodes   []string `json:"nodes"`
}

type addrAckJSON struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
}

// scoped status: acks only for the addresses this node announces, plus the
// generation of its own EgressNodeState spec.
type scNodeStatus struct {
	ObservedAddrs          []addrAckJSON  `json:"observedAddrs"`
	ObservedNodeGeneration int64          `json:"observedNodeGeneration"`
	Capability             map[string]any `json:"capability"`
	Claims                 []claimJSON    `json:"claims"`
	Conditions             []condJSON     `json:"conditions"`
}

var cniInfo = map[string]any{"name": "flannel", "dataplane": "iptables", "masqueradeRules": []string{"FLANNEL-POSTRTG"}}
var kubeProxyInfo = map[string]any{"mode": "nftables"}
