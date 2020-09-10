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
package acnodal

import (
	"fmt"
	"net/url"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"purelb.io/internal/election"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
)

type announcer struct {
	logger     log.Logger
	myNode     string
	nodeLabels labels.Set
	config     *purelbv1.ServiceGroupEGWSpec
	baseURL    *url.URL
}

func NewAnnouncer(l log.Logger, node string) *announcer {
	return &announcer{logger: l, myNode: node}
}

func (c *announcer) SetConfig(cfg *purelbv1.Config) error {
	c.logger.Log("event", "newConfig")

	// the default is nil which means that we don't announce
	c.config = nil

	// if there's an "EGW" service group then we'll announce
	for _, group := range cfg.Groups {
		if spec := group.Spec.EGW; spec != nil {
			c.config = spec
			// Use the hostname from the service group, but reset the path.  EGW
			// and Netbox each have their own API URL schemes so we only need
			// the protocol, host, port, credentials, etc.
			url, err := url.Parse(group.Spec.EGW.URL)
			if err != nil {
				c.logger.Log("op", "setConfig", "error", err)
				return fmt.Errorf("cannot parse EGW URL %v", err)
			}
			url.Path = ""
			c.baseURL = url
		}
	}

	return nil
}

func (c *announcer) SetBalancer(name string, svc *v1.Service, endpoints *v1.Endpoints) error {
	c.logger.Log("event", "announceService", "service", name)

	// if we haven't been configured then we won't announce
	if c.config == nil {
		return nil
	}

	// connect to the EGW
	egw, err := New(c.baseURL.String(), "")
	if err != nil {
		c.logger.Log("op", "SetBalancer", "error", err, "msg", "Connection init to EGW failed")
		return fmt.Errorf("Connection init to EGW failed")
	}

	createUrl := svc.Annotations["acnodal.io/endpointcreateURL"]

	// For each endpoint address in each subset on this node, contact the EGW
	for _, ep := range endpoints.Subsets {
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

func (c *announcer) SetElection(election *election.Election) {
	// this is a no-op, we don't care about elections
}

func (c *announcer) Shutdown() {
}
