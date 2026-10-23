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

package allocator

import (
	"sort"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Slice states.
const (
	slicePending  = "Pending"
	sliceActive   = "Active"
	sliceDraining = "Draining"
)

const (
	// firstPort is the lowest port a slice may cover.
	firstPort = 1024

	// defaultReserveSlices applies when the group doesn't set one (D17).
	defaultReserveSlices = 4

	// slotBudget is each node's slots, shared by slices and pod-set ids
	// (D18).
	slotBudget = 127
)

// sliceInput is everything planSlices needs for one address.
type sliceInput struct {
	size    int32
	current []purelbv2.EgressSlice

	// want are the pod nodes that need a slice.
	want map[string]bool

	// ports are the Service and reserved ports on the address; no live
	// slice may cover one.
	ports []int32

	// reserve is how many unassigned slices to keep.
	reserve int

	// freed are slice indices to remove: Draining slices their node
	// reported drained or that reached the maximum, and a gone node's.
	freed map[int32]bool

	// slotOK reports whether node has a slot left for a new slice.
	slotOK func(node string) bool
}

// slicePlan is planSlices' result.
type slicePlan struct {
	slices []purelbv2.EgressSlice // ordered by index

	// exhausted are wanted nodes left without a slice for lack of free
	// slices; overBudget for lack of a slot.
	exhausted, overBudget []string
}

// sliceRange is the indices whose ports lie in [firstPort, 65535].
func sliceRange(size int32) (first, last int32) {
	return (firstPort + size - 1) / size, 65536/size - 1
}

// covers reports whether slice index of size holds any of ports.
func covers(size, index int32, ports []int32) bool {
	lo, hi := index*size, (index+1)*size-1
	for _, p := range ports {
		if p >= lo && p <= hi {
			return true
		}
	}
	return false
}

// sliceLive reports whether a slice can carry new flows for its node.
func sliceLive(s purelbv2.EgressSlice) bool {
	return s.State == slicePending || s.State == sliceActive
}

// planSlices computes an address's slices (§5.6): each wanted node keeps
// its slice or gets one, taking an unassigned reserve slice first, then the
// lowest free index; a node no longer wanted, or whose slice now holds a
// Service or reserved port, drains; the reserve is topped up only from
// slices still free. An in-use slice never moves.
func planSlices(in sliceInput) slicePlan {
	first, last := sliceRange(in.size)
	used := map[int32]bool{}
	for i := range in.freed {
		used[i] = true // reusable once holders ack the generation freeing it
	}
	has := map[string]bool{} // wanted nodes with a live slice
	var out []purelbv2.EgressSlice

	// Live slices first, so a node keeps its live slice over reviving a
	// Draining one.
	cur := append([]purelbv2.EgressSlice(nil), in.current...)
	sort.SliceStable(cur, func(i, j int) bool { return sliceLive(cur[i]) && !sliceLive(cur[j]) })
	for _, s := range cur {
		if used[s.Index] || s.Index < first || s.Index > last {
			continue
		}
		blocked := covers(in.size, s.Index, in.ports)
		switch {
		case s.Node == "":
			if blocked || s.State != sliceActive {
				continue // a reserve carries no flows: drop it
			}
		case s.State == slicePending && (blocked || !in.want[s.Node] || has[s.Node]):
			continue // never activated: no flows
		case sliceLive(s) && (blocked || !in.want[s.Node] || has[s.Node]):
			s.State = sliceDraining
		case s.State == sliceDraining && !blocked && in.want[s.Node] && !has[s.Node]:
			s.State = sliceActive // a selected pod returned
		}
		used[s.Index] = true
		if s.Node != "" && sliceLive(s) {
			has[s.Node] = true
		}
		out = append(out, s)
	}

	free := func() (int32, bool) {
		for i := first; i <= last; i++ {
			if !used[i] && !covers(in.size, i, in.ports) {
				return i, true
			}
		}
		return 0, false
	}

	var plan slicePlan
	nodes := make([]string, 0, len(in.want))
	for n, w := range in.want {
		if w && !has[n] {
			nodes = append(nodes, n)
		}
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		if in.slotOK != nil && !in.slotOK(n) {
			plan.overBudget = append(plan.overBudget, n)
			continue
		}
		if r := lowestReserve(out); r >= 0 {
			out[r].Node = n // a claim: the reserve is Active already
			continue
		}
		i, ok := free()
		if !ok {
			plan.exhausted = append(plan.exhausted, n)
			continue
		}
		used[i] = true
		out = append(out, purelbv2.EgressSlice{Index: i, Node: n, State: slicePending})
	}

	reserves := 0
	for _, s := range out {
		if s.Node == "" {
			reserves++
		}
	}
	for ; reserves < in.reserve; reserves++ {
		i, ok := free()
		if !ok {
			break
		}
		used[i] = true
		out = append(out, purelbv2.EgressSlice{Index: i, State: sliceActive})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	for reserves > in.reserve { // the configured reserve shrank: drop the highest
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Node == "" {
				out = append(out[:i], out[i+1:]...)
				break
			}
		}
		reserves--
	}
	if len(out) > 0 {
		plan.slices = out
	}
	return plan
}

// lowestReserve is the index in slices of the lowest-index reserve, or -1.
func lowestReserve(slices []purelbv2.EgressSlice) int {
	r := -1
	for i, s := range slices {
		if s.Node == "" && (r < 0 || s.Index < slices[r].Index) {
			r = i
		}
	}
	return r
}

// sliceCapacity is the address's usable slices (D28): the slices in
// range, less those holding a Service or reserved port, less those still
// Draining. Reserves cost nothing.
func sliceCapacity(size int32, ports []int32, slices []purelbv2.EgressSlice) int {
	first, last := sliceRange(size)
	n := 0
	for i := first; i <= last; i++ {
		if !covers(size, i, ports) {
			n++
		}
	}
	for _, s := range slices {
		if s.State == sliceDraining && !covers(size, s.Index, ports) {
			n--
		}
	}
	return n
}
