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

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"purelb.io/internal/egress/cnisock"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// The snapshot lists every planned pod, FailClosed when any FailClosed
// policy selects it (its slice being Active or not), and the namespaces a
// FailClosed policy covers.
func TestSnapshotOf(t *testing.T) {
	closed := pnEgress("voice", "closed", 100, nil)
	openToo := pnEgress("voice", "open-too", 200, func(p *pnPolicy) { p.spec.FailurePolicy = "FailOpen" })
	open := pnEgress("web", "open", 100, func(p *pnPolicy) { p.spec.FailurePolicy = "FailOpen" })
	pod := func(ns, name string, l map[string]string) *corev1.Pod {
		p := pnPod(ns, name, l, "10.1.0.1")
		p.UID = types.UID("u-" + name)
		return p
	}
	host := pod("voice", "host", appA)
	host.Spec.HostNetwork = true
	in := pnIn("Active", []*corev1.Pod{pod("web", "w", appA), pod("voice", "b", nil), pod("voice", "a", appA), host},
		closed, openToo, open)
	pn := planPodNode(in, nil)
	assert.False(t, pn.pods["voice/a"].hold, "its slice is Active")

	s := snapshotOf(pn, in.pods, purelbv2.LBNodeAgentEgressSpec{})
	assert.Equal(t, &cnisock.Snapshot{MaxAgeSeconds: 3600, FailClosedNamespaces: []string{"voice"},
		Pods: []cnisock.SnapshotPod{
			{Namespace: "voice", Name: "a", UID: "u-a", FailClosed: true},
			{Namespace: "voice", Name: "b", UID: "u-b"},
			{Namespace: "web", Name: "w", UID: "u-w"},
		}}, s, "host-network pods never reach the plugin")

	s = snapshotOf(pn, in.pods, purelbv2.LBNodeAgentEgressSpec{SnapshotMaxAge: &metav1.Duration{Duration: time.Minute}})
	assert.Equal(t, int64(60), s.MaxAgeSeconds)
	assert.Equal(t, 15*time.Second, refreshOf(s), "a quarter of a short bound")
	assert.Equal(t, time.Minute, refreshOf(&cnisock.Snapshot{MaxAgeSeconds: 3600}))
}

// The writer writes what is set, again only when it changes or its
// refresh comes, and retries a failed write.
func TestSnapshotWriter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "var-lib-purelb")
	path := filepath.Join(dir, "egress-snapshot.json")
	desired := filepath.Join(dir, "cni-desired.json")
	entry := cnisock.PluginEntry("purelb-system", "info")
	w := newSnapshotWriter(log.NewNopLogger(), testNode, path, desired, entry)
	snapshots.Store(w)
	t.Cleanup(func() { snapshots.Store(nil) })
	assert.Zero(t, testutil.ToFloat64(snapshotAge), "not written yet")
	stop := make(chan struct{})
	defer close(stop)
	go w.run(stop)

	read := func() cnisock.Snapshot {
		t.Helper()
		s, err := cnisock.ReadSnapshot(path)
		require.NoError(t, err)
		return s
	}
	exists := func() bool { _, err := os.Stat(path); return err == nil }
	// Its directory missing (no hostPath): nothing is written. A failed
	// write is retried the next time the content is set, even unchanged.
	w.set(&cnisock.Snapshot{MaxAgeSeconds: 3600})
	time.Sleep(100 * time.Millisecond)
	require.False(t, exists())
	require.NoError(t, os.Mkdir(dir, 0o700))
	w.set(&cnisock.Snapshot{MaxAgeSeconds: 3600})
	require.Eventually(t, exists, 3*time.Second, 20*time.Millisecond)
	// Once there is a snapshot, the installer is asked to chain the plugin.
	require.Eventually(t, func() bool { _, err := cnisock.ReadDesired(desired); return err == nil }, 3*time.Second, 20*time.Millisecond)
	d, err := cnisock.ReadDesired(desired)
	require.NoError(t, err)
	assert.True(t, d.Enabled)
	assert.JSONEq(t, `{"type":"purelb-egress","purelbNamespace":"purelb-system","logLevel":"info"}`, string(d.Entry))
	require.NoError(t, os.Remove(desired))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Remove(dir))

	// And at the next refresh, nothing set.
	quick := &cnisock.Snapshot{MaxAgeSeconds: 1, FailClosedNamespaces: []string{"voice"}}
	w.set(quick)
	time.Sleep(100 * time.Millisecond)
	require.False(t, exists())
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.Eventually(t, exists, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, cnisock.SnapshotVersion, read().Version)
	assert.Equal(t, []string{"voice"}, read().FailClosedNamespaces)

	// Rewritten every refresh (a quarter of its 1 s bound) with nothing set.
	first := read().Written
	require.Eventually(t, func() bool { return read().Written.After(first) }, 3*time.Second, 20*time.Millisecond)
	assert.Greater(t, testutil.ToFloat64(snapshotAge), 0.0)
	assert.Less(t, testutil.ToFloat64(snapshotAge), 1.0)

	// A one-hour bound refreshes each minute: the same content set again
	// isn't written, new content at once.
	slow := &cnisock.Snapshot{MaxAgeSeconds: 3600}
	w.set(slow)
	require.Eventually(t, func() bool { return read().MaxAgeSeconds == 3600 }, 3*time.Second, 20*time.Millisecond)
	written := read().Written
	w.set(&cnisock.Snapshot{MaxAgeSeconds: 3600})
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, written, read().Written, "unchanged")
	w.set(&cnisock.Snapshot{MaxAgeSeconds: 3600, Pods: []cnisock.SnapshotPod{{Namespace: "voice", Name: "a"}}})
	require.Eventually(t, func() bool { return len(read().Pods) == 1 }, 3*time.Second, 20*time.Millisecond)
}
