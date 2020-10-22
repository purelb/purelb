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

package acnodal

import (
	"fmt"
	"net"
	"net/url"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
)

type announcer struct {
	client  k8s.ServiceEvent
	logger  log.Logger
	myNode  string
	config  *purelbv1.ServiceGroupEGWSpec
	baseURL *url.URL
}

// NewAnnouncer returns a new Acnodal EGW Announcer.
func NewAnnouncer(l log.Logger, node string) lbnodeagent.Announcer {
	return &announcer{logger: l, myNode: node}
}

// SetClient configures this announcer to use the provided client.
func (a *announcer) SetClient(client *k8s.Client) {
	a.client = client
}

func (a *announcer) SetConfig(cfg *purelbv1.Config) error {
	// the default is nil which means that we don't announce
	a.config = nil

	// if there's an "EGW" *service group* then we'll announce. At this
	// point there's no egw node agent-specific config so we don't
	// require an EGW LBNodeAgent resource, just a ServiceGroup
	for _, group := range cfg.Groups {
		if spec := group.Spec.EGW; spec != nil {
			a.logger.Log("op", "setConfig", "config", spec)
			a.config = spec
			// Use the hostname from the service group, but reset the path.  EGW
			// and Netbox each have their own API URL schemes so we only need
			// the protocol, host, port, credentials, etc.
			url, err := url.Parse(group.Spec.EGW.URL)
			if err != nil {
				a.logger.Log("op", "setConfig", "error", err)
				return fmt.Errorf("cannot parse EGW URL %v", err)
			}
			url.Path = ""
			a.baseURL = url
		}
	}

	if a.config == nil {
		a.logger.Log("event", "noConfig")
	}

	return nil
}

func (a *announcer) SetBalancer(svc *v1.Service, endpoints *v1.Endpoints) error {
	l := log.With(a.logger, "service", svc.Name)

	// if we haven't been configured then we won't announce
	if a.config == nil {
		l.Log("event", "noConfig")
		return nil
	}

	// connect to the EGW
	egw, err := NewEGW(a.baseURL.String())
	if err != nil {
		l.Log("op", "SetBalancer", "error", err, "msg", "Connection init to EGW failed")
		return fmt.Errorf("Connection init to EGW failed")
	}

	createUrl := svc.Annotations["acnodal.io/endpointcreateURL"]

	// For each endpoint address in each subset on this node, contact the EGW
	for _, ep := range endpoints.Subsets {
		port := ep.Ports[0].Port
		for _, address := range ep.Addresses {
			if address.NodeName == nil || *address.NodeName != a.myNode {
				l.Log("op", "DontAnnounceEndpoint", "address", address.IP, "port", port, "node", "not me")
			} else {
				l.Log("op", "AnnounceEndpoint", "ep-address", address.IP, "ep-port", port, "node", a.myNode)
				err := egw.AnnounceEndpoint(createUrl, address.IP, ep.Ports[0])
				if err != nil {
					l.Log("op", "AnnounceEndpoint", "error", err)
				}
			}
		}
	}

	return nil
}

func (a *announcer) DeleteBalancer(name, reason string, addr net.IP) error {
	return nil
}

func (a *announcer) SetElection(election *election.Election) {
	// this is a no-op, we don't care about elections
}

func (a *announcer) Shutdown() {
}
