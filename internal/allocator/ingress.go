// Copyright 2021 Acnodal Inc.
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
	"net"

	"github.com/go-kit/kit/log"
	v1 "k8s.io/api/core/v1"
)

// addIngress adds "address" to the Spec.Ingress field of "svc".
func addIngress(log log.Logger, svc *v1.Service, address net.IP) {
	svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress, v1.LoadBalancerIngress{IP: address.String()})
	log.Log("op", "program ingress address", "dest", "IP", "address", address.String())
}
