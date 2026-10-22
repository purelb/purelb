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

// Command purelb-cni-spike is the Phase 0 spike 5 prototype of decisions
// D34/D35 -- NOT production code. One binary, two modes:
//
//	install     (the purelb-cni DaemonSet container) copies itself to
//	            /host/opt/cni/bin/purelb-egress-spike, then watches
//	            /host/etc/cni/net.d with inotify and keeps a
//	            {"type":"purelb-egress-spike"} entry at the end of the
//	            primary conflist's plugin chain, re-inserting it by atomic
//	            rename whenever the primary CNI rewrites the file. On SIGTERM
//	            it leaves everything in place (D26: never remove on SIGTERM).
//	            With WAR_N set it also detects a write war (item 5): a
//	            re-insert counts as a revert when the entry is gone again
//	            within WAR_REVERT (default 5s) of the installer's previous
//	            write; WAR_N reverts within WAR_WINDOW (default 60s) stop
//	            all editing, and from then on it only reports whether the
//	            chain holds the entry (put there by someone else, e.g.
//	            Cilium's cni.configMap) or not (CNIChainMissing).
//	uninstall   removes the entry, then the binary.
//	(CNI)       when kubelet's runtime invokes it as a chained plugin
//	            (CNI_COMMAND set), it passes the previous result through and
//	            appends one line per call to /var/lib/purelb/spike5.log,
//	            recording whether the persistent snapshot existed.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	pluginType = "purelb-egress-spike"
	stateDir   = "/var/lib/purelb"
	snapshot   = stateDir + "/snapshot.json"
)

func main() {
	if os.Getenv("CNI_COMMAND") != "" {
		os.Exit(cni())
	}
	if len(os.Args) < 2 {
		log.Fatal("usage: purelb-cni-spike install|uninstall")
	}
	root := os.Getenv("HOST_ROOT") // "/host" in the DaemonSet
	confDir := filepath.Join(root, "/etc/cni/net.d")
	binDir := filepath.Join(root, "/opt/cni/bin")
	switch os.Args[1] {
	case "install":
		install(root, confDir, binDir)
	case "uninstall":
		if f := primary(confDir); f != "" {
			must(edit(f, false))
		}
		_ = os.Remove(filepath.Join(binDir, pluginType))
		log.Printf("uninstalled")
	default:
		log.Fatalf("unknown mode %q", os.Args[1])
	}
}

func install(root, confDir, binDir string) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	self, err := os.Executable()
	must(err)
	must(copyAtomic(self, filepath.Join(binDir, pluginType), 0o755))
	must(os.MkdirAll(filepath.Join(root, stateDir), 0o755))
	// The persistent snapshot (D35): created once, survives reboots
	// because /var/lib is on disk, not tmpfs.
	snap := filepath.Join(root, snapshot)
	if _, err := os.Stat(snap); os.IsNotExist(err) {
		must(os.WriteFile(snap, []byte(fmt.Sprintf(`{"version":1,"written":%q}`, time.Now().UTC().Format(time.RFC3339))), 0o600))
	}
	log.Printf("installed %s; watching %s", filepath.Join(binDir, pluginType), confDir)

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	must(err)
	_, err = unix.InotifyAddWatch(fd, confDir, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_CREATE|unix.IN_DELETE)
	must(err)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	events := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := unix.Read(fd, buf); err != nil {
				log.Fatalf("inotify: %v", err)
			}
			select {
			case events <- struct{}{}:
			default:
			}
		}
	}()
	warN, _ := strconv.Atoi(os.Getenv("WAR_N"))
	warWindow, warRevert := envDur("WAR_WINDOW", 60*time.Second), envDur("WAR_REVERT", 5*time.Second)
	var lastWrite time.Time
	var reverts []time.Time
	backedOff, lastReport := false, ""
	ensure := func(why string) {
		f := primary(confDir)
		if f == "" {
			log.Printf("%s: no conflist yet", why)
			return
		}
		if backedOff {
			has, err := hasEntry(f)
			r := "CNIChainMissing: the chain lacks the entry and PureLB no longer edits it"
			if err != nil {
				r = "CNIChainMissing: " + err.Error()
			} else if has {
				r = "chain holds the entry (written by someone else); not editing"
			}
			if r != lastReport {
				log.Printf("%s: %s: %s", why, filepath.Base(f), r)
				lastReport = r
			}
			return
		}
		changed, err := editReport(f, true)
		if err != nil {
			log.Printf("%s: %s: %v", why, f, err)
			return
		}
		if !changed {
			if why == "start" {
				log.Printf("start: %s already holds the chain entry", filepath.Base(f))
			}
			return
		}
		now := time.Now()
		since := now.Sub(lastWrite)
		log.Printf("%s: re-inserted the chain entry into %s (%d ms after our previous write)", why, filepath.Base(f), since.Milliseconds())
		if warN > 0 && !lastWrite.IsZero() && since <= warRevert {
			reverts = append(reverts, now)
			for len(reverts) > 0 && now.Sub(reverts[0]) > warWindow {
				reverts = reverts[1:]
			}
			log.Printf("WAR revert %d within %v (entry removed %d ms after our write)", len(reverts), warWindow, since.Milliseconds())
			if len(reverts) >= warN {
				backedOff = true
				log.Printf("WAR detected: %d reverts within %v; stop editing %s", len(reverts), warWindow, filepath.Base(f))
			}
		}
		lastWrite = now
	}
	ensure("start")
	tick := time.NewTicker(30 * time.Second) // belt and braces for missed events
	for {
		select {
		case <-events:
			ensure("inotify")
		case <-tick.C:
			ensure("periodic")
		case s := <-sig:
			log.Printf("%v: leaving the chain entry and binary in place (D26)", s)
			return
		}
	}
}

// primary is the conflist the runtime loads: the lexically first
// *.conflist/*.conf/*.json in the directory.
func primary(dir string) string {
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".conflist") || strings.HasSuffix(n, ".conf") || strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return filepath.Join(dir, names[0])
}

func envDur(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return d
	}
	return def
}

// hasEntry reports whether the conflist's chain holds the plugin.
func hasEntry(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var conf struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return false, err
	}
	for _, p := range conf.Plugins {
		if p["type"] == pluginType {
			return true, nil
		}
	}
	return false, nil
}

func edit(path string, present bool) error {
	_, err := editReport(path, present)
	return err
}

// editReport adds (present=true) or removes the chain entry, writing by
// atomic rename. Returns whether the file changed.
func editReport(path string, present bool) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var conf map[string]any
	if err := json.Unmarshal(raw, &conf); err != nil {
		return false, err
	}
	plugins, _ := conf["plugins"].([]any)
	if plugins == nil {
		return false, fmt.Errorf("not a conflist")
	}
	var out []any
	has := false
	for _, p := range plugins {
		if m, ok := p.(map[string]any); ok && m["type"] == pluginType {
			has = true
			if !present {
				continue
			}
		}
		out = append(out, p)
	}
	if present == has {
		return false, nil
	}
	if present {
		out = append(out, map[string]any{"type": pluginType})
	}
	conf["plugins"] = out
	b, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return false, err
	}
	// The temp name must not end in .conflist/.conf/.json, or the runtime
	// could load it half-written.
	tmp := filepath.Join(filepath.Dir(path), ".purelb-tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

func copyAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".purelb-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// cni implements the chained plugin: pass prevResult through and log.
func cni() int {
	cmd := os.Getenv("CNI_COMMAND")
	raw, _ := io.ReadAll(os.Stdin)
	var conf map[string]any
	_ = json.Unmarshal(raw, &conf)
	_, snapErr := os.Stat(snapshot)
	line := fmt.Sprintf("%s %s container=%s netns=%s ifname=%s args=%s snapshot=%v\n",
		time.Now().UTC().Format(time.RFC3339Nano), cmd, short(os.Getenv("CNI_CONTAINERID")),
		os.Getenv("CNI_NETNS"), os.Getenv("CNI_IFNAME"), podName(os.Getenv("CNI_ARGS")), snapErr == nil)
	if f, err := os.OpenFile(filepath.Join(stateDir, "spike5.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	switch cmd {
	case "VERSION":
		fmt.Print(`{"cniVersion":"1.0.0","supportedVersions":["0.3.0","0.3.1","0.4.0","1.0.0","1.1.0"]}`)
	case "ADD":
		if prev, ok := conf["prevResult"]; ok {
			b, _ := json.Marshal(prev)
			fmt.Print(string(b))
		} else {
			fmt.Printf(`{"cniVersion":%q}`, conf["cniVersion"])
		}
	}
	return 0
}

func podName(args string) string {
	ns, name := "", ""
	for _, kv := range strings.Split(args, ";") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "K8S_POD_NAMESPACE":
			ns = v
		case "K8S_POD_NAME":
			name = v
		}
	}
	return ns + "/" + name
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
