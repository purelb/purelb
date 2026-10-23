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

package allocator

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ownerHarness plays the queue goroutine: worker notifications arrive on
// done, and the test calls writer.done for each.
type ownerHarness struct {
	w    *writer
	done chan string
	stop chan struct{}
}

func newOwnerHarness(t *testing.T) *ownerHarness {
	h := &ownerHarness{done: make(chan string, 100), stop: make(chan struct{})}
	h.w = newWriter(4, func(key string, _ bool) { h.done <- key })
	h.w.run(h.stop)
	t.Cleanup(func() { close(h.stop) })
	return h
}

// collect waits for one notification and collects it, returning the key
// and the write's error.
func (h *ownerHarness) collect(t *testing.T) (string, error) {
	select {
	case key := <-h.done:
		err, ok := h.w.done(key)
		require.True(t, ok, "a notification has an outcome")
		return key, err
	case <-time.After(5 * time.Second):
		t.Fatal("no write finished")
		return "", nil
	}
}

// At most four writes are in flight.
func TestWriterConcurrencyCap(t *testing.T) {
	h := newOwnerHarness(t)
	gate := make(chan struct{})
	var now, peak atomic.Int32
	for i := 0; i < 10; i++ {
		h.w.submit(fmt.Sprintf("obj%d", i), false, func(context.Context) error {
			n := now.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-gate
			now.Add(-1)
			return nil
		})
	}
	require.Eventually(t, func() bool { return now.Load() == 4 }, time.Second, time.Millisecond)
	close(gate)
	for i := 0; i < 10; i++ {
		_, err := h.collect(t)
		assert.NoError(t, err)
	}
	assert.Equal(t, int32(4), peak.Load())
}

// One object never has two writes in flight, and only its latest waiting
// write runs after the one in flight.
func TestWriterSerializesAnObject(t *testing.T) {
	h := newOwnerHarness(t)
	gate := make(chan struct{})
	var ran []int
	write := func(n int, block bool) func(context.Context) error {
		return func(context.Context) error {
			if block {
				<-gate
			}
			ran = append(ran, n) // writes of one object never overlap
			return nil
		}
	}
	h.w.submit("obj", false, write(1, true))
	h.w.submit("obj", false, write(2, false))
	h.w.submit("obj", false, write(3, false))
	assert.True(t, h.w.inProgress("obj"))
	close(gate)
	h.collect(t)
	h.collect(t)
	assert.Equal(t, []int{1, 3}, ran, "write 2 was superseded while waiting")
	assert.False(t, h.w.inProgress("obj"))
}

// A priority write goes out before ordinary writes queued earlier.
func TestWriterPriorityFirst(t *testing.T) {
	h := newOwnerHarness(t)
	gate := make(chan struct{})
	for i := 0; i < 4; i++ {
		h.w.submit(fmt.Sprintf("busy%d", i), false, func(context.Context) error { <-gate; return nil })
	}
	var order []string
	h.w.submit("ordinary", false, func(context.Context) error { order = append(order, "ordinary"); return nil })
	h.w.submit("claim", true, func(context.Context) error { order = append(order, "claim"); return nil })
	gate <- struct{}{} // free one worker
	h.collect(t)       // a busy write
	h.collect(t)       // the next dispatched: the claim
	close(gate)
	for i := 0; i < 4; i++ {
		h.collect(t)
	}
	assert.Equal(t, []string{"claim", "ordinary"}, order)
}

// A write's error reaches the owner, and a second notification for the
// same outcome is ignored.
func TestWriterDeliversErrors(t *testing.T) {
	h := newOwnerHarness(t)
	boom := errors.New("conflict")
	h.w.submit("obj", false, func(context.Context) error { return boom })
	key, err := h.collect(t)
	assert.Equal(t, "obj", key)
	assert.ErrorIs(t, err, boom)
	_, ok := h.w.done("obj")
	assert.False(t, ok)
}
