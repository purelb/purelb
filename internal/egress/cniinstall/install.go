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

// Package cniinstall is the purelb-cni installer (D34, design 10.4): the
// lbnodeagent binary in installer mode, in a DaemonSet with no API
// credential. It owns the node's CNI directories: it installs the plugin
// binary and keeps the plugin's entry in the primary conflist's chain
// while the agent's desired-state file asks for it, re-inserting it when
// the CNI rewrites the file, and reports what it finds to the agent in a
// status file. It never branches on the CNI: a CNI that keeps reverting
// the entry (a write war) is left alone and reported.
package cniinstall

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/logging"
)

// Command is the subcommand that runs the installer.
const Command = "cni-install"

// A write war (§10.4): warReverts of its writes undone within warWindow,
// each within revertWithin of the write, stop the installer editing.
const (
	revertWithin = time.Second
	warWindow    = 10 * time.Second
	warReverts   = 5
)

// Installer keeps the plugin and its chain entry in the CNI directories.
// Its state belongs to Run's goroutine.
type Installer struct {
	Logger log.Logger
	// ConfDir and BinDir are the node's CNI directories as mounted.
	ConfDir, BinDir string
	// DesiredPath is the agent's desired state, StatusPath the report.
	DesiredPath, StatusPath string
	// Self is the binary installed as the plugin.
	Self string
	now  func() time.Time

	// removing is whether the agent wants the plugin removed (D26);
	// lastWrite is when it last wrote the conflist, reverts when its
	// writes were undone; stopped once a write war is found, until the
	// agent's wish changes. retry wakes Run to look again (a removal's
	// binary waits for the entry to stay out).
	removing  bool
	lastWrite time.Time
	reverts   []time.Time
	stopped   bool
	retry     <-chan time.Time
	// reported is the status last written, at reportedAt.
	reported   cnisock.InstallerStatus
	reportedAt time.Time
	// selfSum is Self's digest; verified the installed plugin last found
	// to match it, unchanged while its file is.
	selfSum  []byte
	verified os.FileInfo
}

// Main runs the installer until SIGTERM. Its flags are the mounted
// directories.
func Main(args []string) {
	logger := logging.Init()
	fs := flag.NewFlagSet(Command, flag.ExitOnError)
	in := &Installer{Logger: logger, now: time.Now}
	fs.StringVar(&in.ConfDir, "conf-dir", "/host/cni/net.d", "the node's CNI configuration directory, as mounted")
	fs.StringVar(&in.BinDir, "bin-dir", "/host/cni/bin", "the node's CNI binary directory, as mounted")
	fs.StringVar(&in.DesiredPath, "desired", cnisock.DesiredPath, "the agent's desired state")
	fs.StringVar(&in.StatusPath, "status", cnisock.StatusPath, "the installer's status, for the agent")
	_ = fs.Parse(args)
	self, err := os.Executable()
	if err != nil {
		logging.Info(logger, "op", "cniInstall", "error", err, "msg", "can't find its own binary")
		os.Exit(1)
	}
	in.Self = self
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		logging.Info(logger, "op", "cniInstall", "signal", s.String(),
			"msg", "stopping; the plugin and its chain entry stay in place (D26)")
		close(stop)
	}()
	if err := in.Run(stop); err != nil {
		logging.Info(logger, "op", "cniInstall", "error", err)
		os.Exit(1)
	}
}

// Run checks the CNI directories at start, on every change to them or to
// the desired state, and every StatusRefresh, until stop closes.
func (in *Installer) Run(stop <-chan struct{}) error {
	if in.now == nil {
		in.now = time.Now
	}
	events, err := watch(stop, in.ConfDir, filepath.Dir(in.DesiredPath))
	if err != nil {
		return err
	}
	logging.Info(in.Logger, "op", "cniInstall", "confDir", in.ConfDir, "binDir", in.BinDir, "desired", in.DesiredPath,
		"msg", "installer started")
	in.reconcile()
	t := time.NewTicker(cnisock.StatusRefresh)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-events:
		case <-t.C:
		case <-in.retry:
		}
		in.reconcile()
	}
}

// watch sends on the returned channel when anything changes in dirs.
func watch(stop <-chan struct{}, dirs ...string) (<-chan struct{}, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify: %w", err)
	}
	for _, d := range dirs {
		if _, err := unix.InotifyAddWatch(fd, d, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_MOVED_FROM|
			unix.IN_CREATE|unix.IN_DELETE); err != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("watching %s: %w", d, err)
		}
	}
	// Non-blocking, so Close ends the pending Read.
	f := os.NewFile(uintptr(fd), "inotify")
	go func() {
		<-stop
		_ = f.Close()
	}()
	events := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			select {
			case events <- struct{}{}:
			default:
			}
		}
	}()
	return events, nil
}

// reconcile brings the CNI directories in line with the desired state
// and reports.
func (in *Installer) reconcile() {
	in.report(in.check())
}

func missing(reason, msg string) cnisock.InstallerStatus {
	return cnisock.InstallerStatus{Chain: cnisock.ChainMissing, Reason: reason, Message: msg}
}

// check makes one pass and says what it found, and for which desired
// state.
func (in *Installer) check() cnisock.InstallerStatus {
	d, err := cnisock.ReadDesired(in.DesiredPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cnisock.InstallerStatus{Chain: cnisock.ChainOff, Message: "the agent's desired state is unreadable: " + err.Error()}
	}
	if err != nil {
		// D-idle: nothing is written while the agent never asked.
		return cnisock.InstallerStatus{Chain: cnisock.ChainOff}
	}
	if removing := !d.Enabled; removing != in.removing {
		// Reverts of the other kind of write say nothing about these.
		in.removing, in.lastWrite, in.reverts, in.stopped = removing, time.Time{}, nil, false
	}
	var st cnisock.InstallerStatus
	if d.Enabled {
		st = in.install(d)
	} else {
		st = in.teardown()
	}
	st.DesiredWritten = d.Written
	return st
}

// install keeps the plugin binary and its chain entry in place.
func (in *Installer) install(d cnisock.Desired) cnisock.InstallerStatus {
	if err := in.ensureBinary(); err != nil {
		// Never chain a plugin the runtime can't find.
		return missing(cnisock.ReasonFailed, "installing the plugin binary: "+err.Error())
	}
	path, err := primary(in.ConfDir)
	if err != nil {
		return missing(cnisock.ReasonFailed, err.Error())
	}
	if path == "" {
		return missing(cnisock.ReasonNoConflist, "no CNI configuration in "+in.ConfDir+" yet")
	}
	st := in.chain(path, d.Entry)
	st.Conflist = filepath.Base(path)
	return st
}

// chain keeps entry in path's chain, unless a write war stopped it.
func (in *Installer) chain(path string, entry []byte) cnisock.InstallerStatus {
	b, err := os.ReadFile(path)
	if err != nil {
		return missing(cnisock.ReasonFailed, err.Error())
	}
	c, err := parseChain(b)
	if err != nil {
		return missing(cnisock.ReasonNotAConflist, filepath.Base(path)+": "+err.Error())
	}
	if c.find(cnisock.PluginType) >= 0 {
		// Its own, or the CNI's own configuration's (Cilium's
		// cni.configMap, §10.4), which it never rewrites.
		return cnisock.InstallerStatus{Chain: cnisock.ChainPresent}
	}
	if in.stopped {
		return missing(cnisock.ReasonWriteWar, filepath.Base(path)+
			" is rewritten without the plugin each time it is added; chain it through the CNI's own configuration")
	}
	now := in.now()
	if in.undone(path, now) {
		logging.Info(in.Logger, "op", "cniInstall", "conflist", path, "reverts", len(in.reverts), "window", warWindow.String(),
			"msg", "write war: the CNI keeps rewriting its configuration without the plugin; no longer editing it (CNIChainMissing)")
		return in.chain(path, entry)
	}
	if err := writeFile(path, c.insert(b, entry)); err != nil {
		return missing(cnisock.ReasonFailed, "writing "+filepath.Base(path)+": "+err.Error())
	}
	first := in.lastWrite.IsZero()
	in.lastWrite = now
	msg := "the plugin is chained"
	if !first {
		msg = "the plugin is chained again: the file was rewritten without it"
	}
	logging.Info(in.Logger, "op", "cniInstall", "conflist", path, "msg", msg)
	return cnisock.InstallerStatus{Chain: cnisock.ChainPresent}
}

// undone notes, at now, that the conflist is back as it was before the
// installer's last write, which the CNI undid if that write was within
// revertWithin. It reports a write war found: warReverts undone within
// warWindow stop the installer editing.
func (in *Installer) undone(path string, now time.Time) bool {
	if in.lastWrite.IsZero() || now.Sub(in.lastWrite) > revertWithin {
		return false
	}
	in.reverts = append(in.reverts, now)
	for len(in.reverts) > 0 && now.Sub(in.reverts[0]) > warWindow {
		in.reverts = in.reverts[1:]
	}
	logging.Debug(in.Logger, "op", "cniInstall", "conflist", path, "reverts", len(in.reverts),
		"msg", "the installer's last write was undone within a second")
	if len(in.reverts) < warReverts {
		return false
	}
	in.stopped = true
	return true
}

// teardown removes the plugin from the CNI directories (D26): its entry
// from the primary conflist, every other byte as the CNI wrote it, then
// the binary once the entry has stayed out past revertWithin. An entry the
// CNI puts back each time it is removed is its own configuration's
// (Cilium's cni.configMap, §10.4): the installer stops editing and keeps
// the binary that entry needs (user decision 2026-11-03). A chain naming
// the plugin always has its binary: one the CNI writes again long after
// the binary went (Cilium starting after a reboot, found on t2-cilium
// 2026-11-03) gets it back, or every pod ADD on the node fails. Anything
// left reports the chain Present.
func (in *Installer) teardown() cnisock.InstallerStatus {
	st := cnisock.InstallerStatus{Chain: cnisock.ChainPresent}
	path, err := primary(in.ConfDir)
	if err != nil {
		st.Reason, st.Message = cnisock.ReasonFailed, err.Error()
		return st
	}
	now := in.now()
	if path != "" {
		st.Conflist = filepath.Base(path)
		b, err := os.ReadFile(path)
		if err != nil {
			st.Reason, st.Message = cnisock.ReasonFailed, err.Error()
			return st
		}
		// A single-plugin .conf holds no entry of its.
		if c, err := parseChain(b); err == nil {
			if i := c.find(cnisock.PluginType); i >= 0 {
				if err := in.ensureBinary(); err != nil {
					st.Reason, st.Message = cnisock.ReasonFailed, "installing the plugin binary the chain names: "+err.Error()
					return st
				}
				if in.stopped || in.undone(path, now) {
					if in.reported.Reason != cnisock.ReasonCNIOwned {
						logging.Info(in.Logger, "op", "cniInstall", "conflist", path,
							"msg", "the CNI chains the plugin again each time it is removed: no longer editing it; the plugin binary stays")
					}
					st.Reason, st.Message = cnisock.ReasonCNIOwned, st.Conflist+
						" chains the plugin again each time it is removed: remove it from the CNI's own configuration; the plugin binary stays until then"
					return st
				}
				if err := writeFile(path, c.remove(b, i)); err != nil {
					st.Reason, st.Message = cnisock.ReasonFailed, "writing "+st.Conflist+": "+err.Error()
					return st
				}
				in.lastWrite = now
				logging.Info(in.Logger, "op", "cniInstall", "conflist", path, "msg", "the plugin is unchained")
			}
		}
	}
	if !in.lastWrite.IsZero() && now.Sub(in.lastWrite) <= revertWithin {
		// The removal may yet be undone: the binary waits.
		in.retry = time.After(revertWithin - now.Sub(in.lastWrite) + 10*time.Millisecond)
		st.Message = "unchained; the plugin binary goes once the entry stays out"
		return st
	}
	dst := filepath.Join(in.BinDir, cnisock.PluginType)
	if err := os.Remove(dst); err == nil {
		logging.Info(in.Logger, "op", "cniInstall", "binary", dst, "msg", "plugin binary removed")
	} else if !errors.Is(err, os.ErrNotExist) {
		st.Reason, st.Message = cnisock.ReasonFailed, "removing the plugin binary: "+err.Error()
		return st
	}
	in.verified = nil
	return cnisock.InstallerStatus{Chain: cnisock.ChainOff, Conflist: st.Conflist}
}

// ensureBinary installs Self as the plugin unless the installed one has
// the same content. Images have every file's time at the epoch, so only
// the content tells two builds apart; it is read again only when the
// installed file changes.
func (in *Installer) ensureBinary() error {
	dst := filepath.Join(in.BinDir, cnisock.PluginType)
	cur, err := os.Stat(dst)
	if err == nil && in.verified != nil && os.SameFile(cur, in.verified) && cur.Size() == in.verified.Size() &&
		cur.ModTime().Equal(in.verified.ModTime()) {
		return nil
	}
	if in.selfSum == nil {
		if in.selfSum, err = digest(in.Self); err != nil {
			return err
		}
	}
	if sum, err := digest(dst); err == nil && bytes.Equal(sum, in.selfSum) {
		in.verified = cur
		return nil
	}
	in.verified = nil
	r, err := os.Open(in.Self)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.CreateTemp(in.BinDir, ".purelb-egress-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after the rename, there is nothing to remove
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Chmod(0o755)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err == nil {
		logging.Info(in.Logger, "op", "cniInstall", "binary", dst, "msg", "plugin binary installed")
	}
	return err
}

// digest is the SHA-256 of the file at path.
func digest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// writeFile replaces path with b by atomic rename, keeping its mode. The
// temporary file has no CNI configuration suffix, so the runtime never
// loads it.
func writeFile(path string, b []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".purelb-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(fi.Mode().Perm())
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// report writes st for the agent when it changes, and every
// StatusRefresh so the agent knows the installer runs.
func (in *Installer) report(st cnisock.InstallerStatus) {
	now := in.now()
	changed := st.Chain != in.reported.Chain || st.Reason != in.reported.Reason ||
		st.Message != in.reported.Message || st.Conflist != in.reported.Conflist ||
		!st.DesiredWritten.Equal(in.reported.DesiredWritten)
	if !changed && now.Sub(in.reportedAt) < cnisock.StatusRefresh {
		return
	}
	if changed {
		logging.Info(in.Logger, "op", "cniInstall", "chain", st.Chain, "reason", st.Reason, "conflist", st.Conflist,
			"detail", st.Message, "msg", "chain state")
	}
	st.Written = now
	if err := cnisock.WriteStatus(in.StatusPath, st); err != nil {
		logging.Info(in.Logger, "op", "cniInstall", "status", in.StatusPath, "error", err,
			"msg", "can't write the status for the agent")
		return
	}
	in.reported, in.reportedAt = st, now
}
