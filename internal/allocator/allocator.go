// Copyright 2017 Google Inc.
// Copyright 2020,2021 Acnodal Inc.
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
	"strings"

	"github.com/go-kit/log"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/k8s"
	"purelb.io/internal/logging"
	purelbv1 "purelb.io/pkg/apis/purelb/v1"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

const (
	defaultPoolName string = "default"
)

// An Allocator tracks IP address pools and allocates addresses from them.
type Allocator struct {
	client k8s.ServiceEvent
	logger log.Logger
	pools  map[string]Pool
}

// New returns an Allocator managing no pools.
func New(log log.Logger) *Allocator {
	return &Allocator{
		logger: log,
		pools:  map[string]Pool{},
	}
}

// SetClient sets this Allocator's client field.
func (a *Allocator) SetClient(client k8s.ServiceEvent) {
	a.client = client
}

// SetPools updates the set of address pools that the allocator owns.
func (a *Allocator) SetPools(groups []*purelbv1.ServiceGroup) error {
	pools := a.parseGroups(groups)

	// If we have groups but they're all bogus then let the user know.
	if len(groups) > 0 && len(pools) == 0 {
		return fmt.Errorf("No valid pools found")
	}

	for n := range a.pools {
		if pools[n] == nil {
			poolCapacity.DeleteLabelValues(n)
			poolActive.DeleteLabelValues(n)
		}
	}

	a.pools = pools

	// Refresh or initiate stats
	for _, p := range a.pools {
		a.updateStats(p)
	}

	return nil
}

// updateStats unconditionally updates internal state to reflect svc's
// allocation of alloc. Caller must ensure that this call is safe.
func (a *Allocator) updateStats(pool Pool) {
	poolCapacity.WithLabelValues(pool.String()).Set(float64(pool.Size()))
	poolActive.WithLabelValues(pool.String()).Set(float64(pool.InUse()))
}

// NotifyExisting notifies the allocator of existing IP assignments,
// for example, at startup time.
func (a *Allocator) NotifyExisting(svc *v1.Service) error {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		// validate the allocated address
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			logging.Info(a.logger, "op", "setBalancer", "error", "invalid LoadBalancer IP", "ip", ingress.IP)
			continue
		}

		// Find the pool which contains the address
		pool := poolFor(a.pools, lbIP)
		if pool == nil {
			logging.Info(a.logger, "op", "setBalancer", "error", "unknown LoadBalancer IP: no pool found", "ip", ingress.IP)
			continue
		}

		// Tell the pool about the assignment
		if err := pool.Notify(svc); err != nil {
			return err
		}
		a.updateStats(pool)
	}
	return nil
}

// Allocate allocates an IP address for svc based on svc's
// annotations and current configuration. If the user asks for a
// specific IP then we'll attempt to use that, and if not we'll use
// the pool specified in the purelbv1.DesiredGroupAnnotation
// annotation. If neither is specified then we will attempt to
// allocate from a pool named "default", if it exists.
func (a *Allocator) Allocate(svc *v1.Service) error {
	// If the user asked for a specific IP, allocate that.
	allocated, err := a.allocateSpecificIP(svc)
	if err != nil {
		return err
	}

	// The user didn't ask for a specific IP so we can allocate one from
	// a pool.
	if !allocated {
		// Start with the default pool name.
		poolName := defaultPoolName

		// If the user specified a desiredGroup, then use that.
		if userPool, has := svc.Annotations[purelbv1.DesiredGroupAnnotation]; has {
			poolName = userPool
		}

		pool, has := a.pools[poolName]
		if !has {
			return fmt.Errorf("unknown pool %q", poolName)
		}

		// Try to allocate from the pool.
		if err = a.allocateFromPool(svc, pool); err != nil {
			return err
		}
	}

	return nil
}

// allocateSpecificIP assigns the requested ip to svc, if the
// assignment is permissible by sharingKey. If the user didn't ask for
// a specific address then the return values will be ("", nil). If an
// address was allocated then the string return value will be
// non-"". If an error happened then the error return will be non-nil.
func (a *Allocator) allocateSpecificIP(svc *v1.Service) (bool, error) {
	pools := ""
	var firstPool Pool // Track first pool for annotations

	// See if the user configured a specific address and return if not.
	ips, err := a.serviceAddresses(svc)
	if err != nil {
		return false, err
	}
	if len(ips) == 0 { // no user-configured address
		return false, nil
	}

	// Warn if the user provided the group annotation - the IP
	// annotation overrides it.
	if _, exists := svc.Annotations[purelbv1.DesiredGroupAnnotation]; exists {
		a.client.Infof(svc, "ConfigurationWarning", "Both the addresses annotation and the service-group annotation were provided. service-group will be ignored.")
		logging.Info(a.logger, "op", "allocateSpecificIP", "warning", "addresses annotation overrides service-group annotation")
	}

	// If the service had addresses before, release them.
	if err := a.Unassign(namespacedName(svc)); err != nil {
		return false, err
	}

	for _, ip := range ips {

		// Check that the address belongs to a pool
		pool := poolFor(a.pools, ip)
		if pool == nil {
			return false, fmt.Errorf("%q does not belong to any group", ip)
		}

		// Track the first pool for setting annotations
		if firstPool == nil {
			firstPool = pool
		}

		// Does the IP already have allocs? If so, needs to be the same
		// sharing key, and have non-overlapping ports. If not, the proposed
		// IP needs to be allowed by configuration.
		if err := pool.Assign(ip, svc); err != nil {
			return false, err
		}

		a.updateStats(pool)

		// annotate the pool from which the address came
		a.client.Infof(svc, "AddressAssigned", "Assigned %+v from pool %s", svc.Status.LoadBalancer, pool)
		if pools == "" {
			pools = pool.String()
		} else {
			pools = pools + ", " + pool.String()
		}
	}

	svc.Annotations[purelbv1.PoolAnnotation] = pools

	// Set pool type annotation based on the first pool
	// (in practice, specific IPs should come from pools of the same type)
	if firstPool != nil {
		svc.Annotations[purelbv2.PoolTypeAnnotation] = firstPool.PoolType()

		// Set skip-ipv6-dad annotation if the pool has it enabled
		if firstPool.SkipIPv6DAD() {
			svc.Annotations[purelbv2.SkipIPv6DADAnnotation] = "true"
		} else {
			delete(svc.Annotations, purelbv2.SkipIPv6DADAnnotation)
		}
	}

	return true, nil
}

// AllocateFromPool assigns an available IP from pool to service.
func (a *Allocator) allocateFromPool(svc *v1.Service, pool Pool) error {
	// Only release existing IPs if service has no ingress addresses.
	// If it already has addresses, this might be an IP family transition
	// (e.g., SingleStack → DualStack) where we want to keep existing IPs
	// and only allocate missing families.
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		if err := a.Unassign(namespacedName(svc)); err != nil {
			return err
		}
	}

	if err := pool.AssignNext(svc); err != nil {
		// Woops, no IPs :( Fail.
		return err
	}

	// annotate the pool from which the address came
	a.client.Infof(svc, "AddressAssigned", "Assigned %+v from pool %s", svc.Status.LoadBalancer, pool)
	svc.Annotations[purelbv1.PoolAnnotation] = pool.String()

	// Set pool type annotation so lbnodeagent knows which interface to use
	svc.Annotations[purelbv2.PoolTypeAnnotation] = pool.PoolType()

	// Set skip-ipv6-dad annotation if the pool has it enabled
	if pool.SkipIPv6DAD() {
		svc.Annotations[purelbv2.SkipIPv6DADAnnotation] = "true"
	} else {
		// Remove the annotation if it was previously set
		delete(svc.Annotations, purelbv2.SkipIPv6DADAnnotation)
	}

	a.updateStats(pool)

	return nil
}

// Unassign frees the IP associated with service, if any.
func (a *Allocator) Unassign(svc string) error {
	var err error

	// tell the pools that the address has been released. there might
	// not be a pool, e.g., in the case of a config change that moves
	// addresses from one pool to another
	for _, p := range a.pools {
		if err = p.Release(svc); err == nil {
			a.updateStats(p)  // This pool released the address
		}
	}

	return nil
}

// ReleaseIPs releases specific IP addresses for a service. Used during
// IP family transitions (e.g., DualStack → SingleStack) when only some
// addresses need to be released.
func (a *Allocator) ReleaseIPs(svc string, ips []string) error {
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			logging.Info(a.logger, "op", "releaseIPs", "error", "invalid IP", "ip", ipStr)
			continue
		}

		// Find which pool contains this IP and release it
		pool := poolFor(a.pools, ip)
		if pool == nil {
			logging.Info(a.logger, "op", "releaseIPs", "warning", "no pool found for IP", "ip", ipStr)
			continue
		}

		if err := pool.ReleaseIP(svc, ip); err != nil {
			logging.Info(a.logger, "op", "releaseIPs", "error", err, "ip", ipStr)
			// Continue releasing other IPs even if one fails
		}
		a.updateStats(pool)
	}

	return nil
}

// poolFor returns the pool that owns the requested IP, or "" if none.
func poolFor(pools map[string]Pool, ip net.IP) Pool {
	for _, p := range pools {
		if p.Contains(ip) {
			return p
		}
	}
	return nil
}

// serviceAddresses returns any IP addresses configured in the provided
// service. There can be 0-2 addresses: the deprecated
// svc.Spec.LoadBalancer field can contain one, and the
// purelbv1.DesiredAddressAnnotation can contain one or two, separated
// by commas.
func (a *Allocator) serviceAddresses(svc *v1.Service) ([]net.IP, error) {
	ips := []net.IP{}

	// Try our annotation first.
	rawAddrs, exists := svc.Annotations[purelbv1.DesiredAddressAnnotation]
	if !exists {
		// There's no DesiredAddressAnnotation so try the (deprecated)
		// LoadBalancerIP field.
		rawAddrs = svc.Spec.LoadBalancerIP
		if rawAddrs == "" {
			return nil, nil
		}

		// Warn the user about the deprecated LoadBalancerIP field
		a.client.Infof(svc, "DeprecationWarning", "Service.Spec.LoadBalancerIP is deprecated, please use the \"%s\" annotation instead", purelbv1.DesiredAddressAnnotation)
		logging.Info(a.logger, "op", "serviceAddresses", "svc", svc.Name, "deprecation", "Service.Spec.LoadBalancerIP is deprecated, use "+purelbv1.DesiredAddressAnnotation+" annotation")
	}

	for _, rawAddr := range(strings.Split(rawAddrs, ",")) {
		ip := net.ParseIP(rawAddr)
		if ip == nil {
			return nil, fmt.Errorf("invalid user-specified address: \"%q\"", rawAddr)
		}
		ips = append(ips, ip)
	}

	return ips, nil
}

// parseGroups parses a slice of ServiceGroups and returns a map of
// the pools specified by those groups. We try to return any good
// pools so if a pool fails our validation it won't be in the output,
// but other valid pools will be. Therefore there might be fewer pools
// in the output than there are groups in the input.
func (a *Allocator) parseGroups(groups []*purelbv1.ServiceGroup) map[string]Pool {
	pools := map[string]Pool{}

	// Log deprecation warning once per config update if using v1 ServiceGroups
	if len(groups) > 0 {
		logging.Info(a.logger, "op", "parseGroups", "deprecation",
			"ServiceGroup v1 API is deprecated, please migrate to v2")
	}

Group:
	for _, group := range groups {
		pool, err := parsePool(a.logger, group.Name, group.Spec)
		if err != nil {
			a.client.Errorf(group, "ParseFailed", "Failed to parse: %s", err)
			logging.Info(a.logger, "op", "parseGroups", "error", "parsing ServiceGroup", "group", group.Name, "msg", err)
			continue Group
		}

		// Check that the pool isn't already defined
		if pools[group.Name] != nil {
			a.client.Errorf(group, "ParseFailed", "Duplicate definition of pool %s", group.Name)
			logging.Info(a.logger, "op", "parseGroups", "error", "duplicate ServiceGroup", "group", group.Name)
			continue Group
		}

		// Check that this pool doesn't overlap with any of the previous
		// ones
		for name, r := range pools {
			if pool.Overlaps(r) {
				a.client.Errorf(group, "ParseFailed", "Pool overlaps with already defined pool \"%s\"", name)
				logging.Info(a.logger, "op", "parseGroups", "error", "ServiceGroup overlaps", "group", group.Name, "overlaps", name)
				continue Group
			}
		}

		pools[group.Name] = pool
		a.client.Infof(group, "Parsed", "ServiceGroup parsed successfully")
	}

	return pools
}
