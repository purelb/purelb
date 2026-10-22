package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// report renders results/*.json as the markdown tables used in RESULTS.md.
func report(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	var runs []runResult
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var r runResult
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool {
		a, b := runs[i].Config, runs[j].Config
		if a.Nodes != b.Nodes {
			return a.Nodes < b.Nodes
		}
		if a.Policies*a.AddrsPer != b.Policies*b.AddrsPer {
			return a.Policies*a.AddrsPer < b.Policies*b.AddrsPer
		}
		if a.QPS != b.QPS {
			return a.QPS < b.QPS
		}
		if a.Design != b.Design {
			return a.Design > b.Design // orig first
		}
		return a.AckInterval < b.AckInterval
	})

	egress := func(res string) bool {
		return strings.Contains(res, "egress") && !strings.HasPrefix(res, "egresses")
	}
	sumW := func(p phaseResult, match func(string) bool) float64 {
		t := 0.0
		for k, v := range p.API.Writes {
			if match(strings.Fields(k)[0]) {
				t += v
			}
		}
		return t / p.Seconds
	}
	sumWatch := func(p phaseResult, match func(string) bool) (float64, float64) {
		ev, by := 0.0, 0.0
		for k, v := range p.API.WatchBytes {
			if match(k) {
				by += v
				ev += p.API.WatchEvents[k]
			}
		}
		return ev / p.Seconds, by / p.Seconds
	}
	is := func(names ...string) func(string) bool {
		return func(r string) bool {
			base := strings.SplitN(r, "/", 2)[0]
			for _, n := range names {
				if base == n {
					return true
				}
			}
			return false
		}
	}
	label := func(c config) string {
		d := c.Design
		if c.Design == "decided" && c.AckInterval != 1e9 {
			d += fmt.Sprintf(" (debounce %s)", c.AckInterval)
		}
		if c.Design == "orig" && c.OrigInterval != 0 {
			d += fmt.Sprintf(" (debounce %s)", c.OrigInterval)
		}
		if c.AllocWriters > 1 {
			d += fmt.Sprintf(" (%d writers)", c.AllocWriters)
		}
		if c.AllocPriority {
			d += " (priority)"
		}
		if c.Reserve > 1 {
			d += fmt.Sprintf(" (reserve %d)", c.Reserve)
		}
		if c.AllocQPS != c.QPS && c.AllocQPS > 0 {
			d += fmt.Sprintf(" (allocator QPS %.0f)", c.AllocQPS)
		}
		if c.EPMerge > 0 {
			d += fmt.Sprintf(" (ep-merge %s)", c.EPMerge)
		}
		if c.SpecMerge > 0 {
			d += fmt.Sprintf(" (spec-merge %s)", c.SpecMerge)
		}
		if c.HostRouteFrac > 0 {
			d += fmt.Sprintf(" (host-route %.0f%% model %s, selector %d", c.HostRouteFrac*100, c.HostRouteModel, c.SelectorNodes)
			if c.SelectorNoPods {
				d += ", no pods"
			}
			d += ")"
		}
		if c.LeaseDuration != 5e9 { // runs before 2026-10-19 used 5 s
			d += fmt.Sprintf(" (lease %s)", c.LeaseDuration)
		}
		return d
	}
	sum429 := func(p phaseResult) float64 {
		t := 0.0
		for _, v := range p.API.Rejected429 {
			t += v
		}
		return t
	}
	mb := func(b float64) string {
		switch {
		case b >= 1e6:
			return fmt.Sprintf("%.1f MB", b/1e6)
		case b >= 1e3:
			return fmt.Sprintf("%.0f kB", b/1e3)
		}
		return fmt.Sprintf("%.0f B", b)
	}

	for _, ph := range []string{"rollout", "claim-burst", "steady"} {
		fmt.Printf("\n### Phase: %s\n\n", ph)
		fmt.Println("| N | addrs | QPS/Burst | design | dur s | quiesced | EgressEndpoints w/s | EgressMembers w/s | EgressNodeState w/s | Lease w/s | all writes w/s | egress watch ev/s | egress watch B/s | etcd put B/s | etcd put / 5 min | ack p50 / p99 s | claim n, p50 / p99 s | 429s | late lease renewals | CPU apiserver / etcd / loadgen | mem peak MiB apiserver / etcd / loadgen |")
		fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
		for _, r := range runs {
			c := r.Config
			for _, p := range r.Phases {
				if p.Name != ph {
					continue
				}
				wEP := sumW(p, is("origegressendpoints", "egressendpoints"))
				wM := sumW(p, is("egressmembers"))
				wNS := sumW(p, is("origegressnodestates", "egressnodestates"))
				wL := sumW(p, is("leases"))
				wAll := sumW(p, func(string) bool { return true })
				ev, by := sumWatch(p, egress)
				put := p.API.Etcd["etcd_debugging_mvcc_total_put_size_in_bytes"] / p.Seconds
				q := "yes"
				if !p.Quiesced {
					q = "**no**"
				}
				fmt.Printf("| %d | %d | %.0f/%d | %s | %.0f | %s | %.1f | %.1f | %.1f | %.0f | %.0f | %.0f | %s/s | %s/s | %s | %.2f / %.2f | %d, %.2f / %.2f | %.0f | %d | %.1f / %.1f / %.1f | %.0f / %.0f / %.0f |\n",
					c.Nodes, c.Policies*c.AddrsPer, c.QPS, c.Burst, label(c), p.Seconds, q,
					wEP, wM, wNS, wL, wAll, ev, mb(by), mb(put), mb(put*300),
					p.AckP50, p.AckP99, p.ClaimN, p.ClaimP50, p.ClaimP99, sum429(p), p.LeaseLate,
					p.CPUCores["kube-apiserver"], p.CPUCores["etcd"], p.CPUCores["loadgen"],
					p.MemPeakMiB["kube-apiserver"], p.MemPeakMiB["etcd"], p.MemPeakMiB["loadgen"])
			}
		}
	}

	fmt.Printf("\n### Object sizes (JSON as served, includes managedFields)\n\n")
	fmt.Println("| N | addrs | design | resource | count | avg bytes | max bytes |")
	fmt.Println("|---|---|---|---|---|---|---|")
	seen := map[string]bool{}
	for _, r := range runs {
		c := r.Config
		keys := make([]string, 0, len(r.Sizes))
		for k := range r.Sizes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			id := fmt.Sprintf("%d/%d/%s/%s", c.Nodes, c.Policies*c.AddrsPer, c.Design, k)
			if seen[id] {
				continue
			}
			seen[id] = true
			s := r.Sizes[k]
			fmt.Printf("| %d | %d | %s | %s | %d | %.0f | %d |\n", c.Nodes, c.Policies*c.AddrsPer, c.Design, k, s.Count, s.Avg, s.Max)
		}
	}
	return nil
}
