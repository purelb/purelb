package config

import (
	"net"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/labels"
)

func selector(s string) labels.Selector {
	ret, err := labels.Parse(s)
	if err != nil {
		panic(err)
	}
	return ret
}

func ipnet(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func TestParse(t *testing.T) {
	tests := []struct {
		desc string
		raw  string
		want *Config
	}{
		{
			desc: "empty config",
			raw:  "",
			want: &Config{
				Pools: map[string]*Pool{},
			},
		},

		{
			desc: "invalid yaml",
			raw:  "foo:<>$@$2r24j90",
		},

		{
			desc: "config using all features",
			raw: `
address-pools:
- name: pool1
  addresses:
  - 10.20.0.0/16
  - 10.50.0.0/24
  avoid-buggy-ips: true
  auto-assign: false
- name: pool2
  addresses:
  - 30.0.0.0/8
- name: pool3
  addresses:
  - 40.0.0.0/25
  - 40.0.0.150-40.0.0.200
  - 40.0.0.210 - 40.0.0.240
- name: pool4
  addresses:
  - 2001:db8::/64
`,
			want: &Config{
				Pools: map[string]*Pool{
					"pool1": {
						CIDR:          []*net.IPNet{ipnet("10.20.0.0/16"), ipnet("10.50.0.0/24")},
						AvoidBuggyIPs: true,
						AutoAssign:    false,
					},
					"pool2": {
						CIDR:       []*net.IPNet{ipnet("30.0.0.0/8")},
						AutoAssign: true,
					},
					"pool3": {
						CIDR: []*net.IPNet{
							ipnet("40.0.0.0/25"),
							ipnet("40.0.0.150/31"),
							ipnet("40.0.0.152/29"),
							ipnet("40.0.0.160/27"),
							ipnet("40.0.0.192/29"),
							ipnet("40.0.0.200/32"),
							ipnet("40.0.0.210/31"),
							ipnet("40.0.0.212/30"),
							ipnet("40.0.0.216/29"),
							ipnet("40.0.0.224/28"),
							ipnet("40.0.0.240/32"),
						},
						AutoAssign: true,
					},
					"pool4": {
						CIDR:       []*net.IPNet{ipnet("2001:db8::/64")},
						AutoAssign: true,
					},
				},
			},
		},

		{
			desc: "no pool name",
			raw: `
address-pools:
-
`,
		},

		{
			desc: "address pool with no addresses",
			raw: `
address-pools:
- name: pool1
`,
		},

		{
			desc: "invalid pool CIDR",
			raw: `
address-pools:
- name: pool1
  addresses:
  - 100.200.300.400/24
`,
		},

		{
			desc: "invalid pool CIDR prefix length",
			raw: `
address-pools:
- name: pool1
  addresses:
  - 1.2.3.0/33
`,
		},

		{
			desc: "duplicate pool definition",
			raw: `
address-pools:
- name: pool1
- name: pool1
- name: pool2
`,
		},

		{
			desc: "duplicate CIDRs",
			raw: `
address-pools:
- name: pool1
  addresses:
  - 10.0.0.0/8
- name: pool2
  addresses:
  - 10.0.0.0/8
`,
		},

		{
			desc: "overlapping CIDRs",
			raw: `
address-pools:
- name: pool1
  addresses:
  - 10.0.0.0/8
- name: pool2
  addresses:
  - 10.0.0.0/16
`,
		},
}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			got, err := Parse([]byte(test.raw))
			if err != nil && test.want != nil {
				t.Errorf("%q: parse failed: %s", test.desc, err)
				return
			}
			if test.want == nil && err == nil {
				t.Errorf("%q: parse unexpectedly succeeded", test.desc)
				return
			}
			selectorComparer := cmp.Comparer(func(x, y labels.Selector) bool {
				if x == nil {
					return y == nil
				}
				if y == nil {
					return x == nil
				}
				// Nothing() and Everything() have the same string
				// representation, stupidly. So, compare explicitly for
				// Nothing.
				if x == labels.Nothing() {
					return y == labels.Nothing()
				}
				if y == labels.Nothing() {
					return x == labels.Nothing()
				}
				return x.String() == y.String()
			})
			if diff := cmp.Diff(test.want, got, selectorComparer); diff != "" {
				t.Errorf("%q: parse returned wrong result (-want, +got)\n%s", test.desc, diff)
			}
		})
	}
}
