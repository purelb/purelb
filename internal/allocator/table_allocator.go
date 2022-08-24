// Copyright 2017 Google Inc.
// Copyright 2020,2021 Acnodal Inc.
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

package allocator

import (
	"strconv"

	"github.com/go-kit/kit/log"
	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"
)

// An Allocator tracks IP address pools and allocates addresses from them.
type TableAllocator struct {
	client k8s.ServiceEvent
	logger log.Logger
	index  int
}

// New returns an Allocator managing no pools.
func NewTableAllocator(log log.Logger) *TableAllocator {
	return &TableAllocator{
		logger: log,
	}
}

func (a *TableAllocator) NextTableIndex() int {
	// FIXME: critical region
	a.index++
	return a.index
}

// SetClient sets this Allocator's client field.
func (a *TableAllocator) SetClient(client k8s.ServiceEvent) {
	a.client = client
}

// SetPools updates the set of address pools that the allocator owns.
func (a *TableAllocator) SetAgents(agents []*purelbv1.LBNodeAgent) error {
	for _, agent := range agents {
		if agent != nil && agent.Spec.Egress != nil {
			a.index = agent.Spec.Egress.RouteTableBase
			a.logger.Log("event", "setTableIndex", "index", a.index)
			return nil
		}
	}
	return nil
}

// NotifyExisting notifies the allocator of an existing IP assignment,
// for example, at startup time.
func (a *TableAllocator) NotifyExisting(svc *v1.Service) error {

	// If this service has a higher table index than our current
	// high-water mark, then raise the high-water mark to match.
	if rawTableKey, hasTableKey := svc.Annotations[purelbv1.RouteTableAnnotation]; hasTableKey {
		tableKey, err := strconv.Atoi(rawTableKey)
		if err != nil {
			return err
		}
		if tableKey > a.index {
			a.index = tableKey
			a.logger.Log("event", "setTableIndex", "index", a.index)
		}
	}
	return nil
}

// Unassign frees the IP associated with service, if any.
func (a *TableAllocator) Unassign(svc string) error {
	return nil
}
