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

package election

import (
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	"github.com/vishvananda/netlink"

	"purelb.io/internal/logging"
)

// Default hold-downs for LinkDamper. A link must stay down this long
// before its subnets leave the Lease, which rides out a brief carrier
// blip without moving addresses. It must stay up longer before they come
// back, because coming back can move addresses to this node again: a
// flapping link settles before it attracts them.
const (
	DefaultLinkDownHoldDown = 2 * time.Second
	DefaultLinkUpHoldDown   = 10 * time.Second
)

// LinkDamper decides whether a link the election scans for subnets counts
// as up. A down link's subnets leave this node's Lease, so its local-pool
// addresses move to a node that can still reach the subnet. Operational
// state UP or UNKNOWN counts as up: many virtual links never report a
// carrier. Changes are damped by a hold-down each way, and each change
// that reaches the Lease is logged and counted.
//
// The link state is owned by the goroutine that collects subnets (Start,
// then the renewLoop), so it needs no lock. Watch runs on its own
// goroutine and shares only the published set of scanned names.
type LinkDamper struct {
	downHold, upHold time.Duration
	logger           log.Logger

	// renew asks for a Lease renewal now (Election.RenewNow). Set by
	// SetRenew before the election starts.
	renew func()

	now       func() time.Time
	afterFunc func(time.Duration, func())

	links   map[string]*linkState
	scanned atomic.Pointer[map[string]struct{}]
}

type linkState struct {
	effectiveUp bool      // what the Lease reflects
	rawUp       bool      // what the link reports
	since       time.Time // when rawUp last changed
	pending     bool      // a renewal is scheduled for the hold-down's end
}

// NewLinkDamper returns a LinkDamper with the given hold-downs.
func NewLinkDamper(downHold, upHold time.Duration, logger log.Logger) *LinkDamper {
	return &LinkDamper{
		downHold:  downHold,
		upHold:    upHold,
		logger:    logger,
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		links:     map[string]*linkState{},
	}
}

// SetRenew sets the function that asks for a Lease renewal now, normally
// Election.RenewNow. Call it before the election starts.
func (d *LinkDamper) SetRenew(renew func()) {
	d.renew = renew
}

func (d *LinkDamper) renewNow() {
	if d.renew != nil {
		d.renew()
	}
}

// operUp reports whether an operational state counts as up.
func operUp(s netlink.LinkOperState) bool {
	return s == netlink.OperUp || s == netlink.OperUnknown
}

// Up reports whether link counts as up for the election now, updating its
// hold-down. A link seen for the first time counts as it reports. A nil
// LinkDamper counts every link as up.
func (d *LinkDamper) Up(link netlink.Link) bool {
	if d == nil {
		return true
	}
	name := link.Attrs().Name
	raw := operUp(link.Attrs().OperState)
	now := d.now()

	st, ok := d.links[name]
	if !ok {
		d.links[name] = &linkState{effectiveUp: raw, rawUp: raw, since: now}
		return raw
	}
	if raw != st.rawUp {
		st.rawUp, st.since, st.pending = raw, now, false
	}
	if st.rawUp == st.effectiveUp {
		return st.effectiveUp
	}

	hold := d.upHold
	if !raw {
		hold = d.downHold
	}
	elapsed := now.Sub(st.since)
	if elapsed < hold {
		// Renew when the hold-down ends, so the change reaches the Lease
		// then rather than at the next periodic renewal.
		if !st.pending {
			st.pending = true
			d.afterFunc(hold-elapsed, d.renewNow)
		}
		return st.effectiveUp
	}

	st.effectiveUp = raw
	state := "down"
	if raw {
		state = "up"
	}
	logging.Info(d.logger, "op", "linkState", "interface", name, "state", state,
		"heldFor", elapsed.String(), "msg", "link change reaches the lease")
	linkChanges.WithLabelValues(name, state).Inc()
	return raw
}

// setScanned publishes the names of the links the election last scanned,
// so Watch reacts only to them.
func (d *LinkDamper) setScanned(names []string) {
	if d == nil {
		return
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	d.scanned.Store(&set)
}

func (d *LinkDamper) isScanned(name string) bool {
	set := d.scanned.Load()
	if set == nil {
		return false
	}
	_, ok := (*set)[name]
	return ok
}

// linkResubscribeDelay is how long Watch waits before subscribing again
// after a subscription fails or ends.
const linkResubscribeDelay = 5 * time.Second

// Watch asks for a Lease renewal whenever a scanned link changes, so a
// link event starts its hold-down at once instead of at the next periodic
// renewal. Other links, such as pod veths, are ignored. It returns when
// stop closes.
func (d *LinkDamper) Watch(stop <-chan struct{}) {
	for {
		ch := make(chan netlink.LinkUpdate, 64)
		done := make(chan struct{})
		err := netlink.LinkSubscribeWithOptions(ch, done, netlink.LinkSubscribeOptions{
			ErrorCallback: func(err error) {
				logging.Debug(d.logger, "op", "linkWatch", "error", err)
			},
		})
		if err != nil {
			logging.Info(d.logger, "op", "linkWatch", "error", err,
				"msg", "link changes reach the lease only at periodic renewals until the retry succeeds")
		} else {
			d.watch(stop, ch)
			close(done)
			// netlink's receive goroutine sends without watching done, so
			// drain until it closes the channel.
			for range ch {
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(linkResubscribeDelay):
		}
	}
}

// watch handles updates until stop closes or netlink ends the
// subscription.
func (d *LinkDamper) watch(stop <-chan struct{}, ch <-chan netlink.LinkUpdate) {
	for {
		select {
		case <-stop:
			return
		case u, ok := <-ch:
			if !ok {
				return
			}
			if d.isScanned(u.Attrs().Name) {
				d.renewNow()
			}
		}
	}
}
