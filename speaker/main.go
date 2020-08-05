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

package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	"go.universe.tf/metallb/internal/logging"
	"go.universe.tf/metallb/internal/version"
	v1 "k8s.io/api/core/v1"

	gokitlog "github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

var announcing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "metallb",
	Subsystem: "speaker",
	Name:      "announced",
	Help:      "Services being announced from this node. This is desired state, it does not guarantee that the routing protocols have converged.",
}, []string{
	"service",
	"node",
	"ip",
})

// Service offers methods to mutate a Kubernetes service object.
type service interface {
	Update(svc *v1.Service) (*v1.Service, error)
	UpdateStatus(svc *v1.Service) error
	Infof(svc *v1.Service, desc, msg string, args ...interface{})
	Errorf(svc *v1.Service, desc, msg string, args ...interface{})
}

func main() {
	prometheus.MustRegister(announcing)

	logger, err := logging.Init()
	if err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}

	var (
		config      = flag.String("config", "config", "Kubernetes ConfigMap containing configuration")
		configNS    = flag.String("config-ns", "", "config file namespace (only needed when running outside of k8s)")
		kubeconfig  = flag.String("kubeconfig", "", "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host        = flag.String("host", os.Getenv("METALLB_HOST"), "HTTP host address")
		myNode      = flag.String("node-name", os.Getenv("METALLB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		port        = flag.Int("port", 80, "HTTP listening port")
	)
	flag.Parse()

	logger.Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "msg", "Speaker starting "+version.String())

	if *myNode == "" {
		logger.Log("op", "startup", "error", "must specify --node-name or METALLB_NODE_NAME", "msg", "missing configuration")
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	go func() {
		c1 := make(chan os.Signal)
		signal.Notify(c1, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
		<-c1
		logger.Log("op", "shutdown", "msg", "starting shutdown")
		signal.Stop(c1)
		close(stopCh)
	}()
	defer logger.Log("op", "shutdown", "msg", "done")

	// Setup all clients and speakers, config decides what is being done runtime.
	ctrl, err := newController(controllerConfig{
		MyNode: *myNode,
		Logger: logger,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create controller")
		os.Exit(1)
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "metallb-speaker",
		ConfigMapName: *config,
		ConfigMapNS:   *configNS,
		NodeName:      *myNode,
		Logger:        logger,
		Kubeconfig:    *kubeconfig,

		MetricsHost:   *host,
		MetricsPort:   *port,
		ReadEndpoints: true,

		ServiceChanged: ctrl.SetBalancer,
		ConfigChanged:  ctrl.SetConfig,
		NodeChanged:    ctrl.SetNode,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}
	ctrl.client = client

	if err := client.Run(stopCh); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}

type controller struct {
	myNode string

	config *config.Config
	client service

	announcer Announcer
	svcIP   map[string]net.IP // service name -> assigned IP
}

type controllerConfig struct {
	MyNode string
	Logger gokitlog.Logger
}

func newController(cfg controllerConfig) (*controller, error) {
	announcer := acnodalController{
		logger: cfg.Logger,
		myNode: cfg.MyNode,
	}

	ret := &controller{
		myNode:  cfg.MyNode,
		announcer: &announcer,
		svcIP:   map[string]net.IP{},
	}

	return ret, nil
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

	announcing.With(prometheus.Labels{
		"service":  name,
		"node":     c.myNode,
		"ip":       lbIP.String(),
	}).Set(1)
	l.Log("event", "serviceAnnounced", "msg", "service has IP, announcing")
	c.client.Infof(svc, "nodeAssigned", "announcing from node %q", c.myNode)

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(l gokitlog.Logger, name, reason string) k8s.SyncState {
	if err := c.announcer.DeleteBalancer(l, name, reason); err != nil {
		l.Log("op", "deleteBalancer", "error", err, "msg", "failed to clear balancer state")
		return k8s.SyncStateError
	}

	announcing.Delete(prometheus.Labels{
		"service":  name,
		"node":     c.myNode,
		"ip":       c.svcIP[name].String(),
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

// An Announcer can announce an IP address
type Announcer interface {
	SetConfig(gokitlog.Logger, *config.Config) error
	ShouldAnnounce(gokitlog.Logger, string, *v1.Service, *v1.Endpoints) string
	SetBalancer(gokitlog.Logger, string, net.IP, *config.Pool) error
	DeleteBalancer(gokitlog.Logger, string, string) error
	SetNode(gokitlog.Logger, *v1.Node) error
}
