// Copyright 2020 Acnodal, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acnodal

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

var (
	ServicePort80  = v1.ServicePort{Port: 80}
	EndpointPort80 = v1.EndpointPort{Port: 80}
	EndpointPort81 = v1.EndpointPort{Port: 81}
)

const (
	GroupName       = "sample"
	ServiceName     = "test-service"
	ServiceAddress  = "192.168.1.27"
	EndpointName    = "test-endpoint"
	EndpointAddress = "10.42.27.42"
	GroupURL        = "/api/egw/accounts/sample/groups/sample"
)

func TestMain(m *testing.M) {
	flag.Parse()

	if testing.Short() {
		fmt.Println("Skipping egw tests because short testing was requested.")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func MustEGW(t *testing.T) EGW {
	e, err := NewEGW("")
	if err != nil {
		t.Fatal("initializing EGW", err)
	}
	return e
}

func GetGroup(t *testing.T, e EGW, url string) EGWGroupResponse {
	g, err := e.GetGroup()
	if err != nil {
		t.Fatal("getting group", err)
	}
	return g
}

func TestGetGroup(t *testing.T) {
	e := MustEGW(t)
	g := GetGroup(t, e, GroupURL)
	gotName := g.Group.ObjectMeta.Name
	assert.Equal(t, gotName, GroupName, "group name mismatch")
}

func TestAnnouncements(t *testing.T) {
	e := MustEGW(t)
	g := GetGroup(t, e, GroupURL)

	// announce a service
	svc, err := e.AnnounceService(g.Links["create-service"], ServiceName, []v1.ServicePort{ServicePort80})
	if err != nil {
		t.Fatal("announcing service", err)
	}
	assert.Equal(t, svc.Links["group"], GroupURL, "group url mismatch")

	// announce an endpoint on that service
	err = e.AnnounceEndpoint(svc.Links["create-endpoint"], EndpointAddress, EndpointPort80)
	if err != nil {
		t.Errorf("got error %+v", err)
	}

	// announce another endpoint on that service
	err = e.AnnounceEndpoint(svc.Links["create-endpoint"], EndpointAddress, EndpointPort81)
	if err != nil {
		t.Errorf("got error %+v", err)
	}
}
