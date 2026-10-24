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

// Command egress-kwok measures the API load of the real egress allocator
// against a kwok control plane (Phase 3 API budget): it runs
// cmd/allocator as a separate process and simulates N node agents, then
// drives a steady phase, a rolling restart of every selected pod and a
// burst of reserve claims, recording API writes, watch traffic, etcd
// volume, CPU, memory, Lease renewal gaps and claim latency per phase.
// See README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
	"purelb.io/pkg/generated/clientset/versioned"
)

const (
	nsPureLB = "purelb-system"
	nsLoad   = "egress-load"
)

type config struct {
	Kubeconfig  string        `json:"-"`
	EtcdURL     string        `json:"-"`
	Allocator   string        `json:"-"`
	Out         string        `json:"-"`
	Mode        string        `json:"-"`
	Egress      bool          `json:"egress"`
	Nodes       int           `json:"nodes"`
	Policies    int           `json:"policies"`
	Pods        int           `json:"pods"`
	SliceSize   int           `json:"sliceSize"`
	QPS         float32       `json:"agentQPS"`
	Burst       int           `json:"agentBurst"`
	AckInterval time.Duration `json:"ackInterval"`
	DrainDelay  time.Duration `json:"drainDelay"`
	ChurnRate   float64       `json:"churnRatePodsPerSec"`
	BurstPods   int           `json:"burstPods"`
	Steady      time.Duration `json:"steady"`
	PhaseCap    time.Duration `json:"phaseCap"`
	LeaseDur    time.Duration `json:"leaseDuration"`
}

func main() {
	var c config
	flag.StringVar(&c.Mode, "mode", "run", "setup | run")
	flag.StringVar(&c.Kubeconfig, "kubeconfig", "", "admin kubeconfig of the kwok cluster")
	flag.StringVar(&c.EtcdURL, "etcd-metrics", "", "etcd /metrics URL")
	flag.StringVar(&c.Allocator, "allocator", "./allocator", "cmd/allocator binary")
	flag.StringVar(&c.Out, "out", "result.json", "result file")
	flag.BoolVar(&c.Egress, "egress", true, "create the Egress policies (false: the egress-free baseline)")
	flag.IntVar(&c.Nodes, "nodes", 500, "kwok nodes, one simulated agent each")
	flag.IntVar(&c.Policies, "policies", 250, "Egress policies, each dual-stack (2 addresses)")
	flag.IntVar(&c.Pods, "pods", 5000, "selected pods, spread over the policies")
	flag.IntVar(&c.SliceSize, "slice-size", 1024, "the ServiceGroup's sliceSize")
	var qps float64
	flag.Float64Var(&qps, "qps", 50, "agent client QPS (D-lease: the election has its own 5/10)")
	flag.IntVar(&c.Burst, "burst", 100, "agent client burst")
	flag.DurationVar(&c.AckInterval, "ack-interval", time.Second, "agent status debounce (D6)")
	flag.DurationVar(&c.DrainDelay, "drain-delay", 5*time.Second, "time a Draining slice takes to report drained")
	flag.Float64Var(&c.ChurnRate, "churn-rate", 50, "rollout replacements per second")
	flag.IntVar(&c.BurstPods, "burst-pods", 200, "pods in the claim burst")
	flag.DurationVar(&c.Steady, "steady", 60*time.Second, "steady phase length")
	flag.DurationVar(&c.PhaseCap, "phase-cap", 600*time.Second, "longest wait for quiescence after a phase")
	flag.DurationVar(&c.LeaseDur, "lease-duration", 10*time.Second, "Lease duration; renewed every half")
	flag.Parse()
	c.QPS = float32(qps)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	admin, err := clientcmd.BuildConfigFromFlags("", c.Kubeconfig)
	must(err)
	admin.QPS, admin.Burst = 500, 1000
	switch c.Mode {
	case "setup":
		must(setup(ctx, admin, c))
	case "run":
		res := run(ctx, admin, c)
		b, _ := json.MarshalIndent(res, "", "  ")
		must(os.WriteFile(c.Out, b, 0o644))
		summary(res)
	default:
		must(fmt.Errorf("unknown mode %q", c.Mode))
	}
}

func must(err error) {
	if err != nil {
		logf("fatal: %v", err)
		os.Exit(1)
	}
}

var logStart = time.Now()

func logf(format string, args ...any) {
	fmt.Printf("[%7.1fs] %s\n", time.Since(logStart).Seconds(), fmt.Sprintf(format, args...))
}

func nodeName(i int) string { return fmt.Sprintf("kwok-node-%d", i) }

func ignoreExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// parallel runs fn(i) for i in [0,n) on w workers and returns the first
// error.
func parallel(n, w int, fn func(i int) error) error {
	var wg sync.WaitGroup
	ch := make(chan int)
	var first atomic.Pointer[error]
	for k := 0; k < w; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				if err := fn(i); err != nil {
					first.CompareAndSwap(nil, &err)
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		ch <- i
	}
	close(ch)
	wg.Wait()
	if e := first.Load(); e != nil {
		return *e
	}
	return nil
}

// setup creates the namespaces, service accounts (so APF classifies the
// allocator and agents as workloads, not as exempt system:masters), the
// kwok nodes, the ServiceGroup, the policies and the selected pods.
func setup(ctx context.Context, admin *rest.Config, c config) error {
	kube := kubernetes.NewForConfigOrDie(admin)
	pl := versioned.NewForConfigOrDie(admin)
	for _, ns := range []string{nsPureLB, nsLoad} {
		_, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		if ignoreExists(err) != nil {
			return err
		}
	}
	for _, sa := range []string{"allocator", "lbnodeagent"} {
		_, err := kube.CoreV1().ServiceAccounts(nsPureLB).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa}}, metav1.CreateOptions{})
		if ignoreExists(err) != nil {
			return err
		}
		_, err = kube.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "egress-kwok-" + sa},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: nsPureLB}},
		}, metav1.CreateOptions{})
		if ignoreExists(err) != nil {
			return err
		}
	}
	err := parallel(c.Nodes, 32, func(i int) error {
		n := nodeName(i)
		rl := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64"), corev1.ResourceMemory: resource.MustParse("256Gi"),
			corev1.ResourcePods: resource.MustParse("250")}
		_, err := kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: n,
				Annotations: map[string]string{"node.alpha.kubernetes.io/ttl": "0", "kwok.x-k8s.io/node": "fake"},
				Labels:      map[string]string{"kubernetes.io/hostname": n, "kubernetes.io/os": "linux", "type": "kwok"}},
			Spec: corev1.NodeSpec{PodCIDR: fmt.Sprintf("10.%d.%d.0/24", 64+i/256, i%256),
				Taints: []corev1.Taint{{Key: "kwok.x-k8s.io/node", Value: "fake", Effect: corev1.TaintEffectNoSchedule}}},
			Status: corev1.NodeStatus{Allocatable: rl, Capacity: rl},
		}, metav1.CreateOptions{})
		return ignoreExists(err)
	})
	if err != nil {
		return err
	}

	sg := &purelbv2.ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: nsPureLB},
		Spec: purelbv2.ServiceGroupSpec{
			Local: &purelbv2.ServiceGroupLocalSpec{
				V4Pool: &purelbv2.AddressPool{Name: "public", Pool: "10.200.0.1-10.200.3.254", Subnet: "10.200.0.0/22"},
				V6Pool: &purelbv2.AddressPool{Name: "public", Pool: "fd00:200::1-fd00:200::fff", Subnet: "fd00:200::/64"},
			},
			Egress: &purelbv2.ServiceGroupEgressSpec{Allowed: true, AffinityModes: []string{"Hash"}, SliceSize: int32(c.SliceSize)},
		},
	}
	if _, err := pl.PurelbV2().ServiceGroups(nsPureLB).Create(ctx, sg, metav1.CreateOptions{}); ignoreExists(err) != nil {
		return err
	}
	if c.Egress {
		err := parallel(c.Policies, 32, func(p int) error {
			_, err := pl.PurelbV2().Egresses(nsLoad).Create(ctx, &purelbv2.Egress{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pol-%d", p)},
				Spec: purelbv2.EgressSpec{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": fmt.Sprintf("pol-%d", p)}},
					EgressPolicy: purelbv2.EgressPolicy{Destinations: purelbv2.EgressDestinations{Include: []purelbv2.EgressDestination{
						{CIDRs: []string{"0.0.0.0/0", "::/0"}}}}},
				},
			}, metav1.CreateOptions{})
			return ignoreExists(err)
		})
		if err != nil {
			return err
		}
	}
	per := c.Pods / c.Policies
	return parallel(c.Policies*per, 64, func(i int) error {
		_, err := createPod(ctx, kube, i%c.Policies, nodeName(rand.IntN(c.Nodes)))
		return err
	})
}

func createPod(ctx context.Context, kube kubernetes.Interface, p int, node string) (string, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: fmt.Sprintf("pol-%d-", p), Namespace: nsLoad,
			Labels: map[string]string{"egress": fmt.Sprintf("pol-%d", p)}},
		Spec: corev1.PodSpec{NodeName: node, TerminationGracePeriodSeconds: new(int64),
			Tolerations: []corev1.Toleration{{Key: "kwok.x-k8s.io/node", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			Containers:  []corev1.Container{{Name: "app", Image: "registry.example/egress-app:1.0"}}},
	}
	out, err := kube.CoreV1().Pods(nsLoad).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return out.Name, nil
}

// saConfig returns a config authenticating as a service account token.
func saConfig(ctx context.Context, admin *rest.Config, sa string) (*rest.Config, error) {
	kube := kubernetes.NewForConfigOrDie(admin)
	exp := int64(86400)
	tr, err := kube.CoreV1().ServiceAccounts(nsPureLB).CreateToken(ctx, sa,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &exp}}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return &rest.Config{Host: admin.Host, BearerToken: tr.Status.Token,
		TLSClientConfig: rest.TLSClientConfig{CAFile: admin.CAFile, CAData: admin.CAData}}, nil
}

// writeKubeconfig writes cfg as a kubeconfig file for the allocator.
func writeKubeconfig(cfg *rest.Config, path string) error {
	ca := cfg.CAData
	if len(ca) == 0 && cfg.CAFile != "" {
		b, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return err
		}
		ca = b
	}
	kc := clientcmdapi.NewConfig()
	kc.Clusters["kwok"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: ca}
	kc.AuthInfos["allocator"] = &clientcmdapi.AuthInfo{Token: cfg.BearerToken}
	kc.Contexts["kwok"] = &clientcmdapi.Context{Cluster: "kwok", AuthInfo: "allocator"}
	kc.CurrentContext = "kwok"
	return clientcmd.WriteToFile(*kc, path)
}

// owner is the node that "announces" an address: the simulation's stand-in
// for the Lease election's winner, spread evenly.
func owner(address string, nodes int) int {
	h := fnv.New32a()
	h.Write([]byte(address))
	return int(h.Sum32() % uint32(nodes))
}

// phaseResult is one phase's measurements.
type phaseResult struct {
	Name        string             `json:"name"`
	Seconds     float64            `json:"seconds"`
	Quiesced    bool               `json:"quiesced"`
	API         phaseMetrics       `json:"api"`
	CPUCores    map[string]float64 `json:"cpuCores"`
	MemPeakMiB  map[string]float64 `json:"memPeakMiB"`
	LeaseRenews int64              `json:"leaseRenewals"`
	LeaseLate   int64              `json:"leaseRenewalsLate"`
	LeaseMaxGap float64            `json:"leaseMaxGapSec"`
	ClaimN      int                `json:"claimN"`
	ClaimP50    float64            `json:"claimP50"`
	ClaimP99    float64            `json:"claimP99"`
	ClaimMax    float64            `json:"claimMax"`
	AgentWrites int64              `json:"agentStatusWrites"`
	AgentErrs   int64              `json:"agentWriteErrors"`
}

type runResult struct {
	Config config        `json:"config"`
	Start  time.Time     `json:"start"`
	Phases []phaseResult `json:"phases"`
}

func run(ctx context.Context, admin *rest.Config, c config) runResult {
	res := runResult{Config: c, Start: time.Now()}
	kube := kubernetes.NewForConfigOrDie(admin)
	pl := versioned.NewForConfigOrDie(admin)

	allocCfg, err := saConfig(ctx, admin, "allocator")
	must(err)
	must(writeKubeconfig(allocCfg, "allocator.kubeconfig"))
	agentCfg, err := saConfig(ctx, admin, "lbnodeagent")
	must(err)

	alloc := exec.CommandContext(ctx, c.Allocator, "-kubeconfig", "allocator.kubeconfig", "-namespace", nsPureLB, "-port", "17472")
	logFile, err := os.Create(strings.TrimSuffix(c.Out, ".json") + ".allocator.log")
	must(err)
	alloc.Stdout, alloc.Stderr = logFile, logFile
	must(alloc.Start())
	defer func() {
		_ = alloc.Process.Signal(syscall.SIGTERM)
		_ = alloc.Wait()
	}()
	logf("allocator pid %d", alloc.Process.Pid)

	sh := &shared{c: c, nodes: c.Nodes, latCh: make(chan float64, 1<<16)}
	agents := make([]*agent, c.Nodes)
	for i := range agents {
		agents[i] = newAgent(ctx, i, sh, agentCfg)
	}
	must(parallel(len(agents), 32, func(i int) error { return agents[i].start(ctx) }))
	logf("%d agents started", len(agents))

	// Bootstrap: every policy has its addresses, which their announcers
	// then watch; then the whole system settles.
	policyEEs := map[int][]string{}
	if c.Egress {
		for start := time.Now(); ; time.Sleep(2 * time.Second) {
			eps, err := pl.PurelbV2().EgressEndpointses(nsPureLB).List(ctx, metav1.ListOptions{})
			must(err)
			logf("bootstrap: %d/%d EgressEndpoints", len(eps.Items), 2*c.Policies)
			if len(eps.Items) >= 2*c.Policies {
				for _, ep := range eps.Items {
					var p int
					fmt.Sscanf(ep.Labels["purelb.io/egress-name"], "pol-%d", &p)
					policyEEs[p] = append(policyEEs[p], ep.Name)
					agents[owner(ep.Spec.Address, c.Nodes)].announce(ep.Name)
				}
				break
			}
			if time.Since(start) > c.PhaseCap {
				must(fmt.Errorf("bootstrap: addresses not allocated"))
			}
		}
	}
	sh.policyEEs = policyEEs
	logf("bootstrap: settling")
	if q := quiet(ctx, kube, sh, c.PhaseCap); !q {
		logf("bootstrap: did not quiesce")
	}

	pods, err := kube.CoreV1().Pods(nsLoad).List(ctx, metav1.ListOptions{})
	must(err)
	type podRef struct {
		name string
		p    int
	}
	var current []podRef
	used := map[[2]int]bool{} // policy, node
	for _, pod := range pods.Items {
		var p, n int
		fmt.Sscanf(pod.Labels["egress"], "pol-%d", &p)
		fmt.Sscanf(pod.Spec.NodeName, "kwok-node-%d", &n)
		current = append(current, podRef{pod.Name, p})
		used[[2]int{p, n}] = true
	}

	mw := newMemWatch(ctx, "egress", alloc.Process.Pid)
	phase := func(name string, work func()) {
		logf("phase %s: start", name)
		mw.reset()
		sh.drainLat()
		sh.leaseMaxGap.Store(0)
		a0 := mustScrape(ctx, kube)
		e0, _ := scrapeEtcd(ctx, c.EtcdURL)
		cpu0 := cpuSample("egress", alloc.Process.Pid)
		lr0, ll0, aw0, ae0 := sh.leaseRenews.Load(), sh.leaseLate.Load(), sh.statusWrites.Load(), sh.writeErrors.Load()
		t0 := time.Now()
		work()
		q := quiet(ctx, kube, sh, c.PhaseCap)
		dur := time.Since(t0).Seconds()
		a1 := mustScrape(ctx, kube)
		e1, _ := scrapeEtcd(ctx, c.EtcdURL)
		cpu1 := cpuSample("egress", alloc.Process.Pid)
		cores := map[string]float64{}
		for k, v := range cpu1 {
			cores[k] = (v - cpu0[k]) / dur
		}
		lat := sh.drainLat()
		sort.Float64s(lat)
		pr := phaseResult{Name: name, Seconds: dur, Quiesced: q, API: diff(a0, a1, e0, e1), CPUCores: cores, MemPeakMiB: mw.peaks(),
			LeaseRenews: sh.leaseRenews.Load() - lr0, LeaseLate: sh.leaseLate.Load() - ll0,
			LeaseMaxGap: time.Duration(sh.leaseMaxGap.Load()).Seconds(),
			ClaimN:      len(lat), ClaimP50: percentile(lat, .5), ClaimP99: percentile(lat, .99), ClaimMax: percentile(lat, 1),
			AgentWrites: sh.statusWrites.Load() - aw0, AgentErrs: sh.writeErrors.Load() - ae0}
		res.Phases = append(res.Phases, pr)
		logf("phase %s: done in %.1fs quiesced=%v claims n=%d p50/p99 %.1f/%.1fs leaseLate=%d maxGap=%.1fs",
			name, dur, q, pr.ClaimN, pr.ClaimP50, pr.ClaimP99, pr.LeaseLate, pr.LeaseMaxGap)
	}

	phase("steady", func() { time.Sleep(c.Steady) })

	phase("rollout", func() {
		rand.Shuffle(len(current), func(i, j int) { current[i], current[j] = current[j], current[i] })
		tick := time.NewTicker(time.Duration(float64(time.Second) / c.ChurnRate))
		defer tick.Stop()
		var wg sync.WaitGroup
		for _, old := range current {
			<-tick.C
			wg.Add(1)
			go func() {
				defer wg.Done()
				n := rand.IntN(c.Nodes)
				if _, err := createPod(ctx, kube, old.p, nodeName(n)); err != nil {
					logf("rollout create: %v", err)
					return
				}
				agents[n].cniAdd(old.p)
				if err := kube.CoreV1().Pods(nsLoad).Delete(ctx, old.name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
					logf("rollout delete: %v", err)
				}
			}()
		}
		wg.Wait()
	})

	phase("claim-burst", func() {
		var wg sync.WaitGroup
		skipped := 0
		for i := 0; i < c.BurstPods; i++ {
			p := i % c.Policies
			// a node with no pod of this policy; none left on a small cluster
			var free []int
			for n := range c.Nodes {
				if !used[[2]int{p, n}] {
					free = append(free, n)
				}
			}
			if len(free) == 0 {
				skipped++
				continue
			}
			n := free[rand.IntN(len(free))]
			used[[2]int{p, n}] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := createPod(ctx, kube, p, nodeName(n)); err != nil {
					logf("burst create: %v", err)
					return
				}
				agents[n].cniAdd(p)
			}()
		}
		if skipped > 0 {
			logf("claim-burst: %d pods skipped, their policy has a pod on every node", skipped)
		}
		wg.Wait()
	})
	return res
}

// quiet waits until no egress object has been written for 3 s and no
// claim is outstanding.
func quiet(ctx context.Context, kube kubernetes.Interface, sh *shared, limit time.Duration) bool {
	start := time.Now()
	last, stable := -1.0, 0
	for time.Since(start) < limit {
		s, err := scrapeAPIServer(ctx, kube)
		if err == nil {
			w := egressWrites(s)
			if w == last && sh.outstanding.Load() == 0 {
				stable++
				if stable >= 3 {
					return true
				}
			} else {
				stable = 0
			}
			last = w
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

func mustScrape(ctx context.Context, kube kubernetes.Interface) sample {
	s, err := scrapeAPIServer(ctx, kube)
	must(err)
	return s
}

func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	i := int(q*float64(len(xs))+0.5) - 1
	return xs[max(0, min(i, len(xs)-1))]
}

func summary(r runResult) {
	for _, p := range r.Phases {
		fmt.Printf("-- %s %.1fs quiesced=%v\n", p.Name, p.Seconds, p.Quiesced)
		keys := make([]string, 0, len(p.API.Writes))
		total := 0.0
		for k, v := range p.API.Writes {
			keys = append(keys, k)
			total += v
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("   writes %-45s %8.0f  %6.1f/s\n", k, p.API.Writes[k], p.API.Writes[k]/p.Seconds)
		}
		fmt.Printf("   writes total %.0f (%.1f/s); 429s %v; APF rejected %v\n", total, total/p.Seconds, p.API.Rejected429, p.API.APFRejected)
		wb := 0.0
		for _, v := range p.API.WatchBytes {
			wb += v
		}
		fmt.Printf("   watch bytes %.1f MiB (%.2f MiB/s); cpu %v; mem MiB %v\n", wb/(1<<20), wb/(1<<20)/p.Seconds, round(p.CPUCores), round(p.MemPeakMiB))
		fmt.Printf("   leases %d renewed, %d late, max gap %.1fs; claims n=%d p50/p99/max %.1f/%.1f/%.1fs; agent status writes %d, errors %d\n",
			p.LeaseRenews, p.LeaseLate, p.LeaseMaxGap, p.ClaimN, p.ClaimP50, p.ClaimP99, p.ClaimMax, p.AgentWrites, p.AgentErrs)
	}
}

func round(m map[string]float64) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = fmt.Sprintf("%.2f", v)
	}
	return out
}

// lease returns a node's Lease as the election writes it.
func lease(node string, dur time.Duration, now time.Time) *coordinationv1.Lease {
	secs := int32(dur.Seconds())
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: purelbv2.LeasePrefix + node, Namespace: nsPureLB},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &node, LeaseDurationSeconds: &secs,
			RenewTime: &metav1.MicroTime{Time: now}},
	}
}

// renewPatch is the Lease renewal the election sends.
func renewPatch(node string, now time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"spec": map[string]any{"holderIdentity": node,
		"renewTime": now.UTC().Format("2006-01-02T15:04:05.000000Z")}})
	return b
}
