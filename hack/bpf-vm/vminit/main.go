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

// vminit is the /init of the throwaway VMs hack/bpf-vm/test.sh boots to
// test the address guard's BPF code on specific kernels. It must be built
// static (CGO_ENABLED=0): it is the only program in the initramfs besides
// the test binaries.
//
// As /init it mounts the basics, loads /modules/*.ko[.zst], runs each
// line of /cmds, and powers off. A line is "<caps> <program> <args...>":
// the program runs with only the comma-separated capabilities, like
// setpriv --inh-caps=-all --bounding-set=-all,+<caps>, so the BPF verifier
// treats it as it treats lbnodeagent. Each line's output goes to the
// console, followed by "VMTEST <exit code> <line>"; "VMTEST-DONE" ends the
// run.
//
// "vminit -caps <caps> -- program args..." is the capability-dropping
// step on its own, which /init uses for each line.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var capNames = map[string]int{
	"bpf":       unix.CAP_BPF,
	"net_admin": unix.CAP_NET_ADMIN,
	"net_raw":   unix.CAP_NET_RAW,
	"sys_admin": unix.CAP_SYS_ADMIN,
	"perfmon":   unix.CAP_PERFMON,
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "-caps" {
		dropAndExec(os.Args[2], os.Args[3:])
		return
	}
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "vminit: run as /init in a VM, or as vminit -caps <caps> -- program args...")
		os.Exit(2)
	}
	runInit()
}

// dropAndExec limits the bounding set to caps, clears the inheritable set,
// and execs args. Run as root, the program then has exactly caps.
func dropAndExec(caps string, args []string) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	keep := map[int]bool{}
	for _, n := range strings.Split(caps, ",") {
		c, ok := capNames[n]
		if !ok {
			fail("unknown capability %q", n)
		}
		keep[c] = true
	}
	runtime.LockOSThread() // capabilities are per thread: exec from this one
	for c := 0; c <= unix.CAP_LAST_CAP; c++ {
		if keep[c] {
			continue
		}
		// EINVAL: a capability newer than the kernel; nothing to drop.
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && err != unix.EINVAL {
			fail("dropping capability %d: %v", c, err)
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		fail("capget: %v", err)
	}
	data[0].Inheritable, data[1].Inheritable = 0, 0
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		fail("capset: %v", err)
	}
	if err := syscall.Exec(args[0], args, os.Environ()); err != nil {
		fail("exec %s: %v", args[0], err)
	}
}

func runInit() {
	for _, m := range []struct{ src, dst, fs string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"bpffs", "/sys/fs/bpf", "bpf"},
	} {
		_ = os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.fs, 0, ""); err != nil {
			fmt.Println("vminit: mounting", m.dst+":", err)
		}
	}
	var uts unix.Utsname
	_ = unix.Uname(&uts)
	fmt.Println("vminit: kernel", unix.ByteSliceToString(uts.Release[:]))

	mods, _ := filepath.Glob("/modules/*.ko*")
	for _, p := range mods {
		flags := 0
		if !strings.HasSuffix(p, ".ko") {
			flags = unix.MODULE_INIT_COMPRESSED_FILE
		}
		f, err := os.Open(p)
		if err == nil {
			err = unix.FinitModule(int(f.Fd()), "", flags)
			_ = f.Close()
		}
		fmt.Println("vminit: module", filepath.Base(p)+":", errText(err))
	}

	if f, err := os.Open("/cmds"); err != nil {
		fmt.Println("vminit:", err)
	} else {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			argv := strings.Fields(line)
			cmd := exec.Command("/init", append([]string{"-caps", argv[0], "--"}, argv[1:]...)...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
			cmd.Dir = "/tmp"
			cmd.Env = append(os.Environ(), "HOME=/tmp", "TMPDIR=/tmp",
				"ADDRGUARD_BPF_TESTS=required", "ADDRGUARD_NETNS_TESTS=required")
			rc := 0
			if err := cmd.Run(); err != nil {
				rc = 1
				if ee, ok := err.(*exec.ExitError); ok {
					rc = ee.ExitCode()
				}
			}
			fmt.Printf("VMTEST %d %s\n", rc, line)
		}
		_ = f.Close()
	}
	fmt.Println("VMTEST-DONE")
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}

func errText(err error) string {
	if err == nil {
		return "loaded"
	}
	return err.Error()
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "vminit: "+format+"\n", a...)
	os.Exit(2)
}
