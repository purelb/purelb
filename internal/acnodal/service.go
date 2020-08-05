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
	"fmt"
	"net"

	"github.com/go-kit/kit/log"
	v1 "k8s.io/api/core/v1"

	"go.universe.tf/metallb/internal/pool"
)

func (c *controller) convergeBalancer(l log.Logger, key string, svc *v1.Service) bool {
	var lbIP net.IP

	// Not a LoadBalancer, early exit. It might have been a balancer
	// in the past, so we still need to clear LB state.
	if svc.Spec.Type != "LoadBalancer" {
		l.Log("event", "clearAssignment", "reason", "notLoadBalancer", "msg", "not a LoadBalancer")
		c.clearServiceState(key, svc)
		// Early return, we explicitly do *not* want to reallocate
		// an IP.
		return true
	}

	// If the ClusterIP is malformed or not set we can't determine the
	// ipFamily to use.
	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	if clusterIP == nil {
		l.Log("event", "clearAssignment", "reason", "noClusterIP", "msg", "No ClusterIP")
		c.clearServiceState(key, svc)
		return true
	}

	// The assigned LB IP is the end state of convergence. If there's
	// none or a malformed one, nuke all controlled state so that we
	// start converging from a clean slate.
	if len(svc.Status.LoadBalancer.Ingress) == 1 {
		lbIP = net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	}
	if lbIP == nil {
		c.clearServiceState(key, svc)
	}

	// Clear the lbIP if it has a different ipFamily compared to the clusterIP.
	// (this should not happen since the "ipFamily" of a service is immutable)
	if (clusterIP.To4() == nil) != (lbIP.To4() == nil) {
		c.clearServiceState(key, svc)
		lbIP = nil
	}

	// User set or changed the desired LB IP, nuke the
	// state. allocateIP will pay attention to LoadBalancerIP and try
	// to meet the user's demands.
	if svc.Spec.LoadBalancerIP != "" && svc.Spec.LoadBalancerIP != lbIP.String() {
		l.Log("event", "clearAssignment", "reason", "differentIPRequested", "msg", "user requested a different IP than the one currently assigned")
		c.clearServiceState(key, svc)
		lbIP = nil
	}

	// If lbIP is still nil at this point, try to allocate.
	if lbIP == nil {
		if !c.synced {
			l.Log("op", "allocateIP", "error", "controller not synced", "msg", "controller not synced yet, cannot allocate IP; will retry after sync")
			return false
		}
		ip, err := c.allocateIP(key, svc)
		if err != nil {
			l.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
			c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", key, err)
			// The outer controller loop will retry converging this
			// service when another service gets deleted, so there's
			// nothing to do here but wait to get called again later.
			return true
		}
		lbIP = ip
		l.Log("event", "ipAllocated", "ip", lbIP, "msg", "IP address assigned by controller")
		c.client.Infof(svc, "IPAllocated", "Assigned IP %q", lbIP)
	}

	if lbIP == nil {
		l.Log("bug", "true", "msg", "internal error: failed to allocate an IP, but did not exit convergeService early!")
		c.client.Errorf(svc, "InternalError", "didn't allocate an IP but also did not fail")
		c.clearServiceState(key, svc)
		return true
	}

	pool := c.ips.Pool(key)
	if pool == "" || c.config.Pools[pool] == nil {
		l.Log("bug", "true", "ip", lbIP, "msg", "internal error: allocated IP has no matching address pool")
		c.client.Errorf(svc, "InternalError", "allocated an IP that has no pool")
		c.clearServiceState(key, svc)
		return true
	}

	// At this point, we have an IP selected somehow, all that remains
	// is to program the data plane.
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP.String()}}
	return true
}

// clearServiceState clears all fields that are actively managed by
// this controller.
func (c *controller) clearServiceState(key string, svc *v1.Service) {
	c.ips.Unassign(key)
	svc.Status.LoadBalancer = v1.LoadBalancerStatus{}
}

func (c *controller) allocateIP(key string, svc *v1.Service) (net.IP, error) {
	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	if clusterIP == nil {
		// (we should never get here because the caller ensured that Spec.ClusterIP != nil)
		return nil, fmt.Errorf("invalid ClusterIP [%s], can't determine family", svc.Spec.ClusterIP)
	}
	isIPv6 := clusterIP.To4() == nil

	// If the user asked for a specific IP, try that.
	if svc.Spec.LoadBalancerIP != "" {
		ip := net.ParseIP(svc.Spec.LoadBalancerIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid spec.loadBalancerIP %q", svc.Spec.LoadBalancerIP)
		}
		if (ip.To4() == nil) != isIPv6 {
			return nil, fmt.Errorf("requested spec.loadBalancerIP %q does not match the ipFamily of the service", svc.Spec.LoadBalancerIP)
		}
		if err := c.ips.Assign(key, ip, pool.Ports(svc), pool.SharingKey(svc), pool.BackendKey(svc)); err != nil {
			return nil, err
		}
		return ip, nil
	}

	// Otherwise, did the user ask for a specific pool?
	desiredPool := svc.Annotations["metallb.universe.tf/address-pool"]
	if desiredPool != "" {
		ip, err := c.ips.AllocateFromPool(key, isIPv6, desiredPool, pool.Ports(svc), pool.SharingKey(svc), pool.BackendKey(svc))
		if err != nil {
			return nil, err
		}
		return ip, nil
	}

	// Okay, in that case just bruteforce across all pools.
	return c.ips.Allocate(key, isIPv6, pool.Ports(svc), pool.SharingKey(svc), pool.BackendKey(svc))
}
