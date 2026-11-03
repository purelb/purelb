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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RecordDir is where the plugin records each pod sandbox it let start, one
// file per sandbox named by its container ID, removed at its DEL (design
// 8.1): the agent reports the node's sandboxes without one. /run is
// emptied at boot, as the sandboxes are.
const RecordDir = "/run/purelb/sandboxes"

// RecordVersion is the newest record format this build writes and reads,
// RecordMinVersion the oldest it reads (N±1, as the socket).
const (
	RecordVersion    = 1
	RecordMinVersion = 1
)

// Record is a sandbox the plugin let start: its pod and the addresses the
// primary gave it.
type Record struct {
	Version   int      `json:"version"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	UID       string   `json:"uid,omitempty"`
	IPs       []string `json:"ips,omitempty"`
}

// recordPath is container's record in dir. A container ID is the
// runtime's, so one that could leave dir, or be taken for a temporary
// file, is refused.
func recordPath(dir, container string) (string, error) {
	if container == "" || strings.HasPrefix(container, ".") ||
		strings.ContainsFunc(container, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
		}) {
		return "", fmt.Errorf("container ID %q can't name a record", container)
	}
	return filepath.Join(dir, container), nil
}

// WriteRecord records container's sandbox in dir, by atomic rename.
func WriteRecord(dir, container string, r Record) error {
	path, err := recordPath(dir, container)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	r.Version = RecordVersion
	return writeJSON(path, r)
}

// RemoveRecord removes container's record from dir, if it has one.
func RemoveRecord(dir, container string) error {
	path, err := recordPath(dir, container)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReadRecords reads every record in dir, by container ID; none when dir
// doesn't exist. A record this build can't read is left out, so its pod
// is reported as one without a record.
func ReadRecords(dir string) (map[string]Record, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Record{}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // removed meanwhile (its DEL)
		}
		var r Record
		if json.Unmarshal(b, &r) != nil || r.Version < RecordMinVersion || r.Version > RecordVersion {
			continue
		}
		out[e.Name()] = r
	}
	return out, nil
}
