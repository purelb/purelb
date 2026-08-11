// Copyright 2020-2026 Acnodal Inc.
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
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/lbnodeagent"
	"purelb.io/internal/logging"
	"purelb.io/internal/netutil"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
)

// announcerConfig is everything one SetConfig delivery produces, held as
// an immutable unit.
//
// These five values used to be independent mutable fields on announcer,
// written by SetConfig on the CR-controller goroutine and read by
// SetBalancer's call tree on the service-sync goroutine with no
// synchronization. That raced two ways: SetConfig blanked the spec
// pointer before rebuilding it, so a reader between the nil-guard and any
// later dereference took a nil-pointer panic; and groups/remoteGroups
// were assigned empty and then filled, so a reader iterating them hit
// "concurrent map iteration and map write" -- a Go runtime fatal error,
// which no recover() can catch.
//
// Publishing them together as one immutable snapshot means a reader sees
// generation N or N+1, never a mixture and never a half-built map. This
// is the same discipline the allocator uses for allocatorState.
//
// Nothing in here may be mutated after the atomic Store that publishes
// it. Build a fresh one instead; deliveries are rare (CR changes plus the
// informer resync) and the snapshot is a handful of words.
type announcerConfig struct {
	// spec is the Local spec of the LBNodeAgent governing this node.
	// Never nil in a published snapshot -- an absent or rejected config
	// is represented by storing a nil *announcerConfig, not by a nil spec.
	spec *purelbv2.LBNodeAgentLocalSpec

	groups       map[string]*purelbv2.ServiceGroupLocalSpec  // groupName -> ServiceGroupLocalSpec
	remoteGroups map[string]*purelbv2.ServiceGroupRemoteSpec // groupName -> ServiceGroupRemoteSpec

	// localNameRegex is the pattern that we use to determine if an
	// interface is local or not. nil means "default interface".
	localNameRegex *regexp.Regexp

	// dummyInt is the interface carrying non-local announcements.
	// addDummyInterface is idempotent and resolves by name, so a handle
	// held by an in-flight reader across a config swap still refers to
	// the same interface.
	dummyInt netlink.Link
}

// nodeElection is the slice of the election the announcer actually uses:
// two methods, not the whole Election. Narrowed so the announcer can be
// tested without standing up leases and informers -- which is why the
// affinity/GARP bug had no unit test to catch it. Same pattern as the
// nodeClient and configSetter interfaces in cmd/lbnodeagent.
type nodeElection interface {
	// WinnerWithPreference must be the ONLY ownership question the
	// announcer asks. Plain Winner gives a different answer for
	// affinity-placed addresses, and mixing the two is what made a node
	// announce an address it then refused to advertise.
	WinnerWithPreference(key string, preferred []string) string
	MemberCount() int
}

type announcer struct {
	client   k8s.ServiceEvent
	logger   log.Logger
	myNode   string
	election nodeElection

	// cfg is the current configuration snapshot. A nil pointer means we
	// are not configured and must not announce; see SetConfig. Loaded
	// exactly once per SetBalancer cycle and threaded down, so a config
	// swap mid-cycle cannot be observed halfway.
	cfg atomic.Pointer[announcerConfig]

	// svcIngresses is a map from svcName to that Service's
	// Ingresses. Note that we may or may not advertise all of them
	// because we might lose an election or not have any active
	// endpoints, but in any case we need to ensure that we clean them
	// up if the Service is deleted.
	//
	// Not synchronized, and does not need to be: it is written only by
	// SetBalancer/DeleteBalancer on the service-sync goroutine, and read
	// by WithdrawAll, which runs from Shutdown after Client.Run has
	// returned and that goroutine has exited.
	svcIngresses map[string][]v1.LoadBalancerIngress

	// addressRenewals tracks addresses that need periodic renewal.
	// Key format: "namespace/servicename:ip" to support shared IPs.
	addressRenewals sync.Map // map[string]*addressRenewal

	// announced tracks the addresses that this node has actually added to
	// an interface. Key format matches addressRenewals (see renewalKey).
	//
	// This is the reference count that decides whether a shared address may
	// be withdrawn. It deliberately does not use svcIngresses, which holds
	// every LoadBalancer Service this node has seen whether or not this node
	// won the election for it: two services sharing an IP would each find
	// the other there and both decline to withdraw, stranding the address on
	// a node that lost.
	announced sync.Map // map[string]struct{}
}

// addressRenewal holds the state needed to periodically refresh an address
// before its lifetime expires.
//
// timer is atomic because it is written by the renewal timer's own
// goroutine (renewAddress re-arming itself) and read by the service-sync
// goroutine (scheduleRenewal, cancelRenewal, WithdrawAll, all calling
// Stop). The cancelled flag does not make a plain field safe: cancelRenewal
// and WithdrawAll set it before Stop and renewAddress re-checks it before
// re-arming, which narrows the window to the couple of instructions between
// that check and the assignment but does not close it -- and scheduleRenewal
// Stops the old timer without touching cancelled at all, leaving the window
// wide open.
type addressRenewal struct {
	ipNet     net.IPNet
	link      netlink.Link
	opts      AddressOptions
	timer     atomic.Pointer[time.Timer]
	interval  time.Duration
	cancelled atomic.Bool // Set when renewal is cancelled to prevent race with timer
}

// stopTimer stops this renewal's timer if one has been armed. Safe to call
// from any goroutine and safe before the first arm.
func (r *addressRenewal) stopTimer() {
	if t := r.timer.Load(); t != nil {
		t.Stop()
	}
}

var announcing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: purelbv2.MetricsNamespace,
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

// SetConfig applies a config delivery. It runs on the CR-controller
// goroutine, concurrently with SetBalancer on the service-sync goroutine.
//
// Everything is built into locals first and published with a single
// atomic Store, so readers never observe a partially-applied config. In
// particular the previous snapshot stays live and usable for the whole
// build: this deliberately does NOT blank the config on entry the way it
// used to. That blanking was doing two jobs -- opening the race window,
// and encoding "no agent means announce nothing" -- and only the second
// is wanted. Every path that means "announce nothing" now stores nil
// explicitly, and nothing else disturbs the live config.
func (a *announcer) SetConfig(cfg *purelbv2.Config) error {
	// The election selector uses the same helper, so both sides always
	// choose the same agent (the coherence invariant).
	agent := purelbv2.FirstLocalAgent(cfg.Agents)
	if agent == nil {
		// No agent governs this node: the LBNodeAgent was deleted, or a
		// nodeSelector deselected us. Announce nothing. This store is what
		// the old entry-point blanking used to do implicitly; without it a
		// deselected node would keep announcing on its last config forever.
		a.cfg.Store(nil)
		logging.Info(a.logger, "event", "noConfig")
		return nil
	}
	spec := agent.Spec.Local
	logging.Info(a.logger, "op", "setConfig", "spec", spec, "name", agent.Namespace+"/"+agent.Name)

	// stash the local ServiceGroup configs
	groups := map[string]*purelbv2.ServiceGroupLocalSpec{}
	remoteGroups := map[string]*purelbv2.ServiceGroupRemoteSpec{}
	for _, group := range cfg.Groups {
		if group.Spec.Local != nil {
			groups[group.ObjectMeta.Name] = group.Spec.Local
		}
		if group.Spec.Remote != nil {
			remoteGroups[group.ObjectMeta.Name] = group.Spec.Remote
		}
	}

	localNameRegex, err := compileLocalInterface(spec)
	if err != nil {
		// An invalid config must not leave the previous one announcing:
		// the node agent reports SyncStateError upward and its election
		// selector is emptied to match, so the lease advertises nothing.
		a.cfg.Store(nil)
		return err
	}

	// now that we've got a config we can create the dummy interface
	dummyInt, err := addDummyInterface(spec.DummyInterface)
	if err != nil {
		a.cfg.Store(nil)
		return fmt.Errorf("error adding interface \"%s\": %s", spec.DummyInterface, err.Error())
	}

	// The dummy interface is set up so we can publish the config, which
	// will allow announcements to happen. One store, everything at once.
	a.cfg.Store(&announcerConfig{
		spec:           spec,
		groups:         groups,
		remoteGroups:   remoteGroups,
		localNameRegex: localNameRegex,
		dummyInt:       dummyInt,
	})

	return nil
}

// compileLocalInterface compiles the netlink-free part of a Local spec:
// the localInterface regex. Kept separate from SetConfig so it can be
// unit-tested without CAP_NET_ADMIN. A nil return means "use the default
// interface". An empty LocalInterface is treated as "default" —
// kubebuilder defaulting can be bypassed by programmatic clients, and
// compiling "" would yield a match-everything regex. The election's
// SelectorFromConfig applies the identical rule.
func compileLocalInterface(spec *purelbv2.LBNodeAgentLocalSpec) (*regexp.Regexp, error) {
	if spec.LocalInterface == "default" || spec.LocalInterface == "" {
		return nil, nil
	}
	regex, err := regexp.Compile(spec.LocalInterface)
	if err != nil {
		return nil, fmt.Errorf("error compiling regex \"%s\": %s", spec.LocalInterface, err.Error())
	}
	return regex, nil
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

	// Take the config snapshot ONCE for this whole cycle and thread it
	// down. Re-reading a.cfg per dereference is what allowed a concurrent
	// SetConfig to nil the spec out from under an in-flight announcement.
	cfg := a.cfg.Load()

	// if we haven't been configured then we won't announce
	if cfg == nil {
		logging.Info(l, "event", "noConfig")
		// We are not announcing anything: withdraw any address we may
		// still hold and drop any slot that still names us. nodeSelector
		// deselection and config removal land here — the slot alone is
		// not enough, because a previously-announced VIP would stay on
		// the NIC with a live renewal timer re-adding it while another
		// node wins the election (duplicate ARP responders).
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if lbIP := net.ParseIP(ingress.IP); lbIP != nil {
				if err := a.deleteAddress(nsName, "noConfig", lbIP); err != nil {
					retErr = err
				}
				a.clearOwnAnnounceSlot(svc, lbIP)
			}
		}
		return retErr
	}

	// add the address to our announcement database
	a.svcIngresses[nsName] = svc.Status.LoadBalancer.Ingress

	// Compute preferred-node set once per SetBalancer cycle (not per IP).
	// For dual-stack services the inner loop iterates twice — without
	// hoisting, the annotation check + endpoint iteration would run
	// twice for identical input. Returns nil for opt-out services, in
	// which case WinnerWithPreference behaves identically to Winner.
	preferred := buildPreferredNodes(svc, epSlices)

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		// validate the allocated address
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			logging.Info(l, "op", "setBalancer", "error", "invalid LoadBalancer IP", "ip", ingress.IP)
			continue
		}

		if cfg.localNameRegex != nil {
			// The user specified an announcement interface regex so use it to
			// try to find a local interface
			lbIPNet, localif, err := findLocal(cfg.localNameRegex, cfg.spec.DummyInterface, lbIP)
			if err != nil {
				// The regex didn't resolve; try the explicitly-configured
				// extra interfaces (spec.interfaces) before falling through.
				if xNet, xIf, xErr := a.tryExtraInterfaces(cfg, lbIP); xErr == nil {
					lbIPNet, localif, err = xNet, xIf, nil
				} else if errors.Is(xErr, errIndeterminate) {
					err = xErr
				}
			}
			if err == nil {
				// We found a local interface, announce the address on it
				if err := a.announceLocal(cfg, svc, preferred, localif, lbIP, lbIPNet); err != nil {
					retErr = err
				}
			} else if a.isRemotePool(cfg, lbIP) {
				// lbIP is from a remote pool, so add it to dummyInt
				if err := a.announceRemote(cfg, svc, epSlices, lbIP); err != nil {
					retErr = err
				}
			} else if errors.Is(err, errIndeterminate) {
				// A partial netlink dump made resolution uncertain. Do not
				// withdraw on uncertain evidence — the next SetBalancer
				// cycle re-checks.
				logging.Info(l, "event", "resolutionIndeterminate", "ip", lbIP,
					"msg", "interface resolution indeterminate this cycle, skipping withdrawal")
			} else {
				// lbIP is from a local pool but no local interface matches.
				// Withdraw any stale announcement: the annotation slot alone
				// is not enough — a previously-announced VIP would stay on
				// the NIC with a live renewal timer re-adding it while
				// another node wins the election (duplicate ARP responders).
				if err := a.deleteAddress(nsName, "noLocalInterface", lbIP); err != nil {
					retErr = err
				}
				a.clearOwnAnnounceSlot(svc, lbIP)
				logging.Info(l, "event", "noLocalInterface", "ip", lbIP,
					"msg", "local pool IP has no matching local interface, not announcing")
			}

		} else {
			// The user wants us to determine the "default" interface
			var lbIPNet net.IPNet
			var localif netlink.Link
			announceInt, err := netutil.DefaultInterface(purelbv2.AddrFamily(lbIP))
			if err != nil {
				if len(cfg.spec.Interfaces) == 0 {
					// No fallback interfaces configured: keep the historical
					// hard-error path.
					logging.Info(l, "event", "announceError", "err", err)
					retErr = err
					continue
				}
			} else {
				lbIPNet, localif, err = checkLocal(announceInt, lbIP)
			}
			if err != nil {
				// The default interface didn't resolve; try the
				// explicitly-configured extra interfaces (spec.interfaces)
				// before falling through.
				if xNet, xIf, xErr := a.tryExtraInterfaces(cfg, lbIP); xErr == nil {
					lbIPNet, localif, err = xNet, xIf, nil
				} else if errors.Is(xErr, errIndeterminate) {
					err = xErr
				}
			}
			if err == nil {
				// We found a local interface, announce the address on it
				if err := a.announceLocal(cfg, svc, preferred, localif, lbIP, lbIPNet); err != nil {
					retErr = err
				}
			} else if a.isRemotePool(cfg, lbIP) {
				// lbIP is from a remote pool, so add it to dummyInt
				if err := a.announceRemote(cfg, svc, epSlices, lbIP); err != nil {
					retErr = err
				}
			} else if errors.Is(err, errIndeterminate) {
				// A partial netlink dump made resolution uncertain. Do not
				// withdraw on uncertain evidence — the next SetBalancer
				// cycle re-checks.
				logging.Info(l, "event", "resolutionIndeterminate", "ip", lbIP,
					"msg", "interface resolution indeterminate this cycle, skipping withdrawal")
			} else {
				// lbIP is from a local pool but doesn't match the default
				// interface or any extra interface. Withdraw any stale
				// announcement: the annotation slot alone is not enough — a
				// previously-announced VIP would stay on the NIC with a live
				// renewal timer re-adding it while another node wins the
				// election (duplicate ARP responders).
				if err := a.deleteAddress(nsName, "noLocalInterface", lbIP); err != nil {
					retErr = err
				}
				a.clearOwnAnnounceSlot(svc, lbIP)
				logging.Info(l, "event", "noLocalInterface", "ip", lbIP,
					"msg", "local pool IP has no matching local interface, not announcing")
			}
		}
	}

	// Return the most recent error
	return retErr
}

// tryExtraInterfaces tries to resolve lbIP on the interfaces listed in
// the Local spec's Interfaces field, in listed order (the documented
// contract); the first interface whose addresses contain lbIP wins.
// Missing interfaces are skipped without error — the CR is
// cluster-wide, so heterogeneous nodes are expected. Returns
// errIndeterminate when a candidate's address dump was partial and
// none succeeded, so the caller can avoid withdrawing on uncertain
// evidence.
func (a *announcer) tryExtraInterfaces(cfg *announcerConfig, lbIP net.IP) (net.IPNet, netlink.Link, error) {
	indeterminate := false
	for _, name := range cfg.spec.Interfaces {
		if name == "" || name == cfg.spec.DummyInterface {
			continue
		}
		nlIntf, err := netlink.LinkByName(name)
		if err != nil {
			logging.Debug(a.logger, "op", "tryExtraInterfaces", "interface", name,
				"msg", "interface not found, skipping")
			continue
		}
		ipnet, link, err := checkLocal(nlIntf, lbIP)
		if err == nil {
			return ipnet, link, nil
		}
		if errors.Is(err, errIndeterminate) {
			indeterminate = true
		}
	}
	if indeterminate {
		return net.IPNet{}, nil, errIndeterminate
	}
	return net.IPNet{}, nil, fmt.Errorf("no extra interface matched")
}

func (a *announcer) announceLocal(cfg *announcerConfig, svc *v1.Service, preferred []string, announceInt netlink.Link, lbIP net.IP, lbIPNet net.IPNet) error {
	l := log.With(a.logger, "service", svc.Name)
	nsName := svc.Namespace + "/" + svc.Name

	// Local addresses do not support ExternalTrafficPolicyLocal unless the override annotation is present.
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		if _, hasOverride := svc.Annotations[purelbv2.AllowLocalAnnotation]; hasOverride {

			// The user has added the override annotation so we'll allow
			// Local policy but warn them.
			logging.Info(l, "op", "setBalancer", "msg", "ExternalTrafficPolicy override annotation found, will allow Local policy. Incoming traffic will NOT be forwarded inter-node. This is not a recommended configuration; please see the PureLB docs for more info.")
			a.client.Infof(svc, "LocalOverride", "ExternalTrafficPolicy override annotation found, will allow Local policy. Incoming traffic will NOT be forwarded inter-node. This is not a recommended configuration; please see the PureLB docs for more info.")

		} else {

			// There's no override annotation so we'll switch back to
			// "Cluster"
			logging.Info(l, "op", "setBalancer", "msg", "ExternalTrafficPolicy Local not supported on local Interfaces, setting to Cluster. Please see the PureLB docs for info on why we do this, and how to override this policy.")
			a.client.Infof(svc, "LocalOverride", "ExternalTrafficPolicy Local not supported on local Interfaces, setting to Cluster. Please see the PureLB docs for info on why we do this, and how to override this policy.")
			// Set the service back to ExternalTrafficPolicyCluster if adding to local interface
			svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
			if err := a.deleteAddress(nsName, "ClusterLocal", lbIP); err != nil {
				return err
			}
		}
	}

	// See if we won the announcement election. preferred is the set of
	// nodes hosting Ready/Serving endpoints for this service when the
	// caller has opted in via NodeAffinityAnnotation; nil otherwise (in
	// which case WinnerWithPreference behaves identically to Winner).
	winner := a.election.WinnerWithPreference(lbIP.String(), preferred)
	if winner == "" {
		// No nodes have a subnet containing this IP - withdraw any announcement
		// This happens when subnet-aware election finds no eligible candidates
		logging.Info(l, "msg", "noEligibleNodes", "node", a.myNode, "ip", lbIP, "service", nsName,
			"reason", "no nodes have local subnet for this IP")
		return a.deleteAddress(nsName, "noEligibleNodes", lbIP)
	}
	if winner != a.myNode {
		// We lost the election so we'll withdraw any announcement that
		// we might have been making
		RecordElectionLoss()
		logging.Debug(l, "msg", "notWinner", "node", a.myNode, "winner", winner, "service", nsName, "memberCount", a.election.MemberCount())
		return a.deleteAddress(nsName, "lostElection", lbIP)
	}

	// We won the election so we'll add the service address to our
	// node's default interface so linux will respond to ARP
	// requests for it.
	RecordElectionWin()
	logging.Info(l, "msg", "electionWon", "node", a.myNode, "service", nsName, "memberCount", a.election.MemberCount())
	a.client.Infof(svc, "AnnouncingLocal", "Node %s announcing %s on interface %s", a.myNode, lbIP, announceInt.Attrs().Name)

	opts := getLocalAddressOptions(cfg)
	// IPv6 has no IFA_F_SECONDARY equivalent, so flannel can pick VIPs as the
	// node's public IPv6 address, breaking overlay routing. Setting PreferedLft=0
	// marks the address as deprecated (IFA_F_DEPRECATED), which flannel's
	// GetInterfaceIP6Addrs explicitly filters out. The address still receives
	// inbound traffic normally.
	if lbIP.To4() == nil {
		opts.PreferedLft = 0
	}
	if svc.Annotations[purelbv2.SkipIPv6DADAnnotation] == "true" {
		opts.SkipDAD = true
	}
	if err := addNetworkWithOptions(lbIPNet, announceInt, opts); err != nil {
		// We won but could not configure the address, so we are not
		// announcing it: drop any slot that still names us.
		a.clearOwnAnnounceSlot(svc, lbIP)
		return err
	}
	RecordAddressAddition()
	a.announced.Store(renewalKey(nsName, lbIP.String()), struct{}{})
	a.scheduleRenewal(nsName, lbIPNet, announceInt, opts)

	// Claim this IP's slot in the announcing-<family> annotation. The IP is
	// the slot key: whichever node wins the election for it owns the entry,
	// so we overwrite any prior holder rather than appending.
	a.claimAnnounceSlot(svc, announceInt.Attrs().Name, lbIP)

	announcing.With(prometheus.Labels{
		"service": nsName,
		"node":    a.myNode,
		"ip":      lbIP.String(),
	}).Set(1)

	// Send gratuitous announcements if configured: GARP for IPv4,
	// unsolicited Neighbor Advertisements for IPv6. The kernel does NOT
	// notify IPv6 neighbours when an address is added — it only runs
	// DAD, whose probes are sourced from :: and update no neighbor
	// caches (and the skip-DAD annotation suppresses even those) — so
	// without the NA a moved IPv6 VIP converges only via NUD timeouts.
	a.sendGARPSequence(cfg, lbIP, announceInt.Attrs().Name, preferred)

	return nil
}

// The announcement interface is always cfg.dummyInt. It used to also be
// passed in as a parameter that the body then ignored in favour of the
// field -- exactly the kind of "which one is live?" ambiguity that made
// the config race hard to see -- so the parameter is gone.
func (a *announcer) announceRemote(cfg *announcerConfig, svc *v1.Service, epSlices []*discoveryv1.EndpointSlice, lbIP net.IP) error {
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
		logging.Debug(l, "msg", "etpLocalCheck", "node", a.myNode, "sliceCount", sliceCount, "endpointNodes", endpointNodes, "hasHealthyEndpoint", hasEndpoint)

		if !hasEndpoint {
			logging.Debug(l, "msg", "policyLocalNoEndpoints", "node", a.myNode, "service", nsName)
			return a.deleteAddress(nsName, "noEndpoints", lbIP)
		}
	}

	// Find the group from which this address was allocated, which
	// gives us the subnet and aggregation that we need.
	groupName, group, err := a.poolFor(cfg, lbIP)
	if err != nil {
		return err
	}

	logging.Info(l, "msg", "announcingNonLocal", "node", a.myNode, "service", nsName, "interface", cfg.dummyInt.Attrs().Name, "group", groupName)
	a.client.Infof(svc, "AnnouncingNonLocal", "Announcing %s from node %s interface %s", lbIP, a.myNode, cfg.dummyInt.Attrs().Name)

	// Add this address to the dummy interface so routing software
	// (e.g., bird) will announce routes for it.
	logging.Debug(l, "msg", "subnet", "node", a.myNode, "service", nsName, "pool", group)
	opts := getDummyAddressOptions(cfg)
	lbIPNet, err := addVirtualInt(lbIP, cfg.dummyInt, group.Subnet, group.Aggregation, opts)
	if err != nil {
		return err
	}
	RecordAddressAddition()
	a.announced.Store(renewalKey(nsName, lbIP.String()), struct{}{})
	a.scheduleRenewal(nsName, lbIPNet, cfg.dummyInt, opts)

	// The announcing annotation for remote pools is derived by consumers
	// from pool-type=remote + the LBNodeAgent CR's dummy interface name.
	// Writing it here would cause a write storm since all nodes process
	// the service simultaneously.

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
		logging.Debug(a.logger, "msg", "Unknown LB, can't delete", "name", nsName)
		return nil
	}

	// delete this service from our announcement database
	delete(a.svcIngresses, nsName)

	// Every address is attempted before returning, and failures are
	// reported: a swallowed error here leaves the VIP on the NIC after the
	// Service is gone, with nothing left to trigger another withdrawal.
	var errs []error
	for _, ingress := range ingress {
		lbIP := net.ParseIP(ingress.IP)
		if lbIP == nil {
			errs = append(errs, fmt.Errorf("invalid LoadBalancer IP: %s, belongs to %s", ingress.IP, nsName))
			continue
		}
		if err := a.deleteAddress(nsName, reason, lbIP); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

	// This service is no longer announcing the address on this node.
	a.announced.Delete(renewalKey(nsName, svcAddr.String()))

	// If some *other* service is still announcing that address from this
	// node (a shared IP) then the address has to stay. Only entries in
	// announced count: a service that this node never won does not hold a
	// reference, so it cannot block the withdrawal.
	if other, inUse := a.otherAnnouncerOf(svcAddr); inUse {
		logging.Debug(a.logger, "event", "withdrawAnnouncement", "service", nsName, "reason", reason, "msg", "ip in use by other service", "other", other)
		return nil
	}

	removed, err := deleteAddr(svcAddr)
	if err != nil {
		logging.Info(a.logger, "event", "withdrawAddressFailed", "ip", svcAddr, "service", nsName,
			"reason", reason, "removed", removed, "error", err)
		return err
	}
	logging.Info(a.logger, "event", "withdrawAddress", "ip", svcAddr, "service", nsName,
		"reason", reason, "removed", removed)
	if removed == 0 {
		logging.Info(a.logger, "event", "withdrawAddressNotPresent", "ip", svcAddr, "service", nsName,
			"reason", reason, "msg", "address was not on any interface at withdraw time")
	}
	RecordAddressWithdrawal()

	return nil
}

// Shutdown cleans up changes that we've made to the local networking
// configuration.
func (a *announcer) Shutdown() {
	// Withdraw all announcements first
	a.WithdrawAll()

	// remove the "dummy" interface. Guarded: if no config was ever
	// accepted there is no interface to remove, and removeInterface would
	// call Attrs() on a nil Link and panic on the way out.
	if cfg := a.cfg.Load(); cfg != nil && cfg.dummyInt != nil {
		removeInterface(cfg.dummyInt)
	}
}

// WithdrawAll withdraws all announcements without removing the dummy interface.
// This is useful during graceful shutdown before the lease is deleted.
func (a *announcer) WithdrawAll() {
	// Cancel all renewal timers to prevent goroutine leaks
	a.addressRenewals.Range(func(key, val interface{}) bool {
		renewal := val.(*addressRenewal)
		renewal.cancelled.Store(true)
		renewal.stopTimer()
		return true
	})

	// withdraw any announcements that we have made
	for nsName := range a.svcIngresses {
		if err := a.DeleteBalancer(nsName, "withdrawAll", nil); err != nil {
			logging.Info(a.logger, "op", "withdrawAll", "error", err)
		}
	}
}

func (a *announcer) SetElection(e *election.Election) {
	// Explicit nil handling: assigning a typed nil pointer to an
	// interface field yields a NON-nil interface, which would turn every
	// `a.election != nil` guard into a nil-pointer dereference.
	if e == nil {
		a.election = nil
		return
	}
	a.election = e
}

// buildPreferredNodes returns the unique node names that host a Ready
// or Serving endpoint of the given service IF the service has opted in
// to election affinity via NodeAffinityAnnotation. Returns nil when:
//   - the annotation is absent or has a value other than "service-endpoints"
//   - the service is part of a shared-IP family (per-service preferred
//     sets are incompatible with sharing semantics; aggregation across
//     sharing services is a future enhancement)
//
// A nil return tells WinnerWithPreference to behave exactly like Winner
// (no affinity biasing, no fallback metric).
//
// Readiness rule (Ready OR Serving) mirrors nodeHasHealthyEndpoint
// below — terminating pods (Ready=false, Serving=true) stay in the
// preferred set so the VIP doesn't migrate away mid-drain.
func buildPreferredNodes(svc *v1.Service, epSlices []*discoveryv1.EndpointSlice) []string {
	if svc.Annotations[purelbv2.NodeAffinityAnnotation] != purelbv2.NodeAffinityServiceEndpoints {
		return nil
	}
	if _, shared := svc.Annotations[purelbv2.SharingAnnotation]; shared {
		return nil
	}
	seen := make(map[string]struct{})
	var preferred []string
	for _, slice := range epSlices {
		if slice == nil {
			continue
		}
		for _, ep := range slice.Endpoints {
			if ep.NodeName == nil {
				continue
			}
			ready := ep.Conditions.Ready != nil && *ep.Conditions.Ready
			serving := ep.Conditions.Serving != nil && *ep.Conditions.Serving
			if !ready && !serving {
				continue
			}
			if _, dup := seen[*ep.NodeName]; dup {
				continue
			}
			seen[*ep.NodeName] = struct{}{}
			preferred = append(preferred, *ep.NodeName)
		}
	}
	return preferred
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

// poolFor returns the name and AddressPool that contains lbIP.
// It checks both local and remote ServiceGroups.
// If error is not nil then no pool was found.
//
// Group names are walked in sorted order rather than map order. The
// allocator rejects overlapping ServiceGroups in parseGroups, but the
// announcer builds its maps from the unfiltered config and applies no such
// rule, so if two groups do overlap it is this function that decides which
// subnet and aggregation a remote address is announced with. A Go map walk
// would pick a different one per call, making the announced prefix flap
// between reconciles on the same node. Sorted order makes the choice
// arbitrary but stable, and identical on every node.
func (a *announcer) poolFor(cfg *announcerConfig, lbIP net.IP) (string, *purelbv2.AddressPool, error) {
	// Check local groups first
	for _, groupName := range slices.Sorted(maps.Keys(cfg.groups)) {
		pool, err := cfg.groups[groupName].PoolForAddress(lbIP)
		if err == nil {
			return groupName, pool, nil
		}
	}
	// Check remote groups
	for _, groupName := range slices.Sorted(maps.Keys(cfg.remoteGroups)) {
		pool, err := cfg.remoteGroups[groupName].PoolForAddress(lbIP)
		if err == nil {
			return groupName, pool, nil
		}
	}
	return "", nil, fmt.Errorf("Can't find pool for address %+v", lbIP)
}

// isRemotePool returns true if the IP address belongs to a remote pool.
// Remote pools should be announced on kube-lb0, while local pools should
// only be announced on matching local interfaces (or not at all if no match).
func (a *announcer) isRemotePool(cfg *announcerConfig, lbIP net.IP) bool {
	for _, group := range cfg.remoteGroups {
		_, err := group.PoolForAddress(lbIP)
		if err == nil {
			return true
		}
	}
	return false
}

// renewalKey generates the map key for a service's address renewal.
// Format: "namespace/servicename:ip" to support shared IPs across services.
func renewalKey(svcName, ip string) string {
	return svcName + ":" + ip
}

// otherAnnouncerOf reports whether any service other than the ones already
// removed from a.announced is still announcing addr from this node, i.e.
// whether addr is a shared IP that must not be withdrawn yet. It returns the
// name of one such service for logging.
//
// Keys are "namespace/servicename:ip". A service name never contains a colon
// but an IPv6 address does, so the split is on the *first* colon.
func (a *announcer) otherAnnouncerOf(addr net.IP) (string, bool) {
	want := addr.String()
	other := ""
	a.announced.Range(func(k, _ any) bool {
		key, ok := k.(string)
		if !ok {
			return true
		}
		i := strings.Index(key, ":")
		if i < 0 || key[i+1:] != want {
			return true
		}
		other = key[:i]
		return false // found one, stop
	})
	return other, other != ""
}

// netipOf converts a net.IP to a canonical (unmapped) netip.Addr. The second
// return is false if the address is not valid.
func netipOf(ip net.IP) (netip.Addr, bool) {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// announceKeepIPs returns the Service's current ingress addresses that share
// lbIP's family -- the set of IPs that legitimately belong in lbIP's
// announcing-<family> annotation key. It is the sweep set for Reconcile: a
// slot whose IP is not among these is for an address the Service no longer
// has, and is dropped.
func announceKeepIPs(svc *v1.Service, lbIP net.IP) []netip.Addr {
	wantV4 := lbIP.To4() != nil
	var keep []netip.Addr
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		a, err := netip.ParseAddr(ing.IP)
		if err != nil {
			continue
		}
		a = a.Unmap()
		if a.Is4() == wantV4 {
			keep = append(keep, a)
		}
	}
	return keep
}

// claimAnnounceSlot records this node as the announcer of lbIP in the
// annotation, taking the slot from any previous holder and sweeping slots for
// IPs the Service no longer has. Called only on the win path.
func (a *announcer) claimAnnounceSlot(svc *v1.Service, iface string, lbIP net.IP) {
	addr, ok := netipOf(lbIP)
	if !ok {
		return
	}
	key := purelbv2.AnnounceAnnotation + addrFamilyName(lbIP)
	existing := purelbv2.ParseAnnouncements(svc.Annotations[key])

	// If another node currently holds this IP's slot we are taking it from
	// them -- surface that, since two nodes announcing one IP is split-brain
	// the IP-keyed annotation would otherwise render as a clean handover.
	for _, e := range existing {
		if e.IP == addr && e.Node != a.myNode {
			RecordAnnounceSlotSteal(e.Node, a.myNode)
			a.client.Infof(svc, "AnnounceSlotSteal",
				"Node %s took announcement of %s from node %s", a.myNode, lbIP, e.Node)
			break
		}
	}

	mine := purelbv2.Announcement{Node: a.myNode, Interface: iface, IP: addr}
	a.setAnnounceAnnotation(svc, key, existing.Reconcile(mine, announceKeepIPs(svc, lbIP)))
}

// clearOwnAnnounceSlot removes this node's slot for lbIP from the annotation,
// if present. Used where this node knows it is not announcing lbIP for a
// stable reason (no matching local interface, no config) and so no other node
// will necessarily overwrite the slot. It only ever removes this node's own
// entry, so it cannot race another node's claim.
func (a *announcer) clearOwnAnnounceSlot(svc *v1.Service, lbIP net.IP) {
	if svc.Annotations == nil {
		return
	}
	key := purelbv2.AnnounceAnnotation + addrFamilyName(lbIP)
	if svc.Annotations[key] == "" {
		return
	}
	addr, ok := netipOf(lbIP)
	if !ok {
		return
	}
	existing := purelbv2.ParseAnnouncements(svc.Annotations[key])
	kept := make(purelbv2.Announcements, 0, len(existing))
	changed := false
	for _, e := range existing {
		if e.IP == addr && e.Node == a.myNode {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	if !changed {
		return
	}
	a.setAnnounceAnnotation(svc, key, kept)
}

// setAnnounceAnnotation writes the encoded annotation value, deleting the key
// when the value is empty so an empty string is never stored (an empty-valued
// key would defeat the change detection in maybeUpdateService).
func (a *announcer) setAnnounceAnnotation(svc *v1.Service, key string, anns purelbv2.Announcements) {
	encoded := anns.Encode()
	if encoded == "" {
		delete(svc.Annotations, key)
		return
	}
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[key] = encoded
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

	// Cancel existing timer if any. Mark it cancelled as well as stopping
	// it: an in-flight renewAddress for the old renewal re-checks that flag
	// before re-arming, so without this the superseded timer can arm itself
	// again after being Stopped and go on firing untracked.
	if old, loaded := a.addressRenewals.LoadAndDelete(key); loaded {
		oldRenewal := old.(*addressRenewal)
		oldRenewal.cancelled.Store(true)
		oldRenewal.stopTimer()
	}

	renewal.timer.Store(time.AfterFunc(interval, func() {
		a.renewAddress(key)
	}))

	a.addressRenewals.Store(key, renewal)
	logging.Debug(a.logger, "op", "scheduleRenewal", "key", key, "interval", interval)
}

// renewAddress refreshes an address's lifetime. Called by the renewal timer.
func (a *announcer) renewAddress(key string) {
	val, ok := a.addressRenewals.Load(key)
	if !ok {
		return // Cancelled, nothing to do
	}
	renewal := val.(*addressRenewal)

	// Check if cancelled (race condition: timer fired after cancel but before Stop)
	if renewal.cancelled.Load() {
		return
	}

	if err := addNetworkWithOptions(renewal.ipNet, renewal.link, renewal.opts); err != nil {
		RecordAddressRenewalError()
		logging.Info(a.logger, "op", "renewAddress", "key", key, "error", err)
		// Still reschedule - transient errors shouldn't stop renewal
	} else {
		RecordAddressRenewal()
		logging.Debug(a.logger, "op", "renewAddress", "key", key, "msg", "renewed", "next", renewal.interval)
	}

	// Check again before rescheduling (may have been cancelled during renewal)
	if renewal.cancelled.Load() {
		return
	}

	// Reschedule for next renewal
	renewal.timer.Store(time.AfterFunc(renewal.interval, func() {
		a.renewAddress(key)
	}))
}

// cancelRenewal stops the renewal timer for a specific service/IP combination.
func (a *announcer) cancelRenewal(svcName, ip string) {
	key := renewalKey(svcName, ip)
	if val, loaded := a.addressRenewals.LoadAndDelete(key); loaded {
		renewal := val.(*addressRenewal)
		// Mark as cancelled first, then stop timer
		// This prevents a race where timer fires between LoadAndDelete and Stop
		renewal.cancelled.Store(true)
		renewal.stopTimer()
		logging.Debug(a.logger, "op", "cancelRenewal", "key", key)
	}
}

// getLocalAddressOptions returns the AddressOptions for addresses added to
// the local interface. Defaults to finite lifetime (300s) with NoPrefixRoute
// to prevent CNI plugins like Flannel from selecting VIPs as node addresses.
func getLocalAddressOptions(c *announcerConfig) AddressOptions {
	opts := AddressOptions{
		ValidLft:      300, // default 5 minutes
		PreferedLft:   300,
		NoPrefixRoute: true,
	}

	if c != nil && c.spec.AddressConfig != nil && c.spec.AddressConfig.LocalInterface != nil {
		cfg := c.spec.AddressConfig.LocalInterface
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

// sendGARPSequence sends gratuitous announcement packets according to
// the GARPConfig: GARP for IPv4 addresses, unsolicited Neighbor
// Advertisements for IPv6 (its protocol counterpart — same config
// gate, delay, count, interval and verify-before-send semantics).
// The sequence runs in a goroutine to avoid blocking the main announcement.
// preferred is the same set the caller passed to WinnerWithPreference when
// it decided to announce, so verify-before-send re-asks the identical
// question. Passing nil here would make it fall back to plain-Winner
// semantics and silently suppress announcements for affinity-placed
// addresses.
func (a *announcer) sendGARPSequence(c *announcerConfig, lbIP net.IP, ifName string, preferred []string) {
	if c == nil || c.spec.GARPConfig == nil {
		return // GARP not configured
	}

	cfg := c.spec.GARPConfig

	// Check if enabled (defaults to true when GARPConfig is set)
	if cfg.Enabled != nil && !*cfg.Enabled {
		return // Explicitly disabled
	}

	// Parse initial delay (default 100ms)
	initialDelay := 100 * time.Millisecond
	if cfg.InitialDelay != "" {
		if d, err := time.ParseDuration(cfg.InitialDelay); err == nil {
			initialDelay = d
		} else {
			logging.Info(a.logger, "op", "sendGARP", "error", "invalid initialDelay",
				"value", cfg.InitialDelay, "msg", "using default 100ms")
		}
	}

	// Count (default 3)
	count := 3
	if cfg.Count != nil {
		count = *cfg.Count
		if count < 1 {
			count = 1
		} else if count > 10 {
			count = 10
		}
	}

	// Interval (default 500ms)
	interval := 500 * time.Millisecond
	if cfg.Interval != "" {
		if d, err := time.ParseDuration(cfg.Interval); err == nil {
			interval = d
		} else {
			logging.Info(a.logger, "op", "sendGARP", "error", "invalid interval",
				"value", cfg.Interval, "msg", "using default 500ms")
		}
	}

	// Verify before send (default true)
	verifyBeforeSend := true
	if cfg.VerifyBeforeSend != nil {
		verifyBeforeSend = *cfg.VerifyBeforeSend
	}

	// Pick the family-appropriate packet. The sequence semantics are
	// identical; only the wire format differs.
	op := "sendGARP"
	send := func() error { return sendGARP(ifName, lbIP) }
	recordSent, recordError := RecordGARPSent, RecordGARPError
	if lbIP.To4() == nil {
		op = "sendUnsolicitedNA"
		send = func() error { return sendUnsolicitedNA(ifName, lbIP) }
		recordSent, recordError = RecordNASent, RecordNAError
	}

	// Run the sequence in a goroutine to avoid blocking
	go func() {
		ipStr := lbIP.String()

		// Wait initial delay
		if initialDelay > 0 {
			time.Sleep(initialDelay)
		}

		for i := 0; i < count; i++ {
			// Wait interval between packets (not before first)
			if i > 0 && interval > 0 {
				time.Sleep(interval)
			}

			// Verify we still own this address before sending. This must
			// ask the election the SAME question that placed the address
			// -- WinnerWithPreference, with the same preferred set -- not
			// plain Winner. They differ exactly when a service opts into
			// NodeAffinityAnnotation, and then the node holding the
			// address is not the plain-Winner node, so every packet was
			// skipped and an affinity-placed VIP never announced itself
			// at all. The observable was a moved VIP receiving no traffic
			// until the upstream ARP/ND caches aged out.
			if verifyBeforeSend && a.election != nil {
				winner := a.election.WinnerWithPreference(ipStr, preferred)
				if winner != a.myNode {
					logging.Debug(a.logger, "op", op, "ip", ipStr, "packet", i+1, "of", count,
						"msg", "skipping announcement, no longer winner", "winner", winner)
					return // Stop the sequence, we lost ownership
				}
			}

			if err := send(); err != nil {
				recordError()
				logging.Info(a.logger, "op", op, "ip", ipStr, "interface", ifName,
					"packet", i+1, "of", count, "error", err)
				// Continue sending remaining packets even on error
			} else {
				recordSent()
				logging.Debug(a.logger, "op", op, "ip", ipStr, "interface", ifName,
					"packet", i+1, "of", count, "msg", "sent")
			}
		}
	}()
}

// getDummyAddressOptions returns the AddressOptions for addresses added to
// the dummy interface. Defaults to permanent (0) since these don't conflict
// with CNI plugins and permanent addresses provide routing stability.
func getDummyAddressOptions(c *announcerConfig) AddressOptions {
	opts := AddressOptions{
		ValidLft:      0, // default permanent
		PreferedLft:   0,
		NoPrefixRoute: false,
	}

	if c != nil && c.spec.AddressConfig != nil && c.spec.AddressConfig.DummyInterface != nil {
		cfg := c.spec.AddressConfig.DummyInterface
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
