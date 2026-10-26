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
	"net"
	"testing"

	"github.com/google/nftables"
	"github.com/stretchr/testify/assert"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egress/datapath"
)

// Main-table routes out of a pool interface, a multipath hop included,
// and main-table default routes wake the datapath.
func TestPoolRoute(t *testing.T) {
	pools := map[int]bool{3: true}
	main := unix.RT_TABLE_MAIN
	dst := &net.IPNet{IP: net.ParseIP("192.0.2.0").To4(), Mask: net.CIDRMask(24, 32)}
	assert.True(t, poolRoute(netlink.Route{Table: main, Dst: dst, LinkIndex: 3}, pools))
	assert.True(t, poolRoute(netlink.Route{Table: main, Dst: dst, MultiPath: []*netlink.NexthopInfo{{LinkIndex: 2}, {LinkIndex: 3}}}, pools))
	assert.False(t, poolRoute(netlink.Route{Table: main, Dst: dst, LinkIndex: 2}, pools))
	assert.False(t, poolRoute(netlink.Route{Table: 7900, Dst: dst, LinkIndex: 3}, pools))
	assert.False(t, poolRoute(netlink.Route{Table: main, Dst: dst, LinkIndex: 3}, nil))
	// A main-table default route, any interface: the uplinks follow it.
	assert.True(t, poolRoute(netlink.Route{Table: main, LinkIndex: 2}, nil))
	assert.True(t, poolRoute(netlink.Route{Table: main, Dst: &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, LinkIndex: 2}, nil))
	assert.False(t, poolRoute(netlink.Route{Table: 100, LinkIndex: 2}, nil))
}

// The agent's own tables are 7700, 7799 and 7900-7963.
func TestOwnTable(t *testing.T) {
	for _, table := range []int{7700, 7799, 7900, 7963} {
		assert.True(t, ownTable(table), "%d", table)
	}
	for _, table := range []int{unix.RT_TABLE_MAIN, 7701, 7899, 7964, 100} {
		assert.False(t, ownTable(table), "%d", table)
	}
}

// Only events naming the hardening table count; set events name none and
// count as ours.
func TestNftTableOf(t *testing.T) {
	ours := &nftables.Table{Name: datapath.NftTable}
	other := &nftables.Table{Name: "kube-proxy"}
	for _, c := range []struct {
		data any
		want string
	}{
		{ours, datapath.NftTable},
		{other, "kube-proxy"},
		{&nftables.Chain{Table: ours}, datapath.NftTable},
		{&nftables.Rule{Table: other}, "kube-proxy"},
		{&nftables.Rule{}, ""},
		{(*nftables.Chain)(nil), ""},
		{&nftables.Set{}, datapath.NftTable},
		{[]nftables.SetElement{}, ""},
		{nil, ""},
	} {
		assert.Equal(t, c.want, nftTableOf(&nftables.MonitorEvent{Data: c.data}), "%#v", c.data)
	}
}
