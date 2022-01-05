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
	"fmt"
	"net"
	"regexp"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
)

type announcer struct {
	client   k8s.ServiceEvent
	logger   log.Logger
	myNode   string
	config   *purelbv1.LBNodeAgentLocalSpec
	groups   map[string]*purelbv1.ServiceGroupLocalSpec // groupName -> ServiceGroupLocalSpec
	election *election.Election
	dummyInt *netlink.Link // for non-local announcements

	// svcIngresses is a map from svcName to that Service's
	// Ingresses. Note that we may or may not advertise all of them
	// because we might lose an election or not have any active
	// endpoints, but in any case we need to ensure that we clean them
	// up if the Service is deleted.
	svcIngresses map[string][]v1.LoadBalancerIngress

	// localNameRegex is the pattern that we use to determine if an
	// interface is local or not.
	localNameRegex *regexp.Regexp
}

var announcing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: purelbv1.MetricsNamespace,
	Subsystem: "lbnodeagent",
	Name:      "announced",
	Help:      "Services announced from this node",
}, []string{
	"service",
	"node",
	"ip",
})

func init() {
	prometheus.MustRegister(announcing)
}

// NewAnnouncer returns a new local Announcer.
func NewAnnouncer(l log.Logger, node string) lbnodeagent.Announcer {
	return &announcer{logger: l, myNode: node, svcIngresses: map[string][]v1.LoadBalancerIngress{}}
}

// SetClient configures this announcer to use the provided client.
func (a *announcer) SetClient(client *k8s.Client) {
	a.client = client
}

func (a *announcer) SetConfig(cfg *purelbv1.Config) error {

	// the default is nil which means that we don't announce
	a.config = nil

	// if there's a "Local" agent config then we'll announce
	for _, agent := range cfg.Agents {
		if spec := agent.Spec.Local; spec != nil {
			a.logger.Log("op", "setConfig", "spec", spec, "name", agent.Namespace+"/"+agent.Name)
			a.config = spec

			// stash the local service group configs
			a.groups = map[string]*purelbv1.ServiceGroupLocalSpec{}
			for _, group := range cfg.Groups {
				if group.Spec.Local != nil {
					a.groups[group.ObjectMeta.Name] = group.Spec.Local
				}
			}

			// if the user specified an interface regex then we'll compile
			// that now, and use it (when we get an address) to find a local
			// interface
			if spec.LocalInterface != "default" {
				if regex, err := regexp.Compile(spec.LocalInterface); err != nil {
					return fmt.Errorf("error compiling regex \"%s\": %s", spec.LocalInterface, err.Error())
				} else {
					a.localNameRegex = regex
				}
			} else {
				a.localNameRegex = nil

			}

			// now that we've got a config we can create the dummy interface
			var err error
			if a.dummyInt, err = addDummyInterface(spec.ExtLBInterface); err != nil {
				return fmt.Errorf("error adding interface \"%s\": %s", spec.ExtLBInterface, err.Error())
			}

			// we've got our marching orders so we don't need to continue
			// scanning
			return nil
		}
	}

	if a.config == nil {
		a.logger.Log("event", "noConfig")
	}

	return nil
}

func (a *announcer) SetBalancer(svc *v1.Service, endpoints *v1.Endpoints) error {
	// retErr caches an error while we try other operations. Because we
	// might have more than one interface to announce, if an error
	// happens on the first one we still want to try the second. Instead
	// of "return err" we'll stash the error in retErr and keep
	// going. Once we're done we'll return any error that
	// happened. Well, technically we'll return the most recent error.
	var retErr error = nil

	nsName := svc.Namespace + "/" + svc.Name
	l := log.With(a.logger, "service", nsName)

	// if we haven't been configured then we won't announce
	if a.config == nil {
		l.Log("event", "noConfig")
		return nil
	}

	// add the address to our announcement database
	a.svcIngresses[nsName] = svc.Status.LoadBalancer.Ingress

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		// validate the allocated address
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			l.Log("op", "setBalancer", "error", "invalid LoadBalancer IP", "ip", svc.Status.LoadBalancer.Ingress[0].IP)
			continue
		}

		if a.localNameRegex != nil {
			// The user specified an announcement interface regex so use it to
			// try to find a local interface, otherwise announce remote
			lbIPNet, localif, err := findLocal(a.localNameRegex, lbIP)
			if err == nil {
				// We found a local interface, announce the address on it
				if err := a.announceLocal(svc, localif, lbIP, lbIPNet); err != nil {
					retErr = err
				}
			} else {
				// lbIP isn't local to any interfaces so add it to dummyInt
				if err := a.announceRemote(svc, endpoints, a.dummyInt, lbIP); err != nil {
					retErr = err
				}
			}

		} else {
			// The user wants us to determine the "default" interface
			announceInt, err := defaultInterface(AddrFamily(lbIP))
			if err != nil {
				l.Log("event", "announceError", "err", err)
				retErr = err
				continue
			}
			if lbIPNet, defaultif, err := checkLocal(announceInt, lbIP); err == nil {
				// The default interface is a local interface, announce the
				// address on it
				if err := a.announceLocal(svc, defaultif, lbIP, lbIPNet); err != nil {
					retErr = err
				}
			} else {
				// The default interface is remote, so add lbIP to dummyInt
				if err := a.announceRemote(svc, endpoints, a.dummyInt, lbIP); err != nil {
					retErr = err
				}
			}
		}
	}

	// Return the most recent error
	return retErr
}

func (a *announcer) announceLocal(svc *v1.Service, announceInt netlink.Link, lbIP net.IP, lbIPNet net.IPNet) error {
	l := log.With(a.logger, "service", svc.Name)
	nsName := svc.Namespace + "/" + svc.Name

	// Local addresses do not support ExternalTrafficPolicyLocal
	// Set the service back to ExternalTrafficPolicyCluster if adding to local interface
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		l.Log("op", "setBalancer", "error", "ExternalTrafficPolicy Local not supported on local Interfaces, setting to Cluster")
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
		if err := a.deleteAddress(nsName, "ClusterLocal", lbIP); err != nil {
			return err
		}
	}

	// We can announce the address if we win the election
	if winner := a.election.Winner(lbIP.String()); winner == a.myNode {

		// we won the election so we'll add the service address to our
		// node's default interface so linux will respond to ARP
		// requests for it
		l.Log("msg", "Winner, winner, Chicken dinner", "node", a.myNode, "service", nsName, "memberCount", a.election.Memberlist.NumMembers())
		a.client.Infof(svc, "AnnouncingLocal", "Node %s announcing %s on interface %s", a.myNode, lbIP, announceInt.Attrs().Name)

		addNetwork(lbIPNet, announceInt)
		svc.Annotations[purelbv1.NodeAnnotation+addrFamilyName(lbIP)] = a.myNode
		svc.Annotations[purelbv1.IntAnnotation+addrFamilyName(lbIP)] = announceInt.Attrs().Name
		announcing.With(prometheus.Labels{
			"service": nsName,
			"node":    a.myNode,
			"ip":      lbIP.String(),
		}).Set(1)

	} else {

		// We lost the election so we'll withdraw any announcement that
		// we might have been making
		l.Log("msg", "notWinner", "node", a.myNode, "winner", winner, "service", nsName, "memberCount", a.election.Memberlist.NumMembers())
		return a.deleteAddress(nsName, "lostElection", lbIP)
	}

	return nil
}

func (a *announcer) announceRemote(svc *v1.Service, endpoints *v1.Endpoints, announceInt *netlink.Link, lbIP net.IP) error {
	l := log.With(a.logger, "service", svc.Name)
	nsName := svc.Namespace + "/" + svc.Name

	// The service address is non-local, i.e., it's not on the same
	// subnet as our default interface.

	// Should we announce?
	// No, if externalTrafficPolicy is Local && there's no ready local endpoint
	// Yes, in all other cases
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && !nodeHasHealthyEndpoint(endpoints, a.myNode) {
		l.Log("msg", "policyLocalNoEndpoints", "node", a.myNode, "service", nsName)
		return a.deleteAddress(nsName, "noEndpoints", lbIP)
	}

	// add this address to the "dummy" interface so routing software
	// (e.g., bird) will announce routes for it
	poolName, gotName := svc.Annotations[purelbv1.PoolAnnotation]
	if gotName {
		allocPool := a.groups[poolName]
		l.Log("msg", "announcingNonLocal", "node", a.myNode, "service", nsName)
		a.client.Infof(svc, "AnnouncingNonLocal", "Announcing %s from node %s interface %s", lbIP, a.myNode, (*a.dummyInt).Attrs().Name)
		family := AddrFamily(lbIP)
		subnet, err := allocPool.FamilySubnet(family)
		if err != nil {
		}
		aggregation, err := allocPool.FamilyAggregation(family)
		if err != nil {
			return err
		}
		addVirtualInt(lbIP, *a.dummyInt, subnet, aggregation)
		announcing.With(prometheus.Labels{
			"service": nsName,
			"node":    a.myNode,
			"ip":      lbIP.String(),
		}).Set(1)
	} else {
		return fmt.Errorf("PoolAnnotation missing from service %s", nsName)
	}

	return nil
}

// DeleteBalancer deletes the IP address associated with the
// balancer. nsName is a namespaced name, e.g., "root/service42". The
// addr parameter is optional and shouldn't be necessary but in some
// cases (probably involving startup and/or shutdown) we have seen
// calls to DeleteBalancer with services that weren't in the svcAdvs
// map, so the service's address wasn't removed. For now, this is a
// "belt and suspenders" double-check.
func (a *announcer) DeleteBalancer(nsName, reason string, _ net.IP) error {
	ingress, knowAboutIt := a.svcIngresses[nsName]
	if !knowAboutIt {
		a.logger.Log("msg", "Unknown LB, can't delete", "name", nsName)
		return nil
	}

	// delete this service from our announcement database
	delete(a.svcIngresses, nsName)

	for _, ingress := range ingress {
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			return fmt.Errorf("invalid LoadBalancer IP: %s, belongs to %s", ingress.IP, nsName)
		}
		a.deleteAddress(nsName, reason, lbIP)
	}
	return nil
}

// deleteAddress deletes the IP address associated with the
// balancer. The addr parameter is optional and shouldn't be necessary
// but in some cases (probably involving startup and/or shutdown) we
// have seen calls to DeleteBalancer with services that weren't in the
// svcAdvs map, so the service's address wasn't removed. For now, this
// is a "belt and suspenders" double-check.
func (a *announcer) deleteAddress(nsName, reason string, svcAddr net.IP) error {
	// delete the service from Prometheus, i.e., it won't show up in the
	// metrics anymore
	announcing.Delete(prometheus.Labels{
		"service": nsName,
		"node":    a.myNode,
		"ip":      svcAddr.String(),
	})

	// if any other service is still using that address then we don't
	// want to withdraw it
	for otherSvc, announcedAddrs := range a.svcIngresses {
		for _, announcedAddr := range announcedAddrs {
			if announcedAddr.IP == svcAddr.String() && otherSvc != nsName {
				a.logger.Log("event", "withdrawAnnouncement", "service", nsName, "reason", reason, "msg", "ip in use by other service", "other", otherSvc)
				return nil
			}
		}
	}

	a.logger.Log("event", "withdrawAddress", "ip", svcAddr, "service", nsName, "reason", reason)
	deleteAddr(svcAddr)

	return nil
}

// Shutdown cleans up changes that we've made to the local networking
// configuration.
func (a *announcer) Shutdown() {
	// withdraw any announcements that we have made
	for nsName := range a.svcIngresses {
		if err := a.DeleteBalancer(nsName, "shutdown", nil); err != nil {
			a.logger.Log("op", "shutdown", "error", err)
		}
	}

	// remove the "dummy" interface
	removeInterface(a.dummyInt)
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

// addrFamilyName returns whether lbIP is an IPV4 or IPV6 address.
// The return value will be "IPv6" if the address is an IPV6 address,
// "IPv4" if it's IPV4, or "unknown" if the family can't be determined.
func addrFamilyName(lbIP net.IP) (lbIPFamily string) {
	lbIPFamily = "-unknown"

	if nil != lbIP.To16() {
		lbIPFamily = "-IPv6"
	}

	if nil != lbIP.To4() {
		lbIPFamily = "-IPv4"
	}

	return
}
