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

package config

import (
	"fmt"
	"net"
	"strings"
)

// Pool is the configuration of an IP address pool.
type LocalPool struct {
	// The addresses that are part of this pool. config.Parse guarantees
	// that these are non-overlapping, both within and between pools.
	addresses *IPRange

	// Map of the addresses that have been assigned.
	addressesInUse map[string]map[string]bool // ip.String() -> svc name -> true

	// Map of the "sharing keys" for each IP address
	sharingKeys map[string]*Key // ip.String() -> pointer to sharing key

	portsInUse map[string]map[Port]string // ip.String() -> Port -> svc

	// If false, prevents IP addresses from being automatically assigned
	// from this pool.
	autoAssign bool

	subnetV4    string
	aggregation string
}

func NewLocalPool(autoassign bool, rawrange string, subnet string, aggregation string) (*LocalPool, error) {
	iprange, err := NewIPRange(rawrange)
	if err != nil {
		return nil, err
	}
	return &LocalPool{
		autoAssign:     autoassign,
		addresses:      &iprange,
		addressesInUse: map[string]map[string]bool{},
		sharingKeys:    map[string]*Key{},
		portsInUse:     map[string]map[Port]string{},
		subnetV4: subnet,
		aggregation: aggregation,
	}, nil
}

// Available determines whether an address is available. The decision
// depends on whether another service is using the address, and if so,
// whether this service can share the address with it. error will be
// nil if the ip is available, and will contain an explanation if not.
func (p LocalPool) Available(ip net.IP, ports []Port, service string, key *Key) error {
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
				if otherSvc != service {
					otherSvcs = append(otherSvcs, otherSvc)
				}
			}
			if len(otherSvcs) > 0 {
				return fmt.Errorf("can't change sharing key for %q, address also in use by %s", service, strings.Join(otherSvcs, ","))
			}
		}

		for _, port := range ports {
			if curSvc, ok := p.portsInUse[ip.String()][port]; ok && curSvc != service {
				return fmt.Errorf("port %s is already in use on %q", port, ip)
			}
		}
	}

	return nil
}

// AssignNext assigns a service to the next available IP.
func (p LocalPool) AssignNext(service string, ports []Port, sharingKey *Key) (net.IP, error) {
	for pos := p.First(); pos != nil; pos = p.Next(pos) {
		if err := p.Assign(pos, ports, service, sharingKey); err == nil {
			// we found an available address
			return pos, err
		}
	}

	return nil, fmt.Errorf("no available addresses in pool")
}

// Assign assigns a service to an IP.
func (p LocalPool) Assign(ip net.IP, ports []Port, service string, sharingKey *Key) error {
	ipstr := ip.String()

	if err := p.Available(ip, ports, service, sharingKey); err != nil {
		return err
	}

	p.sharingKeys[ipstr] = sharingKey
	if p.addressesInUse[ipstr] == nil {
		p.addressesInUse[ipstr] = map[string]bool{}
	}
	p.addressesInUse[ipstr][service] = true
	if p.portsInUse[ipstr] == nil {
		p.portsInUse[ipstr] = map[Port]string{}
	}
	for _, port := range ports {
		p.portsInUse[ipstr][port] = service
	}

	return nil
}

// Release releases an IP so it can be assigned again.
func (p LocalPool) Release(ip net.IP, service string) {
	ipstr := ip.String()
	delete(p.sharingKeys, ip.String())
	delete(p.addressesInUse[ipstr], service)
	if len(p.addressesInUse[ipstr]) == 0 {
		delete(p.addressesInUse, ipstr)
	}
	for port, svc := range p.portsInUse[ipstr] {
		if svc != service {
			delete(p.portsInUse[ipstr], port)
		}
	}
	if len(p.portsInUse[ipstr]) == 0 {
		delete(p.portsInUse, ipstr)
	}
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

// First returns the first (i.e., lowest-valued) net.IP within this
// Pool, or nil if the pool has no addresses.
func (p LocalPool) First() net.IP {
	if p.addresses != nil {
		return p.addresses.First()
	}
	return nil
}

// Next returns the next net.IP within this Pool, or nil if the
// provided net.IP is the last address in the range.
func (p LocalPool) Next(ip net.IP) net.IP {
	if p.addresses != nil {
		return p.addresses.Next(ip)
	}
	return nil
}

// IsIPV6 returns true if this Pool is a range of IPV6 addresses.
func (p LocalPool) IsIPV6() bool {
	if p.addresses != nil {
		return p.addresses.IsIPV6()
	}
	return false
}

// Size returns the total number of addresses in this pool if it's a
// local pool, or 0 if it's a remote pool.
func (p LocalPool) Size() uint64 {
	if p.addresses != nil {
		return p.addresses.Size()
	}
	return uint64(0)
}

// Overlaps indicates whether the other Pool overlaps with this one
// (i.e., has any addresses in common).  It returns true if there are
// any common addresses and false if there aren't.
func (p LocalPool) Overlaps(other Pool) bool {
	lpool := other.(LocalPool)
	if p.addresses == nil || lpool.addresses == nil {
		return false
	}
	return p.addresses.Overlaps(*lpool.addresses)
}

// Contains indicates whether the provided net.IP represents an
// address within this Pool.  It returns true if so, false otherwise.
func (p LocalPool) Contains(ip net.IP) bool {
	if p.addresses != nil {
		return p.addresses.Contains(ip)
	}
	return false
}

// AutoAssign indicates whether this pool is available for autoassign.
func (p LocalPool) AutoAssign() bool {
  return p.autoAssign
}
