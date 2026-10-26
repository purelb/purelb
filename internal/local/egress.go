// Copyright 2026 Acnodal Inc.
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
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egresshook"
	"purelb.io/internal/logging"
)

var _ egresshook.Placer = (*announcer)(nil)

// errNotConfigured is returned while no config is accepted: nothing is
// announced then (see SetBalancer).
var errNotConfigured = errors.New("the announcer has no configuration")

// EgressInterface implements egresshook.Placer.
func (a *announcer) EgressInterface(ip net.IP) (int, string, error) {
	cfg := a.cfg.Load()
	if cfg == nil {
		return 0, "", errNotConfigured
	}
	_, link, err := a.resolveLocal(cfg, ip)
	var noDefault *noDefaultInterface
	if errors.As(err, &noDefault) {
		err = noDefault.err
	}
	if err != nil {
		return 0, "", err
	}
	return link.Attrs().Index, link.Attrs().Name, nil
}

// PlaceEgress implements egresshook.Placer. The address joins announced
// under key, so a Service sharing nothing with it can't withdraw it, and
// its renewal is keyed the same way (D31).
func (a *announcer) PlaceEgress(key string, ip net.IP, p egresshook.EgressPlacement) error {
	cfg := a.cfg.Load()
	if cfg == nil {
		return errNotConfigured
	}
	if p.Routed {
		opts := getDummyAddressOptions(cfg)
		if ip.To4() == nil {
			opts.Deprecated = true // as a remote VIP: never a source
		}
		ipNet, err := addVirtualInt(ip, cfg.dummyInt, p.Subnet, p.Aggregation, opts)
		if err != nil {
			return err
		}
		a.announced.Store(renewalKey(key, ip.String()), struct{}{})
		a.scheduleRenewal(key, ipNet, cfg.dummyInt, opts)
		logging.Info(a.logger, "op", "placeEgress", "key", key, "ip", ip, "interface", cfg.dummyInt.Attrs().Name,
			"prefix", ipNet.String())
		return nil
	}
	ipNet, link, err := a.resolveLocal(cfg, ip)
	var noDefault *noDefaultInterface
	if errors.As(err, &noDefault) {
		err = noDefault.err
	}
	if err != nil {
		return err
	}
	opts := getLocalAddressOptions(cfg)
	if ip.To4() == nil {
		// Deprecated, as a local VIP: never a source, and flannel never
		// takes it for the node's address (design 6.7).
		opts.Deprecated = true
	}
	opts.SkipDAD = p.SkipDAD
	if err := addNetworkWithOptions(ipNet, link, opts); err != nil {
		return err
	}
	a.announced.Store(renewalKey(key, ip.String()), struct{}{})
	a.scheduleRenewal(key, ipNet, link, opts)
	logging.Info(a.logger, "op", "placeEgress", "key", key, "ip", ip, "interface", link.Attrs().Name)
	still := func() (bool, string) { return true, "" }
	if p.Still != nil {
		still = func() (bool, string) { return p.Still(), "" }
	}
	if ip.To4() == nil && !opts.SkipDAD {
		a.announceAfterDAD(cfg, ip, link, still)
		return nil
	}
	a.sendAnnouncements(cfg, ip, link.Attrs().Name, still)
	return nil
}

// dadWait bounds announceAfterDAD's wait: DAD takes dad_transmits
// retransmit intervals after a random delay of up to a second.
const dadWait = 10 * time.Second

// tentative reports whether ip is on link and whether it is still in DAD
// (announcer.tentative, which tests set, replaces it).
func tentative(link netlink.Link, ip net.IP) (present, inDAD bool) {
	list, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return true, true // unknown: keep waiting
	}
	for _, a := range list {
		if a.IP.Equal(ip) {
			return true, a.Flags&unix.IFA_F_TENTATIVE != 0
		}
	}
	return false, false
}

// announceAfterDAD sends the unsolicited NAs once ip has passed DAD, so a
// duplicate address is never claimed (user decision 2026-10-26); an address
// that leaves instead (DAD failed, or withdrawn) is never announced.
func (a *announcer) announceAfterDAD(cfg *announcerConfig, ip net.IP, link netlink.Link,
	still func() (bool, string)) {
	if cfg.spec.GARPConfig == nil {
		return // nothing to send
	}
	look := tentative
	if a.tentative != nil {
		look = a.tentative
	}
	go func() {
		deadline := time.Now().Add(dadWait)
		for time.Now().Before(deadline) {
			present, inDAD := look(link, ip)
			if !present {
				logging.Debug(a.logger, "op", "placeEgress", "ip", ip, "msg", "gone before passing DAD: not announced")
				return
			}
			if !inDAD {
				a.sendAnnouncements(cfg, ip, link.Attrs().Name, still)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		logging.Debug(a.logger, "op", "placeEgress", "ip", ip, "msg", "still in DAD: not announced")
	}()
}

// WithdrawEgress implements egresshook.Placer.
func (a *announcer) WithdrawEgress(key string, ip net.IP) error {
	a.cancelRenewal(key, ip.String())
	a.announced.Delete(renewalKey(key, ip.String()))
	if other, inUse := a.otherAnnouncerOf(ip); inUse {
		logging.Debug(a.logger, "op", "withdrawEgress", "key", key, "ip", ip, "msg", "ip in use by", "other", other)
		return nil
	}
	removed, err := deleteAddr(ip)
	if err != nil {
		logging.Info(a.logger, "op", "withdrawEgress", "key", key, "ip", ip, "removed", removed, "error", err)
		return err
	}
	logging.Info(a.logger, "op", "withdrawEgress", "key", key, "ip", ip, "removed", removed)
	return nil
}
