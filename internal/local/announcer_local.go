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
	"strconv"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	utiliptables "k8s.io/kubernetes/pkg/util/iptables"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	"purelb.io/internal/masq"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"github.com/nadoo/ipset"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

type announcer struct {
	client   k8s.ServiceEvent
	logger   log.Logger
	myNode   string
	config   *purelbv1.LBNodeAgentSpec
	groups   map[string]*purelbv1.ServiceGroupLocalSpec // groupName -> ServiceGroupLocalSpec
	election *election.Election
	dummyInt *netlink.Link // for non-local announcements
	masqer   *masq.MasqDaemon

	// svcIngresses is a map from svcName to that Service's resource. We
	// need to ensure that we clean them up if the Service is deleted.
	svcIngresses map[string]*v1.Service

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
	c := masq.NewMasqConfig(false)

	return &announcer{
		logger:       l,
		myNode:       node,
		svcIngresses: map[string]*v1.Service{},
		masqer:       masq.NewMasqDaemon(c),
	}
}

// SetClient configures this announcer to use the provided client.
func (a *announcer) SetClient(client *k8s.Client) {
	a.client = client
}

func (a *announcer) SetConfig(cfg *purelbv1.Config) error {

	// if there's a "Local" agent config then we'll announce
	for _, agent := range cfg.Agents {
		a.config = &agent.Spec

		// Configure any Local specs.
		if spec := agent.Spec.Local; spec != nil {
			a.logger.Log("op", "setConfig", "spec", spec, "name", agent.Namespace+"/"+agent.Name)

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
		}

		// Configure any Egress specs.
		if agent.Spec.Egress != nil && len(agent.Spec.Egress.DontNAT) > 0 {
			c := masq.NewMasqConfig(false)
			c.NonMasqueradeCIDRs = agent.Spec.Egress.DontNAT
			a.masqer.UpdateConfig(*c)
		}
	}

	if a.config == nil || a.config.Local == nil {
		a.logger.Log("event", "noConfig")
	}

	// Initialize the ipset driver.
	if err := ipset.Init(); err != nil {
		return err
	}

	return nil
}

func (a *announcer) SetBalancer(svc *v1.Service, _ *v1.Endpoints) error {
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
	if a.config == nil || a.config.Local == nil {
		l.Log("event", "noConfig")
		return nil
	}

	// add the address to our announcement database
	a.svcIngresses[nsName] = svc

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		// validate the allocated address
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			l.Log("op", "setBalancer", "error", "invalid LoadBalancer IP", "ip", ingress.IP)
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
				if err := a.announceRemote(svc, lbIP); err != nil {
					retErr = err
				}
			}

		} else {
			// The user wants us to determine the "default" interface
			announceInt, err := defaultInterface(purelbv1.AddrFamily(lbIP))
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
				if err := a.announceRemote(svc, lbIP); err != nil {
					retErr = err
				}
			}
		}
	}

	// Return the most recent error
	return retErr
}

func (a *announcer) announceLocal(svc *v1.Service, announceInt netlink.Link, lbIP net.IP, lbIPNet net.IPNet) error {
	nsName := svc.Namespace + "/" + svc.Name
	l := log.With(a.logger, "service", nsName)

	// Local addresses do not support ExternalTrafficPolicyLocal
	// Set the service back to ExternalTrafficPolicyCluster if adding to local interface
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		l.Log("op", "setBalancer", "error", "ExternalTrafficPolicy Local not supported on local Interfaces, setting to Cluster")
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
		if err := a.deleteAddress(nsName, "ClusterLocal", lbIP); err != nil {
			return err
		}
	}

	// Get the node info since we'll use parts of it whether we win or
	// lose the election
	nodeMap, err := a.client.Nodes()
	if err != nil {
		return err
	}

	// See if we won the announcement election
	if winner := a.election.Winner(lbIP.String()); winner != a.myNode {
		// We lost the election so we'll withdraw any announcement that
		// we might have been making
		l.Log("msg", "notWinner", "node", a.myNode, "winner", winner, "memberCount", a.election.Memberlist.NumMembers(), "ip", lbIP)
		// Update egress routing rules. These send egress traffic to the
		// winning node for SNAT.
		if err := a.updateEgressNonWinner(svc, lbIP, nodeMap, winner, announceInt); err != nil {
			return err
		}
		return a.deleteAddress(nsName, "lostElection", lbIP)
	} else {

		// We won the election so we'll add the service address to our
		// node's default interface so linux will respond to ARP
		// requests for it.
		l.Log("msg", "Winner, winner, Chicken dinner", "node", a.myNode, "service", nsName, "memberCount", a.election.Memberlist.NumMembers())
		a.client.Infof(svc, "AnnouncingLocal", "Node %s announcing %s on interface %s", a.myNode, lbIP, announceInt.Attrs().Name)

		addNetwork(lbIPNet, announceInt)
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations[purelbv1.AnnounceAnnotation+AddrFamilyName(lbIP)] = a.myNode + "," + announceInt.Attrs().Name
		announcing.With(prometheus.Labels{
			"service": nsName,
			"node":    a.myNode,
			"ip":      lbIP.String(),
		}).Set(1)

		// If we're configured to do so, broadcast a GARP message to say
		// that we own the address.
		// Update SNAT egress rules
		if err := a.updateEgressWinner(svc, lbIP, nodeMap, winner); err != nil {
			return err
		}

		if a.config.Local.SendGratuitousARP {
			return sendGARP(announceInt.Attrs().Name, lbIP)
		}
	}

	return nil
}

func (a *announcer) announceRemote(svc *v1.Service, lbIP net.IP) error {
	l := log.With(a.logger, "service", svc.Name)
	nsName := svc.Namespace + "/" + svc.Name

	// The service address is non-local, i.e., it's not on the same
	// subnet as our default interface.

	// Should we announce?
	// No, if externalTrafficPolicy is Local && there's no ready local endpoint
	// Yes, in all other cases
	eps, err := a.healthyEndpoints(svc, a.myNode)
	if err != nil {
		return err
	}
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && len(eps) > 0 {
		l.Log("msg", "policyLocalNoEndpoints", "node", a.myNode, "service", nsName)

		// Clean up any egress config.
		if err := a.removeEgressWinner(nsName); err != nil {
			a.logger.Log("ERROR", err)
		}

		return a.deleteAddress(nsName, "noEndpoints", lbIP)
	}

	// add this address to the "dummy" interface so routing software
	// (e.g., bird) will announce routes for it
	poolName, gotName := svc.Annotations[purelbv1.PoolAnnotation]
	if gotName {
		allocPool := a.groups[poolName]
		l.Log("msg", "announcingNonLocal", "node", a.myNode, "service", nsName)
		a.client.Infof(svc, "AnnouncingNonLocal", "Announcing %s from node %s interface %s", lbIP, a.myNode, (*a.dummyInt).Attrs().Name)

		// Find the pool from which this address was allocated, which
		// gives us the subnet and aggregation that we need.
		pool, err := allocPool.PoolForAddress(lbIP)
		if err != nil {
			return err
		}

		// Add the address to the dummy interface.
		l.Log("msg", "subnet", "node", a.myNode, "service", nsName, "pool", pool)
		if err := addVirtualInt(lbIP, *a.dummyInt, pool.Subnet, pool.Aggregation); err != nil {
			return err
		}

		announcing.With(prometheus.Labels{
			"service": nsName,
			"node":    a.myNode,
			"ip":      lbIP.String(),
		}).Set(1)
	} else {
		return fmt.Errorf("PoolAnnotation missing from service %s", nsName)
	}

	// Update SNAT egress rules for this node only. We pass an empty
	// node map because we're going to SNAT traffic only for this node -
	// no other nodes should route traffic to us.
	if err := a.updateEgressWinner(svc, lbIP, map[string]v1.Node{}, ""); err != nil {
		return err
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
	svc, knowAboutIt := a.svcIngresses[nsName]
	if !knowAboutIt {
		a.logger.Log("msg", "Unknown LB, can't delete", "name", nsName)
		return nil
	}

	// delete this service from our announcement database
	delete(a.svcIngresses, nsName)

	// Withdraw any addresses.
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			return fmt.Errorf("invalid LoadBalancer IP: %s, belongs to %s", ingress.IP, nsName)
		}
		a.deleteAddress(nsName, reason, lbIP)
	}

	// Clean up any egress config.
	tableKeyRaw, gotEgress := svc.Annotations[purelbv1.RouteTableAnnotation]
	if !gotEgress {
		return nil
	}
	tableKey, err := strconv.Atoi(tableKeyRaw)
	if err != nil {
		return err
	}
	if err := a.removeEgressWinner(nsName); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := a.removeEgressNonWinner(tableKey, nsName); err != nil {
		a.logger.Log("ERROR", err)
		return err
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
	for otherSvc, svc := range a.svcIngresses {
		for _, announcedAddr := range svc.Status.LoadBalancer.Ingress {
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

// healthyEndpoints returns eps' healthy endpoints.
func (a *announcer) healthyEndpoints(svc *v1.Service, node string) ([]discoveryv1.Endpoint, error) {
	healthy := []discoveryv1.Endpoint{}

	slices, err := a.client.EndpointSlices(svc)
	if err != nil {
		return healthy, err
	}

	for _, slice := range slices.Items {
		for _, addr := range slice.Endpoints {
			healthy = append(healthy, addr)
		}
	}

	return healthy, nil
}

// updateEgressWinner updates the SNAT rules that implement our Egress
// on the locally-announcing node, i.e., the "winner". If egress is
// enabled we add a rule for each service pod running on this host and
// one for each other node that also hosts endpoints.
func (a *announcer) updateEgressWinner(svc *v1.Service, lbIP net.IP, nodes map[string]v1.Node, winnerNodeName string) error {
	nsName := svc.Namespace + "/" + svc.Name

	// Return if the service isn't configured for egress.
	if _, gotEgress := svc.Annotations[purelbv1.RouteTableAnnotation]; !gotEgress {
		return nil
	}

	// Return if the service has no healthy endpoints.
	ips, err := a.healthyEndpoints(svc, a.myNode)
	if err != nil {
		return err
	}
	if len(ips) < 1 {
		a.logger.Log("message", "no healthy endpoints", "service", nsName)
		return nil
	}

	if AddrFamily(lbIP) == nl.FAMILY_V6 {

		// Set up the service's iptables chains.
		if err := a.masqer.SyncChainIPv6(nsName, svc.Status.LoadBalancer.Ingress); err != nil {
			a.logger.Log("message", "error syncing masquerade rules", "error", err, "chain", nsName)
			return err
		}

		// Create the ipset and the postrouting jump
		chain := masq.ChainNameV6(nsName)
		if err := a.createIPSetV6(chain, ips, nodes, winnerNodeName); err != nil {
			return err
		}
		if err := a.masqer.EnsurePostroutingJumpIPv6(utiliptables.Chain(chain)); err != nil {
			a.logger.Log("message", "error syncing masquerade rules", "error", err, "chain", chain)
			return err
		}

	} else if AddrFamily(lbIP) == nl.FAMILY_V4 {

		// Set up the service's iptables chains.
		if err := a.masqer.SyncChainIPv4(nsName, svc.Status.LoadBalancer.Ingress); err != nil {
			a.logger.Log("message", "error syncing masquerade rules", "error", err, "chain", nsName)
			return err
		}

		// Create the ipset and the postrouting jump
		if err := a.createIPSetV4(nsName, ips, nodes, winnerNodeName); err != nil {
			return err
		}
		chain := masq.ChainNameV4(nsName)
		if err := a.masqer.EnsurePostroutingJump(utiliptables.Chain(chain)); err != nil {
			a.logger.Log("message", "error syncing masquerade rules", "error", err, "chain", chain)
			return err
		}
	}

	return nil
}

// updateEgressNonWinner updates the routes that send egress traffic over
// to the "winner" to be SNAT'ed.
func (a *announcer) updateEgressNonWinner(svc *v1.Service, lbIP net.IP, nodes map[string]v1.Node, winnerNodeName string, announceInt netlink.Link) error {
	nsName := svc.Namespace + "/" + svc.Name

	// Return if we're not configured for egress.
	if a.config.Egress == nil {
		return nil
	}

	// Return if the service isn't annotated for egress.
	tableKeyRaw, gotEgress := svc.Annotations[purelbv1.RouteTableAnnotation]
	if !gotEgress {
		return nil
	}
	tableKey, err := strconv.Atoi(tableKeyRaw)
	if err != nil {
		return err
	}

	// // Log rules
	// rules, err := netlink.RuleList(nl.FAMILY_ALL)
	// if err != nil {
	// 	return err
	// }
	// for _, rule := range rules {
	// 	bytes, _ := json.Marshal(rule)
	// 	a.logger.Log("rule", string(bytes))
	// }

	// Empty our routing table and rules.
	err = a.removeEgressNonWinner(tableKey, nsName)
	if err != nil {
		a.logger.Log("ERROR", err)
		return err
	}

	if AddrFamily(lbIP) == nl.FAMILY_V6 {
		// Add a default route to our routing table
		winner := nodes[winnerNodeName]
		winnerAddr := net.ParseIP(*nodeAddress(winner, nl.FAMILY_V6))
		if winnerAddr == nil {
			return fmt.Errorf("can't determine node %s V6 address", winner.Name)
		}
		defaultRoute := netlink.Route{
			Table:     tableKey,
			Scope:     netlink.SCOPE_UNIVERSE,
			LinkIndex: announceInt.Attrs().Index,
			Gw:        winnerAddr,
			Protocol:  4,
		}
		err = netlink.RouteAdd(&defaultRoute)
		if err != nil {
			a.logger.Log("op", "addDefaultRoute", "ERROR", err)
		}

		// Add a route for the local pod network
		cni0, err := netlink.LinkByName(a.config.Egress.CNIInterface)
		if err != nil {
			a.logger.Log("op", "addCNI0Route", "ERROR", err, "name", a.config.Egress.CNIInterface)
		}
		me := nodes[a.myNode]
		_, myCidr, err := net.ParseCIDR(me.Spec.PodCIDR)
		if err != nil {
			a.logger.Log("op", "addCNI0Route", "ERROR", err)
		}
		cidrRoute := netlink.Route{
			Dst:       myCidr,
			Table:     tableKey,
			Scope:     netlink.SCOPE_UNIVERSE,
			LinkIndex: cni0.Attrs().Index,
			Protocol:  4,
		}
		err = netlink.RouteAdd(&cidrRoute)
		if err != nil {
			a.logger.Log("op", "addPodCIDRRoute", "ERROR", err)
		}

		// Add rules to jump to our routing table.
		ips, err := a.healthyEndpoints(svc, a.myNode)
		if err != nil {
			a.logger.Log("op", "addPodCIDRRoute", "ERROR", err)
		}
		for _, ep := range ips {
			for _, addr := range ep.Addresses {
				ip := net.ParseIP(addr)
				if ip == nil {
					a.logger.Log("message", "invalid endpoint address, can't add rule", "service", nsName, "endpoint", addr)
					continue
				}
				if AddrFamily(ip) == nl.FAMILY_V6 {
					rule := netlink.NewRule()
					rule.Family = nl.FAMILY_V6
					rule.Table = tableKey
					_, rule.Src, err = net.ParseCIDR(addr + "/128")
					if err != nil {
						a.logger.Log("op", "addEndpointRule", "ERROR", err, "ip", addr)
						continue
					}
					err = netlink.RuleAdd(rule)
					if err != nil {
						a.logger.Log("op", "addEndpointRule", "ERROR", err)
					}
				}
			}
		}

		// Create the ipset and the postrouting jump
		if err := a.createIPSetV6(nsName, ips, nodes, winnerNodeName); err != nil {
			return err
		}
		chainName := masq.ChainNameV6(nsName)
		if err := a.masqer.EnsurePostroutingReturnIPv6(utiliptables.Chain(chainName)); err != nil {
			a.logger.Log("message", "error syncing masquerade rules", "error", err, "chain", chainName)
			return err
		}

	} else if AddrFamily(lbIP) == nl.FAMILY_V4 {

		// Add a default route to our routing table
		winner := nodes[winnerNodeName]
		winnerAddr := net.ParseIP(*nodeAddress(winner, nl.FAMILY_V4))
		if winnerAddr == nil {
			return fmt.Errorf("can't determine node %s address", winner.Name)
		}
		defaultRoute := netlink.Route{
			Table:     tableKey,
			Scope:     netlink.SCOPE_UNIVERSE,
			LinkIndex: announceInt.Attrs().Index,
			Gw:        winnerAddr,
			Protocol:  4,
		}
		err = netlink.RouteAdd(&defaultRoute)
		if err != nil {
			a.logger.Log("op", "addDefaultRoute", "ERROR", err)
		}

		// Add a route for the local pod network
		cni0, err := netlink.LinkByName(a.config.Egress.CNIInterface)
		if err != nil {
			a.logger.Log("op", "addCNI0Route", "ERROR", err, "name", a.config.Egress.CNIInterface)
		}
		me := nodes[a.myNode]
		_, myCidr, err := net.ParseCIDR(me.Spec.PodCIDR)
		if err != nil {
			a.logger.Log("op", "addCNI0Route", "ERROR", err)
		}
		cidrRoute := netlink.Route{
			Dst:       myCidr,
			Table:     tableKey,
			Scope:     netlink.SCOPE_UNIVERSE,
			LinkIndex: cni0.Attrs().Index,
			Protocol:  4,
		}
		err = netlink.RouteAdd(&cidrRoute)
		if err != nil {
			a.logger.Log("op", "addPodCIDRRoute", "ERROR", err)
		}

		// Add rules to jump to our routing table.
		ips, err := a.healthyEndpoints(svc, a.myNode)
		if err != nil {
			a.logger.Log("op", "addPodCIDRRoute", "ERROR", err)
		}
		for _, ep := range ips {
			for _, addr := range ep.Addresses {
				ip := net.ParseIP(addr)
				if ip == nil {
					a.logger.Log("message", "invalid endpoint address, can't add rule", "service", nsName, "endpoint", addr)
					continue
				}
				if AddrFamily(ip) == nl.FAMILY_V4 {
					rule := netlink.NewRule()
					rule.Family = nl.FAMILY_V4
					rule.Table = tableKey
					_, rule.Src, err = net.ParseCIDR(addr + "/32")
					if err != nil {
						a.logger.Log("op", "addEndpointRule", "ERROR", err, "ip", addr)
						continue
					}
					err = netlink.RuleAdd(rule)
					if err != nil {
						a.logger.Log("op", "addEndpointRule", "ERROR", err)
					}
				}
			}
		}
		// Create the ipset and the postrouting jump
		if err := a.createIPSetV4(nsName, ips, nodes, winnerNodeName); err != nil {
			return err
		}
		chainName := masq.ChainNameV4(nsName)
		if err := a.masqer.EnsurePostroutingReturn(utiliptables.Chain(chainName)); err != nil {
			a.logger.Log("message", "error syncing masquerade rules", "error", err, "chain", chainName)
			return err
		}
	}

	return nil
}

// removeEgressWinner cleans up one service's worth of ip chains and
// jump rules on a winner node.
func (a *announcer) removeEgressWinner(nsName string) error {
	chainV6 := masq.ChainNameV6(nsName)
	chainV4 := masq.ChainNameV4(nsName)
	if err := a.masqer.DeletePostroutingJumpIPv6(utiliptables.Chain(chainV6)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := a.masqer.DeletePostroutingReturnIPv6(utiliptables.Chain(chainV6)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := a.masqer.DeletePostroutingJump(utiliptables.Chain(chainV4)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := a.masqer.DeletePostroutingReturn(utiliptables.Chain(chainV4)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := ipset.Destroy(chainV6); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := ipset.Destroy(chainV4); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := a.masqer.DeleteChains(nsName); err != nil {
		a.logger.Log("ERROR", err)
	}

	return nil
}

// removeEgressNonWinner cleans up one service's worth of ip rules and
// routes on a non-winner node.
func (a *announcer) removeEgressNonWinner(tableKey int, nsName string) error {
	chainV6 := masq.ChainNameV6(nsName)
	chainV4 := masq.ChainNameV4(nsName)
	if err := a.masqer.DeletePostroutingReturnIPv6(utiliptables.Chain(chainV6)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := a.masqer.DeletePostroutingReturn(utiliptables.Chain(chainV4)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := ipset.Destroy(chainV6); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := ipset.Destroy(chainV4); err != nil {
		a.logger.Log("ERROR", err)
	}

	// Remove any rules that point to this table.
	rules, err := netlink.RuleList(nl.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.Table == tableKey {
			err := netlink.RuleDel(&rule)
			if err != nil {
				a.logger.Log("ERROR", err)
			}
		}
	}

	// Empty the routing table.
	routes, err := netlink.RouteListFiltered(nl.FAMILY_ALL, &netlink.Route{Table: tableKey}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if err := netlink.RouteDel(&route); err != nil {
			a.logger.Log("op", "deleteRoute", "ERROR", err)
		}
	}

	return nil
}

func (a *announcer) createIPSetV4(nsName string, ips []discoveryv1.Endpoint, nodes map[string]v1.Node, winnerNodeName string) error {
	// Destroy our ipsets - it's easier then synchronizing them
	// incrementally.
	chain := masq.ChainNameV4(nsName)
	var destroyErr error
	if err := ipset.Destroy(chain); err != nil {
		destroyErr = err
		a.logger.Log("op", "cleanup v4 ipset", "error", err)
	}
	if destroyErr != nil {
		return destroyErr
	}

	// Create an ipset which will contain this service's endpoints
	if err := ipset.Create(chain); err != nil {
		a.logger.Log("op", "create v4 ipset", "error", err)
		return err
	}

	// Add each endpoint address to the ipset so they'll get SNATed
	for _, endpoint := range ips {
		for _, addr := range endpoint.Addresses {
			ip := net.ParseIP(addr)
			if ip == nil {
				a.logger.Log("message", "invalid endpoint address, can't NAT", "service", nsName, "endpoint", addr)
				continue
			}
			if AddrFamily(ip) == nl.FAMILY_V4 {
				if err := ipset.Add(chain, addr); err != nil {
					a.logger.Log("error", err, "service", nsName, "endpoint", ip)
					continue
				}
			}
		}
	}

	return nil
}

func (a *announcer) createIPSetV6(nsName string, ips []discoveryv1.Endpoint, nodes map[string]v1.Node, winnerNodeName string) error {
	// Destroy our ipsets - it's easier then synchronizing them
	// incrementally.
	chain := masq.ChainNameV6(nsName)
	var destroyErr error
	if err := ipset.Destroy(chain); err != nil {
		destroyErr = err
		a.logger.Log("op", "cleanup v6 ipset", "error", err)
	}
	if destroyErr != nil {
		return destroyErr
	}

	// Create ipsets which will contain this service's endpoints on this
	// host.
	var createErr error
	if err := ipset.Create(chain, ipset.OptIPv6()); err != nil {
		createErr = err
		a.logger.Log("op", "create v6 ipset", "error", err)
	}
	if createErr != nil {
		return createErr
	}

	// Add each endpoint address to the IP set so they'll get SNATed
	for _, endpoint := range ips {
		for _, addr := range endpoint.Addresses {
			ip := net.ParseIP(addr)
			if ip == nil {
				a.logger.Log("message", "invalid endpoint address, can't NAT", "service", nsName, "endpoint", addr)
				continue
			}
			if AddrFamily(ip) == nl.FAMILY_V6 {
				if err := ipset.Add(chain, addr); err != nil {
					a.logger.Log("error", err, "service", nsName, "endpoint", ip)
					continue
				}
			}
		}
	}
	// Add each *non-winner* node IP to the IP set so we'll SNAT egress
	// traffic that they route to us.
	for name, node := range nodes {
		if name == winnerNodeName {
			continue
		}
		a.logger.Log("message", "will SNAT", "service", nsName, "node", *nodeAddress(node, nl.FAMILY_V6), "chain", nsName)
		if err := ipset.Add(chain, *nodeAddress(node, nl.FAMILY_V6)); err != nil {
			return err
		}
	}

	return nil
}
