// Copyright 2020-2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v2

import (
	"fmt"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================================
// ServiceGroup - defines IP address pools for LoadBalancer services
// ============================================================================

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceGroup is the top-level custom resource for configuring
// ServiceGroups. It contains the usual CRD metadata, and the service
// group spec and status.
// +kubebuilder:resource:shortName=sg;sgs
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Announce",type=string,JSONPath=`.status.announce`
// +kubebuilder:printcolumn:name="IPAM",type=string,JSONPath=`.status.ipam`
// +kubebuilder:printcolumn:name="Addresses",type=string,JSONPath=`.status.addresses`
// +kubebuilder:printcolumn:name="Allocated-V4",type=integer,JSONPath=`.status.allocatedIPv4`,priority=1
// +kubebuilder:printcolumn:name="Allocated-V6",type=integer,JSONPath=`.status.allocatedIPv6`,priority=1
// +kubebuilder:printcolumn:name="Available-V4",type=integer,JSONPath=`.status.availableIPv4`,priority=1
// +kubebuilder:printcolumn:name="Available-V6",type=integer,JSONPath=`.status.availableIPv6`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ServiceGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ServiceGroupSpec `json:"spec"`
	// +optional
	Status ServiceGroupStatus `json:"status,omitempty"`
}

// ServiceGroupSpec configures the allocator. Exactly one of Local, Remote,
// or External must be specified.
//
// - Local: IP pool managed locally, addresses announced on the node's local interface
// - Remote: IP pool managed locally, addresses announced on the dummy interface (for BGP/routing)
// - External: IP addresses managed by an external IPAM system reached via a sidecar process
//
// +kubebuilder:validation:XValidation:rule="(has(self.local) ? 1 : 0) + (has(self.remote) ? 1 : 0) + (has(self.external) ? 1 : 0) == 1",message="exactly one of local, remote, or external must be specified"
type ServiceGroupSpec struct {
	// Local configures a pool of IP addresses that will be announced
	// on the node's local interface (the interface with the default route).
	// Use this for addresses on the same subnet as your nodes.
	// +optional
	Local *ServiceGroupLocalSpec `json:"local,omitempty"`

	// Remote configures a pool of IP addresses that will be announced
	// on the dummy interface (kube-lb0) for routing protocols like BGP.
	// Use this for addresses on a different subnet from your nodes.
	// +optional
	Remote *ServiceGroupRemoteSpec `json:"remote,omitempty"`

	// External configures PureLB to request addresses from an external
	// IPAM system reached via a sidecar process running in the allocator
	// pod. The sidecar owns its configuration; PureLB only needs to know
	// where to dial.
	// +optional
	External *ServiceGroupExternalSpec `json:"external,omitempty"`
}

// ServiceGroupLocalSpec configures a local IP address pool.
// Addresses from this pool are announced on the node's local interface.
type ServiceGroupLocalSpec struct {
	// V4Pool specifies a single pool of IPv4 addresses.
	// Use V4Pools for multiple pools.
	// +optional
	V4Pool *AddressPool `json:"v4pool,omitempty"`

	// V6Pool specifies a single pool of IPv6 addresses.
	// Use V6Pools for multiple pools.
	// +optional
	V6Pool *AddressPool `json:"v6pool,omitempty"`

	// V4Pools specifies multiple pools of IPv4 addresses.
	// +optional
	V4Pools []AddressPool `json:"v4pools,omitempty"`

	// V6Pools specifies multiple pools of IPv6 addresses.
	// +optional
	V6Pools []AddressPool `json:"v6pools,omitempty"`

	// SkipIPv6DAD when true disables Duplicate Address Detection for IPv6
	// addresses. This can speed up address configuration but should only
	// be used when you are certain there are no address conflicts.
	// +kubebuilder:default=false
	// +optional
	SkipIPv6DAD bool `json:"skipIPv6DAD,omitempty"`

	// MultiPool enables multi-pool allocation. When true, services get one IP
	// from each address range (per family) that has active nodes, making the
	// service reachable from every subnet.
	// +kubebuilder:default=false
	// +optional
	MultiPool bool `json:"multiPool,omitempty"`

	// BalancePools enables balanced allocation across address ranges.
	// When true, new allocations pick the range with the fewest IPs
	// currently in use, distributing services evenly across subnets.
	// Each IP family (IPv4/IPv6) is balanced independently.
	// Mutually exclusive with MultiPool.
	// +kubebuilder:default=false
	// +optional
	BalancePools bool `json:"balancePools,omitempty"`
}

// PoolForAddress returns the AddressPool that contains the given IP address.
// If no pool contains the address, an error is returned.
func (s *ServiceGroupLocalSpec) PoolForAddress(address net.IP) (*AddressPool, error) {
	// Check V6Pools first (array)
	for i := range s.V6Pools {
		pool, err := NewIPRange(s.V6Pools[i].Pool)
		if err == nil && pool.Contains(address) {
			return &s.V6Pools[i], nil
		}
	}
	// Check V4Pools (array)
	for i := range s.V4Pools {
		pool, err := NewIPRange(s.V4Pools[i].Pool)
		if err == nil && pool.Contains(address) {
			return &s.V4Pools[i], nil
		}
	}
	// Check singular V6Pool
	if s.V6Pool != nil {
		pool, err := NewIPRange(s.V6Pool.Pool)
		if err == nil && pool.Contains(address) {
			return s.V6Pool, nil
		}
	}
	// Check singular V4Pool
	if s.V4Pool != nil {
		pool, err := NewIPRange(s.V4Pool.Pool)
		if err == nil && pool.Contains(address) {
			return s.V4Pool, nil
		}
	}
	return nil, fmt.Errorf("unable to find pool for address %+v", address)
}

// ServiceGroupRemoteSpec configures a remote IP address pool.
// Addresses from this pool are announced on the dummy interface for
// routing protocols (e.g., BGP via BIRD).
type ServiceGroupRemoteSpec struct {
	// V4Pool specifies a single pool of IPv4 addresses.
	// Use V4Pools for multiple pools.
	// +optional
	V4Pool *AddressPool `json:"v4pool,omitempty"`

	// V6Pool specifies a single pool of IPv6 addresses.
	// Use V6Pools for multiple pools.
	// +optional
	V6Pool *AddressPool `json:"v6pool,omitempty"`

	// V4Pools specifies multiple pools of IPv4 addresses.
	// +optional
	V4Pools []AddressPool `json:"v4pools,omitempty"`

	// V6Pools specifies multiple pools of IPv6 addresses.
	// +optional
	V6Pools []AddressPool `json:"v6pools,omitempty"`

	// MultiPool enables multi-pool allocation. When true, services get one IP
	// from each address range (per family) that has active nodes, making the
	// service reachable from every subnet.
	// +kubebuilder:default=false
	// +optional
	MultiPool bool `json:"multiPool,omitempty"`

	// BalancePools enables balanced allocation across address ranges.
	// When true, new allocations pick the range with the fewest IPs
	// currently in use, distributing services evenly across subnets.
	// Each IP family (IPv4/IPv6) is balanced independently.
	// Mutually exclusive with MultiPool.
	// +kubebuilder:default=false
	// +optional
	BalancePools bool `json:"balancePools,omitempty"`
}

// PoolForAddress returns the AddressPool that contains the given IP address.
// If no pool contains the address, an error is returned.
func (s *ServiceGroupRemoteSpec) PoolForAddress(address net.IP) (*AddressPool, error) {
	// Check V6Pools first (array)
	for i := range s.V6Pools {
		pool, err := NewIPRange(s.V6Pools[i].Pool)
		if err == nil && pool.Contains(address) {
			return &s.V6Pools[i], nil
		}
	}
	// Check V4Pools (array)
	for i := range s.V4Pools {
		pool, err := NewIPRange(s.V4Pools[i].Pool)
		if err == nil && pool.Contains(address) {
			return &s.V4Pools[i], nil
		}
	}
	// Check singular V6Pool
	if s.V6Pool != nil {
		pool, err := NewIPRange(s.V6Pool.Pool)
		if err == nil && pool.Contains(address) {
			return s.V6Pool, nil
		}
	}
	// Check singular V4Pool
	if s.V4Pool != nil {
		pool, err := NewIPRange(s.V4Pool.Pool)
		if err == nil && pool.Contains(address) {
			return s.V4Pool, nil
		}
	}
	return nil, fmt.Errorf("unable to find pool for address %+v", address)
}

// ServiceGroupExternalSpec configures a pool whose addresses come from
// an external IPAM system reached via a sidecar process running in the
// allocator pod. The sidecar owns its configuration; PureLB only needs
// to know where to dial.
type ServiceGroupExternalSpec struct {
	// Provider is the IPAM provider name. Cosmetic display value only
	// (surfaced in .status.ipam). PureLB does NOT verify this against
	// the sidecar's identity.
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// Socket is the absolute path to the sidecar's Unix domain socket
	// inside the allocator pod (typically a shared emptyDir volume).
	// +kubebuilder:default="/var/run/purelb/ipam.sock"
	// +optional
	Socket string `json:"socket,omitempty"`

	// Announce specifies which mechanism announces addresses from this
	// pool: "local" (primary interface) or "remote" (kubelb0 dummy
	// interface, for BGP/routing).
	// +kubebuilder:validation:Enum=local;remote
	Announce string `json:"announce"`
}

// AddressPool specifies a pool of IP addresses with routing configuration.
type AddressPool struct {
	// Pool specifies a pool of addresses that PureLB manages. It can be
	// a CIDR or a from-to range of addresses, e.g.,
	// "192.168.1.240/29" or "192.168.1.240-192.168.1.250".
	// +kubebuilder:validation:Required
	Pool string `json:"pool"`

	// Subnet specifies the subnet that contains all of the addresses in
	// the Pool. It's specified with CIDR notation, e.g., "192.168.1.0/24".
	// All of the addresses in the Pool must be contained within the Subnet.
	// +kubebuilder:validation:Required
	Subnet string `json:"subnet"`

	// Aggregation changes the address mask of the allocated address
	// from the subnet mask to the specified mask. It can be "default"
	// or an integer in the range 8-128.
	// +optional
	Aggregation string `json:"aggregation,omitempty"`
}

// ServiceGroupStatus reports runtime information about the ServiceGroup,
// populated by the allocator as services are assigned and released.
// Values may briefly lag the spec after a config change.
type ServiceGroupStatus struct {
	// Announce is the announcement mechanism: "Local" (IP added to the
	// node's primary interface, for directly-attached subnets) or
	// "Remote" (IP added to the kubelb0 dummy interface and advertised
	// via BGP for non-local subnets). Derived from Pool.PoolType().
	// +optional
	Announce string `json:"announce,omitempty"`

	// IPAM identifies the address-management source: "Cluster" when
	// PureLB's allocator is authoritative for the address space, or the
	// name of an external IPAM system (e.g. a sidecar plugin's provider
	// name) when allocation is delegated. Derived from Pool.IPAMSource().
	// +optional
	IPAM string `json:"ipam,omitempty"`

	// Addresses is a human-readable summary of the pool's address scope,
	// produced by the Pool implementation. For cluster-IPAM pools this
	// is the configured CIDRs and ranges; for external-IPAM pools it is
	// whatever descriptor the source chooses (may be empty when the
	// source does not expose address scope locally).
	// +optional
	Addresses []string `json:"addresses,omitempty"`

	// AllocatedIPv4 is the number of IPv4 addresses currently assigned.
	// +kubebuilder:validation:Minimum=0
	AllocatedIPv4 int64 `json:"allocatedIPv4"`

	// AllocatedIPv6 is the number of IPv6 addresses currently assigned.
	// +kubebuilder:validation:Minimum=0
	AllocatedIPv6 int64 `json:"allocatedIPv6"`

	// AvailableIPv4 is the number of free IPv4 addresses. Absent when
	// pool capacity is not knowable locally (external IPAM).
	// Distinct from the value 0, which means the pool is full.
	// +kubebuilder:validation:Minimum=0
	// +optional
	AvailableIPv4 *int64 `json:"availableIPv4,omitempty"`

	// AvailableIPv6 is the number of free IPv6 addresses. Absent when
	// pool capacity is not knowable locally (external IPAM).
	// +kubebuilder:validation:Minimum=0
	// +optional
	AvailableIPv6 *int64 `json:"availableIPv6,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceGroupList holds a list of ServiceGroup.
type ServiceGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []ServiceGroup `json:"items"`
}

// ============================================================================
// LBNodeAgent - configures the node agent's announcement behavior
// ============================================================================

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LBNodeAgent is the top-level custom resource for configuring node
// agents. It contains the usual CRD metadata, and the agent spec and
// status.
// +kubebuilder:resource:shortName=lbna;lbnas
// +kubebuilder:storageversion
type LBNodeAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LBNodeAgentSpec `json:"spec"`
	// +optional
	Status LBNodeAgentStatus `json:"status,omitempty"`
}

// LBNodeAgentSpec configures the node agents.
type LBNodeAgentSpec struct {
	// Local configures announcement of service addresses by configuring
	// the underlying operating system networking.
	// +optional
	Local *LBNodeAgentLocalSpec `json:"local,omitempty"`
}

// LBNodeAgentLocalSpec configures the local announcer behavior.
type LBNodeAgentLocalSpec struct {
	// LocalInterface specifies the interface to use for announcement of
	// local addresses (addresses on the same subnet as the node).
	// Can be:
	//   - "default": use the interface with the default route (recommended)
	//   - A regex pattern to match interface names (e.g., "eth.*", "enp0s.*")
	// +kubebuilder:default="default"
	// +optional
	LocalInterface string `json:"localInterface,omitempty"`

	// DummyInterface specifies the name of the dummy interface to use for
	// announcement of remote addresses (addresses on different subnets).
	// The dummy interface is created automatically if it doesn't exist.
	// +kubebuilder:default="kube-lb0"
	// +optional
	DummyInterface string `json:"dummyInterface,omitempty"`

	// GARPConfig configures Gratuitous ARP behavior for address announcements.
	// GARP packets notify network equipment (switches, routers) that the
	// IP-to-MAC binding has changed, enabling faster failover.
	// +optional
	GARPConfig *GARPConfig `json:"garpConfig,omitempty"`

	// AddressConfig configures how VIP addresses are added to interfaces.
	// This allows control over address lifetimes and flags to prevent
	// conflicts with CNI plugins like Flannel that inspect address flags.
	// +optional
	AddressConfig *AddressConfig `json:"addressConfig,omitempty"`

	// Interfaces specifies additional interfaces to include in subnet detection
	// for the subnet-aware election. By default, only the interface with
	// the default route is used. Add interface names here to include
	// additional subnets in the election.
	// +optional
	Interfaces []string `json:"interfaces,omitempty"`
}

// GARPConfig configures Gratuitous ARP behavior for service address announcements.
type GARPConfig struct {
	// Enabled determines whether GARP packets should be sent.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// InitialDelay is the time to wait after adding an address before sending
	// the first GARP. This allows time for the address to be fully configured.
	// Format: Go duration string (e.g., "100ms", "1s").
	// +kubebuilder:default="100ms"
	// +optional
	InitialDelay string `json:"initialDelay,omitempty"`

	// Count is the number of GARP packets to send. Sending multiple GARPs
	// increases reliability as network equipment may miss individual packets.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	// +optional
	Count *int `json:"count,omitempty"`

	// Interval is the time between GARP packets when Count > 1.
	// Format: Go duration string (e.g., "500ms", "1s").
	// +kubebuilder:default="500ms"
	// +optional
	Interval string `json:"interval,omitempty"`

	// VerifyBeforeSend when true causes the announcer to verify it still
	// owns the address (won the election) before sending each GARP packet.
	// This prevents announcing addresses during rapid failover scenarios.
	// +kubebuilder:default=true
	// +optional
	VerifyBeforeSend *bool `json:"verifyBeforeSend,omitempty"`
}

// AddressConfig specifies how IP addresses should be configured on different
// interface types.
type AddressConfig struct {
	// LocalInterface configures addresses on the local interface (e.g., eth0).
	// +optional
	LocalInterface *InterfaceAddressConfig `json:"localInterface,omitempty"`

	// DummyInterface configures addresses on the dummy interface (e.g., kube-lb0).
	// +optional
	DummyInterface *InterfaceAddressConfig `json:"dummyInterface,omitempty"`
}

// InterfaceAddressConfig specifies address configuration for an interface type.
type InterfaceAddressConfig struct {
	// ValidLifetime is the valid lifetime in seconds for addresses added to this
	// interface. A value of 0 means permanent (no expiry). When non-zero, addresses
	// will not have the IFA_F_PERMANENT flag, which prevents CNI plugins like
	// Flannel from incorrectly selecting VIPs as node addresses.
	// Minimum value when non-zero is 60 seconds.
	// Default: 300 for local interface, 0 for dummy interface.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ValidLifetime *int `json:"validLifetime,omitempty"`

	// PreferredLifetime is the preferred lifetime in seconds. Must be <= ValidLifetime.
	// A value of 0 means permanent. Defaults to ValidLifetime if not specified.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PreferredLifetime *int `json:"preferredLifetime,omitempty"`

	// NoPrefixRoute when true prevents the kernel from automatically creating
	// a prefix route for the address.
	// Default: true for local interface, false for dummy interface.
	// +optional
	NoPrefixRoute *bool `json:"noPrefixRoute,omitempty"`
}

// LBNodeAgentStatus contains runtime information about the node agent.
type LBNodeAgentStatus struct {
	// ActiveLeases is the number of active election leases this node holds.
	// +optional
	ActiveLeases int `json:"activeLeases,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LBNodeAgentList holds a list of LBNodeAgent.
type LBNodeAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []LBNodeAgent `json:"items"`
}
