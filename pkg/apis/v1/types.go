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

type ServiceGroupSpec struct {
	Local *ServiceGroupLocalSpec `json:"local"`
	EGW   *ServiceGroupEGWSpec   `json:"egw"`
}

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

type LBNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LBNodeSpec   `json:"spec"`
	Status LBNodeStatus `json:"status"`
}

type LBNodeSpec struct {
	LocalInterface string `json:"localinterface"`
	ExtLBInterface string `json:"extlbinterface"`
}

type LBNodeStatus struct {
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ServiceGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []ServiceGroup `json:"items"`
}

type LBNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []LBNode `json:"items"`
}
