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

	"github.com/go-kit/log"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/netbox"
	purelbv1 "purelb.io/pkg/apis/purelb/v1"
)

// NetboxPool is the IP address pool that requests IP addresses from a
// Netbox IPAM system.
type NetboxPool struct {
	name string

	logger log.Logger

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
func NewNetboxPool(name string, log log.Logger, spec purelbv1.ServiceGroupNetboxSpec) (*NetboxPool, error) {
	// Make sure that we've got credentials for Netbox
	userToken, ok := os.LookupEnv("NETBOX_USER_TOKEN")
	if !ok {
		return nil, fmt.Errorf("NETBOX_USER_TOKEN not set, can't connect to Netbox")
	}

	// Validate the url from the ServiceGroup
	url, err := url.Parse(spec.URL)
	if err != nil {
		return nil, fmt.Errorf("Netbox URL invalid")
	}

	return &NetboxPool{
		name: name,
		logger:         log,
		url:            url.String(),
		userToken:      userToken,
		netbox:         netbox.NewNetbox(url.String(), spec.Tenant, userToken),
		services:       map[string][]net.IP{},
		addressesInUse: map[string]map[string]bool{},
	}, nil
}

func (p NetboxPool) Notify(service *v1.Service) error {
	nsName := namespacedName(service)

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ipstr := ingress.IP
		ip := net.ParseIP(ipstr)
		if ip == nil {
			return fmt.Errorf("Service %s has unparseable IP %s", namespacedName(service), ipstr)
		}

		if p.addressesInUse[ipstr] == nil {
			p.addressesInUse[ipstr] = map[string]bool{}
		}
		p.addressesInUse[ipstr][nsName] = true
		p.services[nsName] = append(p.services[nsName], ip)
	}

	return nil
}

// AssignNext assigns a service to the next available IP.
func (p NetboxPool) AssignNext(service *v1.Service) error {
	// fetch from netbox
	cidr, err := p.netbox.Fetch()
	if err != nil {
		return fmt.Errorf("no available IPs in pool %q", err)
	}
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("error parsing CIDR %s", cidr)
	}
	if err := p.Assign(ip, service); err != nil {
		return err
	}

	return nil
}

// Assign assigns a service to an IP.
func (p NetboxPool) Assign(ip net.IP, service *v1.Service) error {
	// we have an IP selected somehow, so program the data plane
	addIngress(p.logger, service, ip)

	// Update our internal allocation data structures
	return p.Notify(service)
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

// ReleaseIP releases a specific IP address for a service. Used during
// IP family transitions (e.g., DualStack → SingleStack).
func (p NetboxPool) ReleaseIP(service string, ip net.IP) error {
	ipstr := ip.String()

	// Check if this IP is tracked in our pool
	_, exists := p.addressesInUse[ipstr]
	if !exists {
		return nil // IP not in this pool, nothing to do
	}

	// Remove service from this IP's allocations
	delete(p.addressesInUse[ipstr], service)
	if len(p.addressesInUse[ipstr]) == 0 {
		delete(p.addressesInUse, ipstr)
	}

	// Remove from services map - filter out this specific IP
	if ips, exists := p.services[service]; exists {
		newIPs := make([]net.IP, 0, len(ips))
		for _, existingIP := range ips {
			if !existingIP.Equal(ip) {
				newIPs = append(newIPs, existingIP)
			}
		}
		if len(newIPs) > 0 {
			p.services[service] = newIPs
		} else {
			delete(p.services, service)
		}
	}

	p.logger.Log("netboxpool", "release-ip", "service", service, "ip", ipstr)
	return nil
}

// InUse returns the count of addresses that currently have services
// assigned.
func (p NetboxPool) InUse() int {
	return len(p.addressesInUse)
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

func (p NetboxPool) String() string {
	return p.name
}
