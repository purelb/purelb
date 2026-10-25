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

// Package egresshook is the only way the allocator, the announcers, the
// address guard and internal/k8s reach egress. Those packages never import
// egress itself (hack/check-egress-imports.sh enforces it); they call
// these interfaces, whose Noop implementation is the default and leaves
// load-balancer behaviour exactly as it is without egress.
//
// Hooks are called from the goroutine that owns the caller's state (the
// allocator's queue worker, the announcer's service-sync goroutine), so an
// implementation must return quickly and never block.
package egresshook

import (
	"net"

	"k8s.io/client-go/rest"
)

// Allocator is what the allocator asks egress.
type Allocator interface {
	// Synced reports whether egress has the state it needs to replay
	// (its informers have synced, or its CRDs don't exist). The allocator
	// doesn't consider its pools populated, and so allocates nothing,
	// until it is.
	Synced() bool

	// Replay records egress's allocations and reference holds into the
	// pools, after every pool rebuild and after the Services have been
	// replayed. record allocates ips in pool to key, as a reference hold
	// when reference is true.
	Replay(record func(pool, key string, ips []net.IP, reference bool) error)

	// ReleaseService is asked before the allocator releases a Service's
	// addresses in pool: all of them when ip is nil, else just ip. It
	// returns false when egress holds the address and will release it
	// for key itself when its hold ends; the allocator then skips the
	// release.
	ReleaseService(pool, key string, ip net.IP) bool
}

// Agent is what the node agent's announcers ask and tell egress.
type Agent interface {
	// Referenced reports whether egress uses ip on this node, so a
	// Service withdrawing ip must leave it on its interface.
	Referenced(ip net.IP) bool

	// ServiceAddress reports that this node now announces (announced
	// true) or no longer announces a Service's address. key is the
	// Service's "namespace/name". It may repeat a report.
	ServiceAddress(key string, ip net.IP, announced bool)

	// PrepareShutdown runs at the start of a graceful shutdown, before
	// any address is withdrawn and while the lease stands: egress
	// withdraws the addresses it placed.
	PrepareShutdown()
}

// Placer is how egress places its own addresses on this node (D31):
// through the announcer's VIP path, on the work-queue goroutine, as a
// Service's address is placed. Shared addresses are the Service's and
// never come through here.
type Placer interface {
	// EgressInterface is the interface a local-pool address goes on,
	// chosen by the rules a Service's address follows.
	EgressInterface(ip net.IP) (ifindex int, name string, err error)

	// PlaceEgress adds ip for key, an "egress/<uid>" name no Service can
	// take, and keeps it renewed. A local address goes on its pool
	// interface and is announced by GARP or unsolicited NA (garpConfig)
	// while still reports true; a routed one goes on the dummy interface
	// with the pool's prefix.
	PlaceEgress(key string, ip net.IP, p EgressPlacement) error

	// WithdrawEgress removes ip placed for key, unless a Service on this
	// node announces it too.
	WithdrawEgress(key string, ip net.IP) error
}

// EgressPlacement is how PlaceEgress places one address.
type EgressPlacement struct {
	Routed bool
	// Subnet and Aggregation are a routed address's pool's.
	Subnet, Aggregation string
	// SkipDAD skips IPv6 duplicate address detection (local only).
	SkipDAD bool
	// Still reports whether this node still holds the address, before
	// each GARP or NA; nil means always.
	Still func() bool
}

// Controller is how internal/k8s runs egress's own objects in either
// process, on the same work queue as Services.
type Controller interface {
	// Start runs once, after the core caches have synced. It discovers
	// whether the egress CRDs exist and, if they do, starts egress's
	// informers, which add egress keys to the work queue with enqueue.
	// A priority key is synced ahead of every queued key (the claim path).
	Start(stop <-chan struct{}, cfg *rest.Config, enqueue func(key string, priority bool)) error

	// Sync processes one egress key on the work-queue goroutine. An
	// error requeues the key with backoff.
	Sync(key string) error

	// Keys returns every egress key, so ForceSync can reprocess them.
	Keys() []string

	// Live reports whether egress's own goroutines are turning over; an
	// error fails /healthz, as a wedged work item does.
	Live() error
}

// Noop is the default hook: no egress.
type Noop struct{}

var (
	_ Allocator  = Noop{}
	_ Agent      = Noop{}
	_ Controller = Noop{}
)

func (Noop) Synced() bool                                                  { return true }
func (Noop) Replay(func(pool, key string, ips []net.IP, ref bool) error)   {}
func (Noop) ReleaseService(string, string, net.IP) bool                    { return true }
func (Noop) Referenced(net.IP) bool                                        { return false }
func (Noop) ServiceAddress(string, net.IP, bool)                           {}
func (Noop) PrepareShutdown()                                              {}
func (Noop) Start(<-chan struct{}, *rest.Config, func(string, bool)) error { return nil }
func (Noop) Sync(string) error                                             { return nil }
func (Noop) Keys() []string                                                { return nil }
func (Noop) Live() error                                                   { return nil }
