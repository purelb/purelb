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

package egress

import (
	"time"
)

// Rank is what orders overlapping policies.
type Rank struct {
	Priority int32
	Created  time.Time
	UID      string
}

// Before reports whether a ranks ahead of b: priority descending, then
// creation time ascending, then UID ascending. The allocator and the
// agents use this one order, so they agree which policy supplies a flow's
// slot (D24). ClusterEgress priorities start at 1000 and Egress ones end
// at 999, so every ClusterEgress ranks ahead of every Egress.
func Before(a, b Rank) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if !a.Created.Equal(b.Created) {
		return a.Created.Before(b.Created)
	}
	return a.UID < b.UID
}
