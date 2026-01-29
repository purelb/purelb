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
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	purelbv1 "purelb.io/pkg/apis/purelb/v1"

	"github.com/go-kit/log"
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
	dummyInt netlink.Link // for non-local announcements

	// svcIngresses is a map from svcName to that Service's
	// Ingresses. Note that we may or may not advertise all of them
	// because we might lose an election or not have any active
	// endpoints, but in any case we need to ensure that we clean them
	// up if the Service is deleted.
	svcIngresses map[string][]v1.LoadBalancerIngress

	// localNameRegex is the pattern that we use to determine if an
	// interface is local or not.
	localNameRegex *regexp.Regexp

	// addressRenewals tracks addresses that need periodic renewal.
	// Key format: "namespace/servicename:ip" to support shared IPs.
	addressRenewals sync.Map // map[string]*addressRenewal
}

// addressRenewal holds the state needed to periodically refresh an address
// before its lifetime expires.
type addressRenewal struct {
	ipNet    net.IPNet
	link     netlink.Link
	opts     AddressOptions
	timer    *time.Timer
	interval time.Duration
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

			// stash the local ServiceGroup configs
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

			// The dummy interface is set up so we can set the config which
			// will allow announcements to happen.
			a.config = spec

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

func (a *announcer) SetBalancer(svc *v1.Service, epSlices []*discoveryv1.EndpointSlice) error {
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
				if err := a.announceRemote(svc, epSlices, a.dummyInt, lbIP); err != nil {
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
				if err := a.announceRemote(svc, epSlices, a.dummyInt, lbIP); err != nil {
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

	// Local addresses do not support ExternalTrafficPolicyLocal unless the override annotation is present.
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		if _, hasOverride := svc.Annotations[purelbv1.AllowLocalAnnotation]; hasOverride {

			// The user has added the override annotation so we'll allow
			// Local policy but warn them.
			l.Log("op", "setBalancer", "error", "ExternalTrafficPolicy override annotation found, will allow Local policy. Incoming traffic will NOT be forwarded inter-node. This is not a recommended configuration; please see the PureLB docs for more info.")
			a.client.Infof(svc, "LocalOverride", "ExternalTrafficPolicy override annotation found, will allow Local policy. Incoming traffic will NOT be forwarded inter-node. This is not a recommended configuration; please see the PureLB docs for more info.")

		} else {

			// There's no override annotation so we'll switch back to
			// "Cluster"
			l.Log("op", "setBalancer", "error", "ExternalTrafficPolicy Local not supported on local Interfaces, setting to Cluster. Please see the PureLB docs for info on why we do this, and how to override this policy.")
			a.client.Infof(svc, "LocalOverride", "ExternalTrafficPolicy Local not supported on local Interfaces, setting to Cluster. Please see the PureLB docs for info on why we do this, and how to override this policy.")
			// Set the service back to ExternalTrafficPolicyCluster if adding to local interface
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
			if err := a.deleteAddress(nsName, "ClusterLocal", lbIP); err != nil {
				return err
			}
		}
	}

	// See if we won the announcement election
	if winner := a.election.Winner(lbIP.String()); winner != a.myNode {
		// We lost the election so we'll withdraw any announcement that
		// we might have been making
		l.Log("msg", "notWinner", "node", a.myNode, "winner", winner, "service", nsName, "memberCount", a.election.Memberlist.NumMembers())
		return a.deleteAddress(nsName, "lostElection", lbIP)
	}

	// We won the election so we'll add the service address to our
	// node's default interface so linux will respond to ARP
	// requests for it.
	l.Log("msg", "Winner, winner, Chicken dinner", "node", a.myNode, "service", nsName, "memberCount", a.election.Memberlist.NumMembers())
	a.client.Infof(svc, "AnnouncingLocal", "Node %s announcing %s on interface %s", a.myNode, lbIP, announceInt.Attrs().Name)

	opts := a.getLocalAddressOptions()
	if err := addNetworkWithOptions(lbIPNet, announceInt, opts); err != nil {
		return err
	}
	a.scheduleRenewal(nsName, lbIPNet, announceInt, opts)

	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[purelbv1.AnnounceAnnotation+addrFamilyName(lbIP)] = a.myNode + "," + announceInt.Attrs().Name
	announcing.With(prometheus.Labels{
		"service": nsName,
		"node":    a.myNode,
		"ip":      lbIP.String(),
	}).Set(1)

	// If we're configured to do so, broadcast a GARP message to say
	// that we own the address.
	if a.config.SendGratuitousARP {
		return sendGARP(announceInt.Attrs().Name, lbIP)
	}

	return nil
}

func (a *announcer) announceRemote(svc *v1.Service, epSlices []*discoveryv1.EndpointSlice, announceInt netlink.Link, lbIP net.IP) error {
	l := log.With(a.logger, "service", svc.Name)
	nsName := svc.Namespace + "/" + svc.Name

	// The service address is non-local, i.e., it's not on the same
	// subnet as our default interface.

	// Should we announce?
	// No, if externalTrafficPolicy is Local && there's no ready local endpoint
	// Yes, in all other cases
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		// Debug: log endpoint slice info
		sliceCount := len(epSlices)
		var endpointNodes []string
		for _, slice := range epSlices {
			if slice == nil {
				continue
			}
			for _, ep := range slice.Endpoints {
				if ep.NodeName != nil {
					isReady := ep.Conditions.Ready != nil && *ep.Conditions.Ready
					isServing := ep.Conditions.Serving != nil && *ep.Conditions.Serving
					endpointNodes = append(endpointNodes, fmt.Sprintf("%s(ready=%v,serving=%v)", *ep.NodeName, isReady, isServing))
				}
			}
		}
		hasEndpoint := nodeHasHealthyEndpoint(epSlices, a.myNode)
		l.Log("msg", "etpLocalCheck", "node", a.myNode, "sliceCount", sliceCount, "endpointNodes", endpointNodes, "hasHealthyEndpoint", hasEndpoint)

		if !hasEndpoint {
			l.Log("msg", "policyLocalNoEndpoints", "node", a.myNode, "service", nsName)
			return a.deleteAddress(nsName, "noEndpoints", lbIP)
		}
	}

	// Find the group from which this address was allocated, which
	// gives us the subnet and aggregation that we need.
	groupName, group, err := poolFor(a.groups, lbIP)
	if err != nil {
		return err
	}

	l.Log("msg", "announcingNonLocal", "node", a.myNode, "service", nsName, "interface", a.dummyInt.Attrs().Name, "group", groupName)
	a.client.Infof(svc, "AnnouncingNonLocal", "Announcing %s from node %s interface %s", lbIP, a.myNode, a.dummyInt.Attrs().Name)

	// Add this address to the dummy interface so routing software
	// (e.g., bird) will announce routes for it.
	l.Log("msg", "subnet", "node", a.myNode, "service", nsName, "pool", group)
	opts := a.getDummyAddressOptions()
	lbIPNet, err := addVirtualInt(lbIP, a.dummyInt, group.Subnet, group.Aggregation, opts)
	if err != nil {
		return err
	}
	a.scheduleRenewal(nsName, lbIPNet, a.dummyInt, opts)

	announcing.With(prometheus.Labels{
		"service": nsName,
		"node":    a.myNode,
		"ip":      lbIP.String(),
	}).Set(1)

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
	// Cancel any pending renewal timer for this service/IP
	a.cancelRenewal(nsName, svcAddr.String())

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
	// Cancel all renewal timers to prevent goroutine leaks
	a.addressRenewals.Range(func(key, val interface{}) bool {
		val.(*addressRenewal).timer.Stop()
		return true
	})

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
// healthy endpoint. An endpoint is considered healthy if it is Ready
// OR Serving (for graceful termination support) across ALL EndpointSlices.
//
// EndpointSlices may be split by port, so the same endpoint IP may appear
// in multiple slices. We track readiness per-IP and require ALL appearances
// to be healthy for the endpoint to be considered fully healthy.
func nodeHasHealthyEndpoint(slices []*discoveryv1.EndpointSlice, node string) bool {
	// Track per-IP readiness across all slices
	// Same IP may appear in multiple slices with different ready states
	ready := map[string]bool{}

	for _, slice := range slices {
		if slice == nil {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.NodeName == nil || *endpoint.NodeName != node {
				continue
			}

			// Check Ready OR Serving (for graceful termination)
			isHealthy := (endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready) ||
				(endpoint.Conditions.Serving != nil && *endpoint.Conditions.Serving)

			for _, addr := range endpoint.Addresses {
				if existing, ok := ready[addr]; ok {
					// If ANY slice shows this endpoint as not healthy, mark it not healthy
					// This preserves the original semantics where all ports must be ready
					ready[addr] = existing && isHealthy
				} else {
					ready[addr] = isHealthy
				}
			}
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

// poolFor returns the name of the ServiceGroupLocalSpec that contains
// lbIP. If error is not nil then no pool was found
func poolFor(groups map[string]*purelbv1.ServiceGroupLocalSpec, lbIP net.IP) (string, *purelbv1.ServiceGroupAddressPool, error) {
	for groupName, group := range groups {
		if _, err := groups[groupName].PoolForAddress(lbIP); err == nil {
			pool, err := group.PoolForAddress(lbIP)
			if err != nil {
				return "", nil, err
			}
			return groupName, pool, nil
		}
	}
	return "", nil, fmt.Errorf("Can't find pool for address %+v", lbIP)
}

// renewalKey generates the map key for a service's address renewal.
// Format: "namespace/servicename:ip" to support shared IPs across services.
func renewalKey(svcName, ip string) string {
	return svcName + ":" + ip
}

// scheduleRenewal sets up a timer to periodically refresh an address before
// its lifetime expires. This is necessary because addresses with finite
// lifetimes will be removed by the kernel when they expire.
func (a *announcer) scheduleRenewal(svcName string, lbIPNet net.IPNet, link netlink.Link, opts AddressOptions) {
	if opts.ValidLft == 0 {
		return // Permanent address, no renewal needed
	}

	// Renew at 50% of lifetime, with a minimum of 30 seconds
	interval := time.Duration(opts.ValidLft/2) * time.Second
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}

	key := renewalKey(svcName, lbIPNet.IP.String())

	renewal := &addressRenewal{
		ipNet:    lbIPNet,
		link:     link,
		opts:     opts,
		interval: interval,
	}

	// Cancel existing timer if any
	if old, loaded := a.addressRenewals.LoadAndDelete(key); loaded {
		old.(*addressRenewal).timer.Stop()
	}

	renewal.timer = time.AfterFunc(interval, func() {
		a.renewAddress(key)
	})

	a.addressRenewals.Store(key, renewal)
	a.logger.Log("op", "scheduleRenewal", "key", key, "interval", interval)
}

// renewAddress refreshes an address's lifetime. Called by the renewal timer.
func (a *announcer) renewAddress(key string) {
	val, ok := a.addressRenewals.Load(key)
	if !ok {
		return // Cancelled, nothing to do
	}
	renewal := val.(*addressRenewal)

	if err := addNetworkWithOptions(renewal.ipNet, renewal.link, renewal.opts); err != nil {
		a.logger.Log("op", "renewAddress", "key", key, "error", err)
		// Still reschedule - transient errors shouldn't stop renewal
	} else {
		a.logger.Log("op", "renewAddress", "key", key, "msg", "renewed", "next", renewal.interval)
	}

	// Reschedule for next renewal
	renewal.timer = time.AfterFunc(renewal.interval, func() {
		a.renewAddress(key)
	})
}

// cancelRenewal stops the renewal timer for a specific service/IP combination.
func (a *announcer) cancelRenewal(svcName, ip string) {
	key := renewalKey(svcName, ip)
	if val, loaded := a.addressRenewals.LoadAndDelete(key); loaded {
		val.(*addressRenewal).timer.Stop()
		a.logger.Log("op", "cancelRenewal", "key", key)
	}
}

// getLocalAddressOptions returns the AddressOptions for addresses added to
// the local interface. Defaults to finite lifetime (300s) with NoPrefixRoute
// to prevent CNI plugins like Flannel from selecting VIPs as node addresses.
func (a *announcer) getLocalAddressOptions() AddressOptions {
	opts := AddressOptions{
		ValidLft:      300, // default 5 minutes
		PreferedLft:   300,
		NoPrefixRoute: true,
	}

	if a.config != nil && a.config.AddressConfig != nil && a.config.AddressConfig.LocalInterface != nil {
		cfg := a.config.AddressConfig.LocalInterface
		if cfg.ValidLifetime != nil {
			v := *cfg.ValidLifetime
			// Enforce minimum 60s if non-zero to prevent DoS via tiny lifetime
			if v > 0 && v < 60 {
				v = 60
			}
			opts.ValidLft = v
			opts.PreferedLft = v // default preferred to same as valid
		}
		if cfg.PreferredLifetime != nil {
			opts.PreferedLft = *cfg.PreferredLifetime
		}
		if cfg.NoPrefixRoute != nil {
			opts.NoPrefixRoute = *cfg.NoPrefixRoute
		}
	}

	// Ensure PreferredLft <= ValidLft
	if opts.PreferedLft > opts.ValidLft {
		opts.PreferedLft = opts.ValidLft
	}

	return opts
}

// getDummyAddressOptions returns the AddressOptions for addresses added to
// the dummy interface. Defaults to permanent (0) since these don't conflict
// with CNI plugins and permanent addresses provide routing stability.
func (a *announcer) getDummyAddressOptions() AddressOptions {
	opts := AddressOptions{
		ValidLft:      0, // default permanent
		PreferedLft:   0,
		NoPrefixRoute: false,
	}

	if a.config != nil && a.config.AddressConfig != nil && a.config.AddressConfig.DummyInterface != nil {
		cfg := a.config.AddressConfig.DummyInterface
		if cfg.ValidLifetime != nil {
			v := *cfg.ValidLifetime
			// Enforce minimum 60s if non-zero
			if v > 0 && v < 60 {
				v = 60
			}
			opts.ValidLft = v
			opts.PreferedLft = v
		}
		if cfg.PreferredLifetime != nil {
			opts.PreferedLft = *cfg.PreferredLifetime
		}
		if cfg.NoPrefixRoute != nil {
			opts.NoPrefixRoute = *cfg.NoPrefixRoute
		}
	}

	// Ensure PreferredLft <= ValidLft
	if opts.PreferedLft > opts.ValidLft {
		opts.PreferedLft = opts.ValidLft
	}

	return opts
}
