// Copyright 2020 Acnodal Inc.  All rights reserved.

package local

import (
	"fmt"
	"net"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"purelb.io/internal/config"

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
}

func NewAnnouncer(l log.Logger, node string) *announcer {
	return &announcer{logger: l, myNode: node, svcAdvs: map[string]net.IP{}}
}

func (c *announcer) SetConfig(cfg *config.Config) error {
	c.logger.Log("event", "newConfig")

	c.config = cfg

	fmt.Println("*****c.announcer SetConfig : ", cfg)

	// Call to should announce memberlist function

	return nil
}

func (c *announcer) SetBalancer(name string, lbIP net.IP, _ string) error {
	fmt.Println("******announcer.SetBalancer", name, "svcAdvs:", c.svcAdvs)
	c.logger.Log("event", "updatedNodes", "msg", "Announce balancer", "service", name)

	// k8s may send us multiple events to advertize same address
	if _, ok := c.svcAdvs[name]; ok {
		return nil
	}

	if lbIPNet, defaultifindex, err := c.CheckLocal(lbIP); err == nil {
		fmt.Println(err)
		c.addLocalInt(lbIPNet, defaultifindex)
		c.svcAdvs[name] = lbIP
	}

	if _, _, err := c.CheckLocal(lbIP); err != nil {

		fmt.Println(err)

		fmt.Println("***** nonlocal lbIP: ", lbIP)
		fmt.Println("***** c.config: ", c.config.Pools)

	}

	return nil
}

func (c *announcer) DeleteBalancer(name, reason string) error {
	fmt.Println("****** announcer.DeleteBalancer", name, "svcAdvs:", c.svcAdvs)

	if _, ok := c.svcAdvs[name]; !ok {
		fmt.Print("****** announcer.DeleteBalancer svc not in map, ignore: ", name)
		return nil
	}

	c.deletesvcAdv(name)

	delete(c.svcAdvs, name)

	c.logger.Log("event", "updatedNodes", "msg", "Delete balancer", "service", name, "reason", reason)
	return nil
}

func (c *announcer) SetNode(node *v1.Node) error {
	fmt.Println("***** c.announcer SetNode: ", node)

	c.logger.Log("event", "updatedNodes", "msg", "Node announced", "name", node.Name)
	return nil
}

func (c *announcer) CheckLocal(lbIP net.IP) (net.IPNet, int, error) {

	var defaultifindex int = 0
	var lbIPNet net.IPNet
	lbIPNet.IP = lbIP
	var lbIPFamily int

	if nil != lbIP.To16() {
		lbIPFamily = (nl.FAMILY_V6)
	}

	if nil != lbIP.To4() {
		lbIPFamily = (nl.FAMILY_V4)
	}

	switch lbIPFamily {
	case (nl.FAMILY_V4):

		rt, _ := netlink.RouteList(nil, (nl.FAMILY_V4))
		for _, r := range rt {
			if r.Dst == nil && defaultifindex == 0 {
				defaultifindex = r.LinkIndex
			} else if r.Dst == nil && defaultifindex != 0 {
				fmt.Println("error, cannot determine default in, multiple default routes")
				return lbIPNet, defaultifindex, fmt.Errorf("error, cannot determine default in, multiple default routes")
			}
		}

		if defaultifindex != 0 {
			defaultint, _ := netlink.LinkByIndex(defaultifindex)
			defaddrs, _ := netlink.AddrList(defaultint, lbIPFamily)

			for _, addrs := range defaddrs {
				localnet := addrs.IPNet

				if localnet.Contains(lbIPNet.IP) == true {

					lbIPNet.Mask = localnet.Mask
					fmt.Println("local", lbIPNet)

				} else {
					fmt.Println("checklocal nonlocal")
				}
			}
		}
	case (nl.FAMILY_V6):

		rt, _ := netlink.RouteList(nil, (nl.FAMILY_V6))
		for _, r := range rt {
			if r.Dst == nil && defaultifindex == 0 {
				defaultifindex = r.LinkIndex
			} else if r.Dst == nil && defaultifindex != 0 {
				fmt.Println("error, cannot determine default in, multiple default routes")
				return lbIPNet, defaultifindex, fmt.Errorf("error, cannot determine default in, multiple default routes")
			}
		}

		if defaultifindex != 0 {
			defaultint, _ := netlink.LinkByIndex(defaultifindex)
			defaddrs, _ := netlink.AddrList(defaultint, lbIPFamily)

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
					fmt.Println("local", lbIPNet)

				} else {
					fmt.Println("checklocal nonlocal", lbIP)
				}

			}
		}

	}

	if lbIPNet.Mask == nil {
		return lbIPNet, defaultifindex, fmt.Errorf("non-local address")
	}

	return lbIPNet, defaultifindex, nil

}

func (c *announcer) addLocalInt(lbIPNet net.IPNet, defaultifindex int) error {

	fmt.Println("adding", lbIPNet, "to ifindex", defaultifindex)

	addr, _ := netlink.ParseAddr(lbIPNet.String())
	ifindex, _ := netlink.LinkByIndex(defaultifindex)
	err := netlink.AddrReplace(ifindex, addr)
	if err != nil {
		fmt.Printf("could not add %v: to %v %v", addr, ifindex, err)
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

				fmt.Println("****** announcer.delete", ipaddr)

				ifindex, _ := netlink.LinkByIndex(hostint.Index)
				deladdr, _ := netlink.ParseAddr(ipnet.String())
				err := netlink.AddrDel(ifindex, deladdr)
				if err != nil {
					fmt.Printf("could not delete %v: to %v %v", deladdr, ifindex, err)
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
			fmt.Println("failed adding dummy int", dummyint)
			return fmt.Errorf("failed adding dummy int %s: ", dummyint)
		}

	}

	return nil

}
