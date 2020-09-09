// Copyright 2020 Acnodal Inc.
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
	"net/url"
	"reflect"

	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"k8s.io/api/core/v1"
)

type controller struct {
	client   k8s.Service
	synced   bool
	ips      *Allocator
	baseURL  *url.URL
	groupURL *string
	logger   log.Logger
}

func NewController(l log.Logger, ips *Allocator) (*controller, error) {
	con := &controller{
		logger: l,
		ips: ips,
	}

	return con, nil
}

func (c *controller) SetClient(client *k8s.Client) {
	c.client = client
}

func (c *controller) SetBalancer(name string, svcRo *v1.Service, _ *v1.Endpoints) k8s.SyncState {
	c.logger.Log("event", "startUpdate", "msg", "start of service update")
	defer c.logger.Log("event", "endUpdate", "msg", "end of service update")

	if svcRo == nil {
		c.deleteBalancer(name)
		// There might be other LBs stuck waiting for an IP, so when
		// we delete a balancer we should reprocess all of them to
		// check for newly feasible balancers.
		return k8s.SyncStateReprocessAll
	}

	// Making a copy unconditionally is a bit wasteful, since we don't
	// always need to update the service. But, making an unconditional
	// copy makes the code much easier to follow, and we have a GC for
	// a reason.
	svc := svcRo.DeepCopy()
	if !c.convergeBalancer(name, svc) {
		return k8s.SyncStateError
	}
	if reflect.DeepEqual(svcRo, svc) {
		c.logger.Log("event", "noChange", "msg", "service converged, no change")
		return k8s.SyncStateSuccess
	}

	var err error

	if !(reflect.DeepEqual(svcRo.Annotations, svc.Annotations) && reflect.DeepEqual(svcRo.Spec, svc.Spec)) {
		svcRo, err = c.client.Update(svc)
		if err != nil {
			c.logger.Log("op", "updateService", "error", err, "msg", "failed to update service")
			return k8s.SyncStateError
		}
	}
	if !reflect.DeepEqual(svcRo.Status, svc.Status) {
		var st v1.ServiceStatus
		st, svc = svc.Status, svcRo.DeepCopy()
		svc.Status = st
		if err = c.client.UpdateStatus(svc); err != nil {
			c.logger.Log("op", "updateServiceStatus", "error", err, "msg", "failed to update service status")
			return k8s.SyncStateError
		}
	}
	c.logger.Log("event", "serviceUpdated", "msg", "updated service object")

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(name string) {
	if c.ips.Unassign(name) {
		c.logger.Log("event", "serviceDeleted", "msg", "service deleted")
	}
}

func (c *controller) SetConfig(cfg *purelbv1.Config) k8s.SyncState {
	c.logger.Log("event", "startUpdate", "msg", "start of config update")
	defer c.logger.Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		c.logger.Log("op", "setConfig", "error", "no PureLB configuration in cluster", "msg", "configuration is missing, PureLB will not function")
		return k8s.SyncStateError
	}

	if err := c.ips.SetPools(cfg.Groups); err != nil {
		c.logger.Log("op", "setConfig", "error", err)
		return k8s.SyncStateError
	}

	// see if there's an EGW config. if so then we'll announce new
	// services to the EGW
	c.groupURL = nil
	c.baseURL = nil
	for _, group := range cfg.Groups {
		if group.Spec.EGW != nil {
			c.groupURL = &group.Spec.EGW.URL
			// Use the hostname from the service group, but reset the path.  EGW
			// and Netbox each have their own API URL schemes so we only need
			// the protocol, host, port, credentials, etc.
			url, err := url.Parse(*c.groupURL)
			if err != nil {
				c.logger.Log("op", "setConfig", "error", err)
				return k8s.SyncStateError
			}
			url.Path = ""
			c.baseURL = url
		}
	}

	return k8s.SyncStateReprocessAll
}

func (c *controller) MarkSynced() {
	c.synced = true
	c.logger.Log("event", "stateSynced", "msg", "controller synced, can allocate IPs now")
}
