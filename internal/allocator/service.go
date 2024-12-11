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

package allocator

import (
	"fmt"
	"net"

	"github.com/go-kit/kit/log"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"
)

// SetBalancer is the main entry point that handles LoadBalancer
// create/change events. It takes a Service and decides what to do
// based on that Service's configuration. It returns a k8s.SyncState
// value - SyncStateSuccess or SyncStateError.
func (c *controller) SetBalancer(svc *v1.Service, _ *v1.Endpoints) k8s.SyncState {
	nsName := svc.Namespace + "/" + svc.Name
	log := log.With(c.logger, "svc-name", nsName)

	if !c.synced {
		log.Log("op", "allocateIP", "error", "controller not synced")
		return k8s.SyncStateError
	}

	// If the user has specified an LB class and it's not ours then we
	// ignore the LB.
	if svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass != purelbv1.ServiceLBClass {
		log.Log("event", "ignore", "reason", "user has specified another class", "class", *svc.Spec.LoadBalancerClass)
		return k8s.SyncStateSuccess
	}

	// If we are not configured to be the default announcer then we
	// ignore services with no explicit LoadBalancerClass.
	if !c.isDefault && svc.Spec.LoadBalancerClass == nil {
		log.Log("event", "ignore", "reason", "service has no explicit LBClass and PureLB is not the default announcer")
		return k8s.SyncStateSuccess
	}

	// Ensure that the Service has an annotation map (so we can assume
	// it has one later).
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}

	// If the service isn't a LoadBalancer then we might need to clean
	// up. It might have been a load balancer before and the user might
	// have changed it to tell us to release the address
	if svc.Spec.Type != "LoadBalancer" {

		// If it's ours then we need to clean up
		if _, hasAnnotation := svc.Annotations[purelbv1.PoolAnnotation]; hasAnnotation {

			// If it has an address then release it
			if len(svc.Status.LoadBalancer.Ingress) > 0 {
				log.Log("event", "unassign", "ingress-address", svc.Status.LoadBalancer.Ingress, "reason", "type is not LoadBalancer")
				c.client.Infof(svc, "AddressReleased", fmt.Sprintf("Service is Type %s, not LoadBalancer", svc.Spec.Type))
				if err := c.ips.Unassign(nsName); err != nil {
					c.logger.Log("event", "unassign", "error", err)
					return k8s.SyncStateError
				}
				svc.Status.LoadBalancer.Ingress = nil
			}
		}

		// "Un-own" the service. Remove PureLB's Pool annotation so
		// we'll re-allocate if the user flips this service back to a
		// LoadBalancer
		delete(svc.Annotations, purelbv1.PoolAnnotation)

		// It's not a LoadBalancer so there's nothing more for us to do
		return k8s.SyncStateSuccess
	}

	// If the ClusterIP is malformed or not set we can't determine the
	// ipFamily to use.
	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	if clusterIP == nil {
		log.Log("event", "clearAssignment", "reason", "noClusterIP")
		return k8s.SyncStateSuccess
	}

	// Check if the service already has an address
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		log.Log("event", "hasIngress", "ingress", svc.Status.LoadBalancer.Ingress)

		// if it's one of ours then we'll tell the allocator about it, in
		// case it didn't know but needs to. one example of this is at
		// startup where our allocation database is empty and we get
		// notifications of all the services. we can use the notifications
		// to warm up our database so we don't allocate the same address
		// twice. another example is when the user edits a service,
		// although that would be better handled in a webhook.
		if svc.Annotations[purelbv1.BrandAnnotation] == purelbv1.Brand {
			// The service has an IP so we'll attempt to formally allocate
			// it. If something goes wrong then we'll log it but won't do
			// anything else so we don't cause more trouble.
			if err := c.ips.NotifyExisting(svc); err != nil {
				log.Log("event", "notifyFailure", "ingress-address", svc.Status.LoadBalancer.Ingress, "reason", err.Error())
			}
		}

		// If the service already has an address then we don't need to
		// allocate one.
		return k8s.SyncStateSuccess
	}

	// Annotate the service as "ours"
	svc.Annotations[purelbv1.BrandAnnotation] = purelbv1.Brand

	if err := c.ips.Allocate(svc); err != nil {
		log.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
		c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", nsName, err)
		return k8s.SyncStateSuccess
	}

	return k8s.SyncStateSuccess
}
