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

package kernelversion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		release string
		want    Version
		guard   bool
	}{
		{"6.12.107+deb13-amd64", Version{6, 12}, true},
		{"5.15.0-91-generic", Version{5, 15}, false},
		{"6.6.0", Version{6, 6}, true},
		{"6.5.13-1-pve", Version{6, 5}, false},
		{"6.6-rc1", Version{6, 6}, true},
		{"4.18.0-513.el8.x86_64", Version{4, 18}, false},
		{"7.0", Version{7, 0}, true},
	} {
		v, err := Parse(c.release)
		require.NoError(t, err, c.release)
		assert.Equal(t, c.want, v, c.release)
		assert.Equal(t, c.guard, v.AtLeast(AddressGuardMin), c.release)
	}
	for _, bad := range []string{"", "6", "x.y.z", "6.x"} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}
}
