// Copyright 2017 Google Inc.
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

package allocator

import (
	"sync/atomic"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"

	"github.com/go-kit/log"
)

// Controller provides an event-handling interface for the k8s client
// to use.
type Controller interface {
	SetClient(*k8s.Client)
	SetConfig(*purelbv2.Config) k8s.SyncState
	SetBalancer(*v1.Service, []*discoveryv1.EndpointSlice) k8s.SyncState
	DeleteBalancer(string) k8s.SyncState
	MarkSynced()
	Shutdown()
}

type controller struct {
	client k8s.ServiceEvent
	synced bool
	ips    *Allocator
	// installNamespace is the namespace PureLB runs in, resolved once by
	// the caller so that this controller and the CR controller cannot
	// disagree about it. Empty when it could not be determined.
	installNamespace string
	groupURL         *string
	logger           log.Logger
	isDefault        atomic.Bool
}

// NewController configures a new controller. If error is non-nil then
// the controller object shouldn't be used.
func NewController(l log.Logger, ips *Allocator, installNamespace string) (Controller, error) {
	con := &controller{
		logger:           l,
		ips:              ips,
		installNamespace: installNamespace,
	}

	return con, nil
}

func (c *controller) SetClient(client *k8s.Client) {
	c.client = client
	c.ips.SetClient(client)
	c.ips.SetServiceGroupStatusWriter(client)
	c.ips.SetListServices(client.ListServices)
	client.SetPoolStatusPublisher(c.ips.PublishAllSGStatus)

	if c.installNamespace == "" {
		logging.Info(c.logger, "op", "setClient",
			"msg", "install namespace unknown, multi-pool allocation will not work")
		return
	}
	c.ips.SetActiveSubnets(client.ActiveSubnets, c.installNamespace)
}

func (c *controller) DeleteBalancer(name string) k8s.SyncState {
	pools := c.ips.Pools()
	if err := c.ips.Unassign(pools, name); err != nil {
		logging.Info(c.logger, "op", "deleteBalancer", "error", err)
		return k8s.SyncStateError
	}

	logging.Debug(c.logger, "op", "deleteBalancer", "msg", "service deleted successfully")
	return k8s.SyncStateReprocessAll
}

// reportOutOfScope surfaces ServiceGroups that were listed but ignored
// because they live outside the install namespace.
//
// This runs in the allocator only. The filtering happens in a code path both
// binaries share, so emitting events there would produce one per node per
// reconcile; the node agent logs its own copy locally instead.
func (c *controller) reportOutOfScope(dropped []*purelbv2.ServiceGroup) {
	serviceGroupsOutOfScope.Reset()
	for _, g := range dropped {
		kind := "local"
		switch {
		case g.Spec.Remote != nil:
			kind = "remote"
		case g.Spec.External != nil:
			kind = "external"
		}
		serviceGroupsOutOfScope.WithLabelValues(g.Namespace, g.Name, kind).Set(1)

		// A remote group vanishing withdraws addresses from every node; a
		// local one only makes its pool unallocatable. Name the difference,
		// because it decides how urgently an operator has to act.
		impact := "its pool is not allocatable"
		if kind == "remote" {
			impact = "addresses allocated from it are withdrawn from every node"
		}
		c.client.Errorf(g, "ServiceGroupOutOfScope",
			"Ignored: ServiceGroups are read only from the PureLB install namespace, and %s is not it. This group is %s, so %s. Move it: kubectl get servicegroup %s -n %s -o yaml | sed 's/namespace: %s/namespace: <install-namespace>/' | kubectl apply -f -",
			g.Namespace, kind, impact, g.Name, g.Namespace, g.Namespace)
		logging.Info(c.logger, "op", "setConfig", "event", "serviceGroupOutOfScope",
			"sg", g.Namespace+"/"+g.Name, "type", kind, "msg", "ignored: not in the install namespace")
	}
}

func (c *controller) SetConfig(cfg *purelbv2.Config) k8s.SyncState {
	defer logging.Debug(c.logger, "op", "setConfig", "msg", "config updated")

	if cfg == nil {
		logging.Info(c.logger, "op", "setConfig", "error", "no PureLB configuration in cluster", "msg", "PureLB will not function")
		return k8s.SyncStateError
	}

	c.reportOutOfScope(cfg.DroppedGroups)

	if err := c.ips.SetPools(cfg.Groups); err != nil {
		logging.Info(c.logger, "op", "setConfig", "error", err)
		return k8s.SyncStateError
	}

	// Cache the config that indicates if we are the default Service
	// announcer.
	c.isDefault.Store(cfg.DefaultAnnouncer)

	return k8s.SyncStateReprocessAll
}

func (c *controller) MarkSynced() {
	c.synced = true
	logging.Info(c.logger, "op", "markSynced", "msg", "controller synced, can allocate IPs now")
}

func (c *controller) Shutdown() {
	logging.Info(c.logger, "op", "shutdown")
	c.ips.closeAllSidecarConns()
}
