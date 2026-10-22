package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// sample is one scrape: series key -> value. Keys are
// "metric{label=value,...}" with labels sorted as exposed.
type sample map[string]float64

var keepAPIServer = map[string]bool{
	"apiserver_request_total":                       true,
	"apiserver_watch_events_total":                  true,
	"apiserver_watch_events_sizes_sum":              true,
	"apiserver_watch_events_sizes_count":            true,
	"apiserver_flowcontrol_rejected_requests_total": true,
	"apiserver_registered_watchers":                 true,
	"apiserver_longrunning_requests":                true,
}

var keepEtcd = map[string]bool{
	"etcd_mvcc_put_total":                             true,
	"etcd_mvcc_delete_total":                          true,
	"etcd_mvcc_txn_total":                             true,
	"etcd_debugging_mvcc_total_put_size_in_bytes":     true,
	"etcd_network_client_grpc_received_bytes_total":   true,
	"etcd_network_client_grpc_sent_bytes_total":       true,
	"etcd_mvcc_db_total_size_in_bytes":                true,
	"etcd_mvcc_db_total_size_in_use_in_bytes":         true,
	"etcd_server_proposals_committed_total":           true,
	"etcd_disk_backend_commit_duration_seconds_sum":   true,
	"etcd_disk_backend_commit_duration_seconds_count": true,
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
		v, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			continue
		}
		s[line[:sp]] += v
	}
	return s
}

func scrapeAPIServer(ctx context.Context, c *client) (sample, error) {
	raw, err := c.get(ctx, "/metrics")
	if err != nil {
		return nil, err
	}
	return parseProm(bytes.NewReader(raw), keepAPIServer), nil
}

func scrapeEtcd(ctx context.Context, url string) (sample, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseProm(resp.Body, keepEtcd), nil
}

// labels parses the label set of a series key.
func labels(key string) (string, map[string]string) {
	i := strings.IndexByte(key, '{')
	if i < 0 {
		return key, nil
	}
	name := key[:i]
	body := strings.TrimSuffix(key[i+1:], "}")
	m := map[string]string{}
	for len(body) > 0 {
		eq := strings.IndexByte(body, '=')
		if eq < 0 {
			break
		}
		k := body[:eq]
		rest := body[eq+1:]
		if len(rest) == 0 || rest[0] != '"' {
			break
		}
		// find closing quote (values here contain no escaped quotes)
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			break
		}
		m[k] = rest[1 : 1+end]
		body = rest[2+end:]
		body = strings.TrimPrefix(body, ",")
	}
	return name, m
}

var writeVerbs = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true, "DELETECOLLECTION": true, "APPLY": true}

// phaseMetrics is the delta between two scrapes.
type phaseMetrics struct {
	// Writes by "resource[/subresource] VERB" (successful and failed).
	Writes map[string]float64 `json:"writes"`
	// Requests rejected with 429 by resource.
	Rejected429 map[string]float64 `json:"rejected429"`
	// Watch events delivered (summed over every watcher) by resource.
	WatchEvents map[string]float64 `json:"watchEvents"`
	// Watch bytes delivered (summed over every watcher) by resource.
	WatchBytes map[string]float64 `json:"watchBytes"`
	// APF rejections by priority level / reason.
	APFRejected map[string]float64 `json:"apfRejected"`
	Etcd        map[string]float64 `json:"etcd"`
	EtcdEnd     map[string]float64 `json:"etcdEnd"`
}

func diff(a, b sample, ea, eb sample) phaseMetrics {
	pm := phaseMetrics{
		Writes: map[string]float64{}, Rejected429: map[string]float64{},
		WatchEvents: map[string]float64{}, WatchBytes: map[string]float64{},
		APFRejected: map[string]float64{}, Etcd: map[string]float64{}, EtcdEnd: map[string]float64{},
	}
	for k, vb := range b {
		d := vb - a[k]
		if d == 0 {
			continue
		}
		name, l := labels(k)
		res := l["resource"]
		if res == "" {
			res = l["kind"]
		}
		if l["subresource"] != "" {
			res += "/" + l["subresource"]
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
		if strings.Contains(name, "db_total_size") {
			pm.EtcdEnd[name] = vb
			continue
		}
		pm.Etcd[name] += vb - ea[k]
	}
	return pm
}

// cpuSample returns cumulative CPU seconds for the kwok control-plane
// containers (cgroup v2) and for this process.
func cpuSample(cluster string) map[string]float64 {
	out := map[string]float64{}
	for _, comp := range []string{"kube-apiserver", "etcd", "kwok-controller", "kube-controller-manager"} {
		id, err := exec.Command("docker", "inspect", "-f", "{{.Id}}", "kwok-"+cluster+"-"+comp).Output()
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/sys/fs/cgroup/system.slice/docker-" + strings.TrimSpace(string(id)) + ".scope/cpu.stat")
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(l, "usage_usec ") {
				v, _ := strconv.ParseFloat(strings.TrimPrefix(l, "usage_usec "), 64)
				out[comp] = v / 1e6
			}
		}
	}
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		out["loadgen"] = float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
	}
	return out
}

// memWatch samples memory every second -- cgroup v2 memory.current of the
// kwok control-plane containers and this process's VmRSS -- and keeps the
// peak per component since the last reset. (memory.peak is not usable: it
// is a high-water mark since the container started, and one cluster serves
// every run of a matrix row.) Each component has its own atomic, so the
// sampling goroutine and the phase code share no lock.
type memWatch struct {
	files map[string]string // component -> memory.current, or /proc/self/status
	max   map[string]*atomic.Uint64
}

func newMemWatch(ctx context.Context, cluster string) *memWatch {
	w := &memWatch{files: map[string]string{"loadgen": "/proc/self/status"}, max: map[string]*atomic.Uint64{}}
	for _, comp := range []string{"kube-apiserver", "etcd", "kwok-controller", "kube-controller-manager"} {
		id, err := exec.Command("docker", "inspect", "-f", "{{.Id}}", "kwok-"+cluster+"-"+comp).Output()
		if err != nil {
			continue
		}
		w.files[comp] = "/sys/fs/cgroup/system.slice/docker-" + strings.TrimSpace(string(id)) + ".scope/memory.current"
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
		if comp == "loadgen" {
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

// peaks returns the peak per component in MiB since the last reset.
func (w *memWatch) peaks() map[string]float64 {
	w.sample()
	out := map[string]float64{}
	for comp, m := range w.max {
		out[comp] = float64(m.Load()) / (1 << 20)
	}
	return out
}
