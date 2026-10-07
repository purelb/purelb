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

package main

import (
	"net"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// recordingAnnouncer appends "<name>:<call>" to a shared log so tests can
// assert which announcers were called, and in what order.
type recordingAnnouncer struct {
	name string
	log  *[]string
}

func (r recordingAnnouncer) rec(call string) { *r.log = append(*r.log, r.name+":"+call) }

func (r recordingAnnouncer) SetConfig(*purelbv2.Config) error { r.rec("SetConfig"); return nil }
func (r recordingAnnouncer) SetClient(*k8s.Client)            { r.rec("SetClient") }
func (r recordingAnnouncer) SetBalancer(*v1.Service, []*discoveryv1.EndpointSlice) error {
	r.rec("SetBalancer")
	return nil
}
func (r recordingAnnouncer) DeleteBalancer(string, string, net.IP) error {
	r.rec("DeleteBalancer")
	return nil
}
func (r recordingAnnouncer) SetElection(*election.Election) { r.rec("SetElection") }
func (r recordingAnnouncer) Shutdown()                      { r.rec("Shutdown") }

func lbService(annotations map[string]string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc", Annotations: annotations},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		Status: v1.ServiceStatus{LoadBalancer: v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{{IP: "192.0.2.10"}},
		}},
	}
}

// A LoadBalancer Service with an address but no annotations at all was
// allocated by someone else -- k3s ServiceLB, for one, whose ingress IPs are
// node IPs. It used to slip past the brand check because the check was
// skipped whenever the annotation map was nil, so the node agent announced
// another controller's addresses (and the address guard would have treated
// node IPs as VIPs, dropping SSH and kubelet).
func TestServiceChangedSkipsUnbrandedServices(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		announced   bool
	}{
		{"nil annotations", nil, false},
		{"empty annotations", map[string]string{}, false},
		{"other allocator", map[string]string{purelbv2.BrandAnnotation: "SomeoneElse"}, false},
		{"PureLB brand", map[string]string{purelbv2.BrandAnnotation: purelbv2.Brand}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			c := &controller{
				logger:     log.NewNopLogger(),
				myNode:     "node1",
				announcers: []lbnodeagent.Announcer{recordingAnnouncer{"a", &calls}},
			}
			assert.Equal(t, k8s.SyncStateSuccess, c.ServiceChanged(lbService(tt.annotations), nil))
			if tt.announced {
				assert.Equal(t, []string{"a:SetBalancer"}, calls)
			} else {
				assert.Empty(t, calls)
			}
		})
	}
}

// The address guard is first in the announcer list. Its rules must exist
// before the local announcer adds an address (SetBalancer in order), and
// must outlive the address (DeleteBalancer and Shutdown in reverse).
func TestControllerAnnouncerOrder(t *testing.T) {
	var calls []string
	c := &controller{
		logger: log.NewNopLogger(),
		myNode: "node1",
		announcers: []lbnodeagent.Announcer{
			recordingAnnouncer{"guard", &calls},
			recordingAnnouncer{"local", &calls},
		},
	}
	c.ServiceChanged(lbService(map[string]string{purelbv2.BrandAnnotation: purelbv2.Brand}), nil)
	c.DeleteBalancer("ns/svc", "")
	c.Shutdown()
	assert.Equal(t, []string{
		"guard:SetBalancer", "local:SetBalancer",
		"local:DeleteBalancer", "guard:DeleteBalancer",
		"local:Shutdown", "guard:Shutdown",
	}, calls)

	// A Service that stops being a LoadBalancer is withdrawn through the
	// same reverse order.
	calls = nil
	notLB := lbService(map[string]string{purelbv2.BrandAnnotation: purelbv2.Brand})
	notLB.Spec.Type = v1.ServiceTypeNodePort
	c.ServiceChanged(notLB, nil)
	assert.Equal(t, []string{"local:DeleteBalancer", "guard:DeleteBalancer"}, calls)
}
