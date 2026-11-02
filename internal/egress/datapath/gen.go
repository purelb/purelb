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

// Package datapath is egress's BPF programs and maps: the announcer stage
// (pl_announce) and their typed map entries. See bpf/egress.c.
package datapath

// Regenerate with `make bpf`, which runs this inside the pinned builder
// image (hack/bpf-builder). The generated object is committed; CI rebuilds
// it and fails on any byte difference.
//go:generate go tool bpf2go -target bpfel -type addr_key -type addr_entry -type slice_key -type port_key -type frag_key -type node -type underlay_key -type pool_if -type holders -type egress_config -type pod_dev -type lpm4 -type lpm6 egress bpf/egress.c -- -I/usr/include/x86_64-linux-gnu -Wall -Werror
