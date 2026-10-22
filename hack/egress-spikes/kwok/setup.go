package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	randv2 "math/rand/v2"
	"strings"
	"sync"
	"time"
)

func nodeName(i int) string { return fmt.Sprintf("kwok-node-%d", i) }

// v6Of maps a kwok-assigned pod IPv4 to a deterministic IPv6 so endpoint
// entries carry both families, as a dual-stack cluster's would.
func v6Of(ip string) string {
	p := strings.Split(ip, ".")
	if len(p) != 4 {
		return ""
	}
	return fmt.Sprintf("fd00:10:%s:%s::%s", p[1], p[2], p[3])
}

func ignoreExists(err error) error {
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// parallel runs fn(i) for i in [0,n) on w workers.
func parallel(n, w int, fn func(i int) error) error {
	var wg sync.WaitGroup
	ch := make(chan int)
	errs := make(chan error, n)
	for k := 0; k < w; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				if err := fn(i); err != nil {
					errs <- err
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		ch <- i
	}
	close(ch)
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

// setup creates namespaces, service accounts (so simulated components are
// classified by APF like real workloads) and N kwok fake nodes.
func setup(ctx context.Context, admin *client, nodes int) error {
	for _, ns := range []string{nsPureLB, nsLoad} {
		body := fmt.Sprintf(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":%q}}`, ns)
		if _, err := admin.post(ctx, "/api/v1/namespaces", []byte(body)); ignoreExists(err) != nil {
			return err
		}
	}
	for _, sa := range []string{"allocator", "lbnodeagent"} {
		body := fmt.Sprintf(`{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":%q,"namespace":%q}}`, sa, nsPureLB)
		if _, err := admin.post(ctx, "/api/v1/namespaces/"+nsPureLB+"/serviceaccounts", []byte(body)); ignoreExists(err) != nil {
			return err
		}
		crb := fmt.Sprintf(`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"spike-%s"},
"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"cluster-admin"},
"subjects":[{"kind":"ServiceAccount","name":%q,"namespace":%q}]}`, sa, sa, nsPureLB)
		if _, err := admin.post(ctx, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", []byte(crb)); ignoreExists(err) != nil {
			return err
		}
	}
	err := parallel(nodes, 32, func(i int) error {
		n := nodeName(i)
		body := fmt.Sprintf(`{"apiVersion":"v1","kind":"Node","metadata":{"name":%q,
"annotations":{"node.alpha.kubernetes.io/ttl":"0","kwok.x-k8s.io/node":"fake"},
"labels":{"kubernetes.io/arch":"amd64","kubernetes.io/hostname":%q,"kubernetes.io/os":"linux","type":"kwok","purelb.io/egress-capable":"true"}},
"spec":{"podCIDR":"10.%d.%d.0/24","taints":[{"effect":"NoSchedule","key":"kwok.x-k8s.io/node","value":"fake"}]},
"status":{"allocatable":{"cpu":"64","memory":"256Gi","pods":"250"},"capacity":{"cpu":"64","memory":"256Gi","pods":"250"}}}`,
			n, n, 64+i/256, i%256)
		_, err := admin.post(ctx, "/api/v1/nodes", []byte(body))
		return ignoreExists(err)
	})
	if err != nil {
		return err
	}
	// wait for kwok to mark every node Ready
	for start := time.Now(); time.Since(start) < 5*time.Minute; time.Sleep(2 * time.Second) {
		items, _, err := admin.listAll(ctx, "/api/v1/nodes", map[string]string{"labelSelector": "type=kwok"})
		if err != nil {
			return err
		}
		ready := 0
		for _, it := range items {
			if strings.Contains(string(it), `"type":"Ready","status":"True"`) {
				ready++
			}
		}
		logf("nodes ready %d/%d", ready, nodes)
		if ready >= nodes {
			return nil
		}
	}
	return fmt.Errorf("nodes not ready")
}

func saToken(ctx context.Context, admin *client, sa string) (string, error) {
	raw, err := admin.post(ctx, "/api/v1/namespaces/"+nsPureLB+"/serviceaccounts/"+sa+"/token",
		[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenRequest","spec":{"expirationSeconds":86400}}`))
	if err != nil {
		return "", err
	}
	var tr struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", err
	}
	return tr.Status.Token, nil
}

type podMini struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		PodIP string `json:"podIP"`
		Phase string `json:"phase"`
	} `json:"status"`
}

func podBody(name string, p, pset int, node string) []byte {
	return []byte(fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":%q,
"labels":{"app":"egress-load","spike.purelb.io/pset":"%d","spike.purelb.io/policy-idx":"%d","egress":"pol-%d"}},
"spec":{"nodeName":%q,"terminationGracePeriodSeconds":1,
"tolerations":[{"key":"kwok.x-k8s.io/node","operator":"Exists","effect":"NoSchedule"}],
"containers":[{"name":"app","image":"registry.example/egress-app:1.0","ports":[{"containerPort":8080}]}]}}`,
		name, nsLoad, pset, p, p, node))
}

// randSuffix is called from many goroutines at once, so it uses math/rand/v2's
// top-level source, which is safe for concurrent use. 10 characters make a
// name collision across the ~10,000 pods of a run negligible: the client
// reports every 409 as a generic conflict, so a duplicate name is fatal.
func randSuffix() string {
	const a = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, 10)
	for i := range b {
		b[i] = a[randv2.IntN(len(a))]
	}
	return string(b)
}

// ensurePods makes exactly E Running pods labelled for P policies exist,
// spread uniformly at random over nodes [first, nodes).
func ensurePods(ctx context.Context, admin *client, E, P, first, nodes int, rng *rand.Rand) error {
	items, _, err := admin.listAll(ctx, "/api/v1/namespaces/"+nsLoad+"/pods", nil)
	if err != nil {
		return err
	}
	perPolicy := map[int][]string{}
	var wrong []string
	for _, it := range items {
		var pm podMini
		_ = json.Unmarshal(it, &pm)
		if pm.Metadata.DeletionTimestamp != nil {
			continue
		}
		var p, ps int
		fmt.Sscanf(pm.Metadata.Labels["spike.purelb.io/policy-idx"], "%d", &p)
		fmt.Sscanf(pm.Metadata.Labels["spike.purelb.io/pset"], "%d", &ps)
		var ni int
		fmt.Sscanf(pm.Spec.NodeName, "kwok-node-%d", &ni)
		if ps != P || ni >= nodes || ni < first {
			wrong = append(wrong, pm.Metadata.Name)
			continue
		}
		perPolicy[p] = append(perPolicy[p], pm.Metadata.Name)
	}
	var del []string
	type cr struct {
		p    int
		node string
	}
	var create []cr
	want := E / P
	del = append(del, wrong...)
	for p := 0; p < P; p++ {
		have := perPolicy[p]
		if len(have) > want {
			del = append(del, have[want:]...)
		}
		for i := len(have); i < want; i++ {
			create = append(create, cr{p, nodeName(first + rng.Intn(nodes-first))})
		}
	}
	logf("pods: deleting %d, creating %d", len(del), len(create))
	if err := parallel(len(del), 64, func(i int) error {
		var err error
		for try := 0; try < 10; try++ {
			err = admin.delete(ctx, "/api/v1/namespaces/"+nsLoad+"/pods/"+del[i], []byte(`{"gracePeriodSeconds":0}`))
			if err == nil || strings.Contains(err.Error(), "not found") {
				return nil
			}
			if !strings.Contains(err.Error(), "conflict") {
				return err
			}
			time.Sleep(200 * time.Millisecond)
		}
		return err
	}); err != nil {
		return err
	}
	if err := parallel(len(create), 64, func(i int) error {
		_, err := admin.post(ctx, "/api/v1/namespaces/"+nsLoad+"/pods",
			podBody(fmt.Sprintf("pol-%d-%s", create[i].p, randSuffix()), create[i].p, P, create[i].node))
		return ignoreExists(err)
	}); err != nil {
		return err
	}
	// wait until all pods are gone/Running with an IP
	for start := time.Now(); time.Since(start) < 5*time.Minute; time.Sleep(2 * time.Second) {
		items, _, err := admin.listAll(ctx, "/api/v1/namespaces/"+nsLoad+"/pods", nil)
		if err != nil {
			return err
		}
		running := 0
		for _, it := range items {
			var pm podMini
			_ = json.Unmarshal(it, &pm)
			if pm.Status.Phase == "Running" && pm.Status.PodIP != "" && pm.Metadata.DeletionTimestamp == nil {
				running++
			}
		}
		logf("pods running %d/%d (total objects %d)", running, want*P, len(items))
		if running == want*P && len(items) == want*P {
			return nil
		}
	}
	return fmt.Errorf("pods did not converge")
}

// cleanup removes all spike egress objects and PureLB leases.
func cleanup(ctx context.Context, admin *client) error {
	paths := []string{
		gv + "/namespaces/" + nsLoad + "/origegressendpoints",
		gv + "/origegressnodestates",
		gv + "/namespaces/" + nsPureLB + "/egressendpoints",
		gv + "/namespaces/" + nsPureLB + "/egressmembers",
		gv + "/egressnodestates",
		gv + "/namespaces/" + nsLoad + "/egresses",
	}
	for _, p := range paths {
		if err := admin.deleteCollection(ctx, p, ""); err != nil {
			return fmt.Errorf("deletecollection %s: %w", p, err)
		}
	}
	if err := admin.deleteCollection(ctx, "/apis/coordination.k8s.io/v1/namespaces/"+nsPureLB+"/leases", leaseLabel+"=1"); err != nil {
		return err
	}
	for start := time.Now(); time.Since(start) < 2*time.Minute; time.Sleep(time.Second) {
		left := 0
		for _, p := range paths {
			items, _, err := admin.listAll(ctx, p, nil)
			if err != nil {
				return err
			}
			left += len(items)
		}
		if left == 0 {
			return nil
		}
	}
	return fmt.Errorf("cleanup did not finish")
}
