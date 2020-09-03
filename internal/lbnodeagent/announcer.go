// Copyright 2020 Acnodal Inc.
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
package lbnodeagent

import (
	"net"

	v1 "k8s.io/api/core/v1"
	"purelb.io/internal/config"
	"purelb.io/internal/election"
)

// Announces service IP addresses
type Announcer interface {
	SetConfig(*config.Config) error
	SetBalancer(string, net.IP, string) error
	DeleteBalancer(string, string) error
	SetNode(*v1.Node) error
	SetElection(*election.Election)
}
