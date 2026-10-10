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

// Package kernelversion parses Linux kernel release strings. It has no
// dependencies so both the node agent and the kubectl plugin (which is
// built for macOS and Windows too) can use it.
package kernelversion

import (
	"fmt"
	"strconv"
	"strings"
)

// AddressGuardMin is the oldest kernel the address guard runs on: tcx
// (TC ingress via bpf_link) arrived in 6.6.
var AddressGuardMin = Version{6, 6}

// Version is a kernel major.minor.
type Version struct{ Major, Minor int }

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// AtLeast reports whether v is min or newer.
func (v Version) AtLeast(min Version) bool {
	return v.Major > min.Major || (v.Major == min.Major && v.Minor >= min.Minor)
}

// Parse extracts major.minor from a release string such as
// "6.12.107+deb13-amd64" or "5.15.0-91-generic".
func Parse(release string) (Version, error) {
	fields := strings.SplitN(release, ".", 3)
	if len(fields) < 2 {
		return Version{}, fmt.Errorf("unrecognised kernel release %q", release)
	}
	major, err := strconv.Atoi(fields[0])
	if err != nil {
		return Version{}, fmt.Errorf("unrecognised kernel release %q", release)
	}
	// The minor may run straight into a suffix ("6.6-rc1", "6.6+deb").
	minorDigits := fields[1]
	for i, r := range minorDigits {
		if r < '0' || r > '9' {
			minorDigits = minorDigits[:i]
			break
		}
	}
	minor, err := strconv.Atoi(minorDigits)
	if err != nil {
		return Version{}, fmt.Errorf("unrecognised kernel release %q", release)
	}
	return Version{major, minor}, nil
}
