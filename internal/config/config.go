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

// Package "config" provides code for parsing and validating
// configuration data.
package config

import (
	"fmt"

	purelbv1 "purelb.io/pkg/apis/v1"
)

// Config is a parsed and validated PureLB configuration.
type Config struct {
	// Address pools from which to allocate load balancer IP addresses.
	Pools map[string]Pool
}

func ParseServiceGroups(groups []*purelbv1.ServiceGroup) (*Config, error) {
	cfg := &Config{Pools: map[string]Pool{}}

	for i, group := range groups {
		pool, err := parseGroup(group.Name, group.Spec)
		if err != nil {
			return nil, fmt.Errorf("parsing address pool #%d: %s", i+1, err)
		}

		// Check that the pool isn't already defined
		if cfg.Pools[group.Name] != nil {
			return nil, fmt.Errorf("duplicate definition of pool %q", group.Name)
		}

		// Check that this pool doesn't overlap with any of the previous
		// ones
		for name, r := range cfg.Pools {
			if pool.Overlaps(r) {
				return nil, fmt.Errorf("pool %q overlaps with already defined pool %q", group.Name, name)
			}
		}

		cfg.Pools[group.Name] = pool
	}

	return cfg, nil
}

func parseGroup(name string, group purelbv1.ServiceGroupSpec) (Pool, error) {
	if group.Local != nil {
		nets, err := parseCIDR(group.Local.Pool)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	} else if group.EGW != nil {
		ret, err := NewEGWPool(true, group.EGW.URL, group.EGW.Aggregation)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	}

	return nil, fmt.Errorf("Pool is not local or EGW")
}
