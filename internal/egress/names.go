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

// Package egress holds what the allocator's and the agents' egress code
// share: object names and labels, sidecar keys and the policy order.
package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
)

// Kinds of policy, as they appear in names, labels and sidecar keys.
const (
	KindEgress        = "Egress"
	KindClusterEgress = "ClusterEgress"
)

// Labels on EgressEndpoints and EgressMembers. The routed-pool nodes watch
// EgressEndpoints by LabelServiceGroup and LabelPool (D6).
const (
	LabelKind         = "purelb.io/egress-kind"
	LabelNamespace    = "purelb.io/egress-namespace"
	LabelName         = "purelb.io/egress-name"
	LabelUID          = "purelb.io/egress-uid"
	LabelServiceGroup = "purelb.io/egress-servicegroup"
	LabelPool         = "purelb.io/egress-pool"
)

// FirstSlicePort is the lowest port a slice may cover: the allocator plans
// slices from it and the datapath treats lower ports as no slice's.
const FirstSlicePort = 1024

// Finalizer is added to a policy after its first allocation (D5).
const Finalizer = "purelb.io/egress"

// PolicyRef names an Egress (Namespace set) or a ClusterEgress.
type PolicyRef struct {
	Kind      string
	Namespace string
	Name      string
}

func (p PolicyRef) String() string {
	if p.Kind == KindClusterEgress {
		return "clusteregress/" + p.Name
	}
	return "egress/" + p.Namespace + "/" + p.Name
}

// prefix is the start of the policy's object names: "e-<ns>.<name>" or
// "c-<name>". Namespaces and names are DNS labels, so the dot can't be
// ambiguous.
func (p PolicyRef) prefix() string {
	if p.Kind == KindClusterEgress {
		return "c-" + p.Name
	}
	return "e-" + p.Namespace + "." + p.Name
}

// EndpointsName is the EgressEndpoints name for one of the policy's
// addresses: the policy and a hash of the address.
func EndpointsName(p PolicyRef, addr netip.Addr) string {
	return p.prefix() + "-" + shortHash(addr.String())
}

// MembersName is the name of the policy's i'th EgressMembers.
func MembersName(p PolicyRef, i int) string {
	return fmt.Sprintf("%s-%d", p.prefix(), i)
}

// PoolLabelValue is the LabelPool value for a pool, given by its name, or
// its subnet when unnamed. Pool names may be 64 characters and subnets
// contain "/", neither of which a label value allows, so it is a hash.
func PoolLabelValue(pool string) string {
	return shortHash(pool)
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// SidecarKey is the key an external IPAM sidecar receives as
// AllocateRequest.service for one of the policy's allocations (D15):
// "egress:<kind>:<namespace or empty>:<name>", plus ":<identity>" for a
// PerPod address. Kubernetes names never contain a colon, so it can't
// collide with a Service's "namespace/name".
func SidecarKey(p PolicyRef, identity string) string {
	k := strings.Join([]string{"egress", p.Kind, p.Namespace, p.Name}, ":")
	if identity != "" {
		k += ":" + identity
	}
	return k
}
