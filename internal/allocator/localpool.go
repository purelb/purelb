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

	"github.com/go-kit/log"
	"github.com/vishvananda/netlink/nl"
	v1 "k8s.io/api/core/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// LocalPool is the configuration of an IP address pool.
type LocalPool struct {
	name string

	logger log.Logger

	// poolType indicates whether this is a "local" or "remote" pool.
	// Local pools announce on the node's real interface (same subnet).
	// Remote pools announce on the dummy interface (different subnet, for BGP).
	poolType string

	// skipIPv6DAD indicates whether to skip IPv6 Duplicate Address Detection.
	skipIPv6DAD bool

	// v4Ranges contains the IPV4 addresses that are part of this
	// pool. config.Parse guarantees that these are non-overlapping,
	// both within and between pools.
	v4Ranges []*purelbv2.IPRange

	// v6Ranges contains the IPV6 addresses that are part of this
	// pool. config.Parse guarantees that these are non-overlapping,
	// both within and between pools.
	v6Ranges []*purelbv2.IPRange

	// Map of the addresses that have been assigned.
	addressesInUse map[string]map[string]bool // ip.String() -> svc name -> true

	// Map of the "sharing keys" for each IP address
	sharingKeys map[string]*Key // ip.String() -> pointer to sharing key

	portsInUse map[string]map[Port]string // ip.String() -> Port -> svc

	// sharingKeyToIP is a reverse index mapping sharing keys to their bound IP,
	// per address family. Once a sharing key is assigned to an IP in a family,
	// all services with that key must use the same IP in that family.
	// Key format: "sharingKey:4" or "sharingKey:6" for IPv4/IPv6 respectively.
	sharingKeyToIP map[string]string // "sharingKey:family" -> ip.String()
}

// NewLocalPool creates a new LocalPool from the given address pools.
// poolType should be "local" for addresses announced on the node's interface,
// or "remote" for addresses announced on the dummy interface (for BGP/routing).
func NewLocalPool(name string, log log.Logger, v4Pool *purelbv2.AddressPool, v6Pool *purelbv2.AddressPool, v4Pools []purelbv2.AddressPool, v6Pools []purelbv2.AddressPool, poolType string, skipIPv6DAD bool) (LocalPool, error) {
	pool := LocalPool{
		name:           name,
		logger:         log,
		poolType:       poolType,
		skipIPv6DAD:    skipIPv6DAD,
		addressesInUse: map[string]map[string]bool{},
		sharingKeys:    map[string]*Key{},
		portsInUse:     map[string]map[Port]string{},
		sharingKeyToIP: map[string]string{},
	}

	// If there are ranges in the singular slots, add them to the slices.
	if v6Pool != nil {
		v6Pools = append(v6Pools, *v6Pool)
	}
	if v4Pool != nil {
		v4Pools = append(v4Pools, *v4Pool)
	}

	// See if there's an IPV6 range in the spec
	for _, v6pool := range v6Pools {
		iprange, err := purelbv2.NewIPRange(v6pool.Pool)
		if err != nil {
			return pool, err
		}

		// Validate that the range is contained by the subnet.
		_, subnet, err := net.ParseCIDR(v6pool.Subnet)
		if err != nil {
			return pool, err
		}
		if !iprange.ContainedBy(*subnet) {
			return pool, fmt.Errorf("IPV6 range %s not contained by network %s", iprange, subnet)
		}

		pool.v6Ranges = append(pool.v6Ranges, &iprange)
	}

	// See if there's an IPV4 range in the spec
	for _, v4pool := range v4Pools {
		iprange, err := purelbv2.NewIPRange(v4pool.Pool)
		if err != nil {
			return pool, err
		}

		// Validate that the range is contained by the subnet.
		_, subnet, err := net.ParseCIDR(v4pool.Subnet)
		if err != nil {
			return pool, err
		}
		if !iprange.ContainedBy(*subnet) {
			return pool, fmt.Errorf("IPV4 range %s not contained by network %s", iprange, subnet)
		}

		pool.v4Ranges = append(pool.v4Ranges, &iprange)
	}

	// Last check: if we don't have *any* valid range then it's a bad spec
	if pool.v6Ranges == nil && pool.v4Ranges == nil {
		return pool, fmt.Errorf("no valid address range found")
	}

	return pool, nil
}

func (p LocalPool) Notify(service *v1.Service) error {
	nsName := namespacedName(service)
	sharingKey := &Key{Sharing: SharingKey(service)}
	ports := Ports(service)

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ipstr := ingress.IP
		ip := net.ParseIP(ipstr)
		if ip == nil {
			p.logger.Log("localpool", "notify-failure", "svc-name", nsName, "ip", ipstr)
			continue
		}
		p.logger.Log("localpool", "notify-existing", "svc-name", nsName, "ip", ipstr)

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

		// Update reverse index: sharing key -> IP (per address family)
		if sharingKey.Sharing != "" {
			family := purelbv2.AddrFamily(ip)
			indexKey := fmt.Sprintf("%s:%d", sharingKey.Sharing, family)
			p.sharingKeyToIP[indexKey] = ipstr
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

	// If this service has a sharing key, check if that key is already
	// bound to a different IP in this address family. If so, this service
	// MUST use that IP - it cannot be assigned to any other IP.
	if key.Sharing != "" {
		family := purelbv2.AddrFamily(ip)
		boundIP := p.ipForSharingKey(key.Sharing, family)
		if boundIP != nil && !boundIP.Equal(ip) {
			return fmt.Errorf("sharing key %q is bound to %s, cannot use %s",
				key.Sharing, boundIP, ip)
		}
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
	// Check if the service already has an address for this family
	// This supports IP family transitions (e.g., SingleStack → DualStack)
	// where we want to keep existing addresses and only allocate missing ones
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ip := net.ParseIP(ingress.IP)
		if ip == nil {
			continue
		}
		ingressFamily := nl.FAMILY_V4
		if ip.To4() == nil {
			ingressFamily = nl.FAMILY_V6
		}
		if ingressFamily == family {
			// Already have an address for this family, skip allocation
			p.logger.Log("localpool", "skip-assign", "service", namespacedName(service),
				"family", family, "existing-ip", ingress.IP)
			return nil
		}
	}

	var lastErr error
	var boundIPErr error

	// Pre-compute the bound IP for this service's sharing key (if any)
	sharingKey := SharingKey(service)
	boundIP := p.ipForSharingKey(sharingKey, family)

	for pos := p.first(family); pos != nil; pos = p.next(pos) {
		if err := p.Assign(pos, service); err == nil {
			// we found an available address
			return nil
		} else {
			lastErr = err
			// Capture the error from the bound IP specifically -
			// this is the most relevant error for sharing conflicts
			if boundIP != nil && boundIP.Equal(pos) {
				boundIPErr = err
			}
		}
	}

	// Determine final error - prefer bound IP error (e.g., port conflict)
	finalErr := boundIPErr
	if finalErr == nil {
		finalErr = lastErr
	}
	if finalErr == nil {
		finalErr = fmt.Errorf("no available addresses for service %s in family %d",
			namespacedName(service), family)
	}

	// Categorize the error for metrics
	reason := "exhausted"
	errStr := finalErr.Error()
	if strings.Contains(errStr, "port") && strings.Contains(errStr, "already in use") {
		reason = "port_conflict"
	} else if strings.Contains(errStr, "sharing key") {
		reason = "sharing_key_conflict"
	}

	// Log and record metric once per failed allocation
	if p.logger != nil {
		p.logger.Log("op", "assignFamily", "result", "failed",
			"service", namespacedName(service),
			"family", family,
			"reason", reason,
			"error", finalErr.Error())
	}
	allocationRejected.WithLabelValues(p.name, reason).Inc()

	return finalErr
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

			// Clean up sharing key reverse index before deleting the forward mapping
			if key := p.sharingKeys[ipstr]; key != nil && key.Sharing != "" {
				ip := net.ParseIP(ipstr)
				family := purelbv2.AddrFamily(ip)
				indexKey := fmt.Sprintf("%s:%d", key.Sharing, family)
				delete(p.sharingKeyToIP, indexKey)
			}
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

// ReleaseIP releases a specific IP address for a service. Used during
// IP family transitions (e.g., DualStack → SingleStack).
func (p LocalPool) ReleaseIP(service string, ip net.IP) error {
	ipstr := ip.String()

	// Check if this IP is in our pool
	allocs, exists := p.addressesInUse[ipstr]
	if !exists {
		return nil // IP not in this pool, nothing to do
	}

	// Remove service from this IP's allocations
	delete(allocs, service)

	// If no services are using this IP, clean up completely
	if len(allocs) == 0 {
		delete(p.addressesInUse, ipstr)

		// Clean up sharing key reverse index before deleting the forward mapping
		if key := p.sharingKeys[ipstr]; key != nil && key.Sharing != "" {
			family := purelbv2.AddrFamily(ip)
			indexKey := fmt.Sprintf("%s:%d", key.Sharing, family)
			delete(p.sharingKeyToIP, indexKey)
		}
		delete(p.sharingKeys, ipstr)
	}

	// Clean up ports for this service on this IP
	for port, svc := range p.portsInUse[ipstr] {
		if svc == service {
			delete(p.portsInUse[ipstr], port)
		}
	}
	if len(p.portsInUse[ipstr]) == 0 {
		delete(p.portsInUse, ipstr)
	}

	p.logger.Log("localpool", "release-ip", "service", service, "ip", ipstr)
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

// ipForSharingKey returns the IP address that's already bound to the
// given sharing key in the specified address family, or nil if no IP
// has that sharing key yet. This is an O(1) lookup using the reverse index.
// family should be nl.FAMILY_V4 or nl.FAMILY_V6.
func (p LocalPool) ipForSharingKey(key string, family int) net.IP {
	if key == "" {
		return nil
	}
	indexKey := fmt.Sprintf("%s:%d", key, family)
	if ipstr, exists := p.sharingKeyToIP[indexKey]; exists {
		return net.ParseIP(ipstr)
	}
	return nil
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
	if purelbv2.AddrFamily(ip) == nl.FAMILY_V6 {
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

	if purelbv2.AddrFamily(ip) == nl.FAMILY_V4 {
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

func (p LocalPool) String() string {
	return p.name
}

// PoolType returns the type of this pool: "local" for addresses announced
// on the node's local interface (same subnet), or "remote" for addresses
// announced on the dummy interface (different subnet, for BGP/routing).
func (p LocalPool) PoolType() string {
	return p.poolType
}

// SkipIPv6DAD returns whether IPv6 Duplicate Address Detection should
// be skipped for addresses from this pool.
func (p LocalPool) SkipIPv6DAD() bool {
	return p.skipIPv6DAD
}
