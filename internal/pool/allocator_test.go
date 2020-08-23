package pool

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"purelb.io/internal/config"

	ptu "github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAssignment(t *testing.T) {
	alloc := New()
	if err := alloc.SetPools(map[string]config.Pool{
		"test0": mustPool("1.2.3.4/31", true),
		"test1": mustPool("1000::4/127", true),
		"test2": mustPool("1.2.4.0/24", true),
		"test3": mustPool("1000::4:0/120", true),
	}); err != nil {
		t.Fatalf("SetPools: %s", err)
	}

	tests := []struct {
		desc       string
		svc        string
		ip         string
		ports      []config.Port
		sharingKey string
		backendKey string
		wantErr    bool
	}{
		{
			desc: "assign s1",
			svc:  "s1",
			ip:   "1.2.3.4",
		},
		{
			desc: "s1 idempotent reassign",
			svc:  "s1",
			ip:   "1.2.3.4",
		},
		{
			desc:    "s2 can't grab s1's IP",
			svc:     "s2",
			ip:      "1.2.3.4",
			wantErr: true,
		},
		{
			desc: "s2 can get the other IP",
			svc:  "s2",
			ip:   "1.2.3.5",
		},
		{
			desc:    "s1 now can't grab s2's IP",
			svc:     "s1",
			ip:      "1.2.3.5",
			wantErr: true,
		},
		{
			desc: "s1 frees its IP",
			svc:  "s1",
			ip:   "",
		},
		{
			desc: "s2 can grab s1's former IP",
			svc:  "s2",
			ip:   "1.2.3.4",
		},
		{
			desc: "s1 can now grab s2's former IP",
			svc:  "s1",
			ip:   "1.2.3.5",
		},
		{
			desc: "s3 can grab another IP in that pool",
			svc:  "s3",
			ip:   "1.2.4.254",
		},
		{
			desc:       "s4 takes an IP, with sharing",
			svc:        "s4",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "sharing",
			backendKey: "backend",
		},
		{
			desc:       "s4 changes its sharing key in place",
			svc:        "s4",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			backendKey: "backend",
		},
		{
			desc:       "s3 can't share with s4 (port conflict)",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			backendKey: "backend",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong sharing key)",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			backendKey: "backend",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong backend key)",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			backendKey: "otherbackend",
			wantErr:    true,
		},
		{
			desc:       "s3 takes the same IP as s4",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			backendKey: "backend",
		},
		{
			desc:       "s3 can change its ports while keeping the same IP",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("udp/53"),
			sharingKey: "share",
			backendKey: "backend",
		},
		{
			desc:       "s3 can't change its sharing key while keeping the same IP",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			backendKey: "backend",
			wantErr:    true,
		},
		{
			desc:       "s3 can't change its backend key while keeping the same IP",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			backendKey: "otherbackend",
			wantErr:    true,
		},
		{
			desc: "s4 takes s3's former IP",
			svc:  "s4",
			ip:   "1.2.4.254",
		},

		// IPv6 tests (same as ipv4 but with ipv6 addresses)
		{
			desc: "ipv6 assign s1",
			svc:  "s1",
			ip:   "1000::4",
		},
		{
			desc: "s1 idempotent reassign",
			svc:  "s1",
			ip:   "1000::4",
		},
		{
			desc:    "s2 can't grab s1's IP",
			svc:     "s2",
			ip:      "1000::4",
			wantErr: true,
		},
		{
			desc: "s2 can get the other IP",
			svc:  "s2",
			ip:   "1000::4:5",
		},
		{
			desc:    "s1 now can't grab s2's IP",
			svc:     "s1",
			ip:      "1000::4:5",
			wantErr: true,
		},
		{
			desc: "s1 frees its IP",
			svc:  "s1",
			ip:   "",
		},
		{
			desc: "s2 can grab s1's former IP",
			svc:  "s2",
			ip:   "1000::4",
		},
		{
			desc: "s1 can now grab s2's former IP",
			svc:  "s1",
			ip:   "1000::4:5",
		},
		{
			desc: "s3 can grab another IP in that pool",
			svc:  "s3",
			ip:   "1000::4:ff",
		},
		{
			desc:       "s4 takes an IP, with sharing",
			svc:        "s4",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "sharing",
			backendKey: "backend",
		},
		{
			desc:       "s4 changes its sharing key in place",
			svc:        "s4",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			backendKey: "backend",
		},
		{
			desc:       "s3 can't share with s4 (port conflict)",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			backendKey: "backend",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong sharing key)",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			backendKey: "backend",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong backend key)",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			backendKey: "otherbackend",
			wantErr:    true,
		},
		{
			desc:       "s3 takes the same IP as s4",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			backendKey: "backend",
		},
		{
			desc:       "s3 can change its ports while keeping the same IP",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("udp/53"),
			sharingKey: "share",
			backendKey: "backend",
		},
		{
			desc:       "s3 can't change its sharing key while keeping the same IP",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			backendKey: "backend",
			wantErr:    true,
		},
		{
			desc:       "s3 can't change its backend key while keeping the same IP",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			backendKey: "otherbackend",
			wantErr:    true,
		},
		{
			desc: "s4 takes s3's former IP",
			svc:  "s4",
			ip:   "1000::4:ff",
		},
	}

	for _, test := range tests {
		if test.ip == "" {
			alloc.Unassign(test.svc)
			continue
		}
		ip := net.ParseIP(test.ip)
		if ip == nil {
			t.Fatalf("invalid IP %q in test %q", test.ip, test.desc)
		}
		alreadyHasIP := assigned(alloc, test.svc) == test.ip
		err := alloc.Assign(test.svc, ip, test.ports, test.sharingKey, test.backendKey)
		if test.wantErr {
			if err == nil {
				t.Errorf("%q should have caused an error, but did not", test.desc)
			} else if a := assigned(alloc, test.svc); !alreadyHasIP && a == test.ip {
				t.Errorf("%q: Assign(%q, %q) failed, but allocator did record allocation", test.desc, test.svc, test.ip)
			}

			continue
		}

		if err != nil {
			t.Errorf("%q: Assign(%q, %q): %s", test.desc, test.svc, test.ip, err)
		}
		if a := assigned(alloc, test.svc); a != test.ip {
			t.Errorf("%q: ran Assign(%q, %q), but allocator has recorded allocation of %q", test.desc, test.svc, test.ip, a)
		}
	}
}

func TestPoolAllocation(t *testing.T) {
	alloc := New()
	// This test only allocates from the "test" and "testV6" pools, so
	// it will run out of IPs quickly even though there are tons
	// available in other pools.
	if err := alloc.SetPools(map[string]config.Pool{
		"not_this_one": mustPool("192.168.0.0/16", true),
		"test":         mustPool("1.2.3.4/30", true),
		"testV6":       mustPool("1000::/126", true),
		"test2":        mustPool("10.20.30.0/24", true),
	}); err != nil {
		t.Fatalf("SetPools: %s", err)
	}

	validIP4s := map[string]bool{
		"1.2.3.4": true,
		"1.2.3.5": true,
		"1.2.3.6": true,
		"1.2.3.7": true,
	}
	validIP6s := map[string]bool{
		"1000::":  true,
		"1000::1": true,
		"1000::2": true,
		"1000::3": true,
	}

	tests := []struct {
		desc       string
		svc        string
		ports      []config.Port
		sharingKey string
		unassign   bool
		wantErr    bool
		isIPv6     bool
	}{
		{
			desc: "s1 gets an IP",
			svc:  "s1",
		},
		{
			desc: "s2 gets an IP",
			svc:  "s2",
		},
		{
			desc: "s3 gets an IP",
			svc:  "s3",
		},
		{
			desc: "s4 gets an IP",
			svc:  "s4",
		},
		{
			desc:    "s5 can't get an IP",
			svc:     "s5",
			wantErr: true,
		},
		{
			desc:    "s6 can't get an IP",
			svc:     "s6",
			wantErr: true,
		},
		{
			desc:     "s1 releases its IP",
			svc:      "s1",
			unassign: true,
		},
		{
			desc: "s5 can now grab s1's former IP",
			svc:  "s5",
		},
		{
			desc:    "s6 still can't get an IP",
			svc:     "s6",
			wantErr: true,
		},
		{
			desc:     "s5 unassigns in prep for enabling IP sharing",
			svc:      "s5",
			unassign: true,
		},
		{
			desc:       "s5 enables IP sharing",
			svc:        "s5",
			ports:      ports("tcp/80"),
			sharingKey: "share",
		},
		{
			desc:       "s6 can get an IP now, with sharing",
			svc:        "s6",
			ports:      ports("tcp/443"),
			sharingKey: "share",
		},

		// Clear old ipv4 addresses
		{
			desc:     "s1 clear old ipv4 address",
			svc:      "s1",
			unassign: true,
		},
		{
			desc:     "s2 clear old ipv4 address",
			svc:      "s2",
			unassign: true,
		},
		{
			desc:     "s3 clear old ipv4 address",
			svc:      "s3",
			unassign: true,
		},
		{
			desc:     "s4 clear old ipv4 address",
			svc:      "s4",
			unassign: true,
		},
		{
			desc:     "s5 clear old ipv4 address",
			svc:      "s5",
			unassign: true,
		},
		{
			desc:     "s6 clear old ipv4 address",
			svc:      "s6",
			unassign: true,
		},

		// IPv6 tests.
		{
			desc:   "s1 gets an IP6",
			svc:    "s1",
			isIPv6: true,
		},
		{
			desc:   "s2 gets an IP6",
			svc:    "s2",
			isIPv6: true,
		},
		{
			desc:   "s3 gets an IP6",
			svc:    "s3",
			isIPv6: true,
		},
		{
			desc:   "s4 gets an IP6",
			svc:    "s4",
			isIPv6: true,
		},
		{
			desc:    "s5 can't get an IP6",
			svc:     "s5",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:    "s6 can't get an IP6",
			svc:     "s6",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:     "s1 releases its IP6",
			svc:      "s1",
			unassign: true,
		},
		{
			desc:   "s5 can now grab s1's former IP6",
			svc:    "s5",
			isIPv6: true,
		},
		{
			desc:    "s6 still can't get an IP6",
			svc:     "s6",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:     "s5 unassigns in prep for enabling IP6 sharing",
			svc:      "s5",
			unassign: true,
		},
		{
			desc:       "s5 enables IP6 sharing",
			svc:        "s5",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			isIPv6:     true,
		},
		{
			desc:       "s6 can get an IP6 now, with sharing",
			svc:        "s6",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			isIPv6:     true,
		},

		// Test the "should-not-happen" case where an svc already has a IP from the wrong family
		{
			desc:     "s1 clear",
			svc:      "s1",
			unassign: true,
		},
		{
			desc: "s1 get an IPv4",
			svc:  "s1",
		},
		{
			desc:    "s1 get an IPv6",
			svc:     "s1",
			isIPv6:  true,
			wantErr: true,
		},
	}

	for _, test := range tests {
		if test.unassign {
			alloc.Unassign(test.svc)
			continue
		}
		pool := "test"
		if test.isIPv6 {
			pool = "testV6"
		}
		ip, err := alloc.AllocateFromPool(test.svc, test.isIPv6, pool, test.ports, test.sharingKey, "")
		if test.wantErr {
			if err == nil {
				t.Errorf("%s: should have caused an error, but did not", test.desc)

			}
			continue
		}
		if err != nil {
			t.Errorf("%s: AllocateFromPool(%q, \"test\"): %s", test.desc, test.svc, err)
		}
		validIPs := validIP4s
		if test.isIPv6 {
			validIPs = validIP6s
		}
		if !validIPs[ip.String()] {
			t.Errorf("%s: allocated unexpected IP %q", test.desc, ip)
		}
	}

	alloc.Unassign("s5")
	if _, err := alloc.AllocateFromPool("s5", false, "nonexistentpool", nil, "", ""); err == nil {
		t.Error("Allocating from non-existent pool succeeded")
	}
}

func TestAllocation(t *testing.T) {
	alloc := New()
	if err := alloc.SetPools(map[string]config.Pool{
		"test1":   mustPool("1.2.3.4/31", true),
		"test1V6": mustPool("1000::4/127", true),
		"test2":   mustPool("1.2.3.10/31", true),
		"test2V6": mustPool("1000::10/127", true),
	}); err != nil {
		t.Fatalf("SetPools: %s", err)
	}

	validIP4s := map[string]bool{
		"1.2.3.4":  true,
		"1.2.3.5":  true,
		"1.2.3.10": true,
		"1.2.3.11": true,
	}
	validIP6s := map[string]bool{
		"1000::4":  true,
		"1000::5":  true,
		"1000::10": true,
		"1000::11": true,
	}

	tests := []struct {
		desc       string
		svc        string
		ports      []config.Port
		sharingKey string
		unassign   bool
		wantErr    bool
		isIPv6     bool
	}{
		{
			desc: "s1 gets an IP",
			svc:  "s1",
		},
		{
			desc: "s2 gets an IP",
			svc:  "s2",
		},
		{
			desc: "s3 gets an IP",
			svc:  "s3",
		},
		{
			desc: "s4 gets an IP",
			svc:  "s4",
		},
		{
			desc:    "s5 can't get an IP",
			svc:     "s5",
			wantErr: true,
		},
		{
			desc:    "s6 can't get an IP",
			svc:     "s6",
			wantErr: true,
		},
		{
			desc:     "s1 gives up its IP",
			svc:      "s1",
			unassign: true,
		},
		{
			desc:       "s5 can now get an IP",
			svc:        "s5",
			ports:      ports("tcp/80"),
			sharingKey: "share",
		},
		{
			desc:    "s6 still can't get an IP",
			svc:     "s6",
			wantErr: true,
		},
		{
			desc:       "s6 can get an IP with sharing",
			svc:        "s6",
			ports:      ports("tcp/443"),
			sharingKey: "share",
		},

		// Clear old ipv4 addresses
		{
			desc:     "s1 clear old ipv4 address",
			svc:      "s1",
			unassign: true,
		},
		{
			desc:     "s2 clear old ipv4 address",
			svc:      "s2",
			unassign: true,
		},
		{
			desc:     "s3 clear old ipv4 address",
			svc:      "s3",
			unassign: true,
		},
		{
			desc:     "s4 clear old ipv4 address",
			svc:      "s4",
			unassign: true,
		},
		{
			desc:     "s5 clear old ipv4 address",
			svc:      "s5",
			unassign: true,
		},
		{
			desc:     "s6 clear old ipv4 address",
			svc:      "s6",
			unassign: true,
		},

		// IPv6 tests
		{
			desc:   "s1 gets an IP",
			svc:    "s1",
			isIPv6: true,
		},
		{
			desc:   "s2 gets an IP",
			svc:    "s2",
			isIPv6: true,
		},
		{
			desc:   "s3 gets an IP",
			svc:    "s3",
			isIPv6: true,
		},
		{
			desc:   "s4 gets an IP",
			svc:    "s4",
			isIPv6: true,
		},
		{
			desc:    "s5 can't get an IP",
			svc:     "s5",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:    "s6 can't get an IP",
			svc:     "s6",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:     "s1 gives up its IP",
			svc:      "s1",
			unassign: true,
		},
		{
			desc:       "s5 can now get an IP",
			svc:        "s5",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			isIPv6:     true,
		},
		{
			desc:    "s6 still can't get an IP",
			svc:     "s6",
			isIPv6:  true,
			wantErr: true,
		},
		{
			desc:       "s6 can get an IP with sharing",
			svc:        "s6",
			ports:      ports("tcp/443"),
			sharingKey: "share",
			isIPv6:     true,
		},
	}

	for _, test := range tests {
		if test.unassign {
			alloc.Unassign(test.svc)
			continue
		}
		ip, err := alloc.Allocate(test.svc, test.isIPv6, test.ports, test.sharingKey, "")
		if test.wantErr {
			if err == nil {
				t.Errorf("%s: should have caused an error, but did not", test.desc)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Allocate(%q, \"test\"): %s", test.desc, test.svc, err)
		}
		validIPs := validIP4s
		if test.isIPv6 {
			validIPs = validIP6s
		}
		if !validIPs[ip.String()] {
			t.Errorf("%s allocated unexpected IP %q", test.desc, ip)
		}
	}
}

func TestConfigReload(t *testing.T) {
	alloc := New()
	if err := alloc.SetPools(map[string]config.Pool{
		"test":   mustPool("1.2.3.0/30", true),
		"testV6": mustPool("1000::/126", true),
	}); err != nil {
		t.Fatalf("SetPools: %s", err)
	}
	if err := alloc.Assign("s1", net.ParseIP("1.2.3.0"), nil, "", ""); err != nil {
		t.Fatalf("Assign(s1, 1.2.3.0): %s", err)
	}
	if err := alloc.Assign("s2", net.ParseIP("1000::"), nil, "", ""); err != nil {
		t.Fatalf("Assign(s1, 1000::): %s", err)
	}

	tests := []struct {
		desc    string
		pools   map[string]config.Pool
		wantErr bool
		pool    string // Pool that 1.2.3.0 should be in
		pool6   string // Pool that 1000:: should be in
	}{
		{
			desc: "set same config is no-op",
			pools: map[string]config.Pool{
				"test":   mustPool("1.2.3.0/30", true),
				"testV6": mustPool("1000::/126", true),
			},
			pool:  "test",
			pool6: "testV6",
		},
		{
			desc: "expand pool",
			pools: map[string]config.Pool{
				"test":   mustPool("1.2.3.0/29", true),
				"testV6": mustPool("1000::/126", true),
			},
			pool:  "test",
			pool6: "testV6",
		},
		{
			desc: "shrink pool",
			pools: map[string]config.Pool{
				"test":   mustPool("1.2.3.0/31", true),
				"testV6": mustPool("1000::/126", true),
			},
			pool:  "test",
			pool6: "testV6",
		},
		{
			desc: "can't shrink further",
			pools: map[string]config.Pool{
				"test": mustPool("1.2.3.0/31", true),
			},
			pool:    "test",
			pool6:   "testV6",
			wantErr: true,
		},
		{
			desc: "can't shrink further ipv6",
			pools: map[string]config.Pool{
				"test": mustPool("1000::2/127", true),
			},
			pool:    "test",
			pool6:   "testV6",
			wantErr: true,
		},
		{
			desc: "rename the pool",
			pools: map[string]config.Pool{
				"test2":  mustPool("1.2.3.0/30", true),
				"testV6": mustPool("1000::/126", true),
			},
			pool:  "test2",
			pool6: "testV6",
		},
		{
			desc: "split pool",
			pools: map[string]config.Pool{
				"testV4": mustPool("1.2.3.0/30", true),
				"test":   mustPool("1000::/127", true),
				"test2":  mustPool("1000::2/127", true),
			},
			pool:  "testV4",
			pool6: "test",
		},
		{
			desc: "swap pool names",
			pools: map[string]config.Pool{
				"testV4": mustPool("1.2.3.0/30", true),
				"test2":  mustPool("1000::/127", true),
				"test":   mustPool("1000::2/127", true),
			},
			pool:  "testV4",
			pool6: "test2",
		},
		{
			desc: "delete used pool",
			pools: map[string]config.Pool{
				"test": mustPool("1000::/126", true),
			},
			pool:    "testV4",
			pool6:   "test2",
			wantErr: true,
		},
		{
			desc: "delete used pool ipv6",
			pools: map[string]config.Pool{
				"test": mustPool("1000::2/127", true),
			},
			pool:    "testV4",
			pool6:   "test2",
			wantErr: true,
		},
		{
			desc: "delete unused pool",
			pools: map[string]config.Pool{
				"testV4": mustPool("1.2.3.0/30", true),
				"test2":  mustPool("1000::/127", true),
			},
			pool:  "testV4",
			pool6: "test2",
		},
	}

	for _, test := range tests {
		err := alloc.SetPools(test.pools)
		if test.wantErr {
			if err == nil {
				t.Errorf("%q should have failed to SetPools, but succeeded", test.desc)
			}
		} else if err != nil {
			t.Errorf("%q failed to SetPools: %s", test.desc, err)
		}
		gotPool := alloc.Pool("s1")
		if gotPool != test.pool {
			t.Errorf("%q: s1 is in wrong pool, want %q, got %q", test.desc, test.pool, gotPool)
		}
		gotPool = alloc.Pool("s2")
		if gotPool != test.pool6 {
			t.Errorf("%q: s2 is in wrong pool, want %q, got %q", test.desc, test.pool6, gotPool)
		}
	}
}

func TestAutoAssign(t *testing.T) {
	alloc := New()
	if err := alloc.SetPools(map[string]config.Pool{
		"test0": mustPool("1.2.3.4/31", false),
		"test1": mustPool("1000::4/127", false),
		"test2": mustPool("1000::10/127", true),
		"test3": mustPool("1.2.3.10/31", true),
	}); err != nil {
		t.Fatalf("SetPools: %s", err)
	}

	validIP4s := map[string]bool{
		"1.2.3.4":  false,
		"1.2.3.5":  false,
		"1.2.3.10": true,
		"1.2.3.11": true,
	}
	validIP6s := map[string]bool{
		"1000::4":  false,
		"1000::5":  false,
		"1000::10": true,
		"1000::11": true,
	}

	tests := []struct {
		svc      string
		wantErr  bool
		isIPv6   bool
		unassign bool
	}{
		{svc: "s1"},
		{svc: "s2"},
		{
			svc:     "s3",
			wantErr: true,
		},
		{
			svc:     "s4",
			wantErr: true,
		},
		{
			svc:     "s5",
			wantErr: true,
		},

		// Clear old ipv4 addresses
		{
			svc:      "s1",
			unassign: true,
		},
		{
			svc:      "s2",
			unassign: true,
		},
		{
			svc:      "s3",
			unassign: true,
		},
		{
			svc:      "s4",
			unassign: true,
		},
		{
			svc:      "s5",
			unassign: true,
		},
		{
			svc:      "s6",
			unassign: true,
		},

		// IPv6 tests;
		{
			svc:    "s1",
			isIPv6: true,
		},
		{
			svc:    "s2",
			isIPv6: true,
		},
		{
			svc:     "s3",
			isIPv6:  true,
			wantErr: true,
		},
		{
			svc:     "s4",
			isIPv6:  true,
			wantErr: true,
		},
		{
			svc:     "s5",
			isIPv6:  true,
			wantErr: true,
		},
	}

	for i, test := range tests {
		if test.unassign {
			alloc.Unassign(test.svc)
			continue
		}
		ip, err := alloc.Allocate(test.svc, test.isIPv6, nil, "", "")
		if test.wantErr {
			if err == nil {
				t.Errorf("#%d should have caused an error, but did not", i+1)
			}
			continue
		}
		if err != nil {
			t.Errorf("#%d Allocate(%q, \"test\"): %s", i+1, test.svc, err)
		}
		validIPs := validIP4s
		if test.isIPv6 {
			validIPs = validIP6s
		}
		if !validIPs[ip.String()] {
			t.Errorf("#%d allocated unexpected IP %q", i+1, ip)
		}
	}
}

func TestPoolMetrics(t *testing.T) {
	alloc := New()
	if err := alloc.SetPools(map[string]config.Pool{
		"test": mustPool("1.2.3.4/30", true),
	}); err != nil {
		t.Fatalf("SetPools: %s", err)
	}

	tests := []struct {
		desc       string
		svc        string
		ip         string
		ports      []config.Port
		sharingKey string
		backendKey string
		ipsInUse   float64
	}{
		{
			desc:     "assign s1",
			svc:      "s1",
			ip:       "1.2.3.4",
			ipsInUse: 1,
		},
		{
			desc:     "assign s2",
			svc:      "s2",
			ip:       "1.2.3.5",
			ipsInUse: 2,
		},
		{
			desc:     "unassign s1",
			svc:      "s1",
			ipsInUse: 1,
		},
		{
			desc:     "unassign s2",
			svc:      "s2",
			ipsInUse: 0,
		},
		{
			desc:       "assign s1 shared",
			svc:        "s1",
			ip:         "1.2.3.4",
			sharingKey: "key",
			ports:      ports("tcp/80"),
			ipsInUse:   1,
		},
		{
			desc:       "assign s2 shared",
			svc:        "s2",
			ip:         "1.2.3.4",
			sharingKey: "key",
			ports:      ports("tcp/443"),
			ipsInUse:   1,
		},
		{
			desc:       "assign s3 shared",
			svc:        "s3",
			ip:         "1.2.3.4",
			sharingKey: "key",
			ports:      ports("tcp/23"),
			ipsInUse:   1,
		},
		{
			desc:     "unassign s1 shared",
			svc:      "s1",
			ports:    ports("tcp/80"),
			ipsInUse: 1,
		},
		{
			desc:     "unassign s2 shared",
			svc:      "s2",
			ports:    ports("tcp/443"),
			ipsInUse: 1,
		},
		{
			desc:     "unassign s3 shared",
			svc:      "s3",
			ports:    ports("tcp/23"),
			ipsInUse: 0,
		},
	}

	// The "test" pool contains one range: 1.2.3.4/30
	value := ptu.ToFloat64(stats.poolCapacity.WithLabelValues("test"))
	if int(value) != 4 {
		t.Errorf("stats.poolCapacity invalid %f. Expected 4", value)
	}

	for _, test := range tests {
		if test.ip == "" {
			alloc.Unassign(test.svc)
			value := ptu.ToFloat64(stats.poolActive.WithLabelValues("test"))
			if value != test.ipsInUse {
				t.Errorf("%v; in-use %v. Expected %v", test.desc, value, test.ipsInUse)
			}
			continue
		}

		ip := net.ParseIP(test.ip)
		if ip == nil {
			t.Fatalf("invalid IP %q in test %q", test.ip, test.desc)
		}
		err := alloc.Assign(test.svc, ip, test.ports, test.sharingKey, test.backendKey)

		if err != nil {
			t.Errorf("%q: Assign(%q, %q): %v", test.desc, test.svc, test.ip, err)
		}
		if a := assigned(alloc, test.svc); a != test.ip {
			t.Errorf("%q: ran Assign(%q, %q), but allocator has recorded allocation of %q", test.desc, test.svc, test.ip, a)
		}
		value := ptu.ToFloat64(stats.poolActive.WithLabelValues("test"))
		if value != test.ipsInUse {
			t.Errorf("%v; in-use %v. Expected %v", test.desc, value, test.ipsInUse)
		}
	}
}

// Some helpers

func assigned(a *Allocator, svc string) string {
	ip := a.IP(svc)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func mustPool(r string, aa bool) config.Pool {
	p, err := config.NewLocalPool(r, aa, "", "")
	if err != nil {
		panic(err)
	}
	return *p
}

func ports(ports ...string) []config.Port {
	var ret []config.Port
	for _, s := range ports {
		fs := strings.Split(s, "/")
		p, err := strconv.Atoi(fs[1])
		if err != nil {
			panic("bad port in test")
		}
		ret = append(ret, config.Port{Proto: fs[0], Port: p})
	}
	return ret
}
