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

	purelbv1 "purelb.io/pkg/apis/v1"
)

// addIngress adds "address" to the Spec.Ingress field of "svc".
func addIngress(log log.Logger, svc *v1.Service, address net.IP) {
	var ingress []v1.LoadBalancerIngress = svc.Status.LoadBalancer.Ingress

	// We program the service differently depending on where the address
	// came from.
	//
	// If it's a locally-allocated address then we add it to the
	// LoadBalancerIngress.IP field which in ipvs mode will cause
	// kube-proxy to add the address to the kube-ipvs0 bridge
	// interface. This is needed for proper packet forwarding.
	//
	// If the address was allocated from the Acnodal EPIC, though, then
	// it should not be added to kube-ipvs0 because that would interfere
	// with proper packet forwarding to the endpoints after the packets
	// have been decapsulated. To do that we add the address to the
	// LoadBalancerIngress.Hostname field. This isn't documented well
	// but it's also done by cloud providers.
	//
	// More info: https://github.com/kubernetes/kubernetes/pull/79976
	if hostName, hasAnnotation := svc.Annotations[purelbv1.HostnameAnnotation]; hasAnnotation {
		ingress = append(ingress, v1.LoadBalancerIngress{Hostname: hostName})
		log.Log("event", "programmed ingress", "dest", "hostname", "hostname", hostName)
	} else {
		ingress = append(ingress, v1.LoadBalancerIngress{IP: address.String()})
		log.Log("event", "programmed ingress", "dest", "IP", "IP", address.String())
	}

	svc.Status.LoadBalancer.Ingress = ingress
}

// parseIngress parses the contents of a service Spec.Ingress
// field. The contents can be either a hostname or an IP address. If
// it's an IP then we'll return that, but if it's a hostname then it
// was formatted by EPIC and we probably can't get the IP address from
// it. The returned IP will be valid only if it is not nil.
func parseIngress(log log.Logger, raw v1.LoadBalancerIngress) net.IP {
	// This is the easy case. It's an IP address so net.ParseIP will do
	// the work for us.
	if ip := net.ParseIP(raw.IP); ip != nil {
		return ip
	}

	return nil
}
