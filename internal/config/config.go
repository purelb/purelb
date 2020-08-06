// Copyright 2017 Google Inc.
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

package config // import "purelb.io/internal/config"

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/mikioh/ipaddr"
	yaml "gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// configFile is the configuration as parsed out of the ConfigMap,
// without validation or useful high level types.
type configFile struct {
	Pools          []addressPool     `yaml:"address-pools"`
}

type nodeSelector struct {
	MatchLabels      map[string]string      `yaml:"match-labels"`
	MatchExpressions []selectorRequirements `yaml:"match-expressions"`
}

type selectorRequirements struct {
	Key      string   `yaml:"key"`
	Operator string   `yaml:"operator"`
	Values   []string `yaml:"values"`
}

type addressPool struct {
	Name              string
	Addresses         []string
	AvoidBuggyIPs     bool               `yaml:"avoid-buggy-ips"`
	AutoAssign        *bool              `yaml:"auto-assign"`
}

// Config is a parsed PureLB configuration.
type Config struct {
	// Address pools from which to allocate load balancer IPs.
	Pools map[string]*Pool
}

// Pool is the configuration of an IP address pool.
type Pool struct {
	// The addresses that are part of this pool, expressed as CIDR
	// prefixes. config.Parse guarantees that these are
	// non-overlapping, both within and between pools.
	CIDR []*net.IPNet
	// Some buggy consumer devices mistakenly drop IPv4 traffic for IP
	// addresses ending in .0 or .255, due to poor implementations of
	// smurf protection. This setting marks such addresses as
	// unusable, for maximum compatibility with ancient parts of the
	// internet.
	AvoidBuggyIPs bool
	// If false, prevents IP addresses to be automatically assigned
	// from this pool.
	AutoAssign bool
}

func parseNodeSelector(ns *nodeSelector) (labels.Selector, error) {
	if len(ns.MatchLabels)+len(ns.MatchExpressions) == 0 {
		return labels.Everything(), nil
	}

	// Convert to a metav1.LabelSelector so we can use the fancy
	// parsing function to create a Selector.
	//
	// Why not use metav1.LabelSelector in the raw config object?
	// Because metav1.LabelSelector doesn't have yaml tag
	// specifications.
	sel := &metav1.LabelSelector{
		MatchLabels: ns.MatchLabels,
	}
	for _, req := range ns.MatchExpressions {
		sel.MatchExpressions = append(sel.MatchExpressions, metav1.LabelSelectorRequirement{
			Key:      req.Key,
			Operator: metav1.LabelSelectorOperator(req.Operator),
			Values:   req.Values,
		})
	}

	return metav1.LabelSelectorAsSelector(sel)
}

func parseHoldTime(ht string) (time.Duration, error) {
	if ht == "" {
		return 90 * time.Second, nil
	}
	d, err := time.ParseDuration(ht)
	if err != nil {
		return 0, fmt.Errorf("invalid hold time %q: %s", ht, err)
	}
	rounded := time.Duration(int(d.Seconds())) * time.Second
	if rounded != 0 && rounded < 3*time.Second {
		return 0, fmt.Errorf("invalid hold time %q: must be 0 or >=3s", ht)
	}
	return rounded, nil
}

// Parse loads and validates a Config from bs.
func Parse(bs []byte) (*Config, error) {
	var raw configFile
	if err := yaml.UnmarshalStrict(bs, &raw); err != nil {
		return nil, fmt.Errorf("could not parse config: %s", err)
	}

	cfg := &Config{Pools: map[string]*Pool{}}
	var allCIDRs []*net.IPNet
	for i, p := range raw.Pools {
		pool, err := parseAddressPool(p)
		if err != nil {
			return nil, fmt.Errorf("parsing address pool #%d: %s", i+1, err)
		}

		// Check that the pool isn't already defined
		if cfg.Pools[p.Name] != nil {
			return nil, fmt.Errorf("duplicate definition of pool %q", p.Name)
		}

		// Check that all specified CIDR ranges are non-overlapping.
		for _, cidr := range pool.CIDR {
			for _, m := range allCIDRs {
				if cidrsOverlap(cidr, m) {
					return nil, fmt.Errorf("CIDR %q in pool %q overlaps with already defined CIDR %q", cidr, p.Name, m)
				}
			}
			allCIDRs = append(allCIDRs, cidr)
		}

		cfg.Pools[p.Name] = pool
	}

	return cfg, nil
}

func parseAddressPool(p addressPool) (*Pool, error) {
	if p.Name == "" {
		return nil, errors.New("missing pool name")
	}

	ret := &Pool{
		AvoidBuggyIPs: p.AvoidBuggyIPs,
		AutoAssign:    true,
	}

	if p.AutoAssign != nil {
		ret.AutoAssign = *p.AutoAssign
	}

	if len(p.Addresses) == 0 {
		return nil, errors.New("pool has no prefixes defined")
	}
	for _, cidr := range p.Addresses {
		nets, err := parseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in pool %q: %s", cidr, p.Name, err)
		}
		ret.CIDR = append(ret.CIDR, nets...)
	}

	return ret, nil
}

func parseCIDR(cidr string) ([]*net.IPNet, error) {
	if !strings.Contains(cidr, "-") {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", cidr)
		}
		return []*net.IPNet{n}, nil
	}

	fs := strings.SplitN(cidr, "-", 2)
	if len(fs) != 2 {
		return nil, fmt.Errorf("invalid IP range %q", cidr)
	}
	start := net.ParseIP(strings.TrimSpace(fs[0]))
	if start == nil {
		return nil, fmt.Errorf("invalid IP range %q: invalid start IP %q", cidr, fs[0])
	}
	end := net.ParseIP(strings.TrimSpace(fs[1]))
	if end == nil {
		return nil, fmt.Errorf("invalid IP range %q: invalid end IP %q", cidr, fs[1])
	}

	var ret []*net.IPNet
	for _, pfx := range ipaddr.Summarize(start, end) {
		n := &net.IPNet{
			IP:   pfx.IP,
			Mask: pfx.Mask,
		}
		ret = append(ret, n)
	}
	return ret, nil
}

func cidrsOverlap(a, b *net.IPNet) bool {
	return cidrContainsCIDR(a, b) || cidrContainsCIDR(b, a)
}

func cidrContainsCIDR(outer, inner *net.IPNet) bool {
	ol, _ := outer.Mask.Size()
	il, _ := inner.Mask.Size()
	if ol == il && outer.IP.Equal(inner.IP) {
		return true
	}
	if ol < il && outer.Contains(inner.IP) {
		return true
	}
	return false
}

func isIPv4(ip net.IP) bool {
	return ip.To16() != nil && ip.To4() != nil
}

func isIPv6(ip net.IP) bool {
	return ip.To16() != nil && ip.To4() == nil
}
