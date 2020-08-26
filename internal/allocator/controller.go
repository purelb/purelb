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

package allocator

import (
	"reflect"

	"purelb.io/internal/config"
	"purelb.io/internal/k8s"

	"github.com/go-kit/kit/log"
	"k8s.io/api/core/v1"
)

type controller struct {
	client k8s.Service
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

	if svcRo.Spec.Type != "LoadBalancer" {
		// Not a LoadBalancer, early exit
		l.Log("event", "clearAssignment", "reason", "notLoadBalancer", "msg", "not a LoadBalancer")
		return k8s.SyncStateSuccess
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
	if c.ips.Unassign(name) {
		l.Log("event", "serviceDeleted", "msg", "service deleted")
	}
}

func (c *controller) SetConfig(l log.Logger, cfg *config.Config) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of config update")
	defer l.Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		l.Log("op", "setConfig", "error", "no PureLB configuration in cluster", "msg", "configuration is missing, PureLB will not function")
		return k8s.SyncStateError
	}

	if err := c.ips.SetPools(cfg.Pools); err != nil {
		l.Log("op", "setConfig", "error", err, "msg", "applying new configuration failed")
		return k8s.SyncStateError
	}
	c.config = cfg
	return k8s.SyncStateReprocessAll
}

func (c *controller) MarkSynced(l log.Logger) {
	c.synced = true
	l.Log("event", "stateSynced", "msg", "controller synced, can allocate IPs now")
}
