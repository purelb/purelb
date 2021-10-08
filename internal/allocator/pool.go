// Copyright 2017 Google Inc.
// Copyright 2020,2021 Acnodal Inc.
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
	"errors"
	"fmt"
	"net"

	v1 "k8s.io/api/core/v1"
)

// Port represents one port in use by a service.
type Port struct {
	Proto v1.Protocol
	Port  int
}

// String returns a text description of the port.
func (p Port) String() string {
	return fmt.Sprintf("%s/%d", p.Proto, p.Port)
}

type Key struct {
	Sharing string
}

// Pool describes the interface to code that manages pools of
// addresses.
type Pool interface {
	Available(net.IP, *v1.Service) error
	AssignNext(*v1.Service) (net.IP, error)
	Assign(net.IP, *v1.Service) error
	Release(string) error
	InUse() int
	Overlaps(Pool) bool
	Contains(net.IP) bool // FIXME: I'm not sure that we need this. It might be the case that we can always rely on the service's pool annotation to find to which pool an address belongs
	Size() uint64
}

func sharingOK(existing, new *Key) error {
	if existing.Sharing == "" {
		return errors.New("existing service does not allow sharing")
	}
	if new.Sharing == "" {
		return errors.New("new service does not allow sharing")
	}
	if existing.Sharing != new.Sharing {
		return fmt.Errorf("sharing key %q does not match existing sharing key %q", new.Sharing, existing.Sharing)
	}
	return nil
}
