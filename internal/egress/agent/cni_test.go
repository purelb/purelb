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

package agent

import (
	"context"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"purelb.io/internal/egress"
	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/egress/datapath"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// An ADD's verdict (§8.1), from the pod, the plan and the devices the
// datapath tagged.
func TestCNIDecide(t *testing.T) {
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	pod := pnPod("voice", "a", appA)
	pod.UID = "u-a"
	require.NoError(t, pods.Add(pod))
	s := &session{pods: pods}
	ip := netip.MustParseAddr("10.1.0.5")
	w := &waiter{namespace: "voice", name: "a", uid: "u-a", ips: []netip.Addr{ip}}
	pn := &podNode{pods: map[string]podState{}, failClosedNS: map[string]bool{}}
	capable := &purelbv2.EgressCapability{EgressCapable: true}
	tagged := func(set uint8, hold bool) map[netip.Addr]datapath.PodDev {
		return map[netip.Addr]datapath.PodDev{ip: {V4: ip, PodSet: set, Hold: hold}}
	}
	verdict := func(c *purelbv2.EgressCapability, tg map[netip.Addr]datapath.PodDev) *cniVerdict {
		return cniDecide(s, pn, c, tg, w)
	}
	ready := &cniVerdict{result: cnisock.ResultReady, final: true}

	assert.Equal(t, ready, verdict(nil, nil), "planned, not selected: nothing to wait for")
	pn.pods["voice/a"] = podState{ips: w.ips, podSet: 3, selected: true}
	assert.Nil(t, verdict(nil, nil), "the first probe is still running")
	assert.Nil(t, verdict(capable, nil), "its device isn't tagged yet")
	assert.Nil(t, verdict(capable, tagged(4, false)), "tagged with another pod-set")
	assert.Equal(t, ready, verdict(capable, tagged(3, false)))

	pn.pods["voice/a"] = podState{ips: w.ips, podSet: 3, selected: true, pending: true}
	assert.Equal(t, &cniVerdict{result: cnisock.ResultPass}, verdict(capable, tagged(3, false)),
		"FailOpen, slot not ready: passes at the hold's end unless it gets ready")
	pn.pods["voice/a"] = podState{ips: w.ips, podSet: 3, selected: true, pending: true, hold: true}
	assert.Nil(t, verdict(capable, tagged(3, false)), "the hold isn't on the device yet")
	assert.Equal(t, &cniVerdict{result: cnisock.ResultHeld}, verdict(capable, tagged(3, true)))
	pn.pods["voice/a"] = podState{ips: w.ips, selected: true, hold: true}
	assert.Equal(t, &cniVerdict{result: cnisock.ResultHeld, final: true}, verdict(capable, tagged(0, true)),
		"no pod-set (budget exhausted): held for good")
	assert.Equal(t, &cniVerdict{err: "this node can't run egress, and a FailClosed policy selects the pod (D29)", final: true},
		verdict(&purelbv2.EgressCapability{}, nil))
	pn.pods["voice/a"] = podState{ips: w.ips, podSet: 3, selected: true, pending: true}
	assert.Equal(t, &cniVerdict{result: cnisock.ResultPass, final: true}, verdict(&purelbv2.EgressCapability{}, nil))

	// Not seen yet (or another pod of the name): passes, unless a
	// FailClosed policy covers its namespace.
	other := &waiter{namespace: "voice", name: "a", uid: "u-other", ips: w.ips}
	assert.Equal(t, &cniVerdict{result: cnisock.ResultPass, final: true}, cniDecide(s, pn, capable, nil, other))
	pn.failClosedNS["voice"] = true
	assert.Nil(t, cniDecide(s, pn, capable, nil, other))
}

// Sandboxes reach the plan by their pod's UID; a deleted one ends its pod
// until another starts, and is forgotten once the pod is gone or has
// other addresses.
func TestSandboxInput(t *testing.T) {
	a := New(log.NewNopLogger(), testNode, "purelb-system")
	addr := func(s string) []netip.Addr { return []netip.Addr{netip.MustParseAddr(s)} }
	withUID := func(p *corev1.Pod, uid string) *corev1.Pod { p.UID = types.UID(uid); return p }
	running := withUID(pnPod("ns", "running", nil, "10.0.0.1"), "u-running")
	starting := withUID(pnPod("ns", "starting", nil), "u-starting")
	stopped := withUID(pnPod("ns", "stopped", nil, "10.0.0.3"), "u-stopped")
	restarted := withUID(pnPod("ns", "restarted", nil, "10.0.0.4"), "u-restarted")
	moved := withUID(pnPod("ns", "moved", nil, "10.0.0.9"), "u-moved")
	a.sandboxes.Store("c-starting", &cniSandbox{namespace: "ns", name: "starting", uid: "u-starting", ips: addr("10.0.0.2")})
	a.sandboxes.Store("c-stopped", &cniSandbox{namespace: "ns", name: "stopped", uid: "u-stopped", ips: addr("10.0.0.3"), deleted: true})
	a.sandboxes.Store("c-restarted-old", &cniSandbox{namespace: "ns", name: "restarted", uid: "u-restarted", ips: addr("10.0.0.4"), deleted: true})
	a.sandboxes.Store("c-restarted-new", &cniSandbox{namespace: "ns", name: "restarted", uid: "u-restarted", ips: addr("10.0.0.5")})
	a.sandboxes.Store("c-moved", &cniSandbox{namespace: "ns", name: "moved", uid: "u-moved", ips: addr("10.0.0.8"), deleted: true})
	a.sandboxes.Store("c-gone", &cniSandbox{namespace: "ns", name: "gone", uid: "u-gone", ips: addr("10.0.0.6"), deleted: true})
	a.sandboxes.Store("c-early", &cniSandbox{namespace: "ns", name: "running", uid: "u-recreated", ips: addr("10.0.0.7")})
	a.sandboxes.Store("c-nouid", &cniSandbox{namespace: "ns", name: "running", ips: addr("10.0.0.1")})

	ips, ended := a.sandboxInput([]*corev1.Pod{running, starting, stopped, restarted, moved})
	assert.Equal(t, map[string][]netip.Addr{"ns/starting": addr("10.0.0.2"), "ns/restarted": addr("10.0.0.5"),
		"ns/running": addr("10.0.0.1")}, ips, "a live sandbox's addresses; without a UID matched by name")
	assert.Equal(t, map[string]bool{"ns/stopped": true}, ended)
	var left []string
	a.sandboxes.Range(func(k, _ any) bool { left = append(left, k.(string)); return true })
	assert.ElementsMatch(t, []string{"c-starting", "c-stopped", "c-restarted-old", "c-restarted-new", "c-early", "c-nouid"}, left,
		"forgotten: the deleted sandbox of a pod gone, and one whose pod has other addresses now")
}

// The plugin's requests through the socket (§8.1): an ADD waits for a
// selected pod's slot and device, is held when its FailClosed slice isn't
// ready, claims a slice the node lacks; a DEL takes the pod out; CHECK
// knows its sandboxes; the socket goes with egress.
func TestCNISocket(t *testing.T) {
	const ep = "e-voice.sip-0a0b0c0d"
	ens := &purelbv2.EgressNodeState{ObjectMeta: metav1.ObjectMeta{Name: testNode},
		Spec: purelbv2.EgressNodeStateSpec{Slices: []purelbv2.EgressNodeSlice{
			{Endpoints: ep, Address: "192.0.2.1", Index: 2, State: "Active", SliceSize: 4096}}}}
	labels := func(name string) map[string]string {
		return map[string]string{egress.LabelKind: egress.KindEgress, egress.LabelNamespace: "voice", egress.LabelName: name}
	}
	ea := &purelbv2.EgressAddress{ObjectMeta: metav1.ObjectMeta{Namespace: "purelb-system", Name: ep, Labels: labels("sip")},
		Spec: purelbv2.EgressAddressSpec{State: "Active", Address: "192.0.2.1", Family: "IPv4", ServiceGroup: "default",
			PoolType: "Local", Placement: "SingleAnnouncer"}}
	eg := &purelbv2.Egress{ObjectMeta: metav1.ObjectMeta{Namespace: "voice", Name: "sip", UID: "uid-sip"},
		Spec: purelbv2.EgressSpec{PodSelector: &metav1.LabelSelector{MatchLabels: appA}}}
	pod := pnPod("voice", "a", appA) // its addresses aren't in the API yet
	pod.UID = "u-a"
	other := pnPod("web", "b", nil)
	other.UID = "u-b"
	f := newFixture(t, true, testNodeObj, ens, ea, eg, pod, other)
	f.a.cniHold = 300 * time.Millisecond
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: true}))
	cl := cnisock.NewClient(f.a.sockPath)
	call := func(req cnisock.Request) cnisock.Response {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var resp cnisock.Response
		require.Eventually(t, func() bool {
			var err error
			resp, err = cl.Call(ctx, req)
			return err == nil // the socket opens with the session
		}, 5*time.Second, 20*time.Millisecond)
		return resp
	}
	add := cnisock.Request{Op: cnisock.OpAdd, ContainerID: "c-a", Namespace: "voice", Name: "a", UID: "u-a", IPs: []string{"10.1.0.5"}}

	// Selected, its slice Active: ready once its device carries its
	// pod-set, the address from ADD.
	var first cnisock.Response
	require.Eventually(t, func() bool { first = call(add); return first.Result == cnisock.ResultReady }, 5*time.Second, 50*time.Millisecond)
	assert.True(t, first.Probe, "the first selected pod after the start sends the stage probe")

	// The snapshot the plugin decides by without the agent (D35).
	var snap cnisock.Snapshot
	require.Eventually(t, func() bool {
		var err error
		snap, err = cnisock.ReadSnapshot(f.a.snapPath)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, []cnisock.SnapshotPod{{Namespace: "voice", Name: "a", UID: "u-a", FailClosed: true},
		{Namespace: "web", Name: "b", UID: "u-b"}}, snap.Pods)
	assert.Equal(t, []string{"voice"}, snap.FailClosedNamespaces)
	assert.Equal(t, int64(3600), snap.MaxAgeSeconds)
	assert.WithinDuration(t, time.Now(), snap.Written, 5*time.Second)
	assert.Greater(t, testutil.ToFloat64(snapshotAge), 0.0, "the session's writer reports its age")
	// With a snapshot written, purelb-cni is asked to chain the plugin.
	d, err := cnisock.ReadDesired(f.a.desiredPath)
	require.NoError(t, err)
	assert.True(t, d.Enabled)
	assert.JSONEq(t, `{"type":"purelb-egress","purelbNamespace":"purelb-system","logLevel":"info"}`, string(d.Entry))

	// The installer's findings reach the node's conditions (§10.4).
	eventually(t, "True "+condDatapathCompatible, func() string { return f.condition(condDatapathCompatible) }, "chained")
	require.NoError(t, cnisock.WriteStatus(f.a.statusPath, cnisock.InstallerStatus{Written: time.Now(), Chain: cnisock.ChainMissing,
		Reason: cnisock.ReasonWriteWar, Conflist: "05-cilium.conflist", Message: "rewritten without the plugin"}))
	eventually(t, "False CNIChainMissing", func() string { return f.condition(condDatapathCompatible) }, "write war")
	require.NoError(t, os.Remove(f.a.statusPath))
	eventually(t, "False ChainMissing", func() string { return f.condition(condDatapathCompatible) }, "no installer")
	require.NoError(t, cnisock.WriteStatus(f.a.statusPath, cnisock.InstallerStatus{Written: time.Now(), Chain: cnisock.ChainPresent}))
	eventually(t, "True "+condDatapathCompatible, func() string { return f.condition(condDatapathCompatible) }, "chained again")
	assert.Equal(t, cnisock.ResultReady, call(cnisock.Request{Op: cnisock.OpCheck, ContainerID: "c-a", Namespace: "voice", Name: "a"}).Result)
	assert.Contains(t, call(cnisock.Request{Op: cnisock.OpCheck, ContainerID: "c-x", Namespace: "voice", Name: "a"}).Error, "no record")
	unsel := call(cnisock.Request{Op: cnisock.OpAdd, ContainerID: "c-b", Namespace: "web", Name: "b", UID: "u-b", IPs: []string{"10.1.0.6"}})
	assert.Equal(t, cnisock.ResultReady, unsel.Result, "not selected")
	assert.False(t, unsel.Probe)
	assert.Equal(t, cnisock.ResultPass, call(cnisock.Request{Op: cnisock.OpAdd, ContainerID: "c-k", Namespace: "kube-system",
		Name: "dns", UID: "u-k", IPs: []string{"10.1.0.7"}}).Result, "system namespaces are never held")
	assert.Contains(t, call(cnisock.Request{Op: cnisock.OpAdd, ContainerID: "c-z", Namespace: "voice", Name: "z",
		IPs: []string{"nope"}}).Error, "bad pod address")
	assert.Equal(t, cnisock.ResultPass, call(cnisock.Request{Op: cnisock.OpAdd, ContainerID: "c-u", Namespace: "web", Name: "unknown",
		UID: "u-u", IPs: []string{"10.1.0.8"}}).Result, "not known, and no FailClosed policy covers its namespace")

	// The stage probe's report (design 6.1): only from the sandbox asked.
	// Classified by mark from then on, and reported.
	assert.Contains(t, call(cnisock.Request{Op: cnisock.OpProbe, ContainerID: "c-b", Namespace: "web", Name: "b",
		IPs: []string{"10.1.0.6"}}).Error, "asked no stage probe")
	assert.Equal(t, cnisock.ResultReady, call(cnisock.Request{Op: cnisock.OpProbe, ContainerID: "c-a", Namespace: "voice", Name: "a",
		UID: "u-a", IPs: []string{"10.1.0.5"}}).Result)
	f.until(t, func(w dpWant) bool { return w.stage != nil && !w.stage.SourceIP })
	eventually(t, "Netfilter", func() string {
		if ns := f.nodeState(); ns != nil && ns.Status.Capability != nil {
			return ns.Status.Capability.PodNodeStage
		}
		return ""
	}, "the stage reported")
	assert.Contains(t, call(cnisock.Request{Op: cnisock.OpProbe, ContainerID: "c-a", Namespace: "voice", Name: "a",
		IPs: []string{"10.1.0.5"}}).Error, "asked no stage probe", "once per start")

	// Sandboxes the socket wasn't told of (design 8.1): one the plugin
	// recorded while no agent answered isn't reported; one without a
	// record is, by a Warning event on the pod, and counted.
	require.NoError(t, cnisock.WriteRecord(f.a.recordDir, "c-d", cnisock.Record{Namespace: "web", Name: "d", UID: "u-d",
		IPs: []string{"10.1.0.21"}}))
	for name, ip := range map[string]string{"c": "10.1.0.20", "d": "10.1.0.21"} {
		p := pnPod("web", name, nil, ip)
		p.UID = types.UID("u-" + name)
		_, err := f.kube.CoreV1().Pods("web").Create(context.Background(), p, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	var reported []corev1.Event
	require.Eventually(t, func() bool {
		evs, err := f.kube.CoreV1().Events("web").List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		reported = evs.Items
		return len(reported) > 0
	}, 5*time.Second, 20*time.Millisecond)
	require.Len(t, reported, 1)
	assert.Equal(t, reasonUnrecorded, reported[0].Reason)
	assert.Equal(t, corev1.EventTypeWarning, reported[0].Type)
	assert.Equal(t, "c", reported[0].InvolvedObject.Name)
	assert.Equal(t, 1.0, testutil.ToFloat64(cniUnrecorded))
	_, ok := f.a.sandboxes.Load("c-d")
	assert.True(t, ok, "the record joins the sandboxes the socket told of")

	// DEL: its address leaves the datapath's pods.
	assert.Equal(t, cnisock.ResultReady, call(cnisock.Request{Op: cnisock.OpDel, ContainerID: "c-a", Namespace: "voice", Name: "a",
		UID: "u-a", IPs: []string{"10.1.0.5"}}).Result)
	gone := func() bool {
		for {
			select {
			case w := <-f.wants:
				if len(f.wants) > 0 {
					continue // only the latest counts
				}
				for _, p := range w.pods {
					if p.v4 == netip.MustParseAddr("10.1.0.5") {
						return false
					}
				}
				return w.podsKnown
			default:
				return false
			}
		}
	}
	require.Eventually(t, gone, 5*time.Second, 20*time.Millisecond)

	// The slice Pending: a new sandbox of the pod is held at the hold's
	// end.
	cur := f.nodeState()
	cur.Spec.Slices[0].State = "Pending"
	_, err = f.purelb.PurelbV2().EgressNodeStates().Update(context.Background(), cur, metav1.UpdateOptions{})
	require.NoError(t, err)
	add.ContainerID = "c-a2"
	require.Eventually(t, func() bool { return call(add).Result == cnisock.ResultHeld }, 5*time.Second, 50*time.Millisecond)

	// No slice of its policy here: the node claims one.
	cur = f.nodeState()
	cur.Spec.Slices = nil
	_, err = f.purelb.PurelbV2().EgressNodeStates().Update(context.Background(), cur, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ns := f.nodeState()
		return ns != nil && len(ns.Status.Claims) == 1 && ns.Status.Claims[0].Endpoints == egress.EndpointsName(
			egress.PolicyRef{Kind: egress.KindEgress, Namespace: "voice", Name: "sip"}, netip.MustParseAddr("192.0.2.1"))
	}, 5*time.Second, 20*time.Millisecond)

	// Egress off: no socket (D-idle), no claims.
	f.a.Configure(agentWith(&purelbv2.LBNodeAgentEgressSpec{Enabled: false}))
	require.Eventually(t, func() bool {
		_, err := os.Stat(f.a.sockPath)
		return os.IsNotExist(err)
	}, 5*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { ns := f.nodeState(); return ns != nil && len(ns.Status.Claims) == 0 },
		5*time.Second, 20*time.Millisecond)
	assert.Zero(t, testutil.ToFloat64(snapshotAge), "egress off")
	assert.Zero(t, testutil.ToFloat64(cniUnrecorded), "egress off")
	// The snapshot stays: the plugin may still be in the chain.
	_, err = cnisock.ReadSnapshot(f.a.snapPath)
	require.NoError(t, err)
}
