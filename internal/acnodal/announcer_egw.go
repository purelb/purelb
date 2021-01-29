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
	CNI_INTERFACE = "cni0"
)

type announcer struct {
	client     *k8s.Client
	logger     log.Logger
	myNode     string
	myNodeAddr string
	groups     map[string]*purelbv1.ServiceGroupEGWSpec // groupURL -> ServiceGroupEGWSpec
	pinger     *exec.Cmd
	sweeper    *exec.Cmd
	groupID    uint16
	// announcements is a map of services, keyed by the EGW service
	// URL. The value is a pseudo-set of that service's endpoints that
	// we have announced.
	announcements map[string]map[string]struct{} // key: endpoint create URL, value: pseudo-set of key: endpoint URL, value: none
}

// NewAnnouncer returns a new Acnodal EGW Announcer.
func NewAnnouncer(l log.Logger, node string, nodeAddr string) lbnodeagent.Announcer {
	return &announcer{logger: l, myNode: node, myNodeAddr: nodeAddr, announcements: map[string]map[string]struct{}{}}
}

// SetClient configures this announcer to use the provided client.
func (a *announcer) SetClient(client *k8s.Client) {
	a.client = client

	a.resetPFC()
}

// SetConfig responds to configuration changes.
func (a *announcer) SetConfig(cfg *purelbv1.Config) error {
	// we'll announce for any an "EGW" *service groups*. At this point
	// there's no egw node agent-specific config so we don't require an
	// EGW LBNodeAgent resource, just one or more EGW ServiceGroup.
	haveConfig := false
	groups := map[string]*purelbv1.ServiceGroupEGWSpec{}
	for _, group := range cfg.Groups {
		if spec := group.Spec.EGW; spec != nil {
			a.logger.Log("op", "setConfig", "name", group.Name, "config", spec)
			groups[group.Name] = spec
			haveConfig = true
		}
	}

	// if we don't have any EGW configs then we can return
	if !haveConfig {
		a.logger.Log("event", "noConfig")
		return nil
	}

	// update our configuration
	a.groups = groups

	// We might have been notified of some services before we got this
	// config notification and so were unable to announce, so trigger a
	// resync.
	a.client.ForceSync()

	// Start the GUE pinger if it's not running
	if a.pinger == nil {
		a.pinger = exec.Command("/opt/acnodal/bin/gue_ping_svc_auto", "25", "10", "3")
		err := a.pinger.Start()
		if err != nil {
			a.logger.Log("event", "error starting pinger", "error", err)
			a.pinger = nil // retry next time we get a config announcement
		}
	}

	return nil
}

func (a *announcer) SetBalancer(svc *v1.Service, endpoints *v1.Endpoints) error {
	var err error
	groupName, haveGroupURL := svc.Annotations[purelbv1.PoolAnnotation]
	serviceURL, _ := svc.Annotations[purelbv1.ServiceAnnotation]

	l := log.With(a.logger, "service", svc.ObjectMeta.Name, "group", groupName)
	l.Log("op", "SetBalancer", "endpoints", endpoints.Subsets)

	// if the service doesn't have a group annotation then don't
	// announce
	if !haveGroupURL {
		l.Log("event", "noGroupAnnotation")
		return nil
	}

	// if we don't have a config for this service's group then don't
	// announce
	group, haveGroup := a.groups[groupName]
	if !haveGroup {
		l.Log("event", "noConfig")
		return nil
	}

	// connect to the EGW
	egw, err := NewEGW(*group)
	if err != nil {
		l.Log("op", "SetBalancer", "error", err, "msg", "Connection init to EGW failed")
		return fmt.Errorf("Connection init to EGW failed")
	}

	// get the group's owning account
	account, err := egw.GetAccount()
	if err != nil {
		l.Log("op", "SetBalancer", "error", err, "msg", "can't get owning account")
		return fmt.Errorf("can't get owning account")
	}

	createUrl := svc.Annotations[purelbv1.EndpointAnnotation]

	announcements := map[string]struct{}{} // pseudo-set: key: endpoint URLs, value: struct{}

	// For each port in each endpoint address in each subset on this node, contact the EGW
	for _, ep := range endpoints.Subsets {
		for _, address := range ep.Addresses {
			for _, port := range ep.Ports {
				if address.NodeName != nil && *address.NodeName == a.myNode {

					// Announce this endpoint to the EGW and add it to the
					// announcements list
					epResponse, err := egw.AnnounceEndpoint(createUrl, address.IP, port, a.myNodeAddr)
					if err != nil {
						l.Log("op", "AnnounceEndpoint", "error", err)

						// retry once before moving on
						epResponse, err = egw.AnnounceEndpoint(createUrl, address.IP, port, a.myNodeAddr)
					}

					// Add this endpoint to the set of endpoints that we've
					// announced this time
					announcements[epResponse.Links["self"]] = struct{}{}
					l.Log("op", "AnnounceEndpoint", "ep-address", address.IP, "ep-port", port.Port, "node", a.myNode, "link", epResponse.Links["self"])

					// Get the service that owns this endpoint. This endpoint
					// will either re-use an existing tunnel or set up a new one
					// for this node. Tunnels belong to the service.
					svcResponse, err := egw.FetchService(svc.Annotations[purelbv1.ServiceAnnotation])
					if err != nil {
						l.Log("op", "AnnounceEndpoint", "error", err)
						return fmt.Errorf("service not found")
					}

					// See if the tunnel is there (it might not be yet since it
					// sometimes takes a while to set up). If it's not there
					// then return an error which will cause a retry.
					myTunnel, exists := svcResponse.Service.Status.EGWTunnelEndpoints[a.myNodeAddr]
					if !exists {
						l.Log("op", "fetchTunnelConfig", "endpoints", svcResponse.Service.Status.EGWTunnelEndpoints)
						return fmt.Errorf("tunnel config not found")
					}

					// Now that we've got the service response we have enough
					// info to set up the tunnel
					err = a.setupPFC(address, myTunnel.TunnelID, account.Account.Spec.GroupID, svcResponse.Service.Spec.ServiceID, a.myNodeAddr, myTunnel.Address, myTunnel.Port.Port, group.AuthCreds)
					if err != nil {
						l.Log("op", "SetupPFC", "error", err)
					}
				} else {
					l.Log("op", "DontAnnounceEndpoint", "ep-address", address.IP, "ep-port", port.Port, "node", "not me")
				}
			}
		}
	}

	// See if there are any endpoints that we need to delete, i.e.,
	// endpoints that we had previously announced but didn't announce
	// this time.
	for epURL := range a.announcements[serviceURL] {
		if _, announcedThisTime := announcements[epURL]; !announcedThisTime {
			l.Log("op", "DeleteEndpoint", "ep-url", epURL)
			if err := egw.Delete(epURL); err != nil {
				l.Log("op", "DeleteEndpoint", "error", err)
			}
			if err := a.cleanupPFC(); err != nil {
				l.Log("op", "cleanupPFC", "error", err)
			}
		}
	}

	// Update the persistent announcement set
	a.announcements[serviceURL] = announcements

	l.Log("announcements", a.announcements)

	return err
}

func (a *announcer) DeleteBalancer(name, reason string, addr *v1.LoadBalancerIngress) error {
	return nil
}

func (a *announcer) SetElection(election *election.Election) {
	// this is a no-op, we don't care about elections
}

func (a *announcer) Shutdown() {
}

// setupPFC sets up the Acnodal PFC components and GUE tunnel to
// communicate with the Acnodal EGW.
func (a *announcer) setupPFC(address v1.EndpointAddress, tunnelID uint32, groupID uint16, serviceID uint16, myAddr string, tunnelAddr string, tunnelPort int32, tunnelAuth string) error {
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
	err = pfc.SetTunnel(a.logger, tunnelID, tunnelAddr, myAddr, tunnelPort)
	if err != nil {
		a.logger.Log("op", "SetTunnel", "error", err)
		return err
	}

	// set up service forwarding to forward packets through the GUE
	// tunnel
	return pfc.SetService(a.logger, groupID, serviceID, tunnelAuth, tunnelID)
}

func (a *announcer) cleanupPFC() error {
	return nil
}

func (a *announcer) resetPFC() error {
	// we want to ensure that we load the PFC filter programs and
	// maps. Filters survive a pod restart, but maps don't, so we delete
	// the filters so they'll get reloaded in SetBalancer() which will
	// implicitly set up the maps.
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

	return nil
}
