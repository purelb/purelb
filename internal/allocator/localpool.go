// Copyright 2017 Google Inc.
// Copyright 2020-2026 Acnodal Inc.
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
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/go-kit/log"
	"github.com/vishvananda/netlink/nl"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/logging"
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

	// multiPool indicates whether multi-pool allocation is enabled.
	// When true, services get one IP from each address range (per family)
	// that has active nodes.
	multiPool bool

	// balancePools indicates whether balanced allocation is enabled.
	// When true, new allocations pick the range with the fewest IPs
	// currently in use, distributing services evenly across subnets.
	balancePools bool

	// v4Ranges contains the IPV4 addresses that are part of this
	// pool. config.Parse guarantees that these are non-overlapping,
	// both within and between pools.
	v4Ranges []*purelbv2.IPRange

	// v6Ranges contains the IPV6 addresses that are part of this
	// pool. config.Parse guarantees that these are non-overlapping,
	// both within and between pools.
	v6Ranges []*purelbv2.IPRange

	// v4Subnets stores the subnet CIDR for each v4Range, in parallel.
	// Used by AssignNextPerRange to match ranges to active subnets.
	v4Subnets []string

	// v6Subnets stores the subnet CIDR for each v6Range, in parallel.
	v6Subnets []string

	// v4Names and v6Names are, in parallel, each range's entry name: its
	// AddressPool name, or its subnet when unnamed (AllocRequest.Range).
	v4Names []string
	v6Names []string

	// v4Prefix and v6Prefix are, for each range in parallel, the prefix
	// length the announcer adds an address from that range with: the
	// subnet's, or a remote pool's aggregation. See reserved.
	v4Prefix []int
	v6Prefix []int

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
// multiPool enables multi-pool allocation where services get one IP per range.
// balancePools enables balanced allocation where new services get IPs from the least-used range.
func NewLocalPool(name string, log log.Logger, v4Pool *purelbv2.AddressPool, v6Pool *purelbv2.AddressPool, v4Pools []purelbv2.AddressPool, v6Pools []purelbv2.AddressPool, poolType string, skipIPv6DAD bool, multiPool bool, balancePools bool) (LocalPool, error) {
	pool := LocalPool{
		name:           name,
		logger:         log,
		poolType:       poolType,
		skipIPv6DAD:    skipIPv6DAD,
		multiPool:      multiPool,
		balancePools:   balancePools,
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

		prefix, err := announcedPrefix(subnet, poolType, v6pool.Aggregation)
		if err != nil {
			return pool, err
		}
		pool.v6Ranges = append(pool.v6Ranges, &iprange)
		pool.v6Subnets = append(pool.v6Subnets, v6pool.Subnet)
		pool.v6Names = append(pool.v6Names, entryName(v6pool))
		pool.v6Prefix = append(pool.v6Prefix, prefix)
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

		prefix, err := announcedPrefix(subnet, poolType, v4pool.Aggregation)
		if err != nil {
			return pool, err
		}
		pool.v4Ranges = append(pool.v4Ranges, &iprange)
		pool.v4Subnets = append(pool.v4Subnets, v4pool.Subnet)
		pool.v4Names = append(pool.v4Names, entryName(v4pool))
		pool.v4Prefix = append(pool.v4Prefix, prefix)
	}

	// Last check: if we don't have *any* valid range then it's a bad spec
	if pool.v6Ranges == nil && pool.v4Ranges == nil {
		return pool, fmt.Errorf("no valid address range found")
	}

	return pool, nil
}

func (p LocalPool) Notify(_ context.Context, service *v1.Service) error {
	req := serviceRequest(service, nil)

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ipstr := ingress.IP
		ip := net.ParseIP(ipstr)
		if ip == nil {
			p.logger.Log("localpool", "notify-failure", "svc-name", req.Key, "ip", ipstr)
			continue
		}

		logging.Debug(p.logger, "localpool", "notify-existing", "svc-name", req.Key, "ip", ipstr)
		p.record(req, ipstr, ip)
	}

	return nil
}

// record records ip, whose map key is ipstr, as allocated to req.Key
// with req's sharing key and ports.
func (p LocalPool) record(req AllocRequest, ipstr string, ip net.IP) {
	p.sharingKeys[ipstr] = &Key{Sharing: req.SharingKey}
	if p.addressesInUse[ipstr] == nil {
		p.addressesInUse[ipstr] = map[string]bool{}
	}
	p.addressesInUse[ipstr][req.Key] = true
	if p.portsInUse[ipstr] == nil {
		p.portsInUse[ipstr] = map[Port]string{}
	}
	for _, port := range req.Ports {
		p.portsInUse[ipstr][port] = req.Key
	}

	// Update reverse index: sharing key -> IP (per address family)
	if req.SharingKey != "" {
		family := purelbv2.AddrFamily(ip)
		indexKey := fmt.Sprintf("%s:%d", req.SharingKey, family)
		p.sharingKeyToIP[indexKey] = ipstr
	}
}

// hold records ip as held by key without ports or a sharing key (see
// AllocRequest.Reference). An address nobody else has recorded is marked
// unshareable, so no other owner can take it.
func (p LocalPool) hold(key string, ip net.IP) {
	ipstr := ip.String()
	if p.sharingKeys[ipstr] == nil {
		p.sharingKeys[ipstr] = &Key{}
	}
	if p.addressesInUse[ipstr] == nil {
		p.addressesInUse[ipstr] = map[string]bool{}
	}
	p.addressesInUse[ipstr][key] = true
	logging.Debug(p.logger, "localpool", "hold", "key", key, "ip", ipstr)
}

// available determines whether an address is available. The decision
// depends on whether another owner is using the address, and if so,
// whether req can share the address with it. error will be nil if the
// ip is available, and will contain an explanation if not.
func (p LocalPool) available(ip net.IP, req AllocRequest) error {
	nsName := req.Key
	key := &Key{Sharing: req.SharingKey}
	ports := req.Ports

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
func (p LocalPool) AssignNext(ctx context.Context, service *v1.Service) error {
	families, err := p.whichFamilies(service)
	if err != nil {
		return err
	}
	ips, err := p.Allocate(ctx, serviceRequest(service, families))
	for _, ip := range ips {
		p.addService(ctx, service, ip)
	}
	return err
}

// addService adds ip to service's ingress and records the service's
// addresses.
func (p LocalPool) addService(ctx context.Context, service *v1.Service, ip net.IP) {
	addIngress(p.logger, service, ip)
	p.Notify(ctx, service)
}

// Allocate allocates addresses for req: a reference hold on req.Explicit,
// req.Explicit itself, or the next free address in each requested family.
func (p LocalPool) Allocate(ctx context.Context, req AllocRequest) ([]net.IP, error) {
	if req.Reference {
		for _, ip := range req.Explicit {
			if !p.Contains(ip) {
				return nil, fmt.Errorf("%s is not in pool %q", ip, p.name)
			}
		}
		for _, ip := range req.Explicit {
			p.hold(req.Key, ip)
		}
		return req.Explicit, nil
	}

	if len(req.Explicit) > 0 {
		var ips []net.IP
		for _, ip := range req.Explicit {
			if err := p.take(ip, req); err != nil {
				return ips, err
			}
			ips = append(ips, ip)
		}
		return ips, nil
	}

	if len(req.Families) == 0 {
		// Any address is OK so try V6 first then V4 and assign the first
		// one that succeeds
		if ip, err := p.assignFamily(ctx, nl.FAMILY_V6, req); err == nil {
			return ipList(ip), nil
		}
		ip, err := p.assignFamily(ctx, nl.FAMILY_V4, req)
		return ipList(ip), err
	}

	// We have a specific set of families to assign
	var ips []net.IP
	var lastErr error
	satisfied := 0
	for _, family := range req.Families {
		ip, err := p.assignFamily(ctx, family, req)
		if err != nil {
			if req.Policy == AllFamilies {
				return ips, err
			}
			lastErr = err
			continue
		}
		satisfied++
		ips = append(ips, ipList(ip)...)
	}
	if satisfied == 0 {
		return ips, lastErr
	}
	return ips, nil
}

// ipList returns ip as a one-element list, or an empty list if it's nil.
func ipList(ip net.IP) []net.IP {
	if ip == nil {
		return nil
	}
	return []net.IP{ip}
}

// take allocates ip to req if it's free for req.
func (p LocalPool) take(ip net.IP, req AllocRequest) error {
	if err := p.reserved(ip); err != nil {
		return err
	}
	if err := p.available(ip, req); err != nil {
		return err
	}
	p.record(req, ip.String(), ip)
	return nil
}

// assignFamily allocates the next free address in family to req and
// returns it. It returns a nil address and no error when req already
// holds an address in family.
func (p LocalPool) assignFamily(ctx context.Context, family int, req AllocRequest) (net.IP, error) {
	// Check if the owner already has an address for this family
	// This supports IP family transitions (e.g., SingleStack → DualStack)
	// where we want to keep existing addresses and only allocate missing ones
	for _, ip := range req.Existing {
		if purelbv2.AddrFamily(ip) == family {
			// Already have an address for this family, skip allocation
			p.logger.Log("localpool", "skip-assign", "service", req.Key,
				"family", family, "existing-ip", ip.String())
			return nil, nil
		}
	}

	// A named entry: first fit within its ranges only.
	if req.Range != "" {
		return p.assignFamilyInEntry(ctx, family, req)
	}

	// Balanced allocation: pick least-used range.
	// Skip for owners with sharing keys — they must use the bound IP.
	if p.balancePools && req.SharingKey == "" {
		return p.assignFamilyBalancePools(ctx, family, req)
	}

	var lastErr error
	var boundIPErr error

	// Pre-compute the bound IP for this owner's sharing key (if any)
	sharingKey := req.SharingKey
	boundIP := p.ipForSharingKey(sharingKey, family)

	for pos := p.first(family); pos != nil; pos = p.next(pos) {
		if err := p.take(pos, req); err == nil {
			// we found an available address
			return pos, nil
		} else {
			lastErr = err
			// Capture the error from the bound IP specifically -
			// this is the most relevant error for sharing conflicts
			if boundIP != nil && boundIP.Equal(pos) {
				boundIPErr = err
			}
		}
	}

	// Determine final error:
	// - If the service has a sharing key, prefer the bound IP error (port conflict etc.)
	// - If no sharing key, every IP was taken by a different service — pool exhausted
	var finalErr error
	if boundIPErr != nil {
		finalErr = boundIPErr
	} else if sharingKey != "" && lastErr != nil {
		finalErr = lastErr
	} else {
		finalErr = fmt.Errorf("pool %q exhausted: no available addresses for %s",
			p.name, req.Key)
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
			"service", req.Key,
			"family", family,
			"reason", reason,
			"error", finalErr.Error())
	}
	allocationRejected.WithLabelValues(p.name, reason).Inc()

	return nil, finalErr
}

// Assign assigns a service to an IP.
func (p LocalPool) Assign(ctx context.Context, ip net.IP, service *v1.Service) error {
	req := serviceRequest(service, nil)
	req.Explicit = []net.IP{ip}
	ips, err := p.Allocate(ctx, req)
	for _, ip := range ips {
		p.addService(ctx, service, ip)
	}
	return err
}

// announcedPrefix is the prefix length an address from a pool with this
// subnet is added to an interface with: the subnet's, except that a remote
// pool's aggregation replaces it (local.addVirtualInt).
func announcedPrefix(subnet *net.IPNet, poolType, aggregation string) (int, error) {
	ones, _ := subnet.Mask.Size()
	if poolType != purelbv2.PoolTypeRemote || aggregation == "" || aggregation == "default" {
		return ones, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(aggregation, "/"))
	if err != nil {
		return 0, fmt.Errorf("invalid aggregation %q: %w", aggregation, err)
	}
	return n, nil
}

// reserved refuses the one address in each announced prefix that the
// kernel makes special when it adds an address with that prefix: the IPv4
// broadcast (the last address, for prefixes up to /30) and the IPv6
// subnet-router anycast (the first, for prefixes up to /126). As a VIP it
// wouldn't work as a unicast address, and the address guard keeps its own
// entry for it.
func (p LocalPool) reserved(ip net.IP) error {
	ranges, prefixes, bits, max := p.v6Ranges, p.v6Prefix, 128, 126
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
		ranges, prefixes, bits, max = p.v4Ranges, p.v4Prefix, 32, 30
	}
	for i, r := range ranges {
		if i >= len(prefixes) || !r.Contains(ip) || prefixes[i] > max {
			continue
		}
		mask := net.CIDRMask(prefixes[i], bits)
		special := ip.Mask(mask) // IPv6: the prefix itself
		if bits == 32 {
			special = make(net.IP, 4)
			for b := range special {
				special[b] = ip[b] | ^mask[b] // IPv4: all host bits set
			}
		}
		if ip.Equal(special) {
			kind := "broadcast"
			if bits == 128 {
				kind = "subnet-router anycast"
			}
			return fmt.Errorf("%s is the %s address of %s/%d and can't be a LoadBalancer address", ip, kind, ip.Mask(mask), prefixes[i])
		}
	}
	return nil
}

// Release releases every address held by key so it can be assigned
// again.
func (p LocalPool) Release(_ context.Context, key string) error {
	for ipstr, allocs := range p.addressesInUse {
		delete(allocs, key)
		if len(allocs) == 0 {
			delete(p.addressesInUse, ipstr)

			// Clean up sharing key reverse index before deleting the forward mapping
			if sk := p.sharingKeys[ipstr]; sk != nil && sk.Sharing != "" {
				ip := net.ParseIP(ipstr)
				family := purelbv2.AddrFamily(ip)
				indexKey := fmt.Sprintf("%s:%d", sk.Sharing, family)
				delete(p.sharingKeyToIP, indexKey)
			}
			delete(p.sharingKeys, ipstr)
		}
		for port, svc := range p.portsInUse[ipstr] {
			if svc == key {
				delete(p.portsInUse[ipstr], port)
			}
		}
		if len(p.portsInUse[ipstr]) == 0 {
			delete(p.portsInUse, ipstr)
		}
	}
	return nil
}

// ReleaseIP releases one address held by key. Used during IP family
// transitions (e.g., DualStack → SingleStack).
func (p LocalPool) ReleaseIP(_ context.Context, key string, ip net.IP) error {
	ipstr := ip.String()

	// Check if this IP is in our pool
	allocs, exists := p.addressesInUse[ipstr]
	if !exists {
		return nil // IP not in this pool, nothing to do
	}

	// Remove key from this IP's allocations
	delete(allocs, key)

	// If no services are using this IP, clean up completely
	if len(allocs) == 0 {
		delete(p.addressesInUse, ipstr)

		// Clean up sharing key reverse index before deleting the forward mapping
		if sk := p.sharingKeys[ipstr]; sk != nil && sk.Sharing != "" {
			family := purelbv2.AddrFamily(ip)
			indexKey := fmt.Sprintf("%s:%d", sk.Sharing, family)
			delete(p.sharingKeyToIP, indexKey)
		}
		delete(p.sharingKeys, ipstr)
	}

	// Clean up ports for this key on this IP
	for port, svc := range p.portsInUse[ipstr] {
		if svc == key {
			delete(p.portsInUse[ipstr], port)
		}
	}
	if len(p.portsInUse[ipstr]) == 0 {
		delete(p.portsInUse, ipstr)
	}

	p.logger.Log("localpool", "release-ip", "service", key, "ip", ipstr)
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
	for i, v6 := range p.v6Ranges {
		size += usable(v6, p.v6Prefix, i)
	}
	for i, v4 := range p.v4Ranges {
		size += usable(v4, p.v4Prefix, i)
	}
	return
}

// usable is how many of r's addresses can be handed out: its size less the
// reserved addresses in it (see reserved). prefixes[i] is r's announced
// prefix. A range too big to count (IPRange.Size returns MaxUint64) stays
// that.
func usable(r *purelbv2.IPRange, prefixes []int, i int) uint64 {
	size := r.Size()
	if size == math.MaxUint64 || i >= len(prefixes) {
		return size
	}
	from := r.First()
	bits, max := 128, 126
	if f4 := from.To4(); f4 != nil {
		from, bits, max = f4, 32, 30
	}
	if prefixes[i] > max {
		return size
	}
	lo := new(big.Int).SetBytes(from)
	hi := new(big.Int).Add(lo, new(big.Int).SetUint64(size-1))
	block := new(big.Int).Lsh(big.NewInt(1), uint(bits-prefixes[i]))
	// Euclidean division floors for a positive divisor, which is what
	// counting multiples needs at both ends.
	var n *big.Int
	if bits == 32 {
		// The broadcast is the block's last address: x+1 is a multiple of
		// block, for x in [lo, hi].
		n = new(big.Int).Sub(
			new(big.Int).Div(new(big.Int).Add(hi, big.NewInt(1)), block),
			new(big.Int).Div(lo, block))
	} else {
		// The anycast is the block's first address: x is a multiple of block.
		n = new(big.Int).Sub(
			new(big.Int).Div(hi, block),
			new(big.Int).Div(new(big.Int).Sub(lo, big.NewInt(1)), block))
	}
	return size - n.Uint64()
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

// MultiPool returns whether multi-pool allocation is enabled for this pool.
func (p LocalPool) MultiPool() bool {
	return p.multiPool
}

// BalancePools returns whether balanced allocation is enabled for this pool.
func (p LocalPool) BalancePools() bool {
	return p.balancePools
}

// IPAMSource returns "Cluster" — LocalPool is authoritative for its own
// address space; PureLB's allocator does the IPAM directly.
func (p LocalPool) IPAMSource() string {
	return "Cluster"
}

// InUseV4 returns the number of IPv4 addresses currently allocated.
//
// The family is decided by looking for a colon rather than by parsing.
// Keys in addressesInUse are produced by net.IP.String(), which renders
// IPv4 -- including the IPv4-mapped form -- as a dotted quad and IPv6 with
// colons, so the test is exact. It matters because buildStatus calls this
// on every allocation and every release, for every pool, and a ParseIP per
// address meant an allocation and a full parse each time.
//
// Still counted from the map rather than cached, so it cannot drift out of
// step with the allocations it describes.
func (p LocalPool) InUseV4() int {
	n := 0
	for ipStr := range p.addressesInUse {
		if !strings.Contains(ipStr, ":") {
			n++
		}
	}
	return n
}

// InUseV6 returns the number of IPv6 addresses currently allocated.
// Same colon test as InUseV4; see there for why.
func (p LocalPool) InUseV6() int {
	n := 0
	for ipStr := range p.addressesInUse {
		if strings.Contains(ipStr, ":") {
			n++
		}
	}
	return n
}

// SizeV4 returns the total IPv4 capacity of this pool.
func (p LocalPool) SizeV4() (size uint64) {
	for i, v4 := range p.v4Ranges {
		size += usable(v4, p.v4Prefix, i)
	}
	return
}

// SizeV6 returns the total IPv6 capacity of this pool.
func (p LocalPool) SizeV6() (size uint64) {
	for i, v6 := range p.v6Ranges {
		size += usable(v6, p.v6Prefix, i)
	}
	return
}

// HasKnownCapacity returns true — LocalPool's capacity is sum of its
// configured ranges and is always known.
func (p LocalPool) HasKnownCapacity() bool {
	return true
}

// DisplayAddresses returns the original spec strings of the configured
// ranges (CIDRs and/or from-to forms), in v4-then-v6 order. Surfaced as
// .status.addresses so kubectl shows what the user wrote rather than the
// parsed "(from - to)" form.
func (p LocalPool) DisplayAddresses() []string {
	out := make([]string, 0, len(p.v4Ranges)+len(p.v6Ranges))
	for _, r := range p.v4Ranges {
		out = append(out, r.Raw())
	}
	for _, r := range p.v6Ranges {
		out = append(out, r.Raw())
	}
	return out
}

// countAllocationsPerRange counts how many IPs are currently allocated
// in each range. Used by balanced allocation to pick the least-used range.
func (p LocalPool) countAllocationsPerRange(ranges []*purelbv2.IPRange) []int {
	counts := make([]int, len(ranges))
	for ipStr := range p.addressesInUse {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		for i, r := range ranges {
			if r.Contains(ip) {
				counts[i]++
				break // IP can only be in one range
			}
		}
	}
	return counts
}

// assignFamilyBalancePools allocates an IP from the range with the fewest
// current allocations, distributing services evenly across subnets.
// Falls through to subsequent ranges if the preferred range is exhausted.
func (p LocalPool) assignFamilyBalancePools(ctx context.Context, family int, req AllocRequest) (net.IP, error) {
	var ranges []*purelbv2.IPRange
	if family == nl.FAMILY_V6 {
		ranges = p.v6Ranges
	} else {
		ranges = p.v4Ranges
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("no ranges for family %d", family)
	}

	// Count current allocations per range and sort by least-used.
	// Stable sort preserves declaration order on ties.
	counts := p.countAllocationsPerRange(ranges)
	indices := make([]int, len(ranges))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		return counts[indices[a]] < counts[indices[b]]
	})

	logging.Debug(p.logger, "op", "assignFamilyBalancePools", "service", req.Key,
		"family", family, "rangeCounts", fmt.Sprintf("%v", counts), "order", fmt.Sprintf("%v", indices))

	// Try ranges in least-allocated order
	for _, idx := range indices {
		if ip, err := p.assignFromRange(ctx, ranges[idx], req); err == nil {
			logging.Info(p.logger, "op", "assignFamilyBalancePools", "service", req.Key,
				"range", ranges[idx], "msg", "balancePools allocation selected range")
			balancePoolsAllocations.WithLabelValues(p.name).Inc()
			return ip, nil
		}
	}

	allocationRejected.WithLabelValues(p.name, "exhausted").Inc()
	return nil, fmt.Errorf("no available addresses for service %s in family %d (balancePools, all ranges exhausted)",
		req.Key, family)
}

// assignFromRange allocates the next available IP from a single range
// to req and returns it. Returns an error if the range is exhausted or
// all IPs have conflicts.
func (p LocalPool) assignFromRange(_ context.Context, ipRange *purelbv2.IPRange, req AllocRequest) (net.IP, error) {
	var lastErr error
	for ip := ipRange.First(); ip != nil; ip = ipRange.Next(ip) {
		if err := p.take(ip, req); err == nil {
			return ip, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("range %s is empty", ipRange)
	}
	return nil, lastErr
}

// AssignNextPerRange allocates one IP per address range (per family) that
// has an active subnet. activeSubnets is the set of subnets (in CIDR notation)
// with healthy lbnodeagents. Returns error only if NO IPs could be allocated.
func (p LocalPool) AssignNextPerRange(ctx context.Context, svc *v1.Service, activeSubnets []string) error {
	activeSet := make(map[string]bool, len(activeSubnets))
	for _, s := range activeSubnets {
		activeSet[s] = true
	}

	families, err := p.whichFamilies(svc)
	if err != nil {
		return err
	}
	// Default to IPv4 if no families specified
	if len(families) == 0 {
		families = []int{nl.FAMILY_V4}
	}

	allocated := 0
	activeRanges := 0
	skippedNoActive := 0
	req := serviceRequest(svc, families)

	for _, family := range families {
		var ranges []*purelbv2.IPRange
		var subnets []string
		if family == nl.FAMILY_V6 {
			ranges = p.v6Ranges
			subnets = p.v6Subnets
		} else {
			ranges = p.v4Ranges
			subnets = p.v4Subnets
		}

		for i, ipRange := range ranges {
			// Remote pools: all ranges are always active (all nodes announce to kubelb0)
			// Local pools: only ranges with active node subnets participate
			if p.poolType == purelbv2.PoolTypeLocal && !activeSet[subnets[i]] {
				skippedNoActive++
				p.logger.Log("op", "assignNextPerRange", "range", ipRange,
					"subnet", subnets[i], "msg", "skipping, no active nodes on subnet")
				continue
			}
			activeRanges++

			// Skip if service already has an IP from this range
			if p.serviceHasIPInRange(svc, ipRange) {
				p.logger.Log("op", "assignNextPerRange", "range", ipRange,
					"msg", "skipping, service already has IP in this range")
				allocated++ // Count existing as allocated
				continue
			}

			ip, err := p.assignFromRange(ctx, ipRange, req)
			if err != nil {
				p.logger.Log("op", "assignNextPerRange", "range", ipRange,
					"subnet", subnets[i], "error", err, "msg", "range exhausted, continuing")
				allocationRejected.WithLabelValues(p.name, "exhausted").Inc()
				continue
			}
			p.addService(ctx, svc, ip)
			allocated++
		}
	}

	if allocated == 0 {
		return fmt.Errorf("no IPs allocated for service %s: %d ranges skipped (no active nodes)",
			namespacedName(svc), skippedNoActive)
	}

	// Track partial allocations where some active ranges couldn't provide IPs
	if allocated < activeRanges {
		multipoolPartial.WithLabelValues(p.name).Inc()
	}

	return nil
}

// serviceHasIPInRange checks if the service already has an ingress IP
// that falls within the given range.
func (p LocalPool) serviceHasIPInRange(svc *v1.Service, ipRange *purelbv2.IPRange) bool {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ip := net.ParseIP(ingress.IP)
		if ip != nil && ipRange.Contains(ip) {
			return true
		}
	}
	return false
}

// ErrPoolEntryNotFound is returned for an AllocRequest.Range that names
// no entry of the pool.
var ErrPoolEntryNotFound = errors.New("pool entry not found")

// entryName is an AddressPool's entry name: its name, or its subnet.
func entryName(ap purelbv2.AddressPool) string {
	if ap.Name != "" {
		return ap.Name
	}
	return ap.Subnet
}

// entryRanges returns family's ranges in the entry name.
func (p LocalPool) entryRanges(family int, name string) []*purelbv2.IPRange {
	ranges, names := p.v4Ranges, p.v4Names
	if family == nl.FAMILY_V6 {
		ranges, names = p.v6Ranges, p.v6Names
	}
	var out []*purelbv2.IPRange
	for i, r := range ranges {
		if i < len(names) && names[i] == name {
			out = append(out, r)
		}
	}
	return out
}

// HasEntry reports whether the pool has an entry name, in either family.
func (p LocalPool) HasEntry(name string) bool {
	return len(p.entryRanges(nl.FAMILY_V4, name)) > 0 || len(p.entryRanges(nl.FAMILY_V6, name)) > 0
}

// EntryOf returns the name of the entry whose range holds ip.
func (p LocalPool) EntryOf(ip net.IP) (string, bool) {
	ranges, names := p.v4Ranges, p.v4Names
	if purelbv2.AddrFamily(ip) == nl.FAMILY_V6 {
		ranges, names = p.v6Ranges, p.v6Names
	}
	for i, r := range ranges {
		if i < len(names) && r.Contains(ip) {
			return names[i], true
		}
	}
	return "", false
}

// assignFamilyInEntry allocates the next free address in family from the
// ranges of entry req.Range.
func (p LocalPool) assignFamilyInEntry(ctx context.Context, family int, req AllocRequest) (net.IP, error) {
	ranges := p.entryRanges(family, req.Range)
	if len(ranges) == 0 {
		return nil, fmt.Errorf("%w: %q has no family %d entry %q", ErrPoolEntryNotFound, p.name, family, req.Range)
	}
	var lastErr error
	for _, r := range ranges {
		ip, err := p.assignFromRange(ctx, r, req)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	allocationRejected.WithLabelValues(p.name, "exhausted").Inc()
	return nil, fmt.Errorf("pool %q entry %q exhausted: %w", p.name, req.Range, lastErr)
}
