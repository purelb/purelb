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

	"purelb.io/internal/acnodal"
	purelbv1 "purelb.io/pkg/apis/v1"
)

// An Allocator tracks IP address pools and allocates addresses from them.
type Allocator struct {
	logger    log.Logger
	pools     map[string]Pool
	allocated map[string]*alloc // svc -> alloc
}

type alloc struct {
	pool string
	ip   net.IP
	Key
}

// New returns an Allocator managing no pools.
func New(log log.Logger) *Allocator {
	return &Allocator{
		logger:    log,
		pools:     map[string]Pool{},
		allocated: map[string]*alloc{},
	}
}

// SetPools updates the set of address pools that the allocator owns.
func (a *Allocator) SetPools(myCluster string, groups []*purelbv1.ServiceGroup) error {
	pools, err := a.parseConfig(myCluster, groups)
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

// assign unconditionally updates internal state to reflect svc's
// allocation of alloc. Caller must ensure that this call is safe.
func (a *Allocator) assign(service *v1.Service, poolName string, ip net.IP) error {
	svc := namespacedName(service)

	alloc := &alloc{
		pool: poolName,
		ip:   ip,
		Key: Key{
			Sharing: SharingKey(service),
		},
	}
	a.allocated[svc] = alloc

	pool := a.pools[alloc.pool]
	pool.Assign(alloc.ip, service)

	poolCapacity.WithLabelValues(alloc.pool).Set(float64(a.pools[alloc.pool].Size()))
	poolActive.WithLabelValues(alloc.pool).Set(float64(pool.InUse()))

	return nil
}

// NotifyExisting notifies the allocator of an existing IP assignment,
// for example, at startup time.
func (a *Allocator) NotifyExisting(svc *v1.Service, ip net.IP) error {
	nsName := namespacedName(svc)

	// Get the pool name from our annotation
	poolName, exists := svc.Annotations[purelbv1.PoolAnnotation]
	if !exists {
		return fmt.Errorf("Service %s no pool", nsName)
	}

	// Tell the pool about the assignment
	if _, havePool := a.pools[poolName]; !havePool {
		return nil
	} else {
		fmt.Println("notify", poolName, nsName, ip.String())
		return a.assign(svc, poolName, ip)
	}
}

func (a *Allocator) AllocateAnyIP(svc *v1.Service) (string, net.IP, error) {
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
		pool, err := a.AllocateSpecificIP(svc, ip)
		if err != nil {
			return "", nil, err
		}
		return pool, ip, nil
	}

	// Otherwise, did the user ask for a specific pool?
	if desiredGroup != "" {
		ip, err := a.AllocateFromPool(svc, desiredGroup)
		if err != nil {
			return "", nil, err
		}
		return desiredGroup, ip, nil
	}

	// Okay, in that case just bruteforce across all pools.
	return a.Allocate(svc)
}

// AllocateSpecificIP assigns the requested ip to svc, if the assignment is
// permissible by sharingKey.
func (a *Allocator) AllocateSpecificIP(svc *v1.Service, ip net.IP) (string, error) {
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

	// Does the IP already have allocs? If so, needs to be the same
	// sharing key, and have non-overlapping ports. If not, the proposed
	// IP needs to be allowed by configuration.
	err := a.pools[pool].Available(ip, svc) // FIXME: this should Assign() here, not check Available.  Might need to iterate over pools rather than do poolFor
	if err != nil {
		return "", err
	}

	// If the service had an IP before, release it
	if err := a.Unassign(namespacedName(svc)); err != nil {
		return "", err
	}

	// Either the IP is entirely unused, or the requested use is
	// compatible with existing uses. Assign!
	if err := a.assign(svc, pool, ip); err != nil {
		return "", err
	}
	return pool, nil
}

// AllocateFromPool assigns an available IP from pool to service.
func (a *Allocator) AllocateFromPool(svc *v1.Service, poolName string) (net.IP, error) {
	var ip net.IP

	pool := a.pools[poolName]
	if pool == nil {
		return nil, fmt.Errorf("unknown pool %q", poolName)
	}

	ip, err := pool.AssignNext(svc)
	if err != nil {
		// Woops, no IPs :( Fail.
		return nil, err
	}

	// If the service had an IP before, release it
	if err := a.Unassign(namespacedName(svc)); err != nil {
		return nil, err
	}

	if err := a.assign(svc, poolName, ip); err != nil {
		return nil, err
	}

	return ip, nil
}

// Allocate any available and assignable IP to service.
func (a *Allocator) Allocate(svc *v1.Service) (string, net.IP, error) {
	var (
		err error
		ip  net.IP
	)

	// if we have already allocated an address for this service then
	// return it
	if alloc := a.allocated[svc.Name]; alloc != nil {
		return alloc.pool, alloc.ip, nil
	}

	// we need an address but no pool was specified so it's either the
	// "default" pool or nothing
	if ip, err = a.AllocateFromPool(svc, "default"); err == nil {
		return "default", ip, nil
	}
	return "", nil, err
}

// Unassign frees the IP associated with service, if any.
func (a *Allocator) Unassign(svc string) error {
	al := a.allocated[svc]
	if al == nil {
		// not much we can do here, but if we don't know about svc then it
		// doesn't have an address so we don't need to do anything
		fmt.Println("don't know about service", svc)
		return nil
	}

	// tell the pool that the address has been released. there might not
	// be a pool, e.g., in the case of a config change that moves
	// addresses from one pool to another
	pool, tracked := a.pools[al.pool]
	if tracked {
		if err := pool.Release(al.ip, svc); err != nil {
			return err
		}
		poolActive.WithLabelValues(al.pool).Set(float64(pool.InUse()))
	}

	delete(a.allocated, svc)

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

func (a *Allocator) parseConfig(myCluster string, groups []*purelbv1.ServiceGroup) (map[string]Pool, error) {
	pools := map[string]Pool{}

	for i, group := range groups {
		pool, err := a.parseGroup(myCluster, group.Spec)
		if err != nil {
			return nil, fmt.Errorf("parsing address pool #%d: %s", i+1, err)
		}

		// Check that the pool isn't already defined
		if pools[group.Name] != nil {
			return nil, fmt.Errorf("duplicate definition of pool %q", group.Name)
		}

		// Check that this pool doesn't overlap with any of the previous
		// ones
		for name, r := range pools {
			if pool.Overlaps(r) {
				return nil, fmt.Errorf("pool %q overlaps with already defined pool %q", group.Name, name)
			}
		}

		pools[group.Name] = pool
	}

	return pools, nil
}

func (a *Allocator) parseGroup(myCluster string, group purelbv1.ServiceGroupSpec) (Pool, error) {
	if group.Local != nil {
		ret, err := NewLocalPool(group.Local.Pool, group.Local.Subnet, group.Local.Aggregation)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	} else if group.Netbox != nil {
		ret, err := NewNetboxPool(group.Netbox.URL, group.Netbox.Tenant, group.Netbox.Aggregation)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	} else if group.EGW != nil {
		// Initialize the EGW proxy
		egw, err := acnodal.NewEGW(myCluster, *group.EGW)
		if err != nil {
			return nil, err
		}

		ret, err := NewEGWPool(a.logger, egw, group.EGW.Aggregation)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	}

	return nil, fmt.Errorf("Pool is not local or EGW")
}
