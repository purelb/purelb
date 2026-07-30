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

package v2

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	require.NoError(t, err)
	return a.Unmap()
}

func TestParseAnnouncements(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []Announcement
	}{
		{name: "empty", value: "", want: nil},
		{
			name:  "single",
			value: "node-a,eth0,192.0.2.10",
			want:  []Announcement{{"node-a", "eth0", addr(t, "192.0.2.10")}},
		},
		{
			name:  "multiple",
			value: "node-a,eth0,192.0.2.10 node-b,eth1,192.0.2.11",
			want: []Announcement{
				{"node-a", "eth0", addr(t, "192.0.2.10")},
				{"node-b", "eth1", addr(t, "192.0.2.11")},
			},
		},
		{
			name:  "interface name containing commas",
			value: "node-a,a,b,10.0.0.1",
			want:  []Announcement{{"node-a", "a,b", addr(t, "10.0.0.1")}},
		},
		{
			name:  "legacy two-field form is dropped",
			value: "node1,enp1s0",
			want:  nil,
		},
		{
			name:  "legacy one-field remote form is dropped",
			value: "kube-lb0",
			want:  nil,
		},
		{
			name:  "unparseable ip is dropped, good entries kept",
			value: "node-a,eth0,not-an-ip node-b,eth0,192.0.2.11",
			want:  []Announcement{{"node-b", "eth0", addr(t, "192.0.2.11")}},
		},
		{
			name:  "zoned address is rejected",
			value: "node-a,eth0,fe80::1%eth0",
			want:  nil,
		},
		{
			name:  "empty node or interface is dropped",
			value: ",eth0,192.0.2.10 node-a,,192.0.2.11",
			want:  nil,
		},
		{
			name:  "ipv4-mapped ipv6 normalises to v4",
			value: "node-a,eth0,::ffff:10.0.0.1",
			want:  []Announcement{{"node-a", "eth0", addr(t, "10.0.0.1")}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, Announcements(tt.want), ParseAnnouncements(tt.value))
		})
	}
}

func TestEncode(t *testing.T) {
	t.Run("empty is empty string", func(t *testing.T) {
		assert.Equal(t, "", Announcements(nil).Encode())
		assert.Equal(t, "", Announcements{}.Encode())
	})

	t.Run("sorted by IP regardless of insertion order", func(t *testing.T) {
		a := Announcements{
			{"node-b", "eth0", addr(t, "192.0.2.11")},
			{"node-a", "eth0", addr(t, "192.0.2.10")},
		}
		assert.Equal(t, "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11", a.Encode())
	})

	t.Run("duplicate IP collapses to one slot (upgrade case)", func(t *testing.T) {
		// The exact shape of the append-only bug: two nodes listed for one IP.
		a := ParseAnnouncements("node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.10")
		assert.Equal(t, "node-a,eth0,192.0.2.10", a.Encode())
	})

	t.Run("byte-identical for 4-byte and 16-byte IP constructions", func(t *testing.T) {
		four := netip.AddrFrom4([4]byte{10, 0, 0, 1})
		sixteen := netip.AddrFrom16([16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1}).Unmap()
		a := Announcements{{"node-a", "eth0", four}}
		b := Announcements{{"node-a", "eth0", sixteen}}
		assert.Equal(t, a.Encode(), b.Encode())
	})

	t.Run("Encode(Parse(s)) == s for canonical s", func(t *testing.T) {
		for _, s := range []string{
			"node-a,eth0,192.0.2.10",
			"node-a,eth0,192.0.2.10 node-b,eth1,192.0.2.11",
			"node-a,a,b,10.0.0.1",
			"node-a,eth0,2001:db8::1",
		} {
			assert.Equal(t, s, ParseAnnouncements(s).Encode(), "not canonical: %q", s)
		}
	})
}

func TestReconcile(t *testing.T) {
	mine := Announcement{"node-a", "eth0", addr(t, "192.0.2.10")}

	t.Run("win an unclaimed IP", func(t *testing.T) {
		got := Announcements(nil).Reconcile(mine, []netip.Addr{addr(t, "192.0.2.10")})
		assert.Equal(t, "node-a,eth0,192.0.2.10", got.Encode())
	})

	t.Run("take the slot from a previous holder", func(t *testing.T) {
		existing := ParseAnnouncements("node-b,eth0,192.0.2.10")
		got := existing.Reconcile(mine, []netip.Addr{addr(t, "192.0.2.10")})
		assert.Equal(t, "node-a,eth0,192.0.2.10", got.Encode())
	})

	t.Run("leave another node's slot for a different live IP", func(t *testing.T) {
		existing := ParseAnnouncements("node-b,eth0,192.0.2.11")
		got := existing.Reconcile(mine, []netip.Addr{addr(t, "192.0.2.10"), addr(t, "192.0.2.11")})
		assert.Equal(t, "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11", got.Encode())
	})

	t.Run("sweep a slot whose IP is no longer an ingress", func(t *testing.T) {
		existing := ParseAnnouncements("node-a,eth0,192.0.2.99 node-b,eth0,192.0.2.11")
		got := existing.Reconcile(mine, []netip.Addr{addr(t, "192.0.2.10"), addr(t, "192.0.2.11")})
		// .99 gone (not in keep), .11 kept (still live, other node), .10 mine.
		assert.Equal(t, "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11", got.Encode())
	})

	t.Run("idempotent", func(t *testing.T) {
		keep := []netip.Addr{addr(t, "192.0.2.10"), addr(t, "192.0.2.11")}
		existing := ParseAnnouncements("node-b,eth0,192.0.2.11")
		once := existing.Reconcile(mine, keep)
		twice := once.Reconcile(mine, keep)
		assert.Equal(t, once.Encode(), twice.Encode())
	})
}
