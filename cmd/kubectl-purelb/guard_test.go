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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func guardSpec(guard map[string]interface{}, selector map[string]interface{}) map[string]interface{} {
	spec := localSpec("default")
	if guard != nil {
		spec["local"].(map[string]interface{})["addressGuard"] = guard
	}
	if selector != nil {
		spec["nodeSelector"] = map[string]interface{}{"matchLabels": selector}
	}
	return spec
}

func guardNode(name, kernel string, labels map[string]string) v1.Node {
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{KernelVersion: kernel}},
	}
}

func TestGuardModeResolution(t *testing.T) {
	none := decodeLBNodeAgents(lbnaListOf())
	assert.Equal(t, guardOff, resolveNodeConfig(none, nil).Guard, "no LBNodeAgent")

	plain := decodeLBNodeAgents(lbnaListOf(makeLBNA("default", guardSpec(nil, nil))))
	assert.Equal(t, guardOff, resolveNodeConfig(plain, nil).Guard, "no addressGuard")

	defaults := decodeLBNodeAgents(lbnaListOf(makeLBNA("default", guardSpec(map[string]interface{}{}, nil))))
	assert.Equal(t, guardEnforce, resolveNodeConfig(defaults, nil).Guard, "addressGuard: {} defaults to enforce")

	// A scoped override (monitor on zone a) wins over the catch-all
	// (enforce): the canary pattern for rolling the guard out.
	canary := decodeLBNodeAgents(lbnaListOf(
		makeLBNA("default", guardSpec(map[string]interface{}{"mode": "enforce"}, nil)),
		makeLBNA("canary", guardSpec(map[string]interface{}{"mode": "monitor"}, map[string]interface{}{"zone": "a"})),
	))
	assert.Equal(t, "monitor", resolveNodeConfig(canary, map[string]string{"zone": "a"}).Guard)
	assert.Equal(t, guardEnforce, resolveNodeConfig(canary, map[string]string{"zone": "b"}).Guard)
}

func TestGuardSummary(t *testing.T) {
	assert.Equal(t, "off", guardSummary(nil))
	assert.Equal(t, "off", guardSummary([]string{"off", "off"}))
	assert.Equal(t, "enforce 2", guardSummary([]string{"enforce", "enforce"}))
	assert.Equal(t, "enforce 3 | monitor 1 | off 1",
		guardSummary([]string{"monitor", "enforce", "off", "enforce", "enforce"}))
}

func purelbSvc(name string, ips []string, ports ...v1.ServicePort) v1.Service {
	s := v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name,
			Annotations: map[string]string{annotationAllocatedBy: brandPureLB}},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: ports},
	}
	for _, ip := range ips {
		s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, v1.LoadBalancerIngress{IP: ip})
	}
	return s
}

func TestGuardPorts(t *testing.T) {
	foreign := purelbSvc("foreign", []string{"192.0.2.1"}, v1.ServicePort{Protocol: v1.ProtocolTCP, Port: 22})
	foreign.Annotations = nil // not allocated by PureLB
	svcs := []v1.Service{
		purelbSvc("web", []string{"192.0.2.1", "2001:db8::1"},
			v1.ServicePort{Protocol: v1.ProtocolTCP, Port: 443, NodePort: 30443}),
		purelbSvc("dns", []string{"192.0.2.1"}, // shares the IPv4 address
			v1.ServicePort{Protocol: v1.ProtocolUDP, Port: 53},
			v1.ServicePort{Port: 80}), // protocol defaults to TCP
		purelbSvc("other", []string{"192.0.2.9"}, v1.ServicePort{Protocol: v1.ProtocolTCP, Port: 8080}),
		foreign,
	}
	assert.Equal(t, []string{"TCP/443", "TCP/80", "UDP/53"}, guardPorts("192.0.2.1", svcs),
		"the union across sharers; never a NodePort; never another allocator's Service")
	assert.Equal(t, []string{"TCP/443"}, guardPorts("2001:db8::1", svcs))
	assert.Empty(t, guardPorts("192.0.2.200", svcs))
}

// lbnaCRDWith returns an LBNodeAgent CRD whose v2 schema has (or lacks) the
// addressGuard field.
func lbnaCRDWith(hasGuard bool) *unstructured.Unstructured {
	localProps := map[string]interface{}{"localInterface": map[string]interface{}{"type": "string"}}
	if hasGuard {
		localProps["addressGuard"] = map[string]interface{}{"type": "object"}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"versions": []interface{}{map[string]interface{}{
			"name": "v2",
			"schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
				"properties": map[string]interface{}{"spec": map[string]interface{}{
					"properties": map[string]interface{}{"local": map[string]interface{}{
						"properties": localProps,
					}},
				}},
			}},
		}}},
	}}
}

func agentDaemonSet(caps ...v1.Capability) *appsv1.DaemonSetList {
	return &appsv1.DaemonSetList{Items: []appsv1.DaemonSet{{
		ObjectMeta: metav1.ObjectMeta{Name: "lbnodeagent"},
		Spec: appsv1.DaemonSetSpec{Template: v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{{
			Name:            "lbnodeagent",
			SecurityContext: &v1.SecurityContext{Capabilities: &v1.Capabilities{Add: caps}},
		}}}}},
	}}}
}

func messages(checks []checkResult) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString(c.Status + " " + c.Message + "\n")
	}
	return b.String()
}

func TestAddressGuardChecks(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "daemonsets"}, "", errors.New("no"))
	enforce := decodeLBNodeAgents(lbnaListOf(makeLBNA("default", guardSpec(map[string]interface{}{}, nil))))
	off := decodeLBNodeAgents(lbnaListOf(makeLBNA("default", guardSpec(nil, nil))))
	nodes := []v1.Node{
		guardNode("new", "6.12.107+deb13-amd64", nil),
		guardNode("old", "5.15.0-91-generic", nil),
	}
	goodDS := agentDaemonSet("NET_ADMIN", "NET_RAW", "BPF")

	t.Run("guard off, CRD current: nothing to say", func(t *testing.T) {
		assert.Empty(t, addressGuardChecks(off, nodes, goodDS, nil, lbnaCRDWith(true), nil))
	})

	t.Run("stale CRD is flagged even when the guard is off", func(t *testing.T) {
		out := messages(addressGuardChecks(off, nodes, goodDS, nil, lbnaCRDWith(false), nil))
		assert.Contains(t, out, "WARN The installed LBNodeAgent CRD has no addressGuard field")
	})

	t.Run("configured: kernel and capability checks", func(t *testing.T) {
		out := messages(addressGuardChecks(enforce, nodes, goodDS, nil, lbnaCRDWith(true), nil))
		assert.Contains(t, out, `PASS LBNodeAgent "purelb-system/default": address guard enforce, failurePolicy closed`)
		assert.Contains(t, out, "FAIL 1 node(s) can't run the address guard",
			"fail-closed (the default): the old node announces nothing")
		assert.Contains(t, out, "old (5.15.0-91-generic)")
		assert.NotContains(t, out, "new (", "a 6.12 node can run it")
		assert.NotContains(t, out, "BPF capability")
	})

	openPolicy := decodeLBNodeAgents(lbnaListOf(makeLBNA("default", guardSpec(map[string]interface{}{"failurePolicy": "open"}, nil))))

	t.Run("fail-open turns the same problems into warnings", func(t *testing.T) {
		out := messages(addressGuardChecks(openPolicy, nodes, agentDaemonSet("NET_ADMIN"), nil, lbnaCRDWith(true), nil))
		assert.Contains(t, out, "WARN 1 node(s) can't run the address guard")
		assert.Contains(t, out, `WARN DaemonSet "lbnodeagent": the lbnodeagent container lacks the BPF capability`)
		assert.NotContains(t, out, "FAIL")
	})

	t.Run("missing BPF capability", func(t *testing.T) {
		out := messages(addressGuardChecks(enforce, nodes[:1], agentDaemonSet("NET_ADMIN"), nil, lbnaCRDWith(true), nil))
		assert.Contains(t, out, `FAIL DaemonSet "lbnodeagent": the lbnodeagent container lacks the BPF capability`)
		out = messages(addressGuardChecks(enforce, nodes[:1], agentDaemonSet("CAP_BPF"), nil, lbnaCRDWith(true), nil))
		assert.NotContains(t, out, "lacks the BPF capability", "CAP_ prefix accepted")
	})

	t.Run("unreadable CRD is not noise when the guard is off", func(t *testing.T) {
		assert.Empty(t, addressGuardChecks(off, nodes, nil, forbidden, nil, forbidden))
	})

	t.Run("forbidden reads say so instead of failing", func(t *testing.T) {
		out := messages(addressGuardChecks(enforce, nodes[:1], nil, forbidden, nil, forbidden))
		assert.Contains(t, out, "unable to check the LBNodeAgent CRD schema: forbidden (see rbac-sample.yaml)")
		assert.Contains(t, out, "unable to check the lbnodeagent capabilities: forbidden (see rbac-sample.yaml)")
	})

	t.Run("excluded interfaces are always called out", func(t *testing.T) {
		excl := decodeLBNodeAgents(lbnaListOf(makeLBNA("default",
			guardSpec(map[string]interface{}{"excludeInterfaces": []interface{}{"eth2"}}, nil))))
		out := messages(addressGuardChecks(excl, nodes[:1], goodDS, nil, lbnaCRDWith(true), nil))
		assert.Contains(t, out, "WARN LBNodeAgent \"purelb-system/default\": address guard excludes eth2")
	})
}

// inspect reports the protocols allowed where the addresses are filtered:
// the announcing nodes for a local pool, every node for a remote one.
func TestGuardProtocolsFor(t *testing.T) {
	agents := decodeLBNodeAgents(lbnaListOf(
		makeLBNA("default", guardSpec(map[string]interface{}{"allowedProtocols": []interface{}{int64(47)}}, nil)),
		makeLBNA("edge", guardSpec(map[string]interface{}{"allowedProtocols": []interface{}{int64(50), int64(47)}},
			map[string]interface{}{"role": "edge"})),
	))
	nodes := []v1.Node{
		guardNode("node-a", "6.12.0", nil),
		guardNode("node-b", "6.12.0", map[string]string{"role": "edge"}),
	}
	onA := []announcementInfo{{Node: "node-a"}}
	assert.Equal(t, []int32{47}, guardProtocolsFor("local", onA, nodes, agents), "only the announcing node's agent")
	assert.Equal(t, []int32{47, 50}, guardProtocolsFor(poolTypeRemote, onA, nodes, agents), "every node holds a remote address")
	assert.Equal(t, "enforce on node-a", guardModeFor("local", onA, nodes, agents))
}

func TestHasCapability(t *testing.T) {
	assert.False(t, hasCapability(nil, "BPF"))
	assert.True(t, hasCapability(&v1.SecurityContext{Capabilities: &v1.Capabilities{Add: []v1.Capability{"CAP_BPF"}}}, "BPF"))
	assert.False(t, hasCapability(&v1.SecurityContext{Capabilities: &v1.Capabilities{Add: []v1.Capability{"NET_ADMIN"}}}, "BPF"))
	privileged := true
	assert.True(t, hasCapability(&v1.SecurityContext{Privileged: &privileged}, "BPF"), "a privileged container has every capability")
}

func TestGuardConfigured(t *testing.T) {
	assert.False(t, guardConfigured(nil))
	assert.False(t, guardConfigured(decodeLBNodeAgents(lbnaListOf(makeLBNA("default", localSpec("default"))))))
	assert.True(t, guardConfigured(decodeLBNodeAgents(lbnaListOf(
		makeLBNA("default", localSpec("default")),
		makeLBNA("edge", guardSpec(map[string]interface{}{}, map[string]interface{}{"role": "edge"}))))))
}

// status reads the agents while the guard is configured, and also when it
// may have been removed since an agent started: until that agent restarts
// the program is still loaded, and only the agent can say so.
func TestGuardReadNeeded(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	before, after := metav1.NewTime(started.Add(-time.Hour)), metav1.NewTime(started.Add(time.Minute))
	pod := agentPod("a", "node-a")
	pod.Status.StartTime = &started
	pods := []v1.Pod{pod}
	lbna := func(changed metav1.Time, guard bool) *unstructured.UnstructuredList {
		var g map[string]interface{}
		if guard {
			g = map[string]interface{}{}
		}
		a := makeLBNA("default", guardSpec(g, nil))
		a.SetCreationTimestamp(before)
		a.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Time: &changed}})
		return lbnaListOf(a)
	}
	event := func(at metav1.Time) *v1.EventList {
		return &v1.EventList{Items: []v1.Event{{Reason: "AddressGuardRestartRequired", LastTimestamp: at}}}
	}
	need := func(list *unstructured.UnstructuredList, events *v1.EventList, pods []v1.Pod) bool {
		return guardReadNeeded(decodeLBNodeAgents(list), list, events, pods)
	}

	assert.True(t, need(lbna(before, true), nil, pods), "configured")
	assert.False(t, need(lbna(before, false), nil, pods), "never touched since the agents started")
	assert.True(t, need(lbna(after, false), nil, pods), "edited since an agent started: the guard may have been removed")
	assert.True(t, need(lbna(before, false), event(after), pods), "an agent said a restart is required since")
	assert.False(t, need(lbna(before, false), event(before), pods), "a restart since then cleared it")
	assert.False(t, need(lbna(after, false), event(after), nil), "no agents to read")
}
