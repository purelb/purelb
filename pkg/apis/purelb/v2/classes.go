// Copyright 2020 Acnodal Inc.
// Copyright 2024 Acnodal Inc.
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

package v2

const (
	// ServiceLBClass is the Service.spec.loadBalancerClass value that
	// PureLB v2 responds to. This is different from v1 ("purelb.io/purelb")
	// to allow side-by-side deployment of v1 and v2 during migration.
	// Any other value, if set, will cause PureLB to ignore that Service.
	// If loadBalancerClass is *not* set then PureLB will respond to the service.
	ServiceLBClass string = "purelb.io/purelbv2"
)
