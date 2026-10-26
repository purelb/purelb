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
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/go-kit/log"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"purelb.io/internal/logging"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
)

// statusInterval is the most often the agent writes its status (D6).
const statusInterval = time.Second

// statusRetry is how long a failed write waits before the next attempt.
const statusRetry = 5 * time.Second

// statusWriter writes this node's EgressNodeState status from its own
// goroutine, at most once per statusInterval, always the latest status
// set. The agent is the status's only writer, so each write replaces the
// whole status and needs no resourceVersion.
type statusWriter struct {
	logger log.Logger
	node   string
	kube   kubernetes.Interface
	purelb versioned.Interface

	// want is the status to write; off is a request to report egress off
	// instead (capability egressCapable false), which a later set
	// replaces. Set by the queue goroutine.
	want atomic.Pointer[purelbv2.EgressNodeStateStatus]
	off  atomic.Bool
	kick chan struct{}

	interval, retry time.Duration
	// written is the status last written. Writer goroutine only.
	written *purelbv2.EgressNodeStateStatus
}

func newStatusWriter(logger log.Logger, node string, kube kubernetes.Interface, purelb versioned.Interface) *statusWriter {
	return &statusWriter{logger: logger, node: node, kube: kube, purelb: purelb,
		kick: make(chan struct{}, 1), interval: statusInterval, retry: statusRetry}
}

// set asks for st to be written.
func (w *statusWriter) set(st *purelbv2.EgressNodeStateStatus) {
	w.want.Store(st)
	w.off.Store(false)
	w.wake()
}

// disable asks for the status to say egress is off, if the object exists.
func (w *statusWriter) disable() {
	w.want.Store(nil)
	w.off.Store(true)
	w.wake()
}

func (w *statusWriter) wake() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// run writes until stop closes.
func (w *statusWriter) run(stop <-chan struct{}) {
	var last time.Time
	for {
		select {
		case <-stop:
			return
		case <-w.kick:
		}
		if wait := w.interval - time.Since(last); wait > 0 {
			select {
			case <-stop:
				return
			case <-time.After(wait):
			}
		}
		last = time.Now()
		if err := w.write(); err != nil {
			logging.Info(w.logger, "op", "egressStatus", "node", w.node, "error", err)
			go func() {
				select {
				case <-stop:
				case <-time.After(w.retry):
					w.wake()
				}
			}()
		}
	}
}

// write writes the latest request.
func (w *statusWriter) write() error {
	if w.off.Load() {
		return w.writeOff()
	}
	st := w.want.Load()
	if st == nil || (w.written != nil && equality.Semantic.DeepEqual(st, w.written)) {
		return nil
	}
	err := w.patch(st)
	if k8serrors.IsNotFound(err) {
		if err = w.create(); err == nil {
			err = w.patch(st)
		}
	}
	if err != nil {
		w.written = nil
		return err
	}
	w.written = st
	logging.Debug(w.logger, "op", "egressStatus", "node", w.node, "msg", "status written")
	return nil
}

// writeOff reports egress off in an existing object, leaving the rest of
// its status alone.
func (w *statusWriter) writeOff() error {
	ctx, cancel := apiContext()
	defer cancel()
	cur, err := w.purelb.PurelbV2().EgressNodeStates().Get(ctx, w.node, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	w.written = nil
	capable := cur.Status.Capability != nil && cur.Status.Capability.EgressCapable
	if !capable && len(cur.Status.Announcing) == 0 && len(cur.Status.Conditions) == 0 && len(cur.Status.AddressFaults) == 0 {
		return nil
	}
	st := cur.Status.DeepCopy()
	if st.Capability != nil {
		st.Capability.EgressCapable = false
	}
	// A node with egress off holds nothing: an ack left behind would keep
	// it in the allocator's gating (D6), and a condition or fault left
	// behind would go on failing its policies.
	st.Announcing, st.Conditions, st.AddressFaults = nil, nil, nil
	if err := w.patch(st); err != nil {
		return err
	}
	logging.Info(w.logger, "op", "egressStatus", "node", w.node, "msg", "reported egress off")
	return nil
}

// patch replaces the whole status.
func (w *statusWriter) patch(st *purelbv2.EgressNodeStateStatus) error {
	body, _ := json.Marshal([]map[string]interface{}{{"op": "add", "path": "/status", "value": st}})
	ctx, cancel := apiContext()
	defer cancel()
	_, err := w.purelb.PurelbV2().EgressNodeStates().Patch(ctx, w.node, types.JSONPatchType, body, metav1.PatchOptions{}, "status")
	return err
}

// create creates this node's EgressNodeState, owned by its Node (D6); the
// allocator may have created it first.
func (w *statusWriter) create() error {
	ctx, cancel := apiContext()
	defer cancel()
	n, err := w.kube.CoreV1().Nodes().Get(ctx, w.node, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ens := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: w.node, OwnerReferences: []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Node", Name: n.Name, UID: n.UID}}}}
	_, err = w.purelb.PurelbV2().EgressNodeStates().Create(ctx, ens, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		return nil
	}
	if err == nil {
		logging.Info(w.logger, "op", "egressStatus", "node", w.node, "msg", "created EgressNodeState")
	}
	return err
}
