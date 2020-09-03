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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"purelb.io/internal/config"
	"purelb.io/internal/election"

	"github.com/go-kit/kit/log"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

type announcer struct {
	logger     log.Logger
	myNode     string
	nodeLabels labels.Set
	config     *config.Config
	svcAdvs    map[string]net.IP //svcName -> IP
	election   *election.Election
}

func NewAnnouncer(l log.Logger, node string) *announcer {
	return &announcer{logger: l, myNode: node, svcAdvs: map[string]net.IP{}}
}

func (c *announcer) SetConfig(cfg *config.Config) error {
	c.logger.Log("event", "newConfig")

	c.config = cfg

	// Call to should announce memberlist function

	return nil
}

func (c *announcer) SetBalancer(name string, lbIP net.IP, _ string) error {
	c.logger.Log("event", "announceService", "service", name)

	// k8s may send us multiple events to advertize same address
	if _, ok := c.svcAdvs[name]; ok {
		return nil
	}

	if lbIPNet, defaultifindex, err := c.checkLocal(lbIP); err == nil {
		if winner := c.election.Winner(name); winner == c.myNode {
			c.logger.Log("msg", "Winner, winner, Chicken dinner", "node", c.myNode, "service", name)

			c.addLocalInt(lbIPNet, defaultifindex)
			c.svcAdvs[name] = lbIP
		}
	}

	return nil
}

func (c *announcer) DeleteBalancer(name, reason string) error {
	if _, ok := c.svcAdvs[name]; !ok {
		return nil
	}

	c.deletesvcAdv(name)

	delete(c.svcAdvs, name)

	c.logger.Log("event", "updateService", "msg", "Delete balancer", "service", name, "reason", reason)
	return nil
}

func (c *announcer) SetNode(node *v1.Node) error {
	c.logger.Log("event", "updatedNodes", "msg", "Node announced", "name", node.Name)
	return nil
}

// checkLocal determines whether the provided net.IP is on the same
// network as the machine on which this code is running.  If the
// interface is local then the int return value will be the default
// interface index and error will be nil.  If error is non-nil then
// the address is non-local.
func (c *announcer) checkLocal(lbIP net.IP) (net.IPNet, int, error) {

	var err error
	var defaultifindex int
	var lbIPNet net.IPNet = net.IPNet{IP: lbIP}
	var family int = nl.FAMILY_V6
	if lbIP.To4() != nil {
		family = nl.FAMILY_V4
	}

	defaultifindex, err = defaultInterface(family)
	if err != nil {
		return lbIPNet, defaultifindex, err
	}

	defaultint, _ := netlink.LinkByIndex(defaultifindex)
	defaddrs, _ := netlink.AddrList(defaultint, family)

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
		return lbIPNet, defaultifindex, fmt.Errorf("non-local address")
	}

	return lbIPNet, defaultifindex, nil
}

func (c *announcer) addLocalInt(lbIPNet net.IPNet, defaultifindex int) error {
	c.logger.Log("event", "addLocalInt", "ip-address", lbIPNet.String(), "index", defaultifindex)

	addr, _ := netlink.ParseAddr(lbIPNet.String())
	ifindex, _ := netlink.LinkByIndex(defaultifindex)
	err := netlink.AddrReplace(ifindex, addr)
	if err != nil {
		return fmt.Errorf("could not add %v: to %v %v", addr, ifindex, err)
	}
	return nil
}

func (c *announcer) deletesvcAdv(name string) error {

	lbIP := c.svcAdvs[name]

	hostints, _ := net.Interfaces()
	for _, hostint := range hostints {
		addrs, _ := hostint.Addrs()
		for _, ipnet := range addrs {

			ipaddr, _, _ := net.ParseCIDR(ipnet.String())

			if lbIP.Equal(ipaddr) {
				ifindex, _ := netlink.LinkByIndex(hostint.Index)
				deladdr, _ := netlink.ParseAddr(ipnet.String())
				err := netlink.AddrDel(ifindex, deladdr)
				if err != nil {
					return fmt.Errorf("could not add %v: to %v %v", deladdr, ifindex, err)
				}
			}
		}
	}

	return nil
}

func CreateDummyInt(dummyint string) error {

	_, err := netlink.LinkByName(dummyint)
	if err != nil {

		dumint := netlink.NewLinkAttrs()
		dumint.Name = dummyint
		targetint := &netlink.Dummy{LinkAttrs: dumint}
		if err == netlink.LinkAdd(targetint) {
			return fmt.Errorf("failed adding dummy int %s: ", dummyint)
		}
	}

	return nil
}

// defaultInterface finds the default interface (i.e., the one with
// the default route) for the given IP family.
func defaultInterface(family int) (int, error) {
	var defaultifindex int = 0

	rt, _ := netlink.RouteList(nil, family)
	for _, r := range rt {
		// check each route to see if it's the default (i.e., no destination)
		if r.Dst == nil && defaultifindex == 0 {
			// this is the first default route we've seen
			defaultifindex = r.LinkIndex
		} else if r.Dst == nil && defaultifindex != 0 {
			// if there's a *second* default route then we're in trouble
			return -1, fmt.Errorf("error, cannot determine default in, multiple default routes")
		}
	}

	// there's only one default route
	return defaultifindex, nil
}

func (c *announcer) SetElection(election *election.Election) {
	c.election = election
}
