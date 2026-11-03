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

package cnisock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SnapshotPath is the agent's snapshot on the node (hostPath
// /var/lib/purelb): it survives reboots.
const SnapshotPath = "/var/lib/purelb/egress-snapshot.json"

// SnapshotVersion is the newest snapshot format this build writes and
// reads, SnapshotMinVersion the oldest it reads (N±1, as the socket).
const (
	SnapshotVersion    = 1
	SnapshotMinVersion = 1
)

// Snapshot is what the plugin decides by while no agent answers (D35):
// the node's pods as the agent last planned them, and how long that may
// be trusted.
type Snapshot struct {
	Version int `json:"version"`
	// Written is when the agent last wrote it; the agent rewrites it while
	// it runs, so its age is how long the agent has been gone.
	Written time.Time `json:"written"`
	// MaxAgeSeconds is the age past which the plugin passes pods through
	// (spec.egress.snapshotMaxAge).
	MaxAgeSeconds int64 `json:"maxAgeSeconds"`
	// FailClosedNamespaces are those a FailClosed policy covers: a pod
	// there that the snapshot doesn't list may be one it selects.
	FailClosedNamespaces []string      `json:"failClosedNamespaces,omitempty"`
	Pods                 []SnapshotPod `json:"pods,omitempty"`
}

// SnapshotPod is one of the node's pods.
type SnapshotPod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
	// FailClosed: a FailClosed policy selects it.
	FailClosed bool `json:"failClosed,omitempty"`
}

// WriteSnapshot replaces the snapshot at path whole: a reader sees the old
// one or the new one, and a crash leaves one of them.
func WriteSnapshot(path string, s Snapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".egress-snapshot-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after the rename, there is nothing to remove
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ReadSnapshot reads the snapshot at path, refusing a version this build
// doesn't read.
func ReadSnapshot(path string) (Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parsing %s: %w", path, err)
	}
	if s.Version < SnapshotMinVersion || s.Version > SnapshotVersion {
		return s, fmt.Errorf("%s is version %d; this plugin reads %d to %d", path, s.Version, SnapshotMinVersion, SnapshotVersion)
	}
	return s, nil
}
