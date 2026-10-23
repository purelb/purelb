// Copyright 2026 Acnodal Inc.
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

package k8s

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-kit/log"
	ptu "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
)

// A CRD stamped with this release is current; one stamped with another,
// or unstamped, is stale and raises one event however often it's checked;
// one that isn't installed is skipped.
func TestCheckCRDVersions(t *testing.T) {
	stamps := map[string]string{
		"servicegroups.purelb.io": `{"purelb.io/crd-version":"v1"}`,
		"lbnodeagents.purelb.io":  `{"purelb.io/crd-version":"v0"}`,
		"egresses.purelb.io":      `{}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		ann, ok := stamps[name]
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
			return
		}
		fmt.Fprintf(w, `{"kind":"CustomResourceDefinition","apiVersion":"apiextensions.k8s.io/v1","metadata":{"name":%q,"annotations":%s}}`, name, ann)
	}))
	defer srv.Close()

	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	require.NoError(t, err)
	events := record.NewFakeRecorder(10)
	c := &Client{client: cs, logger: log.NewNopLogger(), events: events}
	crdStale.Reset()

	stale := map[string]bool{}
	c.checkCRDVersions("v1", stale)
	assert.Equal(t, 0.0, ptu.ToFloat64(crdStale.WithLabelValues("servicegroups.purelb.io")))
	assert.Equal(t, 1.0, ptu.ToFloat64(crdStale.WithLabelValues("lbnodeagents.purelb.io")))
	assert.Equal(t, 1.0, ptu.ToFloat64(crdStale.WithLabelValues("egresses.purelb.io")))
	assert.Equal(t, 3, ptu.CollectAndCount(crdStale), "CRDs that aren't installed have no series")
	require.Len(t, events.Events, 2)
	for i := 0; i < 2; i++ {
		e := <-events.Events
		assert.Contains(t, e, "Warning CRDVersionMismatch")
	}

	c.checkCRDVersions("v1", stale)
	assert.Len(t, events.Events, 0, "a CRD that stays stale isn't evented again")
}
