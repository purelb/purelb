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
	"net/url"
	"os"
	"strings"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/netbox"
)

// NetboxPool is the IP address pool that requests IP addresses from a
// Netbox IPAM system.
type NetboxPool struct {
	url       string
	userToken string
	netbox    netbox.Netbox

	// Map of the addresses that have been assigned.
	addressesInUse map[string]map[string]bool // ip.String() -> svc name -> true

	// Map of the "sharing keys" for each IP address
	sharingKeys map[string]*Key // ip.String() -> pointer to sharing key

	portsInUse map[string]map[Port]string // ip.String() -> Port -> svc

	subnetV4    string
	aggregation string
}

// NewNetboxPool initializes a new instance of NetboxPool. If error is
// non-nil then the returned NetboxPool should not be used.
func NewNetboxPool(rawurl string, tenant string, aggregation string) (*NetboxPool, error) {
	// Make sure that we've got credentials for Netbox
	userToken, ok := os.LookupEnv("NETBOX_USER_TOKEN")
	if !ok {
		return nil, fmt.Errorf("NETBOX_USER_TOKEN not set, can't connect to Netbox")
	}

	// Validate the url from the service group
	url, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("Netbox URL invalid")
	}

	return &NetboxPool{
		url:            rawurl,
		userToken:      userToken,
		netbox:         *netbox.NewNetbox(url.String(), tenant, userToken),
		addressesInUse: map[string]map[string]bool{},
		sharingKeys:    map[string]*Key{},
		portsInUse:     map[string]map[Port]string{},
		aggregation:    aggregation,
	}, nil
}

// Available determines whether an address is available. The decision
// depends on whether another service is using the address, and if so,
// whether this service can share the address with it. error will be
// nil if the ip is available, and will contain an explanation if not.
func (p NetboxPool) Available(ip net.IP, service *v1.Service) error {
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
				if otherSvc != service.Name {
					otherSvcs = append(otherSvcs, otherSvc)
				}
			}
			if len(otherSvcs) > 0 {
				return fmt.Errorf("can't change sharing key for %q, address also in use by %s", service, strings.Join(otherSvcs, ","))
			}
		}

		for _, port := range ports {
			if curSvc, ok := p.portsInUse[ip.String()][port]; ok && curSvc != service.Name {
				return fmt.Errorf("port %s is already in use on %q", port, ip)
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
		return nil, fmt.Errorf("error parsing IP %q", ip)
	}

	return ip, nil
}

// Assign assigns a service to an IP.
func (p NetboxPool) Assign(ip net.IP, service *v1.Service) error {
	return nil
}

// Release releases an IP so it can be assigned again.
func (p NetboxPool) Release(ip net.IP, service string) {
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

// SharingKey returns the "sharing key" for the specified address.
func (p NetboxPool) SharingKey(ip net.IP) *Key {
	return p.sharingKeys[ip.String()]
}

// First returns the first (i.e., lowest-valued) net.IP within this
// Pool, or nil if the pool has no addresses.
func (p NetboxPool) First() net.IP {
	return nil
}

// Next returns the next net.IP within this Pool, or nil if the
// provided net.IP is the last address in the range.
func (p NetboxPool) Next(ip net.IP) net.IP {
	return nil
}

// Size returns the total number of addresses in this pool if it's a
// local pool, or 0 if it's a remote pool.
func (p NetboxPool) Size() uint64 {
	return uint64(0)
}

// Overlaps indicates whether the other Pool overlaps with this one
// (i.e., has any addresses in common).  It returns true if there are
// any common addresses and false if there aren't.
func (p NetboxPool) Overlaps(other Pool) bool {
	return false
}

// Contains indicates whether the provided net.IP represents an
// address within this Pool.  It returns true if so, false otherwise.
func (p NetboxPool) Contains(ip net.IP) bool {
	return false
}
