// Copyright 2020 Acnodal Inc.
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
	"math"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vishvananda/netlink/nl"
)

func TestParseCIDR(t *testing.T) {
	_, error := parseCIDR("1.1.1.1")
	assert.Error(t, error, "1.1.1.1 should have failed to parse but didn't")
	_, error = parseCIDR("1.1.X.1")
	assert.Error(t, error, "1.1.X.1 should have failed to parse but didn't")

	assertCIDR(t, "1.1.1.1/32", "1.1.1.1", "1.1.1.1")
	assertCIDR(t, "1.1.1.0/31", "1.1.1.0", "1.1.1.1")
	assertCIDR(t, "1.1.1.0/24", "1.1.1.0", "1.1.1.255")
	assertCIDR(t, "1.1.0.0/16", "1.1.0.0", "1.1.255.255")
	assertCIDR(t, "1.0.0.0/8", "1.0.0.0", "1.255.255.255")

	assertCIDR(t, "2001:db8::/112", "2001:db8::", "2001:db8::ffff")
}

func TestParseRange(t *testing.T) {
	_, error := parseFromTo("1.1.1.1")
	assert.Error(t, error, "1.1.1.1 should have failed to parse but didn't")
	_, error = parseFromTo("1.1.1.foo-1.1.1.2")
	assert.Error(t, error, "1.1.1.foo-1.1.1.2 should have failed to parse but didn't")
	_, error = parseFromTo("1.1.1.1-1.1.1.foo")
	assert.Error(t, error, "1.1.1.1-1.1.1.foo should have failed to parse but didn't")

	assertFromTo(t, "1.1.1.0-1.1.1.1", "1.1.1.0", "1.1.1.1")
	assertFromTo(t, " 1.1.1.1 - 1.1.1.1 ", "1.1.1.1", "1.1.1.1")

	assertFromTo(t, "2001:db8::0 - 2001:db8::ffff", "2001:db8::", "2001:db8::ffff")
}

func TestNewIPRange(t *testing.T) {
	assertIPRange(t, mustIPRange(t, "1.1.1.1/32"), "1.1.1.1", "1.1.1.1")
	assertIPRange(t, mustIPRange(t, "1.1.1.0-1.1.1.1"), "1.1.1.0", "1.1.1.1")

	assertIPRange(t, mustIPRange(t, "2001:db8::0 - 2001:db8::ffff"), "2001:db8::", "2001:db8::ffff")
}

func TestOverlaps(t *testing.T) {
	ipr1 := mustIPRange(t, "1.1.1.1/32")
	ipr2 := mustIPRange(t, "1.1.1.2/32")
	assert.False(t, ipr1.Overlaps(ipr2))

	ipr1 = mustIPRange(t, "1.1.1.0/24")
	ipr2 = mustIPRange(t, "1.1.1.0/30")
	assert.True(t, ipr1.Overlaps(ipr2))
	assert.True(t, ipr2.Overlaps(ipr1))

	ipr1 = mustIPRange(t, "1.1.1.0-1.1.1.128")
	ipr2 = mustIPRange(t, "1.1.1.128-1.1.1.255")
	assert.True(t, ipr1.Overlaps(ipr2))
	assert.True(t, ipr2.Overlaps(ipr1))
}

func TestFirst(t *testing.T) {
	ipr1 := mustIPRange(t, "1.1.1.1/32")
	assert.Equal(t, "1.1.1.1", ipr1.First().String())
}

func TestNext(t *testing.T) {
	ipr1 := mustIPRange(t, "1.1.1.2/31")
	assert.Nil(t, ipr1.Next(net.ParseIP("1.1.1.1")))
	ip := ipr1.First()
	assert.Equal(t, "1.1.1.2", ip.String())
	ip = ipr1.Next(ip)
	assert.Equal(t, "1.1.1.3", ip.String())
	ip = ipr1.Next(ip)
	assert.Nil(t, ip)
}

func TestFamily(t *testing.T) {
	iprV4 := mustIPRange(t, "1.1.1.0/31")
	assert.Equal(t, nl.FAMILY_V4, iprV4.Family(), "wrong family")

	iprV6 := mustIPRange(t, "2001:db8::68/124")
	assert.Equal(t, nl.FAMILY_V6, iprV6.Family(), "wrong family")
}

func TestContains(t *testing.T) {
	ipr1 := mustIPRange(t, "1.1.1.0/31")
	assert.False(t, ipr1.Contains(net.ParseIP("1.1.0.0")))
	assert.True(t, ipr1.Contains(net.ParseIP("1.1.1.0")))
	assert.True(t, ipr1.Contains(net.ParseIP("1.1.1.1")))
	assert.False(t, ipr1.Contains(net.ParseIP("1.1.1.2")))
}

func TestContainedBy(t *testing.T) {
	_, sn, err := net.ParseCIDR("1.1.1.2/31")
	assert.Nil(t, err)

	assert.True(t, mustIPRange(t, "1.1.1.2-1.1.1.3").ContainedBy(*sn))
	assert.False(t, mustIPRange(t, "1.1.1.1-1.1.1.3").ContainedBy(*sn))
	assert.False(t, mustIPRange(t, "1.1.1.2-1.1.1.4").ContainedBy(*sn))
}

func TestInt(t *testing.T) {
	// anything greater than 64 bits gets truncated
	assert.Equal(t, uint64(0x68), toInt(net.ParseIP("2001:db8::68")))

	// explicit IPV4
	assert.Equal(t, uint64(0x01000100), toInt(net.ParseIP("1.0.1.0").To4()))

	// IPV4 in IPV6
	assert.Equal(t, uint64(0xffff00000100), toInt(net.ParseIP("0.0.1.0")))
}

func TestSize(t *testing.T) {
	// IPV4 CIDR
	assert.Equal(t, uint64(1), mustIPRange(t, "1.1.1.1/32").Size())
	assert.Equal(t, uint64(256), mustIPRange(t, "1.1.1.0/24").Size())

	// IPV4 to-from
	assert.Equal(t, uint64(2), mustIPRange(t, "1.1.1.0-1.1.1.1").Size())
	assert.Equal(t, uint64(256), mustIPRange(t, "1.1.0.0-1.1.0.255").Size())

	// IPV6 CIDR
	assert.Equal(t, uint64(65535), mustIPRange(t, "2001:db8::1/112").Size())

	// IPV6 to-from
	assert.Equal(t, uint64(5), mustIPRange(t, "2001:db8::68 - 2001:db8::6c").Size())
	assert.Equal(t, uint64(math.MaxUint64), mustIPRange(t, "2002:db8::68 - 2001:db8::68").Size())
}

func assertFromTo(t *testing.T, raw string, from string, to string) {
	ipr, error := parseFromTo(raw)
	assert.Nil(t, error)
	assertIPRange(t, ipr, from, to)
}

func assertCIDR(t *testing.T, cidr string, from string, to string) {
	ipr, error := parseCIDR(cidr)
	assert.Nil(t, error)
	assertIPRange(t, ipr, from, to)
}

func assertIPRange(t *testing.T, ipr IPRange, from string, to string) {
	assert.Equal(t, from, ipr.from.String(), "bad from address")
	assert.Equal(t, to, ipr.to.String(), "bad to address")
}

func mustIPRange(t *testing.T, s string) IPRange {
	n, err := NewIPRange(s)
	assert.Nil(t, err, s)
	return n
}
