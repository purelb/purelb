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
	pools, err := a.parseConfig(groups)
	if err != nil {
		return err
	}

	for n := range a.pools {
		if pools[n] == nil {
			poolCapacity.DeleteLabelValues(n)
			poolActive.DeleteLabelValues(n)
		}
	}

	a.pools = pools

	// Refresh or initiate stats
	for n, p := range a.pools {
		poolCapacity.WithLabelValues(n).Set(float64(p.Size()))
		poolActive.WithLabelValues(n).Set(float64(p.InUse()))
	}

	return nil
}

// updateStats unconditionally updates internal state to reflect svc's
// allocation of alloc. Caller must ensure that this call is safe.
func (a *Allocator) updateStats(service *v1.Service, poolName string, ip net.IP) error {
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
		existingIP := parseIngress(a.logger, svc.Status.LoadBalancer.Ingress[0])
		a.logger.Log("allocator", "notify-existing", "pool", poolName, "name", namespacedName(svc), "ip", existingIP)
		err := pool.Assign(existingIP, svc)
		if err != nil {
			return err
		}
		return a.updateStats(svc, poolName, existingIP)
	}
}

// AllocateAnyIP allocates an IP address for svc based on svc's
// annotations and current configuration. If the user asks for a
// specific IP then we'll attempt to use that, and if not we'll use
// the pool specified in the purelbv1.DesiredGroupAnnotation
// annotation. If neither is specified then we will attempt to
// allocate from a pool named "default", if it exists.
func (a *Allocator) AllocateAnyIP(svc *v1.Service) (string, net.IP, error) {
	var (
		poolName string
		ip       net.IP
		err      error
	)

	if svc.Spec.LoadBalancerIP != "" {
		// The user asked for a specific IP, so try that.
		if ip = net.ParseIP(svc.Spec.LoadBalancerIP); ip == nil {
			return "", nil, fmt.Errorf("invalid spec.loadBalancerIP %q", svc.Spec.LoadBalancerIP)
		}

		if poolName, err = a.allocateSpecificIP(svc, ip); err != nil {
			return "", nil, err
		}
	} else {
		// The user didn't ask for a specific IP so we can allocate one
		// ourselves

		// If no desiredGroup was specified, then we will try "default"
		if poolName = svc.Annotations[purelbv1.DesiredGroupAnnotation]; poolName == "" {
			poolName = defaultPoolName
		}

		// Otherwise, allocate from the pool that the user specified
		if ip, err = a.allocateFromPool(svc, poolName); err != nil {
			return "", nil, err
		}
	}

	if err := a.updateStats(svc, poolName, ip); err != nil {
		return "", nil, err
	}

	// we have an IP selected somehow, so program the data plane
	addIngress(a.logger, svc, ip)
	a.logger.Log("event", "ipAllocated", "ip", ip, "pool", poolName)

	return poolName, ip, nil
}

// allocateSpecificIP assigns the requested ip to svc, if the assignment is
// permissible by sharingKey.
func (a *Allocator) allocateSpecificIP(svc *v1.Service, ip net.IP) (string, error) {
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
	err := a.pools[pool].Assign(ip, svc)
	if err != nil {
		return "", err
	}

	return pool, nil
}

// AllocateFromPool assigns an available IP from pool to service.
func (a *Allocator) allocateFromPool(svc *v1.Service, poolName string) (net.IP, error) {
	var ip net.IP

	pool := a.pools[poolName]
	if pool == nil {
		return nil, fmt.Errorf("unknown pool %q", poolName)
	}

	// If the service had an IP before, release it
	if err := a.Unassign(namespacedName(svc)); err != nil {
		return nil, err
	}

	ip, err := pool.AssignNext(svc)
	if err != nil {
		// Woops, no IPs :( Fail.
		return nil, err
	}

	return ip, nil
}

// Unassign frees the IP associated with service, if any.
func (a *Allocator) Unassign(svc string) error {
	var err error

	// tell the pools that the address has been released. there might
	// not be a pool, e.g., in the case of a config change that moves
	// addresses from one pool to another
	for pname, p := range a.pools {
		if err = p.Release(svc); err == nil {
			// This pool released the address
			poolActive.WithLabelValues(pname).Set(float64(p.InUse()))
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

func (a *Allocator) parseConfig(groups []*purelbv1.ServiceGroup) (map[string]Pool, error) {
	pools := map[string]Pool{}

	for i, group := range groups {
		pool, err := a.parseGroup(group.Name, group.Spec)
		if err != nil {
			a.client.Errorf(group, "ParseFailed", "Failed to parse: %s", err)
			return nil, fmt.Errorf("parsing address pool #%d: %s", i+1, err)
		}

		// Check that the pool isn't already defined
		if pools[group.Name] != nil {
			a.client.Errorf(group, "ParseFailed", "Duplicate definition of pool %s", group.Name)
			return nil, fmt.Errorf("duplicate definition of pool %q", group.Name)
		}

		// Check that this pool doesn't overlap with any of the previous
		// ones
		for name, r := range pools {
			if pool.Overlaps(r) {
				a.client.Errorf(group, "ParseFailed", "Pool overlaps with already defined pool \"%s\"", name)
				return nil, fmt.Errorf("pool %q overlaps with already defined pool %q", group.Name, name)
			}
		}

		pools[group.Name] = pool
		a.client.Infof(group, "Parsed", "ServiceGroup parsed successfully", group.Name)
	}

	return pools, nil
}

func (a *Allocator) parseGroup(name string, group purelbv1.ServiceGroupSpec) (Pool, error) {
	if group.Local != nil {
		ret, err := NewLocalPool(*group.Local)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	} else if group.Netbox != nil {
		ret, err := NewNetboxPool(*group.Netbox)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	}

	return nil, fmt.Errorf("Pool is not local or Netbox")
}
