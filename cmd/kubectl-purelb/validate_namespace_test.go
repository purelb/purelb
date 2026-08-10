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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// sgInNamespace is makeSG placed somewhere other than the install namespace.
func sgInNamespace(namespace, name, poolType string) *unstructured.Unstructured {
	sg := makeSG(name, poolType, "192.168.1.0-192.168.1.10", "192.168.1.0/24")
	sg.SetNamespace(namespace)
	return sg
}

// withNamespaces adds the binding fields to a ServiceGroup.
func withNamespaces(sg *unstructured.Unstructured, namespaces []string, isDefault bool) *unstructured.Unstructured {
	spec := sg.Object["spec"].(map[string]interface{})
	nss := make([]interface{}, 0, len(namespaces))
	for _, n := range namespaces {
		nss = append(nss, n)
	}
	spec["namespaces"] = nss
	spec["namespaceDefault"] = isDefault
	return sg
}

// validateOutput runs validate against a fake cluster and captures what it
// prints. runValidate writes the table straight to os.Stdout, so the pipe is
// the only way to assert on the operator-visible text -- which is the whole
// point of these checks.
func validateOutput(t *testing.T, objs ...runtime.Object) string {
	t.Helper()
	c := newFakeClients(nil, objs...)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	_ = runValidate(context.Background(), c, outputTable, false)
	os.Stdout = orig
	w.Close()

	var sb strings.Builder
	if _, err := io.Copy(&sb, r); err != nil {
		t.Fatalf("read captured output: %v", err)
	}
	return sb.String()
}

// TestValidateFlagsOutOfNamespaceServiceGroup: this check is PR C's only
// pre-upgrade gate, and a namespace-scoped list cannot see what it must flag.
// A remote group is called out specifically because ignoring one withdraws
// its addresses from every node, where a local one only makes its pool
// unallocatable.
func TestValidateFlagsOutOfNamespaceServiceGroup(t *testing.T) {
	out := validateOutput(t,
		makeSG("default", "local", "192.168.9.0-192.168.9.10", "192.168.9.0/24"),
		sgInNamespace("tenant-a", "stray", "remote"),
	)
	if !strings.Contains(out, "stray") || !strings.Contains(out, "tenant-a") {
		t.Fatalf("out-of-namespace ServiceGroup not reported:\n%s", out)
	}
	if !strings.Contains(out, "withdrawn from every node") {
		t.Fatalf("a remote group's consequence must be named, not generic:\n%s", out)
	}
}

// TestValidateAcceptsInstallNamespaceServiceGroup guards the false-positive
// direction: a correctly-placed ServiceGroup must not be flagged.
func TestValidateAcceptsInstallNamespaceServiceGroup(t *testing.T) {
	out := validateOutput(t, makeSG("default", "local", "192.168.9.0-192.168.9.10", "192.168.9.0/24"))
	if strings.Contains(out, "not the PureLB install namespace") {
		t.Fatalf("correctly-placed ServiceGroup was flagged:\n%s", out)
	}
}

// TestValidateFlagsAmbiguousNamespaceDefault: with enforcement off, an
// unresolved namespaceDefault is silent at allocation time -- the Service
// quietly lands on "default" -- so this check is the only signal an operator
// gets. One ServiceGroup serving a namespace must NOT be flagged.
func TestValidateFlagsAmbiguousNamespaceDefault(t *testing.T) {
	out := validateOutput(t,
		makeSG("default", "local", "192.168.9.0-192.168.9.10", "192.168.9.0/24"),
		withNamespaces(makeSG("a-l2", "local", "192.168.2.0-192.168.2.10", "192.168.2.0/24"), []string{"tenant-a"}, false),
		withNamespaces(makeSG("a-bgp", "remote", "192.168.3.0-192.168.3.10", "192.168.3.0/24"), []string{"tenant-a"}, false),
	)
	if !strings.Contains(out, "namespaceDefault") || !strings.Contains(out, "tenant-a") {
		t.Fatalf("ambiguous namespaceDefault not reported:\n%s", out)
	}

	// Exactly one marked resolves it.
	out = validateOutput(t,
		makeSG("default", "local", "192.168.9.0-192.168.9.10", "192.168.9.0/24"),
		withNamespaces(makeSG("a-l2", "local", "192.168.2.0-192.168.2.10", "192.168.2.0/24"), []string{"tenant-a"}, true),
		withNamespaces(makeSG("a-bgp", "remote", "192.168.3.0-192.168.3.10", "192.168.3.0/24"), []string{"tenant-a"}, false),
	)
	if strings.Contains(out, "exactly one must") {
		t.Fatalf("a resolved namespaceDefault must not be flagged:\n%s", out)
	}
}
