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

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/acnodal"
	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"
)

func (c *controller) SetBalancer(name string, svc *v1.Service, _ *v1.Endpoints) k8s.SyncState {
	if !c.synced {
		c.logger.Log("op", "allocateIP", "error", "controller not synced")
		return k8s.SyncStateError
	}

	// If the ClusterIP is malformed or not set we can't determine the
	// ipFamily to use.
	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	if clusterIP == nil {
		c.logger.Log("event", "clearAssignment", "reason", "noClusterIP")
		return k8s.SyncStateSuccess
	}

	// Check if the service already has an address
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		c.logger.Log("event", "ipAlreadySet")

		// if it's one of ours then we'll tell the allocator about it, in
		// case it didn't know. one example of this is at startup where
		// our allocation database is empty and we get notifications of
		// all the services. we can use the notifications to warm up our
		// database so we don't allocate the same address twice.
		if svc.Annotations != nil && svc.Annotations[purelbv1.BrandAnnotation] == purelbv1.Brand {
			if existingIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP); existingIP != nil {
				_, _ = c.ips.Assign(svc, existingIP)
			}
		}

		// If the service already has an address then we don't need to
		// allocate one.
		return k8s.SyncStateSuccess
	}

	pool, lbIP, err := c.allocateIP(name, svc)
	if err != nil {
		c.logger.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
		c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", name, err)
		return k8s.SyncStateSuccess
	}
	c.logger.Log("event", "ipAllocated", "ip", lbIP, "pool", pool, "service", name)
	c.client.Infof(svc, "IPAllocated", "Assigned IP %s from pool %s", lbIP, pool)

	// we have an IP selected somehow, so program the data plane
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP.String()}}

	// annotate the service as "ours" and annotate the pool from which
	// the address came
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[purelbv1.BrandAnnotation] = purelbv1.Brand
	svc.Annotations[purelbv1.PoolAnnotation] = pool

	if c.baseURL != nil {
		// Connect to the EGW
		egw, err := acnodal.New(c.baseURL.String(), "")
		if err != nil {
			c.logger.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
			c.client.Errorf(svc, "AllocationFailed", "Failed to create EGW service for %s: %s", svc.Name, err)
			return k8s.SyncStateError
		}

		// Look up the EGW group (which gives us the URL to create services)
		group, err := egw.GetGroup(*c.groupURL)
		if err != nil {
			c.logger.Log("op", "GetGroup", "group", c.groupURL, "error", err)
			c.client.Errorf(svc, "GetGroupFailed", "Failed to get group %s: %s", c.groupURL, err)
			return k8s.SyncStateError
		}

		// Announce the service to the EGW
		egwsvc, err := egw.AnnounceService(group.Links["create-service"], svc.Name, svc.Status.LoadBalancer.Ingress[0].IP)
		if err != nil {
			c.logger.Log("op", "AnnouncementFailed", "service", svc.Name, "error", err)
			c.client.Errorf(svc, "AnnouncementFailed", "Failed to announce service for %s: %s", svc.Name, err)
			return k8s.SyncStateError
		}
		svc.Annotations[purelbv1.GroupAnnotation] = egwsvc.Links["group"]
		svc.Annotations[purelbv1.ServiceAnnotation] = egwsvc.Links["self"]
		svc.Annotations[purelbv1.EndpointAnnotation] = egwsvc.Links["create-endpoint"]
	}

	return k8s.SyncStateSuccess
}

func (c *controller) allocateIP(key string, svc *v1.Service) (string, net.IP, error) {
	desiredGroup := svc.Annotations[purelbv1.DesiredGroupAnnotation]

	// If the user asked for a specific IP, try that.
	if svc.Spec.LoadBalancerIP != "" {

		// It doesn't make sense to use Spec.LoadBalancerIP *and*
		// DesiredGroupAnnotation because Spec.LoadBalancerIP is more
		// specific so DesiredGroupAnnotation can only cause problems. If
		// you're using Spec.LoadBalancerIP then you don't need
		// DesiredGroupAnnotation.
		if desiredGroup != "" {
			return "", nil, fmt.Errorf("spec.loadBalancerIP and DesiredGroupAnnotation are mutually exclusive, use Spec.LoadBalancerIP alone")
		}

		ip := net.ParseIP(svc.Spec.LoadBalancerIP)
		if ip == nil {
			return "", nil, fmt.Errorf("invalid spec.loadBalancerIP %q", svc.Spec.LoadBalancerIP)
		}
		pool, err := c.ips.Assign(svc, ip)
		if err != nil {
			return "", nil, err
		}
		return pool, ip, nil
	}

	// Otherwise, did the user ask for a specific pool?
	if desiredGroup != "" {
		ip, err := c.ips.AllocateFromPool(svc, desiredGroup)
		if err != nil {
			return "", nil, err
		}
		return desiredGroup, ip, nil
	}

	// Okay, in that case just bruteforce across all pools.
	return c.ips.Allocate(svc)
}
