// Copyright 2026 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// =============================================================================
// Test helpers
// =============================================================================

func newFakeClients(coreObjects []runtime.Object, dynamicObjects ...runtime.Object) *clients {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "ServiceGroupList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "LBNodeAgentList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "bgp.purelb.io", Version: "v1", Kind: "BGPConfigurationList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "bgp.purelb.io", Version: "v1", Kind: "BGPNodeStatusList"},
		&unstructured.UnstructuredList{},
	)

	// The dynamic fake client needs resource-to-list-kind mappings for every GVR we List().
	// gvrBGPConfigurations uses resource name "configs" (not "bgpconfigurations").
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvrServiceGroups:     "ServiceGroupList",
			gvrLBNodeAgents:      "LBNodeAgentList",
			gvrBGPConfigurations: "BGPConfigurationList",
			gvrBGPNodeStatuses:   "BGPNodeStatusList",
		},
		dynamicObjects...,
	)

	return &clients{
		core:      fake.NewSimpleClientset(coreObjects...),
		dynamic:   dynClient,
		namespace: "purelb-system",
	}
}

func makeSG(name, poolType string, v4pool, v4subnet string) *unstructured.Unstructured {
	sg := &unstructured.Unstructured{}
	sg.SetGroupVersionKind(schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "ServiceGroup"})
	sg.SetName(name)
	sg.SetNamespace("purelb-system")
	sg.Object["spec"] = map[string]interface{}{
		poolType: map[string]interface{}{
			"v4pools": []interface{}{
				map[string]interface{}{
					"pool":   v4pool,
					"subnet": v4subnet,
				},
			},
		},
	}
	return sg
}

func makeLease(nodeName string, subnets string, renewSeconds int) *coordinationv1.Lease {
	renewTime := metav1.NewMicroTime(time.Now().Add(-time.Duration(renewSeconds) * time.Second))
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leasePrefix + nodeName,
			Namespace: "purelb-system",
			Annotations: map[string]string{
				subnetsAnnotation: subnets,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To(nodeName),
			LeaseDurationSeconds: ptr.To(int32(10)),
			RenewTime:            &renewTime,
		},
	}
}

func makePureLBService(ns, name, ip, pool, poolType string) *v1.Service {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				annotationAllocatedBy:   brandPureLB,
				annotationAllocatedFrom: pool,
				annotationPoolType:      poolType,
			},
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{Port: 80, Protocol: v1.ProtocolTCP},
			},
		},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: ip}},
			},
		},
	}
	return svc
}

func makeDualStackService(ns, name, ipv4, ipv6, pool, poolType string) *v1.Service {
	svc := makePureLBService(ns, name, ipv4, pool, poolType)
	svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress,
		v1.LoadBalancerIngress{IP: ipv6})
	return svc
}

func makeSharedService(ns, name, ip, pool, poolType, sharingKey string, port int32) *v1.Service {
	svc := makePureLBService(ns, name, ip, pool, poolType)
	svc.Annotations[annotationSharing] = sharingKey
	svc.Spec.Ports[0].Port = port
	return svc
}

func TestCountHealthyAnnouncers(t *testing.T) {
	withAnnouncing := func(svc *v1.Service, v4, v6 string) *v1.Service {
		if v4 != "" {
			svc.Annotations[annotationAnnouncing+"-IPv4"] = v4
		}
		if v6 != "" {
			svc.Annotations[annotationAnnouncing+"-IPv6"] = v6
		}
		return svc
	}

	healthy := map[string]bool{"node-a": true, "node-b": true}

	t.Run("counts a healthy announcer", func(t *testing.T) {
		svcs := []v1.Service{
			*withAnnouncing(makePureLBService("ns", "s1", "10.0.0.1", "p", "local"),
				"node-a,eth0,10.0.0.1", ""),
		}
		assert.Equal(t, map[string]int{"node-a": 1}, countHealthyAnnouncers(svcs, healthy))
	})

	t.Run("a stale slot for a node without a lease is not counted", func(t *testing.T) {
		// node-c has no healthy lease: it died without clearing its slot.
		svcs := []v1.Service{
			*withAnnouncing(makePureLBService("ns", "s1", "10.0.0.1", "p", "local"),
				"node-c,eth0,10.0.0.1", ""),
		}
		assert.Empty(t, countHealthyAnnouncers(svcs, healthy))
	})

	t.Run("non-PureLB service is ignored", func(t *testing.T) {
		svc := withAnnouncing(makePureLBService("ns", "s1", "10.0.0.1", "p", "local"),
			"node-a,eth0,10.0.0.1", "")
		delete(svc.Annotations, annotationAllocatedBy)
		assert.Empty(t, countHealthyAnnouncers([]v1.Service{*svc}, healthy))
	})

	t.Run("dual-stack counts each family", func(t *testing.T) {
		svcs := []v1.Service{
			*withAnnouncing(makePureLBService("ns", "s1", "10.0.0.1", "p", "local"),
				"node-a,eth0,10.0.0.1", "node-a,eth0,2001:db8::1"),
		}
		assert.Equal(t, map[string]int{"node-a": 2}, countHealthyAnnouncers(svcs, healthy))
	})

	t.Run("mixed healthy and stale across services", func(t *testing.T) {
		svcs := []v1.Service{
			*withAnnouncing(makePureLBService("ns", "s1", "10.0.0.1", "p", "local"),
				"node-a,eth0,10.0.0.1", ""),
			*withAnnouncing(makePureLBService("ns", "s2", "10.0.0.2", "p", "local"),
				"node-b,eth0,10.0.0.2 node-c,eth0,10.0.0.2", ""),
		}
		// node-c is stale (no lease) and must not be counted for s2.
		assert.Equal(t, map[string]int{"node-a": 1, "node-b": 1}, countHealthyAnnouncers(svcs, healthy))
	})
}

// =============================================================================
// pools command tests
// =============================================================================

func TestRunPools_BasicUtilization(t *testing.T) {
	sg := makeSG("test-pool", "local", "10.0.0.0/28", "10.0.0.0/24")
	svc1 := makePureLBService("default", "svc-a", "10.0.0.1", "test-pool", "local")
	svc2 := makePureLBService("default", "svc-b", "10.0.0.2", "test-pool", "local")

	c := newFakeClients([]runtime.Object{svc1, svc2}, sg)

	err := runPools(context.Background(), c, outputJSON, "", false)
	require.NoError(t, err)
}

func TestRunPools_FilterServiceGroup(t *testing.T) {
	sg1 := makeSG("pool-a", "local", "10.0.0.0/28", "10.0.0.0/24")
	sg2 := makeSG("pool-b", "remote", "10.1.0.0/28", "10.1.0.0/24")
	svc := makePureLBService("default", "svc-a", "10.0.0.1", "pool-a", "local")

	c := newFakeClients([]runtime.Object{svc}, sg1, sg2)

	// Filter to pool-a only
	err := runPools(context.Background(), c, outputJSON, "pool-a", false)
	require.NoError(t, err)
}

func TestRunPools_EmptyCluster(t *testing.T) {
	c := newFakeClients(nil)
	err := runPools(context.Background(), c, outputJSON, "", false)
	require.NoError(t, err)
}

// =============================================================================
// services command tests
// =============================================================================

func TestRunServices_BasicList(t *testing.T) {
	svc := makePureLBService("test", "web", "10.0.0.1", "my-pool", "local")
	svc.Annotations[annotationAnnouncing+"-IPv4"] = "node-a,eth0,10.0.0.1"

	lease := makeLease("node-a", "10.0.0.0/24", 2)

	c := newFakeClients([]runtime.Object{svc, lease})

	err := runServices(context.Background(), c, outputJSON, "", "", "", false)
	require.NoError(t, err)
}

func TestRunServices_SharedIPs(t *testing.T) {
	svc1 := makeSharedService("test", "http", "10.0.0.1", "pool", "remote", "web-group", 80)
	svc2 := makeSharedService("test", "https", "10.0.0.1", "pool", "remote", "web-group", 443)

	c := newFakeClients([]runtime.Object{svc1, svc2})

	err := runServices(context.Background(), c, outputJSON, "", "", "", false)
	require.NoError(t, err)
}

func TestRunServices_FilterByPool(t *testing.T) {
	svc1 := makePureLBService("test", "svc-a", "10.0.0.1", "pool-a", "local")
	svc2 := makePureLBService("test", "svc-b", "10.1.0.1", "pool-b", "remote")

	c := newFakeClients([]runtime.Object{svc1, svc2})

	err := runServices(context.Background(), c, outputJSON, "pool-a", "", "", false)
	require.NoError(t, err)
}

func TestRunServices_NoAnnouncerRemoteIsOK(t *testing.T) {
	svc := makePureLBService("test", "remote-svc", "10.0.0.1", "pool", "remote")
	// No announcing annotation — remote pools don't set it

	c := newFakeClients([]runtime.Object{svc})

	err := runServices(context.Background(), c, outputJSON, "", "", "", false)
	require.NoError(t, err)
}

// =============================================================================
// election command tests
// =============================================================================

func TestBuildHealthyNodeSet(t *testing.T) {
	healthy := makeLease("node-a", "10.0.0.0/24", 2)  // renewed 2s ago, dur=10s, not expired
	expired := makeLease("node-b", "10.1.0.0/24", 20) // renewed 20s ago, dur=10s, expired

	result := buildHealthyNodeSet([]coordinationv1.Lease{*healthy, *expired})
	assert.True(t, result["node-a"])
	assert.False(t, result["node-b"])
}

func TestRunElection_SubnetCoverage(t *testing.T) {
	lease1 := makeLease("node-a", "192.168.1.0/24", 2)
	lease2 := makeLease("node-b", "192.168.2.0/24", 2)
	sg := makeSG("pool", "local", "192.168.1.100-192.168.1.110", "192.168.1.0/24")

	c := newFakeClients([]runtime.Object{lease1, lease2}, sg)

	err := runElection(context.Background(), c, outputJSON, "", false, "")
	require.NoError(t, err)
}

func TestRunElection_DrainSimulation(t *testing.T) {
	lease1 := makeLease("node-a", "192.168.1.0/24", 2)
	lease2 := makeLease("node-b", "192.168.1.0/24", 2)
	sg := makeSG("pool", "local", "192.168.1.100-192.168.1.110", "192.168.1.0/24")

	svc := makePureLBService("test", "web", "192.168.1.100", "pool", "local")
	svc.Annotations[annotationAnnouncing+"-IPv4"] = "node-a,eth0,192.168.1.100"

	c := newFakeClients([]runtime.Object{lease1, lease2, svc}, sg)

	err := runElection(context.Background(), c, outputJSON, "", false, "node-a")
	require.NoError(t, err)
}

// =============================================================================
// validate command tests
// =============================================================================

func TestRunValidate_OverlappingRanges(t *testing.T) {
	sg1 := makeSG("pool-a", "local", "10.0.0.0/24", "10.0.0.0/24")
	sg2 := makeSG("pool-b", "local", "10.0.0.128/25", "10.0.0.0/24") // overlaps with pool-a

	c := newFakeClients(nil, sg1, sg2)

	// Table format returns error on FAIL
	err := runValidate(context.Background(), c, outputTable, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")
}

func TestRunValidate_Clean(t *testing.T) {
	sg1 := makeSG("pool-a", "local", "10.0.0.0/28", "10.0.0.0/24")
	sg2 := makeSG("pool-b", "remote", "10.1.0.0/28", "10.1.0.0/24")

	lbna := &unstructured.Unstructured{}
	lbna.SetGroupVersionKind(schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "LBNodeAgent"})
	lbna.SetName("default")
	lbna.SetNamespace("purelb-system")
	lbna.Object["spec"] = map[string]interface{}{
		"local": map[string]interface{}{
			"dummyInterface": "kube-lb0",
		},
	}

	c := newFakeClients(nil, sg1, sg2, lbna)

	err := runValidate(context.Background(), c, outputJSON, false)
	assert.NoError(t, err)
}

// =============================================================================
// output format tests
// =============================================================================

func TestParseOutputFormat(t *testing.T) {
	f, err := parseOutputFormat("")
	assert.NoError(t, err)
	assert.Equal(t, outputTable, f)

	f, err = parseOutputFormat("json")
	assert.NoError(t, err)
	assert.Equal(t, outputJSON, f)

	f, err = parseOutputFormat("yaml")
	assert.NoError(t, err)
	assert.Equal(t, outputYAML, f)

	f, err = parseOutputFormat("table")
	assert.NoError(t, err)
	assert.Equal(t, outputTable, f)

	_, err = parseOutputFormat("xml")
	assert.Error(t, err)
}

// =============================================================================
// snapshot tests
// =============================================================================

func TestFetchSnapshot(t *testing.T) {
	sg := makeSG("pool", "local", "10.0.0.0/28", "10.0.0.0/24")
	svc := makePureLBService("default", "web", "10.0.0.1", "pool", "local")
	lease := makeLease("node-a", "10.0.0.0/24", 2)

	lbna := &unstructured.Unstructured{}
	lbna.SetGroupVersionKind(schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "LBNodeAgent"})
	lbna.SetName("default")
	lbna.SetNamespace("purelb-system")

	c := newFakeClients([]runtime.Object{svc, lease}, sg, lbna)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	assert.NotNil(t, snap.pods)
	assert.NotNil(t, snap.services)
	assert.NotNil(t, snap.serviceGroups)
	assert.NotNil(t, snap.leases)
	assert.NotNil(t, snap.bgpNodeStatuses)
	assert.NotNil(t, snap.lbNodeAgents)
	assert.Len(t, snap.serviceGroups.Items, 1)
	assert.Len(t, snap.lbNodeAgents.Items, 1)
}

// =============================================================================
// render function tests
// =============================================================================

func TestRenderStatus(t *testing.T) {
	sg := makeSG("pool", "local", "10.0.0.0/28", "10.0.0.0/24")
	svc := makePureLBService("default", "web", "10.0.0.1", "pool", "local")
	lease := makeLease("node-a", "10.0.0.0/24", 2)

	c := newFakeClients([]runtime.Object{svc, lease}, sg)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	err = renderStatus(snap, outputJSON)
	assert.NoError(t, err)
}

func TestRenderServices(t *testing.T) {
	svc := makePureLBService("test", "web", "10.0.0.1", "pool", "local")
	svc.Annotations[annotationAnnouncing+"-IPv4"] = "node-a,eth0,10.0.0.1"
	lease := makeLease("node-a", "10.0.0.0/24", 2)

	lbna := &unstructured.Unstructured{}
	lbna.SetGroupVersionKind(schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "LBNodeAgent"})
	lbna.SetName("default")
	lbna.SetNamespace("purelb-system")

	c := newFakeClients([]runtime.Object{svc, lease}, lbna)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	err = renderServices(snap, outputJSON, "", "", "", false)
	assert.NoError(t, err)
}

func TestRenderPools(t *testing.T) {
	sg := makeSG("pool", "local", "10.0.0.0/28", "10.0.0.0/24")
	svc1 := makePureLBService("default", "a", "10.0.0.1", "pool", "local")
	svc2 := makePureLBService("default", "b", "10.0.0.2", "pool", "local")

	c := newFakeClients([]runtime.Object{svc1, svc2}, sg)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	err = renderPools(snap, outputJSON, "", false)
	assert.NoError(t, err)
}

func TestRenderStatus_EmptyCluster(t *testing.T) {
	c := newFakeClients(nil)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	err = renderStatus(snap, outputJSON)
	assert.NoError(t, err)
}

func TestRenderPools_EmptyCluster(t *testing.T) {
	c := newFakeClients(nil)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	err = renderPools(snap, outputJSON, "", false)
	assert.NoError(t, err)
}

// =============================================================================
// pod categorization tests (install-method-agnostic)
// =============================================================================

func makePodWithContainers(name string, containerNames ...string) v1.Pod {
	containers := make([]v1.Container, 0, len(containerNames))
	for _, n := range containerNames {
		containers = append(containers, v1.Container{Name: n})
	}
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "purelb-system"},
		Spec:       v1.PodSpec{Containers: containers},
	}
}

func TestCategorizePureLBPods(t *testing.T) {
	tests := []struct {
		name        string
		podList     *v1.PodList // nil exercises the nil-input path
		wantAlloc   int
		wantLBNA    int
		wantK8GoBGP int
	}{
		{
			name:    "nil PodList",
			podList: nil,
		},
		{
			name:    "empty Items",
			podList: &v1.PodList{},
		},
		{
			name: "allocator pod only",
			podList: &v1.PodList{Items: []v1.Pod{
				makePodWithContainers("alloc", "allocator"),
			}},
			wantAlloc: 1,
		},
		{
			name: "lbnodeagent with k8gobgp sidecar",
			podList: &v1.PodList{Items: []v1.Pod{
				makePodWithContainers("lbna", "lbnodeagent", "k8gobgp"),
			}},
			wantLBNA:    1,
			wantK8GoBGP: 1,
		},
		{
			name: "lbnodeagent without sidecar (no-bgp install)",
			podList: &v1.PodList{Items: []v1.Pod{
				makePodWithContainers("lbna", "lbnodeagent"),
			}},
			wantLBNA: 1,
		},
		{
			name: "full multi-node install with BGP",
			podList: &v1.PodList{Items: []v1.Pod{
				makePodWithContainers("alloc", "allocator"),
				makePodWithContainers("lbna1", "lbnodeagent", "k8gobgp"),
				makePodWithContainers("lbna2", "lbnodeagent", "k8gobgp"),
			}},
			wantAlloc:   1,
			wantLBNA:    2,
			wantK8GoBGP: 2,
		},
		{
			name: "full install without BGP (gobgp.enabled=false / no-bgp manifest)",
			podList: &v1.PodList{Items: []v1.Pod{
				makePodWithContainers("alloc", "allocator"),
				makePodWithContainers("lbna1", "lbnodeagent"),
				makePodWithContainers("lbna2", "lbnodeagent"),
			}},
			wantAlloc: 1,
			wantLBNA:  2,
		},
		{
			name: "unrelated pod in namespace is ignored",
			podList: &v1.PodList{Items: []v1.Pod{
				makePodWithContainers("alloc", "allocator"),
				makePodWithContainers("other", "nginx"),
			}},
			wantAlloc: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat := categorizePureLBPods(tt.podList)
			assert.Equal(t, tt.wantAlloc, len(cat.allocator), "allocator pod count")
			assert.Equal(t, tt.wantLBNA, len(cat.lbnodeagent), "lbnodeagent pod count")
			assert.Equal(t, tt.wantK8GoBGP, len(cat.withK8GoBGP), "withK8GoBGP pod count")
		})
	}
}

// =============================================================================
// BGP state detection + formatting tests
// =============================================================================

// makeBGPNodeStatus builds an unstructured BGPNodeStatus CR with the given
// neighbor states and a count of failed (inRIB=false) imported addresses.
// neighborStates may be nil to model a freshly-started sidecar with no
// BGPConfiguration applied yet.
func makeBGPNodeStatus(nodeName string, neighborStates []string, importFailures int) *unstructured.Unstructured {
	bgpns := &unstructured.Unstructured{}
	bgpns.SetGroupVersionKind(schema.GroupVersionKind{Group: "bgp.purelb.io", Version: "v1", Kind: "BGPNodeStatus"})
	bgpns.SetName(nodeName)

	neighbors := make([]interface{}, 0, len(neighborStates))
	for i, state := range neighborStates {
		neighbors = append(neighbors, map[string]interface{}{
			"address": "192.0.2." + string(rune('1'+i)),
			"state":   state,
		})
	}

	importedAddrs := make([]interface{}, 0, importFailures)
	for i := 0; i < importFailures; i++ {
		importedAddrs = append(importedAddrs, map[string]interface{}{
			"address": "198.51.100." + string(rune('1'+i)),
			"inRIB":   false,
		})
	}

	bgpns.Object["status"] = map[string]interface{}{
		"nodeName":  nodeName,
		"neighbors": neighbors,
		"netlinkImport": map[string]interface{}{
			"importedAddresses": importedAddrs,
		},
	}
	return bgpns
}

func TestDetectBGPState(t *testing.T) {
	tests := []struct {
		name                 string
		bgpns                []*unstructured.Unstructured // nil → list is nil; empty slice → list with 0 items
		nilList              bool                         // explicit nil list (overrides bgpns)
		podsWithK8GoBGP      int                          // pods to put in purelbPods.withK8GoBGP
		wantState            bgpState
		wantPeersTotal       int
		wantPeersEstablished int
		wantImportFailures   int
		wantSummary          string
		wantSentence         string
	}{
		{
			name:         "nil list, no k8gobgp pods → NotEnabled",
			nilList:      true,
			wantState:    bgpStateNotEnabled,
			wantSummary:  "not enabled",
			wantSentence: "BGP not enabled.",
		},
		{
			name:            "nil list, k8gobgp pods present → NotConfigured (startup race)",
			nilList:         true,
			podsWithK8GoBGP: 1,
			wantState:       bgpStateNotConfigured,
			wantSummary:     "not configured",
			wantSentence:    "BGP not configured.",
		},
		{
			name:         "empty list, no pods → NotEnabled",
			bgpns:        []*unstructured.Unstructured{},
			wantState:    bgpStateNotEnabled,
			wantSummary:  "not enabled",
			wantSentence: "BGP not enabled.",
		},
		{
			name:            "empty list, sidecar present → NotConfigured",
			bgpns:           []*unstructured.Unstructured{},
			podsWithK8GoBGP: 2,
			wantState:       bgpStateNotConfigured,
			wantSummary:     "not configured",
			wantSentence:    "BGP not configured.",
		},
		{
			name:         "BGPNodeStatus row with no neighbors → NotConfigured",
			bgpns:        []*unstructured.Unstructured{makeBGPNodeStatus("node-a", nil, 0)},
			wantState:    bgpStateNotConfigured,
			wantSummary:  "not configured",
			wantSentence: "BGP not configured.",
		},
		{
			name:                 "all peers Established, no import failures → Active OK",
			bgpns:                []*unstructured.Unstructured{makeBGPNodeStatus("node-a", []string{"Established", "Established"}, 0)},
			wantState:            bgpStateActive,
			wantPeersTotal:       2,
			wantPeersEstablished: 2,
			wantSummary:          "2/2 peers established | netlinkImport OK",
			wantSentence:         "",
		},
		{
			name:                 "some peers down → Active with mixed count",
			bgpns:                []*unstructured.Unstructured{makeBGPNodeStatus("node-a", []string{"Established", "Idle", "Active"}, 0)},
			wantState:            bgpStateActive,
			wantPeersTotal:       3,
			wantPeersEstablished: 1,
			wantSummary:          "1/3 peers established | netlinkImport OK",
		},
		{
			name:                 "import failures → Active with failure count",
			bgpns:                []*unstructured.Unstructured{makeBGPNodeStatus("node-a", []string{"Established"}, 2)},
			wantState:            bgpStateActive,
			wantPeersTotal:       1,
			wantPeersEstablished: 1,
			wantImportFailures:   2,
			wantSummary:          "1/1 peers established | 2 import failure(s)",
		},
		{
			name: "multi-node aggregates correctly",
			bgpns: []*unstructured.Unstructured{
				makeBGPNodeStatus("node-a", []string{"Established"}, 0),
				makeBGPNodeStatus("node-b", []string{"Established", "Idle"}, 1),
			},
			wantState:            bgpStateActive,
			wantPeersTotal:       3,
			wantPeersEstablished: 2,
			wantImportFailures:   1,
			wantSummary:          "2/3 peers established | 1 import failure(s)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var list *unstructured.UnstructuredList
			if !tt.nilList {
				list = &unstructured.UnstructuredList{}
				for _, item := range tt.bgpns {
					list.Items = append(list.Items, *item)
				}
			}

			pods := purelbPods{}
			for i := 0; i < tt.podsWithK8GoBGP; i++ {
				pods.withK8GoBGP = append(pods.withK8GoBGP, v1.Pod{})
			}

			info := detectBGPState(list, pods)
			assert.Equal(t, tt.wantState, info.state, "state")
			assert.Equal(t, tt.wantPeersTotal, info.peersTotal, "peersTotal")
			assert.Equal(t, tt.wantPeersEstablished, info.peersEstablished, "peersEstablished")
			assert.Equal(t, tt.wantImportFailures, info.importFailures, "importFailures")
			if tt.wantSummary != "" {
				assert.Equal(t, tt.wantSummary, info.statusSummary(), "statusSummary")
			}
			assert.Equal(t, tt.wantSentence, info.sentence(), "sentence")
		})
	}
}

// =============================================================================
// nodeSelector / announcement-visibility tests (v0.17.0)
// =============================================================================

// captureOutput redirects stdout while fn runs so tests can assert on what the
// command actually prints, not merely that it returned nil.
func captureOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	os.Stdout = w

	runErr := fn()

	require.NoError(t, w.Close())
	os.Stdout = old
	out, readErr := io.ReadAll(r)
	require.NoError(t, readErr)
	return string(out), runErr
}

func makeNode(name string, labels map[string]string) *v1.Node {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func makeLBNodeAgent(name string, spec map[string]interface{}) *unstructured.Unstructured {
	lbna := &unstructured.Unstructured{}
	lbna.SetGroupVersionKind(schema.GroupVersionKind{Group: "purelb.io", Version: "v2", Kind: "LBNodeAgent"})
	lbna.SetName(name)
	lbna.SetNamespace("purelb-system")
	lbna.Object["spec"] = spec
	return lbna
}

// TestRenderStatus_CountsUnannouncedIPs covers the bug where `status` reported
// "Overall: OK" while every allocated address had lost its announcer, because
// it only counted services awaiting allocation. `services` flagged the same
// addresses as NO ANNOUNCER at that moment.
func TestRenderStatus_CountsUnannouncedIPs(t *testing.T) {
	sg := makeSG("pool", "local", "10.0.0.0/28", "10.0.0.0/24")
	// Allocated, but no announcing-* annotation: nothing is answering for it.
	svc := makePureLBService("default", "web", "10.0.0.1", "pool", "local")
	lease := makeLease("node-a", "10.0.0.0/24", 2)

	c := newFakeClients([]runtime.Object{svc, lease}, sg)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	out, err := captureOutput(t, func() error { return renderStatus(snap, outputJSON) })
	require.NoError(t, err)

	assert.Contains(t, out, "1 IP(s) not announced")
	assert.Contains(t, out, "WARNING")
	assert.NotContains(t, out, `"overall": "OK"`)
}

// The same service WITH a healthy announcer must stay clean, so the check
// cannot be satisfied by simply always warning.
func TestRenderStatus_AnnouncedIPIsNotAProblem(t *testing.T) {
	sg := makeSG("pool", "local", "10.0.0.0/28", "10.0.0.0/24")
	svc := makePureLBService("default", "web", "10.0.0.1", "pool", "local")
	svc.Annotations[annotationAnnouncing+"-IPv4"] = "node-a,eth0,10.0.0.1"
	lease := makeLease("node-a", "10.0.0.0/24", 2)

	c := newFakeClients([]runtime.Object{svc, lease}, sg)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	out, err := captureOutput(t, func() error { return renderStatus(snap, outputJSON) })
	require.NoError(t, err)
	assert.NotContains(t, out, "not announced")
}

// A node with a perfectly healthy lease that no LBNodeAgent selects announces
// nothing; lease health alone cannot express that.
func TestRenderStatus_DeselectedNodeIsVisible(t *testing.T) {
	lease := makeLease("node-a", "", 2)
	node := makeNode("node-a", map[string]string{"role": "worker"})
	lbna := makeLBNodeAgent("edge-only", map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"role": "edge"},
		},
		"local": map[string]interface{}{"localInterface": "default"},
	})

	c := newFakeClients([]runtime.Object{lease, node}, lbna)
	snap, err := fetchSnapshot(context.Background(), c)
	require.NoError(t, err)

	out, err := captureOutput(t, func() error { return renderStatus(snap, outputJSON) })
	require.NoError(t, err)
	assert.Contains(t, out, "selected by no LBNodeAgent")
	assert.Contains(t, out, "announcing nothing")
}

// TestRunElection_ConfigColumn checks the per-node config state that tells a
// deliberately-excluded node apart from a broken one.
func TestRunElection_ConfigColumn(t *testing.T) {
	leaseEdge := makeLease("node-edge", "10.0.0.0/24", 2)
	leaseWorker := makeLease("node-worker", "", 2)
	nodeEdge := makeNode("node-edge", map[string]string{"role": "edge"})
	nodeWorker := makeNode("node-worker", map[string]string{"role": "worker"})
	lbna := makeLBNodeAgent("edge-only", map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"role": "edge"},
		},
		"local": map[string]interface{}{"localInterface": "default"},
	})

	c := newFakeClients([]runtime.Object{leaseEdge, leaseWorker, nodeEdge, nodeWorker}, lbna)

	out, err := captureOutput(t, func() error {
		return runElection(context.Background(), c, outputTable, "", false, "")
	})
	require.NoError(t, err)

	assert.Contains(t, out, "CONFIG")
	assert.Contains(t, out, configStateConfigured)
	assert.Contains(t, out, configStateDeselected)
	assert.Contains(t, out, "node-worker is selected by no LBNodeAgent")
}

// TestRunValidate_NodeSelectorChecks covers the LBNodeAgent nodeSelector
// checks: a resource that selects nothing, and nodes selected by nothing.
func TestRunValidate_NodeSelectorChecks(t *testing.T) {
	node := makeNode("node-a", map[string]string{"role": "worker"})
	lbna := makeLBNodeAgent("edge-only", map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"role": "edge"},
		},
		"local": map[string]interface{}{"localInterface": "default"},
	})

	c := newFakeClients([]runtime.Object{node}, lbna)

	out, err := captureOutput(t, func() error {
		return runValidate(context.Background(), c, outputTable, false)
	})
	require.NoError(t, err)

	assert.Contains(t, out, "nodeSelector matches 0 of 1 nodes")
	assert.Contains(t, out, "selected by no LBNodeAgent")
}

// A specific selector overriding the catch-all is the documented
// default-plus-override pattern. It must report which resource governs the
// node WITHOUT warning — otherwise a correct configuration could never
// validate clean and --strict would fail CI on it.
func TestRunValidate_ScopedOverrideIsNotAWarning(t *testing.T) {
	node := makeNode("node-a", map[string]string{"role": "edge"})
	catchAll := makeLBNodeAgent("aaa-catchall", map[string]interface{}{
		"local": map[string]interface{}{"localInterface": "default"},
	})
	scoped := makeLBNodeAgent("zzz-scoped", map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"role": "edge"},
		},
		"local": map[string]interface{}{"localInterface": "default"},
	})

	c := newFakeClients([]runtime.Object{node}, catchAll, scoped)

	out, err := captureOutput(t, func() error {
		return runValidate(context.Background(), c, outputTable, false)
	})
	require.NoError(t, err)

	assert.Contains(t, out, "using a scoped LBNodeAgent override")
	assert.Contains(t, out, "using purelb-system/zzz-scoped")
	assert.Contains(t, out, "over purelb-system/aaa-catchall")
	assert.NotContains(t, out, "resolved by name order")
	assert.Contains(t, out, "0 WARN")

	// --strict must also pass: this configuration is correct.
	_, err = captureOutput(t, func() error {
		return runValidate(context.Background(), c, outputTable, true)
	})
	assert.NoError(t, err, "--strict must not fail on the default-plus-override pattern")
}

// Two SPECIFIC selectors matching one node is genuinely ambiguous: the winner
// is decided only by namespace/name sort, so it stays a warning.
func TestRunValidate_ContestedNodeWarns(t *testing.T) {
	node := makeNode("node-a", map[string]string{"role": "edge", "tier": "front"})
	byRole := makeLBNodeAgent("aaa-by-role", map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"role": "edge"},
		},
		"local": map[string]interface{}{"localInterface": "default"},
	})
	byTier := makeLBNodeAgent("zzz-by-tier", map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"tier": "front"},
		},
		"local": map[string]interface{}{"localInterface": "default"},
	})

	c := newFakeClients([]runtime.Object{node}, byRole, byTier)

	out, err := captureOutput(t, func() error {
		return runValidate(context.Background(), c, outputTable, false)
	})
	require.NoError(t, err)

	assert.Contains(t, out, "matched by multiple specific nodeSelectors")
	assert.Contains(t, out, "resolved by name order")
	assert.Contains(t, out, "using purelb-system/aaa-by-role")
}

// TestRunValidate_LocalInterfaceRegex covers the documented footgun: matching
// is unanchored, so "eth" also selects veth interfaces.
func TestRunValidate_LocalInterfaceRegex(t *testing.T) {
	node := makeNode("node-a", nil)
	lbna := makeLBNodeAgent("default", map[string]interface{}{
		"local": map[string]interface{}{"localInterface": "eth"},
	})

	c := newFakeClients([]runtime.Object{node}, lbna)

	out, err := captureOutput(t, func() error {
		return runValidate(context.Background(), c, outputTable, false)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "unanchored")
	assert.Contains(t, out, "^eth$")
}

// An invalid regex is a hard failure: the agent rejects the config and the
// selected nodes announce nothing.
func TestRunValidate_InvalidLocalInterface(t *testing.T) {
	node := makeNode("node-a", nil)
	lbna := makeLBNodeAgent("default", map[string]interface{}{
		"local": map[string]interface{}{"localInterface": "["},
	})

	c := newFakeClients([]runtime.Object{node}, lbna)

	out, err := captureOutput(t, func() error {
		return runValidate(context.Background(), c, outputTable, false)
	})
	assert.Error(t, err, "an invalid localInterface must fail validation")
	assert.Contains(t, out, "not a valid regex")
}
