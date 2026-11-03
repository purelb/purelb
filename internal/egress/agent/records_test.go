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
	"maps"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/egress/datapath"
)

// Sandboxes without a plugin record (design 8.1): pods whose address is
// on a device the datapath found, which neither the socket nor the
// plugin's records account for, are reported once a session.
func TestSyncRecords(t *testing.T) {
	a := New(log.NewNopLogger(), testNode, "purelb-system")
	a.recordDir = filepath.Join(t.TempDir(), "sandboxes")
	events := record.NewFakeRecorder(16)
	a.events = events
	var kicked []string
	enqueue := func(k string, _ bool) { kicked = append(kicked, k) }
	a.enqueue.Store(&enqueue)
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	s := &session{pods: pods, reported: map[types.UID]bool{}}
	pn := &podNode{pods: map[string]podState{"web/old": {selected: true}}}

	pod := func(name, uid string, ips ...string) *corev1.Pod {
		p := pnPod("web", name, nil, ips...)
		p.UID = types.UID(uid)
		require.NoError(t, pods.Add(p))
		return p
	}
	pod("told", "u-told", "10.1.0.1", "fd04::1") // by the socket
	pod("recorded", "u-rec", "10.1.0.2")         // by the plugin, while no agent ran
	pod("byname", "u-name", "10.1.0.3")          // a record without a UID
	pod("old", "u-old", "10.1.0.4", "fd04::4")   // predates the plugin
	pod("stale", "u-stale", "10.1.0.5")          // its status' address isn't wired (a reboot)
	pod("redone", "u-redone", "10.1.0.6")        // only its deleted sandbox has the address
	host := pod("host", "u-host", "10.1.0.7")
	host.Spec.HostNetwork = true
	require.NoError(t, pods.Update(host))
	ending := pod("ending", "u-ending", "10.1.0.8")
	ending.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	require.NoError(t, pods.Update(ending))
	other := pod("other", "u-other", "10.1.0.9")
	other.Spec.NodeName = "elsewhere"
	require.NoError(t, pods.Update(other))
	done := pod("done", "u-done", "10.1.0.10")
	done.Status.Phase = corev1.PodSucceeded
	require.NoError(t, pods.Update(done))

	tagged := map[netip.Addr]datapath.PodDev{}
	for _, ip := range []string{"10.1.0.1", "fd04::1", "10.1.0.2", "10.1.0.3", "fd04::4", "10.1.0.6", "10.1.0.7", "10.1.0.8",
		"10.1.0.9", "10.1.0.10"} {
		tagged[netip.MustParseAddr(ip)] = datapath.PodDev{}
	}
	a.tagged.Store(&tagged)
	a.sandboxes.Store("c-told", &cniSandbox{namespace: "web", name: "told", uid: "u-told", ips: []netip.Addr{netip.MustParseAddr("fd04::1")}})
	a.sandboxes.Store("c-redone", &cniSandbox{namespace: "web", name: "redone", uid: "u-redone",
		ips: []netip.Addr{netip.MustParseAddr("10.1.0.6")}, deleted: true})
	require.NoError(t, cnisock.WriteRecord(a.recordDir, "c-rec", cnisock.Record{Namespace: "web", Name: "recorded", UID: "u-rec", IPs: []string{"10.1.0.2"}}))
	require.NoError(t, cnisock.WriteRecord(a.recordDir, "c-name", cnisock.Record{Namespace: "web", Name: "byname", IPs: []string{"10.1.0.3"}}))
	// The socket's DEL stands over a record read after it.
	require.NoError(t, cnisock.WriteRecord(a.recordDir, "c-redone", cnisock.Record{Namespace: "web", Name: "redone", UID: "u-redone", IPs: []string{"10.1.0.6"}}))

	a.syncRecords(s, pn)
	assert.Equal(t, []string{keyPods}, kicked, "the records' sandboxes join the plan")
	_, ok := a.sandboxes.Load("c-rec")
	assert.True(t, ok)
	sb, _ := a.sandboxes.Load("c-redone")
	assert.True(t, sb.(*cniSandbox).deleted)
	reported := func() []string {
		var out []string
		for {
			select {
			case e := <-events.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}
	got := reported()
	require.Len(t, got, 2)
	for _, e := range got {
		assert.True(t, strings.HasPrefix(e, "Warning "+reasonUnrecorded+" The pod's sandbox started without PureLB's CNI plugin"), e)
	}
	assert.Equal(t, map[types.UID]bool{"u-old": true, "u-redone": true}, s.reported)
	assert.Equal(t, 2.0, testutil.ToFloat64(cniUnrecorded))

	// Once a session: no second event, still counted; the records aren't
	// read again for pods already reported.
	kicked = nil
	require.NoError(t, cnisock.WriteRecord(a.recordDir, "c-late", cnisock.Record{Namespace: "web", Name: "x", UID: "u-x"}))
	a.syncRecords(s, pn)
	assert.Empty(t, reported())
	assert.Empty(t, kicked)
	_, ok = a.sandboxes.Load("c-late")
	assert.False(t, ok)
	assert.Equal(t, 2.0, testutil.ToFloat64(cniUnrecorded))

	// The stale pod's new sandbox, recorded by the plugin: read when its
	// address is wired, and not reported.
	wired := maps.Clone(tagged)
	wired[netip.MustParseAddr("10.1.0.5")] = datapath.PodDev{}
	a.tagged.Store(&wired)
	require.NoError(t, cnisock.WriteRecord(a.recordDir, "c-stale2", cnisock.Record{Namespace: "web", Name: "stale", UID: "u-stale", IPs: []string{"10.1.0.5"}}))
	a.syncRecords(s, pn)
	assert.Empty(t, reported())
	assert.Equal(t, []string{keyPods}, kicked)

	// A pod gone is forgotten, and no longer counted.
	require.NoError(t, pods.Delete(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "old"}}))
	a.syncRecords(s, pn)
	assert.Equal(t, map[types.UID]bool{"u-redone": true}, s.reported)
	assert.Equal(t, 1.0, testutil.ToFloat64(cniUnrecorded))
}
