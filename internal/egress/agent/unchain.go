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
	"errors"
	"os"
	"time"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/logging"
)

// unchainWait bounds teardown's wait for the purelb-cni installer to take
// the plugin out of the CNI chain; then the rest of the teardown goes on
// (D26). unchainPoll is how often its status is read meanwhile.
const unchainWait, unchainPoll = 30 * time.Second, 200 * time.Millisecond

// snapshotWait bounds the wait for the session's snapshot writer to stop.
const snapshotWait = 5 * time.Second

// unchain asks the purelb-cni installer to take the plugin out of the CNI
// chain and waits, up to a.unchainWait, for it to report the chain entry
// and binary gone (D26). First it writes a snapshot that passes every
// pod: no agent answers the plugin from now on, and until the entry is
// gone the plugin decides by it. It reports whether nothing of the
// plugin's is left in the CNI directories, true when it was never asked
// in (no desired state, D-idle). Datapath goroutine only.
func (a *Agent) unchain(snapDone <-chan struct{}) bool {
	if snapDone != nil {
		select {
		case <-snapDone:
		case <-time.After(snapshotWait):
		}
	}
	d, err := cnisock.ReadDesired(a.desiredPath)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "desired", a.desiredPath, "error", err,
			"msg", "can't read the CNI installer's desired state: the plugin stays chained")
		return false
	}
	if d.Enabled {
		// Pods start while the entry is in the chain, whatever selected
		// them: their policies are gone with egress.
		pass := cnisock.Snapshot{Version: cnisock.SnapshotVersion, Written: a.now(), MaxAgeSeconds: int64(snapshotMaxAge.Seconds())}
		if err := cnisock.WriteSnapshot(a.snapPath, pass); err != nil {
			logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "snapshot", a.snapPath, "error", err,
				"msg", "can't write the snapshot that passes every pod")
		}
		d = cnisock.Desired{Version: cnisock.DesiredVersion, Enabled: false, Written: a.now()}
		if err := cnisock.WriteDesired(a.desiredPath, d); err != nil {
			logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "desired", a.desiredPath, "error", err,
				"msg", "can't ask the CNI installer to remove the plugin: it stays chained, and passes every pod")
			return false
		}
		logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "msg", "asked the CNI installer to remove the plugin")
	}
	deadline := time.Now().Add(a.unchainWait)
	for {
		st, err := cnisock.ReadStatus(a.statusPath)
		if err != nil || a.now().Sub(st.Written) > installerStale {
			logging.Info(a.logger, "op", "egressTeardown", "node", a.node,
				"msg", "no CNI installer is running: the plugin stays chained, passing every pod, until it runs")
			return false
		}
		if st.DesiredWritten.Equal(d.Written) {
			switch {
			case st.Chain == cnisock.ChainOff:
				logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "msg", "the plugin is out of the CNI chain")
				return true
			case st.Reason == cnisock.ReasonCNIOwned:
				logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "conflist", st.Conflist, "detail", st.Message,
					"msg", "the CNI's own configuration chains the plugin: it stays, passing every pod")
				return false
			}
		}
		if c := a.config.Load(); c != nil && c.spec != nil && c.spec.Enabled {
			return false // enabled again
		}
		if time.Now().After(deadline) {
			logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "chain", st.Chain, "reason", st.Reason, "detail", st.Message,
				"wait", a.unchainWait.String(), "msg", "the CNI installer hasn't removed the plugin; going on, the plugin passing every pod")
			return false
		}
		time.Sleep(unchainPoll)
	}
}

// removePluginFiles removes what only the plugin reads, once nothing can
// run it: the snapshot, its sandbox records, and the installer's desired
// state (with none it writes nothing, D-idle). Datapath goroutine only.
func (a *Agent) removePluginFiles() {
	for _, path := range []string{a.snapPath, a.desiredPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "path", path, "error", err)
		}
	}
	if err := os.RemoveAll(a.recordDir); err != nil {
		logging.Info(a.logger, "op", "egressTeardown", "node", a.node, "path", a.recordDir, "error", err)
	}
}
