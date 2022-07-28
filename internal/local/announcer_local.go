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
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"regexp"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/json"

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

const (
	CHAIN_PREFIX string = "PURELB-"
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
	c := masq.NewMasqConfig(false)

	return &announcer{
		logger:       l,
		myNode:       node,
		svcIngresses: map[string][]v1.LoadBalancerIngress{},
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
	if a.config == nil || a.config.Local == nil {
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
				if err := a.announceLocal(svc, endpoints, localif, lbIP, lbIPNet); err != nil {
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
				if err := a.announceLocal(svc, endpoints, defaultif, lbIP, lbIPNet); err != nil {
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

func (a *announcer) announceLocal(svc *v1.Service, endpoints *v1.Endpoints, announceInt netlink.Link, lbIP net.IP, lbIPNet net.IPNet) error {
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

	// Get the node info since we'll use parts of it whether we win or
	// lose the election
	nodeMap, err := a.client.Nodes()
	if err != nil {
		return err
	}

	// We can announce the address if we win the election
	if winner := a.election.Winner(lbIP.String()); winner == a.myNode {

		// we won the election so we'll add the service address to our
		// node's default interface so linux will respond to ARP
		// requests for it
		l.Log("msg", "Winner, winner, Chicken dinner", "node", a.myNode, "service", nsName, "memberCount", a.election.Memberlist.NumMembers())
		a.client.Infof(svc, "AnnouncingLocal", "Node %s announcing %s on interface %s", a.myNode, lbIP, announceInt.Attrs().Name)

		addNetwork(lbIPNet, announceInt)
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations[purelbv1.AnnounceAnnotation+addrFamilyName(lbIP)] = a.myNode + "," + announceInt.Attrs().Name
		announcing.With(prometheus.Labels{
			"service": nsName,
			"node":    a.myNode,
			"ip":      lbIP.String(),
		}).Set(1)

		// Update SNAT egress rules
		if err := a.updateEgressWinner(l, svc, endpoints, nodeMap, winner); err != nil {
			return err
		}
	} else {
		// Update egress routing rules. These send egress traffic to the
		// winning node for SNAT.
		if err := a.updateEgressNonWinner(l, svc, endpoints, nodeMap, winner, announceInt); err != nil {
			return err
		}

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
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && len(healthyEndpoints(endpoints, a.myNode)) > 0 {
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

	// Clean up any egress config.
	chain := utiliptables.Chain(chainName(nsName))
	if err := a.masqer.DeletePostroutingJump(chain); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := ipset.Flush(string(chain)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := ipset.Destroy(string(chain)); err != nil {
		a.logger.Log("ERROR", err)
	}
	if err := a.masqer.DeleteChain(chain); err != nil {
		a.logger.Log("ERROR", err)
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

// healthyEndpoints returns eps' healthy endpoints.
func healthyEndpoints(eps *v1.Endpoints, node string) []v1.EndpointAddress {
	ready := map[v1.EndpointAddress]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if _, ok := ready[ep]; !ok {
				// Only set true if nothing else has expressed an
				// opinion. This means that false will take precedence
				// if there's any unready ports for a given endpoint.
				ready[ep] = true
			}
		}
		for _, ep := range subset.NotReadyAddresses {
			ready[ep] = false
		}
	}

	healthy := []v1.EndpointAddress{}
	for addr, r := range ready {
		if r {
			healthy = append(healthy, addr)
		}
	}
	return healthy
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

// updateEgressWinner updates the SNAT rules that implement our Egress
// on the locally-announcing node, i.e., the "winner". If egress is
// enabled we add a rule for each service pod running on this host and
// one for each other node that also hosts endpoints.
func (a *announcer) updateEgressWinner(l log.Logger, svc *v1.Service, endpoints *v1.Endpoints, nodes map[string]v1.Node, winnerNodeName string) error {
	nsName := svc.Namespace + "/" + svc.Name

	// Return if the service isn't configured for egress.
	if _, gotEgress := svc.Annotations[purelbv1.RouteTableAnnotation]; !gotEgress {
		return nil
	}

	// Return if the service has no healthy endpoints.
	ips := healthyEndpoints(endpoints, a.myNode)
	if len(ips) < 1 {
		l.Log("message", "no healthy endpoints", "service", nsName)
		return nil
	}

	// Set up the service's iptables chain.
	chain := utiliptables.Chain(chainName(nsName))
	if err := a.masqer.SyncChain(chain, svc.Status.LoadBalancer.Ingress[0].IP); err != nil {
		l.Log("message", "error syncing masquerade rules", "error", err, "chain", chain)
		return err
	}

	// Create an ipset containing this service's endpoints on this host.
	if err := ipset.Destroy(string(chain)); err != nil {
		return err
	}
	if err := ipset.Create(string(chain)); err != nil {
		return err
	}
	// Add each endpoint address to the IP set so they'll get SNATed
	for _, ip := range ips {
		l.Log("message", "will SNAT", "service", nsName, "endpoint", ip.IP, "chain", string(chain))
		if err := ipset.Add(string(chain), ip.IP); err != nil {
			return err
		}
	}
	// Add each *non-winner* node IP to the IP set so we'll SNAT egress
	// traffic that they route to us.
	for name, node := range nodes {
		if name == winnerNodeName {
			continue
		}
		l.Log("message", "will SNAT", "service", nsName, "node", *nodeAddress(node), "chain", string(chain))
		if err := ipset.Add(string(chain), *nodeAddress(node)); err != nil {
			return err
		}
	}

	if err := a.masqer.EnsurePostroutingJump(chain, "no-op"); err != nil {
		l.Log("message", "error syncing masquerade rules", "error", err, "chain", chain)
		return err
	}

	return nil
}

// updateEgressNonWinner updates the routes that send egress traffic over
// to the "winner" to be SNAT'ed.
func (a *announcer) updateEgressNonWinner(l log.Logger, svc *v1.Service, endpoints *v1.Endpoints, nodes map[string]v1.Node, winnerNodeName string, announceInt netlink.Link) error {
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

	// Log rules
	rules, err := netlink.RuleList(nl.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		bytes, _ := json.Marshal(rule)
		l.Log("rule", string(bytes))
	}

	// Empty our routing table and rules.
	err = a.removeEgressNonWinner(l, tableKey)
	if err != nil {
		l.Log("ERROR", err)
		return err
	}

	// Add a default route to our routing table
	winner := nodes[winnerNodeName]
	winnerAddr := net.ParseIP(*nodeAddress(winner))
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
		l.Log("op", "addDefaultRoute", "ERROR", err)
	}

	// Add a route for the local pod network
	cni0, err := netlink.LinkByName(a.config.Egress.CNIInterface)
	if err != nil {
		l.Log("op", "addCNI0Route", "ERROR", err, "name", a.config.Egress.CNIInterface)
	}
	me := nodes[a.myNode]
	_, myCidr, err := net.ParseCIDR(me.Spec.PodCIDR)
	if err != nil {
		l.Log("op", "addCNI0Route", "ERROR", err)
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
		l.Log("op", "addPodCIDRRoute", "ERROR", err)
	}

	// Add rules to jump to our routing table.
	ips := healthyEndpoints(endpoints, a.myNode)
	for _, ep := range ips {
		rule := netlink.NewRule()
		rule.Table = tableKey
		_, rule.Src, err = net.ParseCIDR(ep.IP + "/32")
		if err != nil {
			l.Log("op", "addEndpointRule", "ERROR", err, "ip", ep.IP)
			continue
		}
		err = netlink.RuleAdd(rule)
		if err != nil {
			l.Log("op", "addEndpointRule", "ERROR", err)
		}
	}

	return nil
}

// removeEgressNonWinner cleans up one service's worth of ip rules and
// routes on a non-winner node.
func (a *announcer) removeEgressNonWinner(l log.Logger, tableKey int) error {
	// Remove any rules that point to this table.
	rules, err := netlink.RuleList(nl.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.Table == tableKey {
			err := netlink.RuleDel(&rule)
			if err != nil {
				l.Log("ERROR", err)
			}
		}
	}

	// Empty the routing table.
	routes, err := netlink.RouteListFiltered(nl.FAMILY_ALL, &netlink.Route{Table: tableKey}, netlink.RT_FILTER_TABLE)
	if err != nil {
		l.Log("ERROR", err)
		return err
	}
	for _, route := range routes {
		bytes, _ := json.Marshal(route)
		l.Log("route", string(bytes))

		if err := netlink.RouteDel(&route); err != nil {
			l.Log("op", "deleteDefaultRoute", "ERROR", err)
		}
	}

	return nil
}

// chainName takes serviceName and returns the associated 16 character
// hash. This is computed by hashing (sha256) then encoding to base32
// and truncating to 16 chars. We do this because IPTables Chain Names
// must be <= 28 chars long, and the longer they are the harder they
// are to read.
//
// This is lifted from proxier.go:portProtoHash() in the k8s source.
func chainName(serviceName string) string {
	hash := sha256.Sum256([]byte(serviceName))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return CHAIN_PREFIX + encoded[:16]
}
