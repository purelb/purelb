// Copyright 2020-2026 Acnodal Inc.
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

package v2

// Config holds the runtime configuration that is passed to components
// like the announcer via SetConfig(). It aggregates all ServiceGroups
// and LBNodeAgents from the cluster.
type Config struct {
	// DefaultAnnouncer indicates whether there's at least one LBNodeAgent
	// configured, enabling announcement of service addresses.
	DefaultAnnouncer bool

	// Groups contains all ServiceGroup resources from the cluster.
	Groups []*ServiceGroup

	// Agents contains all LBNodeAgent resources from the cluster.
	Agents []*LBNodeAgent

	// DroppedGroups contains ServiceGroups that were listed but excluded
	// from Groups, so that consumers can report them. They are carried
	// rather than discarded because the filtering happens in a code path
	// shared by both binaries, while the events belong to exactly one of
	// them -- emitting from the shared path would produce one event per
	// node per reconcile.
	DroppedGroups []*ServiceGroup
}
