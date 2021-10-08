// Copyright 2017 Google Inc.
// Copyright 2020,2021 Acnodal Inc.
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
	"net/url"
	"os"
	"strings"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/netbox"
	purelbv1 "purelb.io/pkg/apis/v1"
)

// NetboxPool is the IP address pool that requests IP addresses from a
// Netbox IPAM system.
type NetboxPool struct {
	url       string
	userToken string
	netbox    netbox.Netbox

	// services caches the addresses that we've allocated to a specific
	// service. It's used so we can release addresses when we're given
	// only the service name. The key is the service's namespaced name,
	// and the value is an array of the addresses assigned to that
	// service.
	services map[string][]net.IP

	// Map of the addresses that have been assigned.
	addressesInUse map[string]map[string]bool // ip.String() -> svc name -> true
}

// NewNetboxPool initializes a new instance of NetboxPool. If error is
// non-nil then the returned NetboxPool should not be used.
func NewNetboxPool(spec purelbv1.ServiceGroupNetboxSpec) (*NetboxPool, error) {
	// Make sure that we've got credentials for Netbox
	userToken, ok := os.LookupEnv("NETBOX_USER_TOKEN")
	if !ok {
		return nil, fmt.Errorf("NETBOX_USER_TOKEN not set, can't connect to Netbox")
	}

	// Validate the url from the service group
	url, err := url.Parse(spec.URL)
	if err != nil {
		return nil, fmt.Errorf("Netbox URL invalid")
	}

	return &NetboxPool{
		url:            url.String(),
		userToken:      userToken,
		netbox:         netbox.NewNetbox(url.String(), spec.Tenant, userToken),
		services:       map[string][]net.IP{},
		addressesInUse: map[string]map[string]bool{},
	}, nil
}

// Available determines whether an address is available. The decision
// depends on whether another service is using the address, and if so,
// whether this service can share the address with it. error will be
// nil if the ip is available, and will contain an explanation if not.
func (p NetboxPool) Available(ip net.IP, service *v1.Service) error {
	key := &Key{Sharing: SharingKey(service)}

	// No key: no sharing
	if key == nil {
		key = &Key{}
	}

	// Does the IP already have allocs? If so, needs to be the same
	// sharing key, and have non-overlapping ports. If not, the
	// proposed IP needs to be allowed by configuration.
	if existingSK := p.sharingKey(ip); existingSK != nil {
		if err := sharingOK(existingSK, key); err != nil {

			// Sharing key is incompatible. However, if the owner is
			// the same service, and is the only user of the IP, we
			// can just update its sharing key in place.
			var otherSvcs []string
			for _, otherSvc := range p.servicesOnIP(ip) {
				if otherSvc != service.Name {
					otherSvcs = append(otherSvcs, otherSvc)
				}
			}
			if len(otherSvcs) > 0 {
				return fmt.Errorf("can't change sharing key for %q, address also in use by %s", service, strings.Join(otherSvcs, ","))
			}
		}
	}

	return nil
}

// AssignNext assigns a service to the next available IP.
func (p NetboxPool) AssignNext(service *v1.Service) (net.IP, error) {
	// fetch from netbox
	cidr, err := p.netbox.Fetch()
	if err != nil {
		return nil, fmt.Errorf("no available IPs in pool %q", err)
	}
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("error parsing CIDR %s", cidr)
	}
	if err := p.Assign(ip, service); err != nil {
		return nil, err
	}

	return ip, nil
}

// Assign assigns a service to an IP.
func (p NetboxPool) Assign(ip net.IP, service *v1.Service) error {
	nsName := namespacedName(service)
	ipstr := ip.String()

	if p.addressesInUse[ipstr] == nil {
		p.addressesInUse[ipstr] = map[string]bool{}
	}
	p.addressesInUse[ipstr][nsName] = true
	p.services[nsName] = append(p.services[nsName], ip)

	return nil
}

// Release releases an IP so it can be assigned again.
func (p NetboxPool) Release(service string) error {
	ip, haveIp := p.services[service]
	if !haveIp {
		return fmt.Errorf("trying to release an IP from unknown service %s", service)
	}
	delete(p.services, service)
	ipstr := ip[0].String()
	delete(p.addressesInUse[ipstr], service)
	if len(p.addressesInUse[ipstr]) == 0 {
		delete(p.addressesInUse, ipstr)
	}
	return nil
}

// InUse returns the count of addresses that currently have services
// assigned.
func (p NetboxPool) InUse() int {
	return -1
}

// servicesOnIP returns the names of the services who are assigned to
// the address.
func (p NetboxPool) servicesOnIP(ip net.IP) []string {
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

// sharingKey returns the "sharing key" for the specified address.
func (p NetboxPool) sharingKey(ip net.IP) *Key {
	return nil
}

// Size returns the total number of addresses in this pool if it's a
// local pool, or 0 if it's a remote pool.
func (p NetboxPool) Size() uint64 {
	return uint64(0)
}

// Overlaps indicates whether the other Pool overlaps with this one
// (i.e., has any addresses in common).  It returns true if there are
// any common addresses and false if there aren't. This implementation
// always returns false since the pool is managed by a remote system.
func (p NetboxPool) Overlaps(other Pool) bool {
	return false
}

// Contains indicates whether the provided net.IP represents an
// address within this Pool.  It returns true if so, false
// otherwise. In this case the pool is owned by a remote system so
// "address within this Pool" means that the address has been
// allocated by a previous call to AssignNext().
func (p NetboxPool) Contains(ip net.IP) bool {
	_, allocated := p.addressesInUse[ip.String()]
	return allocated
}
