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

	"github.com/go-kit/kit/log"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"
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
	for n := range a.pools {
		a.updateStats(n)
	}

	return nil
}

// updateStats unconditionally updates internal state to reflect svc's
// allocation of alloc. Caller must ensure that this call is safe.
func (a *Allocator) updateStats(poolName string) error {
	pool := a.pools[poolName]
	poolCapacity.WithLabelValues(poolName).Set(float64(pool.Size()))
	poolActive.WithLabelValues(poolName).Set(float64(pool.InUse()))

	return nil
}

// NotifyExisting notifies the allocator of an existing IP assignment,
// for example, at startup time.
func (a *Allocator) NotifyExisting(svc *v1.Service) error {
	// Get the pool name from our annotation
	poolName, exists := svc.Annotations[purelbv1.PoolAnnotation]
	if !exists {
		return fmt.Errorf("Service %s no pool", namespacedName(svc))
	}

	// Tell the pool about the assignment
	if pool, havePool := a.pools[poolName]; !havePool {
		return nil
	} else {
		if err := pool.Notify(svc); err != nil {
			return err
		}
		return a.updateStats(poolName)
	}
}

// AllocateAnyIP allocates an IP address for svc based on svc's
// annotations and current configuration. If the user asks for a
// specific IP then we'll attempt to use that, and if not we'll use
// the pool specified in the purelbv1.DesiredGroupAnnotation
// annotation. If neither is specified then we will attempt to
// allocate from a pool named "default", if it exists.
func (a *Allocator) AllocateAnyIP(svc *v1.Service) (string, error) {
	var (
		poolName string
		err      error
	)

	// If the user asked for a specific IP, allocate that.
	poolName, err = a.allocateSpecificIP(svc)
	if err != nil {
		return "", err
	}
	if poolName == "" {
		// The user didn't ask for a specific IP so we can allocate one
		// ourselves

		// If no desiredGroup was specified, then try "default"
		if poolName = svc.Annotations[purelbv1.DesiredGroupAnnotation]; poolName == "" {
			poolName = defaultPoolName
		}

		// Otherwise, allocate from the pool that the user specified
		if err = a.allocateFromPool(svc, poolName); err != nil {
			return "", err
		}
	}

	if err = a.updateStats(poolName); err != nil {
		return "", err
	}

	return poolName, nil
}

// allocateSpecificIP assigns the requested ip to svc, if the
// assignment is permissible by sharingKey. If the user didn't ask for
// a specific address then the return values will be ("", nil). If an
// address was allocated then the string return value will be
// non-"". If an error happened then the error return will be non-nil.
func (a *Allocator) allocateSpecificIP(svc *v1.Service) (string, error) {
	// See if the user configured a specific address and return if not.
	ip, err := a.serviceIP(svc)
	if err != nil {
		return "", err
	}
	if ip == nil { // no user-configured address
		return "", err
	}

	// Check that the address belongs to a pool
	pool := poolFor(a.pools, ip)
	if pool == "" {
		return "", fmt.Errorf("%q does not belong to any group", ip)
	}

	// Check that the address belongs to the requested pool
	desiredGroup, exists := svc.Annotations[purelbv1.DesiredGroupAnnotation]
	if exists && desiredGroup != pool {
		return "", fmt.Errorf("%q belongs to group %s but desired group is %s", ip, pool, desiredGroup)
	}

	// If the service had an IP before, release it
	if err := a.Unassign(namespacedName(svc)); err != nil {
		return "", err
	}

	// Does the IP already have allocs? If so, needs to be the same
	// sharing key, and have non-overlapping ports. If not, the proposed
	// IP needs to be allowed by configuration.
	if err := a.pools[pool].Assign(ip, svc); err != nil {
		return "", err
	}

	return pool, nil
}

// AllocateFromPool assigns an available IP from pool to service.
func (a *Allocator) allocateFromPool(svc *v1.Service, poolName string) error {
	pool := a.pools[poolName]
	if pool == nil {
		return fmt.Errorf("unknown pool %q", poolName)
	}

	// If the service had an IP before, release it
	if err := a.Unassign(namespacedName(svc)); err != nil {
		return err
	}

	if err := pool.AssignNext(svc); err != nil {
		// Woops, no IPs :( Fail.
		return err
	}

	return nil
}

// Unassign frees the IP associated with service, if any.
func (a *Allocator) Unassign(svc string) error {
	var err error

	// tell the pools that the address has been released. there might
	// not be a pool, e.g., in the case of a config change that moves
	// addresses from one pool to another
	for pname, p := range a.pools {
		if err = p.Release(svc); err == nil {
			a.updateStats(pname)  // This pool released the address
		}
	}

	return nil
}

// poolFor returns the pool that owns the requested IP, or "" if none.
func poolFor(pools map[string]Pool, ip net.IP) string {
	for pname, p := range pools {
		if p.Contains(ip) {
			return pname
		}
	}
	return ""
}

// serviceIP returns any IP addresses configured in the provided
// service. There can be 0-2 addresses: the deprecated
// svc.Spec.LoadBalancer field can contain one, and the
// purelbv1.DesiredAddressAnnotation can contain one or two, separated
// by commas.
func (a *Allocator) serviceIP(svc *v1.Service) (net.IP, error) {

	// Try our annotation first.
	rawAddr, exists := svc.Annotations[purelbv1.DesiredAddressAnnotation]
	if !exists {
		// There's no DesiredAddressAnnotation so try the (deprecated)
		// LoadBalancerIP field.
		rawAddr = svc.Spec.LoadBalancerIP
		if rawAddr == "" {
			return nil, nil
		}

		// Warn the user about the deprecated LoadBalancerIP field
		a.client.Infof(svc, "DeprecationWarning", "Service.Spec.LoadBalancerIP is deprecated, please use the \"%s\" annotation instead", purelbv1.DesiredAddressAnnotation)
		a.logger.Log("svc-name", svc.Name, "deprecation", "Service.Spec.LoadBalancerIP is deprecated, please use the \"" + purelbv1.DesiredAddressAnnotation + "\" annotation instead")
	}

	ip := net.ParseIP(rawAddr)
	if ip == nil {
		return nil, fmt.Errorf("invalid user-specified address: \"%q\"", rawAddr)
	}

	return ip, nil
}

// parseGroups parses a slice of ServiceGroups and returns a map of
// the pools specified by those groups. We try to return any good
// pools so if a pool fails our validation it won't be in the output,
// but other valid pools will be. Therefore there might be fewer pools
// in the output than there are groups in the input.
func (a *Allocator) parseGroups(groups []*purelbv1.ServiceGroup) map[string]Pool {
	pools := map[string]Pool{}

Group:
	for _, group := range groups {
		pool, err := parsePool(a.logger, group.Name, group.Spec)
		if err != nil {
			a.client.Errorf(group, "ParseFailed", "Failed to parse: %s", err)
			a.logger.Log("failure", "parsing ServiceGroup address pool", "service-group", group.Name, "message", err)
			continue Group
		}

		// Check that the pool isn't already defined
		if pools[group.Name] != nil {
			a.client.Errorf(group, "ParseFailed", "Duplicate definition of pool %s", group.Name)
			a.logger.Log("failure", "duplicate definition of ServiceGroup address pool", "service-group", group.Name)
			continue Group
		}

		// Check that this pool doesn't overlap with any of the previous
		// ones
		for name, r := range pools {
			if pool.Overlaps(r) {
				a.client.Errorf(group, "ParseFailed", "Pool overlaps with already defined pool \"%s\"", name)
				a.logger.Log("failure", "ServiceGroup address pool overlaps with already defined pool", "service-group", group.Name, "overlaps-with", name)
				continue Group
			}
		}

		pools[group.Name] = pool
		a.client.Infof(group, "Parsed", "ServiceGroup parsed successfully")
	}

	return pools
}
