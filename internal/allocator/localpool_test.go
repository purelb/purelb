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
	"testing"

	"github.com/go-kit/kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/vishvananda/netlink/nl"
	v1 "k8s.io/api/core/v1"

	purelbv1 "purelb.io/pkg/apis/v1"
)

var (
	key1                = Key{Sharing: "sharing1"}
	key2                = Key{Sharing: "sharing2"}
	http                = Port{Proto: v1.ProtocolTCP, Port: 80}
	smtp                = Port{Proto: v1.ProtocolTCP, Port: 25}
	localPoolTestLogger = log.NewNopLogger()
)

func TestNewLocalPool(t *testing.T) {
	var svc v1.Service
	ip4 := "192.168.1.1"
	ip6 := "2001:470:1f07:98e:d62a:159b:41a3:93d3"

	// Test old-fashioned config (i.e., using the top-level Pool)
	p, err := NewLocalPool(localPoolTestLogger, purelbv1.ServiceGroupLocalSpec{
		Pool: "192.168.1.1/32",
	})
	assert.Nil(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	err = p.AssignNext(&svc)
	assert.Nil(t, err, "Address allocation failed")
	assert.Equal(t, ip4, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")

	// Test IPV4 config
	p, err = NewLocalPool(localPoolTestLogger, purelbv1.ServiceGroupLocalSpec{
		V4Pool: &purelbv1.ServiceGroupAddressPool{
			Pool: "192.168.1.1/32",
		},
	})
	assert.Nil(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	err = p.AssignNext(&svc)
	assert.Nil(t, err, "Address allocation failed")
	// We specified the top-level Pool and the V4Pool so the V4Pool
	// should take precedence
	assert.Equal(t, ip4, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")

	// Test IPV6 config
	p, err = NewLocalPool(localPoolTestLogger, purelbv1.ServiceGroupLocalSpec{
		Pool: "192.168.1.1/32",
		V6Pool: &purelbv1.ServiceGroupAddressPool{
			Pool: "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
		},
	})
	assert.Nil(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	err = p.AssignNext(&svc)
	assert.Nil(t, err, "Address allocation failed")
	// We specified the top-level Pool and the V6Pool so the V6Pool
	// should take precedence
	assert.Equal(t, ip6, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")

	// Test IPV6 config
	p, err = NewLocalPool(localPoolTestLogger, purelbv1.ServiceGroupLocalSpec{
		V4Pool: &purelbv1.ServiceGroupAddressPool{
			Pool: "192.168.1.1/32",
		},
		V6Pool: &purelbv1.ServiceGroupAddressPool{
			Pool: "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
		},
	})
	assert.Nil(t, err, "Pool instantiation failed")
	svc = v1.Service{}
	err = p.AssignNext(&svc)
	assert.Nil(t, err, "Address allocation failed")
	// We specified both pools so the V6Pool should take precedence
	assert.Equal(t, ip6, svc.Status.LoadBalancer.Ingress[0].IP, "AssignNext failed")
}

func TestNotify(t *testing.T) {
	ip1 := net.ParseIP("192.168.1.2")
	ip2 := net.ParseIP("192.168.1.3")
	p := mustLocalPool(t, "192.168.1.2/31")

	svc1 := service("svc1", ports("tcp/80"), "")
	svc2 := service("svc2", ports("tcp/80"), "")

	// Tell the pool that ip1 is in use
	addIngress(localPoolTestLogger, &svc1, ip1)
	assert.Nil(t, p.Notify(&svc1), "Notify failed")

	// Allocate an address to svc2 - it should get ip2 since ip1 is in
	// use by svc1
	err := p.AssignNext(&svc2)
	assert.Nil(t, err, "Assigning an address failed")
	assert.Equal(t, ip2.String(), svc2.Status.LoadBalancer.Ingress[0].IP, "svc2 was assigned the wrong address")
}

func TestInUse(t *testing.T) {
	ip := net.ParseIP("192.168.1.1")
	ip2 := net.ParseIP("192.168.1.2")
	p := mustLocalPool(t, "192.168.1.1/32")
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
	p := mustLocalPool(t, "192.168.1.1/32")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/25"), "sharing1")

	p.Assign(ip2, &svc1)
	assert.Equal(t, []string{namespacedName(&svc1)}, p.servicesOnIP(ip2))
	p.Assign(ip2, &svc2)
	sameStrings(t, []string{namespacedName(&svc1), namespacedName(&svc2)}, p.servicesOnIP(ip2))
	p.Release(namespacedName(&svc1))
	assert.Equal(t, []string{namespacedName(&svc2)}, p.servicesOnIP(ip2))
}

func TestSharingKeys(t *testing.T) {
	ip := net.ParseIP("192.168.1.1")
	p := mustLocalPool(t, "192.168.1.1/32")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/81"), "sharing1")
	svc3 := service("svc3", ports("tcp/81"), "sharing1")

	p.Release(namespacedName(&svc3)) // releasing a not-assigned service should be OK

	assert.Nil(t, p.Assign(ip, &svc1))
	assert.Equal(t, &key1, p.SharingKey(ip))
	assert.Nil(t, p.Assign(ip, &svc2))
	p.Release(namespacedName(&svc1))
	// svc2 is still using the IP
	assert.Equal(t, &key1, p.SharingKey(ip))
	assert.NotNil(t, p.Assign(ip, &svc3)) // svc3 is blocked by svc2 (same port)
	p.Release(namespacedName(&svc2))
	// the IP is unused
	assert.Nil(t, p.SharingKey(ip))
	assert.Nil(t, p.Assign(ip, &svc3)) // svc2 is out of the picture so svc3 can use the address
}

func TestAvailable(t *testing.T) {
	p := mustLocalPool(t, "192.168.1.1/32")
	ip := net.ParseIP("192.168.1.1")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")

	// no assignment, should be available
	assert.Nil(t, p.available(ip, &svc1))

	p.Assign(ip, &svc1)

	// same service can "share" with or without the key
	assert.Nil(t, p.available(ip, &svc1))
	svc1X := service("svc1", ports("tcp/80"), "XshareX")
	assert.Nil(t, p.available(ip, &svc1X))

	svc2 := service("svc2", ports("tcp/80"), "")
	// other service, no key: no share
	assert.NotNil(t, p.available(ip, &svc2))
	// other service, has key, same port: no share
	svc2 = service("svc2", ports("tcp/80"), "sharing1")
	assert.NotNil(t, p.available(ip, &svc2))
	// other service, has key, different port: share
	svc2 = service("svc2", ports("tcp/25"), "sharing1")
	assert.Nil(t, p.available(ip, &svc2))
}

func TestAssignNext(t *testing.T) {
	p := mustLocalPool(t, "192.168.1.0/31")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/80"), "sharing2")
	svc3 := service("svc3", ports("tcp/80"), "sharing2")

	// The pool has two addresses; allocate both of them
	err := p.AssignNext(&svc1)
	assert.Nil(t, err)
	assert.Equal(t, "192.168.1.0", svc1.Status.LoadBalancer.Ingress[0].IP, "svc1 was assigned the wrong address")
	err = p.AssignNext(&svc2)
	assert.Nil(t, err)
	assert.Equal(t, "192.168.1.1", svc2.Status.LoadBalancer.Ingress[0].IP, "svc2 was assigned the wrong address")

	// Same port: should fail
	err = p.AssignNext(&svc3)
	assert.NotNil(t, err)

	// Shared key, different ports: should succeed
	svc3.Spec.Ports = ports("tcp/25")
	err = p.AssignNext(&svc3)
	assert.Nil(t, err)
}

func TestPoolSize(t *testing.T) {
	p, err := NewLocalPool(localPoolTestLogger, purelbv1.ServiceGroupLocalSpec{
		V4Pool: &purelbv1.ServiceGroupAddressPool{
			Pool: "192.168.1.0/31",
		},
		V6Pool: &purelbv1.ServiceGroupAddressPool{
			Pool: "2001:470:1f07:98e:d62a:159b:41a3:93d3/128",
		},
	})
	assert.Nil(t, err, "Pool instantiation failed")
	assert.Equal(t, uint64(3), p.Size(), "Pool Size() failed")
}

func TestWhichFamilies(t *testing.T) {
	var (
		families []int
		err      error
	)
	p := mustLocalPool(t, "2001:470:1f07:98e:d62a:159b:41a3:93d3/128")
	svc := service("svc1", ports("tcp/80"), "sharing1")

	svc.Spec.IPFamilies = []v1.IPFamily{}
	families, err = p.whichFamilies(&svc)
	assert.Nil(t, err, "whichFamilies() failed")
	assert.ElementsMatch(t, []int{}, families, "incorrect empty families")

	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol}
	families, err = p.whichFamilies(&svc)
	assert.Nil(t, err, "whichFamilies() failed")
	assert.ElementsMatch(t, []int{nl.FAMILY_V4}, families, "incorrect empty families")

	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol}
	families, err = p.whichFamilies(&svc)
	assert.Nil(t, err, "whichFamilies() failed")
	assert.ElementsMatch(t, []int{nl.FAMILY_V6, nl.FAMILY_V4}, families, "incorrect empty families")
}

func sameStrings(t *testing.T, want []string, got []string) {
	sort.Strings(got)
	assert.Equal(t, want, got)
}
