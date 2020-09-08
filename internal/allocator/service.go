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
	"fmt"
	"net"

	"github.com/go-kit/kit/log"
	"k8s.io/api/core/v1"

	"purelb.io/internal/acnodal"
)

const (
	brand                 string = "PureLB"
	brandAnnotation       string = "purelb.io/allocated-by"
	poolAnnotation        string = "purelb.io/allocated-from"
	sharingAnnotation     string = "purelb.io/allow-shared-ip"
	desiredPoolAnnotation string = "purelb.io/address-pool"
)

func (c *controller) convergeBalancer(l log.Logger, key string, svc *v1.Service) bool {
	if !c.synced {
		l.Log("op", "allocateIP", "error", "controller not synced")
		return false
	}

	// If the ClusterIP is malformed or not set we can't determine the
	// ipFamily to use.
	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	if clusterIP == nil {
		l.Log("event", "clearAssignment", "reason", "noClusterIP")
		return true
	}

	// If the service already has an address then we don't need to
	// allocate one.
	if len(svc.Status.LoadBalancer.Ingress) == 1 {
		if existingIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP); existingIP != nil {
			l.Log("event", "ipAlreadySet")
			return true
		}
	}

	pool, lbIP, err := c.allocateIP(key, svc, clusterIP.To4() == nil)
	if err != nil {
		l.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
		c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", key, err)
		return true
	}
	l.Log("event", "ipAllocated", "ip", lbIP, "pool", pool, "msg", "IP address assigned by controller")
	c.client.Infof(svc, "IPAllocated", "Assigned IP %q", lbIP)

	// we have an IP selected somehow, so program the data plane
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP.String()}}

	// annotate the service as "ours" and annotate the pool from which
	// the address came
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
 	svc.Annotations[brandAnnotation] = brand
	svc.Annotations[poolAnnotation] = pool

	if c.baseURL != nil {
		// Connect to the EGW
		egw, err := acnodal.New(c.baseURL.String(), "")
		if err != nil {
			l.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
			c.client.Errorf(svc, "AllocationFailed", "Failed to create EGW service for %s: %s", svc.Name, err)
			return false
		}

		// Look up the EGW group (which gives us the URL to create services)
		group, err := egw.GetGroup(*c.groupURL)
		if err != nil {
			l.Log("op", "GetGroup", "group", c.groupURL, "error", err)
			c.client.Errorf(svc, "GetGroupFailed", "Failed to get group %s: %s", c.groupURL, err)
			return false
		}

		// Announce the service to the EGW
		egwsvc, err := egw.AnnounceService(group.Links["create-service"], svc.Name, svc.Status.LoadBalancer.Ingress[0].IP)
		if err != nil {
			l.Log("op", "AnnouncementFailed", "service", svc.Name, "error", err)
			c.client.Errorf(svc, "AnnouncementFailed", "Failed to announce service for %s: %s", svc.Name, err)
			return false
		}
		svc.Annotations["acnodal.io/groupURL"] = egwsvc.Links["group"]
		svc.Annotations["acnodal.io/serviceURL"] = egwsvc.Links["self"]
		svc.Annotations["acnodal.io/endpointcreateURL"] = egwsvc.Links["create-endpoint"]
	}

	return true
}

func (c *controller) allocateIP(key string, svc *v1.Service, isIPv6 bool) (string, net.IP, error) {
	// If the user asked for a specific IP, try that.
	if svc.Spec.LoadBalancerIP != "" {
		ip := net.ParseIP(svc.Spec.LoadBalancerIP)
		if ip == nil {
			return "", nil, fmt.Errorf("invalid spec.loadBalancerIP %q", svc.Spec.LoadBalancerIP)
		}
		if (ip.To4() == nil) != isIPv6 {
			return "", nil, fmt.Errorf("requested spec.loadBalancerIP %q does not match the ipFamily of the service", svc.Spec.LoadBalancerIP)
		}
		pool, err := c.ips.Assign(key, ip, Ports(svc), SharingKey(svc), BackendKey(svc))
		if err != nil {
			return "", nil, err
		}
		return pool, ip, nil
	}

	// Otherwise, did the user ask for a specific pool?
	desiredPool := svc.Annotations[desiredPoolAnnotation]
	if desiredPool != "" {
		ip, err := c.ips.AllocateFromPool(key, isIPv6, desiredPool, Ports(svc), SharingKey(svc), BackendKey(svc))
		if err != nil {
			return "", nil, err
		}
		return desiredPool, ip, nil
	}

	// Okay, in that case just bruteforce across all pools.
	return c.ips.Allocate(key, isIPv6, Ports(svc), SharingKey(svc), BackendKey(svc))
}
