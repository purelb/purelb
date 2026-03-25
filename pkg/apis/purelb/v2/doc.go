// Copyright 2020 Acnodal, Inc.
// Copyright 2024 Acnodal, Inc.
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

// +k8s:deepcopy-gen=package
// +groupName=purelb.io

// Package v2 is the v2 version of the PureLB API.
//
// Key changes from v1:
//   - ServiceGroup: Local/Remote/Netbox are now mutually exclusive fields
//   - LBNodeAgent: Renamed fields for clarity, integrated GARPConfig
//   - Added skipIPv6DAD option for ServiceGroup local pools
package v2 // import "purelb.io/pkg/apis/purelb/v2"
