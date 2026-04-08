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
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"golang.org/x/sync/errgroup"
)

// clusterSnapshot holds a point-in-time copy of the cluster resources needed
// by the dashboard. It is raw data only — derived values (e.g. dummy interface
// name, healthy node set) are computed by render functions, not stored here.
type clusterSnapshot struct {
	pods            *v1.PodList
	services        *v1.ServiceList
	serviceGroups   *unstructured.UnstructuredList
	leases          *coordinationv1.LeaseList
	bgpNodeStatuses *unstructured.UnstructuredList
	lbNodeAgents    *unstructured.UnstructuredList
}

// fetchSnapshot fetches all resources needed by the dashboard in parallel.
// Each goroutine writes a distinct field of the snapshot struct, so no
// synchronization is needed beyond the errgroup.Wait barrier. If any
// single fetch fails, the entire snapshot fails (fail-fast).
func fetchSnapshot(ctx context.Context, c *clients) (*clusterSnapshot, error) {
	snap := &clusterSnapshot{}
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		var err error
		snap.pods, err = c.core.CoreV1().Pods(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
		if err != nil {
			return fmt.Errorf("listing pods: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		snap.services, err = c.core.CoreV1().Services("").List(ctx, metav1.ListOptions{
			ResourceVersion: "0",
			FieldSelector:   svcFieldSelector,
		})
		if err != nil {
			return fmt.Errorf("listing services: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		snap.serviceGroups, err = c.dynamic.Resource(gvrServiceGroups).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
		if err != nil {
			return fmt.Errorf("listing ServiceGroups: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		snap.leases, err = c.core.CoordinationV1().Leases(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
		if err != nil {
			return fmt.Errorf("listing leases: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		snap.bgpNodeStatuses, err = c.dynamic.Resource(gvrBGPNodeStatuses).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
		if err != nil {
			return fmt.Errorf("listing BGPNodeStatuses: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		snap.lbNodeAgents, err = c.dynamic.Resource(gvrLBNodeAgents).Namespace(purelbNamespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
		if err != nil {
			return fmt.Errorf("listing LBNodeAgents: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return snap, nil
}
