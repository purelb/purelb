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
	"strconv"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/acnodal"
	purelbv1 "purelb.io/pkg/apis/v1"
)

// EGWPool represents an IP address pool on the Acnodal Enterprise
// GateWay.
type EGWPool struct {
	egw              acnodal.EGW
	createServiceURL string
	serviceURLCache  map[string]string // map from service key to service url
}

// NewEGWPool initializes a new instance of EGWPool. If error is
// non-nil then the returned EGWPool should not be used.
func NewEGWPool(egw acnodal.EGW, _ string) (*EGWPool, error) {
	return &EGWPool{
		egw:             egw,
		serviceURLCache: map[string]string{},
	}, nil
}

// Available determines whether an address is available. The decision
// depends on whether another service is using the address, and if so,
// whether this service can share the address with it. error will be
// nil if the ip is available, and will contain an explanation if not.
func (p EGWPool) Available(ip net.IP, service *v1.Service) error {
	// We haven't yet implemented address sharing
	return nil
}

// AssignNext assigns a service to the next available IP.
func (p EGWPool) AssignNext(service *v1.Service) (net.IP, error) {
	// Lazily look up the EGW group (which gives us the URL to create
	// services)
	if p.createServiceURL == "" {
		group, err := p.egw.GetGroup()
		if err != nil {
			return nil, err
		}
		p.createServiceURL = group.Links["create-service"]
	}

	// Announce the service to the EGW
	egwsvc, err := p.egw.AnnounceService(p.createServiceURL, service.Name, service.Spec.Ports)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(egwsvc.Service.Spec.Address)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP returned by EGW: %s", egwsvc.Service.Spec.Address)
	}

	service.Annotations[purelbv1.GroupAnnotation] = egwsvc.Links["group"]
	service.Annotations[purelbv1.ServiceAnnotation] = egwsvc.Links["self"]
	service.Annotations[purelbv1.ServiceGUEKeyAnnotation] = strconv.Itoa(int(egwsvc.Service.Spec.GUEKey))
	service.Annotations[purelbv1.EndpointAnnotation] = egwsvc.Links["create-endpoint"]

	// add the service's URL to the cache so we'll be able to get back
	// to it later if we need to delete the service
	p.serviceURLCache[namespacedName(service)] = egwsvc.Links["self"]

	return ip, nil
}

// Assign assigns a service to an IP.
func (p EGWPool) Assign(ip net.IP, service *v1.Service) error {
	// Grab the service URL to warm up our cache
	url, exists := service.Annotations[purelbv1.ServiceAnnotation]
	if exists {
		p.serviceURLCache[namespacedName(service)] = url
	}

	return nil
}

// Release releases an IP so it can be assigned again.
func (p EGWPool) Release(ip net.IP, service string) {
	p.egw.WithdrawService(p.serviceURLCache[service])
}

// InUse returns the count of addresses that currently have services
// assigned.
func (p EGWPool) InUse() int {
	return -1
}

// SharingKey returns the "sharing key" for the specified address.
func (p EGWPool) SharingKey(ip net.IP) *Key {
	return nil
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
