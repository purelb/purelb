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

package cniinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"purelb.io/internal/egress/cnisock"
)

type env struct {
	in                     *Installer
	conf, bin, self, flist string
	clock                  time.Time
}

// newEnv is an installer over temporary CNI directories holding
// flannel's conflist (mode 0640), with a fake clock.
func newEnv(t *testing.T) *env {
	root := t.TempDir()
	e := &env{conf: filepath.Join(root, "net.d"), bin: filepath.Join(root, "bin"), self: filepath.Join(root, "lbnodeagent"),
		clock: time.Unix(1700000000, 0)}
	for _, d := range []string{e.conf, e.bin, filepath.Join(root, "lib"), filepath.Join(root, "run")} {
		require.NoError(t, os.Mkdir(d, 0o755))
	}
	require.NoError(t, os.WriteFile(e.self, []byte("binary v1"), 0o755))
	// At the epoch, as in an image.
	require.NoError(t, os.Chtimes(e.self, time.Unix(0, 0), time.Unix(0, 0)))
	e.flist = filepath.Join(e.conf, "10-flannel.conflist")
	require.NoError(t, os.WriteFile(e.flist, []byte(flannelConf), 0o640))
	e.in = &Installer{Logger: log.NewNopLogger(), ConfDir: e.conf, BinDir: e.bin, Self: e.self,
		DesiredPath: filepath.Join(root, "lib", "cni-desired.json"), StatusPath: filepath.Join(root, "run", "cni-status.json"),
		now: func() time.Time { return e.clock }}
	return e
}

func (e *env) enable(t *testing.T, entry string) {
	require.NoError(t, cnisock.WriteDesired(e.in.DesiredPath, cnisock.Desired{Enabled: true, Entry: []byte(entry)}))
}

func (e *env) read(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func (e *env) status(t *testing.T) cnisock.InstallerStatus {
	s, err := cnisock.ReadStatus(e.in.StatusPath)
	require.NoError(t, err)
	return s
}

// The CNI rewrites its conflist after d.
func (e *env) cniRewrites(t *testing.T, d time.Duration, content string) {
	e.clock = e.clock.Add(d)
	require.NoError(t, os.WriteFile(e.flist, []byte(content), 0o640))
	e.in.reconcile()
}

var chained = strings.Replace(flannelConf, "    }\n  ]", "    },"+entry+"\n  ]", 1)

// D-idle: with no desired state, or egress off, nothing is written.
func TestInstallerIdle(t *testing.T) {
	e := newEnv(t)
	e.in.reconcile()
	assert.Equal(t, cnisock.ChainOff, e.status(t).Chain)
	require.NoError(t, cnisock.WriteDesired(e.in.DesiredPath, cnisock.Desired{Enabled: false}))
	e.in.reconcile()
	assert.Equal(t, cnisock.ChainOff, e.status(t).Chain)
	assert.Equal(t, flannelConf, e.read(t, e.flist))
	ents, _ := os.ReadDir(e.bin)
	assert.Empty(t, ents)
}

func TestInstallerChains(t *testing.T) {
	e := newEnv(t)
	e.enable(t, entry)
	e.in.reconcile()
	assert.Equal(t, "binary v1", e.read(t, filepath.Join(e.bin, "purelb-egress")))
	fi, err := os.Stat(filepath.Join(e.bin, "purelb-egress"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
	assert.Equal(t, chained, e.read(t, e.flist), "flannel's bytes and the entry, nothing else")
	fi, err = os.Stat(e.flist)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm(), "its mode kept")
	st := e.status(t)
	assert.Equal(t, cnisock.InstallerStatus{Version: cnisock.StatusVersion, Written: e.clock, Chain: cnisock.ChainPresent,
		Conflist: "10-flannel.conflist"}, st)
	ents, _ := os.ReadDir(e.conf)
	assert.Len(t, ents, 1, "no temporary file left")

	// Unchanged: neither written again.
	bin, err := os.Stat(filepath.Join(e.bin, "purelb-egress"))
	require.NoError(t, err)
	e.clock = e.clock.Add(time.Second)
	e.in.reconcile()
	after, err := os.Stat(e.flist)
	require.NoError(t, err)
	assert.True(t, os.SameFile(fi, after))
	binAfter, err := os.Stat(filepath.Join(e.bin, "purelb-egress"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(bin, binAfter), "the same binary isn't copied again")

	// The CNI rewrites it (a restart): chained again, no war.
	e.cniRewrites(t, 5*time.Second, flannelConf)
	assert.Equal(t, chained, e.read(t, e.flist))
	assert.Empty(t, e.in.reverts)

	// An entry other than the desired one (the CNI's own configuration's,
	// or an earlier agent's) is left as it is: chained.
	other := strings.Replace(chained, `"logLevel":"info"`, `"logLevel":"debug"`, 1)
	e.cniRewrites(t, 5*time.Second, other)
	assert.Equal(t, other, e.read(t, e.flist))
	assert.Equal(t, cnisock.ChainPresent, e.status(t).Chain)

	// An upgrade: the new installer's binary has the old one's size and
	// time (the epoch), not its content. Installed.
	up := *e.in
	up.selfSum, up.verified = nil, nil
	require.NoError(t, os.WriteFile(e.self, []byte("binary v2"), 0o755))
	require.NoError(t, os.Chtimes(e.self, time.Unix(0, 0), time.Unix(0, 0)))
	require.NoError(t, os.Chtimes(filepath.Join(e.bin, "purelb-egress"), time.Unix(0, 0), time.Unix(0, 0)))
	up.reconcile()
	assert.Equal(t, "binary v2", e.read(t, filepath.Join(e.bin, "purelb-egress")))
	// Replaced or removed by something else, after it was verified:
	// installed again.
	up.reconcile()
	require.NotNil(t, up.verified)
	require.NoError(t, os.WriteFile(filepath.Join(e.bin, "other"), []byte("binary v1"), 0o755))
	require.NoError(t, os.Rename(filepath.Join(e.bin, "other"), filepath.Join(e.bin, "purelb-egress")))
	up.reconcile()
	assert.Equal(t, "binary v2", e.read(t, filepath.Join(e.bin, "purelb-egress")))
	require.NoError(t, os.Remove(filepath.Join(e.bin, "purelb-egress")))
	up.reconcile()
	assert.Equal(t, "binary v2", e.read(t, filepath.Join(e.bin, "purelb-egress")))
}

// 5 writes undone within 10 s, each within 1 s, stop the installer; then
// it only reports whether the chain holds the plugin.
func TestInstallerWriteWar(t *testing.T) {
	e := newEnv(t)
	e.enable(t, entry)
	e.in.reconcile()
	for i := 1; i <= warReverts; i++ {
		require.False(t, e.in.stopped, "after %d reverts", i-1)
		e.cniRewrites(t, 300*time.Millisecond, flannelConf)
	}
	require.True(t, e.in.stopped)
	assert.Equal(t, flannelConf, e.read(t, e.flist), "left as the CNI wrote it")
	st := e.status(t)
	assert.Equal(t, cnisock.ChainMissing, st.Chain)
	assert.Equal(t, cnisock.ReasonWriteWar, st.Reason)

	// Further rewrites aren't touched; the CNI chaining the plugin
	// itself (Cilium's cni.configMap) is reported present.
	e.cniRewrites(t, 300*time.Millisecond, flannelConf)
	assert.Equal(t, flannelConf, e.read(t, e.flist))
	cilium := strings.Replace(chained, `"logLevel":"info"`, `"logLevel":"debug"`, 1)
	e.cniRewrites(t, 300*time.Millisecond, cilium)
	assert.Equal(t, cilium, e.read(t, e.flist), "not replaced")
	assert.Equal(t, cnisock.ChainPresent, e.status(t).Chain)
}

// No war: a single rewrite; rewrites more than 1 s after its write; and
// fewer than 5 within 10 s.
func TestInstallerNoWar(t *testing.T) {
	e := newEnv(t)
	e.enable(t, entry)
	e.in.reconcile()
	e.cniRewrites(t, 300*time.Millisecond, flannelConf)
	assert.Len(t, e.in.reverts, 1)
	for range 10 {
		e.cniRewrites(t, 1100*time.Millisecond, flannelConf)
	}
	assert.Len(t, e.in.reverts, 1, "each more than 1 s after its write")
	for range 3 {
		e.cniRewrites(t, 300*time.Millisecond, flannelConf)
	}
	assert.Len(t, e.in.reverts, 3, "the first is over 10 s old")
	e.cniRewrites(t, 11*time.Second, flannelConf) // re-inserted, not a revert
	e.cniRewrites(t, 300*time.Millisecond, flannelConf)
	assert.Len(t, e.in.reverts, 1, "the earlier ones left the 10 s window")
	assert.False(t, e.in.stopped)
	assert.Equal(t, chained, e.read(t, e.flist))
}

func TestInstallerCantChain(t *testing.T) {
	// No configuration yet: the binary is installed, and it waits.
	e := newEnv(t)
	require.NoError(t, os.Remove(e.flist))
	e.enable(t, entry)
	e.in.reconcile()
	assert.Equal(t, cnisock.ReasonNoConflist, e.status(t).Reason)
	assert.FileExists(t, filepath.Join(e.bin, "purelb-egress"))

	// A single-plugin .conf first: left alone.
	conf := `{"cniVersion":"0.3.1","name":"x","type":"bridge"}`
	require.NoError(t, os.WriteFile(filepath.Join(e.conf, "05-bridge.conf"), []byte(conf), 0o644))
	require.NoError(t, os.WriteFile(e.flist, []byte(flannelConf), 0o644))
	e.in.reconcile()
	st := e.status(t)
	assert.Equal(t, cnisock.ChainMissing, st.Chain)
	assert.Equal(t, cnisock.ReasonNotAConflist, st.Reason)
	assert.Equal(t, "05-bridge.conf", st.Conflist)
	assert.Equal(t, conf, e.read(t, filepath.Join(e.conf, "05-bridge.conf")))
	assert.Equal(t, flannelConf, e.read(t, e.flist), "not the primary")

	// No binary, no chain entry: the runtime would fail every ADD.
	e = newEnv(t)
	require.NoError(t, os.Remove(e.bin))
	e.enable(t, entry)
	e.in.reconcile()
	st = e.status(t)
	assert.Equal(t, cnisock.ReasonFailed, st.Reason)
	assert.Contains(t, st.Message, "installing the plugin binary")
	assert.Equal(t, flannelConf, e.read(t, e.flist))
}

// The status is written when it changes and every StatusRefresh.
func TestInstallerReport(t *testing.T) {
	e := newEnv(t)
	e.enable(t, entry)
	e.in.reconcile()
	first := e.status(t).Written
	e.clock = e.clock.Add(cnisock.StatusRefresh - time.Second)
	e.in.reconcile()
	assert.Equal(t, first, e.status(t).Written, "unchanged and fresh")
	e.clock = e.clock.Add(time.Second)
	e.in.reconcile()
	assert.Equal(t, e.clock, e.status(t).Written, "refreshed")
	require.NoError(t, os.Remove(e.in.DesiredPath))
	e.clock = e.clock.Add(time.Second)
	e.in.reconcile()
	assert.Equal(t, cnisock.ChainOff, e.status(t).Chain, "changed")
}

// Run follows the directories with inotify.
// disable writes the agent's wish to remove the plugin, at the clock.
func (e *env) disable(t *testing.T) time.Time {
	require.NoError(t, cnisock.WriteDesired(e.in.DesiredPath, cnisock.Desired{Enabled: false, Written: e.clock}))
	return e.clock
}

// Teardown (D26): the entry out of the conflist, every other byte as the
// CNI wrote it, then the binary once the entry stayed out.
func TestInstallerTeardown(t *testing.T) {
	e := newEnv(t)
	e.enable(t, entry)
	e.in.reconcile()
	require.Equal(t, chained, e.read(t, e.flist))
	bin := filepath.Join(e.bin, "purelb-egress")

	e.clock = e.clock.Add(time.Minute)
	asked := e.disable(t)
	e.in.reconcile()
	assert.Equal(t, flannelConf, e.read(t, e.flist), "flannel's bytes, as they were")
	fi, err := os.Stat(e.flist)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm())
	assert.Equal(t, "binary v1", e.read(t, bin), "kept while the removal may be undone")
	st := e.status(t)
	assert.Equal(t, cnisock.ChainPresent, st.Chain)
	assert.True(t, asked.Equal(st.DesiredWritten), "names the wish it acts on")
	assert.NotNil(t, e.in.retry, "looks again once the window passes")

	e.clock = e.clock.Add(revertWithin + time.Millisecond)
	e.in.reconcile()
	_, err = os.Stat(bin)
	assert.ErrorIs(t, err, os.ErrNotExist)
	st = e.status(t)
	assert.Equal(t, cnisock.ChainOff, st.Chain)
	assert.True(t, asked.Equal(st.DesiredWritten))

	// Done: nothing written again; a later CNI rewrite is left alone.
	e.cniRewrites(t, time.Minute, flannelConf)
	assert.Equal(t, flannelConf, e.read(t, e.flist))
	assert.Equal(t, cnisock.ChainOff, e.status(t).Chain)

	// Enabled again: chained again.
	e.enable(t, entry)
	e.in.reconcile()
	assert.Equal(t, chained, e.read(t, e.flist))
}

// An entry the CNI puts back each time it is removed is its own
// configuration's (Cilium's cni.configMap): the installer stops, keeps the
// binary the entry needs, and reports; once the CNI's configuration drops
// it, the binary goes.
func TestInstallerTeardownCNIOwned(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, os.WriteFile(e.flist, []byte(chained), 0o640))
	bin := filepath.Join(e.bin, "purelb-egress")
	require.NoError(t, os.WriteFile(bin, []byte("binary v1"), 0o755))
	e.disable(t)
	e.in.reconcile()
	for range warReverts {
		require.Equal(t, flannelConf, e.read(t, e.flist))
		e.cniRewrites(t, 50*time.Millisecond, chained)
	}
	st := e.status(t)
	assert.Equal(t, cnisock.ChainPresent, st.Chain)
	assert.Equal(t, cnisock.ReasonCNIOwned, st.Reason)
	assert.Contains(t, st.Message, "remove it from the CNI's own configuration")
	assert.Equal(t, chained, e.read(t, e.flist), "no longer edited")
	e.cniRewrites(t, time.Minute, chained)
	assert.Equal(t, chained, e.read(t, e.flist))
	assert.Equal(t, "binary v1", e.read(t, bin), "the entry still needs it")

	e.cniRewrites(t, time.Minute, flannelConf)
	_, err := os.Stat(bin)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, cnisock.ChainOff, e.status(t).Chain)

	// The war is about removing: enabled again, the installer chains.
	e.enable(t, entry)
	e.in.reconcile()
	assert.Equal(t, chained, e.read(t, e.flist))
	assert.False(t, e.in.stopped)
}

// Undone fewer times than a war: removed again each time, binary kept
// until it stays out.
func TestInstallerTeardownNoWar(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, os.WriteFile(e.flist, []byte(chained), 0o640))
	bin := filepath.Join(e.bin, "purelb-egress")
	require.NoError(t, os.WriteFile(bin, []byte("binary v1"), 0o755))
	e.disable(t)
	e.in.reconcile()
	for range warReverts - 1 {
		e.cniRewrites(t, 50*time.Millisecond, chained)
		require.Equal(t, flannelConf, e.read(t, e.flist), "removed again")
		require.Equal(t, "binary v1", e.read(t, bin))
	}
	e.clock = e.clock.Add(2 * time.Second)
	e.in.reconcile()
	_, err := os.Stat(bin)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestInstallerRun(t *testing.T) {
	e := newEnv(t)
	e.in.now = time.Now
	stop := make(chan struct{})
	done := make(chan error)
	go func() { done <- e.in.Run(stop) }()
	require.Eventually(t, func() bool { _, err := os.Stat(e.in.StatusPath); return err == nil }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, flannelConf, e.read(t, e.flist), "nothing until the agent asks")

	e.enable(t, entry)
	require.Eventually(t, func() bool { return e.read(t, e.flist) == chained }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(1100 * time.Millisecond) // past the revert window
	require.NoError(t, os.WriteFile(e.flist, []byte(flannelConf), 0o640))
	require.Eventually(t, func() bool { return e.read(t, e.flist) == chained }, 5*time.Second, 10*time.Millisecond)
	assert.Empty(t, e.in.reverts)

	// Teardown: the binary goes once the window passes, with no other
	// event to wake the installer.
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, cnisock.WriteDesired(e.in.DesiredPath, cnisock.Desired{Enabled: false, Written: time.Now()}))
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(e.bin, "purelb-egress"))
		return os.IsNotExist(err)
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, flannelConf, e.read(t, e.flist))

	close(stop)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return")
	}
}
