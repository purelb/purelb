// Copyright 2021 Acnodal Inc.
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
	"testing"

	"github.com/go-kit/kit/log"
	"github.com/stretchr/testify/assert"

	"purelb.io/internal/netbox/fake"
	purelbv1 "purelb.io/pkg/apis/v1"
)

var (
	netboxPoolTestLogger = log.NewNopLogger()
)

func TestNetboxContains(t *testing.T) {
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	nsName := namespacedName(&svc1)

	nbp, err := NewNetboxPool("unittest", netboxPoolTestLogger, purelbv1.ServiceGroupNetboxSpec{URL: "url", Tenant: "tenant"})
	assert.Nil(t, err, "NewNetboxPool()")
	nbp.netbox = fake.NewNetbox("base", "tenant", "token") // patch the pool with a fake Netbox client

	err = nbp.AssignNext(&svc1)
	assert.Nil(t, err, "Netbox pool AssignNext() failed")

	assigned := net.ParseIP(svc1.Status.LoadBalancer.Ingress[0].IP)
	assert.NotNil(t, assigned, "service was assigned an unparseable IP")
	assert.True(t, nbp.Contains(assigned), "address should have been contained in pool but wasn't")

	nbp.Release(nsName)
	assert.False(t, nbp.Contains(assigned), "address should not have been contained in pool but was")
}
