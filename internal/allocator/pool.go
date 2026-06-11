// Copyright 2017 Google Inc.
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

package allocator

import (
	"context"
	"errors"
	"fmt"
	"net"

	v1 "k8s.io/api/core/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Port represents one port in use by a service.
type Port struct {
	Proto v1.Protocol
	Port  int
}

// String returns a text description of the port.
func (p Port) String() string {
	return fmt.Sprintf("%s/%d", p.Proto, p.Port)
}

type Key struct {
	Sharing string
}

// Pool describes the interface to code that manages pools of
// addresses. Mutating methods take a context so that pool
// implementations backed by network I/O (e.g. external IPAM sidecars)
// can honor the allocator's shutdown semantics; in-memory
// implementations may safely ignore it.
type Pool interface {
	// Notify notifies the pool of an existing address assignment, for
	// example, at startup time.
	Notify(ctx context.Context, svc *v1.Service) error
	AssignNext(ctx context.Context, svc *v1.Service) error
	Assign(ctx context.Context, ip net.IP, svc *v1.Service) error
	Release(ctx context.Context, service string) error
	// ReleaseIP releases a specific IP address for a service. Used during
	// IP family transitions (e.g., DualStack → SingleStack).
	ReleaseIP(ctx context.Context, service string, ip net.IP) error
	InUse() int
	Overlaps(Pool) bool
	Contains(net.IP) bool // FIXME: I'm not sure that we need this. It might be the case that we can always rely on the service's pool annotation to find to which pool an address belongs
	Size() uint64
	String() string

	// PoolType returns the type of this pool: "local" for addresses that
	// should be announced on the node's local interface (same subnet as nodes),
	// or "remote" for addresses that should be announced on the dummy interface
	// (different subnet, typically for BGP/routing scenarios or external IPAM).
	PoolType() string

	// IPAMSource identifies the source of address management. Returns
	// "Cluster" when PureLB's allocator is authoritative for the address
	// space, or the name of an external IPAM system (e.g. a sidecar
	// plugin's provider name) when allocation is delegated.
	IPAMSource() string

	// SkipIPv6DAD returns whether IPv6 Duplicate Address Detection should
	// be skipped for addresses from this pool. This can speed up address
	// configuration but should only be used when address conflicts are impossible.
	SkipIPv6DAD() bool

	// MultiPool returns whether this pool has multi-pool allocation enabled.
	// When true, services get one IP from each address range (per family)
	// that has active nodes.
	MultiPool() bool

	// BalancePools returns whether balanced allocation across ranges is enabled.
	// When true, new allocations pick the range with the fewest IPs in use.
	BalancePools() bool

	// AssignNextPerRange allocates one IP per address range (per family)
	// that has an active subnet. activeSubnets is the set of subnets with
	// healthy lbnodeagents. Returns error only if NO IPs could be allocated.
	AssignNextPerRange(ctx context.Context, svc *v1.Service, activeSubnets []string) error

	// InUseV4 returns the number of IPv4 addresses currently allocated.
	// Must be a pure in-memory read; implementations that wrap remote
	// state (e.g. sidecar IPAM) MUST cache and never fire RPCs from this
	// accessor.
	InUseV4() int

	// InUseV6 returns the number of IPv6 addresses currently allocated.
	// Same purity contract as InUseV4.
	InUseV6() int

	// SizeV4 returns the IPv4 capacity of the pool. May return 0 when
	// capacity is not knowable locally (use HasKnownCapacity to
	// disambiguate "0 capacity" from "unknown").
	SizeV4() uint64

	// SizeV6 returns the IPv6 capacity of the pool. Same semantics as SizeV4.
	SizeV6() uint64

	// HasKnownCapacity reports whether SizeV4/SizeV6 reflect true capacity.
	// False for pools backed by external systems whose total size is not
	// visible to the allocator (e.g. sidecar IPAM without a Stats reply).
	HasKnownCapacity() bool

	// DisplayAddresses returns a human-readable summary of the pool's
	// address scope, surfaced as .status.addresses. Pool implementations
	// choose their own format (CIDR list for local pools; external-system
	// descriptor for sidecar-IPAM pools).
	DisplayAddresses() []string
}

func sharingOK(existing, new *Key) error {
	if existing.Sharing == "" {
		return errors.New("existing service does not allow sharing")
	}
	if new.Sharing == "" {
		return errors.New("new service does not allow sharing")
	}
	if existing.Sharing != new.Sharing {
		return fmt.Errorf("sharing key %q does not match existing sharing key %q", new.Sharing, existing.Sharing)
	}
	return nil
}

// parsePool builds a Pool from a ServiceGroupSpec. It's a method on
// Allocator because the External branch needs the shared sidecar
// connection pool (getOrDialSidecar).
func (a *Allocator) parsePool(name string, group purelbv2.ServiceGroupSpec) (Pool, error) {
	if group.Local != nil {
		return NewLocalPool(name, a.logger,
			group.Local.V4Pool, group.Local.V6Pool,
			group.Local.V4Pools, group.Local.V6Pools,
			purelbv2.PoolTypeLocal, group.Local.SkipIPv6DAD,
			group.Local.MultiPool, group.Local.BalancePools)
	} else if group.Remote != nil {
		return NewLocalPool(name, a.logger,
			group.Remote.V4Pool, group.Remote.V6Pool,
			group.Remote.V4Pools, group.Remote.V6Pools,
			purelbv2.PoolTypeRemote, false,
			group.Remote.MultiPool, group.Remote.BalancePools)
	} else if group.External != nil {
		socket := group.External.Socket
		if socket == "" {
			socket = defaultSidecarSocket
		}
		conn, err := a.getOrDialSidecar(socket)
		if err != nil {
			return nil, fmt.Errorf("dial sidecar at %s: %w", socket, err)
		}
		return NewSidecarPool(name, a.logger, *group.External, conn), nil
	}

	return nil, fmt.Errorf("Pool must specify local, remote, or external")
}
