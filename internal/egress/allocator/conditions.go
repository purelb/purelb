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

package allocator

import (
	"fmt"
)

// Condition types (§4.8). Every one is True when healthy.
const (
	condAllocated       = "Allocated"
	condContained       = "Contained"
	condAnnounced       = "Announced"
	condDatapathReady   = "DatapathReady"
	condPortCapacity    = "PortCapacity"
	condPolicyEffective = "PolicyEffective"
	condUnderlay        = "UnderlayEncrypted"
	condReady           = "Ready"
)

// cond is one condition's outcome. A zero reason means True.
type cond struct {
	reason, message string
}

// conditions collects a reconcile's outcome per condition type; status
// publishes it.
type conditions map[string]cond

// fail sets a condition False, keeping the first failure reported.
func (c conditions) fail(typ, reason, format string, args ...interface{}) {
	if _, set := c[typ]; set {
		return
	}
	c[typ] = cond{reason: reason, message: fmt.Sprintf(format, args...)}
}

// ok reports whether typ has no failure.
func (c conditions) ok(typ string) bool {
	_, failed := c[typ]
	return !failed
}
