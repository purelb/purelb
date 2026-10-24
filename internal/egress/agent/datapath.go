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
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"purelb.io/internal/kernelversion"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// livenessStuckAfter bounds one datapath item, as internal/k8s bounds one
// work item (§8.7).
const livenessStuckAfter = 120 * time.Second

// defaultMarkField is the mark byte when the LBNodeAgent sets none (D18).
const defaultMarkField = "0x000000ff"

// runner runs kernel work on its own goroutine (D-conc). Each item is a
// whole desired state, so only the latest submitted item runs; the queue
// goroutine never waits for the kernel.
type runner struct {
	next atomic.Pointer[func()]
	kick chan struct{}
	// started is when the running item began (Unix nanoseconds), 0 when
	// idle.
	started atomic.Int64
}

func newRunner() *runner {
	return &runner{kick: make(chan struct{}, 1)}
}

// submit replaces the pending item with f.
func (d *runner) submit(f func()) {
	d.next.Store(&f)
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// run runs items until stop closes.
func (d *runner) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-d.kick:
		}
		f := d.next.Swap(nil)
		if f == nil {
			continue
		}
		d.started.Store(time.Now().UnixNano())
		(*f)()
		d.started.Store(0)
	}
}

// live fails once one item has run for longer than livenessStuckAfter. An
// idle goroutine is always live.
func (d *runner) live(now time.Time) error {
	started := d.started.Load()
	if started == 0 {
		return nil
	}
	if stuck := now.Sub(time.Unix(0, started)); stuck > livenessStuckAfter {
		return fmt.Errorf("egress datapath goroutine stuck in one item for %s", stuck.Round(time.Second))
	}
	return nil
}

// probeCapability reports what this node's datapath supports. It runs on
// the datapath goroutine.
func probeCapability(spec purelbv2.LBNodeAgentEgressSpec) *purelbv2.EgressCapability {
	c := &purelbv2.EgressCapability{MarkField: spec.MarkField}
	if c.MarkField == "" {
		c.MarkField = defaultMarkField
	}
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return c
	}
	c.Kernel = unix.ByteSliceToString(uts.Release[:])
	v, err := kernelversion.Parse(c.Kernel)
	c.EgressCapable = err == nil && v.AtLeast(kernelversion.EgressMin)
	return c
}
