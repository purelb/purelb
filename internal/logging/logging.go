// Package logging sets up structured logging in a uniform way, and
// redirects glog statements into the structured log.
package logging

import (
	"bufio"
	"flag"
	"os"
	"regexp"

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
