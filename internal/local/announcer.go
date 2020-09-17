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

package local

import (
	"net"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/election"
	"purelb.io/internal/lbnodeagent"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"github.com/vishvananda/netlink"
)

const (
	nodeAnnotation string = "purelb.io/announcing-node"
	intAnnotation  string = "purelb.io/announcing-interface"
)

type announcer struct {
	logger     log.Logger
	myNode     string
	nodeLabels labels.Set
	config     *purelbv1.LBNodeAgentLocalSpec
	groups     map[string]*purelbv1.ServiceGroupLocalSpec // groupName -> ServiceGroupLocalSpec
	svcAdvs    map[string]net.IP                          // svcName -> IP
	election   *election.Election
	dummy      *netlink.Link
}

// NewAnnouncer returns a new local Announcer.
func NewAnnouncer(l log.Logger, node string) lbnodeagent.Announcer {
	return &announcer{logger: l, myNode: node, svcAdvs: map[string]net.IP{}}
}

func (a *announcer) SetConfig(cfg *purelbv1.Config) error {
	a.logger.Log("event", "newConfig")

	// the default is nil which means that we don't announce
	a.config = nil

	// if there's a "Local" agent config then we'll announce
	for _, agent := range cfg.Agents {
		if spec := agent.Spec.Local; spec != nil {
			a.config = spec

			// stash the local service group configs
			a.groups = map[string]*purelbv1.ServiceGroupLocalSpec{}
			for _, group := range cfg.Groups {
				if group.Spec.Local != nil {
					a.groups[group.ObjectMeta.Name] = group.Spec.Local
				}
			}

			// now that we've got a config we can create the dummy interface
			var err error
			if a.dummy, err = addDummyInterface(spec.ExtLBInterface); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *announcer) SetBalancer(name string, svc *v1.Service, endpoints *v1.Endpoints) error {
	a.logger.Log("event", "announceService", "announcer", "local", "service", name)

	// if we haven't been configured then we won't announce
	if a.config == nil {
		a.logger.Log("event", "noConfig")
		return nil
	}

	// k8s may send us multiple events to advertize same address
	if _, ok := a.svcAdvs[name]; ok {
		a.logger.Log("event", "duplicateAnnouncement")
		return nil
	}

	// validate the allocated address
	lbIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	if lbIP == nil {
		a.logger.Log("op", "setBalancer", "error", "invalid LoadBalancer IP", "ip", svc.Status.LoadBalancer.Ingress[0].IP)
		return nil
	}

	if lbIPNet, defaultif, err := checkLocal(lbIP); err == nil {

		// the service address is local, i.e., it's within the same subnet
		// as our primary interface.  We can announce the address if we
		// win the election
		if winner := a.election.Winner(lbIP.String()); winner == a.myNode {

			// we won the election so we'll add the service address to our
			// node's default interface so linux will respond to ARP
			// requests for it
			a.logger.Log("msg", "Winner, winner, Chicken dinner", "node", a.myNode, "service", name)

			addNetwork(lbIPNet, defaultif)
			svc.Annotations[nodeAnnotation] = a.myNode
			svc.Annotations[intAnnotation] = defaultif.Attrs().Name
		} else {
			a.logger.Log("msg", "notWinner", "node", a.myNode, "winner", winner, "service", name)
		}
	} else {

		// The service address is non-local, i.e., it's not on the same
		// subnet as our default interface.

		// Should we advertise?
		// No, if externalTrafficPolicy is Local && there's no ready local endpoint
		// Yes, in all other cases
		if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && !nodeHasHealthyEndpoint(endpoints, a.myNode) {
			a.logger.Log("msg", "policyLocalNoEndpoints", "node", a.myNode, "service", name)
			return nil
		}

		// add this address to the "dummy" interface so routing software
		// (e.g., bird) will announce routes for it
		poolName, gotName := svc.Annotations["purelb.io/allocated-from"]
		if gotName {
			allocPool := a.groups[poolName]
			a.logger.Log("msg", "announcingNonLocal", "node", a.myNode, "service", name, "reason", err)
			addVirtualInt(lbIP, *a.dummy, allocPool.Subnet, allocPool.Aggregation)
		}
	}

	// add the address to our announcement database
	a.svcAdvs[name] = lbIP

	return nil
}

func (a *announcer) DeleteBalancer(name, reason string) error {

	// if the service isn't in our database then we can't withdraw the address
	svcAddr, ok := a.svcAdvs[name]
	if !ok {
		a.logger.Log("event", "withdrawAnnouncement", "service", name, "reason", reason, "msg", "service unknown")
		return nil
	}

	// delete this service from our announcement database
	delete(a.svcAdvs, name)

	// if any other service is still using that address then we don't
	// want to withdraw it
	for _, addr := range a.svcAdvs {
		if addr.Equal(svcAddr) {
			a.logger.Log("event", "withdrawAnnouncement", "service", name, "reason", reason, "msg", "ip in use by other service")
			return nil
		}
	}

	a.logger.Log("event", "withdrawAnnouncement", "msg", "Delete balancer", "service", name, "reason", reason)
	deleteAddr(svcAddr)

	return nil
}

func (a *announcer) SetNode(node *v1.Node) error {
	a.logger.Log("event", "updatedNodes", "msg", "Node announced", "name", node.Name)
	return nil
}

// Shutdown cleans up changes that we've made to the local networking
// configuration.
func (a *announcer) Shutdown() {
	// withdraw any announcements that we have made
	for _, ip := range a.svcAdvs {
		deleteAddr(ip)
	}

	// remove the "dummy" interface
	removeInterface(a.config.ExtLBInterface)
}

func (a *announcer) SetElection(election *election.Election) {
	a.election = election
}

// nodeHasHealthyEndpoint returns true if node has at least one
// healthy endpoint.
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
			// At least one fully healthy endpoint on this node
			return true
		}
	}
	return false
}
