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
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"

	ipamv1 "purelb.io/api/ipam/v1"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// defaultSidecarSocket is the Unix domain socket path used when a
// ServiceGroupExternalSpec omits the Socket field.
const defaultSidecarSocket = "/var/run/purelb/ipam.sock"

// maxAllocateResponseIPs caps how many IPs we accept from a single
// Allocate response. A conforming sidecar returns at most one per family
// (so ≤2); a larger number indicates a buggy sidecar and we reject it
// rather than program a flood of addresses onto a service.
const maxAllocateResponseIPs = 16

// SidecarPool is a Pool backed by an external IPAM sidecar reached over
// gRPC. Every mutating operation proxies to the sidecar; display
// accessors are pure reads of a cached Stats response (refreshed by the
// status writer, never by the accessors themselves).
//
// The sidecar owns allocation state and persistence — see the operator
// docs for the sidecar implementation contract. PureLB holds no
// authoritative IPAM state for these pools.
type SidecarPool struct {
	name     string
	provider string
	announce string // "local" | "remote"
	logger   log.Logger
	client   ipamv1.IPAMClient
	stats    atomic.Pointer[ipamv1.StatsResponse]
}

// NewSidecarPool builds a SidecarPool that talks to the sidecar over conn.
func NewSidecarPool(name string, log log.Logger, spec purelbv2.ServiceGroupExternalSpec, conn *grpc.ClientConn) *SidecarPool {
	return &SidecarPool{
		name:     name,
		provider: spec.Provider,
		announce: spec.Announce,
		logger:   log,
		client:   ipamv1.NewIPAMClient(conn),
	}
}

// --- Pool interface: mutating methods proxy to gRPC. ---

func (p *SidecarPool) AssignNext(ctx context.Context, svc *v1.Service) error {
	resp, err := p.client.Allocate(ctx, &ipamv1.AllocateRequest{
		Pool:       p.name,
		Service:    namespacedName(svc),
		SharingKey: svc.Annotations[purelbv2.SharingAnnotation],
		Selector: &ipamv1.AllocateRequest_Families{Families: &ipamv1.FamilyRequest{
			WantIpv4: wantsFamily(svc, v1.IPv4Protocol),
			WantIpv6: wantsFamily(svc, v1.IPv6Protocol),
		}},
	})
	if err != nil {
		return err
	}
	return p.applyAllocateResponse(svc, resp)
}

func (p *SidecarPool) Assign(ctx context.Context, ip net.IP, svc *v1.Service) error {
	resp, err := p.client.Allocate(ctx, &ipamv1.AllocateRequest{
		Pool:       p.name,
		Service:    namespacedName(svc),
		SharingKey: svc.Annotations[purelbv2.SharingAnnotation],
		Selector:   &ipamv1.AllocateRequest_Explicit{Explicit: &ipamv1.ExplicitIPs{Ips: []string{ip.String()}}},
	})
	if err != nil {
		return err
	}
	return p.applyAllocateResponse(svc, resp)
}

// applyAllocateResponse validates a sidecar Allocate response and
// programs the returned IPs onto the service's ingress status. Defends
// against a buggy sidecar returning no IPs (with no error) or an
// implausible flood of them.
func (p *SidecarPool) applyAllocateResponse(svc *v1.Service, resp *ipamv1.AllocateResponse) error {
	if len(resp.Ips) == 0 {
		return fmt.Errorf("sidecar %s returned no IPs and no error", p.provider)
	}
	if len(resp.Ips) > maxAllocateResponseIPs {
		return fmt.Errorf("sidecar %s returned %d IPs (max %d)", p.provider, len(resp.Ips), maxAllocateResponseIPs)
	}
	for _, a := range resp.Ips {
		ip := net.ParseIP(a.Ip)
		if ip == nil {
			return fmt.Errorf("sidecar %s returned unparseable IP %q", p.provider, a.Ip)
		}
		addIngress(p.logger, svc, ip)
	}
	return nil
}

func (p *SidecarPool) Release(ctx context.Context, service string) error {
	_, err := p.client.Release(ctx, &ipamv1.ReleaseRequest{
		Pool:    p.name,
		Service: service,
		Target:  &ipamv1.ReleaseRequest_All{All: true},
	})
	return err
}

func (p *SidecarPool) ReleaseIP(ctx context.Context, service string, ip net.IP) error {
	_, err := p.client.Release(ctx, &ipamv1.ReleaseRequest{
		Pool:    p.name,
		Service: service,
		Target:  &ipamv1.ReleaseRequest_Ip{Ip: ip.String()},
	})
	return err
}

// Notify is a no-op for sidecar pools — the sidecar persists its own
// state, so there's nothing for the allocator to replay at startup.
func (p *SidecarPool) Notify(_ context.Context, _ *v1.Service) error { return nil }

// AssignNextPerRange has no per-range concept for sidecar pools; it
// behaves like AssignNext (the sidecar decides how to satisfy the request).
func (p *SidecarPool) AssignNextPerRange(ctx context.Context, svc *v1.Service, _ []string) error {
	return p.AssignNext(ctx, svc)
}

// --- Pool interface: non-mutating reads (no ctx). ---

// Contains returns false: the service's pool annotation is the source of
// truth for which pool owns an address, so the Contains-iteration
// fallback in poolFor never needs to match a sidecar pool.
func (p *SidecarPool) Contains(_ net.IP) bool { return false }

// Overlaps returns false: a sidecar owns its own address space and there
// is no generic way to enumerate it for a cross-pool overlap check.
func (p *SidecarPool) Overlaps(_ Pool) bool { return false }

func (p *SidecarPool) PoolType() string   { return p.announce }
func (p *SidecarPool) IPAMSource() string { return p.provider }
func (p *SidecarPool) String() string     { return p.name }
func (p *SidecarPool) SkipIPv6DAD() bool  { return false }
func (p *SidecarPool) MultiPool() bool    { return false }
func (p *SidecarPool) BalancePools() bool { return false }

// refreshStats pulls the latest Stats from the sidecar and caches it.
// Called from the status writer (G1) immediately before buildStatus.
// Best-effort: on error the previously-cached stats are retained.
func (p *SidecarPool) refreshStats(ctx context.Context) error {
	resp, err := p.client.Stats(ctx, &ipamv1.StatsRequest{Pool: p.name})
	if err != nil {
		return err
	}
	p.stats.Store(resp)
	return nil
}

// Display accessors are pure reads of the cached Stats. They MUST NOT
// fire RPCs — refreshStats is the only place the cache is updated.

func (p *SidecarPool) InUseV4() int {
	if s := p.stats.Load(); s != nil {
		return int(s.InUseV4)
	}
	return 0
}

func (p *SidecarPool) InUseV6() int {
	if s := p.stats.Load(); s != nil {
		return int(s.InUseV6)
	}
	return 0
}

func (p *SidecarPool) SizeV4() uint64 {
	if s := p.stats.Load(); s != nil {
		return s.SizeV4
	}
	return 0
}

func (p *SidecarPool) SizeV6() uint64 {
	if s := p.stats.Load(); s != nil {
		return s.SizeV6
	}
	return 0
}

func (p *SidecarPool) HasKnownCapacity() bool {
	s := p.stats.Load()
	return s != nil && s.HasKnownCapacity
}

func (p *SidecarPool) DisplayAddresses() []string {
	if s := p.stats.Load(); s != nil {
		return s.DisplayAddresses
	}
	return nil
}

func (p *SidecarPool) InUse() int    { return p.InUseV4() + p.InUseV6() }
func (p *SidecarPool) Size() uint64  { return p.SizeV4() + p.SizeV6() }

// wantsFamily reports whether svc requests the given IP family. A service
// with no explicit IPFamilies (rare for LoadBalancer) is treated as
// wanting nothing in particular; the sidecar decides.
func wantsFamily(svc *v1.Service, family v1.IPFamily) bool {
	for _, f := range svc.Spec.IPFamilies {
		if f == family {
			return true
		}
	}
	return false
}

// newSidecarInstrumentation returns a unary client interceptor that
// records per-RPC count and latency labelled by socket, method, and
// gRPC status code. Replaces the archived grpc_prometheus library with
// ~20 lines we own.
func newSidecarInstrumentation(socket string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		code := status.Code(err).String()
		sidecarRPCTotal.WithLabelValues(socket, method, code).Inc()
		sidecarRPCDuration.WithLabelValues(socket, method).Observe(time.Since(start).Seconds())
		return err
	}
}
