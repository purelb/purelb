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
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var sliceSizes = []int32{64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384}

func TestSliceCapacity(t *testing.T) {
	assert.Equal(t, 15, sliceCapacity(4096, nil, nil), "4096 serves 15 pod nodes")
	assert.Equal(t, 1008, sliceCapacity(64, nil, nil), "the CRD's MaxItems")
	assert.Equal(t, 3, sliceCapacity(16384, nil, nil))
	for _, size := range sliceSizes {
		assert.Equal(t, int((65536-1024)/size), sliceCapacity(size, nil, nil), "size %d", size)
	}
	// A Service port takes its block; a Draining slice counts until freed;
	// a reserve costs nothing.
	assert.Equal(t, 14, sliceCapacity(4096, []int32{5060}, nil))
	assert.Equal(t, 13, sliceCapacity(4096, []int32{5060}, []purelbv2.EgressSlice{
		{Index: 3, Node: "n1", State: sliceDraining}, {Index: 4, State: sliceActive}}))
}

func TestPlanSlicesExamples(t *testing.T) {
	want := func(ns ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range ns {
			m[n] = true
		}
		return m
	}

	// A new address: one Pending slice per pod node, then the reserve.
	p := planSlices(sliceInput{size: 4096, want: want("b", "a"), reserve: 2})
	assert.Equal(t, []purelbv2.EgressSlice{
		{Index: 1, Node: "a", State: slicePending}, {Index: 2, Node: "b", State: slicePending},
		{Index: 3, State: sliceActive}, {Index: 4, State: sliceActive}}, p.slices)

	// A claim takes the lowest reserve, which is replenished.
	p = planSlices(sliceInput{size: 4096, current: p.slices, want: want("a", "b", "c"), reserve: 2})
	assert.Equal(t, []purelbv2.EgressSlice{
		{Index: 1, Node: "a", State: slicePending}, {Index: 2, Node: "b", State: slicePending},
		{Index: 3, Node: "c", State: sliceActive}, {Index: 4, State: sliceActive},
		{Index: 5, State: sliceActive}}, p.slices)

	// A node with no pods drains; it returns before being freed and is
	// Active again; freed, it is gone.
	cur := []purelbv2.EgressSlice{{Index: 1, Node: "a", State: sliceActive}}
	p = planSlices(sliceInput{size: 4096, current: cur, want: want()})
	assert.Equal(t, []purelbv2.EgressSlice{{Index: 1, Node: "a", State: sliceDraining}}, p.slices)
	assert.Equal(t, cur, planSlices(sliceInput{size: 4096, current: p.slices, want: want("a")}).slices)
	assert.Empty(t, planSlices(sliceInput{size: 4096, current: p.slices, freed: map[int32]bool{1: true}}).slices)

	// A Pending slice never carried flows: it is removed, not drained.
	assert.Empty(t, planSlices(sliceInput{size: 4096,
		current: []purelbv2.EgressSlice{{Index: 1, Node: "a", State: slicePending}}}).slices)

	// A Service port added inside an Active slice drains it and allocates
	// a replacement; the reserve in the blocked block is dropped.
	p = planSlices(sliceInput{size: 4096, want: want("a"), ports: []int32{5060}, current: []purelbv2.EgressSlice{
		{Index: 1, Node: "a", State: sliceActive}, {Index: 2, State: sliceActive}}})
	assert.Equal(t, []purelbv2.EgressSlice{
		{Index: 1, Node: "a", State: sliceDraining}, {Index: 2, Node: "a", State: sliceActive}}, p.slices)

	// Full: the node is reported, and no reserve is kept.
	full := planSlices(sliceInput{size: 16384, want: want("a", "b", "c", "d"), reserve: 4})
	assert.Equal(t, []string{"d"}, full.exhausted)
	assert.Len(t, full.slices, 3)

	// Over the slot budget: reported, nothing allocated.
	p = planSlices(sliceInput{size: 4096, want: want("a"), slotOK: func(string) bool { return false }})
	assert.Equal(t, []string{"a"}, p.overBudget)
	assert.Empty(t, p.slices)

	// The configured reserve shrank: the highest reserves go.
	p = planSlices(sliceInput{size: 4096, reserve: 1, current: []purelbv2.EgressSlice{
		{Index: 1, State: sliceActive}, {Index: 2, State: sliceActive}, {Index: 3, State: sliceActive}}})
	assert.Equal(t, []purelbv2.EgressSlice{{Index: 1, State: sliceActive}}, p.slices)
}

// randomSliceInput builds a valid current state: unique in-range indices
// and at most one live slice per node.
func randomSliceInput(r *rand.Rand) sliceInput {
	size := sliceSizes[r.IntN(len(sliceSizes))]
	first, last := sliceRange(size)
	nodes := r.IntN(12)
	in := sliceInput{size: size, want: map[string]bool{}, freed: map[int32]bool{}, reserve: r.IntN(7)}
	used, hasLive := map[int32]bool{}, map[string]bool{}
	for i := r.IntN(int(min(last-first+1, 20))); i > 0; i-- {
		idx := first + r.Int32N(last-first+1)
		if used[idx] {
			continue
		}
		used[idx] = true
		s := purelbv2.EgressSlice{Index: idx, State: sliceActive}
		if r.IntN(4) > 0 && nodes > 0 {
			s.Node = fmt.Sprintf("n%d", r.IntN(nodes))
			s.State = []string{slicePending, sliceActive, sliceDraining}[r.IntN(3)]
			if sliceLive(s) && hasLive[s.Node] {
				s.State = sliceDraining
			}
			hasLive[s.Node] = hasLive[s.Node] || sliceLive(s)
			if s.State == sliceDraining && r.IntN(3) == 0 {
				in.freed[idx] = true
			}
		}
		in.current = append(in.current, s)
	}
	for i := 0; i < nodes; i++ {
		if r.IntN(3) > 0 {
			in.want[fmt.Sprintf("n%d", i)] = true
		}
	}
	for i := r.IntN(4); i > 0; i-- {
		in.ports = append(in.ports, 1+r.Int32N(65535))
	}
	if r.IntN(4) == 0 {
		denied := fmt.Sprintf("n%d", r.IntN(max(nodes, 1)))
		in.slotOK = func(n string) bool { return n != denied }
	}
	return in
}

func TestPlanSlicesProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 5000; iter++ {
		in := randomSliceInput(r)
		p := planSlices(in)
		ctx := fmt.Sprintf("iter %d input %+v\nplan %+v", iter, in, p)
		first, last := sliceRange(in.size)

		liveOf := map[string][]int32{}
		reserves := 0
		seen := map[int32]bool{}
		for i, s := range p.slices {
			require.True(t, s.Index >= first && s.Index <= last, "in range: %s", ctx)
			require.False(t, seen[s.Index], "unique: %s", ctx)
			seen[s.Index] = true
			if i > 0 {
				require.Less(t, p.slices[i-1].Index, s.Index, "ordered: %s", ctx)
			}
			require.False(t, in.freed[s.Index], "freed is gone: %s", ctx)
			if s.Node == "" {
				reserves++
				require.Equal(t, sliceActive, s.State, "a reserve is Active: %s", ctx)
			}
			if sliceLive(s) {
				require.False(t, covers(in.size, s.Index, in.ports), "no live slice holds a port: %s", ctx)
				if s.Node != "" {
					liveOf[s.Node] = append(liveOf[s.Node], s.Index)
				}
			}
		}

		reported := map[string]bool{}
		for _, n := range append(append([]string{}, p.exhausted...), p.overBudget...) {
			require.False(t, reported[n], "reported once: %s", ctx)
			reported[n] = true
		}
		for n := range liveOf {
			require.True(t, in.want[n], "only wanted nodes hold live slices: %s", ctx)
			require.Len(t, liveOf[n], 1, "one live slice per node: %s", ctx)
			require.False(t, reported[n], "a node with a slice isn't reported: %s", ctx)
		}
		for n := range in.want {
			require.True(t, len(liveOf[n]) == 1 || reported[n], "every wanted node is served or reported: %s", ctx)
		}

		hasFree := false
		for i := first; i <= last; i++ {
			hasFree = hasFree || (!seen[i] && !in.freed[i] && !covers(in.size, i, in.ports))
		}
		require.LessOrEqual(t, reserves, in.reserve, "reserve bounded: %s", ctx)
		if reserves < in.reserve {
			require.False(t, hasFree, "the reserve is short only when full: %s", ctx)
		}
		if len(p.exhausted) > 0 {
			require.False(t, hasFree, "exhausted only when full: %s", ctx)
			require.Zero(t, reserves, "claims take reserves first: %s", ctx)
		}

		// Nothing in use is dropped or moved: a Draining slice stays until
		// freed, and a live slice of a still-wanted node keeps its index.
		for _, s := range in.current {
			if in.freed[s.Index] || s.Node == "" {
				continue
			}
			kept := false
			for _, o := range p.slices {
				kept = kept || (o.Index == s.Index && o.Node == s.Node)
			}
			if s.State == sliceDraining || (s.State == sliceActive && !covers(in.size, s.Index, in.ports)) {
				require.True(t, kept, "slice %+v kept: %s", s, ctx)
			}
		}

		// A plan is a fixed point.
		again := planSlices(sliceInput{size: in.size, current: p.slices, want: in.want, ports: in.ports,
			reserve: in.reserve, freed: in.freed, slotOK: in.slotOK})
		require.Equal(t, p, again, "fixed point: %s", ctx)
	}
}
