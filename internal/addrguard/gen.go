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

// Package addrguard drops packets addressed to PureLB VIPs unless their
// (protocol, port) is one of the Service's ports. See bpf/addrguard.c.
package addrguard

// Regenerate with `make bpf`, which runs this inside the pinned builder
// image (hack/bpf-builder). The generated object is committed; CI rebuilds
// it and fails on any byte difference.
//go:generate go tool bpf2go -target bpfel -type port_key -type vip_counters -type guard_config addrguard bpf/addrguard.c -- -I/usr/include/x86_64-linux-gnu -Wall -Werror
