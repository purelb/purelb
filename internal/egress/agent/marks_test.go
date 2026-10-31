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

package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"purelb.io/internal/egress/datapath"
)

func TestChooseByte(t *testing.T) {
	after := func(where string, bits uint32) datapath.MarkUse {
		return datapath.MarkUse{Where: where, Sees: datapath.SeesAfter, Mark: bits}
	}
	kubeProxy := after("ip kube-proxy masquerading", 0x4000)
	low := after("ip cni post", 0x1)
	third := after("ip cni third", 0x00010000)
	top := after("ip cni top", 0x01000000)
	before := datapath.MarkUse{Where: "ip cni pre", Sees: datapath.SeesBefore, MarkSet: ^uint32(0)}
	for _, tc := range []struct {
		name      string
		uses      []datapath.MarkUse
		spec      string
		cur       uint8
		tag       bool
		shift     uint8
		conflicts []datapath.MarkUse
	}{
		{"default free", []datapath.MarkUse{kubeProxy}, "", 0, false, 0, nil},
		{"moved to the first free candidate", []datapath.MarkUse{low}, "", 0, false, 16, nil},
		{"candidates in order", []datapath.MarkUse{low, third}, "", 0, false, 24, nil},
		{"kube-proxy's byte last", []datapath.MarkUse{low, third, top}, "", 0, false, 8, nil},
		{"none free: kept", []datapath.MarkUse{low, third, top, kubeProxy}, "", 0, false, 0, []datapath.MarkUse{low}},
		{"in use and free: kept", []datapath.MarkUse{third}, "", 24, false, 24, nil},
		{"explicit: never moved", []datapath.MarkUse{kubeProxy}, "0x0000ff00", 0, false, 8, []datapath.MarkUse{kubeProxy}},
		{"explicit and free", []datapath.MarkUse{kubeProxy}, "0x00ff0000", 0, false, 16, nil},
		{"before classify, source-IP node", []datapath.MarkUse{before}, "", 0, false, 0, nil},
		{"before classify, tag node: none free", []datapath.MarkUse{before}, "", 0, true, 0, []datapath.MarkUse{before}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shift, conflicts := chooseByte(tc.uses, tc.spec, tc.cur, tc.tag)
			assert.Equal(t, tc.shift, shift)
			assert.Equal(t, tc.conflicts, conflicts)
		})
	}
}

func TestStepForward(t *testing.T) {
	t1, t2 := &datapath.Table{}, &datapath.Table{}
	at := time.Unix(1000, 0)
	tick := func(d time.Duration) time.Time { at = at.Add(d); return at }
	flows := func(m, tr uint64) datapath.SlotFlows { return datapath.SlotFlows{Marked: m, Translated: tr} }
	var w fwdWindow
	assert.False(t, stepForward(&w, t1, at, flows(5, 5)), "a first window starts")
	assert.False(t, stepForward(&w, t1, tick(time.Second), flows(9, 5)), "not over yet")
	assert.True(t, stepForward(&w, t1, tick(forwardWindow), flows(9, 5)))
	assert.Equal(t, 1, w.blocked, "marked, none translated")
	assert.True(t, stepForward(&w, t1, tick(forwardWindow), flows(12, 5)))
	assert.Equal(t, 2, w.blocked)
	// A rewritten table's counters start again: a new window, the count
	// kept.
	assert.False(t, stepForward(&w, t2, tick(time.Second), flows(1, 0)))
	assert.Equal(t, fwdWindow{table: t2, start: at, marked: 1, blocked: 2}, w)
	assert.True(t, stepForward(&w, t2, tick(forwardWindow), flows(3, 0)))
	assert.Equal(t, 3, w.blocked)
	assert.True(t, stepForward(&w, t2, tick(forwardWindow), flows(4, 1)))
	assert.Equal(t, 0, w.blocked, "one translated")
	assert.True(t, stepForward(&w, t2, tick(forwardWindow), flows(5, 1)))
	assert.True(t, stepForward(&w, t2, tick(forwardWindow), flows(5, 1)))
	assert.Equal(t, 0, w.blocked, "no traffic: no evidence")
}

func TestConflictMessage(t *testing.T) {
	uses := []datapath.MarkUse{
		{Where: "legacy ip mangle X", Mark: 0xff0000ff, CtSet: 0xff00},
		{Where: "ip kube-proxy masquerading", Mark: 0x4000, MarkSet: 0x4000},
	}
	assert.Equal(t, "markField 0x0000ff00 is used by other rules: ip kube-proxy masquerading (matches mark 0x4000, sets mark 0x4000); "+
		"legacy ip mangle X (sets ct mark 0xff00)", conflictMessage(8, uses))
	assert.Equal(t, "0x00ff0000", fieldName(16))
}
