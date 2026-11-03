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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"purelb.io/internal/egress/cnisock"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// Teardown's CNI step (D26): a snapshot passing every pod, the installer
// asked to remove the plugin, and a bounded wait for its report on that
// request.
func TestUnchain(t *testing.T) {
	newAgent := func(t *testing.T) *Agent {
		a := New(log.NewNopLogger(), testNode, "purelb-system")
		dir := t.TempDir()
		a.snapPath, a.desiredPath = filepath.Join(dir, "snap.json"), filepath.Join(dir, "desired.json")
		a.statusPath, a.recordDir = filepath.Join(dir, "status.json"), filepath.Join(dir, "sandboxes")
		a.unchainWait = 500 * time.Millisecond
		return a
	}
	chained := func(t *testing.T, a *Agent) {
		require.NoError(t, cnisock.WriteDesired(a.desiredPath, cnisock.Desired{Enabled: true}))
		require.NoError(t, cnisock.WriteSnapshot(a.snapPath, cnisock.Snapshot{Version: cnisock.SnapshotVersion, Written: time.Now(),
			MaxAgeSeconds: 3600, FailClosedNamespaces: []string{"voice"}, Pods: []cnisock.SnapshotPod{{Namespace: "voice", Name: "a", FailClosed: true}}}))
		require.NoError(t, cnisock.WriteStatus(a.statusPath, cnisock.InstallerStatus{Written: time.Now(), Chain: cnisock.ChainPresent}))
		require.NoError(t, cnisock.WriteRecord(a.recordDir, "c1", cnisock.Record{Namespace: "voice", Name: "a"}))
	}
	// installer answers the agent's request with chain and reason, after
	// answerAfter.
	const answerAfter = 150 * time.Millisecond
	installer := func(t *testing.T, a *Agent, chain, reason string) {
		go func() {
			for range 100 {
				if d, err := cnisock.ReadDesired(a.desiredPath); err == nil && !d.Enabled {
					time.Sleep(answerAfter)
					_ = cnisock.WriteStatus(a.statusPath, cnisock.InstallerStatus{Written: time.Now(), Chain: chain, Reason: reason,
						DesiredWritten: d.Written})
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	t.Run("never chained", func(t *testing.T) {
		a := newAgent(t)
		assert.True(t, a.unchain(nil))
		_, err := os.Stat(a.desiredPath)
		assert.ErrorIs(t, err, os.ErrNotExist, "D-idle: nothing written")
	})

	t.Run("removed", func(t *testing.T) {
		a := newAgent(t)
		chained(t, a)
		// A report on an earlier request doesn't count.
		require.NoError(t, cnisock.WriteStatus(a.statusPath, cnisock.InstallerStatus{Written: time.Now(), Chain: cnisock.ChainOff,
			DesiredWritten: time.Unix(1, 0)}))
		installer(t, a, cnisock.ChainOff, "")
		done := make(chan struct{})
		close(done)
		start := time.Now()
		require.True(t, a.unchain(done))
		assert.GreaterOrEqual(t, time.Since(start), answerAfter, "the report on its own request")
		assert.Less(t, time.Since(start), a.unchainWait)
		s, err := cnisock.ReadSnapshot(a.snapPath)
		require.NoError(t, err)
		assert.Empty(t, s.Pods, "every pod passes while the entry is still there")
		assert.Empty(t, s.FailClosedNamespaces)
		a.removePluginFiles()
		for _, p := range []string{a.snapPath, a.desiredPath, a.recordDir} {
			_, err := os.Stat(p)
			assert.ErrorIs(t, err, os.ErrNotExist, p)
		}
		_, err = os.Stat(a.statusPath)
		assert.NoError(t, err, "the installer's own")
	})

	t.Run("the writer's last snapshot", func(t *testing.T) {
		a := newAgent(t)
		chained(t, a)
		installer(t, a, cnisock.ChainOff, "")
		// The session's writer, stopping, writes once more.
		done := make(chan struct{})
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = cnisock.WriteSnapshot(a.snapPath, cnisock.Snapshot{Version: cnisock.SnapshotVersion, Written: time.Now(),
				MaxAgeSeconds: 3600, Pods: []cnisock.SnapshotPod{{Namespace: "voice", Name: "a", FailClosed: true}}})
			close(done)
		}()
		require.True(t, a.unchain(done))
		s, err := cnisock.ReadSnapshot(a.snapPath)
		require.NoError(t, err)
		assert.Empty(t, s.Pods, "written after the writer stopped")
	})

	t.Run("the CNI owns the entry", func(t *testing.T) {
		a := newAgent(t)
		chained(t, a)
		installer(t, a, cnisock.ChainPresent, cnisock.ReasonCNIOwned)
		start := time.Now()
		assert.False(t, a.unchain(nil))
		assert.Less(t, time.Since(start), a.unchainWait, "no point waiting on")
	})

	t.Run("no answer", func(t *testing.T) {
		a := newAgent(t)
		chained(t, a)
		start := time.Now()
		assert.False(t, a.unchain(nil))
		assert.GreaterOrEqual(t, time.Since(start), a.unchainWait)
		d, err := cnisock.ReadDesired(a.desiredPath)
		require.NoError(t, err)
		assert.False(t, d.Enabled, "kept for the installer, whenever it runs")
	})

	t.Run("no installer", func(t *testing.T) {
		a := newAgent(t)
		chained(t, a)
		require.NoError(t, cnisock.WriteStatus(a.statusPath, cnisock.InstallerStatus{Written: time.Now().Add(-time.Hour), Chain: cnisock.ChainPresent}))
		start := time.Now()
		assert.False(t, a.unchain(nil))
		assert.Less(t, time.Since(start), a.unchainWait, "nothing to wait for")
	})

	t.Run("enabled again", func(t *testing.T) {
		a := newAgent(t)
		chained(t, a)
		a.config.Store(&delivery{spec: &purelbv2.LBNodeAgentEgressSpec{Enabled: true}})
		start := time.Now()
		assert.False(t, a.unchain(nil))
		assert.Less(t, time.Since(start), a.unchainWait)
	})
}
