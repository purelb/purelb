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
// local pool) or an EPIC configuration (to get addresses from
// Acnodal's EPIC). For examples, see the "config/" directory in the
// PureLB source tree.
type ServiceGroupSpec struct {
	Local  *ServiceGroupLocalSpec  `json:"local"`
	EPIC   *ServiceGroupEPICSpec   `json:"epic"`
	Netbox *ServiceGroupNetboxSpec `json:"netbox"`
}

// ServiceGroupLocalSpec configures the allocator to manage a pool of
// IP addresses locally. The Pool can be specified as a CIDR or as a
// from-to range of addresses,
// e.g. 'fd53:9ef0:8683::-fd53:9ef0:8683::3'. The subnet is specified
// with CIDR notation, e.g., 'fd53:9ef0:8683::/120'. All of the
// addresses in the Pool must be contained within the
// Subnet. Aggregation is currently unused.
type ServiceGroupLocalSpec struct {
	Subnet      string `json:"subnet"`
	Pool        string `json:"pool"`
	Aggregation string `json:"aggregation"`
}

// ServiceGroupEPICSpec configures the allocator to work with the
// Acnodal Enterprise GateWay. The URL is the base URL of the service
// group on the EPIC. Aggregation is currently unused.
type ServiceGroupEPICSpec struct {
	URL         string `json:"url"`
	Aggregation string `json:"aggregation"`
	WSUsername  string `json:"ws-username"`
	WSPassword  string `json:"ws-password"`
}

// ServiceGroupNetboxSpec configures the allocator to request
// addresses from a Netbox IPAM system.
type ServiceGroupNetboxSpec struct {
	URL         string `json:"url"`
	Tenant      string `json:"tenant"`
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

// LBNodeAgentSpec configures the node agents.  It will have one of
// either a Local configuration (to announce service addresses
// locally) or an EPIC configuration (to announce service addresses to
// Acnodal's EPIC). For examples, see the "config/" directory in the
// PureLB source tree.
type LBNodeAgentSpec struct {
	Local *LBNodeAgentLocalSpec `json:"local"`
	EPIC  *LBNodeAgentEPICSpec  `json:"epic"`
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

// PFCAttachment contains the configuration data that lets us attach
// our packet forwarding components to a network interface.
type PFCAttachment struct {
	// Interface is the name of the interface.
	Interface string `json:"interface"`

	// Direction is either "ingress" or "egress".
	Direction string `json:"direction"`

	// Flags configure the PFC component's behavior.
	Flags int `json:"flags"`

	// QID is a magic parameter that the PFC needs.
	QID int `json:"qid"`
}

// LBNodeAgentEPICSpec configures the announcers to announce service
// addresses to the Acnodal Enterprise GateWay.
type LBNodeAgentEPICSpec struct {
	// EncapAttachment configures how the agent will attach the Packet
	// Forwarding Components for packet encapsulation.
	EncapAttachment PFCAttachment `json:"encapAttachment"`

	// DecapAttachment configures how the agent will attach the Packet
	// Forwarding Components for packet decapsulation.
	DecapAttachment PFCAttachment `json:"decapAttachment"`
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
