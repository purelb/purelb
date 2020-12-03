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
	"os/exec"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	"purelb.io/internal/local"
	"purelb.io/internal/pfc"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"github.com/vishvananda/netlink/nl"
)

const (
	TUNNEL_AUTH   = "fredfredfredfred"
	CNI_INTERFACE = "cni0"
)

type announcer struct {
	client     *k8s.Client
	logger     log.Logger
	myNode     string
	myNodeAddr string
	config     *purelbv1.ServiceGroupEGWSpec
	baseURL    *url.URL
	pinger     *exec.Cmd
}

// NewAnnouncer returns a new Acnodal EGW Announcer.
func NewAnnouncer(l log.Logger, node string, nodeAddr string) lbnodeagent.Announcer {
	return &announcer{logger: l, myNode: node, myNodeAddr: nodeAddr}
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

			// if we're going to announce then we want to ensure that we
			// load the PFC filter programs and maps. Filters survive a pod
			// restart, but maps don't, so we delete the filters so they'll
			// get reloaded in SetBalancer() which will implicitly set up
			// the maps.
			pfc.CleanupFilter(a.logger, CNI_INTERFACE, "ingress")
			pfc.CleanupFilter(a.logger, CNI_INTERFACE, "egress")
			pfc.CleanupQueueDiscipline(a.logger, CNI_INTERFACE)
			// figure out which interface is the default and clean that up, too
			default4, err := local.DefaultInterface(nl.FAMILY_V4)
			if err == nil {
				pfc.CleanupFilter(a.logger, default4.Attrs().Name, "ingress")
				pfc.CleanupFilter(a.logger, default4.Attrs().Name, "egress")
				pfc.CleanupQueueDiscipline(a.logger, default4.Attrs().Name)
			} else {
				a.logger.Log("op", "SetConfig", "error", err)
			}
			default6, err := local.DefaultInterface(nl.FAMILY_V6)
			if err == nil && default6 != nil {
				pfc.CleanupFilter(a.logger, default6.Attrs().Name, "ingress")
				pfc.CleanupFilter(a.logger, default6.Attrs().Name, "egress")
				pfc.CleanupQueueDiscipline(a.logger, default6.Attrs().Name)
			}

			// We might have been notified of some services before we got
			// this config notification, so trigger a resync
			a.client.ForceSync()
		}
	}

	if a.config == nil {
		a.logger.Log("event", "noConfig")
	}

	return nil
}

func (a *announcer) SetBalancer(svc *v1.Service, endpoints *v1.Endpoints) error {
	var err error

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

	createUrl := svc.Annotations[purelbv1.EndpointAnnotation]

	// For each endpoint address in each subset on this node, contact the EGW
	for _, ep := range endpoints.Subsets {
		port := ep.Ports[0].Port
		for _, address := range ep.Addresses {
			if address.NodeName != nil && *address.NodeName == a.myNode {
				l.Log("op", "AnnounceEndpoint", "ep-address", address.IP, "ep-port", port, "node", a.myNode)

				// Start the GUE pinger if it's not running
				if a.pinger == nil {
					a.pinger = exec.Command("/opt/acnodal/bin/gue_ping_svc_auto", "25", "10", "3")
					err := a.pinger.Start()
					if err != nil {
						l.Log("event", "error starting pinger", "error", err)
						a.pinger = nil // retry next time we announce an endpoint
					}
				}

				// Announce this endpoint to the EGW
				response, err := egw.AnnounceEndpoint(createUrl, address.IP, ep.Ports[0], a.myNodeAddr)
				if err != nil {
					l.Log("op", "AnnounceEndpoint", "error", err)
				}

				myTunnel, exists := response.Service.Status.EGWTunnelEndpoints[a.myNodeAddr]
				if !exists {
					l.Log("op", "fetchTunnelConfig", "endpoints", response.Service.Status.EGWTunnelEndpoints)
					return fmt.Errorf("tunnel config not found")
				}

				// Now that we've got the announcement response we have enough
				// info to set up the tunnel
				err = a.setupPFC(address, myTunnel.TunnelID, response.Service.Spec.GUEKey, a.myNodeAddr, myTunnel.Address, myTunnel.Port.Port)
				if err != nil {
					l.Log("op", "SetupPFC", "error", err)
				}
			} else {
				l.Log("op", "DontAnnounceEndpoint", "ep-address", address.IP, "ep-port", port, "node", "not me")
			}
		}
	}

	return err
}

func (a *announcer) DeleteBalancer(name, reason string, addr net.IP) error {
	return nil
}

func (a *announcer) SetElection(election *election.Election) {
	// this is a no-op, we don't care about elections
}

func (a *announcer) Shutdown() {
}

// setupPFC sets up the Acnodal PFC components and GUE tunnel to
// communicate with the Acnodal EGW.
func (a *announcer) setupPFC(address v1.EndpointAddress, tunnelID uint32, tunnelKey uint32, myAddr string, tunnelAddr string, tunnelPort int32) error {
	// cni0 is easy - its name is hard-coded
	pfc.SetupNIC(a.logger, CNI_INTERFACE, "egress", 1, 8)

	// figure out which interface is the default and set that up, too
	defaultNIC, err := local.DefaultInterface(local.AddrFamily(net.ParseIP(address.IP)))
	if err == nil {
		pfc.SetupNIC(a.logger, defaultNIC.Attrs().Name, "ingress", 0, 9)
	} else {
		a.logger.Log("op", "AnnounceEndpoint", "error", err)
	}

	// set up the GUE tunnel to the EGW
	script := fmt.Sprintf("/opt/acnodal/bin/cli_tunnel get %[1]d | grep %[2]s || /opt/acnodal/bin/cli_tunnel set %[1]d %[3]s %[4]d %[2]s %[4]d", tunnelID, tunnelAddr, myAddr, tunnelPort)
	a.logger.Log("op", "SetupTunnel", "script", script)
	cmd := exec.Command("/bin/sh", "-c", script)
	err = cmd.Run()
	if err != nil {
		a.logger.Log("op", "SetupTunnel", "error", err)
		return err
	}

	// split the tunnelKey into its parts: groupId in the upper 16 bits
	// and serviceId in the lower 16
	var groupId uint16 = uint16(tunnelKey & 0xffff)
	var serviceId uint16 = uint16(tunnelKey >> 16)

	// set up service forwarding to forward packets through the GUE
	// tunnel
	script = fmt.Sprintf("/opt/acnodal/bin/cli_service get %[1]d %[2]d | grep %[3]s || /opt/acnodal/bin/cli_service set-node %[1]d %[2]d %[3]s %[4]d", groupId, serviceId, TUNNEL_AUTH, tunnelID)
	a.logger.Log("op", "SetupService", "script", script)
	cmd = exec.Command("/bin/sh", "-c", script)
	return cmd.Run()
}
