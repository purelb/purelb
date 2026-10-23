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

package egress

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var (
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	labelValue   = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)
)

func TestNames(t *testing.T) {
	eg := PolicyRef{Kind: KindEgress, Namespace: "voice", Name: "sip-gw"}
	ce := PolicyRef{Kind: KindClusterEgress, Name: "pci-zone"}
	a4, a6 := netip.MustParseAddr("172.32.100.225"), netip.MustParseAddr("2001:db8::225")

	n4, n6 := EndpointsName(eg, a4), EndpointsName(eg, a6)
	assert.True(t, strings.HasPrefix(n4, "e-voice.sip-gw-"))
	assert.NotEqual(t, n4, n6, "each address has its own object")
	assert.Equal(t, n4, EndpointsName(eg, a4), "stable")
	assert.True(t, strings.HasPrefix(EndpointsName(ce, a4), "c-pci-zone-"))
	assert.Equal(t, "e-voice.sip-gw-0", MembersName(eg, 0))
	assert.Equal(t, "egress/voice/sip-gw", eg.String())
	assert.Equal(t, "clusteregress/pci-zone", ce.String())

	// The longest names still make valid object names.
	long := PolicyRef{Kind: KindEgress, Namespace: strings.Repeat("n", 63), Name: strings.Repeat("e", 63)}
	for _, n := range []string{EndpointsName(long, a6), MembersName(long, 99)} {
		assert.LessOrEqual(t, len(n), 253)
		assert.Regexp(t, dnsSubdomain, n)
	}
}

func TestPoolLabelValue(t *testing.T) {
	for _, pool := range []string{"public", "10.0.0.0/24", "fc00:30:10::/64", strings.Repeat("p", 64)} {
		v := PoolLabelValue(pool)
		assert.Regexp(t, labelValue, v, pool)
		assert.LessOrEqual(t, len(v), 63)
	}
	assert.NotEqual(t, PoolLabelValue("a"), PoolLabelValue("b"))
}

func TestSidecarKey(t *testing.T) {
	assert.Equal(t, "egress:Egress:voice:sip-gw", SidecarKey(PolicyRef{Kind: KindEgress, Namespace: "voice", Name: "sip-gw"}, ""))
	assert.Equal(t, "egress:ClusterEgress::pci:db-2", SidecarKey(PolicyRef{Kind: KindClusterEgress, Name: "pci"}, "db-2"))
}

func TestBefore(t *testing.T) {
	t0 := time.Unix(1000, 0)
	ranks := []Rank{
		{Priority: 0, Created: t0, UID: "b"},
		{Priority: 1000, Created: t0.Add(time.Hour), UID: "z"},
		{Priority: 0, Created: t0, UID: "a"},
		{Priority: 100, Created: t0, UID: "c"},
		{Priority: 0, Created: t0.Add(-time.Hour), UID: "d"},
	}
	sort.Slice(ranks, func(i, j int) bool { return Before(ranks[i], ranks[j]) })
	var uids []string
	for _, r := range ranks {
		uids = append(uids, r.UID)
	}
	assert.Equal(t, []string{"z", "c", "d", "a", "b"}, uids)
	assert.False(t, Before(ranks[0], ranks[0]), "irreflexive")
}
