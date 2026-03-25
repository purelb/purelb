// Copyright 2017 Google Inc.
// Copyright 2020 Acnodal Inc.
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
	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	"purelb.io/internal/local"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"

	"github.com/go-kit/log"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

type controller struct {
	client     k8s.ServiceEvent
	logger     log.Logger
	myNode     string
	announcers []lbnodeagent.Announcer
}

// NewController configures a new controller. If error is non-nil then
// the controller object shouldn't be used.
func NewController(l log.Logger, myNode string) (*controller, error) {
	con := &controller{
		logger: l,
		myNode: myNode,
		announcers: []lbnodeagent.Announcer{
			local.NewAnnouncer(l, myNode),
		},
	}

	return con, nil
}

// SetClient configures this controller and its announcers to use the
// provided client.
func (c *controller) SetClient(client *k8s.Client) {
	c.client = client
	for _, announcer := range c.announcers {
		announcer.SetClient(client)
	}
}

func (c *controller) ServiceChanged(svc *v1.Service, epSlices []*discoveryv1.EndpointSlice) k8s.SyncState {
	nsName := svc.Namespace + "/" + svc.Name

	// If the service isn't a LoadBalancer Type then we might need to
	// clean up. It might have been a load balancer before and the user
	// might have changed it (for example, to NodePort) to tell us to
	// release the address.
	if svc.Spec.Type != "LoadBalancer" && svc.Annotations[purelbv2.BrandAnnotation] == purelbv2.Brand {

		// Remove our annotations in case the user wants the service to be
		// managed by something else
		delete(svc.Annotations, purelbv2.BrandAnnotation)
		delete(svc.Annotations, purelbv2.AnnounceAnnotation)
		delete(svc.Annotations, purelbv2.AnnounceAnnotation+"-IPv4")
		delete(svc.Annotations, purelbv2.AnnounceAnnotation+"-IPv6")
		delete(svc.Annotations, purelbv2.AnnounceAnnotation+"-unknown")

		logging.Info(c.logger, "op", "withdraw", "reason", "notLoadBalancerType", "node", c.myNode, "service", nsName)
		c.DeleteBalancer(nsName)

		// This is a "best-effort" operation. If it fails there's not much
		// point in retrying because it's unlikely that anything will
		// change to allow the retry to succeed. We'll just end up
		// spamming the logs.
		return k8s.SyncStateSuccess
	}

	// If the service has no addresses assigned then there's nothing
	// that we can do.
	if len(svc.Status.LoadBalancer.Ingress) < 1 {
		logging.Debug(c.logger, "msg", "noAddressAllocated", "node", c.myNode, "service", nsName)
		return k8s.SyncStateSuccess
	}

	// If we didn't allocate the address then we shouldn't announce it.
	if svc.Annotations != nil && svc.Annotations[purelbv2.BrandAnnotation] != purelbv2.Brand {
		logging.Debug(c.logger, "msg", "notAllocatedByPureLB", "node", c.myNode, "service", nsName)
		return k8s.SyncStateSuccess
	}

	// give each announcer a chance to announce
	announceError := k8s.SyncStateSuccess
	for _, announcer := range c.announcers {
		if err := announcer.SetBalancer(svc, epSlices); err != nil {
			logging.Info(c.logger, "op", "setBalancer", "error", err, "msg", "failed to announce service")
			announceError = k8s.SyncStateError
		}
	}

	return announceError
}

// DeleteBalancer deletes any changes that we have made on behalf of
// nsName. nsName must be a namespaced name string, e.g.,
// "purelb/example".
func (c *controller) DeleteBalancer(nsName string) k8s.SyncState {
	retval := k8s.SyncStateSuccess

	logging.Debug(c.logger, "op", "deleteBalancer", "name", nsName)

	for _, announcer := range c.announcers {
		if err := announcer.DeleteBalancer(nsName, "cluster event", nil); err != nil {
			logging.Info(c.logger, "op", "deleteBalancer", "error", err, "msg", "failed to clear balancer state")
			retval = k8s.SyncStateError
		}
	}

	return retval
}

func (c *controller) SetConfig(cfg *purelbv2.Config) k8s.SyncState {
	retval := k8s.SyncStateReprocessAll

	for _, announcer := range c.announcers {
		if err := announcer.SetConfig(cfg); err != nil {
			logging.Info(c.logger, "op", "setConfig", "error", err)
			retval = k8s.SyncStateError
		}
	}

	return retval
}

func (c *controller) SetElection(election *election.Election) {
	for _, announcer := range c.announcers {
		announcer.SetElection(election)
	}
}

func (c *controller) Shutdown() {
	for _, announcer := range c.announcers {
		announcer.Shutdown()
	}
}
