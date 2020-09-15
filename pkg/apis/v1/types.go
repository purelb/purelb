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

type ServiceGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceGroupSpec   `json:"spec"`
	Status ServiceGroupStatus `json:"status"`
}

// ServiceGroupSpec configures the allocator.  It will have one of
// either a Local configuration (to allocate service addresses from a
// local pool) or an EGW configuration (to get addresses from
// Acnodal's EGW). For examples, see the "config/" directory in the
// PureLB source tree.
type ServiceGroupSpec struct {
	Local      *ServiceGroupLocalSpec `json:"local"`
	EGW        *ServiceGroupEGWSpec   `json:"egw"`
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

type ServiceGroupEGWSpec struct {
	URL         string `json:"url"`
	Aggregation string `json:"aggregation"`
}

type ServiceGroupStatus struct {
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type LBNodeAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LBNodeAgentSpec   `json:"spec"`
	Status LBNodeAgentStatus `json:"status"`
}

// LBNodeAgentSpec configures the node agents.  It will have one of
// either a Local configuration (to announce service addresses
// locally) or an EGW configuration (to announce service addresses to
// Acnodal's EGW). For examples, see the "config/" directory in the
// PureLB source tree.
type LBNodeAgentSpec struct {
	Local *LBNodeAgentLocalSpec `json:"local"`
	EGW   *LBNodeAgentEGWSpec   `json:"egw"`
}

// LBNodeAgentLocalSpec configures the announcers to announce service
// addresses by configuring the underlying operating
// system. LocalInterface is unimplemented but will be optional. If it
// is not provided then the agents will add the service address to
// whichever interface carries the default route. ExtLBInterface is
// also unimplemented.
type LBNodeAgentLocalSpec struct {
	LocalInterface string `json:"localint"`
	ExtLBInterface string `json:"extlbint"`
}

type LBNodeAgentEGWSpec struct {
}

type LBNodeAgentStatus struct {
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ServiceGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []ServiceGroup `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type LBNodeAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []LBNodeAgent `json:"items"`
}
