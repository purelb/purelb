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

	// Call to should announce memberlist function

	return nil
}

func (c *announcer) SetBalancer(name string, lbIP net.IP, localannounce string) error {
	fmt.Println("******announcer.SetBalancer", name, "svcAdvs:", c.svcAdvs)
	c.logger.Log("event", "updatedNodes", "msg", "Announce balancer", "service", name)

	// k8s may send us multiple events to advertize same address
	if _, ok := c.svcAdvs[name]; ok {
		return nil
	}

	if localannounce == "Winner" {
		lbIPNet, defaultifindex, _ := c.checkLocal(lbIP)
		c.addLocalInt(lbIPNet, defaultifindex)
		c.svcAdvs[name] = lbIP
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
	c.logger.Log("event", "updatedNodes", "msg", "Node announced", "name", node.Name)
	return nil
}

func (c *announcer) checkLocal(lbIP net.IP) (net.IPNet, int, error) {

	var defaultifindex int = 0
	var lbIPNet net.IPNet
	lbIPNet.IP = lbIP

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
		defaultint, _ := net.InterfaceByIndex(defaultifindex)

		defaddrs, _ := defaultint.Addrs()
		for _, addrs := range defaddrs {

			localnetaddr := addrs.String()

			_, ipnet, _ := net.ParseCIDR(localnetaddr)

			if ipnet.Contains(lbIPNet.IP) == true {

				lbIPNet.Mask = ipnet.Mask

			}
		}

	} else {
		return lbIPNet, defaultifindex, fmt.Errorf("unable to match addr to interface")
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
