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

package cnisock

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// PluginType is the plugin's binary name and its conflist entry's type.
const PluginType = "purelb-egress"

// DesiredPath is the agent's desired state for the purelb-cni installer
// (D34). It is on disk, so after a reboot the installer re-inserts the
// chain entry before the agent runs, and the plugin decides by the
// snapshot meanwhile.
const DesiredPath = "/var/lib/purelb/cni-desired.json"

// StatusPath is the installer's report to the agent; the installer
// rewrites it at least every StatusRefresh.
const StatusPath = "/run/purelb/cni-status.json"

// StatusRefresh is the longest the installer leaves its status unwritten.
const StatusRefresh = 30 * time.Second

// DesiredVersion and StatusVersion are the formats this build writes and
// reads.
const (
	DesiredVersion = 1
	StatusVersion  = 1
)

// Desired is what the agent wants in the CNI directories.
type Desired struct {
	Version int  `json:"version"`
	Enabled bool `json:"enabled"`
	// Entry is the conflist entry the installer keeps in the chain.
	Entry json.RawMessage `json:"entry,omitempty"`
}

// PluginEntry is the conflist entry for an agent in namespace logging at
// logLevel.
func PluginEntry(namespace, logLevel string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Type            string `json:"type"`
		PureLBNamespace string `json:"purelbNamespace"`
		LogLevel        string `json:"logLevel"`
	}{PluginType, namespace, logLevel})
	return b
}

// The chain states the installer reports.
const (
	// ChainPresent: the primary conflist's chain holds the plugin.
	ChainPresent = "Present"
	// ChainMissing: it doesn't; Reason says why.
	ChainMissing = "Missing"
	// ChainOff: the agent wants nothing (egress off), so nothing is checked.
	ChainOff = "Off"
)

// Reasons the chain is missing. WriteWar and NotAConflist are ones the
// installer won't fix (CNIChainMissing); the others it retries.
const (
	ReasonWriteWar     = "WriteWar"
	ReasonNotAConflist = "NotAConflist"
	ReasonNoConflist   = "NoConflist"
	ReasonFailed       = "Failed"
)

// InstallerStatus is the installer's latest finding.
type InstallerStatus struct {
	Version int       `json:"version"`
	Written time.Time `json:"written"`
	Chain   string    `json:"chain"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	// Conflist is the primary CNI configuration file it edits.
	Conflist string `json:"conflist,omitempty"`
}

// WriteDesired replaces the desired state at path whole.
func WriteDesired(path string, d Desired) error {
	d.Version = DesiredVersion
	return writeJSON(path, d)
}

// ReadDesired reads the desired state at path.
func ReadDesired(path string) (Desired, error) {
	var d Desired
	return d, readJSON(path, &d, &d.Version, DesiredVersion)
}

// WriteStatus replaces the installer status at path whole.
func WriteStatus(path string, s InstallerStatus) error {
	s.Version = StatusVersion
	return writeJSON(path, s)
}

// ReadStatus reads the installer status at path.
func ReadStatus(path string) (InstallerStatus, error) {
	var s InstallerStatus
	return s, readJSON(path, &s, &s.Version, StatusVersion)
}

// readJSON reads path into v, refusing a version other than want.
func readJSON(path string, v any, version *int, want int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if *version != want {
		return fmt.Errorf("%s is version %d; this build reads %d", path, *version, want)
	}
	return nil
}
