// Copyright 2020-2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"testing"
	"time"

	"github.com/go-kit/log"
	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	purelbfake "purelb.io/pkg/generated/clientset/versioned/fake"
	"purelb.io/pkg/generated/informers/externalversions"
)

func newServiceGroup(name string) *purelbv2.ServiceGroup {
	return &purelbv2.ServiceGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: purelbv2.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceDefault,
		},
	}
}

// TestSGUpdateNeedsReconcile covers the Generation-aware UpdateFunc
// filter that prevents the allocator's own status writes (which don't
// bump .metadata.generation) from triggering full reconcile cycles over
// every service.
func TestSGUpdateNeedsReconcile(t *testing.T) {
	withGen := func(name string, gen int64) *purelbv2.ServiceGroup {
		sg := newServiceGroup(name)
		sg.Generation = gen
		return sg
	}

	cases := []struct {
		name     string
		old, new interface{}
		want     bool
	}{
		{
			name: "same generation (status-only update) -> skip",
			old:  withGen("sg", 3), new: withGen("sg", 3),
			want: false,
		},
		{
			name: "generation bumped (spec change) -> reconcile",
			old:  withGen("sg", 3), new: withGen("sg", 4),
			want: true,
		},
		{
			name: "old not a ServiceGroup -> reconcile (fail safe)",
			old:  "not-an-sg", new: withGen("sg", 3),
			want: true,
		},
		{
			name: "new not a ServiceGroup -> reconcile (fail safe)",
			old:  withGen("sg", 3), new: nil,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sgUpdateNeedsReconcile(tc.old, tc.new); got != tc.want {
				t.Errorf("sgUpdateNeedsReconcile = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSyncHandlerRequeuesOnConfigError pins the SyncStateError
// contract: the delivery is retried with rate-limited backoff (a
// transient failure inside the callback must not leave the consumer on
// stale config until the next CR event), the error metric still
// increments, and forceSync is not called.
func TestSyncHandlerRequeuesOnConfigError(t *testing.T) {
	purelbClient := purelbfake.NewSimpleClientset()
	factory := externalversions.NewSharedInformerFactory(purelbClient, 0)

	state := SyncStateError
	calls := 0
	c := &Controller{
		logger:     log.NewNopLogger(),
		configCB:   func(*purelbv2.Config) SyncState { calls++; return state },
		forceSync:  func() { t.Error("forceSync must not be called on SyncStateError") },
		sgLister:   factory.Purelb().V2().ServiceGroups().Lister(),
		lbnaLister: factory.Purelb().V2().LBNodeAgents().Lister(),
		workqueue:  workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "test"}),
	}
	defer c.workqueue.ShutDown()

	errorsBefore := ptu.ToFloat64(updateErrors.WithLabelValues("config"))

	c.workqueue.Add("lbna/default/test")
	if !c.processNextWorkItem() {
		t.Fatal("processNextWorkItem returned shutdown")
	}
	if calls != 1 {
		t.Fatalf("configCB calls = %d, want 1", calls)
	}
	if got := ptu.ToFloat64(updateErrors.WithLabelValues("config")); got != errorsBefore+1 {
		t.Errorf("updateErrors = %v, want %v", got, errorsBefore+1)
	}

	// The rate limiter re-adds after its base delay; poll for the requeue.
	deadline := time.Now().Add(5 * time.Second)
	for c.workqueue.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c.workqueue.Len() != 1 {
		t.Fatal("item was not requeued after SyncStateError")
	}

	// A subsequent success consumes the requeued item and Forgets it —
	// no further retries.
	state = SyncStateSuccess
	if !c.processNextWorkItem() {
		t.Fatal("processNextWorkItem returned shutdown")
	}
	time.Sleep(50 * time.Millisecond)
	if c.workqueue.Len() != 0 {
		t.Errorf("queue length after success = %d, want 0", c.workqueue.Len())
	}
}
