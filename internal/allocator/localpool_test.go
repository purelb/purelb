// Copyright 2020 Acnodal Inc.
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
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/vishvananda/netlink/nl"
	v1 "k8s.io/api/core/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

var (
	key1                = Key{Sharing: "sharing1"}
	key2                = Key{Sharing: "sharing2"}
	http                = Port{Proto: v1.ProtocolTCP, Port: 80}
	smtp                = Port{Proto: v1.ProtocolTCP, Port: 25}
	localPoolTestLogger = log.NewNopLogger()
)

func TestEmptyPool(t *testing.T) {
	var svc v1.Service
	p := LocalPool{}
	assert.Equal(t, uint64(0), p.Size(), "incorrect pool size")
	assert.Error(t, p.assignFamily(nl.FAMILY_V6, &svc))
	assert.Error(t, p.assignFamily(nl.FAMILY_V4, &svc))
	assert.Error(t, p.AssignNext(&svc))
}

func TestNewLocalPool(t *testing.T) {
	var svc v1.Service
	ip4 := "192.168.1.1"
	ip6 := "2001:470:1f07:98e:d62a:159b:41a3:93d3"

	// Test v4 pool config
	v4Pool := &purelbv2.AddressPool{
		Pool:   "192.168.1.1/32",
		Subnet: "192.168.1.1/32",
	}
	p, err := NewLocalPool("testpool", localPoolTestLogger, v4Pool, nil, nil, nil, purelbv2.PoolTypeLocal, false)
	assert.NoError(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	assert.NoError(t, p.AssignNext(&svc), "Address allocation failed")
	assert.Equal(t, ip4, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")

	// Test IPV4 config with V4Pool
	p, err = NewLocalPool("v4pool", localPoolTestLogger, v4Pool, nil, nil, nil, purelbv2.PoolTypeLocal, false)
	assert.NoError(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	assert.NoError(t, p.AssignNext(&svc), "Address allocation failed")
	// We specified the top-level Pool and the V4Pool so the V4Pool
	// should take precedence
	assert.Equal(t, ip4, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")

	// Test IPV6 config
	v6Pool := &purelbv2.AddressPool{
		Pool:   "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
		Subnet: "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
	}
	p, err = NewLocalPool("v6pool", localPoolTestLogger, nil, v6Pool, nil, nil, purelbv2.PoolTypeLocal, false)
	assert.NoError(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	assert.NoError(t, p.AssignNext(&svc), "Address allocation failed")
	assert.Equal(t, ip6, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")

	// Test both pools config
	p, err = NewLocalPool("bothpools", localPoolTestLogger, v4Pool, v6Pool, nil, nil, purelbv2.PoolTypeLocal, false)
	assert.NoError(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	assert.NoError(t, p.AssignNext(&svc), "Address allocation failed")
	// We specified both pools so the V6Pool should take precedence
	assert.Equal(t, ip6, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")

	// Test invalid ranges - v4 pool not contained in subnet
	invalidV4Pool := &purelbv2.AddressPool{
		Pool:   "192.168.1.0-192.168.1.1",
		Subnet: "192.168.1.0/32",
	}
	_, err = NewLocalPool("uncontained", localPoolTestLogger, invalidV4Pool, nil, nil, nil, purelbv2.PoolTypeLocal, false)
	assert.Error(t, err, "pool isn't contained in its subnet")

	// Test invalid ranges - v6 pool not contained in subnet
	invalidV6Pool := &purelbv2.AddressPool{
		Pool:   "2001:470:1f07:98e:d62a:159b:41a3:93d3-2001:470:1f07:98e:d62a:159b:41a3:93d4",
		Subnet: "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
	}
	_, err = NewLocalPool("uncontained", localPoolTestLogger, nil, invalidV6Pool, nil, nil, purelbv2.PoolTypeLocal, false)
	assert.Error(t, err, "pool isn't contained in its subnet")
}

func TestFirstNext(t *testing.T) {
	p := mustDualStackPool(t, []string{"192.168.2.0/31", "192.168.1.3/32", "192.168.1.2/32"}, []string{"fc00::0043:0000/128", "fc00::0042:0000/128"})

	first := p.first(nl.FAMILY_V4)
	next := first
	assert.Equal(t, net.ParseIP("192.168.2.0"), first, "first() returned an incorrect address")
	next = p.next(next)
	assert.Equal(t, net.ParseIP("192.168.2.1"), next, "next() returned an incorrect address")
	next = p.next(next)
	assert.Equal(t, net.ParseIP("192.168.1.3"), next, "next() returned an incorrect address")
	next = p.next(next)
	assert.Equal(t, net.ParseIP("192.168.1.2"), next, "next() returned an incorrect address")
	next = p.next(next)
	assert.Nil(t, next, "next() returned an incorrect address")

	first = p.first(nl.FAMILY_V6)
	next = first
	assert.Equal(t, net.ParseIP("fc00::0043:0000"), first, "first() returned an incorrect address")
	next = p.next(next)
	assert.Equal(t, net.ParseIP("fc00::0042:0000"), next, "next() returned an incorrect address")
	next = p.next(next)
	assert.Nil(t, next, "next() returned an incorrect address")
}

func TestNotify(t *testing.T) {
	ip1 := net.ParseIP("192.168.1.2")
	ip2 := net.ParseIP("192.168.1.3")
	p := mustLocalPool(t, "notify", "192.168.1.2/31")

	svc1 := service("svc1", ports("tcp/80"), "")
	svc2 := service("svc2", ports("tcp/80"), "")

	// Tell the pool that ip1 is in use
	addIngress(localPoolTestLogger, &svc1, ip1)
	assert.NoError(t, p.Notify(&svc1), "Notify failed")

	// Allocate an address to svc2 - it should get ip2 since ip1 is in
	// use by svc1
	assert.NoError(t, p.AssignNext(&svc2), "Assigning an address failed")
	assert.Equal(t, ip2.String(), svc2.Status.LoadBalancer.Ingress[0].IP, "svc2 was assigned the wrong address")
}

func TestInUse(t *testing.T) {
	ip := net.ParseIP("192.168.1.1")
	ip2 := net.ParseIP("192.168.1.2")
	p := mustLocalPool(t, "inuse", "192.168.1.1/32")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/80"), "sharing2")
	svc3 := service("svc3", ports("tcp/25"), "sharing2")

	p.Assign(ip2, &svc1)
	assert.Equal(t, 1, p.InUse())
	p.Assign(ip, &svc2)
	assert.Equal(t, 2, p.InUse())
	p.Assign(ip, &svc3)
	assert.Equal(t, 2, p.InUse()) // allocating the same address doesn't change the count
	p.Release(namespacedName(&svc2))
	assert.Equal(t, 2, p.InUse()) // the address isn't fully released yet
	p.Release(namespacedName(&svc3))
	assert.Equal(t, 1, p.InUse()) // the address isn't fully released yet
	p.Release(namespacedName(&svc1))
	assert.Equal(t, 0, p.InUse()) // all addresses are released
}

func TestServicesOn(t *testing.T) {
	ip2 := net.ParseIP("192.168.1.2")
	p := mustLocalPool(t, "serviceson", "192.168.1.1/32")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/25"), "sharing1")

	p.Assign(ip2, &svc1)
	assert.Equal(t, []string{namespacedName(&svc1)}, p.servicesOnIP(ip2))
	p.Assign(ip2, &svc2)
	sameStrings(t, []string{namespacedName(&svc1), namespacedName(&svc2)}, p.servicesOnIP(ip2))
	p.Release(namespacedName(&svc1))
	assert.Equal(t, []string{namespacedName(&svc2)}, p.servicesOnIP(ip2))
}

func TestSharing(t *testing.T) {
	ip4 := net.ParseIP("192.168.1.1")
	p := mustDualStackPool(t, []string{"192.168.1.1/32"}, []string{"fc00::0042:0000/120"})
	svc1 := service("svc1", ports("tcp/80"), key1.Sharing)
	svc1.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol}
	svc2 := service("svc2", ports("tcp/81"), key1.Sharing)
	svc2.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol}
	svc3 := service("svc3", ports("tcp/81"), key1.Sharing)
	svc3.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol}

	p.Release(namespacedName(&svc3)) // releasing a not-assigned service should be OK

	// Allocate addresses to svc1 and svc2 and verify that they both
	// have the same ones
	assert.NoError(t, p.AssignNext(&svc1))
	assert.Equal(t, 2, len(svc1.Status.LoadBalancer.Ingress))
	assert.Equal(t, key1, *p.sharingKeys[svc1.Status.LoadBalancer.Ingress[0].IP])
	assert.Equal(t, key1, *p.sharingKeys[svc1.Status.LoadBalancer.Ingress[1].IP])
	assert.NoError(t, p.AssignNext(&svc2))
	assert.EqualValues(t, svc1.Status.LoadBalancer.Ingress, svc2.Status.LoadBalancer.Ingress, "svc1 and svc2 have different addresses")

	p.Release(namespacedName(&svc1))
	// svc2 is still using the IPs
	assert.Equal(t, key1, *p.sharingKeys[svc2.Status.LoadBalancer.Ingress[1].IP])
	assert.Error(t, p.Assign(ip4, &svc3)) // svc3 is blocked by svc2 (same port)
	p.Release(namespacedName(&svc2))
	// the IP is unused
	assert.Nil(t, p.SharingKey(ip4))
	assert.NoError(t, p.Assign(ip4, &svc3)) // svc2 is out of the picture so svc3 can use the address
}

func TestAvailable(t *testing.T) {
	p := mustDualStackPool(t, []string{"192.168.1.1/32"}, []string{})
	ip := net.ParseIP("192.168.1.1")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")

	// no assignment, should be available
	assert.NoError(t, p.available(ip, &svc1))

	p.Assign(ip, &svc1)

	// same service can "share" with or without the key
	assert.NoError(t, p.available(ip, &svc1))
	svc1X := service("svc1", ports("tcp/80"), "XshareX")
	assert.NoError(t, p.available(ip, &svc1X))

	svc2 := service("svc2", ports("tcp/80"), "")
	// other service, no key: no share
	assert.Error(t, p.available(ip, &svc2))
	// other service, has key, same port: no share
	svc2 = service("svc2", ports("tcp/80"), "sharing1")
	assert.Error(t, p.available(ip, &svc2))
	// other service, has key, different port: share
	svc2 = service("svc2", ports("tcp/25"), "sharing1")
	assert.NoError(t, p.available(ip, &svc2))
}

func TestAssignNext(t *testing.T) {
	p := mustDualStackPool(t, []string{"192.168.1.0/32", "192.168.1.1/32"}, []string{})
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/80"), "sharing2")
	svc3 := service("svc3", ports("tcp/80"), "sharing2")

	// The pool has two addresses; allocate both of them
	assert.NoError(t, p.AssignNext(&svc1))
	assert.Equal(t, "192.168.1.0", svc1.Status.LoadBalancer.Ingress[0].IP, "svc1 was assigned the wrong address")
	assert.NoError(t, p.AssignNext(&svc2))
	assert.Equal(t, "192.168.1.1", svc2.Status.LoadBalancer.Ingress[0].IP, "svc2 was assigned the wrong address")

	// Same port: should fail
	assert.Error(t, p.AssignNext(&svc3))

	// Shared key, different ports: should succeed
	svc3.Spec.Ports = ports("tcp/25")
	assert.NoError(t, p.AssignNext(&svc3))
}

func TestPoolSize(t *testing.T) {
	v4Pool := &purelbv2.AddressPool{
		Pool:   "192.168.1.0/31",
		Subnet: "192.168.1.0/31",
	}
	v6Pool := &purelbv2.AddressPool{
		Pool:   "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
		Subnet: "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
	}
	p, err := NewLocalPool("sizetest", localPoolTestLogger, v4Pool, v6Pool, nil, nil, purelbv2.PoolTypeLocal, false)
	assert.NoError(t, err, "Pool instantiation failed")
	assert.Equal(t, uint64(3), p.Size(), "Pool Size() failed")
}

func TestWhichFamilies(t *testing.T) {
	var (
		families []int
		err      error
	)
	p := mustLocalPool(t, "ipv6", "2001:470:1f07:98e:d62a:159b:41a3:93d3/128")
	svc := service("svc1", ports("tcp/80"), "sharing1")

	svc.Spec.IPFamilies = []v1.IPFamily{}
	families, err = p.whichFamilies(&svc)
	assert.NoError(t, err, "whichFamilies() failed")
	assert.ElementsMatch(t, []int{}, families, "incorrect empty families")

	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol}
	families, err = p.whichFamilies(&svc)
	assert.NoError(t, err, "whichFamilies() failed")
	assert.ElementsMatch(t, []int{nl.FAMILY_V4}, families, "incorrect empty families")

	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol}
	families, err = p.whichFamilies(&svc)
	assert.NoError(t, err, "whichFamilies() failed")
	assert.ElementsMatch(t, []int{nl.FAMILY_V6, nl.FAMILY_V4}, families, "incorrect empty families")
}

func TestPoolContains(t *testing.T) {
	containedV4 := net.ParseIP("192.168.1.1")
	containedV6 := net.ParseIP("fc00::0042:0000")
	outsideV4 := net.ParseIP("192.168.1.2")
	outsideV6 := net.ParseIP("fc00::0043:0000")

	p := LocalPool{}
	assert.False(t, p.Contains(outsideV4))
	assert.False(t, p.Contains(outsideV6))
	assert.False(t, p.Contains(containedV4))
	assert.False(t, p.Contains(containedV6))

	p = mustDualStackPool(t, []string{"192.168.1.1/32"}, []string{"fc00::0042:0000/120"})
	assert.False(t, p.Contains(outsideV4))
	assert.False(t, p.Contains(outsideV6))
	assert.True(t, p.Contains(containedV4))
	assert.True(t, p.Contains(containedV6))
}

func sameStrings(t *testing.T, want []string, got []string) {
	sort.Strings(got)
	assert.Equal(t, want, got)
}

func mustLocalPool(_ *testing.T, name string, r string) LocalPool {
	pool := &purelbv2.AddressPool{Pool: r, Subnet: r}
	// Detect if this is an IPv6 range by checking for ':'
	var v4Pool, v6Pool *purelbv2.AddressPool
	if strings.Contains(r, ":") {
		v6Pool = pool
	} else {
		v4Pool = pool
	}
	p, err := NewLocalPool(name, allocatorTestLogger, v4Pool, v6Pool, nil, nil, purelbv2.PoolTypeLocal, false)
	if err != nil {
		panic(err)
	}
	return p
}

func mustDualStackPool(_ *testing.T, pools4 []string, pools6 []string) LocalPool {
	var v4Pools []purelbv2.AddressPool
	var v6Pools []purelbv2.AddressPool
	for _, pool6 := range pools6 {
		v6Pools = append(v6Pools, purelbv2.AddressPool{Pool: pool6, Subnet: pool6})
	}
	for _, pool4 := range pools4 {
		v4Pools = append(v4Pools, purelbv2.AddressPool{Pool: pool4, Subnet: pool4})
	}
	p, err := NewLocalPool("unittest", allocatorTestLogger, nil, nil, v4Pools, v6Pools, purelbv2.PoolTypeLocal, false)
	if err != nil {
		panic(err)
	}
	return p
}

// TestSharingKeyPortConflict verifies that services with the same sharing key
// and same port cannot both get IPs - the second should fail even if there
// are other IPs available in the pool.
func TestSharingKeyPortConflict(t *testing.T) {
	// Pool with 4 IPs to verify we don't escape to another IP
	p := mustDualStackPool(t, []string{"192.168.1.0/30"}, []string{})

	svc1 := service("svc1", ports("tcp/80"), "webservers")
	svc2 := service("svc2", ports("tcp/443"), "webservers") // different port, same key
	svc3 := service("svc3", ports("tcp/80"), "webservers")  // same port, same key - should FAIL

	// svc1 gets first IP
	assert.NoError(t, p.AssignNext(&svc1))
	assert.Equal(t, "192.168.1.0", svc1.Status.LoadBalancer.Ingress[0].IP)

	// svc2 shares the IP (different port)
	assert.NoError(t, p.AssignNext(&svc2))
	assert.Equal(t, "192.168.1.0", svc2.Status.LoadBalancer.Ingress[0].IP)

	// svc3 should FAIL - same sharing key, same port
	err := p.AssignNext(&svc3)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "port TCP/80")
	assert.Contains(t, err.Error(), "already in use")

	// Verify svc3 did NOT get an IP
	assert.Empty(t, svc3.Status.LoadBalancer.Ingress)
}

// TestSharingKeyReleaseCleanup verifies that when all services using a
// sharing key are released, the reverse index is properly cleaned up.
func TestSharingKeyReleaseCleanup(t *testing.T) {
	p := mustDualStackPool(t, []string{"192.168.1.0/32"}, []string{})
	ip := net.ParseIP("192.168.1.0")

	svc1 := service("svc1", ports("tcp/80"), "webservers")
	assert.NoError(t, p.Assign(ip, &svc1))

	// Verify sharing key is tracked
	assert.NotNil(t, p.ipForSharingKey("webservers", nl.FAMILY_V4))
	assert.Equal(t, "192.168.1.0", p.ipForSharingKey("webservers", nl.FAMILY_V4).String())

	// Release the service
	p.Release(namespacedName(&svc1))

	// Verify sharing key mapping is cleaned up
	assert.Nil(t, p.ipForSharingKey("webservers", nl.FAMILY_V4))
}

// TestSharingKeyBindingPreventsOtherIPs verifies that a service with a
// sharing key bound to one IP cannot be assigned to a different IP.
func TestSharingKeyBindingPreventsOtherIPs(t *testing.T) {
	p := mustDualStackPool(t, []string{"192.168.1.0/31"}, []string{}) // 2 IPs

	svc1 := service("svc1", ports("tcp/80"), "webservers")
	svc2 := service("svc2", ports("tcp/80"), "webservers") // same port, same key

	// svc1 gets first IP, binding "webservers" to 192.168.1.0
	assert.NoError(t, p.AssignNext(&svc1))
	assert.Equal(t, "192.168.1.0", svc1.Status.LoadBalancer.Ingress[0].IP)

	// svc2 cannot get 192.168.1.1 because "webservers" is bound to .0
	// and it cannot share .0 because of port conflict
	err := p.AssignNext(&svc2)
	assert.Error(t, err)

	// The error should mention the port conflict (from the bound IP),
	// not "no available addresses"
	assert.Contains(t, err.Error(), "port TCP/80")
}

// TestDifferentSharingKeysGetDifferentIPs verifies that services with
// different sharing keys can get different IPs.
func TestDifferentSharingKeysGetDifferentIPs(t *testing.T) {
	p := mustDualStackPool(t, []string{"192.168.1.0/31"}, []string{}) // 2 IPs

	svc1 := service("svc1", ports("tcp/80"), "webservers")
	svc2 := service("svc2", ports("tcp/80"), "databases") // same port, different key

	assert.NoError(t, p.AssignNext(&svc1))
	assert.Equal(t, "192.168.1.0", svc1.Status.LoadBalancer.Ingress[0].IP)

	// svc2 should get a different IP because it has a different sharing key
	assert.NoError(t, p.AssignNext(&svc2))
	assert.Equal(t, "192.168.1.1", svc2.Status.LoadBalancer.Ingress[0].IP)
}
