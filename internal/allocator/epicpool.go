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

	"github.com/go-kit/kit/log"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/acnodal"
	purelbv1 "purelb.io/pkg/apis/v1"
)

// EPICPool represents an IP address pool on the Acnodal Enterprise
// GateWay.
type EPICPool struct {
	log              log.Logger
	epic             acnodal.EPIC
	createServiceURL string
	clusterURLCache  map[string]string // map from service key to cluster url
}

// NewEPICPool initializes a new instance of EPICPool. If error is
// non-nil then the returned EPICPool should not be used.
func NewEPICPool(log log.Logger, epic acnodal.EPIC) (*EPICPool, error) {
	return &EPICPool{
		log:             log,
		epic:            epic,
		clusterURLCache: map[string]string{},
	}, nil
}

// Available determines whether an address is available. The decision
// depends on whether another service is using the address, and if so,
// whether this service can share the address with it. error will be
// nil if the ip is available, and will contain an explanation if not.
func (p EPICPool) Available(_ net.IP, _ *v1.Service) error {
	// We haven't yet implemented address sharing
	return nil
}

// AssignNext assigns a service to the next available IP.
func (p EPICPool) AssignNext(service *v1.Service) (net.IP, error) {
	nsName := service.Namespace + "/" + service.Name

	// Lazily look up the EPIC group (which gives us the URL to create
	// services)
	if p.createServiceURL == "" {
		group, err := p.epic.GetGroup()
		if err != nil {
			return nil, err
		}
		p.createServiceURL = group.Links["create-service"]
	}

	// Announce the service to the EPIC
	epicsvc, err := p.epic.AnnounceService(p.createServiceURL, service.Name, service.Spec.Ports)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(epicsvc.Service.Spec.Address)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP returned by EPIC: %s", epicsvc.Service.Spec.Address)
	}

	service.Annotations[purelbv1.GroupAnnotation] = epicsvc.Links["group"]
	service.Annotations[purelbv1.ServiceAnnotation] = epicsvc.Links["self"]
	service.Annotations[purelbv1.EndpointAnnotation] = epicsvc.Links["create-endpoint"]
	if len(epicsvc.Service.Spec.Endpoints) > 0 {
		service.Annotations[purelbv1.HostnameAnnotation] = epicsvc.Service.Spec.Endpoints[0].DNSName
	}

	epicCluster, err := p.epic.AddCluster(epicsvc.Links["create-cluster"], nsName)
	if err != nil {
		return nil, err
	}
	service.Annotations[purelbv1.ClusterAnnotation] = epicCluster.Links["self"]

	// add the cluster's URL to the cache so we'll be able to get back
	// to it later if we need to delete the service
	p.clusterURLCache[namespacedName(service)] = epicCluster.Links["self"]

	return ip, nil
}

// Assign assigns a service to an IP.
func (p EPICPool) Assign(_ net.IP, service *v1.Service) error {
	// Grab the service URL to warm up our cache
	url, exists := service.Annotations[purelbv1.ClusterAnnotation]
	if exists {
		p.clusterURLCache[namespacedName(service)] = url
	}

	return nil
}

// Release releases an IP so it can be assigned again. "service"
// should be a namespaced name, i.e., the output of
// namespacedName(service)).
func (p EPICPool) Release(_ net.IP, service string) error {

	// Attempt to remove our cluster from the service, but don't fail if
	// something goes wrong
	cluster, err := p.epic.FetchCluster(p.clusterURLCache[service])
	if err != nil {
		p.log.Log("op", "FetchCluster", "result", err.Error())
		return nil
	}

	// Remove ourselves (i.e., our cluster) from the service
	if err := p.epic.Delete(cluster.Links["self"]); err != nil {
		p.log.Log("op", "DeleteCluster", "result", err.Error())
	}

	// Now try to delete the cluster, which will fail if any other
	// clusters are still attached to it, but we don't need to error
	// when that happens
	if err := p.epic.Delete(cluster.Links["service"]); err != nil {
		p.log.Log("op", "DeleteService", "result", err.Error())
	}

	return nil
}

// InUse returns the count of addresses that currently have services
// assigned.
func (p EPICPool) InUse() int {
	return -1
}

// SharingKey returns the "sharing key" for the specified address.
func (p EPICPool) SharingKey(_ net.IP) *Key {
	return nil
}

// First returns the first (i.e., lowest-valued) net.IP within this
// Pool, or nil if the pool has no addresses.
func (p EPICPool) First() net.IP {
	return nil
}

// Next returns the next net.IP within this Pool, or nil if the
// provided net.IP is the last address in the range.
func (p EPICPool) Next(_ net.IP) net.IP {
	return nil
}

// Size returns the total number of addresses in this pool if it's a
// local pool, or 0 if it's a remote pool.
func (p EPICPool) Size() uint64 {
	return uint64(0)
}

// Overlaps indicates whether the other Pool overlaps with this one
// (i.e., has any addresses in common).  It returns true if there are
// any common addresses and false if there aren't.
func (p EPICPool) Overlaps(_ Pool) bool {
	return false
}

// Contains indicates whether the provided net.IP represents an
// address within this Pool.  It returns true if so, false otherwise.
func (p EPICPool) Contains(_ net.IP) bool {
	return false
}
