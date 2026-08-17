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

package main

import (
	"context"
	"math"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ipamv1 "purelb.io/api/ipam/v1"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	require.NoError(t, err)
	return n
}

// newServer builds a dual-stack sidecar over small pools, so exhaustion
// is reachable in a test rather than theoretical.
func newServer(t *testing.T) *server {
	t.Helper()
	return &server{
		provider: "test",
		cidr4:    mustCIDR(t, "10.20.30.0/29"), // .1 - .7
		cidr6:    mustCIDR(t, "fd00:30::/125"), // ::1 - ::7
		pools:    map[string]*poolSet{},
	}
}

func families(v4, v6 bool) *ipamv1.AllocateRequest_Families {
	return &ipamv1.AllocateRequest_Families{
		Families: &ipamv1.FamilyRequest{WantIpv4: v4, WantIpv6: v6},
	}
}

func ips(resp *ipamv1.AllocateResponse) []string {
	out := make([]string, 0, len(resp.GetIps()))
	for _, a := range resp.GetIps() {
		out = append(out, a.GetIp())
	}
	return out
}

func TestAllocateByFamily(t *testing.T) {
	tests := []struct {
		name    string
		v4, v6  bool
		wantLen int
		wantV4  bool
		wantV6  bool
	}{
		{name: "v4 only", v4: true, wantLen: 1, wantV4: true},
		{name: "v6 only", v6: true, wantLen: 1, wantV6: true},
		{name: "dual stack", v4: true, v6: true, wantLen: 2, wantV4: true, wantV6: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t)
			resp, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
				Pool: "sg", Service: "test/svc", Selector: families(tc.v4, tc.v6),
			})
			require.NoError(t, err)
			require.Len(t, resp.GetIps(), tc.wantLen)

			var sawV4, sawV6 bool
			for _, a := range ips(resp) {
				ip := net.ParseIP(a)
				require.NotNil(t, ip, "unparseable %q", a)
				if ip.To4() != nil {
					sawV4 = true
					assert.True(t, s.cidr4.Contains(ip), "%s outside %s", ip, s.cidr4)
				} else {
					sawV6 = true
					assert.True(t, s.cidr6.Contains(ip), "%s outside %s", ip, s.cidr6)
				}
			}
			assert.Equal(t, tc.wantV4, sawV4, "IPv4 presence")
			assert.Equal(t, tc.wantV6, sawV6, "IPv6 presence")
		})
	}
}

func TestAllocateIsIdempotentPerFamily(t *testing.T) {
	// The allocator re-Allocates on every resync, so a sidecar that hands
	// out a new address each time would renumber every service on a
	// timer. Both families must be stable, not just IPv4.
	s := newServer(t)
	req := &ipamv1.AllocateRequest{Pool: "sg", Service: "test/svc", Selector: families(true, true)}

	first, err := s.Allocate(context.Background(), req)
	require.NoError(t, err)
	second, err := s.Allocate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ips(first), ips(second))

	// A different service must not collide with it.
	other, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/other", Selector: families(true, true),
	})
	require.NoError(t, err)
	assert.NotEqual(t, ips(first), ips(other))
}

func TestAllocateExplicitReturnsTheRequestedAddress(t *testing.T) {
	// The bug this pins: the previous sidecar ignored req.Selector
	// entirely and always returned the next free IPv4. An explicit
	// request -- spec.loadBalancerIP, or re-affirming an address a
	// service already holds -- therefore got a DIFFERENT address
	// programmed onto the service, silently.
	s := newServer(t)
	want := []string{"10.20.30.5", "fd00:30::5"}
	resp, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/svc",
		Selector: &ipamv1.AllocateRequest_Explicit{
			Explicit: &ipamv1.ExplicitIPs{Ips: want},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, want, ips(resp))

	// Idempotent for the holder.
	again, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/svc",
		Selector: &ipamv1.AllocateRequest_Explicit{Explicit: &ipamv1.ExplicitIPs{Ips: want}},
	})
	require.NoError(t, err)
	assert.Equal(t, want, ips(again))
}

func TestAllocateExplicitRejections(t *testing.T) {
	s := newServer(t)
	_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/holder",
		Selector: &ipamv1.AllocateRequest_Explicit{
			Explicit: &ipamv1.ExplicitIPs{Ips: []string{"10.20.30.5"}},
		},
	})
	require.NoError(t, err)

	tests := []struct {
		name    string
		service string
		ips     []string
		code    codes.Code
	}{
		{name: "held by another service", service: "test/thief", ips: []string{"10.20.30.5"}, code: codes.AlreadyExists},
		{name: "outside the pool", service: "test/svc", ips: []string{"10.99.99.1"}, code: codes.InvalidArgument},
		{name: "unparseable", service: "test/svc", ips: []string{"not-an-ip"}, code: codes.InvalidArgument},
		{name: "no ips", service: "test/svc", ips: []string{}, code: codes.InvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
				Pool: "sg", Service: tc.service,
				Selector: &ipamv1.AllocateRequest_Explicit{
					Explicit: &ipamv1.ExplicitIPs{Ips: tc.ips},
				},
			})
			require.Error(t, err)
			assert.Equal(t, tc.code, status.Code(err))
		})
	}
}

func TestAllocateRejectsAnUnsetSelector(t *testing.T) {
	// Guessing a family is what made the old sidecar substitute
	// addresses. Refusing keeps that from returning.
	s := newServer(t)
	_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{Pool: "sg", Service: "test/svc"})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestAllocateRequiresTheFamilyToBeConfigured(t *testing.T) {
	s := newServer(t)
	s.cidr6 = nil
	_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/svc", Selector: families(false, true),
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestReleaseHonoursTheOneof(t *testing.T) {
	// Releasing one family of a dual-stack service must not drop the
	// other. The oneof exists precisely to make that expressible.
	s := newServer(t)
	resp, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/svc", Selector: families(true, true),
	})
	require.NoError(t, err)
	v4, v6 := ips(resp)[0], ips(resp)[1]

	_, err = s.Release(context.Background(), &ipamv1.ReleaseRequest{
		Pool: "sg", Service: "test/svc", Target: &ipamv1.ReleaseRequest_Ip{Ip: v4},
	})
	require.NoError(t, err)

	ps := s.poolFor("sg")
	assert.Empty(t, ps.v4.inUse, "the released IPv4 address should be free")
	assert.Contains(t, ps.v6.inUse, v6, "the IPv6 address must survive a v4-only release")

	_, err = s.Release(context.Background(), &ipamv1.ReleaseRequest{
		Pool: "sg", Service: "test/svc", Target: &ipamv1.ReleaseRequest_All{All: true},
	})
	require.NoError(t, err)
	assert.Empty(t, ps.v6.inUse)
}

func TestReleaseOfAnotherServicesAddressIsANoop(t *testing.T) {
	s := newServer(t)
	_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/owner", Selector: families(true, false),
	})
	require.NoError(t, err)
	held := s.poolFor("sg").v4.bySvc["test/owner"].String()

	_, err = s.Release(context.Background(), &ipamv1.ReleaseRequest{
		Pool: "sg", Service: "test/thief", Target: &ipamv1.ReleaseRequest_Ip{Ip: held},
	})
	require.NoError(t, err)
	assert.Contains(t, s.poolFor("sg").v4.inUse, held, "a release must not free an address the caller does not hold")
}

func TestPoolsAreIndependentPerServiceGroup(t *testing.T) {
	s := newServer(t)
	a, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg-a", Service: "test/svc", Selector: families(true, false),
	})
	require.NoError(t, err)
	b, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg-b", Service: "test/svc", Selector: families(true, false),
	})
	require.NoError(t, err)
	// Separate pools start from the same base, so the same address in
	// both is correct here -- what matters is that the state is separate.
	assert.Equal(t, ips(a), ips(b))
	assert.Len(t, s.pools, 2)
}

func TestSizeSaturatesInsteadOfReportingZero(t *testing.T) {
	// Go yields 0 for a shift at or beyond the operand width, so the
	// naive 1 << (bits-ones) reported a /64 as capacity ZERO while still
	// setting has_known_capacity -- a ServiceGroup status showing
	// addresses in use out of a total of none.
	tests := []struct {
		cidr string
		want uint64
	}{
		{"10.20.30.0/24", 256},
		{"10.20.30.0/29", 8},
		{"fd00:30::/120", 256},
		{"fd00:30::/64", math.MaxUint64},
		{"fd00:30::/0", math.MaxUint64},
		{"0.0.0.0/0", 1 << 32},
	}
	for _, tc := range tests {
		t.Run(tc.cidr, func(t *testing.T) {
			assert.Equal(t, tc.want, newPool(mustCIDR(t, tc.cidr)).size())
		})
	}
}

func TestStatsReportsBothFamilies(t *testing.T) {
	s := newServer(t)
	_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "test/svc", Selector: families(true, true),
	})
	require.NoError(t, err)

	st, err := s.Stats(context.Background(), &ipamv1.StatsRequest{Pool: "sg"})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), st.GetInUseV4())
	assert.Equal(t, uint64(1), st.GetInUseV6())
	assert.Equal(t, uint64(8), st.GetSizeV4())
	assert.Equal(t, uint64(8), st.GetSizeV6())
	assert.True(t, st.GetHasKnownCapacity())
	assert.ElementsMatch(t, []string{"10.20.30.0/29", "fd00:30::/125"}, st.GetDisplayAddresses())
}

func TestStatsOmitsAnUnconfiguredFamily(t *testing.T) {
	s := newServer(t)
	s.cidr6 = nil
	st, err := s.Stats(context.Background(), &ipamv1.StatsRequest{Pool: "sg"})
	require.NoError(t, err)
	assert.Zero(t, st.GetSizeV6())
	assert.Equal(t, []string{"10.20.30.0/29"}, st.GetDisplayAddresses())
}

func TestExhaustionIsReportedNotHung(t *testing.T) {
	// /29 minus the network address leaves 7 usable, and the scan is
	// bounded so an exhausted IPv6 pool fails rather than spinning for
	// the rest of time.
	s := newServer(t)
	for i := 0; i < 7; i++ {
		_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
			Pool: "sg", Service: string(rune('a' + i)), Selector: families(true, false),
		})
		require.NoError(t, err, "allocation %d", i)
	}
	_, err := s.Allocate(context.Background(), &ipamv1.AllocateRequest{
		Pool: "sg", Service: "one-too-many", Selector: families(true, false),
	})
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestParsePoolCIDR(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantV6  bool
		wantNil bool
		wantErr string
	}{
		{name: "empty disables the family", value: "", wantNil: true},
		{name: "v4", value: "10.20.30.0/24"},
		{name: "v6", value: "fd00::/120", wantV6: true},
		// Caught at startup rather than surfacing much later as "IPv6
		// requested but no IPv6 pool is configured".
		{name: "v6 value in the v4 var", value: "fd00::/120", wantErr: "expects an IPv4 CIDR"},
		{name: "v4 value in the v6 var", value: "10.20.30.0/24", wantV6: true, wantErr: "expects an IPv6 CIDR"},
		{name: "not a cidr", value: "10.20.30.1", wantErr: "invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePoolCIDR("SIDECAR_POOL_CIDR", tc.value, tc.wantV6)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got)
			} else {
				assert.NotNil(t, got)
			}
		})
	}
}

func TestFirstHostSkipsTheNetworkAddress(t *testing.T) {
	assert.Equal(t, "10.20.30.1", firstHost(mustCIDR(t, "10.20.30.0/24")).String())
	assert.Equal(t, "fd00:30::1", firstHost(mustCIDR(t, "fd00:30::/120")).String())
}

func TestPoolSetForIPMapsMappedV4ToTheV4Pool(t *testing.T) {
	// net.IP.To4 is non-nil for IPv4-mapped IPv6, which is what we want:
	// ::ffff:10.20.30.5 is an IPv4 address and belongs in the v4 pool.
	ps := &poolSet{v4: newPool(mustCIDR(t, "10.20.30.0/24")), v6: newPool(mustCIDR(t, "fd00:30::/120"))}
	assert.Same(t, ps.v4, ps.forIP(net.ParseIP("10.20.30.5")))
	assert.Same(t, ps.v4, ps.forIP(net.ParseIP("::ffff:10.20.30.5")))
	assert.Same(t, ps.v6, ps.forIP(net.ParseIP("fd00:30::5")))
}
