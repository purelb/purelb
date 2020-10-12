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

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

var (
	key1 = Key{Sharing: "sharing1"}
	key2 = Key{Sharing: "sharing2"}
	http = Port{Proto: string(v1.ProtocolTCP), Port: 80}
	smtp = Port{Proto: string(v1.ProtocolTCP), Port: 25}
)

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
	p.Release(ip, "svc2")
	assert.Equal(t, 2, p.InUse()) // the address isn't fully released yet
	p.Release(ip, "svc3")
	assert.Equal(t, 1, p.InUse()) // the address isn't fully released yet
}

func TestServicesOn(t *testing.T) {
	ip2 := net.ParseIP("192.168.1.2")
	p := mustLocalPool(t, "192.168.1.1/32")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/25"), "sharing1")

	p.Assign(ip2, &svc1)
	assert.Equal(t, []string{"svc1"}, p.servicesOnIP(ip2))
	p.Assign(ip2, &svc2)
	sameStrings(t, []string{"svc1", "svc2"}, p.servicesOnIP(ip2))
	p.Release(ip2, "svc1")
	assert.Equal(t, []string{"svc2"}, p.servicesOnIP(ip2))
}

func TestSharingKeys(t *testing.T) {
	ip := net.ParseIP("192.168.1.1")
	p := mustLocalPool(t, "192.168.1.1/32")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")

	p.Assign(ip, &svc1)
	assert.Equal(t, &key1, p.SharingKey(ip))
	p.Release(ip, "svc1")
	assert.Nil(t, p.SharingKey(ip))
}

func TestAvailable(t *testing.T) {
	p := mustLocalPool(t, "192.168.1.1/32")
	ip := net.ParseIP("192.168.1.1")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")

	// no assignment, should be available
	assert.Nil(t, p.Available(ip, &svc1))

	p.Assign(ip, &svc1)

	// same service can "share" with or without the key
	assert.Nil(t, p.Available(ip, &svc1))
	svc1X := service("svc1", ports("tcp/80"), "XshareX")
	assert.Nil(t, p.Available(ip, &svc1X))

	svc2 := service("svc2", ports("tcp/80"), "")
	// other service, no key: no share
	assert.NotNil(t, p.Available(ip, &svc2))
	// other service, has key, same port: no share
	svc2 = service("svc2", ports("tcp/80"), "sharing1")
	assert.NotNil(t, p.Available(ip, &svc2))
	// other service, has key, different port: share
	svc2 = service("svc2", ports("tcp/25"), "sharing1")
	assert.Nil(t, p.Available(ip, &svc2))
}

func TestAssignNext(t *testing.T) {
	p := mustLocalPool(t, "192.168.1.0/31")
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	svc2 := service("svc2", ports("tcp/80"), "sharing2")
	svc3 := service("svc3", ports("tcp/80"), "sharing2")

	// The pool has two addresses; allocate both of them
	ip, err := p.AssignNext(&svc1)
	assert.Nil(t, err)
	assert.Equal(t, net.ParseIP("192.168.1.0"), ip)
	ip, err = p.AssignNext(&svc2)
	assert.Nil(t, err)
	assert.Equal(t, net.ParseIP("192.168.1.1"), ip)

	// Same port: should fail
	_, err = p.AssignNext(&svc3)
	assert.NotNil(t, err)

	// Shared key, different ports: should succeed
	svc3.Spec.Ports = ports("tcp/25")
	_, err = p.AssignNext(&svc3)
	assert.Nil(t, err)
}

func sameStrings(t *testing.T, want []string, got []string) {
	sort.Strings(got)
	assert.Equal(t, want, got)
}
