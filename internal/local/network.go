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
	"net"
	"net/netip"
	"regexp"
	"sort"
	"syscall"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/mdlayher/ndp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"

	"purelb.io/internal/netutil"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// errIndeterminate means interface resolution could not be decided
// this cycle (e.g. a partial netlink dump). Callers must not treat it
// as "non-local": withdrawing an announcement on uncertain evidence
// would turn transient kernel-dump races into VIP flaps.
var errIndeterminate = errors.New("interface resolution indeterminate")

// findLocal tries to find a "local" network interface based on the
// name of the interface and the IP addresses that are assigned to it.
// A network interface is considered local if its name matches the
// configuration regex (excluding the interface named exclude) and lbIP
// is within the same network as the interface.  If both are true, then
// the netlink.Link return value will be that interface and error will
// be nil.  If error is non-nil then no local interface was found;
// errIndeterminate means resolution was uncertain this cycle.
func findLocal(regex *regexp.Regexp, exclude string, lbIP net.IP) (net.IPNet, netlink.Link, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return net.IPNet{}, nil, err
	}

	// Collect the matching names, then sort: enumeration is in ifindex
	// order which is boot-dependent, and when one subnet spans two
	// matching interfaces the announce interface must not change
	// across reboots.
	matched := []string{}
	for _, intf := range interfaces {
		if intf.Name == exclude {
			continue
		}
		if regex.Match([]byte(intf.Name)) {
			matched = append(matched, intf.Name)
		}
	}
	sort.Strings(matched)

	indeterminate := false
	for _, name := range matched {
		nlIntf, err := netlink.LinkByName(name)
		if err != nil {
			// The interface disappeared between enumeration and lookup —
			// routine veth churn on pod-dense nodes. Skip it; aborting
			// the whole scan would fail resolution for every other
			// matching interface.
			continue
		}
		ipnet, link, err := checkLocal(nlIntf, lbIP)
		if err == nil {
			// The addresses match so this is a local interface
			return ipnet, link, nil
		}
		if errors.Is(err, errIndeterminate) {
			indeterminate = true
		}
	}

	if indeterminate {
		return net.IPNet{}, nil, errIndeterminate
	}
	return net.IPNet{}, nil, fmt.Errorf("No local interface found")
}

// checkLocal determines whether lbIP belongs to the same network as
// intf.  If so, then the netlink.Link return value will be the
// default interface and error will be nil.  If error is non-nil then
// the address is non-local; errIndeterminate means the address dump
// was partial and no containing address was seen, so "non-local"
// cannot be concluded this cycle.
func checkLocal(intf netlink.Link, lbIP net.IP) (net.IPNet, netlink.Link, error) {
	var lbIPNet net.IPNet = net.IPNet{IP: lbIP}

	family := purelbv2.AddrFamily(lbIP)

	defaddrs, err := netlink.AddrList(intf, family)
	partialDump := errors.Is(err, netlink.ErrDumpInterrupted)
	if err != nil && !partialDump {
		return lbIPNet, intf, err
	}

	if family == nl.FAMILY_V4 {
		for _, addrs := range defaddrs {
			if addrs.IPNet == nil {
				continue
			}
			if addrs.IPNet.Contains(lbIPNet.IP) {
				lbIPNet.Mask = addrs.IPNet.Mask
			}
		}

	} else {
		for _, addrs := range defaddrs {
			if addrs.IPNet == nil {
				continue
			}
			if addrs.IPNet.Contains(lbIPNet.IP) && (addrs.Flags&netutil.IPv6BadFlags) == 0 {
				lbIPNet.Mask = addrs.IPNet.Mask
			}
		}
	}

	if lbIPNet.Mask == nil {
		if partialDump {
			// The dump was interrupted and we saw no containing address:
			// it may simply have been missing from the partial results.
			return lbIPNet, intf, errIndeterminate
		}
		return lbIPNet, intf, fmt.Errorf("non-local address")
	}

	return lbIPNet, intf, nil
}

// AddressOptions configures how an address is added to an interface.
type AddressOptions struct {
	// ValidLft is the valid lifetime in seconds. 0 means permanent.
	ValidLft int
	// PreferedLft is the preferred lifetime in seconds. 0 means permanent.
	PreferedLft int
	// NoPrefixRoute prevents automatic prefix route creation when true.
	NoPrefixRoute bool
	// SkipDAD when true sets IFA_F_NODAD to skip IPv6 Duplicate Address
	// Detection. This is an IPv6-only kernel flag, harmlessly ignored for IPv4.
	SkipDAD bool
}

// addNetworkWithOptions adds lbIPNet to link with the specified options.
// When ValidLft and PreferedLft are non-zero, the kernel will NOT set
// IFA_F_PERMANENT on the address, which prevents CNI plugins like Flannel
// from incorrectly selecting VIPs as node addresses.
func addNetworkWithOptions(lbIPNet net.IPNet, link netlink.Link, opts AddressOptions) error {
	addr, err := netlink.ParseAddr(lbIPNet.String())
	if err != nil {
		return err
	}

	addr.ValidLft = opts.ValidLft
	addr.PreferedLft = opts.PreferedLft

	if opts.NoPrefixRoute {
		addr.Flags |= 0x200 // IFA_F_NOPREFIXROUTE
	}
	if opts.SkipDAD {
		addr.Flags |= 0x02 // IFA_F_NODAD
	}

	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("could not add %v to %v: %w", addr, link, err)
	}
	return nil
}

// addDummyInterface creates a "dummy" interface whose name is
// specified by dummyint.
func addDummyInterface(name string) (netlink.Link, error) {

	// check if there's already an interface with that name
	link, err := netlink.LinkByName(name)
	if err != nil {
		var lnfErr netlink.LinkNotFoundError
		if !errors.As(err, &lnfErr) {
			return nil, fmt.Errorf("checking for interface %s: %w", name, err)
		}

		// the interface doesn't exist, so we can add it
		dumint := netlink.NewLinkAttrs()
		dumint.Name = name
		link = &netlink.Dummy{LinkAttrs: dumint}
		if err = netlink.LinkAdd(link); err != nil {
			// Handle TOCTOU race: another process created the interface
			// between our LinkByName and LinkAdd
			if errors.Is(err, syscall.EEXIST) {
				link, err = netlink.LinkByName(name)
				if err != nil {
					return nil, fmt.Errorf("failed to get existing interface %s: %w", name, err)
				}
			} else {
				return nil, fmt.Errorf("failed adding dummy int %s: %w", name, err)
			}
		}

	}
	// Make sure that "dummy" interface is set to up.
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to set %s up: %w", name, err)
	}
	return link, nil
}

// removeInterface removes link. It returns nil if everything goes
// fine, an error otherwise.
func removeInterface(link netlink.Link) error {
	if err := netlink.LinkDel(link); err != nil {
		return err
	}

	return nil
}

// deleteAddr deletes lbIP from whichever interface has it. It returns the
// number of addresses actually removed so callers can distinguish "the
// address was not present" from "the address was removed" from an error.
func deleteAddr(lbIP net.IP) (int, error) {
	removed := 0
	hostints, err := net.Interfaces()
	if err != nil {
		return removed, err
	}
	for _, hostint := range hostints {
		addrs, err := hostint.Addrs()
		if err != nil {
			return removed, err
		}
		for _, ipnet := range addrs {

			ipaddr, _, err := net.ParseCIDR(ipnet.String())
			if err != nil {
				return removed, err
			}

			if lbIP.Equal(ipaddr) {
				ifindex, err := netlink.LinkByIndex(hostint.Index)
				if err != nil {
					return removed, err
				}
				deladdr, err := netlink.ParseAddr(ipnet.String())
				if err != nil {
					return removed, err
				}
				err = netlink.AddrDel(ifindex, deladdr)
				if err != nil {
					return removed, fmt.Errorf("could not remove %v from %v: %w", deladdr, ifindex, err)
				}
				removed++
			}
		}
	}

	return removed, nil
}

func addVirtualInt(lbIP net.IP, link netlink.Link, subnet, aggregation string, opts AddressOptions) (net.IPNet, error) {

	lbIPNet := net.IPNet{IP: lbIP}

	// Empty aggregation defaults to using the subnet's mask
	if aggregation == "default" || aggregation == "" {

		switch purelbv2.AddrFamily(lbIP) {
		case (nl.FAMILY_V4):
			_, poolipnet, err := net.ParseCIDR(subnet)
			if err != nil {
				return lbIPNet, err
			}

			lbIPNet.Mask = poolipnet.Mask

			if err := addNetworkWithOptions(lbIPNet, link, opts); err != nil {
				return lbIPNet, fmt.Errorf("could not add %v to %v: %w", lbIPNet, link, err)
			}

		case (nl.FAMILY_V6):
			_, poolipnet, err := net.ParseCIDR(subnet)
			if err != nil {
				return lbIPNet, err
			}

			lbIPNet.Mask = poolipnet.Mask

			if err := addNetworkWithOptions(lbIPNet, link, opts); err != nil {
				return lbIPNet, fmt.Errorf("could not add %v to %v: %w", lbIPNet, link, err)
			}
		}

	} else {

		switch purelbv2.AddrFamily(lbIP) {
		case (nl.FAMILY_V4):
			_, poolaggr, err := net.ParseCIDR("0.0.0.0" + aggregation)
			if err != nil {
				return lbIPNet, err
			}

			lbIPNet.Mask = poolaggr.Mask

			if err := addNetworkWithOptions(lbIPNet, link, opts); err != nil {
				return lbIPNet, fmt.Errorf("could not add %v to %v: %w", lbIPNet, link, err)
			}

		case (nl.FAMILY_V6):
			_, poolaggr, err := net.ParseCIDR("::" + aggregation)
			if err != nil {
				return lbIPNet, err
			}

			lbIPNet.Mask = poolaggr.Mask

			if err := addNetworkWithOptions(lbIPNet, link, opts); err != nil {
				return lbIPNet, fmt.Errorf("could not add %v to %v: %w", lbIPNet, link, err)
			}
		}
	}

	return lbIPNet, nil
}

// sendGARP sends a gratuitous ARP message for ip on ifi. This is
// based on MetalLB's internal/layer2/arp.go, modified to be a
// standalone function.
func sendGARP(ifName string, ip net.IP) error {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return fmt.Errorf("finding interface named %s: %w", ifName, err)
	}

	client, err := arp.Dial(ifi)
	if err != nil {
		return fmt.Errorf("creating ARP responder for %s: %w", ifName, err)
	}
	// arp.Dial opens an AF_PACKET socket; without this every GARP send would
	// leak a file descriptor.
	defer client.Close()

	for _, op := range []arp.Operation{arp.OperationRequest, arp.OperationReply} {
		pkt, err := arp.NewPacket(op, ifi.HardwareAddr, ip, ethernet.Broadcast, ip)
		if err != nil {
			return fmt.Errorf("assembling %q gratuitous packet for %q: %w", op, ip, err)
		}
		if err = client.WriteTo(pkt, ethernet.Broadcast); err != nil {
			return fmt.Errorf("writing %q gratuitous packet for %q: %w", op, ip, err)
		}
	}
	return nil
}

// buildUnsolicitedNA assembles the unsolicited Neighbor Advertisement
// for target announced from hwAddr: Override set so neighbors replace
// an existing cache entry for the moved VIP, Solicited clear (nobody
// asked), Router clear (the VIP is a host address, not a router).
func buildUnsolicitedNA(target netip.Addr, hwAddr net.HardwareAddr) *ndp.NeighborAdvertisement {
	return &ndp.NeighborAdvertisement{
		Override:      true,
		TargetAddress: target,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{Direction: ndp.Target, Addr: hwAddr},
		},
	}
}

// sendUnsolicitedNA sends an unsolicited Neighbor Advertisement for ip
// on ifName — the IPv6 counterpart of sendGARP. The kernel does not do
// this for us: adding an address only triggers DAD, whose probes are
// sourced from :: and update no neighbor caches, so without this a
// moved IPv6 VIP converges only via NUD timeouts.
func sendUnsolicitedNA(ifName string, ip net.IP) error {
	if ip.To4() != nil {
		return fmt.Errorf("not an IPv6 address: %s", ip)
	}
	target, ok := netip.AddrFromSlice(ip.To16())
	if !ok {
		return fmt.Errorf("invalid IPv6 address: %s", ip)
	}

	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return fmt.Errorf("finding interface named %s: %w", ifName, err)
	}

	c, _, err := ndp.Listen(ifi, ndp.LinkLocal)
	if err != nil {
		return fmt.Errorf("creating NDP responder for %s: %w", ifName, err)
	}
	defer c.Close()

	// RFC 4861 §7.2.6: unsolicited advertisements go to the all-nodes
	// multicast address.
	if err := c.WriteTo(buildUnsolicitedNA(target, ifi.HardwareAddr), nil, netip.AddrFrom16([16]byte{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01})); err != nil {
		return fmt.Errorf("writing unsolicited NA for %q: %w", ip, err)
	}
	return nil
}
