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

func TestSidecarPool_NotifyNoopAndPerRange(t *testing.T) {
	fake := &fakeIPAM{}
	p := newSidecarPoolForTest(t, fake)

	// Notify is a no-op for sidecar pools and must NOT call the sidecar.
	require.NoError(t, p.Notify(context.Background(), extSvc("svc1", v1.IPv4Protocol)))
	assert.Equal(t, 0, fake.allocateCalls, "Notify must not allocate")

	// AssignNextPerRange delegates to AssignNext (the sidecar decides how
	// to satisfy the request; there is no per-range concept).
	svc := extSvc("svc2", v1.IPv4Protocol)
	require.NoError(t, p.AssignNextPerRange(context.Background(), svc, []string{"10.0.0.0/24"}))
	assert.Equal(t, 1, fake.allocateCalls, "AssignNextPerRange must allocate once")
	require.Len(t, svc.Status.LoadBalancer.Ingress, 1)
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

// TestUnassignTargetsNamedExternalPool covers the pool-targeting rule in
// Unassign. Getting it wrong in either direction is costly: skipping too
// eagerly leaks an address in a system PureLB does not own, and skipping
// never means one gRPC per configured sidecar on every Service deletion.
func TestUnassignTargetsNamedExternalPool(t *testing.T) {
	newPool := func(t *testing.T, name string, calls *int) *SidecarPool {
		f := &fakeIPAM{releaseFunc: func(*ipamv1.ReleaseRequest) (*ipamv1.ReleaseResponse, error) {
			*calls++
			return &ipamv1.ReleaseResponse{}, nil
		}}
		conn := startFakeIPAM(t, f)
		return NewSidecarPool(name, log.NewNopLogger(),
			purelbv2.ServiceGroupExternalSpec{Provider: "test", Announce: "local"}, conn)
	}

	t.Run("named pool is released, the other external pool is not asked", func(t *testing.T) {
		var mineCalls, theirsCalls int
		a := New(log.NewNopLogger())
		pools := map[string]Pool{
			"mine":   newPool(t, "mine", &mineCalls),
			"theirs": newPool(t, "theirs", &theirsCalls),
		}
		require.NoError(t, a.Unassign(pools, "ns/svc", "mine"))
		assert.Equal(t, 1, mineCalls, "the pool that held the address must be released")
		assert.Equal(t, 0, theirsCalls, "an unrelated sidecar must not be contacted")
	})

	t.Run("unknown pool falls back to asking every pool", func(t *testing.T) {
		var aCalls, bCalls int
		a := New(log.NewNopLogger())
		pools := map[string]Pool{
			"a": newPool(t, "a", &aCalls),
			"b": newPool(t, "b", &bCalls),
		}
		// "" means the pool could not be determined -- an un-annotated
		// Service, or a tombstone whose payload was lost. Releasing from
		// none of them would leak, so every pool is asked.
		require.NoError(t, a.Unassign(pools, "ns/svc", ""))
		assert.Equal(t, 1, aCalls)
		assert.Equal(t, 1, bCalls)
	})

	t.Run("a stale name does not skip local pools", func(t *testing.T) {
		var extCalls int
		a := New(log.NewNopLogger())
		local, err := NewLocalPool("local", log.NewNopLogger(),
			&purelbv2.AddressPool{Pool: "192.0.2.0/24", Subnet: "192.0.2.0/24", Aggregation: "default"},
			nil, nil, nil, purelbv2.PoolTypeLocal, false, false, false)
		require.NoError(t, err)
		pools := map[string]Pool{
			"local": local,
			"ext":   newPool(t, "ext", &extCalls),
		}
		// The annotation is user-editable, so it must never be able to stop
		// a local pool from releasing; only the (network-cost) external
		// pools are skipped on the strength of it.
		require.NoError(t, a.Unassign(pools, "ns/svc", "ext"))
		assert.Equal(t, 1, extCalls, "the named external pool is still released")
	})
}

// TestParsePoolValidatesExternalAnnounce covers the defence against a stale
// or pruned ServiceGroup CRD. The CRD constrains announce to local|remote,
// but if that constraint is missing the value reaches PoolType() verbatim,
// purelb.io/pool-type is written empty, and the node agent has no way to
// decide between a local interface and the dummy interface -- so it
// announces nothing and the allocator says nothing. Failing the parse turns
// that into a visible ParseFailed event on the ServiceGroup.
func TestParsePoolValidatesExternalAnnounce(t *testing.T) {
	a := New(log.NewNopLogger())

	for _, announce := range []string{"", "Local", "REMOTE", "bgp", "dummy"} {
		t.Run("rejected/"+announce, func(t *testing.T) {
			_, err := a.parsePool("ext", purelbv2.ServiceGroupSpec{
				External: &purelbv2.ServiceGroupExternalSpec{Provider: "p", Announce: announce},
			})
			assert.Error(t, err, "announce %q must not produce a pool", announce)
		})
	}

	for _, announce := range []string{purelbv2.PoolTypeLocal, purelbv2.PoolTypeRemote} {
		t.Run("accepted/"+announce, func(t *testing.T) {
			p, err := a.parsePool("ext", purelbv2.ServiceGroupSpec{
				External: &purelbv2.ServiceGroupExternalSpec{Provider: "p", Announce: announce},
			})
			require.NoError(t, err)
			assert.Equal(t, announce, p.PoolType(),
				"pool-type annotation is written from PoolType(); it must match announce")
		})
	}
}
