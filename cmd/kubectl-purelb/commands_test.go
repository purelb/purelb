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
		core:    fake.NewSimpleClientset(coreObjects...),
		dynamic: dynClient,
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
	healthy := makeLease("node-a", "10.0.0.0/24", 2) // renewed 2s ago, dur=10s, not expired
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
