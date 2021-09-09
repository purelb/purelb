// Copyright 2020 Acnodal Inc.
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

package v1

// Config is a container for our CRDs.  It's used to notify the app
// when any configuration changes.  When we're notified that any
// custom resource has changed, we read all of our resources, load
// them into a Config struct, and pass it to the controllers.
type Config struct {
	// MyCluster identifies the cluster in which PureLB is running
	MyCluster string

	// If true, then PureLB will handle Services when there's no
	// Spec.LoadBalancerClass, i.e., PureLB will be the default
	// announcer.
	DefaultAnnouncer bool

	// Service Groups from which to allocate load balancer IP addresses
	Groups []*ServiceGroup
	// Node agent configurations
	Agents []*LBNodeAgent
}
