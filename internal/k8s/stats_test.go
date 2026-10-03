// Copyright 2020-2026 Acnodal Inc.
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

package k8s

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsAddr(t *testing.T) {
	assert.Equal(t, ":7472", metricsAddr("", 7472), "empty host listens on every address")
	assert.Equal(t, "172.30.250.104:7472", metricsAddr("172.30.250.104", 7472))
	assert.Equal(t, "[2001:470:b8f3:250::104]:7472", metricsAddr("2001:470:b8f3:250::104", 7472),
		"an IPv6 host must be bracketed")
}

// TestMetricsAddrBindsIPv6 proves the address actually listens on an IPv6
// host; the unbracketed form failed with "too many colons in address".
func TestMetricsAddrBindsIPv6(t *testing.T) {
	// Probe IPv6 availability independently, so the skip can't swallow
	// the very "too many colons" failure this test exists to catch.
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	_ = probe.Close()

	ln, err := net.Listen("tcp", metricsAddr("::1", 0))
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	_ = conn.Close()
}
