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

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
)

// sample is one scrape: series key ("metric{labels}") -> value.
type sample map[string]float64

var keepAPIServer = map[string]bool{
	"apiserver_request_total":                       true,
	"apiserver_watch_events_total":                  true,
	"apiserver_watch_events_sizes_sum":              true,
	"apiserver_flowcontrol_rejected_requests_total": true,
}

var keepEtcd = map[string]bool{
	"etcd_mvcc_put_total":                           true,
	"etcd_mvcc_delete_total":                        true,
	"etcd_debugging_mvcc_total_put_size_in_bytes":   true,
	"etcd_network_client_grpc_received_bytes_total": true,
	"etcd_mvcc_db_total_size_in_bytes":              true,
}

func parseProm(r io.Reader, keep map[string]bool) sample {
	s := sample{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		if !keep[name] {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(line[sp+1:], 64); err == nil {
			s[line[:sp]] += v
		}
	}
	return s
}

func scrapeAPIServer(ctx context.Context, kube kubernetes.Interface) (sample, error) {
	raw, err := kube.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	return parseProm(bytes.NewReader(raw), keepAPIServer), nil
}

func scrapeEtcd(ctx context.Context, url string) (sample, error) {
	if url == "" {
		return sample{}, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseProm(resp.Body, keepEtcd), nil
}

// labels parses a series key's label set.
func labels(key string) (string, map[string]string) {
	i := strings.IndexByte(key, '{')
	if i < 0 {
		return key, nil
	}
	name, body := key[:i], strings.TrimSuffix(key[i+1:], "}")
	m := map[string]string{}
	for len(body) > 0 {
		eq := strings.IndexByte(body, '=')
		if eq < 0 || len(body) < eq+2 || body[eq+1] != '"' {
			break
		}
		end := strings.IndexByte(body[eq+2:], '"')
		if end < 0 {
			break
		}
		m[body[:eq]] = body[eq+2 : eq+2+end]
		body = strings.TrimPrefix(body[eq+3+end:], ",")
	}
	return name, m
}

var writeVerbs = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true, "DELETECOLLECTION": true, "APPLY": true}

// resourceOf is a request series' "resource[/subresource]".
func resourceOf(l map[string]string) string {
	res := l["resource"]
	if l["subresource"] != "" {
		res += "/" + l["subresource"]
	}
	return res
}

// egressResources are the objects the egress control plane writes, apart
// from Leases (renewed all the time) and Pods (the driver's).
var egressResources = map[string]bool{
	"egressendpoints": true, "egressendpoints/status": true, "egressaddresses": true, "egressmembers": true,
	"egressnodestates": true, "egressnodestates/status": true, "egresses": true, "egresses/status": true,
	"clusteregresses": true, "clusteregresses/status": true, "nodes": true,
}

// egressWrites counts the egress control plane's writes so far.
func egressWrites(s sample) float64 {
	n := 0.0
	for k, v := range s {
		name, l := labels(k)
		if name == "apiserver_request_total" && writeVerbs[l["verb"]] && egressResources[resourceOf(l)] {
			n += v
		}
	}
	return n
}

// phaseMetrics is the difference between two scrapes.
type phaseMetrics struct {
	Writes      map[string]float64 `json:"writes"`      // "resource VERB"
	Rejected429 map[string]float64 `json:"rejected429"` // "resource VERB"
	WatchEvents map[string]float64 `json:"watchEvents"` // summed over watchers
	WatchBytes  map[string]float64 `json:"watchBytes"`  // summed over watchers
	APFRejected map[string]float64 `json:"apfRejected"`
	Etcd        map[string]float64 `json:"etcd"`
	EtcdDBBytes float64            `json:"etcdDBBytes"`
}

func diff(a, b, ea, eb sample) phaseMetrics {
	pm := phaseMetrics{Writes: map[string]float64{}, Rejected429: map[string]float64{}, WatchEvents: map[string]float64{},
		WatchBytes: map[string]float64{}, APFRejected: map[string]float64{}, Etcd: map[string]float64{}}
	for k, vb := range b {
		d := vb - a[k]
		if d == 0 {
			continue
		}
		name, l := labels(k)
		res := resourceOf(l)
		if res == "" {
			res = l["kind"]
		}
		switch name {
		case "apiserver_request_total":
			if writeVerbs[l["verb"]] {
				pm.Writes[res+" "+l["verb"]] += d
			}
			if l["code"] == "429" {
				pm.Rejected429[res+" "+l["verb"]] += d
			}
		case "apiserver_watch_events_total":
			pm.WatchEvents[res] += d
		case "apiserver_watch_events_sizes_sum":
			pm.WatchBytes[res] += d
		case "apiserver_flowcontrol_rejected_requests_total":
			pm.APFRejected[l["priority_level"]+"/"+l["reason"]] += d
		}
	}
	for k, vb := range eb {
		name, _ := labels(k)
		if name == "etcd_mvcc_db_total_size_in_bytes" {
			pm.EtcdDBBytes = vb
			continue
		}
		pm.Etcd[name] += vb - ea[k]
	}
	return pm
}

var controlPlane = []string{"kube-apiserver", "etcd", "kwok-controller", "kube-controller-manager"}

func containerCgroup(cluster, comp string) string {
	id, err := exec.Command("docker", "inspect", "-f", "{{.Id}}", "kwok-"+cluster+"-"+comp).Output()
	if err != nil {
		return ""
	}
	return "/sys/fs/cgroup/system.slice/docker-" + strings.TrimSpace(string(id)) + ".scope/"
}

// cpuSample is cumulative CPU seconds of the control plane, the allocator
// and this process.
func cpuSample(cluster string, allocPid int) map[string]float64 {
	out := map[string]float64{}
	for _, comp := range controlPlane {
		cg := containerCgroup(cluster, comp)
		raw, err := os.ReadFile(cg + "cpu.stat")
		if cg == "" || err != nil {
			continue
		}
		for _, l := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(l, "usage_usec "); ok {
				f, _ := strconv.ParseFloat(v, 64)
				out[comp] = f / 1e6
			}
		}
	}
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", allocPid)); err == nil {
		// fields after the ")" of comm: utime and stime are the 12th and 13th
		f := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+2:]))
		if len(f) > 12 {
			u, _ := strconv.ParseFloat(f[11], 64)
			s, _ := strconv.ParseFloat(f[12], 64)
			out["allocator"] = (u + s) / 100 // USER_HZ
		}
	}
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		out["loadgen"] = float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
	}
	return out
}

// memWatch samples memory every second and keeps each component's peak
// since the last reset; each component has its own atomic.
type memWatch struct {
	files map[string]string
	max   map[string]*atomic.Uint64
}

func newMemWatch(ctx context.Context, cluster string, allocPid int) *memWatch {
	w := &memWatch{files: map[string]string{"loadgen": "/proc/self/status", "allocator": fmt.Sprintf("/proc/%d/status", allocPid)},
		max: map[string]*atomic.Uint64{}}
	for _, comp := range controlPlane {
		if cg := containerCgroup(cluster, comp); cg != "" {
			w.files[comp] = cg + "memory.current"
		}
	}
	for comp := range w.files {
		w.max[comp] = new(atomic.Uint64)
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			w.sample()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return w
}

func (w *memWatch) sample() {
	for comp, f := range w.files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var v uint64
		if strings.HasSuffix(f, "/status") {
			for _, l := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(l, "VmRSS:") {
					kb, _ := strconv.ParseUint(strings.Fields(l)[1], 10, 64)
					v = kb << 10
				}
			}
		} else {
			v, _ = strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		}
		m := w.max[comp]
		for cur := m.Load(); v > cur && !m.CompareAndSwap(cur, v); cur = m.Load() {
		}
	}
}

func (w *memWatch) reset() {
	for _, m := range w.max {
		m.Store(0)
	}
}

// peaks is each component's peak in MiB since the last reset.
func (w *memWatch) peaks() map[string]float64 {
	w.sample()
	out := map[string]float64{}
	for comp, m := range w.max {
		out[comp] = float64(m.Load()) / (1 << 20)
	}
	return out
}
