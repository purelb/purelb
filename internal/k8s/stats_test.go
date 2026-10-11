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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
)

func TestMetricsAddr(t *testing.T) {
	assert.Equal(t, ":7472", metricsAddr("", 7472), "empty host listens on every address")
	assert.Equal(t, "172.30.250.104:7472", metricsAddr("172.30.250.104", 7472))
	assert.Equal(t, "[2001:470:b8f3:250::104]:7472", metricsAddr("2001:470:b8f3:250::104", 7472),
		"an IPv6 host must be bracketed")
}

// TestMetricsAddrBindsIPv6 proves the address actually listens on an IPv6
// host; the unbracketed form failed with "too many colons in address".
func TestMetricsAddrBindsIPv6(t *testing.T) {
	// Probe IPv6 availability independently, so the skip can't swallow
	// the very "too many colons" failure this test exists to catch.
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	_ = probe.Close()

	ln, err := net.Listen("tcp", metricsAddr("::1", 0))
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	_ = conn.Close()
}

// TestLiveIdleIsAlwaysLive is the property that makes the wedge detector
// usable: a quiet cluster leaves the service-sync goroutine blocked in
// queue.Get() indefinitely, and that must never read as a wedge. A naive
// "time since last progress" stamp would fail this test.
func TestLiveIdleIsAlwaysLive(t *testing.T) {
	c := &Client{}
	assert.NoError(t, c.Live(), "no work item in flight is live")

	// Even with the clock arbitrarily far from any past activity.
	c.syncStartedAt.Store(0)
	assert.NoError(t, c.Live(), "an idle queue is live regardless of elapsed time")
}

func TestLiveDetectsStuckWorkItem(t *testing.T) {
	c := &Client{}

	c.syncStartedAt.Store(time.Now().UnixNano())
	assert.NoError(t, c.Live(), "a work item that just started is live")

	c.syncStartedAt.Store(time.Now().Add(-livenessStuckAfter + time.Second).UnixNano())
	assert.NoError(t, c.Live(), "just inside the threshold is still live")

	c.syncStartedAt.Store(time.Now().Add(-livenessStuckAfter - time.Second).UnixNano())
	err := c.Live()
	require.Error(t, err, "past the threshold the goroutine is wedged")
	assert.Contains(t, err.Error(), "stuck in one work item")
}

func TestReady(t *testing.T) {
	c := &Client{}
	err := c.Ready()
	require.Error(t, err, "cold caches cannot serve")
	assert.Contains(t, err.Error(), "caches not synced")

	c.cachesSynced.Store(true)
	assert.NoError(t, c.Ready(), "synced caches is the whole condition")
}

// TestLivenessIsIndependentOfReadiness is the invariant the startupProbe
// depends on. That probe points at /healthz, and kubelet restarts the
// container when it fails -- so a process that is merely not ready yet,
// or not ready any more, must still report live. Pointing the startup
// probe at /readyz instead crash-looped every agent whose address guard
// had legitimately stood down.
func TestLivenessIsIndependentOfReadiness(t *testing.T) {
	c := &Client{}
	require.Error(t, c.Ready(), "not ready: caches are cold")
	assert.NoError(t, c.Live(), "but live, so the startup probe passes")

	c.cachesSynced.Store(true)
	assert.NoError(t, c.Ready())
	assert.NoError(t, c.Live())
}

// TestSyncTrackedBracketsTheStamp exercises the real work-queue path
// rather than the arithmetic: it proves the stamp is set while sync is
// executing and cleared when it returns. synced{} is the cheapest item
// that reaches a caller-supplied callback.
func TestSyncTrackedBracketsTheStamp(t *testing.T) {
	var insideStamp int64
	var liveWhenStuck error

	c := &Client{
		logger: log.NewNopLogger(),
		queue:  workqueue.NewTypedRateLimitingQueue[queueItem](workqueue.DefaultTypedControllerRateLimiter[queueItem]()),
	}
	c.synced = func() {
		insideStamp = c.syncStartedAt.Load()
		// Backdate the live stamp to prove /healthz would fail if this
		// callback never returned.
		c.syncStartedAt.Store(time.Now().Add(-livenessStuckAfter - time.Second).UnixNano())
		liveWhenStuck = c.Live()
	}

	assert.NoError(t, c.Live(), "live before any work item")
	require.Equal(t, SyncStateSuccess, c.syncTracked(synced("")))

	assert.NotZero(t, insideStamp, "the stamp is set while sync runs")
	require.Error(t, liveWhenStuck, "a sync that overruns the threshold fails liveness")
	assert.Zero(t, c.syncStartedAt.Load(), "the stamp is cleared when sync returns")
	assert.NoError(t, c.Live(), "and the process is live again")
}

func TestHealthHandler(t *testing.T) {
	logger := log.NewNopLogger()

	rec := httptest.NewRecorder()
	healthHandler(logger, "readyz", func() error { return nil }, false)(
		rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	healthHandler(logger, "readyz", func() error { return errors.New("informer caches not synced") }, false)(
		rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"kubelet treats any non-2xx as a failure; 503 is the conventional one")
	// kubelet copies the body into the Unhealthy event, so the reason
	// has to travel in it.
	assert.Contains(t, rec.Body.String(), "informer caches not synced")
}

// TestServeMuxEndpoints drives the real routing over a real socket: the
// paths, the status codes and the metrics endpoint the probes share a
// port with. A path typo in serveMux is invisible to the handler tests.
func TestServeMuxEndpoints(t *testing.T) {
	c := &Client{}
	srv := httptest.NewServer(serveMux(log.NewNopLogger(), c))
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := srv.Client().Get(srv.URL + path)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	// Before cache sync: live (nothing is wedged) but not ready. This is
	// the state the startupProbe sits in while informers prime.
	code, _ := get("/healthz")
	assert.Equal(t, http.StatusOK, code, "a starting process is not a wedged one")
	code, body := get("/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "caches not synced")

	c.cachesSynced.Store(true)
	code, _ = get("/readyz")
	assert.Equal(t, http.StatusOK, code, "ready once the caches are warm")

	// A wedged service-sync goroutine fails liveness, and readiness is
	// deliberately unaffected: the restart is what fixes it.
	c.syncStartedAt.Store(time.Now().Add(-livenessStuckAfter - time.Second).UnixNano())
	code, body = get("/healthz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "stuck in one work item")

	// Metrics still share the port.
	code, body = get("/metrics")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "purelb_", "the PureLB metrics are on the same listener")
}

// TestServeMuxOverIPv6 proves the probe endpoints answer over IPv6, not
// just IPv4. kubelet probes the pod IP, which on an IPv6-only or
// v6-primary dual-stack cluster is a v6 address.
func TestServeMuxOverIPv6(t *testing.T) {
	ln, err := net.Listen("tcp", metricsAddr("::1", 0))
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}

	c := &Client{}
	c.cachesSynced.Store(true)
	srv := &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: serveMux(log.NewNopLogger(), c)},
	}
	srv.Start()
	defer srv.Close()

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := srv.Client().Get(srv.URL + path)
		require.NoError(t, err, path)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
	}
}
