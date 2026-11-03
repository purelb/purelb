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
	"reflect"
	"sort"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// snapshotMaxAge is spec.egress.snapshotMaxAge's default (D35).
const snapshotMaxAge = time.Hour

// snapshotRefresh is the longest the snapshot goes unwritten while the
// agent runs: its age is how long the agent has been gone.
const snapshotRefresh = time.Minute

// snapshots is the running session's snapshot writer, for the age gauge.
var snapshots atomic.Pointer[snapshotWriter]

var snapshotAge = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Namespace: purelbv2.MetricsNamespace, Subsystem: "egress", Name: "cni_snapshot_age_seconds",
	Help: "Age of the CNI plugin's snapshot this agent last wrote; 0 before its first write, or with egress off.",
}, func() float64 {
	if w := snapshots.Load(); w != nil {
		if t := w.written.Load(); t != 0 {
			return time.Since(time.Unix(0, t)).Seconds()
		}
	}
	return 0
})

func init() {
	prometheus.MustRegister(snapshotAge)
}

// snapshotWriter keeps the plugin's snapshot on disk from its own
// goroutine (the queue goroutine does no file I/O): the latest content
// set, written when it changes and at least every refresh. Once the first
// is written it writes the desired state, so the purelb-cni installer
// chains the plugin only when the plugin has a snapshot to decide by.
type snapshotWriter struct {
	logger log.Logger
	node   string
	path   string
	// desiredPath is the installer's desired state, entry the conflist
	// entry it asks for.
	desiredPath string
	entry       []byte
	// want is the content to write, without Version and Written; set by
	// the queue goroutine.
	want atomic.Pointer[cnisock.Snapshot]
	kick chan struct{}
	// written is when it last wrote (Unix nanoseconds), for the gauge.
	written atomic.Int64
}

func newSnapshotWriter(logger log.Logger, node, path, desiredPath string, entry []byte) *snapshotWriter {
	return &snapshotWriter{logger: logger, node: node, path: path, desiredPath: desiredPath, entry: entry,
		kick: make(chan struct{}, 1)}
}

// set asks for s to be written.
func (w *snapshotWriter) set(s *cnisock.Snapshot) {
	w.want.Store(s)
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// run writes until stop closes. The file stays: it is what the plugin
// decides by while no agent runs.
func (w *snapshotWriter) run(stop <-chan struct{}) {
	var last *cnisock.Snapshot
	failing, desired := false, false
	t := time.NewTimer(snapshotRefresh)
	defer t.Stop()
	for {
		refresh := false
		select {
		case <-stop:
			return
		case <-w.kick:
		case <-t.C:
			refresh = true
		}
		s := w.want.Load()
		if s == nil || (!refresh && reflect.DeepEqual(s, last)) {
			continue
		}
		t.Reset(refreshOf(s))
		snap := *s
		snap.Version, snap.Written = cnisock.SnapshotVersion, time.Now()
		if err := cnisock.WriteSnapshot(w.path, snap); err != nil {
			if !failing {
				logging.Info(w.logger, "op", "egressSnapshot", "node", w.node, "path", w.path, "error", err,
					"msg", "can't write the CNI plugin's snapshot: while the agent is down it decides by an older one, or none")
			}
			failing = true
			continue
		}
		if failing {
			logging.Info(w.logger, "op", "egressSnapshot", "node", w.node, "path", w.path, "msg", "the CNI plugin's snapshot is written again")
		}
		failing, last = false, s
		w.written.Store(snap.Written.UnixNano())
		if !desired {
			desired = w.writeDesired()
		}
		logging.Debug(w.logger, "op", "egressSnapshot", "node", w.node, "pods", len(s.Pods),
			"failClosedNamespaces", len(s.FailClosedNamespaces), "msg", "snapshot written")
	}
}

// writeDesired asks the installer to chain the plugin (D34), reporting
// whether it did.
func (w *snapshotWriter) writeDesired() bool {
	if err := cnisock.WriteDesired(w.desiredPath, cnisock.Desired{Enabled: true, Entry: w.entry}); err != nil {
		logging.Info(w.logger, "op", "egressSnapshot", "node", w.node, "path", w.desiredPath, "error", err,
			"msg", "can't ask purelb-cni to chain the CNI plugin; retrying with the next snapshot")
		return false
	}
	logging.Info(w.logger, "op", "egressSnapshot", "node", w.node, "path", w.desiredPath,
		"msg", "asked purelb-cni to chain the CNI plugin")
	return true
}

// refreshOf is how often s is rewritten: often enough that a just-gone
// agent's snapshot is well inside its bound.
func refreshOf(s *cnisock.Snapshot) time.Duration {
	return min(snapshotRefresh, time.Duration(s.MaxAgeSeconds)*time.Second/4)
}

// snapshotOf is the plugin's view of pn (§8.1): every pod planned, by
// identity, whether a FailClosed policy selects it, and the namespaces a
// FailClosed policy covers. Sorted, so an unchanged plan gives an equal
// snapshot.
func snapshotOf(pn *podNode, pods []*corev1.Pod, spec purelbv2.LBNodeAgentEgressSpec) *cnisock.Snapshot {
	maxAge := snapshotMaxAge
	if spec.SnapshotMaxAge != nil {
		maxAge = spec.SnapshotMaxAge.Duration
	}
	s := &cnisock.Snapshot{MaxAgeSeconds: int64(maxAge / time.Second)}
	for _, pod := range pods {
		if st, ok := pn.pods[pod.Namespace+"/"+pod.Name]; ok {
			s.Pods = append(s.Pods, cnisock.SnapshotPod{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID),
				FailClosed: st.failClosed})
		}
	}
	sort.Slice(s.Pods, func(i, j int) bool {
		if s.Pods[i].Namespace != s.Pods[j].Namespace {
			return s.Pods[i].Namespace < s.Pods[j].Namespace
		}
		return s.Pods[i].Name < s.Pods[j].Name
	})
	for ns := range pn.failClosedNS {
		s.FailClosedNamespaces = append(s.FailClosedNamespaces, ns)
	}
	sort.Strings(s.FailClosedNamespaces)
	return s
}
