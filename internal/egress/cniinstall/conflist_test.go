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

package cniinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flannelConf is prox-purelb2's conflist, byte for byte.
const flannelConf = `{
  "name": "cbr0",
  "cniVersion": "0.3.1",
  "plugins": [
    {
      "type": "flannel",
      "delegate": {
        "hairpinMode": true,
        "isDefaultGateway": true
      }
    },
    {
      "type": "portmap",
      "capabilities": {
        "portMappings": true
      }
    }
  ]
}
`

const entry = `{"type":"purelb-egress","purelbNamespace":"purelb-system","logLevel":"info"}`

func TestChainInsert(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"flannel, indented", flannelConf, strings.Replace(flannelConf, "    }\n  ]", "    },"+entry+"\n  ]", 1)},
		{"compact", `{"name":"x","plugins":[{"type":"a"}]}`, `{"name":"x","plugins":[{"type":"a"},` + entry + `]}`},
		{"empty chain", `{"plugins": [ ], "name":"x"}`, `{"plugins": [ ` + entry + `], "name":"x"}`},
		{"plugins before other keys, nested plugins key", `{"plugins":[{"type":"a","plugins":[1]}],"x":{"plugins":[]}}`,
			`{"plugins":[{"type":"a","plugins":[1]},` + entry + `],"x":{"plugins":[]}}`},
		{"a nested plugins key first", `{"x":{"plugins":[{"type":"b"}]},"plugins":[{"type":"a"}]}`,
			`{"x":{"plugins":[{"type":"b"}]},"plugins":[{"type":"a"},` + entry + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseChain([]byte(tc.in))
			require.NoError(t, err)
			out := c.insert([]byte(tc.in), []byte(entry))
			assert.Equal(t, tc.want, string(out), "only the entry and its comma added")
			require.True(t, json.Valid(out))
			c2, err := parseChain(out)
			require.NoError(t, err)
			i := c2.find("purelb-egress")
			require.Equal(t, len(c2.plugins)-1, i, "last in the chain")
			assert.Equal(t, entry, string(out[c2.plugins[i].start:c2.plugins[i].end]))
		})
	}
}

// A chain's plugin types, in order.
func TestChainTypes(t *testing.T) {
	c, err := parseChain([]byte(`{"plugins":[{"type":"a"},  {"type":"purelb-egress","logLevel":"debug"} ,{"type":"b"}]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "purelb-egress", "b"}, c.types)
	assert.Equal(t, 1, c.find("purelb-egress"))
	assert.Equal(t, -1, c.find("nope"))
}

func TestChainNotAConflist(t *testing.T) {
	for _, in := range []string{
		`{"cniVersion":"0.3.1","name":"x","type":"bridge"}`, // a .conf
		`{"plugins":{"type":"a"}}`,
		`[{"type":"a"}]`,
		`{"plugins":[{"type":"a"}`,
		``,
	} {
		_, err := parseChain([]byte(in))
		assert.Error(t, err, in)
	}
}

// The primary configuration is libcni's: the lexically first .conf,
// .conflist or .json, hidden or not; directories and other names aren't.
func TestPrimary(t *testing.T) {
	dir := t.TempDir()
	p, err := primary(dir)
	require.NoError(t, err)
	assert.Empty(t, p)
	for _, n := range []string{"20-b.conflist", "10-a.conf", ".purelb-123", "00-notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "05-dir.conflist"), 0o755))
	p, err = primary(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "10-a.conf"), p)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".00-hidden.json"), []byte("{}"), 0o644))
	p, _ = primary(dir)
	assert.Equal(t, filepath.Join(dir, ".00-hidden.json"), p)
	_, err = primary(filepath.Join(dir, "missing"))
	assert.Error(t, err)
}
