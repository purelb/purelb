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
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidateExitErrorIsFormatIndependent pins a bug the e2e coverage
// found: `validate -o json` reported failures and exited 0.
//
// The exit-code logic lived below an early `return printStructured(...)`,
// so it ran only on the table path. JSON is exactly what a pipeline
// consumes, so the gate did not gate for the people most likely to be
// depending on it -- and it failed in the quiet direction, reporting
// success.
func TestValidateExitErrorIsFormatIndependent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		summary validateSummary
		strict  bool
		wantErr bool
	}{
		{name: "clean", summary: validateSummary{Pass: 3}},
		{name: "a failure is an error", summary: validateSummary{Pass: 1, Fail: 1}, wantErr: true},
		{name: "a warning alone is not", summary: validateSummary{Pass: 1, Warn: 1}},
		{name: "a warning under --strict is", summary: validateSummary{Pass: 1, Warn: 1},
			strict: true, wantErr: true},
		{name: "a failure under --strict is", summary: validateSummary{Fail: 1},
			strict: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExitError(tc.summary, tc.strict)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
