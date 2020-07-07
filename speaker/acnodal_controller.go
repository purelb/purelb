// Copyright 2020 Acnodal Inc.  All rights reserved.

package main

import (
	"net"

	"go.universe.tf/metallb/internal/acnodal"
	"go.universe.tf/metallb/internal/config"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/go-kit/kit/log"
)

type acnodalController struct {
	logger     log.Logger
	myNode     string
	nodeLabels labels.Set
}

func (c *acnodalController) SetConfig(l log.Logger, cfg *config.Config) error {
	l.Log("event", "newConfig")

	return nil
}

func (c *acnodalController) ShouldAnnounce(l log.Logger, name string, svc *v1.Service, eps *v1.Endpoints) string {
	// Should we advertise?
	// Yes, if externalTrafficPolicy is
	//  Cluster && any healthy endpoint exists
	// or
	//  Local && there's a ready local endpoint.
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && !nodeHasHealthyEndpoint(eps, c.myNode) {
		l.Log("op", "SetBalancer", "msg", "no local endpoints")
		return "noLocalEndpoints"
	} else if !healthyEndpointExists(eps) {
		l.Log("op", "SetBalancer", "msg", "no endpoints at all")
		return "noEndpoints"
	}

	l.Log("op", "SetBalancer", "msg", "endpoints to announce")

	// We want to announce the endpoints, but the code assumes that this
	// method returns "" at which point the main loop will call
	// SetBalancer() which does the service announcement.  This won't
	// work for us because we want to announce *all* of the endpoints,
	// and the main loop doesn't provide them to SetBalancer() so we'll
	// announce here since we've got everything that we need.

	egw, err := acnodal.New("", "")
	if err != nil {
		l.Log("op", "SetBalancer", "error", err, "msg", "Connection init to EGW failed")
		return "cantConnectToEGW"
	}

	groupId := svc.Annotations["acnodal.io/groupId"]
	svcId := svc.Annotations["acnodal.io/serviceId"]
	endpointUrl := svc.Annotations["acnodal.io/endpointURL"]

	// For each endpoint address in each subset, contact the EGW
	for _, ep := range eps.Subsets {
		for _, address := range ep.Addresses {
			l.Log("op", "AnnounceEndpoint", "address", address.IP)
			_, err := egw.AnnounceEndpoint(endpointUrl, groupId, name, svcId, svc.Status.LoadBalancer.Ingress[0].IP, address.IP)
			if err != nil {
				l.Log("op", "AnnounceEndpoint", "error", err)
			}
		}
	}

	return ""
}

func (c *acnodalController) SetBalancer(l log.Logger, name string, lbIP net.IP, pool *config.Pool) error {
	// This method is a no-op since we announced the endpoints in ShouldAnnounce()
	return nil
}

func (c *acnodalController) DeleteBalancer(l log.Logger, name, reason string) error {
	l.Log("event", "updatedNodes", "msg", "Delete balancer", "service", name, "reason", reason)

	return nil
}

func (c *acnodalController) SetNode(l log.Logger, node *v1.Node) error {
	l.Log("event", "updatedNodes", "msg", "Node announced", "name", node.Name)

	return nil
}
