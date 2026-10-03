// Copyright 2020-2026 Acnodal Inc.
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

package election

import (
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	purelbv2 "purelb.io/pkg/apis/purelb/v2"
)

func localAgent(name string, spec *purelbv2.LBNodeAgentLocalSpec) *purelbv2.LBNodeAgent {
	return &purelbv2.LBNodeAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "purelb", Name: name},
		Spec:       purelbv2.LBNodeAgentSpec{Local: spec},
	}
}

// loHasIPv6 reports whether the loopback interface carries ::1 —
// some CI runners disable IPv6 entirely.
func loHasIPv6(t *testing.T) bool {
	t.Helper()
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		return false
	}
	addrs, err := lo.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() == nil && ipnet.IP.IsLoopback() {
			return true
		}
	}
	return false
}

func TestSelectorFromConfig(t *testing.T) {
	t.Run("no local agent returns nil", func(t *testing.T) {
		sel, err := SelectorFromConfig(&purelbv2.Config{})
		assert.NoError(t, err)
		assert.Nil(t, sel)

		sel, err = SelectorFromConfig(&purelbv2.Config{
			Agents: []*purelbv2.LBNodeAgent{localAgent("remote-only", nil)},
		})
		assert.NoError(t, err)
		assert.Nil(t, sel)
	})

	t.Run("default", func(t *testing.T) {
		sel, err := SelectorFromConfig(&purelbv2.Config{
			Agents: []*purelbv2.LBNodeAgent{localAgent("default", &purelbv2.LBNodeAgentLocalSpec{
				LocalInterface: "default", DummyInterface: "kube-lb0",
			})},
		})
		require.NoError(t, err)
		require.NotNil(t, sel)
		assert.True(t, sel.UseDefault)
		assert.Nil(t, sel.Regex)
		assert.Equal(t, "kube-lb0", sel.ExcludeName)
	})

	t.Run("empty localInterface treated as default", func(t *testing.T) {
		sel, err := SelectorFromConfig(&purelbv2.Config{
			Agents: []*purelbv2.LBNodeAgent{localAgent("blank", &purelbv2.LBNodeAgentLocalSpec{})},
		})
		require.NoError(t, err)
		require.NotNil(t, sel)
		assert.True(t, sel.UseDefault)
		assert.Nil(t, sel.Regex)
	})

	t.Run("regex with field carry-through", func(t *testing.T) {
		sel, err := SelectorFromConfig(&purelbv2.Config{
			Agents: []*purelbv2.LBNodeAgent{localAgent("regex", &purelbv2.LBNodeAgentLocalSpec{
				LocalInterface: "^(eth1|eth2)$",
				DummyInterface: "kube-lb0",
				Interfaces:     []string{"eth3"},
			})},
		})
		require.NoError(t, err)
		require.NotNil(t, sel)
		assert.False(t, sel.UseDefault)
		require.NotNil(t, sel.Regex)
		assert.True(t, sel.Regex.MatchString("eth1"))
		assert.False(t, sel.Regex.MatchString("eth10"))
		assert.Equal(t, []string{"eth3"}, sel.Interfaces)
		assert.Equal(t, "kube-lb0", sel.ExcludeName)
	})

	t.Run("invalid regex errors", func(t *testing.T) {
		sel, err := SelectorFromConfig(&purelbv2.Config{
			Agents: []*purelbv2.LBNodeAgent{localAgent("broken", &purelbv2.LBNodeAgentLocalSpec{
				LocalInterface: "[",
			})},
		})
		assert.Error(t, err)
		assert.Nil(t, sel)
	})

	t.Run("first local agent wins", func(t *testing.T) {
		sel, err := SelectorFromConfig(&purelbv2.Config{
			Agents: []*purelbv2.LBNodeAgent{
				localAgent("remote-only", nil),
				localAgent("first", &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "^eth1$"}),
				localAgent("second", &purelbv2.LBNodeAgentLocalSpec{LocalInterface: "^eth2$"}),
			},
		})
		require.NoError(t, err)
		require.NotNil(t, sel)
		assert.True(t, sel.Regex.MatchString("eth1"))
		assert.False(t, sel.Regex.MatchString("eth2"))
	})
}

func TestGetSelectedSubnets(t *testing.T) {
	t.Run("nil selector identical to default detection", func(t *testing.T) {
		fromDefault, errDefault := GetLocalSubnets([]string{}, true, nil)
		fromNil, errNil := GetSelectedSubnets(nil, nil)
		assert.Equal(t, errDefault == nil, errNil == nil)
		assert.Equal(t, fromDefault, fromNil)
	})

	t.Run("empty selector selects nothing", func(t *testing.T) {
		subnets, err := GetSelectedSubnets(&InterfaceSelector{}, nil)
		assert.NoError(t, err)
		assert.Empty(t, subnets)
	})

	t.Run("explicit lo", func(t *testing.T) {
		subnets, err := GetSelectedSubnets(&InterfaceSelector{Interfaces: []string{"lo"}}, nil)
		require.NoError(t, err)
		assert.Contains(t, subnets, "127.0.0.0/8")
		if loHasIPv6(t) {
			assert.Contains(t, subnets, "::1/128")
		}
	})

	t.Run("regex enumeration excludes loopback", func(t *testing.T) {
		subnets, err := GetSelectedSubnets(&InterfaceSelector{Regex: regexp.MustCompile("^lo$")}, nil)
		require.NoError(t, err)
		assert.NotContains(t, subnets, "127.0.0.0/8")
	})

	t.Run("explicit name beats ExcludeName only for non-dummy", func(t *testing.T) {
		// lo listed explicitly but also excluded: contributes nothing.
		subnets, err := GetSelectedSubnets(&InterfaceSelector{
			Interfaces: []string{"lo"}, ExcludeName: "lo",
		}, nil)
		require.NoError(t, err)
		assert.NotContains(t, subnets, "127.0.0.0/8")
	})

	t.Run("missing interface skipped", func(t *testing.T) {
		subnets, err := GetSelectedSubnets(&InterfaceSelector{
			Interfaces: []string{"purelb-does-not-exist0"},
		}, nil)
		assert.NoError(t, err)
		assert.Empty(t, subnets)
	})

	t.Run("union and dedup", func(t *testing.T) {
		fromSingle, err := GetSelectedSubnets(&InterfaceSelector{Interfaces: []string{"lo"}}, nil)
		require.NoError(t, err)
		fromDuplicated, err := GetSelectedSubnets(&InterfaceSelector{Interfaces: []string{"lo", "lo"}}, nil)
		require.NoError(t, err)
		assert.Equal(t, fromSingle, fromDuplicated)
	})
}

func TestSelectedInterfaceNames(t *testing.T) {
	t.Run("sorted deterministic order", func(t *testing.T) {
		names := selectedInterfaceNames(&InterfaceSelector{
			Interfaces: []string{"zzz0", "aaa0", "mmm0"},
		}, log.NewNopLogger())
		assert.Equal(t, []string{"aaa0", "mmm0", "zzz0"}, names)
	})

	t.Run("empty names dropped", func(t *testing.T) {
		names := selectedInterfaceNames(&InterfaceSelector{Interfaces: []string{"", "eth1"}}, log.NewNopLogger())
		assert.Equal(t, []string{"eth1"}, names)
	})
}

func TestMatchInterfaceNames(t *testing.T) {
	names := []string{"eth1", "eth2", "veth0abc", "kube-lb0", "docker0"}

	t.Run("unanchored regex matches substrings", func(t *testing.T) {
		matched := matchInterfaceNames(regexp.MustCompile("eth"), "", names)
		assert.Equal(t, []string{"eth1", "eth2", "veth0abc"}, matched)
	})

	t.Run("anchored regex is exact", func(t *testing.T) {
		matched := matchInterfaceNames(regexp.MustCompile("^(eth1|eth2)$"), "", names)
		assert.Equal(t, []string{"eth1", "eth2"}, matched)
	})

	t.Run("exclude removes match", func(t *testing.T) {
		matched := matchInterfaceNames(regexp.MustCompile("."), "kube-lb0", names)
		assert.NotContains(t, matched, "kube-lb0")
		assert.Contains(t, matched, "eth1")
	})
}

func TestParseSubnetsAnnotationHardening(t *testing.T) {
	t.Run("invalid entries dropped, originals kept verbatim", func(t *testing.T) {
		parsed := purelbv2.ParseSubnetsAnnotation("192.168.1.0/24,not-a-cidr,fd53:9ef0:8683::/64,,300.1.1.0/24")
		assert.Equal(t, []string{"192.168.1.0/24", "fd53:9ef0:8683::/64"}, parsed)
	})

	t.Run("capped at purelbv2.MaxAnnotationSubnets", func(t *testing.T) {
		entries := make([]string, purelbv2.MaxAnnotationSubnets+10)
		for i := range entries {
			entries[i] = "10.0.0.0/8"
		}
		assert.Len(t, purelbv2.ParseSubnetsAnnotation(strings.Join(entries, ",")), purelbv2.MaxAnnotationSubnets)
	})
}
