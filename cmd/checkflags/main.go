package main

import (
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

func main() {
	ifname := "eth0"
	if len(os.Args) > 1 {
		ifname = os.Args[1]
	}

	link, err := netlink.LinkByName(ifname)
	if err != nil {
		fmt.Printf("Error finding interface %s: %v\n", ifname, err)
		os.Exit(1)
	}

	fmt.Printf("=== IPv4 addresses on %s ===\n", ifname)
	addrsV4, err := netlink.AddrList(link, nl.FAMILY_V4)
	if err != nil {
		fmt.Printf("Error listing IPv4 addresses: %v\n", err)
	} else {
		for _, addr := range addrsV4 {
			fmt.Printf("  IP: %-45s Flags: 0x%04x (%4d)  <256: %v\n",
				addr.IPNet, addr.Flags, addr.Flags, addr.Flags < 256)
		}
	}

	fmt.Printf("\n=== IPv6 addresses on %s ===\n", ifname)
	addrsV6, err := netlink.AddrList(link, nl.FAMILY_V6)
	if err != nil {
		fmt.Printf("Error listing IPv6 addresses: %v\n", err)
	} else {
		for _, addr := range addrsV6 {
			fmt.Printf("  IP: %-45s Flags: 0x%04x (%4d)  <256: %v\n",
				addr.IPNet, addr.Flags, addr.Flags, addr.Flags < 256)
		}
	}

	fmt.Println("\n=== Flag reference ===")
	fmt.Println("  0x01 (1)    IFA_F_SECONDARY/TEMPORARY")
	fmt.Println("  0x02 (2)    IFA_F_NODAD")
	fmt.Println("  0x04 (4)    IFA_F_OPTIMISTIC")
	fmt.Println("  0x08 (8)    IFA_F_DADFAILED")
	fmt.Println("  0x10 (16)   IFA_F_HOMEADDRESS")
	fmt.Println("  0x20 (32)   IFA_F_DEPRECATED")
	fmt.Println("  0x40 (64)   IFA_F_TENTATIVE")
	fmt.Println("  0x80 (128)  IFA_F_PERMANENT")
	fmt.Println("  0x100 (256) IFA_F_MANAGETEMPADDR")
	fmt.Println("  0x200 (512) IFA_F_NOPREFIXROUTE")
	fmt.Println("  0x800 (2048) IFA_F_STABLE_PRIVACY")
}
