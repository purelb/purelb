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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "egress-snapshot.json")
	_, err := ReadSnapshot(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	s := Snapshot{Version: SnapshotVersion, Written: time.Unix(1700000000, 0).UTC(), MaxAgeSeconds: 3600,
		FailClosedNamespaces: []string{"web"},
		Pods:                 []SnapshotPod{{Namespace: "web", Name: "a", UID: "u", FailClosed: true}, {Namespace: "db", Name: "b"}}}
	require.NoError(t, WriteSnapshot(path, s))
	got, err := ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, s, got)

	// Replaced whole, no temporary file left beside it.
	s.Pods = s.Pods[:1]
	require.NoError(t, WriteSnapshot(path, s))
	got, err = ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, s, got)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// A version this build doesn't read, or a damaged file, is refused.
	for _, v := range []int{0, SnapshotVersion + 1} {
		s.Version = v
		require.NoError(t, WriteSnapshot(path, s))
		_, err = ReadSnapshot(path)
		require.ErrorContains(t, err, "this plugin reads")
	}
	require.NoError(t, os.WriteFile(path, []byte(`{"version":`), 0o600))
	_, err = ReadSnapshot(path)
	require.ErrorContains(t, err, "parsing")

	// A directory it can't write in fails the write.
	require.Error(t, WriteSnapshot(filepath.Join(dir, "missing", "egress-snapshot.json"), s))
	// A failed rename (a directory in the way) leaves no temporary file.
	blocked := filepath.Join(t.TempDir(), "egress-snapshot.json")
	require.NoError(t, os.MkdirAll(filepath.Join(blocked, "x"), 0o700))
	require.Error(t, WriteSnapshot(blocked, s))
	entries, err = os.ReadDir(filepath.Dir(blocked))
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the directory in the way")
}
