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

package allocator

import (
	"fmt"

	"purelb.io/internal/egresshook"
	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

// SetEgress sets the egress hook. Call it before the allocator runs; the
// default is egresshook.Noop.
func (a *Allocator) SetEgress(h egresshook.Allocator) {
	a.egress = h
}

// EgressGroup returns the ServiceGroup, and its pool, that an egress
// policy in namespace ns uses: group when set, else the namespace's
// default group, as for an unannotated Service (§5.2). The same namespace
// rules as Services apply, and one more: a group listing namespaces
// serves egress only in those, enforcing or not. The ServiceGroup is the
// allocator's snapshot copy; don't modify it.
func (a *Allocator) EgressGroup(ns, group string) (*purelbv2.ServiceGroup, Pool, error) {
	st := a.Snapshot()
	if group == "" {
		b, bound := st.nsToPools[ns]
		switch {
		case bound && b.def != "":
			group = b.def
		case bound && b.enforce:
			return nil, nil, fmt.Errorf("%w: %s", ErrAmbiguousDefault, b.defErr)
		default:
			group = defaultPoolName
		}
	}
	pool, err := st.checked(ns, group)
	if err != nil {
		return nil, nil, err
	}
	sg := st.sgByName[group]
	if len(sg.Spec.Namespaces) > 0 && !purelbv2.ServesNamespace(sg, ns) {
		return nil, nil, fmt.Errorf("%w: ServiceGroup %q doesn't list namespace %q", ErrNamespaceDenied, group, ns)
	}
	return sg, pool, nil
}

// ServiceGroup returns the named ServiceGroup and its pool, if both
// exist. The ServiceGroup is the allocator's snapshot copy; don't modify
// it.
func (a *Allocator) ServiceGroup(name string) (*purelbv2.ServiceGroup, Pool, bool) {
	st := a.Snapshot()
	sg, ok := st.sgByName[name]
	if !ok {
		return nil, nil, false
	}
	pool, ok := st.pools[name]
	return sg, pool, ok
}
