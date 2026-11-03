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
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// errNotAConflist: the file has no plugins list to chain into (a
// single-plugin .conf, or not a JSON object).
var errNotAConflist = errors.New("not a conflist: it has no plugins list")

// span is a value's bytes in a file, [start, end).
type span struct{ start, end int }

// chain is where a conflist's plugins are, by byte offset, so the
// installer changes only its own entry and every other byte stays as the
// CNI wrote it (§10.4).
type chain struct {
	plugins []span
	types   []string
	// close is the offset of the plugins list's ']'.
	close int
}

// parseChain finds the top-level plugins list of the conflist b.
func parseChain(b []byte) (*chain, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errNotAConflist
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if k != "plugins" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('[') {
			return nil, errNotAConflist
		}
		c := &chain{}
		for dec.More() {
			// A RawMessage is the value's exact bytes, and the offset is
			// just past them.
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, err
			}
			end := int(dec.InputOffset())
			var p struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &p)
			c.plugins = append(c.plugins, span{end - len(raw), end})
			c.types = append(c.types, p.Type)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		c.close = int(dec.InputOffset()) - 1
		return c, nil
	}
	return nil, errNotAConflist
}

// find is the index of the plugin of type t, or -1.
func (c *chain) find(t string) int {
	return slices.Index(c.types, t)
}

// insert is b with entry appended to the chain, after the last plugin.
func (c *chain) insert(b, entry []byte) []byte {
	if len(c.plugins) == 0 {
		return splice(b, c.close, c.close, entry)
	}
	at := c.plugins[len(c.plugins)-1].end
	return splice(b, at, at, append([]byte(","), entry...))
}

// remove is b without the chain's plugin i, every other byte as it was:
// with the comma before it, or after it when it is first. It undoes
// insert exactly.
func (c *chain) remove(b []byte, i int) []byte {
	switch {
	case i > 0:
		return splice(b, c.plugins[i-1].end, c.plugins[i].end, nil)
	case len(c.plugins) > 1:
		return splice(b, c.plugins[0].start, c.plugins[1].start, nil)
	}
	return splice(b, c.plugins[0].start, c.plugins[0].end, nil)
}

func splice(b []byte, start, end int, with []byte) []byte {
	out := make([]byte, 0, len(b)-(end-start)+len(with))
	out = append(out, b[:start]...)
	out = append(out, with...)
	return append(out, b[end:]...)
}

// primary is the CNI configuration the runtime uses: the lexically first
// .conf, .conflist or .json file in dir (libcni's order); "" if none.
func primary(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range ents {
		switch filepath.Ext(e.Name()) {
		case ".conf", ".conflist", ".json":
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	slices.Sort(names)
	return filepath.Join(dir, names[0]), nil
}
