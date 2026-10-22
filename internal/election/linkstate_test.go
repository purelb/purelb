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

package election

import (
	"testing"
	"time"

	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// testDamper returns a LinkDamper on a clock the test moves, recording the
// delays it schedules renewals for and counting the renewals asked for.
type testDamper struct {
	*LinkDamper
	clock     time.Time
	scheduled []time.Duration
	renews    int
}

func newTestDamper() *testDamper {
	td := &testDamper{clock: time.Unix(1000, 0)}
	td.LinkDamper = NewLinkDamper(2*time.Second, 10*time.Second, nil)
	td.now = func() time.Time { return td.clock }
	td.afterFunc = func(d time.Duration, _ func()) { td.scheduled = append(td.scheduled, d) }
	td.SetRenew(func() { td.renews++ })
	return td
}

// up asks whether a link named name reporting state counts as up, at
// offset after the test's start.
func (td *testDamper) up(name string, state netlink.LinkOperState, offset time.Duration) bool {
	td.clock = time.Unix(1000, 0).Add(offset)
	return td.Up(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name, OperState: state}})
}

func TestLinkDamperFirstSight(t *testing.T) {
	td := newTestDamper()
	assert.True(t, td.up("a", netlink.OperUp, 0))
	assert.False(t, td.up("b", netlink.OperDown, 0), "a link down when first seen counts as down at once")
	assert.True(t, td.up("c", netlink.OperUnknown, 0), "UNKNOWN counts as up")
	assert.False(t, td.up("d", netlink.OperLowerLayerDown, 0))
	assert.Empty(t, td.scheduled)
}

// A link that goes down leaves after the down hold-down; it comes back
// after the longer up hold-down, and each change is counted once.
func TestLinkDamperHoldDowns(t *testing.T) {
	td := newTestDamper()
	downs := ptu.ToFloat64(linkChanges.WithLabelValues("eth9", "down"))
	ups := ptu.ToFloat64(linkChanges.WithLabelValues("eth9", "up"))

	assert.True(t, td.up("eth9", netlink.OperUp, 0))

	assert.True(t, td.up("eth9", netlink.OperDown, 10*time.Second), "held up during the down hold-down")
	assert.Equal(t, []time.Duration{2 * time.Second}, td.scheduled, "a renewal is scheduled for the hold-down's end")
	assert.True(t, td.up("eth9", netlink.OperDown, 11*time.Second))
	assert.Len(t, td.scheduled, 1, "one renewal per pending change")
	assert.False(t, td.up("eth9", netlink.OperDown, 12*time.Second), "down once the hold-down has passed")
	assert.Equal(t, downs+1, ptu.ToFloat64(linkChanges.WithLabelValues("eth9", "down")))

	assert.False(t, td.up("eth9", netlink.OperUp, 20*time.Second), "held down during the up hold-down")
	assert.Equal(t, 10*time.Second, td.scheduled[1])
	assert.False(t, td.up("eth9", netlink.OperUp, 29*time.Second))
	assert.True(t, td.up("eth9", netlink.OperUp, 30*time.Second))
	assert.Equal(t, ups+1, ptu.ToFloat64(linkChanges.WithLabelValues("eth9", "up")))
	assert.Equal(t, downs+1, ptu.ToFloat64(linkChanges.WithLabelValues("eth9", "down")))
}

// A blip shorter than the hold-down never reaches the lease, and a flap
// during the up hold-down restarts it.
func TestLinkDamperFlap(t *testing.T) {
	td := newTestDamper()
	downs := ptu.ToFloat64(linkChanges.WithLabelValues("eth8", "down"))

	assert.True(t, td.up("eth8", netlink.OperUp, 0))
	assert.True(t, td.up("eth8", netlink.OperDown, 1*time.Second))
	assert.True(t, td.up("eth8", netlink.OperUp, 2*time.Second), "back before the hold-down ended")
	assert.True(t, td.up("eth8", netlink.OperUp, 10*time.Second))
	assert.Equal(t, downs, ptu.ToFloat64(linkChanges.WithLabelValues("eth8", "down")), "a blip isn't counted")

	td = newTestDamper()
	assert.False(t, td.up("eth7", netlink.OperDown, 0))
	assert.False(t, td.up("eth7", netlink.OperUp, 1*time.Second))
	assert.False(t, td.up("eth7", netlink.OperDown, 5*time.Second), "flapped down again")
	assert.False(t, td.up("eth7", netlink.OperUp, 6*time.Second))
	assert.False(t, td.up("eth7", netlink.OperUp, 15*time.Second), "the up hold-down restarted at 6s")
	assert.True(t, td.up("eth7", netlink.OperUp, 16*time.Second))
}

func TestLinkDamperNilAndScanned(t *testing.T) {
	var d *LinkDamper
	assert.True(t, d.Up(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "x", OperState: netlink.OperDown}}))
	d.setScanned([]string{"x"}) // no panic

	td := newTestDamper()
	assert.False(t, td.isScanned("eth0"))
	td.setScanned([]string{"eth0", "eth1"})
	assert.True(t, td.isScanned("eth1"))
	assert.False(t, td.isScanned("veth123"))
}

// GetLocalSubnets leaves out a link the damper counts as down, and
// records the links it scanned.
func TestGetLocalSubnetsDamped(t *testing.T) {
	td := newTestDamper()
	subnets, err := GetLocalSubnets([]string{"lo"}, false, td.LinkDamper, nil)
	require.NoError(t, err)
	assert.Contains(t, subnets, "127.0.0.0/8")
	assert.True(t, td.isScanned("lo"))

	// lo went down a moment ago and reports up again: within the up
	// hold-down it still counts as down.
	td.links["lo"] = &linkState{effectiveUp: false, rawUp: false, since: td.clock}
	subnets, err = GetLocalSubnets([]string{"lo"}, false, td.LinkDamper, nil)
	require.NoError(t, err)
	assert.NotContains(t, subnets, "127.0.0.0/8")
}

// The watcher renews the lease only for links the election scans, and
// returns when netlink ends the subscription.
func TestLinkDamperWatch(t *testing.T) {
	td := newTestDamper()
	td.setScanned([]string{"eth0"})
	ch := make(chan netlink.LinkUpdate, 3)
	ch <- netlink.LinkUpdate{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}}
	ch <- netlink.LinkUpdate{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "veth1234"}}}
	close(ch)

	td.watch(make(chan struct{}), ch)
	assert.Equal(t, 1, td.renews)
}
