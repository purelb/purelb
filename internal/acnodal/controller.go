// Copyright 2017 Google Inc.
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

package acnodal

import (
	"reflect"

	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"

	"github.com/go-kit/kit/log"
	"k8s.io/api/core/v1"
)

const (
	GroupURL = "/api/egw/groups/b321256d-31b7-4209-bd76-28dec3c77c25"  // FIXME: use c.ips.Pool(name) but it's safer to hard-code for now
)

type controller struct {
	client *k8s.Client
	synced bool
	config *config.Config
	ips    *Allocator
}

func NewController(ips *Allocator) (*controller, error) {
	con := &controller{
		ips: ips,
	}

	return con, nil
}

func (c *controller) SetClient(client *k8s.Client) {
	c.client = client
}

func (c *controller) SetBalancer(l log.Logger, name string, svcRo *v1.Service, _ *v1.Endpoints) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of service update")
	defer l.Log("event", "endUpdate", "msg", "end of service update")

	if svcRo == nil {
		c.deleteBalancer(l, name)
		// There might be other LBs stuck waiting for an IP, so when
		// we delete a balancer we should reprocess all of them to
		// check for newly feasible balancers.
		return k8s.SyncStateReprocessAll
	}

	if c.config == nil {
		// Config hasn't been read, nothing we can do just yet.
		l.Log("event", "noConfig", "msg", "not processing, still waiting for config")
		return k8s.SyncStateSuccess
	}

	// Making a copy unconditionally is a bit wasteful, since we don't
	// always need to update the service. But, making an unconditional
	// copy makes the code much easier to follow, and we have a GC for
	// a reason.
	svc := svcRo.DeepCopy()
	if !c.convergeBalancer(l, name, svc) {
		return k8s.SyncStateError
	}
	if reflect.DeepEqual(svcRo, svc) {
		l.Log("event", "noChange", "msg", "service converged, no change")
		return k8s.SyncStateSuccess
	}

	var err error

	// Connect to the EGW
	egw, err := New("", "")
	if err != nil {
		l.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
		c.client.Errorf(svc, "AllocationFailed", "Failed to create EGW service for %s: %s", svc.Name, err)
		return k8s.SyncStateError
	}

	// Look up the EGW group (which gives us the URL to create services)
	group, err := egw.GetGroup(GroupURL)
	if err != nil {
		l.Log("op", "GetGroup", "group", GroupURL, "error", err)
		c.client.Errorf(svc, "GetGroupFailed", "Failed to get group %s: %s", GroupURL, err)
		return k8s.SyncStateError
	}

	// Announce the service to the EGW
	egwsvc, err := egw.AnnounceService(group.Links["create-service"], name, svc.Status.LoadBalancer.Ingress[0].IP)
	if err != nil {
		l.Log("op", "AnnouncementFailed", "service", svc.Name, "error", err)
		c.client.Errorf(svc, "AnnouncementFailed", "Failed to announce service for %s: %s", svc.Name, err)
		return k8s.SyncStateError
	}
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations["acnodal.io/groupURL"] = egwsvc.Links["group"]
	svc.Annotations["acnodal.io/serviceURL"] = egwsvc.Links["self"]
	svc.Annotations["acnodal.io/endpointcreateURL"] = egwsvc.Links["create-endpoint"]

	if !(reflect.DeepEqual(svcRo.Annotations, svc.Annotations) && reflect.DeepEqual(svcRo.Spec, svc.Spec)) {
		svcRo, err = c.client.Update(svc)
		if err != nil {
			l.Log("op", "updateService", "error", err, "msg", "failed to update service")
			return k8s.SyncStateError
		}
	}
	if !reflect.DeepEqual(svcRo.Status, svc.Status) {
		var st v1.ServiceStatus
		st, svc = svc.Status, svcRo.DeepCopy()
		svc.Status = st
		if err = c.client.UpdateStatus(svc); err != nil {
			l.Log("op", "updateServiceStatus", "error", err, "msg", "failed to update service status")
			return k8s.SyncStateError
		}
	}
	l.Log("event", "serviceUpdated", "msg", "updated service object")

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(l log.Logger, name string) {
	// FIXME: notify the EGW
}

func (c *controller) SetConfig(l log.Logger, cfg *config.Config) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of config update")
	defer l.Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		l.Log("op", "setConfig", "error", "no MetalLB configuration in cluster", "msg", "configuration is missing, MetalLB will not function")
		return k8s.SyncStateError
	}

	c.config = cfg
	return k8s.SyncStateReprocessAll
}

func (c *controller) MarkSynced(l log.Logger) {
	c.synced = true
	l.Log("event", "stateSynced", "msg", "controller synced, can allocate IPs now")
}
