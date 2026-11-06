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
	"net/netip"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"purelb.io/internal/egress/datapath"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func TestDPCollector(t *testing.T) {
	t.Cleanup(func() { currentDPMetrics.Store(nil) })
	currentDPMetrics.Store(nil)
	assert.Zero(t, testutil.CollectAndCount(dpCollector{}), "nothing before the datapath reports")

	var tr datapath.Traffic
	tr.Packets[datapath.TrafficOutPod], tr.Bytes[datapath.TrafficOutPod] = 3, 300
	tr.Packets[datapath.TrafficReturnAnnouncer], tr.Bytes[datapath.TrafficReturnAnnouncer] = 2, 200
	currentDPMetrics.Store(&dpMetrics{
		reasons:   map[string][2]uint64{"forged_tunnel": {5, 6}, "transit": {7, 0}},
		traffic:   map[netip.Addr]datapath.Traffic{netip.MustParseAddr("2001:db8::1"): tr},
		peersDown: 2,
	})
	require.NoError(t, testutil.CollectAndCompare(dpCollector{}, strings.NewReader(`
# HELP purelb_egress_tunnel_peers_down Tunnel peers not answering this node's probes. kubectl purelb egress dump names them.
# TYPE purelb_egress_tunnel_peers_down gauge
purelb_egress_tunnel_peers_down 2
`), "purelb_egress_tunnel_peers_down"))
	require.NoError(t, testutil.CollectAndCompare(dpCollector{}, strings.NewReader(`
# HELP purelb_egress_datapath_packets_total Packets the egress programs handled, by reason, action (pass or drop) and family.
# TYPE purelb_egress_datapath_packets_total counter
purelb_egress_datapath_packets_total{action="drop",family="ipv4",reason="forged_tunnel"} 5
purelb_egress_datapath_packets_total{action="drop",family="ipv6",reason="forged_tunnel"} 6
purelb_egress_datapath_packets_total{action="pass",family="ipv4",reason="transit"} 7
purelb_egress_datapath_packets_total{action="pass",family="ipv6",reason="transit"} 0
`), "purelb_egress_datapath_packets_total"))
	require.NoError(t, testutil.CollectAndCompare(dpCollector{}, strings.NewReader(`
# HELP purelb_egress_packets_total Packets of an egress address on this node, by direction (out, return) and role (pod_node, announcer). Addresses of Redacted groups are left out.
# TYPE purelb_egress_packets_total counter
purelb_egress_packets_total{direction="out",ip="2001:db8::1",role="announcer"} 0
purelb_egress_packets_total{direction="out",ip="2001:db8::1",role="pod_node"} 3
purelb_egress_packets_total{direction="return",ip="2001:db8::1",role="announcer"} 2
purelb_egress_packets_total{direction="return",ip="2001:db8::1",role="pod_node"} 0
`), "purelb_egress_packets_total"))
}

func TestGroupRedacted(t *testing.T) {
	assert.False(t, groupRedacted(nil))
	assert.False(t, groupRedacted(&purelbv2.ServiceGroup{}))
	g := &purelbv2.ServiceGroup{Spec: purelbv2.ServiceGroupSpec{Egress: &purelbv2.ServiceGroupEgressSpec{
		Tenancy: &purelbv2.EgressTenancy{StatusDetail: "Redacted"}}}}
	assert.True(t, groupRedacted(g))
}
