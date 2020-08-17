package config

import (
	"net"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	purelbv1 "purelb.io/pkg/apis/v1"
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
		raw  []*purelbv1.ServiceGroup
		want *Config
	}{
		{ desc: "empty config",
			raw:  []*purelbv1.ServiceGroup{},
			want: &Config{
				Pools: map[string]*Pool{},
			},
		},

		{ desc: "config using all features",
			raw: []*purelbv1.ServiceGroup{
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool1",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "10.20.0.0/16",
						},
					},
				},
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool2",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "30.0.0.0/8",
						},
					},
				},
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool3",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "40.0.0.0/25",
						},
					},
				},
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool4",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "2001:db8::/126",
						},
					},
				},
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool5",
					},
					Spec: purelbv1.ServiceGroupSpec{
						EGW: &purelbv1.ServiceGroupEGWSpec{
							URL: "url",
						},
					},
				},
			},
			want: &Config{
				Pools: map[string]*Pool{
					"pool1": {
						CIDR:       []*net.IPNet{ipnet("10.20.0.0/16")},
						AutoAssign: true,
					},
					"pool2": {
						CIDR:       []*net.IPNet{ipnet("30.0.0.0/8")},
						AutoAssign: true,
					},
					"pool3": {
						CIDR: []*net.IPNet{
							ipnet("40.0.0.0/25"),
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

		{ desc: "invalid CIDR",
			raw: []*purelbv1.ServiceGroup{
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool1",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "100.200.300.400/24",
						},
					},
				},
			},
		},

		{
			desc: "invalid CIDR prefix length",
			raw: []*purelbv1.ServiceGroup{
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool1",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "1.2.3.0/33",
						},
					},
				},
			},
		},

		{ desc: "duplicate group name",
			raw: []*purelbv1.ServiceGroup{
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool1",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "10.20.0.0/16",
						},
					},
				},
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool1",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "30.0.0.0/8",
						},
					},
				},
			},
		},

		{ desc: "duplicate CIDRs",
			raw: []*purelbv1.ServiceGroup{
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool1",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "10.0.0.0/8",
						},
					},
				},
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool2",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "10.0.0.0/8",
						},
					},
				},
			},
		},

		{ desc: "overlapping CIDRs",
			raw: []*purelbv1.ServiceGroup{
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool1",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "10.0.0.0/8",
						},
					},
				},
				&purelbv1.ServiceGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pool2",
					},
					Spec: purelbv1.ServiceGroupSpec{
						Local: &purelbv1.ServiceGroupLocalSpec{
							Pool: "10.0.0.0/16",
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			got, err := ParseServiceGroups(test.raw)
			if err != nil && test.want != nil {
				t.Errorf("%q: parse failed: %s", test.desc, err)
				return
			}
			if test.want == nil && err == nil {
				t.Errorf("%q: parse succeeded but should have failed", test.desc)
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
