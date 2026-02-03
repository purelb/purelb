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

	"github.com/go-kit/log"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv1 "purelb.io/pkg/apis/purelb/v1"
)

// ipFamilyFromIP returns the IP family (IPv4 or IPv6) for a given IP address.
func ipFamilyFromIP(ip net.IP) v1.IPFamily {
	if ip.To4() != nil {
		return v1.IPv4Protocol
	}
	return v1.IPv6Protocol
}

// analyzeIPFamilyTransition compares the requested IP families with the
// current ingress addresses to determine what needs to be allocated or released.
// Returns:
//   - missingFamilies: families that need new addresses allocated
//   - excessIPs: ingress IPs that should be released (family no longer requested)
//   - keepIPs: ingress IPs that should be kept
func analyzeIPFamilyTransition(svc *v1.Service) (missingFamilies []v1.IPFamily, excessIPs []string, keepIPs []string) {
	// Build set of requested families
	requestedFamilies := make(map[v1.IPFamily]bool)
	for _, family := range svc.Spec.IPFamilies {
		requestedFamilies[family] = true
	}
	// Default to IPv4 if no families specified
	if len(requestedFamilies) == 0 {
		requestedFamilies[v1.IPv4Protocol] = true
	}

	// Track which families we already have addresses for
	haveFamilies := make(map[v1.IPFamily]bool)

	// Categorize current ingress IPs
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ip := net.ParseIP(ingress.IP)
		if ip == nil {
			continue
		}
		family := ipFamilyFromIP(ip)

		if requestedFamilies[family] {
			// This IP's family is still requested - keep it
			keepIPs = append(keepIPs, ingress.IP)
			haveFamilies[family] = true
		} else {
			// This IP's family is no longer requested - mark for release
			excessIPs = append(excessIPs, ingress.IP)
		}
	}

	// Determine which requested families don't have addresses yet
	for family := range requestedFamilies {
		if !haveFamilies[family] {
			missingFamilies = append(missingFamilies, family)
		}
	}

	return missingFamilies, excessIPs, keepIPs
}

// SetBalancer is the main entry point that handles LoadBalancer
// create/change events. It takes a Service and decides what to do
// based on that Service's configuration. It returns a k8s.SyncState
// value - SyncStateSuccess or SyncStateError.
// Note: The allocator ignores EndpointSlices - they are only used by lbnodeagent.
func (c *controller) SetBalancer(svc *v1.Service, _ []*discoveryv1.EndpointSlice) k8s.SyncState {
	nsName := svc.Namespace + "/" + svc.Name
	l := log.With(c.logger, "svc", nsName)

	if !c.synced {
		logging.Info(l, "op", "allocateIP", "error", "controller not synced")
		return k8s.SyncStateError
	}

	// If the user has specified an LB class and it's not ours then we
	// ignore the LB.
	if svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass != purelbv1.ServiceLBClass {
		logging.Debug(l, "op", "setBalancer", "msg", "ignoring, user specified another class", "class", *svc.Spec.LoadBalancerClass)
		return k8s.SyncStateSuccess
	}

	// If we are not configured to be the default announcer then we
	// ignore services with no explicit LoadBalancerClass.
	if !c.isDefault && svc.Spec.LoadBalancerClass == nil {
		logging.Debug(l, "op", "setBalancer", "msg", "ignoring, no LBClass and not default announcer")
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
				logging.Info(l, "op", "unassign", "reason", "type is not LoadBalancer", "ingress", svc.Status.LoadBalancer.Ingress)
				c.client.Infof(svc, "AddressReleased", fmt.Sprintf("Service is Type %s, not LoadBalancer", svc.Spec.Type))
				if err := c.ips.Unassign(nsName); err != nil {
					logging.Info(l, "op", "unassign", "error", err)
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
		logging.Debug(l, "op", "setBalancer", "msg", "no ClusterIP, skipping")
		return k8s.SyncStateSuccess
	}

	// Analyze whether the service needs IP family transitions
	// (e.g., SingleStack → DualStack or DualStack → SingleStack)
	missingFamilies, excessIPs, keepIPs := analyzeIPFamilyTransition(svc)

	// Check if the service already has addresses
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		logging.Debug(l, "op", "setBalancer", "msg", "hasIngress", "ingress", svc.Status.LoadBalancer.Ingress,
			"missingFamilies", missingFamilies, "excessIPs", excessIPs)

		// If it's one of ours, notify the allocator about existing IPs
		// (for database warmup at startup or after config changes)
		if svc.Annotations[purelbv1.BrandAnnotation] == purelbv1.Brand {
			if err := c.ips.NotifyExisting(svc); err != nil {
				logging.Info(l, "op", "notifyExisting", "error", err, "ingress", svc.Status.LoadBalancer.Ingress)
			}
		}

		// Handle DualStack → SingleStack transition: release excess IPs
		if len(excessIPs) > 0 {
			logging.Info(l, "op", "ipFamilyTransition", "action", "releaseExcess", "excessIPs", excessIPs)

			// Release the excess IPs from the pool
			if err := c.ips.ReleaseIPs(nsName, excessIPs); err != nil {
				logging.Info(l, "op", "releaseExcess", "error", err)
				// Continue anyway - we'll update the ingress to remove them
			}

			// Update ingress to only keep the IPs that match requested families
			ipModeVIP := v1.LoadBalancerIPModeVIP
			svc.Status.LoadBalancer.Ingress = nil
			for _, ipStr := range keepIPs {
				svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress,
					v1.LoadBalancerIngress{IP: ipStr, IPMode: &ipModeVIP})
			}
			c.client.Infof(svc, "AddressReleased", "Released addresses no longer needed after IP family transition: %v", excessIPs)
		}

		// If no families are missing, we're done
		if len(missingFamilies) == 0 {
			return k8s.SyncStateSuccess
		}

		// Handle SingleStack → DualStack transition: allocate missing families
		logging.Info(l, "op", "ipFamilyTransition", "action", "allocateMissing", "missingFamilies", missingFamilies)
	}

	// Annotate the service as "ours"
	svc.Annotations[purelbv1.BrandAnnotation] = purelbv1.Brand

	if err := c.ips.Allocate(svc); err != nil {
		logging.Info(l, "op", "allocateIP", "error", err, "msg", "IP allocation failed")
		c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", nsName, err)
		return k8s.SyncStateSuccess
	}

	return k8s.SyncStateSuccess
}
