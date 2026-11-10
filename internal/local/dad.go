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
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"purelb.io/internal/logging"
)

// IPv6 duplicate address detection (DAD) for local addresses. A new IPv6
// address is tentative until DAD passes. Announcing it (unsolicited
// Neighbor Advertisements) before then would claim an address another
// host may own, so the NAs wait for DAD. A failed address is withdrawn
// and tried again later: the kernel deletes a finite-lifetime address
// that fails DAD (only a permanent one stays, with IFA_F_DADFAILED), and
// re-adding it at once, as the renewal timer did, repeats DAD and its NAs
// for as long as the other host keeps the address.

// dadState is an address's DAD state on a link.
type dadState int

const (
	dadGone dadState = iota // not on the link
	dadTentative
	dadFailedState // IFA_F_DADFAILED: a permanent address that failed
	dadPassed
)

const (
	// dadWait bounds the wait for DAD: it takes dad_transmits retransmit
	// intervals (1 s each by default) after a random delay of up to a
	// second.
	dadWait = 10 * time.Second
	dadPoll = 100 * time.Millisecond
	// A failed address is tried again after dadRetryMin, doubling up to
	// dadRetryMax while it keeps failing; passing resets it. The first
	// retry is soon because most failures are an election move: the new
	// winner adds the address while the old one, reacting to the same
	// Lease change, still holds it for a few hundred milliseconds. A
	// retry only repeats DAD, whose probes claim nothing, so retrying
	// soon costs a real conflict nothing.
	dadRetryMin = time.Second
	dadRetryMax = 5 * time.Minute
)

// dadRetry is a failed address's backoff.
type dadRetry struct {
	backoff time.Duration
	until   time.Time
}

// addrDADState reads ip's DAD state on link.
func addrDADState(link netlink.Link, ip net.IP) dadState {
	list, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return dadTentative // unknown: keep waiting
	}
	for _, a := range list {
		if !a.IP.Equal(ip) {
			continue
		}
		switch {
		case a.Flags&unix.IFA_F_DADFAILED != 0:
			return dadFailedState
		case a.Flags&unix.IFA_F_TENTATIVE != 0:
			return dadTentative
		}
		return dadPassed
	}
	return dadGone
}

// lookDAD is addrDADState, or the test's replacement.
func (a *announcer) lookDAD(link netlink.Link, ip net.IP) dadState {
	if a.dadState != nil {
		return a.dadState(link, ip)
	}
	return addrDADState(link, ip)
}

// inDADBackoff reports whether ip failed DAD and is waiting to be tried
// again, and for how long. Service sync goroutine.
func (a *announcer) inDADBackoff(ip net.IP) (bool, time.Duration) {
	v, ok := a.dadFailed.Load(ip.String())
	if !ok {
		return false, 0
	}
	left := time.Until(v.(dadRetry).until)
	return left > 0, left
}

// watchDAD follows ip through DAD on a goroutine of its own, one per
// address. Passed: the backoff is reset and announce, if any, runs.
// Failed, or gone while key still announces it (the kernel deleted it):
// the failure is recorded and every Service is synced again, which
// withdraws it (announceLocal sees the backoff). Still tentative after
// dadWait: nothing is announced; the next sync looks again.
func (a *announcer) watchDAD(link netlink.Link, ip net.IP, key string, announce func()) {
	if _, busy := a.dadWatching.LoadOrStore(ip.String(), struct{}{}); busy {
		return
	}
	go func() {
		defer a.dadWatching.Delete(ip.String())
		for deadline := time.Now().Add(dadWait); ; time.Sleep(dadPoll) {
			switch a.lookDAD(link, ip) {
			case dadPassed:
				a.dadFailed.Delete(ip.String())
				if announce != nil {
					announce()
				}
				return
			case dadGone:
				if _, ours := a.announced.Load(key); !ours {
					return // withdrawn here, not by DAD
				}
				a.failDAD(ip)
				return
			case dadFailedState:
				a.failDAD(ip)
				return
			}
			if time.Now().After(deadline) {
				logging.Info(a.logger, "op", "dad", "ip", ip, "msg", "still tentative: not announced", "waited", dadWait)
				return
			}
		}
	}()
}

// failDAD records that ip failed DAD and schedules the retry. The sync
// that follows withdraws it on every Service that announces it.
func (a *announcer) failDAD(ip net.IP) {
	b := dadRetryMin
	if v, ok := a.dadFailed.Load(ip.String()); ok {
		b = min(v.(dadRetry).backoff*2, dadRetryMax)
	}
	a.dadFailed.Store(ip.String(), dadRetry{backoff: b, until: time.Now().Add(b)})
	dadFailures.Inc()
	logging.Info(a.logger, "op", "dad", "ip", ip, "retry", b,
		"msg", "duplicate address detection failed: another host on the link has this address")
	a.forceSync()
	time.AfterFunc(b, a.forceSync)
}

// forceSync syncs every Service again; nothing before a client is set.
func (a *announcer) forceSync() {
	if a.client != nil {
		a.client.ForceSync()
	}
}
