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
	"fmt"
	"net"

	"github.com/vishvananda/netlink/nl"

	"purelb.io/internal/egresshook"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// SetEgress sets the egress hook. Call it before the allocator runs; the
// default is egresshook.Noop.
func (a *Allocator) SetEgress(h egresshook.Allocator) {
	a.egress = h
}

// EgressGroup returns the ServiceGroup, and its pool, that an egress
// policy in namespace ns uses: group when set, else the namespace's
// default group, as for an unannotated Service (§5.2). The same namespace
// rules as Services apply, and one more: a group listing namespaces
// serves egress only in those, enforcing or not. The ServiceGroup is the
// allocator's snapshot copy; don't modify it.
func (a *Allocator) EgressGroup(ns, group string) (*purelbv2.ServiceGroup, Pool, error) {
	st := a.Snapshot()
	if group == "" {
		b, bound := st.nsToPools[ns]
		switch {
		case bound && b.def != "":
			group = b.def
		case bound && b.enforce:
			return nil, nil, fmt.Errorf("%w: %s", ErrAmbiguousDefault, b.defErr)
		default:
			group = defaultPoolName
		}
	}
	pool, err := st.checked(ns, group)
	if err != nil {
		return nil, nil, err
	}
	sg := st.sgByName[group]
	if len(sg.Spec.Namespaces) > 0 && !purelbv2.ServesNamespace(sg, ns) {
		return nil, nil, fmt.Errorf("%w: ServiceGroup %q doesn't list namespace %q", ErrNamespaceDenied, group, ns)
	}
	return sg, pool, nil
}

// NamespaceBindings returns the namespace bindings of the current config,
// for scoping a ClusterEgress's namespaces with the boundary Services use.
func (a *Allocator) NamespaceBindings() purelbv2.NamespaceBindings {
	return a.Snapshot().bindings
}

// ServiceGroup returns the named ServiceGroup and its pool, if both
// exist. The ServiceGroup is the allocator's snapshot copy; don't modify
// it.
func (a *Allocator) ServiceGroup(name string) (*purelbv2.ServiceGroup, Pool, bool) {
	st := a.Snapshot()
	sg, ok := st.sgByName[name]
	if !ok {
		return nil, nil, false
	}
	pool, ok := st.pools[name]
	return sg, pool, ok
}

// AllocateEgress allocates req from group's pool for egress, keeping the
// pool's statistics current. The pools are populated first, as for a
// Service, so an egress address is never one an existing owner holds.
func (a *Allocator) AllocateEgress(group string, req AllocRequest) ([]net.IP, error) {
	st := a.Snapshot()
	pool, ok := st.pools[group]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownPool, group)
	}
	a.populateFromExisting(st.pools)
	if !a.populated.Load() {
		return nil, errors.New("allocator pools not populated yet")
	}
	ctx, cancel := a.poolContext()
	defer cancel()
	ips, err := pool.Allocate(ctx, req)
	a.updateStats(pool)
	return ips, err
}

// ReleaseEgress releases key's address ip in group's pool, or all of
// key's addresses when ip is nil.
func (a *Allocator) ReleaseEgress(group, key string, ip net.IP) error {
	pool, ok := a.Snapshot().pools[group]
	if !ok {
		return nil
	}
	ctx, cancel := a.poolContext()
	defer cancel()
	var err error
	if ip == nil {
		err = pool.Release(ctx, key)
	} else {
		err = pool.ReleaseIP(ctx, key, ip)
	}
	a.updateStats(pool)
	return err
}

// PoolEntryOf returns the name of the entry of group's pool that holds
// ip, and whether there is one. An external pool has no entries.
func (a *Allocator) PoolEntryOf(group string, ip net.IP) (string, bool) {
	if lp, ok := a.localPool(group); ok {
		return lp.EntryOf(ip)
	}
	return "", false
}

// HasPoolEntry reports whether group's pool has an entry name. An
// external pool accepts any name: its sidecar decides.
func (a *Allocator) HasPoolEntry(group, name string) bool {
	if lp, ok := a.localPool(group); ok {
		return lp.HasEntry(name)
	}
	_, ok := a.Snapshot().pools[group]
	return ok
}

func (a *Allocator) localPool(group string) (LocalPool, bool) {
	switch p := a.Snapshot().pools[group].(type) {
	case LocalPool:
		return p, true
	case *LocalPool:
		return *p, true
	}
	return LocalPool{}, false
}

// PoolFamilies reports which families group's pool, or its entry when
// entry is set, can allocate. An external pool may serve either; its
// sidecar decides.
func (a *Allocator) PoolFamilies(group, entry string) (v4, v6 bool) {
	lp, ok := a.localPool(group)
	if !ok {
		return true, true
	}
	if entry == "" {
		return len(lp.v4Ranges) > 0, len(lp.v6Ranges) > 0
	}
	return len(lp.entryRanges(nl.FAMILY_V4, entry)) > 0, len(lp.entryRanges(nl.FAMILY_V6, entry)) > 0
}

// PoolIsExternal reports whether group's pool is an external IPAM
// sidecar's.
func (a *Allocator) PoolIsExternal(group string) bool {
	_, ok := a.Snapshot().pools[group].(*SidecarPool)
	return ok
}
