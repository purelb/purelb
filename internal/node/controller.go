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
package node

import (
	"net"

	"purelb.io/internal/config"
	"purelb.io/internal/election"
	"purelb.io/internal/k8s"

	"k8s.io/api/core/v1"
	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

type controller struct {
	myNode     string
	prometheus *prometheus.GaugeVec
	announcer  Announcer
	svcIP      map[string]net.IP // service name -> assigned IP
	Election   *election.Election
}

func NewController(myNode string, prometheus *prometheus.GaugeVec, announcer Announcer) (*controller, error) {
	con := &controller{
		myNode:     myNode,
		prometheus: prometheus,
		announcer:  announcer,
		svcIP:      map[string]net.IP{},
	}

	return con, nil
}

func (c *controller) ServiceChanged(l log.Logger, name string, svc *v1.Service, eps *v1.Endpoints) k8s.SyncState {
	if svc == nil {
		return c.deleteBalancer(l, name, "serviceDeleted")
	}

	l.Log("event", "startUpdate", "msg", "start of service update", "service", name)
	defer l.Log("event", "endUpdate", "msg", "end of service update", "service", name)

	if svc.Spec.Type != "LoadBalancer" {
		return c.deleteBalancer(l, name, "notLoadBalancer")
	}

	if len(svc.Status.LoadBalancer.Ingress) != 1 {
		return c.deleteBalancer(l, name, "noIPAllocated")
	}

	lbIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	if lbIP == nil {
		l.Log("op", "setBalancer", "error", "invalid LoadBalancer IP", svc.Status.LoadBalancer.Ingress[0].IP, "msg", "invalid IP allocated by controller")
		return c.deleteBalancer(l, name, "invalidIP")
	}

	l = log.With(l, "ip", lbIP)

	if svcIP, ok := c.svcIP[name]; ok && !lbIP.Equal(svcIP) {
		if st := c.deleteBalancer(l, name, "loadBalancerIPChanged"); st == k8s.SyncStateError {
			return st
		}
	}

	if deleteReason := c.ShouldAnnounce(l, name, svc, eps); deleteReason != "" {
		return c.deleteBalancer(l, name, deleteReason)
	}

	if err := c.announcer.SetBalancer(name, lbIP); err != nil {
		l.Log("op", "setBalancer", "error", err, "msg", "failed to announce service")
		return k8s.SyncStateError
	}

	c.prometheus.With(prometheus.Labels{
		"service": name,
		"node":    c.myNode,
		"ip":      lbIP.String(),
	}).Set(1)
	l.Log("event", "serviceAnnounced", "node", c.myNode, "msg", "service has IP, announcing")

	return k8s.SyncStateSuccess
}

func (c *controller) ShouldAnnounce(l log.Logger, name string, svc *v1.Service, eps *v1.Endpoints) string {
	winner := c.Election.Winner(eps, name)
	if winner == c.myNode {
		l.Log("msg", "I'm the winner", "node", c.myNode, "service", name)
		return ""
	}

	l.Log("msg", "Not the winner", "node", c.myNode, "service", name, "winner", winner)
	return "notWinner"
}

func (c *controller) deleteBalancer(l log.Logger, name, reason string) k8s.SyncState {
	if err := c.announcer.DeleteBalancer(name, reason); err != nil {
		l.Log("op", "deleteBalancer", "error", err, "msg", "failed to clear balancer state")
		return k8s.SyncStateError
	}

	c.prometheus.Delete(prometheus.Labels{
		"service": name,
		"node":    c.myNode,
		"ip":      c.svcIP[name].String(),
	})
	delete(c.svcIP, name)

	l.Log("event", "serviceWithdrawn", "ip", c.svcIP[name], "reason", reason, "msg", "withdrawing service announcement")

	return k8s.SyncStateSuccess
}

func (c *controller) SetConfig(l log.Logger, cfg *config.Config) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of config update")
	defer l.Log("event", "endUpdate", "msg", "end of config update")

	if err := c.announcer.SetConfig(cfg); err != nil {
		l.Log("op", "setConfig", "error", err, "msg", "applying new configuration to announcer failed")
		return k8s.SyncStateError
	}

	return k8s.SyncStateReprocessAll
}

func (c *controller) SetNode(l log.Logger, node *v1.Node) k8s.SyncState {
	if err := c.announcer.SetNode(node); err != nil {
		l.Log("op", "setNode", "error", err, "msg", "failed to propagate node info to announcer")
		return k8s.SyncStateError
	}
	return k8s.SyncStateSuccess
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
