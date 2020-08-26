package config

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv1 "purelb.io/pkg/apis/v1"
)

func mustLocalPool(t *testing.T, r string, aa bool) LocalPool {
	p, err := NewLocalPool(r, aa, "", "")
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

func TestParse(t *testing.T) {
	tests := []struct {
		desc string
		raw  []*purelbv1.ServiceGroup
		want *Config
	}{
		{ desc: "empty config",
			raw: []*purelbv1.ServiceGroup{},
			want: &Config{
				Pools: map[string]Pool{},
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
				Pools: map[string]Pool{
					"pool1": mustLocalPool(t, "10.20.0.0/16", true),
					"pool2": mustLocalPool(t, "30.0.0.0/8", true),
					"pool3": mustLocalPool(t, "40.0.0.0/25", true),
					"pool4": mustLocalPool(t, "2001:db8::/126", true),
					"pool5": mustEGWPool(t, "url", true),
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

		{ desc: "invalid CIDR prefix length",
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
			iprangeComparer := cmp.Comparer(func(x, y IPRange) bool {
				return reflect.DeepEqual(x.from, y.from) && reflect.DeepEqual(x.to, y.to)
			})
			if diff := cmp.Diff(test.want, got, iprangeComparer, cmp.AllowUnexported(LocalPool{}), cmp.AllowUnexported(EGWPool{})); diff != "" {
				t.Errorf("%q: parse returned wrong result (-want, +got)\n%s", test.desc, diff)
			}
		})
	}
}
