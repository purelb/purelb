// Copyright 2017 Google Inc.
// Copyright 2020 Acnodal Inc.
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

// Package logging sets up structured logging in a uniform way, and
// redirects glog statements into the structured log.
package logging

import (
	"bufio"
	"flag"
	"os"
	"regexp"
	"strings"

	"github.com/go-kit/log"
	"k8s.io/klog"
)

// Provided by ldflags during build
var (
	release string
	commit  string
	branch  string
)

// Level represents the logging level
type Level int

const (
	// LevelInfo is the default log level - operational messages
	LevelInfo Level = iota
	// LevelDebug includes detailed diagnostic information
	LevelDebug
)

// currentLevel holds the configured log level (set at init from env var)
var currentLevel Level = LevelInfo

// ParseLevel converts a string to a Level. Returns LevelInfo for unknown values.
func ParseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	default:
		return LevelInfo
	}
}

// String returns the string representation of the level
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	default:
		return "info"
	}
}

// GetLevel returns the current log level
func GetLevel() Level {
	return currentLevel
}

// IsDebugEnabled returns true if debug logging is enabled
func IsDebugEnabled() bool {
	return currentLevel >= LevelDebug
}

// Init returns a logger configured with common settings like
// timestamping and source code locations. Both the stdlib logger and
// glog are reconfigured to push logs into this logger.
//
// Init must be called as early as possible in main(), before any
// application-specific flag parsing or logging occurs, because it
// mutates the contents of the flag package as well as os.Stderr.
//
// The log level can be configured via the PURELB_LOG_LEVEL environment
// variable. Valid values are "info" (default) and "debug".
//
// Logging is fundamental so if something goes wrong this will
// os.Exit(1).
func Init() log.Logger {
	// Parse log level from environment
	currentLevel = ParseLevel(os.Getenv("PURELB_LOG_LEVEL"))

	l := log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	l = &filterLogger{downstream: l, level: currentLevel}

	r, w, err := os.Pipe()
	if err != nil {
		l.Log("failed to initialize logging: creating pipe for glog redirection", err)
		os.Exit(1)
	}
	klog.InitFlags(flag.NewFlagSet("klog", flag.ExitOnError))
	klog.SetOutput(w)
	go collectGlogs(r, l)

	logger := log.With(l, "caller", log.DefaultCaller)

	logger.Log("release", release, "commit", commit, "git-branch", branch, "log-level", currentLevel.String(), "msg", "Starting")

	return logger
}

// Debug logs a message at debug level. The message will only appear if
// PURELB_LOG_LEVEL=debug is set.
func Debug(logger log.Logger, keyvals ...interface{}) {
	args := append([]interface{}{"level", "debug"}, keyvals...)
	logger.Log(args...)
}

// Info logs a message at info level (always shown).
func Info(logger log.Logger, keyvals ...interface{}) {
	args := append([]interface{}{"level", "info"}, keyvals...)
	logger.Log(args...)
}

func collectGlogs(f *os.File, logger log.Logger) {
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		var buf []byte
		l, pfx, err := r.ReadLine()
		if err != nil {
			// TODO: log
			return
		}
		buf = append(buf, l...)
		for pfx {
			l, pfx, err = r.ReadLine()
			if err != nil {
				// TODO: log
				return
			}
			buf = append(buf, l...)
		}

		level, caller, msg := deformat(buf)
		logger.Log("level", level, "caller", caller, "msg", msg)
	}
}

var logPrefix = regexp.MustCompile(`^(.)(\d{2})(\d{2}) (\d{2}):(\d{2}):(\d{2}).(\d{6})\s+\d+ ([^:]+:\d+)] (.*)$`)

func deformat(b []byte) (level string, caller, msg string) {
	// Default deconstruction used when anything goes wrong.
	level = "info"
	caller = ""
	msg = string(b)

	if len(b) < 30 {
		return
	}

	ms := logPrefix.FindSubmatch(b)
	if ms == nil {
		return
	}

	switch ms[1][0] {
	case 'I':
		level = "info"
	case 'W':
		level = "warn"
	case 'E', 'F':
		level = "error"
	}

	caller = string(ms[8])
	msg = string(ms[9])

	return
}

type filterLogger struct {
	downstream log.Logger
	level      Level
}

// Log implements the gokit logging Log() function. This version:
// 1. Filters memberlist DEBUG messages (always suppressed)
// 2. Filters messages based on the configured log level
func (l *filterLogger) Log(keyvals ...interface{}) error {
	var msgLevel Level = LevelInfo

	for i, arg := range keyvals {
		str, ok := arg.(string)
		if !ok {
			continue
		}

		// Check for "level" key to determine message level
		if str == "level" && i+1 < len(keyvals) {
			if levelStr, ok := keyvals[i+1].(string); ok {
				if levelStr == "debug" {
					msgLevel = LevelDebug
				}
			}
		}

		// Look for the "msg" key - the next item will contain the message
		// from memberlist
		if str == "msg" && i+1 < len(keyvals) {
			if message, ok := keyvals[i+1].(string); ok {
				// If the message is a memberlist DEBUG message then we don't
				// want to see it
				if strings.Contains(message, "[DEBUG] memberlist: ") {
					return nil
				}
			}
		}
	}

	// Filter based on configured level
	if msgLevel > l.level {
		return nil
	}

	// Pass through to downstream logger
	return l.downstream.Log(keyvals...)
}
