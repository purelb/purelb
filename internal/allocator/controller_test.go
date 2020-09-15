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
	loggedWarning       bool
	t                   *testing.T
}

func (s *testK8S) Infof(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Info event %q: %s", evtType, fmt.Sprintf(msg, args...))
}

func (s *testK8S) Errorf(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Warning event %q: %s", evtType, fmt.Sprintf(msg, args...))
	s.loggedWarning = true
}

func (s *testK8S) reset() {
	s.loggedWarning = false
}

func TestControllerConfig(t *testing.T) {
	k := &testK8S{t: t}
	c := &controller{
		logger: log.NewNopLogger(),
		ips:    New(),
		client: k,
	}

	// Create service that would need an IP allocation

	svc := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	if c.SetBalancer("test", svc, nil) != k8s.SyncStateError {
		t.Fatalf("SetBalancer should have failed")
	}
	if k.loggedWarning {
		t.Error("SetBalancer with no configuration logged an error")
	}

	// Set an empty config. Balancer should still not do anything to
	// our unallocated service, and return an error to force a
	// retry after sync is complete.
	wantSvc := svc.DeepCopy()
	if c.SetConfig(&purelbv1.Config{}) == k8s.SyncStateError {
		t.Fatalf("SetConfig with empty config failed")
	}
	if c.SetBalancer("test", svc, nil) != k8s.SyncStateError {
		t.Fatal("SetBalancer did not fail")
	}

	if diff := diffService(wantSvc, svc); diff != "" {
		t.Errorf("unsynced SetBalancer mutated service (-in +out)\n%s", diff)
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
					Local: &purelbv1.ServiceGroupLocalSpec{
						Pool: "1.2.3.0/24",
					},
				},
			},
		},
	}
	if c.SetConfig(cfg) == k8s.SyncStateError {
		t.Fatalf("SetConfig failed")
	}
	wantSvc = svc.DeepCopy()
	if c.SetBalancer("test", svc, nil) != k8s.SyncStateError {
		t.Fatal("SetBalancer did not fail")
	}

	if diff := diffService(wantSvc, svc); diff != "" {
		t.Errorf("unsynced SetBalancer mutated service (-in +out)\n%s", diff)
	}
	if k.loggedWarning {
		t.Error("unsynced SetBalancer logged an error")
	}

	// Mark synced. Finally, we can allocate.
	c.MarkSynced()

	wantSvc = svc.DeepCopy()
	wantSvc.Status = statusAssigned("1.2.3.0")
	wantSvc.ObjectMeta = metav1.ObjectMeta{
		Annotations: map[string]string{
			brandAnnotation: brand,
			poolAnnotation:  "default",
		},
	}

	if c.SetBalancer("test", svc, nil) == k8s.SyncStateError {
		t.Fatalf("SetBalancer failed")
	}

	if diff := diffService(wantSvc, svc); diff != "" {
		t.Errorf("SetBalancer produced unexpected mutation (-want +got)\n%s", diff)
	}

	// Deleting the config also makes PureLB sad.
	if c.SetConfig(nil) != k8s.SyncStateError {
		t.Fatalf("SetConfig that deletes the config was accepted")
	}
}

func TestDeleteRecyclesIP(t *testing.T) {
	k := &testK8S{t: t}
	c := &controller{
		logger: log.NewNopLogger(),
		ips:    New(),
		client: k,
	}

	cfg := &purelbv1.Config{
		Groups: []*purelbv1.ServiceGroup{
			&purelbv1.ServiceGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: purelbv1.ServiceGroupSpec{
					Local: &purelbv1.ServiceGroupLocalSpec{
						Pool: "1.2.3.0/32",
					},
				},
			},
		},
	}
	if c.SetConfig(cfg) == k8s.SyncStateError {
		t.Fatal("SetConfig failed")
	}
	c.MarkSynced()

	svc1 := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	if c.SetBalancer("test", svc1, nil) == k8s.SyncStateError {
		t.Fatal("SetBalancer svc1 failed")
	}
	if len(svc1.Status.LoadBalancer.Ingress) == 0 || svc1.Status.LoadBalancer.Ingress[0].IP != "1.2.3.0" {
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
	if c.SetBalancer("test2", svc2, nil) == k8s.SyncStateError {
		t.Fatal("SetBalancer svc2 failed")
	}
	if len(svc2.Status.LoadBalancer.Ingress) > 0 {
		t.Fatal("svc2 has the wrong address")
	}
	k.reset()

	// Deleting the first LB should tell us to reprocess all services.
	if c.DeleteBalancer("test") != k8s.SyncStateReprocessAll {
		t.Fatal("DeleteBalancer didn't tell us to reprocess all balancers")
	}

	// Setting svc2 should now allocate correctly.
	if c.SetBalancer("test2", svc2, nil) == k8s.SyncStateError {
		t.Fatal("SetBalancer svc2 failed")
	}
	if len(svc2.Status.LoadBalancer.Ingress) == 0 || svc2.Status.LoadBalancer.Ingress[0].IP != "1.2.3.0" {
		t.Fatal("svc2 didn't get an IP")
	}
}
