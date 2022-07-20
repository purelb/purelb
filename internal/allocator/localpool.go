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
	"strings"

	"github.com/go-kit/kit/log"
	"github.com/vishvananda/netlink/nl"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/local"
	purelbv1 "purelb.io/pkg/apis/v1"
)

// Pool is the configuration of an IP address pool.
type LocalPool struct {
	logger log.Logger

	// v4Ranges contains the IPV4 addresses that are part of this
	// pool. config.Parse guarantees that these are non-overlapping,
	// both within and between pools.
	v4Ranges []*IPRange

	// v6Ranges contains the IPV6 addresses that are part of this
	// pool. config.Parse guarantees that these are non-overlapping,
	// both within and between pools.
	v6Ranges []*IPRange

	// Map of the addresses that have been assigned.
	addressesInUse map[string]map[string]bool // ip.String() -> svc name -> true

	// Map of the "sharing keys" for each IP address
	sharingKeys map[string]*Key // ip.String() -> pointer to sharing key

	portsInUse map[string]map[Port]string // ip.String() -> Port -> svc
}

func NewLocalPool(log log.Logger, spec purelbv1.ServiceGroupLocalSpec) (*LocalPool, error) {
	pool := LocalPool{
		logger:         log,
		addressesInUse: map[string]map[string]bool{},
		sharingKeys:    map[string]*Key{},
		portsInUse:     map[string]map[Port]string{},
	}

	// If there ranges in the "legacy" slots, add them to the slices.
	if spec.V6Pool != nil {
		spec.V6Pools = append(spec.V6Pools, spec.V6Pool)
	}
	if spec.V4Pool != nil {
		spec.V4Pools = append(spec.V4Pools, spec.V4Pool)
	}

	// See if there's an IPV6 range in the spec
	for _, v6pool := range spec.V6Pools {
		iprange, err := NewIPRange(v6pool.Pool)
		if err != nil {
			return nil, err
		}

		// Validate that the range is contained by the subnet.
		_, subnet, err := net.ParseCIDR(v6pool.Subnet)
		if err != nil {
			return nil, err
		}
		if !iprange.ContainedBy(*subnet) {
			return nil, fmt.Errorf("IPV6 range %s not contained by network %s", iprange, subnet)
		}

		pool.v6Ranges = append(pool.v6Ranges, &iprange)
	}

	// See if there's an IPV4 range in the spec
	for _, v4pool := range spec.V4Pools {
		iprange, err := NewIPRange(v4pool.Pool)
		if err != nil {
			return nil, err
		}

		// Validate that the range is contained by the subnet.
		_, subnet, err := net.ParseCIDR(v4pool.Subnet)
		if err != nil {
			return nil, err
		}
		if !iprange.ContainedBy(*subnet) {
			return nil, fmt.Errorf("IPV4 range %s not contained by network %s", iprange, subnet)
		}

		pool.v4Ranges = append(pool.v4Ranges, &iprange)
	}

	// See if there's a top-level range in the spec
	if spec.Pool != "" {
		// Validate that Subnet is at least well-formed
		iprange, err := NewIPRange(spec.Pool)
		if err == nil {
			// Validate that the range is contained by the subnet.
			_, subnet, err := net.ParseCIDR(spec.Subnet)
			if err != nil {
				return nil, err
			}
			if !iprange.ContainedBy(*subnet) {
				return nil, fmt.Errorf("Legacy range %s not contained by network %s", iprange, subnet)
			}

			// We have a legacy (i.e., top-level) range, let's see where it
			// goes
			if iprange.Family() == nl.FAMILY_V6 {
				if pool.v6Ranges == nil {
					pool.v6Ranges = append(pool.v6Ranges, &iprange)
				} else {
					return nil, fmt.Errorf("Invalid Spec: both legacy Pool and V6Pool are IPV6")
				}
			} else if iprange.Family() == nl.FAMILY_V4 {
				if pool.v4Ranges == nil {
					pool.v4Ranges = append(pool.v4Ranges, &iprange)
				} else {
					return nil, fmt.Errorf("Invalid Spec: both legacy Pool and V4Pool are IPV4")
				}
			}
		}
	}

	// Last check: if we don't have *any* valid range then it's a bad
	// Spec
	if pool.v6Ranges == nil && pool.v4Ranges == nil {
		return nil, fmt.Errorf("no valid address range found")
	}

	return &pool, nil
}

func (p LocalPool) Notify(service *v1.Service) error {
	nsName := namespacedName(service)
	sharingKey := &Key{Sharing: SharingKey(service)}
	ports := Ports(service)

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ipstr := ingress.IP
		ip := net.ParseIP(ipstr)
		if ip == nil {
			p.logger.Log("localpool", "notify-failure", "service", nsName, "ip", ipstr)
			continue
		}
		p.logger.Log("localpool", "notify-existing", "service", nsName, "ip", ipstr)

		p.sharingKeys[ipstr] = sharingKey
		if p.addressesInUse[ipstr] == nil {
			p.addressesInUse[ipstr] = map[string]bool{}
		}
		p.addressesInUse[ipstr][nsName] = true
		if p.portsInUse[ipstr] == nil {
			p.portsInUse[ipstr] = map[Port]string{}
		}
		for _, port := range ports {
			p.portsInUse[ipstr][port] = nsName
		}
	}

	return nil
}

// available determines whether an address is available. The decision
// depends on whether another service is using the address, and if so,
// whether this service can share the address with it. error will be
// nil if the ip is available, and will contain an explanation if not.
func (p LocalPool) available(ip net.IP, service *v1.Service) error {
	nsName := namespacedName(service)
	key := &Key{Sharing: SharingKey(service)}
	ports := Ports(service)

	// No key: no sharing
	if key == nil {
		key = &Key{}
	}

	// Does the IP already have allocs? If so, needs to be the same
	// sharing key, and have non-overlapping ports. If not, the
	// proposed IP needs to be allowed by configuration.
	if existingSK := p.SharingKey(ip); existingSK != nil {
		if err := sharingOK(existingSK, key); err != nil {

			// Sharing key is incompatible. However, if the owner is
			// the same service, and is the only user of the IP, we
			// can just update its sharing key in place.
			var otherSvcs []string
			for _, otherSvc := range p.servicesOnIP(ip) {
				if otherSvc != nsName {
					otherSvcs = append(otherSvcs, otherSvc)
				}
			}
			if len(otherSvcs) > 0 {
				return fmt.Errorf("can't change sharing key for %q, address also in use by %s", nsName, strings.Join(otherSvcs, ","))
			}
		}

		for _, port := range ports {
			if curSvc, ok := p.portsInUse[ip.String()][port]; ok && curSvc != nsName {
				return fmt.Errorf("port %s on %q is already in use by %s", port, ip, curSvc)
			}
		}
	}

	return nil
}

// AssignNext assigns the next available IP to service.
func (p LocalPool) AssignNext(service *v1.Service) error {
	families, err := p.whichFamilies(service)
	if err != nil {
		return err
	}

	if len(families) == 0 {
		// Any address is OK so try V6 first then V4 and assign the first
		// one that succeeds
		if err = p.assignFamily(nl.FAMILY_V6, service); err == nil {
			return err
		}
		return p.assignFamily(nl.FAMILY_V4, service)
	}

	// We have a specific set of families to assign
	for _, family := range families {
		if err := p.assignFamily(family, service); err != nil {
			return err
		}
	}
	return nil
}

func (p LocalPool) assignFamily(family int, service *v1.Service) error {
	for pos := p.first(family); pos != nil; pos = p.next(pos) {
		if err := p.Assign(pos, service); err == nil {
			// we found an available address
			return err
		}
	}

	return fmt.Errorf("no available addresses for service %s in family %d", namespacedName(service), family)
}

// Assign assigns a service to an IP.
func (p LocalPool) Assign(ip net.IP, service *v1.Service) error {
	if err := p.available(ip, service); err != nil {
		return err
	}

	// we have an IP selected somehow, so program the data plane
	addIngress(p.logger, service, ip)

	// Update our internal allocation data structures
	return p.Notify(service)
}

// Release releases an IP so it can be assigned again.
func (p LocalPool) Release(service string) error {
	for ipstr, allocs := range p.addressesInUse {
		delete(allocs, service)
		if len(allocs) == 0 {
			delete(p.addressesInUse, ipstr)
			delete(p.sharingKeys, ipstr)
		}
		for port, svc := range p.portsInUse[ipstr] {
			if svc == service {
				delete(p.portsInUse[ipstr], port)
			}
		}
		if len(p.portsInUse[ipstr]) == 0 {
			delete(p.portsInUse, ipstr)
		}
	}
	return nil
}

// InUse returns the count of addresses that currently have services
// assigned.
func (p LocalPool) InUse() int {
	return len(p.addressesInUse)
}

// servicesOnIP returns the names of the services who are assigned to
// the address.
func (p LocalPool) servicesOnIP(ip net.IP) []string {
	ipstr := ip.String()
	svcs, has := p.addressesInUse[ipstr]
	if has {
		keys := make([]string, 0, len(svcs))
		for k := range svcs {
			keys = append(keys, k)
		}
		return keys
	}
	return []string{}
}

// SharingKey returns the "sharing key" for the specified address.
func (p LocalPool) SharingKey(ip net.IP) *Key {
	return p.sharingKeys[ip.String()]
}

// first returns the first net.IP within this Pool, or nil if the pool
// has no addresses. The "first" address is the lowest address in the
// first range, although it might not be the lowest in the entire
// pool.
func (p LocalPool) first(family int) net.IP {
	if family == nl.FAMILY_V6 && len(p.v6Ranges) > 0 {
		return p.v6Ranges[0].First()
	}
	if family == nl.FAMILY_V4 && len(p.v4Ranges) > 0 {
		return p.v4Ranges[0].First()
	}

	// We found neither V6 nor V4.
	return nil
}

// next returns the next net.IP within this Pool, or nil if the
// provided net.IP is the last address in the range.
func (p LocalPool) next(ip net.IP) net.IP {
	if local.AddrFamily(ip) == nl.FAMILY_V6 {
		for i, v6 := range p.v6Ranges {
			// If this range contains the current address, and has another
			// address available then return that.
			if v6.Contains(ip) {
				next := v6.Next(ip)
				if next != nil {
					return next
				}

				// We've exhausted this range so let's see if there's another.
				if i+1 >= len(p.v6Ranges) {
					// There are no more ranges so we're done.
					return nil
				} else {
					// This range is exhausted so the "next" address is the next
					// range's "first".
					return p.v6Ranges[i+1].First()
				}
			}
		}
	}

	if local.AddrFamily(ip) == nl.FAMILY_V4 {
		for i, v4 := range p.v4Ranges {
			// If this range contains the current address, and has another
			// address available then return that.
			if v4.Contains(ip) {
				next := v4.Next(ip)
				if next != nil {
					return next
				}

				// We've exhausted this range so let's see if there's another.
				if i+1 >= len(p.v4Ranges) {
					// There are no more ranges so we're done.
					return nil
				} else {
					// This range is exhausted so the "next" address is the next
					// range's "first".
					return p.v4Ranges[i+1].First()
				}
			}
		}
	}

	return nil
}

// Size returns the total number of addresses in this pool if it's a
// local pool, or 0 if it's a remote pool.
func (p LocalPool) Size() (size uint64) {
	for _, v6 := range p.v6Ranges {
		size += v6.Size()
	}
	for _, v4 := range p.v4Ranges {
		size += v4.Size()
	}
	return
}

// Overlaps indicates whether the other Pool overlaps with this one
// (i.e., has any addresses in common).  It returns true if there are
// any common addresses and false if there aren't.
func (p LocalPool) Overlaps(other Pool) bool {
	lpool, ok := other.(LocalPool)
	if !ok {
		return false
	}

	for _, v4 := range p.v4Ranges {
		for _, otherV4 := range lpool.v4Ranges {
			if v4.Overlaps(*otherV4) {
				return true
			}
		}
	}
	for _, v6 := range p.v6Ranges {
		for _, otherV6 := range lpool.v6Ranges {
			if v6.Overlaps(*otherV6) {
				return true
			}
		}
	}

	return false
}

// Contains indicates whether the provided net.IP represents an
// address within this Pool.  It returns true if so, false otherwise.
func (p LocalPool) Contains(ip net.IP) bool {
	for _, v4 := range p.v4Ranges {
		if v4.Contains(ip) {
			return true
		}
	}
	for _, v6 := range p.v6Ranges {
		if v6.Contains(ip) {
			return true
		}
	}

	return false
}

// whichFamilies determines which IP families to assign to this
// service. It returns an array of int containing nl.FAMILY_V? values,
// one for each address to assign, in the order that they should be
// assigned. An empty array means that any family is OK.
func (p LocalPool) whichFamilies(service *v1.Service) ([]int, error) {
	if len(service.Spec.IPFamilies) == 0 {
		return []int{}, nil
	}

	families := []int{}
	for _, family := range service.Spec.IPFamilies {
		if family == v1.IPv6Protocol {
			families = append(families, nl.FAMILY_V6)
		} else if family == v1.IPv4Protocol {
			families = append(families, nl.FAMILY_V4)
		} else {
			p.logger.Log("service %s unknown IP family %s", service.Name, family)
		}
	}

	return families, nil
}
