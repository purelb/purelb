// Copyright 2020 Acnodal, Inc.
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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceGroup is the top-level custom resource for configuring
// service groups. It contains the usual CRD metadata, and the service
// group spec and status.
type ServiceGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceGroupSpec   `json:"spec"`
	Status ServiceGroupStatus `json:"status"`
}

// ServiceGroupSpec configures the allocator.  It will have one of
// either a Local configuration (to allocate service addresses from a
// local pool) or a Netbox configuration (to get addresses from the
// Netbox IPAM). For examples, see the "config/" directory in the
// PureLB source tree.
type ServiceGroupSpec struct {
	// +optional
	Local *ServiceGroupLocalSpec `json:"local,omitempty"`
	// +optional
	Netbox *ServiceGroupNetboxSpec `json:"netbox,omitempty"`
}

// ServiceGroupLocalSpec configures the allocator to manage pools of
// IP addresses locally. Pools can be specified as a CIDR or as a
// from-to range of addresses,
// e.g. 'fd53:9ef0:8683::-fd53:9ef0:8683::3'. The subnet is specified
// with CIDR notation, e.g., 'fd53:9ef0:8683::/120'. All of the
// addresses in the Pool must be contained within the
// Subnet. Aggregation is currently unused.
//
// The Subnet, Pool, and Aggregation fields are a legacy from the
// pre-dualStack days when a service could have only one IP
// address. If you're running a single-stack environment then they're
// still valid, but V4Pool and V6Pool are preferred. The V4Pool and
// V6Pool fields allow you to configure pools of both IPV4 and IPV6
// addresses to support dual-stack and you can also use them in a
// single-stack environment.
type ServiceGroupLocalSpec struct {
	Subnet      string `json:"subnet"`
	Pool        string `json:"pool"`
	Aggregation string `json:"aggregation"`

	V4Pool *ServiceGroupAddressPool `json:"v4pool,omitempty"`
	V6Pool *ServiceGroupAddressPool `json:"v6pool,omitempty"`
}

// BestPool returns this Spec's address pool. The order of precedence
// is V6, then V4, and the top-level Pool last
func (s ServiceGroupLocalSpec) BestPool() (pool string) {
	// Figure out which pool contains a useful range.
	pool = s.Pool
	if s.V6Pool != nil {
		pool = s.V6Pool.Pool
	} else if s.V4Pool != nil {
		pool = s.V4Pool.Pool
	}

	return
}

// ServiceGroupNetboxSpec configures the allocator to request
// addresses from a Netbox IPAM system.
type ServiceGroupNetboxSpec struct {
	URL         string `json:"url"`
	Tenant      string `json:"tenant"`
	Aggregation string `json:"aggregation"`
}

// ServiceGroupAddressPool specifies a pool of addresses that belong
// to a ServiceGroupLocalSpec.
type ServiceGroupAddressPool struct {
	// Pool specifies a pool of addresses that PureLB manages. It can be
	// a CIDR or a from-to range of addresses, e.g.,
	// 'fd53:9ef0:8683::-fd53:9ef0:8683::3'.
	Pool string `json:"pool"`

	// Subnet specifies the subnet that contains all of the addresses in
	// the Pool. It's specified with CIDR notation, e.g.,
	// 'fd53:9ef0:8683::/120'. All of the addresses in the Pool must be
	// contained within the Subnet.
	Subnet string `json:"subnet"`

	// Aggregation changes the address mask of the allocated address
	// from the subnet mask to the specified mask. It can be "default"
	// or an integer in the range 8-128.
	Aggregation string `json:"aggregation"`
}

// ServiceGroupStatus is currently unused.
type ServiceGroupStatus struct {
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LBNodeAgent is the top-level custom resource for configuring node
// agents. It contains the usual CRD metadata, and the agent spec and
// status.
type LBNodeAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LBNodeAgentSpec   `json:"spec"`
	Status LBNodeAgentStatus `json:"status"`
}

// LBNodeAgentSpec configures the node agents.  It will have one Local
// configuration to announce service addresses locally. For examples,
// see the "config/" directory in the PureLB source tree.
type LBNodeAgentSpec struct {
	Local *LBNodeAgentLocalSpec `json:"local"`
}

// LBNodeAgentLocalSpec configures the announcers to announce service
// addresses by configuring the underlying operating
// system. LocalInterface is unimplemented but will be optional. If it
// is not provided then the agents will add the service address to
// whichever interface carries the default route. ExtLBInterface is
// also unimplemented.
type LBNodeAgentLocalSpec struct {
	// LocalInterface allows the user to specify the interface to use
	// for announcement of local addresses. This field is optional -
	// PureLB by default will use the interface that has the default
	// route, which works in most cases.
	LocalInterface string `json:"localint"`

	// ExtLBInterface specifies the name of the interface to use for
	// announcement of non-local routes. This field is optional - the
	// default is "kube-lb0" which works in most cases.
	ExtLBInterface string `json:"extlbint"`
}

// LBNodeAgentStatus is currently unused.
type LBNodeAgentStatus struct {
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceGroupList holds a list of ServiceGroup.
type ServiceGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []ServiceGroup `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LBNodeAgentList holds a list of LBNodeAgent.
type LBNodeAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []LBNodeAgent `json:"items"`
}
