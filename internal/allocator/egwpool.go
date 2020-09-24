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

	"purelb.io/internal/netbox"
)

// Pool is the configuration of an IP address pool.
type EGWPool struct {
	url        string
	user_token string
	netbox     netbox.Netbox

	// Map of the addresses that have been assigned.
	addressesInUse map[string]map[string]bool // ip.String() -> svc name -> true

	// Map of the "sharing keys" for each IP address
	sharingKeys map[string]*Key // ip.String() -> pointer to sharing key

	portsInUse map[string]map[Port]string // ip.String() -> Port -> svc

	subnetV4    string
	aggregation string
}

func NewEGWPool(rawurl string, aggregation string) (*EGWPool, error) {
	// Make sure that we've got credentials for Netbox
	user_token, is_set := os.LookupEnv("NETBOX_USER_TOKEN")
	if !is_set {
		return nil, fmt.Errorf("NETBOX_USER_TOKEN not set, can't connect to Netbox")
	}

	// Use the hostname from the service group, but reset the path.  EGW
	// and Netbox each have their own API URL schemes so we only need
	// the protocol, host, port, credentials, etc.
	url, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("Netbox URL invalid")
	}
	url.Path = ""

	return &EGWPool{
		url:            rawurl,
		user_token:     user_token,
		netbox:         *netbox.New(url.String(), user_token),
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
func (p EGWPool) Available(ip net.IP, ports []Port, service string, key *Key) error {
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
func (p EGWPool) AssignNext(service string, ports []Port, sharingKey *Key) (net.IP, error) {
	// fetch from netbox
	cidr, err := p.netbox.Fetch()
	if err != nil {
		return nil, fmt.Errorf("no available IPs in pool")
	}
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("error parsing IP %q", ip)
	}

	return ip, nil
}

// Assign assigns a service to an IP.
func (p EGWPool) Assign(ip net.IP, ports []Port, service string, sharingKey *Key) error {
	return nil
}

// Release releases an IP so it can be assigned again.
func (p EGWPool) Release(ip net.IP, service string) {
}

// InUse returns the count of addresses that currently have services
// assigned.
func (p EGWPool) InUse() int {
	return -1
}

// servicesOnIP returns the names of the services who are assigned to
// the address.
func (p EGWPool) servicesOnIP(ip net.IP) []string {
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
func (p EGWPool) SharingKey(ip net.IP) *Key {
	return p.sharingKeys[ip.String()]
}

// First returns the first (i.e., lowest-valued) net.IP within this
// Pool, or nil if the pool has no addresses.
func (p EGWPool) First() net.IP {
	return nil
}

// Next returns the next net.IP within this Pool, or nil if the
// provided net.IP is the last address in the range.
func (p EGWPool) Next(ip net.IP) net.IP {
	return nil
}

// Size returns the total number of addresses in this pool if it's a
// local pool, or 0 if it's a remote pool.
func (p EGWPool) Size() uint64 {
	return uint64(0)
}

// Overlaps indicates whether the other Pool overlaps with this one
// (i.e., has any addresses in common).  It returns true if there are
// any common addresses and false if there aren't.
func (p EGWPool) Overlaps(other Pool) bool {
	return false
}

// Contains indicates whether the provided net.IP represents an
// address within this Pool.  It returns true if so, false otherwise.
func (p EGWPool) Contains(ip net.IP) bool {
	return false
}
