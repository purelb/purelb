// Copyright 2023 Acnodal Inc.
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

package v1_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "purelb.io/pkg/apis/v1"
)

func TestSubnet(t *testing.T) {
	multi := v1.ServiceGroupLocalSpec{
		Pool:   "10.42.44.0-10.42.44.1",
		Subnet: "10.42.44.0/31",

		V4Pool: &v1.ServiceGroupAddressPool{
			Pool:   "10.42.43.0-10.42.43.1",
			Subnet: "10.42.43.0/31",
		},
		V6Pool: &v1.ServiceGroupAddressPool{
			Pool:   "2001:db9::68-2001:db9::6f",
			Subnet: "2001:db9::68/124",
		},

		V4Pools: []*v1.ServiceGroupAddressPool{
			&v1.ServiceGroupAddressPool{
				Pool:   "10.42.41.0-10.42.41.1",
				Subnet: "10.42.41.0/31",
			},
			&v1.ServiceGroupAddressPool{
				Pool:   "10.42.42.0-10.42.42.1",
				Subnet: "10.42.42.0/31",
			},
		},
		V6Pools: []*v1.ServiceGroupAddressPool{
			&v1.ServiceGroupAddressPool{
				Pool:   "2001:db7::68-2001:db7::6f",
				Subnet: "2001:db7::68/124",
			},
			&v1.ServiceGroupAddressPool{
				Pool:   "2001:db8::68-2001:db8::6f",
				Subnet: "2001:db8::68/124",
			},
		},
	}

	// Test "legacy" config, i.e., the top-level ad-hoc struct members
	// that we first used before we added dual-stack and multi-range.
	subnet, err := multi.AddressSubnet(net.ParseIP("10.42.44.0"))
	assert.NoError(t, err)
	assert.Equal(t, "10.42.44.0/31", subnet, "incorrect legacy IPV4 subnet")

	// Test dual-stack config, i.e., V4Pool and V6Pool.
	subnet, err = multi.AddressSubnet(net.ParseIP("10.42.43.0"))
	assert.NoError(t, err)
	assert.Equal(t, "10.42.43.0/31", subnet, "incorrect dual-stack IPV4 subnet")
	subnet, err = multi.AddressSubnet(net.ParseIP("2001:db9::68"))
	assert.NoError(t, err)
	assert.Equal(t, "2001:db9::68/124", subnet, "incorrect dual-stack IPV6 subnet")

	// Test multi-range config, i.e. V4Pools and V6Pools.
	subnet, err = multi.AddressSubnet(net.ParseIP("10.42.42.0"))
	assert.NoError(t, err)
	assert.Equal(t, "10.42.42.0/31", subnet, "incorrect dual-stack IPV4 subnet")
	subnet, err = multi.AddressSubnet(net.ParseIP("2001:db8::68"))
	assert.NoError(t, err)
	assert.Equal(t, "2001:db8::68/124", subnet, "incorrect dual-stack IPV6 subnet")
}
