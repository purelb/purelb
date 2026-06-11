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
	"net"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1 "purelb.io/api/ipam/v1"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// fakeIPAM is a configurable in-process IPAM sidecar for tests. Unset
// funcs return empty success.
type fakeIPAM struct {
	ipamv1.UnimplementedIPAMServer
	allocateFunc func(*ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error)
	releaseFunc  func(*ipamv1.ReleaseRequest) (*ipamv1.ReleaseResponse, error)
	statsFunc    func(*ipamv1.StatsRequest) (*ipamv1.StatsResponse, error)

	allocateCalls int
	statsCalls    int
}

func (f *fakeIPAM) Allocate(_ context.Context, req *ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error) {
	f.allocateCalls++
	if f.allocateFunc != nil {
		return f.allocateFunc(req)
	}
	return &ipamv1.AllocateResponse{Ips: []*ipamv1.AllocatedIP{{Ip: "10.20.30.5"}}}, nil
}

func (f *fakeIPAM) Release(_ context.Context, req *ipamv1.ReleaseRequest) (*ipamv1.ReleaseResponse, error) {
	if f.releaseFunc != nil {
		return f.releaseFunc(req)
	}
	return &ipamv1.ReleaseResponse{}, nil
}

func (f *fakeIPAM) Stats(_ context.Context, req *ipamv1.StatsRequest) (*ipamv1.StatsResponse, error) {
	f.statsCalls++
	if f.statsFunc != nil {
		return f.statsFunc(req)
	}
	return &ipamv1.StatsResponse{}, nil
}

// startFakeIPAM spins up srv on an in-memory bufconn and returns a
// connected client conn. Both are torn down via t.Cleanup.
func startFakeIPAM(t *testing.T, srv ipamv1.IPAMServer) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	ipamv1.RegisterIPAMServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func newSidecarPoolForTest(t *testing.T, srv ipamv1.IPAMServer) *SidecarPool {
	conn := startFakeIPAM(t, srv)
	return NewSidecarPool("ext", log.NewNopLogger(),
		purelbv2.ServiceGroupExternalSpec{Provider: "test", Announce: "local"}, conn)
}

func extSvc(name string, families ...v1.IPFamily) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1.ServiceSpec{IPFamilies: families},
	}
}

func TestSidecarPool_AllocateRelease(t *testing.T) {
	p := newSidecarPoolForTest(t, &fakeIPAM{
		allocateFunc: func(req *ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error) {
			assert.Equal(t, "ext", req.Pool)
			assert.Equal(t, "default/svc1", req.Service)
			return &ipamv1.AllocateResponse{Ips: []*ipamv1.AllocatedIP{{Ip: "10.20.30.7"}}}, nil
		},
	})
	svc := extSvc("svc1", v1.IPv4Protocol)
	require.NoError(t, p.AssignNext(context.Background(), svc))
	require.Len(t, svc.Status.LoadBalancer.Ingress, 1)
	assert.Equal(t, "10.20.30.7", svc.Status.LoadBalancer.Ingress[0].IP)

	// Release is a thin proxy; just confirm it doesn't error.
	require.NoError(t, p.Release(context.Background(), "default/svc1"))
	require.NoError(t, p.ReleaseIP(context.Background(), "default/svc1", net.ParseIP("10.20.30.7")))
}

func TestSidecarPool_DefensiveEmptyResponse(t *testing.T) {
	p := newSidecarPoolForTest(t, &fakeIPAM{
		allocateFunc: func(*ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error) {
			return &ipamv1.AllocateResponse{}, nil // 0 IPs, no error — buggy sidecar
		},
	})
	err := p.AssignNext(context.Background(), extSvc("svc1", v1.IPv4Protocol))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no IPs and no error")
}

func TestSidecarPool_DefensiveTooManyIPs(t *testing.T) {
	flood := make([]*ipamv1.AllocatedIP, maxAllocateResponseIPs+1)
	for i := range flood {
		flood[i] = &ipamv1.AllocatedIP{Ip: "10.20.30.1"}
	}
	p := newSidecarPoolForTest(t, &fakeIPAM{
		allocateFunc: func(*ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error) {
			return &ipamv1.AllocateResponse{Ips: flood}, nil
		},
	})
	err := p.AssignNext(context.Background(), extSvc("svc1", v1.IPv4Protocol))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max")
}

func TestSidecarPool_GRPCStatusCodes(t *testing.T) {
	p := newSidecarPoolForTest(t, &fakeIPAM{
		allocateFunc: func(*ipamv1.AllocateRequest) (*ipamv1.AllocateResponse, error) {
			return nil, status.Error(codes.ResourceExhausted, "pool full")
		},
	})
	err := p.AssignNext(context.Background(), extSvc("svc1", v1.IPv4Protocol))
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err),
		"gRPC status code must propagate so the allocator can classify it")
}

func TestSidecarPool_ContextCancellation(t *testing.T) {
	p := newSidecarPoolForTest(t, &fakeIPAM{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	err := p.AssignNext(ctx, extSvc("svc1", v1.IPv4Protocol))
	require.Error(t, err)
	assert.Equal(t, codes.Canceled, status.Code(err))
}

func TestSidecarPool_StatsPureRead(t *testing.T) {
	fake := &fakeIPAM{
		statsFunc: func(*ipamv1.StatsRequest) (*ipamv1.StatsResponse, error) {
			return &ipamv1.StatsResponse{
				InUseV4: 3, SizeV4: 254, HasKnownCapacity: true,
				DisplayAddresses: []string{"10.20.30.0/24"},
			}, nil
		},
	}
	p := newSidecarPoolForTest(t, fake)

	// Before any refresh: accessors return zero values, fire NO RPCs.
	assert.Equal(t, 0, p.InUseV4())
	assert.Equal(t, uint64(0), p.SizeV4())
	assert.False(t, p.HasKnownCapacity())
	assert.Nil(t, p.DisplayAddresses())
	assert.Equal(t, 0, fake.statsCalls, "accessors must not call Stats")

	// One refresh populates the cache.
	require.NoError(t, p.refreshStats(context.Background()))
	assert.Equal(t, 1, fake.statsCalls)

	// Accessors now read the cache — still no further RPCs.
	assert.Equal(t, 3, p.InUseV4())
	assert.Equal(t, uint64(254), p.SizeV4())
	assert.True(t, p.HasKnownCapacity())
	assert.Equal(t, []string{"10.20.30.0/24"}, p.DisplayAddresses())
	assert.Equal(t, 1, fake.statsCalls, "accessors after refresh must not call Stats again")
}

func TestParsePool_External(t *testing.T) {
	a := New(log.NewNopLogger())

	// grpc.NewClient is lazy, so parsePool succeeds even though no sidecar
	// is listening on the socket — no RPC is made at parse time.
	spec := purelbv2.ServiceGroupSpec{
		External: &purelbv2.ServiceGroupExternalSpec{
			Provider: "acme", Socket: "/tmp/purelb-test-ipam.sock", Announce: "remote",
		},
	}
	pool, err := a.parsePool("ext-a", spec)
	require.NoError(t, err)
	sp, ok := pool.(*SidecarPool)
	require.True(t, ok, "external spec must yield a *SidecarPool")
	assert.Equal(t, "acme", sp.IPAMSource())
	assert.Equal(t, "remote", sp.PoolType())

	// A second SG on the SAME socket shares the connection.
	pool2, err := a.parsePool("ext-b", spec)
	require.NoError(t, err)
	require.NotNil(t, pool2)

	conns := 0
	a.sidecarConns.Range(func(_, _ interface{}) bool { conns++; return true })
	assert.Equal(t, 1, conns, "two SGs on the same socket must share one connection")

	a.closeAllSidecarConns()
}

func TestParsePool_ExternalDefaultSocket(t *testing.T) {
	a := New(log.NewNopLogger())
	spec := purelbv2.ServiceGroupSpec{
		External: &purelbv2.ServiceGroupExternalSpec{Provider: "acme", Announce: "local"},
	}
	pool, err := a.parsePool("ext", spec)
	require.NoError(t, err)
	require.NotNil(t, pool)
	// The default socket path was used (one conn registered under it).
	_, ok := a.sidecarConns.Load(defaultSidecarSocket)
	assert.True(t, ok, "empty Socket must fall back to defaultSidecarSocket")
	a.closeAllSidecarConns()
}
