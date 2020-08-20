// Copyright 2020 Acnodal Inc.  All rights reserved.

package acnodal

import (
	"net"

	"purelb.io/internal/config"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/go-kit/kit/log"
)

type announcer struct {
	logger     log.Logger
	myNode     string
	nodeLabels labels.Set
}

func NewAnnouncer(l log.Logger, node string) *announcer {
	return &announcer{logger: l, myNode: node}
}

func (c *announcer) SetConfig(cfg *config.Config) error {
	c.logger.Log("event", "newConfig")

	return nil
}

func (c *announcer) ShouldAnnounce(name string, svc *v1.Service, eps *v1.Endpoints) string {
	// Should we advertise?
	// Yes, if externalTrafficPolicy is
	//  Cluster && any healthy endpoint exists
	// or
	//  Local && there's a ready local endpoint.
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && !nodeHasHealthyEndpoint(eps, c.myNode) {
		c.logger.Log("op", "SetBalancer", "msg", "no local endpoints")
		return "noLocalEndpoints"
	} else if !healthyEndpointExists(eps) {
		c.logger.Log("op", "SetBalancer", "msg", "no endpoints at all")
		return "noEndpoints"
	}

	c.logger.Log("op", "SetBalancer", "msg", "endpoints to announce")

	// We want to announce the endpoints, but the code assumes that this
	// method returns "" at which point the main loop will call
	// SetBalancer() which does the service announcement.  This won't
	// work for us because we want to announce *all* of the endpoints,
	// and the main loop doesn't provide them to SetBalancer() so we'll
	// announce here since we've got everything that we need.

	egw, err := config.New("", "")
	if err != nil {
		c.logger.Log("op", "SetBalancer", "error", err, "msg", "Connection init to EGW failed")
		return "cantConnectToEGW"
	}

	createUrl := svc.Annotations["acnodal.io/endpointcreateURL"]

	// For each endpoint address in each subset on this node, contact the EGW
	for _, ep := range eps.Subsets {
		port := ep.Ports[0].Port
		for _, address := range ep.Addresses {
			if address.NodeName == nil || *address.NodeName != c.myNode {
				c.logger.Log("op", "DontAnnounceEndpoint", "address", address.IP, "port", port, "node", "not me")
			} else {
				c.logger.Log("op", "AnnounceEndpoint", "address", address.IP, "port", port, "node", c.myNode)
				err := egw.AnnounceEndpoint(createUrl, address.IP, int(port))
				if err != nil {
					c.logger.Log("op", "AnnounceEndpoint", "error", err)
				}
			}
		}
	}

	return ""
}

func (c *announcer) SetBalancer(name string, lbIP net.IP, _ string) error {
	// This method is a no-op since we announced the endpoints in ShouldAnnounce()
	return nil
}

func (c *announcer) DeleteBalancer(name, reason string) error {
	c.logger.Log("event", "updatedNodes", "msg", "Delete balancer", "service", name, "reason", reason)

	return nil
}

func (c *announcer) SetNode(node *v1.Node) error {
	c.logger.Log("event", "updatedNodes", "msg", "Node announced", "name", node.Name)

	return nil
}

// nodeHasHealthyEndpoint return true if this node has at least one healthy endpoint.
func nodeHasHealthyEndpoint(eps *v1.Endpoints, node string) bool {
	ready := map[string]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if ep.NodeName == nil || *ep.NodeName != node {
				continue
			}
			if _, ok := ready[ep.IP]; !ok {
				// Only set true if nothing else has expressed an
				// opinion. This means that false will take precedence
				// if there's any unready ports for a given endpoint.
				ready[ep.IP] = true
			}
		}
		for _, ep := range subset.NotReadyAddresses {
			ready[ep.IP] = false
		}
	}

	for _, r := range ready {
		if r {
			// At least one fully healthy endpoint on this machine.
			return true
		}
	}
	return false
}

func healthyEndpointExists(eps *v1.Endpoints) bool {
	ready := map[string]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if _, ok := ready[ep.IP]; !ok {
				// Only set true if nothing else has expressed an
				// opinion. This means that false will take precedence
				// if there's any unready ports for a given endpoint.
				ready[ep.IP] = true
			}
		}
		for _, ep := range subset.NotReadyAddresses {
			ready[ep.IP] = false
		}
	}

	for _, r := range ready {
		if r {
			// At least one fully healthy endpoint on this machine.
			return true
		}
	}
	return false
}
