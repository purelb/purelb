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
	"sync"
	"time"
)

// writeTimeout bounds one API write.
const writeTimeout = 10 * time.Second

// writer performs the allocator's egress API writes on a few goroutines,
// so a slow write doesn't hold up the next, while every decision stays on
// the queue goroutine (D-conc). Only the HTTP calls run on the workers.
//
// The queue goroutine (the owner) submits writes and is told of each
// completion by notify, which queues the object's write key; it then
// collects the outcome with done. Everything but results and the jobs
// channel belongs to the owner, so there are no locks:
//   - at most workers writes are in flight;
//   - one object never has two writes in flight. A write submitted for an
//     object with one in flight waits behind it, replacing any write
//     already waiting, because only the latest desired state matters;
//   - priority writes (the claim path) go out before ordinary ones.
type writer struct {
	workers int
	jobs    chan writeJob
	notify  func(key string, priority bool)

	// results carries each finished write's error from its worker to the
	// owner. A worker stores, the owner loads and deletes.
	results sync.Map // object key -> error (nil stored as errNone)

	// Owner only.
	inFlight int
	busy     map[string]bool
	waiting  map[string]writeJob // per busy object, the write to run next
	queued   [2][]string         // object keys ready to dispatch: [priority, ordinary]
	pending  map[string]writeJob // the queued write per object key
}

type writeJob struct {
	key      string
	priority bool
	write    func(context.Context) error
}

// errNone stands for a nil error in results.
type errNone struct{}

func (errNone) Error() string { return "" }

func newWriter(workers int, notify func(key string, priority bool)) *writer {
	return &writer{
		workers: workers,
		jobs:    make(chan writeJob, workers),
		notify:  notify,
		busy:    map[string]bool{},
		waiting: map[string]writeJob{},
		pending: map[string]writeJob{},
	}
}

// run starts the workers; they stop when stop closes.
func (w *writer) run(stop <-chan struct{}) {
	for i := 0; i < w.workers; i++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				case j := <-w.jobs:
					ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
					err := j.write(ctx)
					cancel()
					if err == nil {
						err = errNone{}
					}
					w.results.Store(j.key, err)
					w.notify(j.key, j.priority)
				}
			}
		}()
	}
}

// submit schedules write for the object key. Owner only.
func (w *writer) submit(key string, priority bool, write func(context.Context) error) {
	j := writeJob{key: key, priority: priority, write: write}
	switch {
	case w.busy[key]:
		if prev, ok := w.waiting[key]; ok && prev.priority {
			j.priority = true
		}
		w.waiting[key] = j
	case w.pending[key].write != nil:
		w.pending[key] = j
		if priority {
			w.queue(key, true)
		}
	default:
		w.pending[key] = j
		w.queue(key, priority)
	}
	w.dispatch()
}

func (w *writer) queue(key string, priority bool) {
	lane := 1
	if priority {
		lane = 0
	}
	w.queued[lane] = append(w.queued[lane], key)
}

// dispatch hands queued writes to the workers, priority first, up to the
// worker count. The jobs channel holds as many as there are workers, so
// this never blocks.
func (w *writer) dispatch() {
	for w.inFlight < w.workers {
		key, ok := w.next()
		if !ok {
			return
		}
		j := w.pending[key]
		delete(w.pending, key)
		w.busy[key] = true
		w.inFlight++
		w.jobs <- j
	}
}

// next pops the next queued key that still has a pending write.
func (w *writer) next() (string, bool) {
	for lane := range w.queued {
		for len(w.queued[lane]) > 0 {
			key := w.queued[lane][0]
			w.queued[lane] = w.queued[lane][1:]
			if w.pending[key].write != nil {
				return key, true
			}
		}
	}
	return "", false
}

// done collects the outcome of the write that just finished for key, as
// notified, and starts any write waiting behind it. ok is false if no
// outcome is waiting (a duplicate notification). Owner only.
func (w *writer) done(key string) (err error, ok bool) {
	v, ok := w.results.LoadAndDelete(key)
	if !ok {
		return nil, false
	}
	w.inFlight--
	delete(w.busy, key)
	if j, waiting := w.waiting[key]; waiting {
		delete(w.waiting, key)
		w.pending[key] = j
		w.queue(key, j.priority)
	}
	w.dispatch()
	if _, none := v.(errNone); none {
		return nil, true
	}
	return v.(error), true
}

// inProgress reports whether key has a write in flight or waiting. Owner
// only.
func (w *writer) inProgress(key string) bool {
	return w.busy[key] || w.pending[key].write != nil
}
