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
package speaker

import (
	"fmt"
	"net"

	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	v1 "k8s.io/api/core/v1"

	gokitlog "github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

type controller struct {
	myNode     string
	config     *config.Config
	prometheus *prometheus.GaugeVec
	announcer  Announcer
	svcIP      map[string]net.IP // service name -> assigned IP
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

func (c *controller) SetBalancer(l gokitlog.Logger, name string, svc *v1.Service, eps *v1.Endpoints) k8s.SyncState {
	if svc == nil {
		return c.deleteBalancer(l, name, "serviceDeleted")
	}

	l.Log("event", "startUpdate", "msg", "start of service update")
	defer l.Log("event", "endUpdate", "msg", "end of service update")

	if svc.Spec.Type != "LoadBalancer" {
		return c.deleteBalancer(l, name, "notLoadBalancer")
	}

	if c.config == nil {
		l.Log("event", "noConfig", "msg", "not processing, still waiting for config")
		return k8s.SyncStateSuccess
	}

	if len(svc.Status.LoadBalancer.Ingress) != 1 {
		return c.deleteBalancer(l, name, "noIPAllocated")
	}

	lbIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	if lbIP == nil {
		l.Log("op", "setBalancer", "error", fmt.Sprintf("invalid LoadBalancer IP %q", svc.Status.LoadBalancer.Ingress[0].IP), "msg", "invalid IP allocated by controller")
		return c.deleteBalancer(l, name, "invalidIP")
	}

	l = gokitlog.With(l, "ip", lbIP)

	poolName := poolFor(c.config.Pools, lbIP)
	if poolName == "" {
		l.Log("op", "setBalancer", "error", "assigned IP not allowed by config", "msg", "IP allocated by controller not allowed by config")
		return c.deleteBalancer(l, name, "ipNotAllowed")
	}

	l = gokitlog.With(l, "pool", poolName)
	pool := c.config.Pools[poolName]
	if pool == nil {
		l.Log("bug", "true", "msg", "internal error: allocated IP has no matching address pool")
		return c.deleteBalancer(l, name, "internalError")
	}

	if svcIP, ok := c.svcIP[name]; ok && !lbIP.Equal(svcIP) {
		if st := c.deleteBalancer(l, name, "loadBalancerIPChanged"); st == k8s.SyncStateError {
			return st
		}
	}

	if deleteReason := c.announcer.ShouldAnnounce(l, name, svc, eps); deleteReason != "" {
		return c.deleteBalancer(l, name, deleteReason)
	}

	if err := c.announcer.SetBalancer(l, name, lbIP, pool); err != nil {
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

func (c *controller) deleteBalancer(l gokitlog.Logger, name, reason string) k8s.SyncState {
	if err := c.announcer.DeleteBalancer(l, name, reason); err != nil {
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

func poolFor(pools map[string]*config.Pool, ip net.IP) string {
	for pname, p := range pools {
		for _, cidr := range p.CIDR {
			if cidr.Contains(ip) {
				return pname
			}
		}
	}
	return ""
}

func (c *controller) SetConfig(l gokitlog.Logger, cfg *config.Config) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of config update")
	defer l.Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		l.Log("op", "setConfig", "error", "no configuration in cluster", "msg", "configuration is missing, can not function")
		return k8s.SyncStateError
	}

	for svc, ip := range c.svcIP {
		if pool := poolFor(cfg.Pools, ip); pool == "" {
			l.Log("op", "setConfig", "service", svc, "ip", ip, "error", "service has no configuration under new config", "msg", "new configuration rejected")
			return k8s.SyncStateError
		}
	}

	if err := c.announcer.SetConfig(l, cfg); err != nil {
		l.Log("op", "setConfig", "error", err, "msg", "applying new configuration to announcer failed")
		return k8s.SyncStateError
	}

	c.config = cfg

	return k8s.SyncStateReprocessAll
}

func (c *controller) SetNode(l gokitlog.Logger, node *v1.Node) k8s.SyncState {
	if err := c.announcer.SetNode(l, node); err != nil {
		l.Log("op", "setNode", "error", err, "msg", "failed to propagate node info to announcer")
		return k8s.SyncStateError
	}
	return k8s.SyncStateSuccess
}
