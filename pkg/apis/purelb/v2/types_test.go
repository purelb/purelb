// Copyright 2020-2026 Acnodal Inc.
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

package v2

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poolForAddress is the shared shape of ServiceGroupLocalSpec and
// ServiceGroupRemoteSpec. The two implementations are currently
// byte-identical; running one matrix over both is what catches it if one
// of them drifts.
type poolForAddress interface {
	PoolForAddress(net.IP) (*AddressPool, error)
}

func ap(pool string) AddressPool { return AddressPool{Pool: pool, Subnet: pool} }

// poolForAddressCase is one row of the shared matrix. Exactly one of the
// four pool fields is populated per row unless the case is about
// precedence.
type poolForAddressCase struct {
	name     string
	v4Pools  []AddressPool
	v6Pools  []AddressPool
	v4Pool   *AddressPool
	v6Pool   *AddressPool
	address  string
	wantPool string // the expected AddressPool.Pool value; "" means expect an error
}

func poolForAddressCases() []poolForAddressCase {
	return []poolForAddressCase{
		{
			name:     "v4 in the second element of V4Pools",
			v4Pools:  []AddressPool{ap("192.168.1.0/24"), ap("192.168.2.0/24")},
			address:  "192.168.2.5",
			wantPool: "192.168.2.0/24",
		},
		{
			name:     "v6 in V6Pools",
			v6Pools:  []AddressPool{ap("2001:db8:1::/64"), ap("2001:db8:2::/64")},
			address:  "2001:db8:2::5",
			wantPool: "2001:db8:2::/64",
		},
		{
			name:     "singular V4Pool",
			v4Pool:   func() *AddressPool { p := ap("10.0.0.0/24"); return &p }(),
			address:  "10.0.0.5",
			wantPool: "10.0.0.0/24",
		},
		{
			name:     "singular V6Pool",
			v6Pool:   func() *AddressPool { p := ap("fd00::/64"); return &p }(),
			address:  "fd00::5",
			wantPool: "fd00::/64",
		},
		{
			// Search order is V6Pools -> V4Pools -> V6Pool -> V4Pool, so
			// when both an array and a singular pool contain the address
			// the array element wins.
			name:     "array wins over singular for v4",
			v4Pools:  []AddressPool{ap("192.168.1.0/24")},
			v4Pool:   func() *AddressPool { p := ap("192.168.1.0/24"); return &p }(),
			address:  "192.168.1.5",
			wantPool: "192.168.1.0/24",
		},
		{
			// An unparseable range is skipped rather than fatal -- the
			// `err == nil &&` guard. A malformed pool must not stop a
			// later, valid one from matching.
			name:     "malformed range is skipped not fatal",
			v4Pools:  []AddressPool{ap("not-a-range"), ap("192.168.1.0/24")},
			address:  "192.168.1.5",
			wantPool: "192.168.1.0/24",
		},
		{
			name:    "no match",
			v4Pools: []AddressPool{ap("192.168.1.0/24")},
			address: "10.99.99.99",
		},
		{
			name:    "v6 address against v4-only pools",
			v4Pools: []AddressPool{ap("192.168.1.0/24")},
			address: "2001:db8::1",
		},
		{
			name:    "v4 address against v6-only pools",
			v6Pools: []AddressPool{ap("2001:db8::/64")},
			address: "192.168.1.5",
		},
		{
			name:    "no pools configured at all",
			address: "192.168.1.5",
		},
		{
			name:    "only a malformed range",
			v4Pools: []AddressPool{ap("not-a-range")},
			address: "192.168.1.5",
		},
		{
			name:     "from-to range form",
			v4Pools:  []AddressPool{ap("192.168.5.10-192.168.5.20")},
			address:  "192.168.5.15",
			wantPool: "192.168.5.10-192.168.5.20",
		},
	}
}

func runPoolForAddressCase(t *testing.T, spec poolForAddress, tc poolForAddressCase) {
	t.Helper()
	got, err := spec.PoolForAddress(net.ParseIP(tc.address))
	if tc.wantPool == "" {
		require.Error(t, err, "expected no pool to match")
		assert.ErrorContains(t, err, "unable to find pool for address")
		assert.Nil(t, got)
		return
	}
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tc.wantPool, got.Pool)
}

// TestLocalSpecPoolForAddress covers the lookup that drives the
// announcer's poolFor -> isRemotePool decision, i.e. whether a VIP is
// announced on the local interface or the dummy one.
func TestLocalSpecPoolForAddress(t *testing.T) {
	for _, tc := range poolForAddressCases() {
		t.Run(tc.name, func(t *testing.T) {
			runPoolForAddressCase(t, &ServiceGroupLocalSpec{
				V4Pools: tc.v4Pools,
				V6Pools: tc.v6Pools,
				V4Pool:  tc.v4Pool,
				V6Pool:  tc.v6Pool,
			}, tc)
		})
	}
}

func TestRemoteSpecPoolForAddress(t *testing.T) {
	for _, tc := range poolForAddressCases() {
		t.Run(tc.name, func(t *testing.T) {
			runPoolForAddressCase(t, &ServiceGroupRemoteSpec{
				V4Pools: tc.v4Pools,
				V6Pools: tc.v6Pools,
				V4Pool:  tc.v4Pool,
				V6Pool:  tc.v6Pool,
			}, tc)
		})
	}
}
