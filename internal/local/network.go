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

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"

	purelbv1 "purelb.io/pkg/apis/v1"
)

// findLocal tries to find a "local" network interface based on the
// name of the interface and the IP addresses that are assigned to it.
// A network interface is considered local if its name matches the
// configuration regex and lbIP is within the same network as the
// interface.  If both are true, then the netlink.Link return value
// will be the default interface and error will be nil.  If error is
// non-nil then no local interface was found.
func findLocal(regex *regexp.Regexp, lbIP net.IP) (net.IPNet, netlink.Link, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return net.IPNet{}, nil, err
	}

	for _, intf := range interfaces {
		if regex.Match([]byte(intf.Name)) {
			// The interface name matches the local regex so check if the
			// addresses also match
			nlIntf, err := netlink.LinkByName(intf.Name)
			if err != nil {
				return net.IPNet{}, nil, err
			}
			if ipnet, link, err := checkLocal(nlIntf, lbIP); err == nil {
				// The addresses match so this is a local interface
				return ipnet, link, nil
			}
		}
	}

	return net.IPNet{}, nil, fmt.Errorf("No local interface found")
}

// checkLocal determines whether lbIP belongs to the same network as
// intf.  If so, then the netlink.Link return value will be the
// default interface and error will be nil.  If error is non-nil then
// the address is non-local.
func checkLocal(intf netlink.Link, lbIP net.IP) (net.IPNet, netlink.Link, error) {
	var lbIPNet net.IPNet = net.IPNet{IP: lbIP}

	family := purelbv1.AddrFamily(lbIP)

	defaddrs, err := netlink.AddrList(intf, family)
	if err != nil {
		return lbIPNet, intf, err
	}

	if family == nl.FAMILY_V4 {
		for _, addrs := range defaddrs {
			localnet := addrs.IPNet

			if localnet.Contains(lbIPNet.IP) {
				lbIPNet.Mask = localnet.Mask
			}
		}

	} else {
		for _, addrs := range defaddrs {

			/*  ifa_flags from linux source if_addr.h

			#define IFA_F_SECONDARY		0x01
			#define IFA_F_TEMPORARY		IFA_F_SECONDARY

			#define	IFA_F_NODAD		0x02
			#define IFA_F_OPTIMISTIC	0x04
			#define IFA_F_DADFAILED		0x08
			#define	IFA_F_HOMEADDRESS	0x10
			#define IFA_F_DEPRECATED	0x20
			#define IFA_F_TENTATIVE		0x40
			#define IFA_F_PERMANENT		0x80
			#define IFA_F_MANAGETEMPADDR	0x100
			#define IFA_F_NOPREFIXROUTE	0x200
			#define IFA_F_MCAUTOJOIN	0x400
			#define IFA_F_STABLE_PRIVACY	0x800

			*/

			localnet := addrs.IPNet

			if localnet.Contains(lbIPNet.IP) == true && addrs.Flags < 256 {
				lbIPNet.Mask = localnet.Mask
			}
		}
	}

	if lbIPNet.Mask == nil {
		return lbIPNet, intf, fmt.Errorf("non-local address")
	}

	return lbIPNet, intf, nil
}

// defaultInterface finds the default interface (i.e., the one with
// the default route) for the given family, which should be either
// nl.FAMILY_V6 or nl.FAMILY_V4.
func defaultInterface(family int) (netlink.Link, error) {
	var defaultifindex int = 0
	var defaultifmetric int = 0

	rt, err := netlink.RouteList(nil, family)
	if err != nil {
		return nil, err
	}
	for _, r := range rt {
		// check each route to see if it's the default (i.e., no destination)
		if r.Dst == nil && defaultifindex == 0 {
			// this is the first default route we've seen
			defaultifindex = r.LinkIndex
			defaultifmetric = r.Priority
		} else if r.Dst == nil && defaultifindex != 0 && r.Priority < defaultifmetric {
			// if there's another default route with a lower metric use it
			defaultifindex = r.LinkIndex
		}
	}

	// If none of our routes matched our criteria then we can't pick an
	// interface
	if defaultifindex == 0 {
		return nil, fmt.Errorf("No default interface for family %d can be determined", family)
	}

	// there's only one default route
	defaultint, err := netlink.LinkByIndex(defaultifindex)
	return defaultint, err
}

// addNetwork adds lbIPNet to link.
func addNetwork(lbIPNet net.IPNet, link netlink.Link) error {
	addr, err := netlink.ParseAddr(lbIPNet.String())
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("could not add %v: to %v %w", addr, link, err)
	}
	return nil
}

// addDummyInterface creates a "dummy" interface whose name is
// specified by dummyint.
func addDummyInterface(name string) (netlink.Link, error) {

	// check if there's already an interface with that name
	link, err := netlink.LinkByName(name)
	if err != nil {

		// the interface doesn't exist, so we can add it
		dumint := netlink.NewLinkAttrs()
		dumint.Name = name
		link = &netlink.Dummy{LinkAttrs: dumint}
		if err = netlink.LinkAdd(link); err != nil {
			return nil, fmt.Errorf("failed adding dummy int %s: %w", name, err)
		}

	}
	// Make sure that "dummy" interface is set to up.
	netlink.LinkSetUp(link)
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

// deleteAddr deletes lbIP from whichever interface has it.
func deleteAddr(lbIP net.IP) error {
	hostints, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, hostint := range hostints {
		addrs, err := hostint.Addrs()
		if err != nil {
			return err
		}
		for _, ipnet := range addrs {

			ipaddr, _, err := net.ParseCIDR(ipnet.String())
			if err != nil {
				return err
			}

			if lbIP.Equal(ipaddr) {
				ifindex, err := netlink.LinkByIndex(hostint.Index)
				if err != nil {
					return err
				}
				deladdr, err := netlink.ParseAddr(ipnet.String())
				if err != nil {
					return err
				}
				err = netlink.AddrDel(ifindex, deladdr)
				if err != nil {
					return fmt.Errorf("could not remove %v from %v: %w", deladdr, ifindex, err)
				}
			}
		}
	}

	return nil
}

func addVirtualInt(lbIP net.IP, link netlink.Link, subnet, aggregation string) error {

	lbIPNet := net.IPNet{IP: lbIP}

	if aggregation == "default" {

		switch purelbv1.AddrFamily(lbIP) {
		case (nl.FAMILY_V4):
			_, poolipnet, err := net.ParseCIDR(subnet)
			if err != nil {
				return err
			}

			lbIPNet.Mask = poolipnet.Mask

			if err := addNetwork(lbIPNet, link); err != nil {
				return fmt.Errorf("could not add %v: to %v %w", lbIPNet, link, err)
			}

		case (nl.FAMILY_V6):
			_, poolipnet, err := net.ParseCIDR(subnet)
			if err != nil {
				return err
			}

			lbIPNet.Mask = poolipnet.Mask

			if err := addNetwork(lbIPNet, link); err != nil {
				return fmt.Errorf("could not add %v: to %v %w", lbIPNet, link, err)
			}
		}

	} else {

		switch purelbv1.AddrFamily(lbIP) {
		case (nl.FAMILY_V4):
			_, poolaggr, err := net.ParseCIDR("0.0.0.0" + aggregation)
			if err != nil {
				return err
			}

			lbIPNet.Mask = poolaggr.Mask

			if err := addNetwork(lbIPNet, link); err != nil {
				return fmt.Errorf("could not add %v: to %v %w", lbIPNet, link, err)
			}

		case (nl.FAMILY_V6):
			_, poolaggr, err := net.ParseCIDR("::" + aggregation)
			if err != nil {
				return err
			}

			lbIPNet.Mask = poolaggr.Mask

			if err := addNetwork(lbIPNet, link); err != nil {
				return fmt.Errorf("could not add %v: to %v %w", lbIPNet, link, err)
			}
		}
	}

	return nil
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
