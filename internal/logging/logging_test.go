// Copyright 2020-2026 Acnodal Inc.
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

// These tests deliberately never call Init(). Init mutates the package
// global currentLevel, replaces os.Stderr, mutates the flag package and
// spawns a goroutine that lives for the process -- under -race, in a
// package whose tests share that global, calling it would be a landmine.
// Everything below exercises the pure pieces instead.
package logging

import (
	"errors"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want Level
	}{
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"Debug", LevelDebug},
		{"dEbUg", LevelDebug},
		{"info", LevelInfo},
		{"INFO", LevelInfo},
		// Anything unrecognised falls back to info rather than erroring:
		// a typo in PURELB_LOG_LEVEL must not silence operational logs.
		{"", LevelInfo},
		{"trace", LevelInfo},
		{"warn", LevelInfo},
		{"error", LevelInfo},
		{"  debug  ", LevelInfo}, // not trimmed -- pinning current behaviour
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseLevel(tc.in))
		})
	}
}

func TestLevelString(t *testing.T) {
	assert.Equal(t, "info", LevelInfo.String())
	assert.Equal(t, "debug", LevelDebug.String())
	// Round-trips through ParseLevel.
	assert.Equal(t, LevelInfo, ParseLevel(LevelInfo.String()))
	assert.Equal(t, LevelDebug, ParseLevel(LevelDebug.String()))
	// Any out-of-range value renders as info rather than a number.
	assert.Equal(t, "info", Level(99).String())
}

// TestDefaultLevel documents the package's starting state. Init() is what
// changes it, and these tests never call it.
func TestDefaultLevel(t *testing.T) {
	assert.Equal(t, LevelInfo, GetLevel())
	assert.False(t, IsDebugEnabled())
}

// captureLogger records every Log call and can be told to fail.
type captureLogger struct {
	calls [][]interface{}
	err   error
}

func (c *captureLogger) Logger() log.Logger {
	return log.LoggerFunc(func(keyvals ...interface{}) error {
		c.calls = append(c.calls, keyvals)
		return c.err
	})
}

func TestFilterLogger(t *testing.T) {
	t.Run("info passes at info level", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		require.NoError(t, l.Log("level", "info", "msg", "hello"))
		require.Len(t, c.calls, 1)
		assert.Equal(t, []interface{}{"level", "info", "msg", "hello"}, c.calls[0])
	})

	t.Run("debug is dropped at info level", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		// Dropped messages return nil, not an error: filtering is not a
		// failure, and callers ignore the return anyway.
		require.NoError(t, l.Log("level", "debug", "msg", "noisy"))
		assert.Empty(t, c.calls, "debug must not reach the downstream logger")
	})

	t.Run("debug passes at debug level", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelDebug}
		require.NoError(t, l.Log("level", "debug", "msg", "noisy"))
		assert.Len(t, c.calls, 1)
	})

	t.Run("info passes at debug level", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelDebug}
		require.NoError(t, l.Log("level", "info", "msg", "hello"))
		assert.Len(t, c.calls, 1)
	})

	t.Run("no level key is treated as info", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		require.NoError(t, l.Log("msg", "hello"))
		assert.Len(t, c.calls, 1, "an unlabelled message must not be silently dropped")
	})

	t.Run("odd length keyvals does not panic", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		// "level" as the final element has no following value; the i+1
		// bounds check is what stops this being an index panic.
		require.NotPanics(t, func() { _ = l.Log("level") })
		assert.Len(t, c.calls, 1)
	})

	t.Run("non-string keyvals are skipped", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		require.NoError(t, l.Log(42, "level", "debug"))
		assert.Empty(t, c.calls, "the level key is still found past a non-string")
	})

	t.Run("no keyvals at all", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		require.NoError(t, l.Log())
		assert.Len(t, c.calls, 1)
	})

	t.Run("downstream errors propagate", func(t *testing.T) {
		want := errors.New("write failed")
		c := &captureLogger{err: want}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		assert.ErrorIs(t, l.Log("level", "info"), want)
	})

	// The scan walks every element without tracking key/value position, so
	// the string "level" appearing as a VALUE is also treated as the level
	// key. Pinning current behaviour: a message like
	// Info(logger, "msg", "level", "debug") would be filtered out at info
	// level even though its real level is info.
	t.Run("level appearing as a value is also honoured", func(t *testing.T) {
		c := &captureLogger{}
		l := &filterLogger{downstream: c.Logger(), level: LevelInfo}
		require.NoError(t, l.Log("msg", "level", "debug"))
		assert.Empty(t, c.calls, "positional-naive scan treats this as debug")
	})
}

func TestDeformat(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantLevel  string
		wantCaller string
		wantMsg    string
	}{
		{
			name:       "klog info line",
			in:         "I0811 12:34:56.789012       1 file.go:42] hello world",
			wantLevel:  "info",
			wantCaller: "file.go:42",
			wantMsg:    "hello world",
		},
		{
			name:       "klog warning line",
			in:         "W0811 12:34:56.789012       1 file.go:42] careful",
			wantLevel:  "warn",
			wantCaller: "file.go:42",
			wantMsg:    "careful",
		},
		{
			name:       "klog error line",
			in:         "E0811 12:34:56.789012       1 file.go:42] broken",
			wantLevel:  "error",
			wantCaller: "file.go:42",
			wantMsg:    "broken",
		},
		{
			// Fatal is folded into error rather than getting its own level.
			name:       "klog fatal line maps to error",
			in:         "F0811 12:34:56.789012       1 file.go:42] dying",
			wantLevel:  "error",
			wantCaller: "file.go:42",
			wantMsg:    "dying",
		},
		{
			// An unrecognised severity letter keeps the default level but
			// still parses caller and message.
			name:       "unknown severity letter",
			in:         "X0811 12:34:56.789012       1 file.go:42] odd",
			wantLevel:  "info",
			wantCaller: "file.go:42",
			wantMsg:    "odd",
		},
		{
			name:       "empty message after the bracket",
			in:         "I0811 12:34:56.789012       1 file.go:42] ",
			wantLevel:  "info",
			wantCaller: "file.go:42",
			wantMsg:    "",
		},
		{
			// Under 30 bytes short-circuits before the regex runs.
			name:      "short line passes through",
			in:        "I0811 12:34:56",
			wantLevel: "info",
			wantMsg:   "I0811 12:34:56",
		},
		{
			name:      "long non-matching line passes through",
			in:        "this is a plain long line with no klog prefix at all",
			wantLevel: "info",
			wantMsg:   "this is a plain long line with no klog prefix at all",
		},
		{
			name:      "empty input",
			in:        "",
			wantLevel: "info",
			wantMsg:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			level, caller, msg := deformat([]byte(tc.in))
			assert.Equal(t, tc.wantLevel, level, "level")
			assert.Equal(t, tc.wantCaller, caller, "caller")
			assert.Equal(t, tc.wantMsg, msg, "msg")
		})
	}
}
