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

package cniplugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"purelb.io/internal/egress/cnisock"
)

// TestMain runs the test binary as the plugin when it is called by the
// plugin's name, as lbnodeagent does (TestExec).
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == Name {
		Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// prev031 is a flannel-style 0.3.1 result: the host-side veth (no
// sandbox), the pod's eth0, and an address on each family on eth0.
const prev031 = `{"cniVersion":"0.3.1",
 "interfaces":[{"name":"cni0"},{"name":"veth1234","mac":"aa:bb:cc:dd:ee:01"},{"name":"eth0","sandbox":"/var/run/netns/cni-1"}],
 "ips":[{"version":"4","interface":2,"address":"10.244.1.7/24","gateway":"10.244.1.1"},
        {"version":"6","interface":2,"address":"fd00:244:1::7/64"},
        {"version":"4","interface":0,"address":"10.244.1.1/24"}],
 "dns":{}}`

// prev100 is a 1.0.0 result with no interfaces named.
const prev100 = `{"cniVersion":"1.0.0","ips":[{"address":"10.244.2.9/24"},{"address":"fd00:244:2::9/64"}]}`

// conf is a conflist entry of version v with prevResult prev.
func conf(t *testing.T, v, socket, prev string) []byte {
	t.Helper()
	// The snapshot and records beside the socket, never the host's.
	m := map[string]any{"cniVersion": v, "name": "cbr0", "type": Name, "socket": socket,
		"snapshot": filepath.Join(filepath.Dir(socket), "egress-snapshot.json"),
		"records":  recordDir(socket)}
	if prev != "" {
		var r map[string]any
		require.NoError(t, json.Unmarshal([]byte(prev), &r))
		m["prevResult"] = r
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

const podArgsStr = "IgnoreUnknown=1;K8S_POD_NAMESPACE=web;K8S_POD_NAME=client-0;K8S_POD_INFRA_CONTAINER_ID=c1;K8S_POD_UID=u-1"

// requests are those a fake agent received.
type requests chan cnisock.Request

// take returns the requests received so far (calls are synchronous).
func (r requests) take() []cnisock.Request {
	var got []cnisock.Request
	for {
		select {
		case req := <-r:
			got = append(got, req)
		default:
			return got
		}
	}
}

// agent is a fake agent answering every request with answer.
func agent(t *testing.T, answer cnisock.Response) (string, *requests) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "egress.sock")
	l, err := cnisock.Listen(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	reqs := make(requests, 16)
	go func() {
		_ = cnisock.Server{Min: cnisock.MinVersion, Max: cnisock.Version, Handle: func(r cnisock.Request) cnisock.Response {
			reqs <- r
			return answer
		}}.Serve(l)
	}()
	return path, &reqs
}

func TestAdd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  cnisock.Response
		v, prev string
		ips     []string
		errLike string
	}{
		{"ready 0.3.1", cnisock.Response{Result: cnisock.ResultReady}, "0.3.1", prev031, []string{"10.244.1.7", "fd00:244:1::7"}, ""},
		{"held 1.0.0", cnisock.Response{Result: cnisock.ResultHeld}, "1.0.0", prev100, []string{"10.244.2.9", "fd00:244:2::9"}, ""},
		{"pass 0.4.0", cnisock.Response{Result: cnisock.ResultPass}, "0.4.0", prev031, []string{"10.244.1.7", "fd00:244:1::7"}, ""},
		{"agent error", cnisock.Response{Error: "the pod's egress isn't in place yet; kubelet retries"}, "1.0.0", prev100, []string{"10.244.2.9", "fd00:244:2::9"}, "isn't in place yet"},
		{"unknown result", cnisock.Response{Result: "maybe"}, "1.0.0", prev100, []string{"10.244.2.9", "fd00:244:2::9"}, `answered "maybe"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, reqs := agent(t, tc.answer)
			var out, log bytes.Buffer
			p := plugin{out: &out, log: &log, now: time.Now}
			err := p.add(&skel.CmdArgs{ContainerID: "c1", Netns: "/var/run/netns/cni-1", IfName: "eth0",
				Args: podArgsStr, StdinData: conf(t, tc.v, sock, tc.prev)})
			r := reqs.take()
			require.Len(t, r, 1)
			assert.Equal(t, cnisock.Request{Version: cnisock.Version, Op: cnisock.OpAdd, ContainerID: "c1",
				Netns: "/var/run/netns/cni-1", IfName: "eth0", Namespace: "web", Name: "client-0", UID: "u-1", IPs: tc.ips}, r[0])
			if tc.errLike != "" {
				require.ErrorContains(t, err, tc.errLike)
				assert.Empty(t, out.String())
				assert.Empty(t, records(t, sock), "a refused sandbox isn't recorded")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, map[string]cnisock.Record{"c1": {Version: cnisock.RecordVersion, Namespace: "web", Name: "client-0",
				UID: "u-1", IPs: tc.ips}}, records(t, sock))
			// The primary's result, unchanged, in the conflist's version.
			var got map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &got))
			assert.Equal(t, tc.v, got["cniVersion"])
			assert.Len(t, got["ips"], len(mustMap(t, tc.prev)["ips"].([]any)))
			assert.Contains(t, log.String(), `"result":"`+tc.answer.Result+`"`)
		})
	}
}

// recordDir is where conf has the plugin record sandboxes.
func recordDir(socket string) string {
	return filepath.Join(filepath.Dir(socket), "sandboxes")
}

// records are the sandboxes recorded beside socket.
func records(t *testing.T, socket string) map[string]cnisock.Record {
	t.Helper()
	r, err := cnisock.ReadRecords(recordDir(socket))
	require.NoError(t, err)
	return r
}

func mustMap(t *testing.T, s string) map[string]any {
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &m))
	return m
}

func TestAddNoAgent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "egress.sock")
	for _, tc := range []struct {
		name, args, purelbNS string
		pass                 bool
	}{
		{"selectable pod fails", podArgsStr, "", false},
		{"kube-system passes", "K8S_POD_NAMESPACE=kube-system;K8S_POD_NAME=coredns-1", "", true},
		{"PureLB namespace passes", "K8S_POD_NAMESPACE=purelb-system;K8S_POD_NAME=allocator-1", "", true},
		{"configured PureLB namespace passes", "K8S_POD_NAMESPACE=lb;K8S_POD_NAME=allocator-1", "lb", true},
		{"default PureLB namespace is not passed when another is configured", "K8S_POD_NAMESPACE=purelb-system;K8S_POD_NAME=x", "lb", false},
		{"no pod passes", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMap(t, string(conf(t, "1.0.0", missing, prev100)))
			if tc.purelbNS != "" {
				m["purelbNamespace"] = tc.purelbNS
			}
			stdin, _ := json.Marshal(m)
			var out, log bytes.Buffer
			err := plugin{out: &out, log: &log, now: time.Now}.add(&skel.CmdArgs{ContainerID: "c1", Netns: "/x", IfName: "eth0", Args: tc.args, StdinData: stdin})
			if !tc.pass {
				require.ErrorContains(t, err, "isn't running on this node")
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out.String(), "10.244.2.9/24")
		})
	}
}

func TestAddFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "egress.sock")
	now := time.Unix(1700000000, 0)
	snap := cnisock.Snapshot{Version: cnisock.SnapshotVersion, Written: now.Add(-10 * time.Minute), MaxAgeSeconds: 3600,
		FailClosedNamespaces: []string{"web"},
		Pods: []cnisock.SnapshotPod{
			{Namespace: "web", Name: "client-0", UID: "u-1", FailClosed: true},
			{Namespace: "web", Name: "plain", UID: "u-2"},
			{Namespace: "db", Name: "client-0", UID: "u-3", FailClosed: true},
			{Namespace: "batch", Name: "job", UID: "u-4"},
		}}
	for _, tc := range []struct {
		name    string
		edit    func(*cnisock.Snapshot) // nil: no snapshot file
		args    string
		pass    bool
		because string
	}{
		{"no snapshot", nil, podArgsStr, false, "no usable snapshot"},
		{"FailClosed pod", func(*cnisock.Snapshot) {}, podArgsStr, false, "a FailClosed policy selects the pod"},
		{"pod no FailClosed policy selects", func(*cnisock.Snapshot) {}, "K8S_POD_NAMESPACE=web;K8S_POD_NAME=plain;K8S_POD_UID=u-2", true, "which no FailClosed policy selects"},
		{"absent, FailClosed namespace", func(*cnisock.Snapshot) {}, "K8S_POD_NAMESPACE=web;K8S_POD_NAME=new;K8S_POD_UID=u-9", false, "whose namespace a FailClosed policy covers"},
		{"absent, other namespace", func(*cnisock.Snapshot) {}, "K8S_POD_NAMESPACE=batch;K8S_POD_NAME=new;K8S_POD_UID=u-9", true, "no FailClosed policy covers its namespace"},
		{"same name, another pod (UID)", func(*cnisock.Snapshot) {}, "K8S_POD_NAMESPACE=batch;K8S_POD_NAME=job;K8S_POD_UID=u-other", true, "doesn't have the pod"},
		{"same name, another pod, FailClosed namespace", func(*cnisock.Snapshot) {}, "K8S_POD_NAMESPACE=web;K8S_POD_NAME=plain;K8S_POD_UID=u-other", false, "whose namespace a FailClosed policy covers"},
		{"no UID from the runtime", func(*cnisock.Snapshot) {}, "K8S_POD_NAMESPACE=db;K8S_POD_NAME=client-0", false, "a FailClosed policy selects the pod"},
		{"past its bound", func(s *cnisock.Snapshot) { s.Written = now.Add(-61 * time.Minute) }, podArgsStr, true, "past its 1h0m0s bound"},
		{"at its bound", func(s *cnisock.Snapshot) { s.Written = now.Add(-60 * time.Minute) }, podArgsStr, false, "a FailClosed policy selects the pod"},
		{"configured bound", func(s *cnisock.Snapshot) { s.MaxAgeSeconds = 300 }, podArgsStr, true, "past its 5m0s bound"},
		{"written ahead of the clock", func(s *cnisock.Snapshot) { s.Written = now.Add(time.Hour) }, podArgsStr, false, "a FailClosed policy selects the pod"},
		{"newer version", func(s *cnisock.Snapshot) { s.Version = cnisock.SnapshotVersion + 1 }, podArgsStr, false, "no usable snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "egress-snapshot.json")
			if tc.edit != nil {
				s := snap
				tc.edit(&s)
				require.NoError(t, cnisock.WriteSnapshot(path, s))
			}
			m := mustMap(t, string(conf(t, "1.0.0", missing, prev100)))
			m["snapshot"] = path
			m["records"] = filepath.Join(filepath.Dir(path), "sandboxes")
			stdin, _ := json.Marshal(m)
			var out, log bytes.Buffer
			p := plugin{out: &out, log: &log, now: func() time.Time { return now }}
			err := p.add(&skel.CmdArgs{ContainerID: "c1", Netns: "/x", IfName: "eth0", Args: tc.args, StdinData: stdin})
			assert.Contains(t, log.String(), tc.because)
			recorded := records(t, path)
			if !tc.pass {
				require.ErrorContains(t, err, "isn't running on this node, and ")
				require.ErrorContains(t, err, tc.because)
				assert.Empty(t, out.String())
				assert.Empty(t, recorded)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out.String(), "10.244.2.9/24")
			// With no agent to tell, the record is what it learns from.
			assert.Contains(t, recorded, "c1")
		})
	}
}

// A probe that can't be sent (no such namespace here) is logged and not
// reported; the pod is admitted all the same.
func TestAddProbeUnsent(t *testing.T) {
	sock, reqs := agent(t, cnisock.Response{Result: cnisock.ResultReady, Probe: true})
	var out, log bytes.Buffer
	err := plugin{out: &out, log: &log, now: time.Now}.add(&skel.CmdArgs{ContainerID: "c1", Netns: "/nonexistent/netns",
		IfName: "eth0", Args: podArgsStr, StdinData: conf(t, "1.0.0", sock, prev100)})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "10.244.2.9/24")
	assert.Contains(t, log.String(), "the stage probe wasn't sent")
	r := reqs.take()
	require.Len(t, r, 1, "the ADD, no report")
}

func TestAddSystemAsksAgent(t *testing.T) {
	// A system pod is recorded by the agent, and passes whatever it says.
	sock, reqs := agent(t, cnisock.Response{Error: "anything"})
	var out, log bytes.Buffer
	err := plugin{out: &out, log: &log, now: time.Now}.add(&skel.CmdArgs{ContainerID: "c2", Netns: "/x", IfName: "eth0",
		Args: "K8S_POD_NAMESPACE=kube-system;K8S_POD_NAME=coredns-1", StdinData: conf(t, "1.0.0", sock, prev100)})
	require.NoError(t, err)
	r := reqs.take()
	require.Len(t, r, 1)
	assert.Equal(t, "kube-system", r[0].Namespace)
	assert.Equal(t, "coredns-1", records(t, sock)["c2"].Name, "recorded, as every sandbox let start")
}

func TestAddNoPrevResult(t *testing.T) {
	sock, reqs := agent(t, cnisock.Response{Result: cnisock.ResultReady})
	var out, log bytes.Buffer
	err := plugin{out: &out, log: &log, now: time.Now}.add(&skel.CmdArgs{ContainerID: "c1", Netns: "/x", IfName: "eth0",
		Args: podArgsStr, StdinData: conf(t, "1.0.0", sock, "")})
	require.ErrorContains(t, err, "must follow the primary CNI plugin")
	assert.Empty(t, reqs.take())
	assert.Empty(t, records(t, sock))
}

func TestDel(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "egress.sock")
	for _, tc := range []struct {
		name   string
		sock   func(t *testing.T) (string, *requests)
		stdin  func(sock string) []byte
		logged string
	}{
		{"agent told", func(t *testing.T) (string, *requests) { return agent(t, cnisock.Response{Result: cnisock.ResultReady}) },
			func(s string) []byte { return conf(t, "0.3.1", s, prev031) }, "pod removed"},
		{"agent error", func(t *testing.T) (string, *requests) { return agent(t, cnisock.Response{Error: "boom"}) },
			func(s string) []byte { return conf(t, "1.0.0", s, prev100) }, "wasn't told"},
		{"no agent", func(t *testing.T) (string, *requests) { return missing, nil },
			func(s string) []byte { return conf(t, "1.0.0", s, prev100) }, "wasn't told"},
		{"no prevResult", func(t *testing.T) (string, *requests) { return agent(t, cnisock.Response{Result: cnisock.ResultReady}) },
			func(s string) []byte { return conf(t, "1.0.0", s, "") }, "pod removed"},
		{"bad configuration", func(t *testing.T) (string, *requests) { return missing, nil },
			func(string) []byte { return []byte("{") }, "configuration unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, reqs := tc.sock(t)
			require.NoError(t, cnisock.WriteRecord(recordDir(sock), "c1", cnisock.Record{Namespace: "web", Name: "client-0"}))
			require.NoError(t, cnisock.WriteRecord(recordDir(sock), "c9", cnisock.Record{Namespace: "web", Name: "client-9"}))
			defer os.RemoveAll(recordDir(sock))
			var out, log bytes.Buffer
			err := plugin{out: &out, log: &log, now: time.Now}.del(&skel.CmdArgs{ContainerID: "c1", IfName: "eth0", Args: podArgsStr, StdinData: tc.stdin(sock)})
			require.NoError(t, err, "DEL always succeeds")
			assert.Empty(t, out.String())
			assert.Contains(t, log.String(), tc.logged)
			if tc.name == "bad configuration" {
				assert.Len(t, records(t, sock), 2, "no configuration: no record directory to find")
			} else {
				assert.Equal(t, []string{"c9"}, slices.Collect(maps.Keys(records(t, sock))), "its record removed, whether or not the agent heard")
			}
			if reqs != nil {
				r := reqs.take()
				require.Len(t, r, 1)
				assert.Equal(t, cnisock.OpDel, r[0].Op)
				assert.Equal(t, "client-0", r[0].Name)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "egress.sock")
	var out, log bytes.Buffer
	p := plugin{out: &out, log: &log, now: time.Now}
	args := func(sock string) *skel.CmdArgs {
		return &skel.CmdArgs{ContainerID: "c1", Netns: "/x", IfName: "eth0", Args: podArgsStr, StdinData: conf(t, "1.0.0", sock, prev100)}
	}
	require.NoError(t, p.check(args(missing)), "no agent: nothing to check against")

	sock, reqs := agent(t, cnisock.Response{Result: cnisock.ResultReady})
	require.NoError(t, p.check(args(sock)))
	r := reqs.take()
	require.Len(t, r, 1)
	assert.Equal(t, cnisock.OpCheck, r[0].Op)

	sock, _ = agent(t, cnisock.Response{Error: "the agent has no record of this sandbox"})
	require.ErrorContains(t, p.check(args(sock)), "no record")
}

func TestPodArgs(t *testing.T) {
	assert.Equal(t, map[string]string{"IgnoreUnknown": "1", "K8S_POD_NAMESPACE": "web", "K8S_POD_NAME": "a", "NEW_KEY": "x=y"},
		podArgs("IgnoreUnknown=1;K8S_POD_NAMESPACE=web;K8S_POD_NAME=a;garbage;NEW_KEY=x=y"))
}

// TestExec runs the plugin as the runtime does: a binary named
// purelb-egress, the command in the environment, the configuration on
// stdin, the result or a CNI error on stdout.
func TestExec(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	bin := filepath.Join(t.TempDir(), Name)
	require.NoError(t, os.Symlink(self, bin))
	sock, reqs := agent(t, cnisock.Response{Result: cnisock.ResultReady})
	missing := filepath.Join(t.TempDir(), "egress.sock")

	run := func(cmd string, stdin []byte) (string, error) {
		c := exec.Command(bin)
		c.Env = []string{"CNI_COMMAND=" + cmd, "CNI_CONTAINERID=c1", "CNI_NETNS=/var/run/netns/gone",
			"CNI_IFNAME=eth0", "CNI_PATH=/opt/cni/bin", "CNI_ARGS=" + podArgsStr}
		c.Stdin = bytes.NewReader(stdin)
		var out bytes.Buffer
		c.Stdout = &out
		err := c.Run()
		return out.String(), err
	}

	out, err := run("VERSION", conf(t, "1.0.0", sock, ""))
	require.NoError(t, err)
	assert.Contains(t, out, `"supportedVersions":["0.3.0","0.3.1","0.4.0","1.0.0","1.1.0"]`)

	out, err = run("ADD", conf(t, "0.3.1", sock, prev031))
	require.NoError(t, err, out)
	assert.Contains(t, out, `"cniVersion": "0.3.1"`)
	assert.Contains(t, out, "10.244.1.7/24")
	require.Len(t, reqs.take(), 1)

	// A failed ADD is a CNI error on stdout and a non-zero exit.
	out, err = run("ADD", conf(t, "1.0.0", missing, prev100))
	require.Error(t, err)
	var cniErr struct {
		Code uint   `json:"code"`
		Msg  string `json:"msg"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &cniErr), out)
	assert.Contains(t, cniErr.Msg, "isn't running on this node")

	// With the agent's snapshot, which no FailClosed policy is in, it passes.
	require.NoError(t, cnisock.WriteSnapshot(filepath.Join(filepath.Dir(missing), "egress-snapshot.json"),
		cnisock.Snapshot{Version: cnisock.SnapshotVersion, Written: time.Now(), MaxAgeSeconds: 3600}))
	out, err = run("ADD", conf(t, "1.0.0", missing, prev100))
	require.NoError(t, err, out)
	assert.Contains(t, out, "10.244.2.9/24")

	out, err = run("DEL", conf(t, "1.0.0", missing, prev100))
	require.NoError(t, err, out)

	for _, cmd := range []string{"STATUS", "GC"} {
		out, err = run(cmd, conf(t, "1.1.0", missing, ""))
		require.NoError(t, err, fmt.Sprintf("%s: %s", cmd, out))
	}
}
