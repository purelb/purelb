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
	"testing"

	"github.com/stretchr/testify/assert"

	"purelb.io/internal/netbox/fake"
	purelbv1 "purelb.io/pkg/apis/v1"
)

func TestNetboxContains(t *testing.T) {
	svc1 := service("svc1", ports("tcp/80"), "sharing1")
	nsName := svc1.Namespace + "/" + svc1.Name

	nbp, err := NewNetboxPool(purelbv1.ServiceGroupNetboxSpec{URL: "url", Tenant: "tenant"})
	assert.Nil(t, err, "NewNetboxPool()")
	nbp.netbox = fake.NewNetbox("base", "tenant", "token") // patch the pool with a fake Netbox client

	ip1, err := nbp.AssignNext(&svc1)
	assert.Nil(t, err, "Netbox pool AssignNext() failed")

	assert.True(t, nbp.Contains(ip1), "address should have been contained in pool but wasn't")

	nbp.Release(ip1, nsName)
	assert.False(t, nbp.Contains(ip1), "address should not have been contained in pool but was")
}
