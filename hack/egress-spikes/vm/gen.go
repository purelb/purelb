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

// Package spikes holds the Phase 0 egress feasibility spikes. Not
// production code: see README.md.
package spikes

//go:generate go tool bpf2go -target bpfel -type peer -type steer -type addr_key -type addr_cfg -type slice_key -type pod_dev -type config core bpf/egress_core.c -- -I/usr/include/x86_64-linux-gnu -Wall -Werror
