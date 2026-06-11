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
// service, and per-pool state keyed on AllocateRequest.pool.
//
// Config (env):
//
//	SIDECAR_SOCKET    Unix socket path (default /var/run/purelb/ipam.sock)
//	SIDECAR_PROVIDER  provider name (cosmetic; default "test")
//	SIDECAR_POOL_CIDR IPv4 CIDR to allocate from (default 10.20.30.0/24)
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ipamv1 "purelb.io/api/ipam/v1"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// pool is a single in-memory IPv4 pool over a CIDR.
type pool struct {
	cidr     *net.IPNet
	bySvc    map[string]net.IP // service -> allocated IP (idempotency)
	inUse    map[string]string // ip string -> service
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
	for ip := firstHost(p.cidr); p.cidr.Contains(ip); ip = nextIP(ip) {
		s := ip.String()
		if _, taken := p.inUse[s]; taken {
			continue
		}
		p.inUse[s] = svc
		p.bySvc[svc] = ip
		return ip, nil
	}
	return nil, status.Error(codes.ResourceExhausted, "pool exhausted")
}

func (p *pool) release(svc string) {
	if ip, ok := p.bySvc[svc]; ok {
		delete(p.inUse, ip.String())
		delete(p.bySvc, svc)
	}
}

func (p *pool) size() uint64 {
	ones, bits := p.cidr.Mask.Size()
	return 1 << uint(bits-ones)
}

type server struct {
	ipamv1.UnimplementedIPAMServer
	mu       sync.Mutex
	provider string
	cidr     *net.IPNet
	pools    map[string]*pool // pool name (SG) -> pool
}

func (s *server) poolFor(name string) *pool {
	if p, ok := s.pools[name]; ok {
		return p
	}
	p := newPool(s.cidr)
	s.pools[name] = p
	return p
}

func (s *server) Allocate(_ context.Context, req *ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ip, err := s.poolFor(req.Pool).allocate(req.Service)
	if err != nil {
		return nil, err
	}
	return &ipamv1.AllocateResponse{Ips: []*ipamv1.AllocatedIP{{Ip: ip.String()}}}, nil
}

func (s *server) Release(_ context.Context, req *ipamv1.ReleaseRequest) (*ipamv1.ReleaseResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// This demo treats every Release as "release all of the service's IPs"
	// since each service holds at most one address here.
	s.poolFor(req.Pool).release(req.Service)
	return &ipamv1.ReleaseResponse{}, nil
}

func (s *server) Stats(_ context.Context, req *ipamv1.StatsRequest) (*ipamv1.StatsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.poolFor(req.Pool)
	return &ipamv1.StatsResponse{
		InUseV4:          uint64(len(p.inUse)),
		SizeV4:           p.size(),
		HasKnownCapacity: true,
		DisplayAddresses: []string{p.cidr.String()},
	}, nil
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

func main() {
	socket := env("SIDECAR_SOCKET", "/var/run/purelb/ipam.sock")
	provider := env("SIDECAR_PROVIDER", "test")
	cidrStr := env("SIDECAR_POOL_CIDR", "10.20.30.0/24")

	_, cidr, err := net.ParseCIDR(cidrStr)
	if err != nil {
		log.Fatalf("invalid SIDECAR_POOL_CIDR %q: %v", cidrStr, err)
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
		cidr:     cidr,
		pools:    map[string]*pool{},
	})
	log.Printf("test-sidecar %q listening on %s, pool %s", provider, socket, cidr)
	if err := s.Serve(lis); err != nil {
		log.Fatal(fmt.Errorf("serve: %w", err))
	}
}
