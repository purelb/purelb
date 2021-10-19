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
	"math/rand"
	"net"
	"os/exec"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	"purelb.io/internal/local"
	"purelb.io/internal/pfc"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

var (
	rfc1123Cleaner = strings.NewReplacer(".", "-", ":", "-", "/", "-")
)

type announcer struct {
	client     *k8s.Client
	logger     log.Logger
	myNode     string
	myNodeAddr string
	groups     map[string]*purelbv1.ServiceGroupEPICSpec // groupURL -> ServiceGroupEPICSpec
	pfcspec    *purelbv1.LBNodeAgentEPICSpec
	pinger     *exec.Cmd
	groupID    uint16
	myCluster  string

	// announcements is a map of services, keyed by the EPIC service
	// URL. The value is a pseudo-set of that service's endpoints that
	// we have announced.
	announcements map[string]map[string]struct{} // key: service's namespaced name, value: pseudo-set of key: endpoint URL, value: none

	// servicesGroups is a map from namespaced service names to the
	// groupURL that that service belongs to. We need this because when
	// a service is deleted we get only the service's name so we need to
	// cache everything else that we use to clean up.
	servicesGroups map[string]string
}

// NewAnnouncer returns a new Acnodal EPIC Announcer.
func NewAnnouncer(l log.Logger, node string, nodeAddr string) lbnodeagent.Announcer {
	return &announcer{logger: l, myNode: node, myNodeAddr: nodeAddr, announcements: map[string]map[string]struct{}{}, servicesGroups: map[string]string{}}
}

// SetClient configures this announcer to use the provided client.
func (a *announcer) SetClient(client *k8s.Client) {
	a.client = client
}

// SetConfig responds to configuration changes.
func (a *announcer) SetConfig(cfg *purelbv1.Config) error {
	a.myCluster = cfg.MyCluster

	// Scan the node agent configs to find the EPIC config (if present)
	haveConfig := false
	for _, agent := range cfg.Agents {
		if spec := agent.Spec.EPIC; spec != nil {
			a.logger.Log("op", "setConfig", "name", agent.Name, "config", spec)
			a.pfcspec = spec
			haveConfig = true
		}
	}

	// if we don't have any EPIC configs then we can return
	if !haveConfig {
		a.logger.Log("event", "noConfig")
		return nil
	}

	// We also need a service group config to tell us how to reach EPIC
	groups := map[string]*purelbv1.ServiceGroupEPICSpec{}
	haveConfig = false
	for _, group := range cfg.Groups {
		if spec := group.Spec.EPIC; spec != nil {
			a.logger.Log("op", "setConfig", "name", group.Name, "config", spec)
			groups[group.Name] = spec
			haveConfig = true
		}
	}

	// if we don't have any EPIC configs then we can return
	if !haveConfig {
		a.logger.Log("event", "noConfig")
		return nil
	}

	// update our configuration
	a.groups = groups

	// Ensure that we re-load the PFC components
	a.resetPFC(a.pfcspec.EncapAttachment.Interface, a.pfcspec.DecapAttachment.Interface)

	// We might have been notified of some services before we got this
	// config notification and so were unable to announce, so trigger a
	// resync. This also ensures that we set up the PFC which we reset
	// above.
	a.client.ForceSync()

	// Start the GUE pinger if it's not running
	if a.pinger == nil {
		a.pinger = exec.Command("/opt/acnodal/bin/gue_ping_svc_auto", "25")
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
	nsName := svc.Namespace + "/" + svc.Name

	l := log.With(a.logger, "service", nsName, "group", groupName)
	l.Log("op", "SetBalancer", "endpoints", endpoints.Subsets)

	// Pause a small random interval to avoid the "thundering herd", for
	// example, when a user changes from NodePort to LoadBalancer or
	// vice versa
	time.Sleep(time.Duration(rand.Intn(1000)) * time.Millisecond)

	// if the service isn't a load balancer then clean up instead of
	// announcing
	if svc.Spec.Type != "LoadBalancer" {
		return a.DeleteBalancer(nsName, "notAnLB", nil)
	}

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
	a.servicesGroups[nsName] = groupName

	// connect to the EPIC
	epic, err := NewEPIC(a.myCluster, *group)
	if err != nil {
		l.Log("op", "SetBalancer", "error", err, "msg", "Connection init to EPIC failed")
		return fmt.Errorf("Connection init to EPIC failed")
	}

	createUrl := svc.Annotations[purelbv1.EndpointAnnotation]

	announcements := map[string]struct{}{} // pseudo-set: key: endpoint URLs, value: struct{}

	// For each port in each endpoint address in each subset on this node, contact the EPIC
	for _, ep := range endpoints.Subsets {
		for _, address := range ep.Addresses {
			for _, port := range ep.Ports {
				if address.NodeName != nil && *address.NodeName == a.myNode {

					// Announce this endpoint to the EPIC and add it to the
					// announcements list
					epResponse, err := epic.AnnounceEndpoint(createUrl, nsName, address.IP, port, a.myNodeAddr)
					if err != nil {
						l.Log("op", "AnnounceEndpoint", "error", err)
						return fmt.Errorf("announcement failed")
					}

					// Add this endpoint to the set of endpoints that we've
					// announced this time
					announcements[epResponse.Links["self"]] = struct{}{}
					l.Log("op", "AnnounceEndpoint", "ep-address", address.IP, "ep-port", port.Port, "node", a.myNode, "link", epResponse.Links["self"])

				} else {
					l.Log("op", "DontAnnounceEndpoint", "ep-address", address.IP, "ep-port", port.Port, "node", "not me")
				}
			}
		}
	}

	// Try and try again to set up the tunnels. The most likely cause of
	// retries is that the tunnels won't have been set up yet on the
	// EPIC side in which case we just back off and retry. If something
	// goes really wrong then we return an error.
	tries := 10
	err = fmt.Errorf("")
	for retry := true; err != nil && retry && tries > 0; tries-- {
		err, retry = a.setupTunnels(*a.pfcspec, svc, endpoints, l, epic)
		if err != nil && tries > 1 {
			time.Sleep(3 * time.Second)
		}
	}
	if err != nil {
		return err
	}

	// See if there are any endpoints that we need to delete, i.e.,
	// endpoints that we had previously announced but didn't announce
	// this time.
	for epURL := range a.announcements[nsName] {
		if _, announcedThisTime := announcements[epURL]; !announcedThisTime {
			l.Log("op", "DeleteEndpoint", "ep-url", epURL)
			if err := epic.Delete(epURL); err != nil {
				l.Log("op", "DeleteEndpoint", "error", err)
			}
		}
	}

	// Update the persistent announcement set
	a.announcements[nsName] = announcements

	l.Log("announcements", a.announcements)

	return err
}

func (a *announcer) setupTunnels(spec purelbv1.LBNodeAgentEPICSpec, svc *v1.Service, endpoints *v1.Endpoints, l log.Logger, epic EPIC) (err error, retry bool) {
	// Get the service that owns this endpoint. This endpoint
	// will either re-use an existing tunnel or set up a new one
	// for this node. Tunnels belong to the service.
	svcResponse, err := epic.FetchService(svc.Annotations[purelbv1.ServiceAnnotation])
	if err != nil {
		l.Log("op", "AnnounceEndpoint", "error", err)
		return fmt.Errorf("service not found"), false
	}

	// For each port in each endpoint address in each subset on this node, set up the PFC tunnel
	for _, ep := range endpoints.Subsets {
		for _, address := range ep.Addresses {
			for _, port := range ep.Ports {
				if address.NodeName != nil && *address.NodeName == a.myNode {

					// See if the tunnel is there (it might not be yet since it
					// sometimes takes a while to set up). If it's not there
					// then return an error which will cause a retry.
					myTunnels, exists := svcResponse.Service.Spec.TunnelEndpoints[a.myNodeAddr]
					if !exists {
						l.Log("op", "fetchTunnelConfig", "endpoints", svcResponse.Service.Spec.TunnelEndpoints)

						//  Back off a bit so we don't hammer EPIC
						time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond)

						return fmt.Errorf("tunnel config not found for %s", a.myNodeAddr), true
					}

					// Now that we've got the service response we have enough
					// info to set up the tunnels
					for _, myTunnel := range myTunnels.EPICEndpoints {
						err = a.setupPFC(*a.pfcspec, address, myTunnel.TunnelID, a.myNodeAddr, myTunnel.Address, myTunnel.Port.Port, svcResponse.Service.Spec.TunnelKey)
						if err != nil {
							l.Log("op", "SetupPFC", "error", err)
						}
					}
				} else {
					l.Log("op", "DontAnnounceEndpoint", "ep-address", address.IP, "ep-port", port.Port, "node", "not me")
				}
			}
		}
	}

	return nil, false
}

func (a *announcer) DeleteBalancer(name, reason string, _ *v1.LoadBalancerIngress) error {
	l := log.With(a.logger, "service", name)
	l.Log("op", "DeleteBalancer", "reason", reason)

	// FIXME: clean up PFC tunnels

	// Empty this service's cache entries
	delete(a.announcements, name)
	delete(a.servicesGroups, name)

	return nil
}

func (a *announcer) SetElection(election *election.Election) {
	// this is a no-op, we don't care about elections
}

func (a *announcer) Shutdown() {
}

// setupPFC sets up the Acnodal PFC components and GUE tunnel to
// communicate with the Acnodal EPIC.
func (a *announcer) setupPFC(spec purelbv1.LBNodeAgentEPICSpec, address v1.EndpointAddress, tunnelID uint32, myAddr string, tunnelAddr string, tunnelPort int32, tunnelAuth string) error {
	// Determine the interface to which to attach the Encap PFC
	encapIntf, err := interfaceOrDefault(spec.EncapAttachment.Interface, address)
	if err != nil {
		return err
	}
	pfc.SetupNIC(a.logger, encapIntf.Attrs().Name, "encap", spec.EncapAttachment.Direction, spec.EncapAttachment.QID, spec.EncapAttachment.Flags)

	// Determine the interface to which to attach the Decap PFC
	decapIntf, err := interfaceOrDefault(spec.DecapAttachment.Interface, address)
	if err != nil {
		return err
	}
	pfc.SetupNIC(a.logger, decapIntf.Attrs().Name, "decap", spec.DecapAttachment.Direction, spec.DecapAttachment.QID, spec.DecapAttachment.Flags)

	// set up the GUE tunnel to the EPIC
	err = pfc.SetTunnel(a.logger, tunnelID, tunnelAddr, myAddr, tunnelPort)
	if err != nil {
		a.logger.Log("op", "SetTunnel", "error", err)
		return err
	}

	// set up service forwarding to forward packets through the GUE
	// tunnel
	return pfc.SetService(a.logger, tunnelAuth, tunnelID)
}

func (a *announcer) resetPFC(encapName string, decapName string) error {
	// we want to ensure that we load the PFC filter programs and
	// maps. Filters survive a pod restart, but maps don't, so we delete
	// the filters so they'll get reloaded in SetBalancer() which will
	// implicitly set up the maps.

	// Cleanup any explicitly-specified interfaces (i.e., not "default")
	if encapName != "default" {
		pfc.CleanupFilter(a.logger, encapName, "ingress")
		pfc.CleanupFilter(a.logger, encapName, "egress")
		pfc.CleanupQueueDiscipline(a.logger, encapName)
	}
	if decapName != "default" {
		pfc.CleanupFilter(a.logger, decapName, "ingress")
		pfc.CleanupFilter(a.logger, decapName, "egress")
		pfc.CleanupQueueDiscipline(a.logger, decapName)
	}

	// Clean up the default interfaces, too
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

// interfaceOrDefault returns info about an interface. If intName is
// "default" then the interface will be whichever interface has the
// least-cost default route. Otherwise, it will be the interface whose
// name is "intName". The address family to which "address" belongs is
// used to determine the default interface.
//
// If the error returned is non-nil then the netlink.Link is
// undefined.
func interfaceOrDefault(intName string, address v1.EndpointAddress) (netlink.Link, error) {
	if intName == "default" {
		// figure out which interface is the default
		return local.DefaultInterface(local.AddrFamily(net.ParseIP(address.IP)))
	}

	return netlink.LinkByName(intName)
}
