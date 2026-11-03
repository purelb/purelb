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
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve runs a server of versions min to max on a fresh socket, recording
// the requests it handles.
func serve(t *testing.T, min, max int) (string, chan Request) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "egress.sock")
	l, err := Listen(path)
	require.NoError(t, err)
	got := make(chan Request, 4)
	srv := Server{Min: min, Max: max, Handle: func(r Request) Response {
		got <- r
		return Response{Result: ResultHeld}
	}}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	return path, got
}

func call(t *testing.T, cl Client, req Request) (Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return cl.Call(ctx, req)
}

func TestCall(t *testing.T) {
	path, got := serve(t, MinVersion, Version)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "only root connects")

	req := Request{Op: OpAdd, ContainerID: "c1", Netns: "/var/run/netns/x", IfName: "eth0", Namespace: "ns", Name: "p",
		UID: "u1", IPs: []string{"10.244.1.5", "fd00::5"}}
	resp, err := call(t, NewClient(path), req)
	require.NoError(t, err)
	assert.Equal(t, Response{Version: Version, Result: ResultHeld}, resp)
	req.Version = Version
	assert.Equal(t, req, <-got)

	// A socket an earlier agent left is replaced.
	l, err := Listen(path)
	require.NoError(t, err)
	require.NoError(t, l.Close())

	_, err = call(t, NewClient(filepath.Join(t.TempDir(), "none.sock")), req)
	assert.Error(t, err, "no agent")
}

// N±1: a newer client falls back to an older agent's version, which it
// still speaks; a client too new or too old is refused.
func TestVersions(t *testing.T) {
	path, got := serve(t, 1, 2)
	resp, err := call(t, Client{Path: path, Min: 2, Max: 3}, Request{Op: OpDel})
	require.NoError(t, err)
	assert.Equal(t, ResultHeld, resp.Result)
	assert.Equal(t, 2, resp.Version)
	assert.Equal(t, 2, (<-got).Version)

	resp, err = call(t, Client{Path: path, Min: 3, Max: 4}, Request{Op: OpDel})
	require.NoError(t, err)
	assert.Contains(t, resp.Error, "protocol version 4 not spoken")
	assert.Equal(t, 1, resp.MinVersion)
	assert.Equal(t, 2, resp.MaxVersion)

	path, _ = serve(t, 3, 4)
	resp, err = call(t, Client{Path: path, Min: 1, Max: 2}, Request{Op: OpDel})
	require.NoError(t, err)
	assert.Contains(t, resp.Error, "protocol version 2 not spoken", "an agent newer than N+1")
	assert.Empty(t, got, "nothing refused is handled")
}
