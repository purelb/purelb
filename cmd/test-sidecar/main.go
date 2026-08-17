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

// Command test-sidecar is a minimal in-memory IPAM sidecar used for
// PureLB end-to-end testing. It is NOT production-grade: state lives in
// process memory only and is lost on restart (which is fine for tests —
// it exercises the idempotent-Allocate contract on the allocator side).
//
// It demonstrates the sidecar contract: self-configuration from env vars,
// stale-socket cleanup before bind, idempotent Allocate/Release keyed on
// service, per-pool state keyed on AllocateRequest.pool, and — because
// PureLB is dual-stack throughout — independent IPv4 and IPv6 pools
// driven by the request's FamilyRequest.
//
// Config (env):
//
//	SIDECAR_SOCKET     Unix socket path (default /var/run/purelb/ipam.sock)
//	SIDECAR_PROVIDER   provider name (cosmetic; default "test")
//	SIDECAR_POOL_CIDR  IPv4 CIDR to allocate from (default 10.20.30.0/24).
//	                   Empty disables IPv4.
//	SIDECAR_POOL_CIDR6 IPv6 CIDR to allocate from. REQUIRED for dual-stack
//	                   or IPv6-only Services; no default (see main).
//
// Deliberately NOT implemented, because nothing in PureLB's own e2e needs
// it and a sample that pretends to support it would mislead: sharing keys
// (AllocateRequest.sharing_key is ignored, so two services asking for the
// same shared address will conflict) and persistence.
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ipamv1 "purelb.io/api/ipam/v1"
)

// maxScan bounds the search for a free address. A /64 has 2^64 hosts, so
// an exhausted IPv6 pool would otherwise spin for the rest of time and
// present as a hung test rather than a failed one.
const maxScan = 1 << 20

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// pool is a single in-memory pool over one CIDR of one address family.
type pool struct {
	cidr  *net.IPNet
	bySvc map[string]net.IP // service -> allocated IP (idempotency)
	inUse map[string]string // ip string -> service
}

func newPool(cidr *net.IPNet) *pool {
	return &pool{cidr: cidr, bySvc: map[string]net.IP{}, inUse: map[string]string{}}
}

// allocate returns the existing IP for svc (idempotent) or the next free
// address in the CIDR.
func (p *pool) allocate(svc string) (net.IP, error) {
	if ip, ok := p.bySvc[svc]; ok {
		return ip, nil
	}
	scanned := 0
	for ip := firstHost(p.cidr); p.cidr.Contains(ip); ip = nextIP(ip) {
		if scanned++; scanned > maxScan {
			return nil, status.Errorf(codes.ResourceExhausted,
				"scanned %d addresses in %s without finding a free one", maxScan, p.cidr)
		}
		s := ip.String()
		if _, taken := p.inUse[s]; taken {
			continue
		}
		p.inUse[s] = svc
		p.bySvc[svc] = ip
		return ip, nil
	}
	return nil, status.Errorf(codes.ResourceExhausted, "pool %s exhausted", p.cidr)
}

// assign reserves a specific address, which is what an explicit request
// (spec.loadBalancerIP, or re-affirming an address the service already
// holds) means. Idempotent for the holder; a conflict for anyone else.
func (p *pool) assign(svc string, ip net.IP) error {
	if !p.cidr.Contains(ip) {
		return status.Errorf(codes.InvalidArgument, "%s is outside pool %s", ip, p.cidr)
	}
	s := ip.String()
	if holder, taken := p.inUse[s]; taken {
		if holder == svc {
			return nil
		}
		return status.Errorf(codes.AlreadyExists, "%s is already held by %s", s, holder)
	}
	// A service may hold at most one address per family here, so an
	// explicit request for a different address supersedes the old one.
	if old, ok := p.bySvc[svc]; ok {
		delete(p.inUse, old.String())
	}
	p.inUse[s] = svc
	p.bySvc[svc] = ip
	return nil
}

func (p *pool) release(svc string) {
	if ip, ok := p.bySvc[svc]; ok {
		delete(p.inUse, ip.String())
		delete(p.bySvc, svc)
	}
}

// releaseIP drops one specific address, and only if svc holds it.
func (p *pool) releaseIP(svc string, ip net.IP) {
	s := ip.String()
	if holder, ok := p.inUse[s]; !ok || holder != svc {
		return
	}
	delete(p.inUse, s)
	if held, ok := p.bySvc[svc]; ok && held.Equal(ip) {
		delete(p.bySvc, svc)
	}
}

// size is the number of addresses in the CIDR, saturating rather than
// wrapping. Go yields 0 for a shift at or beyond the operand width, so
// the naive `1 << (bits-ones)` reported a /64 as capacity ZERO while
// still claiming has_known_capacity — a ServiceGroup status showing
// addresses in use out of a total of none.
func (p *pool) size() uint64 {
	ones, bits := p.cidr.Mask.Size()
	if shift := bits - ones; shift < 64 {
		return uint64(1) << uint(shift)
	}
	return math.MaxUint64
}

// poolSet is one ServiceGroup's state: an independent pool per family.
type poolSet struct {
	v4 *pool // nil when IPv4 is not configured
	v6 *pool
}

// forIP picks the family pool an address belongs to. net.IP.To4 returns
// non-nil for IPv4-mapped IPv6 too, which is what we want: an allocator
// that hands us ::ffff:10.20.30.5 means the v4 pool.
func (ps *poolSet) forIP(ip net.IP) *pool {
	if ip.To4() != nil {
		return ps.v4
	}
	return ps.v6
}

type server struct {
	ipamv1.UnimplementedIPAMServer
	mu       sync.Mutex
	provider string
	cidr4    *net.IPNet
	cidr6    *net.IPNet
	pools    map[string]*poolSet // pool name (SG) -> per-family pools
}

func (s *server) poolFor(name string) *poolSet {
	if ps, ok := s.pools[name]; ok {
		return ps
	}
	ps := &poolSet{}
	if s.cidr4 != nil {
		ps.v4 = newPool(s.cidr4)
	}
	if s.cidr6 != nil {
		ps.v6 = newPool(s.cidr6)
	}
	s.pools[name] = ps
	return ps
}

func (s *server) Allocate(_ context.Context, req *ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.poolFor(req.Pool)

	switch sel := req.Selector.(type) {
	case *ipamv1.AllocateRequest_Explicit:
		return s.allocateExplicit(ps, req.Service, sel.Explicit.GetIps())
	case *ipamv1.AllocateRequest_Families:
		return s.allocateFamilies(ps, req.Service, sel.Families)
	default:
		// The previous version ignored the selector entirely and always
		// returned one IPv4 address, so an explicit request for a
		// specific address got a DIFFERENT address programmed onto the
		// service. Refusing an unset selector keeps that class of silent
		// substitution from coming back.
		return nil, status.Error(codes.InvalidArgument,
			"AllocateRequest.selector must be set to explicit or families")
	}
}

func (s *server) allocateExplicit(ps *poolSet, svc string, ips []string) (*ipamv1.AllocateResponse, error) {
	if len(ips) == 0 {
		return nil, status.Error(codes.InvalidArgument, "explicit selector with no IPs")
	}
	out := make([]*ipamv1.AllocatedIP, 0, len(ips))
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			return nil, status.Errorf(codes.InvalidArgument, "unparseable IP %q", raw)
		}
		p := ps.forIP(ip)
		if p == nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"no pool configured for the address family of %s", raw)
		}
		if err := p.assign(svc, ip); err != nil {
			return nil, err
		}
		out = append(out, &ipamv1.AllocatedIP{Ip: ip.String()})
	}
	return &ipamv1.AllocateResponse{Ips: out}, nil
}

func (s *server) allocateFamilies(ps *poolSet, svc string, fams *ipamv1.FamilyRequest) (*ipamv1.AllocateResponse, error) {
	want := []struct {
		want bool
		p    *pool
		name string
	}{
		{fams.GetWantIpv4(), ps.v4, "IPv4"},
		{fams.GetWantIpv6(), ps.v6, "IPv6"},
	}
	out := make([]*ipamv1.AllocatedIP, 0, 2)
	for _, w := range want {
		if !w.want {
			continue
		}
		if w.p == nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"%s requested but no %s pool is configured", w.name, w.name)
		}
		ip, err := w.p.allocate(svc)
		if err != nil {
			return nil, err
		}
		out = append(out, &ipamv1.AllocatedIP{Ip: ip.String()})
	}
	if len(out) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no address family requested")
	}
	return &ipamv1.AllocateResponse{Ips: out}, nil
}

func (s *server) Release(_ context.Context, req *ipamv1.ReleaseRequest) (*ipamv1.ReleaseResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.poolFor(req.Pool)

	// The oneof exists to prevent the "empty string releases all"
	// footgun, so honour it: releasing one family of a dual-stack service
	// must not silently drop the other.
	if t, ok := req.Target.(*ipamv1.ReleaseRequest_Ip); ok {
		ip := net.ParseIP(t.Ip)
		if ip == nil {
			return nil, status.Errorf(codes.InvalidArgument, "unparseable IP %q", t.Ip)
		}
		if p := ps.forIP(ip); p != nil {
			p.releaseIP(req.Service, ip)
		}
		return &ipamv1.ReleaseResponse{}, nil
	}
	for _, p := range []*pool{ps.v4, ps.v6} {
		if p != nil {
			p.release(req.Service)
		}
	}
	return &ipamv1.ReleaseResponse{}, nil
}

func (s *server) Stats(_ context.Context, req *ipamv1.StatsRequest) (*ipamv1.StatsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.poolFor(req.Pool)
	resp := &ipamv1.StatsResponse{HasKnownCapacity: true}
	if ps.v4 != nil {
		resp.InUseV4 = uint64(len(ps.v4.inUse))
		resp.SizeV4 = ps.v4.size()
		resp.DisplayAddresses = append(resp.DisplayAddresses, ps.v4.cidr.String())
	}
	if ps.v6 != nil {
		resp.InUseV6 = uint64(len(ps.v6.inUse))
		resp.SizeV6 = ps.v6.size()
		resp.DisplayAddresses = append(resp.DisplayAddresses, ps.v6.cidr.String())
	}
	return resp, nil
}

func firstHost(n *net.IPNet) net.IP {
	ip := make(net.IP, len(n.IP))
	copy(ip, n.IP)
	return nextIP(ip) // skip the network address
}

func nextIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

// parsePoolCIDR parses one family's CIDR. An empty value disables that
// family; a wrong-family value is an error rather than a silent
// misconfiguration that would only show up as "IPv6 requested but no IPv6
// pool is configured" at allocation time.
func parsePoolCIDR(envName, value string, wantV6 bool) (*net.IPNet, error) {
	if value == "" {
		return nil, nil
	}
	_, cidr, err := net.ParseCIDR(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", envName, value, err)
	}
	if isV6 := cidr.IP.To4() == nil; isV6 != wantV6 {
		family := map[bool]string{false: "IPv4", true: "IPv6"}
		return nil, fmt.Errorf("%s expects an %s CIDR, got %s (%s)",
			envName, family[wantV6], value, family[isV6])
	}
	return cidr, nil
}

func main() {
	socket := env("SIDECAR_SOCKET", "/var/run/purelb/ipam.sock")
	provider := env("SIDECAR_PROVIDER", "test")

	cidr4, err := parsePoolCIDR("SIDECAR_POOL_CIDR", env("SIDECAR_POOL_CIDR", "10.20.30.0/24"), false)
	if err != nil {
		log.Fatal(err)
	}
	// No default, deliberately, and this is the one asymmetry with IPv4.
	// A default IPv6 pool would have to be a ULA range, which is not
	// announceable on any real cluster subnet -- so a dual-stack Service
	// would quietly receive an address that cannot work, and the operator
	// would have to reverse-engineer why the VIP is unreachable. Leaving
	// it unset instead produces "IPv6 requested but no IPv6 pool is
	// configured" at allocation time, which names its own fix. The IPv4
	// default is equally unannounceable, but it predates this and
	// changing it would break existing deployments.
	cidr6, err := parsePoolCIDR("SIDECAR_POOL_CIDR6", env("SIDECAR_POOL_CIDR6", ""), true)
	if err != nil {
		log.Fatal(err)
	}
	if cidr4 == nil && cidr6 == nil {
		log.Fatal("both SIDECAR_POOL_CIDR and SIDECAR_POOL_CIDR6 are empty; nothing to allocate from")
	}

	// Stale-socket cleanup before bind (sidecar contract).
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		log.Fatalf("removing stale socket %s: %v", socket, err)
	}
	lis, err := net.Listen("unix", socket)
	if err != nil {
		log.Fatalf("listening on %s: %v", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		log.Fatalf("chmod %s: %v", socket, err)
	}

	s := grpc.NewServer()
	ipamv1.RegisterIPAMServer(s, &server{
		provider: provider,
		cidr4:    cidr4,
		cidr6:    cidr6,
		pools:    map[string]*poolSet{},
	})
	log.Printf("test-sidecar %q listening on %s, v4=%v v6=%v", provider, socket, cidr4, cidr6)
	if err := s.Serve(lis); err != nil {
		log.Fatal(fmt.Errorf("serve: %w", err))
	}
}
