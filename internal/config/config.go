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

package config

import (
	"fmt"
	"net"
	"strings"

	"github.com/mikioh/ipaddr"

	purelbv1 "purelb.io/pkg/apis/v1"
)

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

	SubnetV4    string
	Aggregation string
}

func ParseServiceGroups(groups []*purelbv1.ServiceGroup) (*Config, error) {
	cfg := &Config{Pools: map[string]*Pool{}}

	var allCIDRs []*net.IPNet
	for i, group := range groups {
		pool, err := parseGroup(group.Name, group.Spec)
		if err != nil {
			return nil, fmt.Errorf("parsing address pool #%d: %s", i+1, err)
		}

		// Check that the pool isn't already defined
		if cfg.Pools[group.Name] != nil {
			return nil, fmt.Errorf("duplicate definition of pool %q", group.Name)
		}

		// Check that all specified CIDR ranges are non-overlapping.
		for _, cidr := range pool.CIDR {
			for _, m := range allCIDRs {
				if cidrsOverlap(cidr, m) {
					return nil, fmt.Errorf("CIDR %q in pool %q overlaps with already defined CIDR %q", cidr, group.Name, m)
				}
			}
			allCIDRs = append(allCIDRs, cidr)
		}

		cfg.Pools[group.Name] = pool
	}

	return cfg, nil
}

func parseGroup(name string, group purelbv1.ServiceGroupSpec) (*Pool, error) {
	ret := &Pool{
		AutoAssign:  true,
	}

	if group.Local != nil {
		nets, err := parseCIDR(group.Local.Pool)
		if err != nil {
			return nil, err
		}
		ret.CIDR = append(ret.CIDR, nets...)
		ret.SubnetV4 = group.Local.SubnetV4
		ret.Aggregation = group.Local.Aggregation
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
