package allocator

import (
	"fmt"
	"testing"

	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"github.com/google/go-cmp/cmp"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func diffService(a, b *v1.Service) string {
	// v5 of the k8s client does not correctly compare nil
	// *metav1.Time objects, which svc.ObjectMeta contains. Add
	// some dummy non-nil values to all of in, want, got to work
	// around this until we migrate to v6.
	if a != nil {
		newA := new(v1.Service)
		*newA = *a
		newA.ObjectMeta.DeletionTimestamp = &metav1.Time{}
		a = newA
	}
	if b != nil {
		newB := new(v1.Service)
		*newB = *b
		newB.ObjectMeta.DeletionTimestamp = &metav1.Time{}
		b = newB
	}
	return cmp.Diff(a, b)
}

func statusAssigned(ip string) v1.ServiceStatus {
	return v1.ServiceStatus{
		LoadBalancer: v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{
					IP: ip,
				},
			},
		},
	}
}

// testK8S implements service by recording what the controller wants
// to do to k8s.
type testK8S struct {
	updateService       *v1.Service
	updateServiceStatus *v1.ServiceStatus
	loggedWarning       bool
	t                   *testing.T
}

func (s *testK8S) Update(svc *v1.Service) (*v1.Service, error) {
	s.updateService = svc
	return svc, nil
}

func (s *testK8S) UpdateStatus(svc *v1.Service) error {
	s.updateServiceStatus = &svc.Status
	return nil
}

func (s *testK8S) Infof(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Info event %q: %s", evtType, fmt.Sprintf(msg, args...))
}

func (s *testK8S) Errorf(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Warning event %q: %s", evtType, fmt.Sprintf(msg, args...))
	s.loggedWarning = true
}

func (s *testK8S) reset() {
	s.updateService = nil
	s.updateServiceStatus = nil
	s.loggedWarning = false
}

func (s *testK8S) gotService(in *v1.Service) *v1.Service {
	if s.updateService == nil && s.updateServiceStatus == nil {
		return nil
	}

	ret := new(v1.Service)
	if in != nil {
		*ret = *in
	}
	if s.updateService != nil {
		*ret = *s.updateService
	}
	if s.updateServiceStatus != nil {
		ret.Status = *s.updateServiceStatus
	}
	return ret
}

func TestControllerConfig(t *testing.T) {
	k := &testK8S{t: t}
	c := &controller{
		ips:    New(),
		client: k,
	}

	// Create service that would need an IP allocation

	l := log.NewNopLogger()
	svc := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	if c.SetBalancer(l, "test", svc, nil) != k8s.SyncStateError {
		t.Fatalf("SetBalancer should have failed")
	}

	gotSvc := k.gotService(svc)
	if gotSvc != nil {
		t.Errorf("SetBalancer with no configuration mutated service (-in +out)\n%s", diffService(svc, gotSvc))
	}
	if k.loggedWarning {
		t.Error("SetBalancer with no configuration logged an error")
	}

	// Set an empty config. Balancer should still not do anything to
	// our unallocated service, and return an error to force a
	// retry after sync is complete.
	if c.SetConfig(l, &purelbv1.Config{}) == k8s.SyncStateError {
		t.Fatalf("SetConfig with empty config failed")
	}
	if c.SetBalancer(l, "test", svc, nil) != k8s.SyncStateError {
		t.Fatal("SetBalancer did not fail")
	}

	gotSvc = k.gotService(svc)
	if gotSvc != nil {
		t.Errorf("unsynced SetBalancer mutated service (-in +out)\n%s", diffService(svc, gotSvc))
	}
	if k.loggedWarning {
		t.Error("unsynced SetBalancer logged an error")
	}

	// Set a config with some IPs. Still no allocation, not synced.
	cfg := &purelbv1.Config{
		Groups: []*purelbv1.ServiceGroup{
			&purelbv1.ServiceGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: purelbv1.ServiceGroupSpec{
					AutoAssign: true,
					Local: &purelbv1.ServiceGroupLocalSpec{
						Pool: "1.2.3.0/24",
					},
				},
			},
		},
	}
	if c.SetConfig(l, cfg) == k8s.SyncStateError {
		t.Fatalf("SetConfig failed")
	}
	if c.SetBalancer(l, "test", svc, nil) != k8s.SyncStateError {
		t.Fatal("SetBalancer did not fail")
	}

	gotSvc = k.gotService(svc)
	if gotSvc != nil {
		t.Errorf("unsynced SetBalancer mutated service (-in +out)\n%s", diffService(svc, gotSvc))
	}
	if k.loggedWarning {
		t.Error("unsynced SetBalancer logged an error")
	}

	// Mark synced. Finally, we can allocate.
	c.MarkSynced(l)

	if c.SetBalancer(l, "test", svc, nil) == k8s.SyncStateError {
		t.Fatalf("SetBalancer failed")
	}

	gotSvc = k.gotService(svc)
	wantSvc := new(v1.Service)
	*wantSvc = *svc
	wantSvc.Status = statusAssigned("1.2.3.0")
	wantSvc.ObjectMeta = metav1.ObjectMeta{
		Annotations: map[string]string{
			brandAnnotation: brand,
			poolAnnotation:  "default",
		},
	}
	if diff := diffService(wantSvc, gotSvc); diff != "" {
		t.Errorf("SetBalancer produced unexpected mutation (-want +got)\n%s", diff)
	}

	// Deleting the config also makes PureLB sad.
	if c.SetConfig(l, nil) != k8s.SyncStateError {
		t.Fatalf("SetConfig that deletes the config was accepted")
	}
}

func TestDeleteRecyclesIP(t *testing.T) {
	k := &testK8S{t: t}
	c := &controller{
		ips:    New(),
		client: k,
	}

	l := log.NewNopLogger()
	cfg := &purelbv1.Config{
		Groups: []*purelbv1.ServiceGroup{
			&purelbv1.ServiceGroup{
				Spec: purelbv1.ServiceGroupSpec{
					AutoAssign: true,
					Local: &purelbv1.ServiceGroupLocalSpec{
						Pool: "1.2.3.0/32",
					},
				},
			},
		},
	}
	if c.SetConfig(l, cfg) == k8s.SyncStateError {
		t.Fatal("SetConfig failed")
	}
	c.MarkSynced(l)

	svc1 := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	if c.SetBalancer(l, "test", svc1, nil) == k8s.SyncStateError {
		t.Fatal("SetBalancer svc1 failed")
	}
	gotSvc := k.gotService(svc1)
	if gotSvc == nil {
		t.Fatal("Didn't get a balancer for svc1")
	}
	if len(gotSvc.Status.LoadBalancer.Ingress) == 0 || gotSvc.Status.LoadBalancer.Ingress[0].IP != "1.2.3.0" {
		t.Fatal("svc1 didn't get an IP")
	}
	k.reset()

	// Second service should converge correctly, but not allocate an
	// IP because we have none left.
	svc2 := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	if c.SetBalancer(l, "test2", svc2, nil) == k8s.SyncStateError {
		t.Fatal("SetBalancer svc2 failed")
	}
	if k.gotService(svc2) != nil {
		t.Fatal("SetBalancer svc2 mutated svc2 even though it should not have allocated")
	}
	k.reset()

	// Deleting the first LB should tell us to reprocess all services.
	if c.SetBalancer(l, "test", nil, nil) != k8s.SyncStateReprocessAll {
		t.Fatal("SetBalancer with nil LB didn't tell us to reprocess all balancers")
	}

	// Setting svc2 should now allocate correctly.
	if c.SetBalancer(l, "test2", svc2, nil) == k8s.SyncStateError {
		t.Fatal("SetBalancer svc2 failed")
	}
	gotSvc = k.gotService(svc2)
	if gotSvc == nil {
		t.Fatal("Didn't get a balancer for svc2")
	}
	if len(gotSvc.Status.LoadBalancer.Ingress) == 0 || gotSvc.Status.LoadBalancer.Ingress[0].IP != "1.2.3.0" {
		t.Fatal("svc2 didn't get an IP")
	}
}
