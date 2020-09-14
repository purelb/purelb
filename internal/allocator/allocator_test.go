package allocator

import (
	"net"
	"strconv"
	"strings"
	"testing"

	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"purelb.io/pkg/apis/v1"
)

func TestAssignment(t *testing.T) {
	alloc := New()
	alloc.pools = map[string]Pool{
		"test0": mustLocalPool(t, "1.2.3.4/31", true),
		"test1": mustLocalPool(t, "1000::4/127", true),
		"test2": mustLocalPool(t, "1.2.4.0/24", true),
		"test3": mustLocalPool(t, "1000::4:0/120", true),
	}

	tests := []struct {
		desc       string
		svc        string
		ip         string
		ports      []Port
		sharingKey string
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
		},
		{
			desc:       "s4 changes its sharing key in place",
			svc:        "s4",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can't share with s4 (port conflict)",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong sharing key)",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			wantErr:    true,
		},
		{
			desc:       "s3 takes the same IP as s4",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can change its ports while keeping the same IP",
			svc:        "s3",
			ip:         "1.2.4.3",
			ports:      ports("udp/53"),
			sharingKey: "share",
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
		},
		{
			desc:       "s4 changes its sharing key in place",
			svc:        "s4",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can't share with s4 (port conflict)",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/80"),
			sharingKey: "share",
			wantErr:    true,
		},
		{
			desc:       "s3 can't share with s4 (wrong sharing key)",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
			wantErr:    true,
		},
		{
			desc:       "s3 takes the same IP as s4",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can change its ports while keeping the same IP",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("udp/53"),
			sharingKey: "share",
		},
		{
			desc:       "s3 can't change its sharing key while keeping the same IP",
			svc:        "s3",
			ip:         "1000::4:3",
			ports:      ports("tcp/443"),
			sharingKey: "othershare",
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
		_, err := alloc.Assign(test.svc, ip, test.ports, test.sharingKey)
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
	alloc.pools = map[string]Pool{
		"not_this_one": mustLocalPool(t, "192.168.0.0/16", false),
		"test":         mustLocalPool(t, "1.2.3.4/30", false),
		"testV6":       mustLocalPool(t, "1000::/126", false),
		"test2":        mustLocalPool(t, "10.20.30.0/24", false),
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
		ports      []Port
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
		ip, err := alloc.AllocateFromPool(test.svc, pool, test.ports, test.sharingKey)
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
	if _, err := alloc.AllocateFromPool("s5", "nonexistentpool", nil, ""); err == nil {
		t.Error("Allocating from non-existent pool succeeded")
	}
}

func TestAllocation(t *testing.T) {
	alloc := New()
	alloc.pools = map[string]Pool{
		"test1":   mustLocalPool(t, "1.2.3.4/31", true),
		"test1V6": mustLocalPool(t, "1000::4/127", true),
	}

	validIPs := map[string]bool{
		"1.2.3.4":  true,
		"1.2.3.5":  true,
		"1000::4":  true,
		"1000::5":  true,
	}

	tests := []struct {
		desc       string
		svc        string
		ports      []Port
		sharingKey string
		unassign   bool
		wantErr    bool
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

		// Clear addresses
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

		{
			desc:   "s1 gets an IP",
			svc:    "s1",
		},
		{
			desc:   "s2 gets an IP",
			svc:    "s2",
		},
		{
			desc:   "s3 gets an IP",
			svc:    "s3",
		},
		{
			desc:   "s4 gets an IP",
			svc:    "s4",
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
	}

	for _, test := range tests {
		if test.unassign {
			alloc.Unassign(test.svc)
			continue
		}
		_, ip, err := alloc.Allocate(test.svc, test.ports, test.sharingKey)
		if test.wantErr {
			if err == nil {
				t.Errorf("%s: should have caused an error, but did not", test.desc)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Allocate(%q, \"test\"): %s", test.desc, test.svc, err)
		}
		if !validIPs[ip.String()] {
			t.Errorf("%s allocated unexpected IP %q", test.desc, ip)
		}
	}
}

func TestAutoAssign(t *testing.T) {
	alloc := New()
	alloc.pools = map[string]Pool{
		"test0": mustLocalPool(t, "1.2.3.4/31", false),
		"test1": mustLocalPool(t, "1000::4/127", false),
		"test2": mustLocalPool(t, "1000::10/127", true),
		"test3": mustLocalPool(t, "1.2.3.10/31", true),
	}

	validIPs := map[string]bool{
		"1.2.3.4":  false,
		"1.2.3.5":  false,
		"1.2.3.10": true,
		"1.2.3.11": true,
		"1000::4":  false,
		"1000::5":  false,
		"1000::10": true,
		"1000::11": true,
	}

	tests := []struct {
		svc      string
		wantErr  bool
		unassign bool
	}{
		{svc: "s1"},
		{svc: "s2"},
		{
			svc:     "s3",
		},
		{
			svc:     "s4",
		},
		{
			svc:     "s5",
			wantErr: true,
		},

		// Clear addresses
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
	}

	for i, test := range tests {
		if test.unassign {
			alloc.Unassign(test.svc)
			continue
		}
		_, ip, err := alloc.Allocate(test.svc, nil, "")
		if test.wantErr {
			if err == nil {
				t.Errorf("#%d should have caused an error, but did not", i+1)
			}
			continue
		}
		if err != nil {
			t.Errorf("#%d Allocate(%q, \"test\"): %s", i+1, test.svc, err)
		}
		if !validIPs[ip.String()] {
			t.Errorf("#%d allocated unexpected IP %q", i+1, ip)
		}
	}
}

func TestPoolMetrics(t *testing.T) {
	alloc := New()
	alloc.pools = map[string]Pool{
		"test": mustLocalPool(t, "1.2.3.4/30", true),
	}

	tests := []struct {
		desc       string
		svc        string
		ip         string
		ports      []Port
		sharingKey string
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
		_, err := alloc.Assign(test.svc, ip, test.ports, test.sharingKey)

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

func mustLocalPool(t *testing.T, r string, aa bool) LocalPool {
	p, err := NewLocalPool(aa, r, "", "")
	if err != nil {
		panic(err)
	}
	return *p
}

func mustEGWPool(t *testing.T, url string, aa bool) EGWPool {
	p, err := NewEGWPool(aa, url, "")
	if err != nil {
		panic(err)
	}
	return *p
}

func ports(ports ...string) []Port {
	var ret []Port
	for _, s := range ports {
		fs := strings.Split(s, "/")
		p, err := strconv.Atoi(fs[1])
		if err != nil {
			panic("bad port in test")
		}
		ret = append(ret, Port{Proto: fs[0], Port: p})
	}
	return ret
}

func localServiceGroup(name string, pool string) *v1.ServiceGroup {
	return serviceGroup(name, v1.ServiceGroupSpec{
		AutoAssign: true,
		Local: &v1.ServiceGroupLocalSpec{Pool: pool},
	})
}

func egwServiceGroup(name string, url string) *v1.ServiceGroup {
	return serviceGroup(name, v1.ServiceGroupSpec{
		AutoAssign: true,
		EGW: &v1.ServiceGroupEGWSpec{URL: url},
	})
}

func serviceGroup(name string, spec v1.ServiceGroupSpec) *v1.ServiceGroup {
	return &v1.ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: spec,
	}
}
