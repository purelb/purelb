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
	"time"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/logging"
)

// installerPoll is how often the agent reads the installer's status.
const installerPoll = 5 * time.Second

// installerStale is how old the installer's status may be before the
// agent takes the installer to be gone (it rewrites it every
// StatusRefresh).
const installerStale = 3 * cnisock.StatusRefresh

// pollInstaller reads the purelb-cni installer's status until stop
// closes, queueing the node's status when the chain condition it gives
// changes (§10.4).
func (a *Agent) pollInstaller(stop <-chan struct{}) {
	var lastReason, lastMsg string
	t := time.NewTicker(a.installerPoll)
	defer t.Stop()
	for {
		if st, err := cnisock.ReadStatus(a.statusPath); err == nil {
			a.installer.Store(&st)
		} else {
			a.installer.Store(nil)
		}
		if reason, msg := chainCondition(a.installer.Load(), a.now()); reason != lastReason || msg != lastMsg {
			lastReason, lastMsg = reason, msg
			if reason != "" {
				logging.Info(a.logger, "op", "egressCNI", "node", a.node, "reason", reason, "msg", msg)
			} else {
				logging.Info(a.logger, "op", "egressCNI", "node", a.node, "msg", "the CNI plugin is chained")
			}
			a.add(keyNode, false)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// chainCondition is the DatapathCompatible reason and message the
// installer's status st gives; "" while the chain holds the plugin.
// CNIChainMissing is a chain the installer won't fix; ChainMissing one it
// is fixing, or can't see.
func chainCondition(st *cnisock.InstallerStatus, now time.Time) (string, string) {
	switch {
	case st == nil:
		return "ChainMissing", "purelb-cni hasn't reported on this node: is its DaemonSet running here?"
	case now.Sub(st.Written) > installerStale:
		return "ChainMissing", "purelb-cni last reported " + now.Sub(st.Written).Round(time.Second).String() +
			" ago: is its DaemonSet running here?"
	case st.Chain == cnisock.ChainPresent:
		return "", ""
	case st.Chain == cnisock.ChainOff:
		return "ChainMissing", "purelb-cni hasn't seen this agent's request to chain the plugin yet"
	case st.Reason == cnisock.ReasonWriteWar || st.Reason == cnisock.ReasonNotAConflist:
		return "CNIChainMissing", st.Message
	}
	return "ChainMissing", st.Message
}
