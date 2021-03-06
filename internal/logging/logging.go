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

	"github.com/go-kit/kit/log"
	"k8s.io/klog"
)

// Provided by ldflags during build
var (
	release string
	commit  string
	branch  string
)

// Init returns a logger configured with common settings like
// timestamping and source code locations. Both the stdlib logger and
// glog are reconfigured to push logs into this logger.
//
// Init must be called as early as possible in main(), before any
// application-specific flag parsing or logging occurs, because it
// mutates the contents of the flag package as well as os.Stderr.
//
// Logging is fundamental so if something goes wrong this will
// os.Exit(1).
func Init() log.Logger {
	l := log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	l = &filterLogger{downstream: l}

	r, w, err := os.Pipe()
	if err != nil {
		l.Log("failed to initialize logging: creating pipe for glog redirection", err)
		os.Exit(1)
	}
	klog.InitFlags(flag.NewFlagSet("klog", flag.ExitOnError))
	klog.SetOutput(w)
	go collectGlogs(r, l)

	logger := log.With(l, "caller", log.DefaultCaller)

	logger.Log("release", release, "commit", commit, "git-branch", branch, "msg", "Starting")

	return logger
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
}

// Log implements the gokit logging Log() function. This version looks
// for memberlist DEBUG-level messages and sends them to the bit
// bucket. They're much more annoying than they are useful.
func (l *filterLogger) Log(keyvals ...interface{}) error {
	for i, arg := range keyvals {
		str, ok := arg.(string)

		// look for the "msg" key - the next item will contain the message
		// from memberlist
		if ok && str == "msg" {
			message := keyvals[i+1].(string)

			// if the message is a memberlist DEBUG message then we don't
			// want to see it
			if strings.Contains(message, "[DEBUG] memberlist: ") {
				return nil
			}
		}
	}

	// it's *not* a memberlist DEBUG message so pass it through
	return l.downstream.Log(keyvals...)
}
