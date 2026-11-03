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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sandboxes")
	got, err := ReadRecords(dir)
	require.NoError(t, err, "no directory yet: no records")
	assert.Empty(t, got)

	a := Record{Namespace: "web", Name: "a", UID: "u-a", IPs: []string{"10.1.0.5", "fd04::5"}}
	require.NoError(t, WriteRecord(dir, "0123abcd", a))
	require.NoError(t, WriteRecord(dir, "k8s_POD-b.1", Record{Namespace: "db", Name: "b"}))
	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	got, err = ReadRecords(dir)
	require.NoError(t, err)
	a.Version = RecordVersion
	assert.Equal(t, map[string]Record{"0123abcd": a, "k8s_POD-b.1": {Version: RecordVersion, Namespace: "db", Name: "b"}}, got)

	// IDs that could leave the directory, or pass for a temporary file.
	for _, id := range []string{"", "../x", "a/b", ".purelb-1", ".", "..", "a b"} {
		assert.Error(t, WriteRecord(dir, id, a), id)
		assert.Error(t, RemoveRecord(dir, id), id)
	}

	// A record this build can't read, a temporary file and a directory
	// are left out.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "newer"), []byte(`{"version":2,"namespace":"web","name":"c"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "torn"), []byte(`{"vers`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".purelb-9"), []byte(`{"version":1,"namespace":"web","name":"d"}`), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	got, err = ReadRecords(dir)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	require.NoError(t, RemoveRecord(dir, "0123abcd"))
	require.NoError(t, RemoveRecord(dir, "0123abcd"), "already gone")
	got, err = ReadRecords(dir)
	require.NoError(t, err)
	assert.NotContains(t, got, "0123abcd")
}
