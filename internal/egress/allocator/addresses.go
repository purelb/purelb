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

package allocator

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/vishvananda/netlink/nl"
	corev1 "k8s.io/api/core/v1"

	coreallocator "purelb.io/internal/allocator"
	"purelb.io/internal/egress"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Allocated=False reasons (§4.8).
const (
	reasonPoolExhausted     = "PoolExhausted"
	reasonPoolNotFound      = "PoolNotFound"
	reasonPoolRequired      = "PoolRequired"
	reasonPinUnavailable    = "PinUnavailable"
	reasonFamilyUnavailable = "FamilyUnavailable"
	reasonIPAMUnavailable   = "IPAMUnavailable"
)

// address is one address the policy holds or references.
type address struct {
	ip       netip.Addr
	identity string // a PerPod address's identity
	shared   bool   // a serviceRef Service's address, referenced only
	key      string // the pool's owner key (the sidecar key)
	entry    string // the pool entry that holds it, "" in an external pool
}

// allocate makes sure the policy holds its addresses (§5.4) and returns
// them, ordered: a dedicated address per family, one per family for each
// PerPod identity, or the serviceRef Service's addresses, referenced. It
// keeps every address it has; an allocation error never removes one.
func (r *Reconciler) allocate(run *run) []address {
	p := run.p
	have := r.heldAddresses(p)

	if run.svc != nil {
		return r.referenceService(run, have)
	}

	entry := p.spec.Pool
	if entry != "" && !r.alloc.HasPoolEntry(run.group.Name, entry) {
		run.conds.fail(condAllocated, reasonPoolNotFound, "ServiceGroup %s has no pool %q", run.group.Name, entry)
		return have
	}
	if entry == "" && groupMultiPool(run.group) {
		run.conds.fail(condAllocated, reasonPoolRequired, "ServiceGroup %s allocates from several pools; set pool", run.group.Name)
		return have
	}
	families, ok := r.families(run, entry)
	if !ok {
		return have
	}

	if p.affinity() == "PerPod" {
		var out []address
		for _, id := range identities(run) {
			out = append(out, r.allocateOwner(run, egress.SidecarKey(p.ref, id), id, entry, families, have)...)
		}
		return out
	}
	return r.allocateOwner(run, egress.SidecarKey(p.ref, ""), "", entry, families, have)
}

// allocateOwner makes sure one owner key holds an address per family,
// allocating what it lacks: the pins, or the next free address. A held
// address the policy no longer fits (another family, entry or pin) isn't
// kept, so its replacement is allocated first and it is retired (§8.9).
func (r *Reconciler) allocateOwner(run *run, key, identity, entry string, families []int, have []address) []address {
	p := run.p
	var mine []address
	var existing []net.IP
	for _, a := range have {
		if a.key == key && r.fits(run, a, identity, entry, families) {
			mine = append(mine, a)
			existing = append(existing, net.IP(a.ip.AsSlice()))
		}
	}
	req := coreallocator.AllocRequest{Key: key, Families: families, Existing: existing, Range: entry,
		Policy: coreallocator.AllFamilies}
	if p.spec.IPFamilyPolicy == "PreferDualStack" || p.spec.IPFamilyPolicy == "" {
		req.Policy = coreallocator.AnyFamily
	}
	if len(p.spec.Addresses) > 0 && identity == "" {
		pins, ok := r.pins(run, entry, mine)
		if !ok {
			return mine
		}
		if len(pins) == 0 {
			return mine
		}
		req = coreallocator.AllocRequest{Key: key, Explicit: pins}
	} else if holdsFamilies(mine, families) {
		return mine // nothing to allocate
	}
	ips, err := r.alloc.AllocateEgress(run.group.Name, req)
	if err != nil {
		run.conds.fail(condAllocated, allocationReason(err, len(req.Explicit) > 0), "%v", err)
	}
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip); ok {
			ent, _ := r.alloc.PoolEntryOf(run.group.Name, ip)
			mine = append(mine, address{ip: a.Unmap(), identity: identity, key: key, entry: ent})
		}
	}
	return mine
}

// holdsFamilies reports whether mine has an address in every family.
func holdsFamilies(mine []address, families []int) bool {
	for _, f := range families {
		if !slices.ContainsFunc(mine, func(a address) bool { return a.ip.Is6() == (f == nl.FAMILY_V6) }) {
			return false
		}
	}
	return true
}

// fits reports whether a held address still fits the policy: a family it
// wants, inside the group's pool and entry, and one of the pins if any.
func (r *Reconciler) fits(run *run, a address, identity, entry string, families []int) bool {
	fam := nl.FAMILY_V4
	if a.ip.Is6() {
		fam = nl.FAMILY_V6
	}
	if !slices.Contains(families, fam) {
		return false
	}
	ip := net.IP(a.ip.AsSlice())
	ent, in := r.alloc.PoolEntryOf(run.group.Name, ip)
	if !in && !r.alloc.PoolIsExternal(run.group.Name) || in && entry != "" && ent != entry {
		return false
	}
	if pins := run.p.spec.Addresses; len(pins) > 0 && identity == "" {
		return slices.ContainsFunc(pins, func(s string) bool {
			pin, err := netip.ParseAddr(s)
			return err == nil && pin.Unmap() == a.ip
		})
	}
	return true
}

// pins returns the pinned addresses not yet held, checking each lies in
// the group's pool (and entry, when set).
func (r *Reconciler) pins(run *run, entry string, mine []address) ([]net.IP, bool) {
	var out []net.IP
	for _, s := range run.p.spec.Addresses {
		a, err := netip.ParseAddr(s)
		if err != nil {
			continue // CEL rejects it at admission
		}
		held := false
		for _, m := range mine {
			held = held || m.ip == a.Unmap()
		}
		if held {
			continue
		}
		ip := net.IP(a.AsSlice())
		ent, in := r.alloc.PoolEntryOf(run.group.Name, ip)
		if (!in && !r.alloc.PoolIsExternal(run.group.Name)) || (entry != "" && ent != entry) {
			run.conds.fail(condAllocated, reasonPinUnavailable, "%s isn't in ServiceGroup %s pool %q", s, run.group.Name, entry)
			return nil, false
		}
		out = append(out, ip)
	}
	return out, true
}

// referenceService holds the serviceRef Service's addresses in its pool,
// so its release can't free them while the policy uses them (D31). An
// address already held stays held when renewing its hold fails.
func (r *Reconciler) referenceService(run *run, have []address) []address {
	key := egress.SidecarKey(run.p.ref, "")
	var out []address
	for _, ing := range run.svc.Status.LoadBalancer.Ingress {
		a, err := netip.ParseAddr(ing.IP)
		if err != nil {
			continue
		}
		ip := net.IP(a.AsSlice())
		if _, err := r.alloc.AllocateEgress(run.group.Name, coreallocator.AllocRequest{
			Key: key, Explicit: []net.IP{ip}, Reference: true}); err != nil {
			run.conds.fail(condAllocated, reasonPinUnavailable, "can't hold Service %s address %s: %v", run.svc.Name, ing.IP, err)
			for _, h := range have {
				if h.shared && h.ip == a.Unmap() {
					out = append(out, h)
				}
			}
			continue
		}
		ent, _ := r.alloc.PoolEntryOf(run.group.Name, ip)
		out = append(out, address{ip: a.Unmap(), shared: true, key: key, entry: ent})
	}
	if len(out) == 0 {
		run.conds.fail(condAllocated, reasonPinUnavailable, "Service %s has no address yet", run.svc.Name)
	}
	return out
}

// families is the families to allocate in: the policy's ipFamilyPolicy,
// within what the pool (or entry) offers. RequireDualStack on a pool
// missing a family fails (FamilyUnavailable); SingleStack takes the
// cluster's primary family.
func (r *Reconciler) families(run *run, entry string) ([]int, bool) {
	v4, v6 := r.alloc.PoolFamilies(run.group.Name, entry)
	switch run.p.spec.IPFamilyPolicy {
	case "SingleStack":
		f := r.primaryFamily()
		if (f == nl.FAMILY_V4 && !v4) || (f == nl.FAMILY_V6 && !v6) {
			run.conds.fail(condAllocated, reasonFamilyUnavailable, "the pool has no address in the cluster's primary family")
			return nil, false
		}
		return []int{f}, true
	case "RequireDualStack":
		if !v4 || !v6 {
			run.conds.fail(condAllocated, reasonFamilyUnavailable, "RequireDualStack, but the pool has only one family")
			return nil, false
		}
	}
	var out []int
	if v4 {
		out = append(out, nl.FAMILY_V4)
	}
	if v6 {
		out = append(out, nl.FAMILY_V6)
	}
	return out, true
}

// primaryFamily is the cluster's primary family: the kubernetes
// Service's first.
func (r *Reconciler) primaryFamily() int {
	obj, exists, _ := r.services.GetByKey("default/kubernetes")
	if exists {
		if fams := obj.(*corev1.Service).Spec.IPFamilies; len(fams) > 0 && fams[0] == corev1.IPv6Protocol {
			return nl.FAMILY_V6
		}
	}
	return nl.FAMILY_V4
}

// identities are the PerPod identities of the selected pods, in order.
func identities(run *run) []string {
	var ids []string
	for _, ms := range run.sel.nodes {
		for _, m := range ms {
			if m.identity != "" {
				ids = append(ids, m.identity)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func groupMultiPool(sg *purelbv2.ServiceGroup) bool {
	return (sg.Spec.Local != nil && sg.Spec.Local.MultiPool) || (sg.Spec.Remote != nil && sg.Spec.Remote.MultiPool)
}

// allocationReason classifies an allocation error.
func allocationReason(err error, pinned bool) string {
	switch {
	case errors.Is(err, coreallocator.ErrPoolEntryNotFound):
		return reasonPoolNotFound
	case pinned:
		return reasonPinUnavailable
	case strings.Contains(err.Error(), "exhausted"):
		return reasonPoolExhausted
	}
	return reasonIPAMUnavailable
}

// heldAddresses are the addresses the policy's EgressEndpoints record as
// Active.
func (r *Reconciler) heldAddresses(p *policy) []address {
	eps := r.ownedEndpoints(p.ref)
	var out []address
	for _, ep := range eps {
		if ep.Spec.State != stateActive {
			continue
		}
		a, err := netip.ParseAddr(ep.Spec.Address)
		if err != nil {
			continue
		}
		out = append(out, address{ip: a, identity: ep.Spec.Identity, shared: ep.Spec.SharedWith != nil,
			key: egress.SidecarKey(p.ref, ep.Spec.Identity), entry: ep.Spec.Pool})
	}
	return out
}
