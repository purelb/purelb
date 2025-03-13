// Copyright 2020 Acnodal, Inc.
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

package v1

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink/nl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceGroup is the top-level custom resource for configuring
// ServiceGroups. It contains the usual CRD metadata, and the service
// group spec and status.
// +kubebuilder:resource:shortName=sg;sgs
type ServiceGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ServiceGroupSpec `json:"spec"`
	// +optional
	Status ServiceGroupStatus `json:"status"`
}

// ServiceGroupSpec configures the allocator.  It will have one of
// either a Local configuration (to allocate service addresses from a
// local pool) or a Netbox configuration (to get addresses from the
// Netbox IPAM). For examples, see the "config/" directory in the
// PureLB source tree.
type ServiceGroupSpec struct {
	// +optional
	Local *ServiceGroupLocalSpec `json:"local,omitempty"`
	// +optional
	Netbox *ServiceGroupNetboxSpec `json:"netbox,omitempty"`
}

// ServiceGroupLocalSpec configures the allocator to manage pools of
// IP addresses locally. Pools can be specified as a CIDR or as a
// from-to range of addresses,
// e.g. 'fd53:9ef0:8683::-fd53:9ef0:8683::3'. The subnet is specified
// with CIDR notation, e.g., 'fd53:9ef0:8683::/120'. All of the
// addresses in the Pool must be contained within the
// Subnet. Aggregation is currently unused.
//
// The Subnet, Pool, and Aggregation fields are a legacy from the
// pre-dualStack days when a service could have only one IP
// address. If you're running a single-stack environment then they're
// still valid, but V4Pool and V6Pool are preferred. The V4Pool and
// V6Pool fields allow you to configure pools of both IPV4 and IPV6
// addresses to support dual-stack and you can also use them in a
// single-stack environment.
type ServiceGroupLocalSpec struct {
	// +optional
	Subnet string `json:"subnet"`
	// +optional
	Pool string `json:"pool"`
	// +optional
	Aggregation string `json:"aggregation"`

	// +optional
	V4Pool *ServiceGroupAddressPool `json:"v4pool,omitempty"`
	// +optional
	V6Pool *ServiceGroupAddressPool `json:"v6pool,omitempty"`

	// +optional
	V4Pools []*ServiceGroupAddressPool `json:"v4pools,omitempty"`
	// +optional
	V6Pools []*ServiceGroupAddressPool `json:"v6pools,omitempty"`
}

// FamilyAggregation returns this Spec's aggregation value that
// corresponds to family.
func (s *ServiceGroupLocalSpec) FamilyAggregation(family int) (string, error) {
	if family == nl.FAMILY_V4 {
		if s.V4Pool != nil {
			return s.V4Pool.Aggregation, nil
		} else {
			// If the legacy pool is V4 we can return that
			ip, _, err := net.ParseCIDR(s.Subnet)
			if err != nil {
				return "", err
			}
			if addrFamily(ip) != nl.FAMILY_V4 {
				return "", fmt.Errorf("no IPV4 aggregation has been configured")
			}
			return s.Aggregation, nil
		}
	}
	if family == nl.FAMILY_V6 {
		if s.V6Pool != nil {
			return s.V6Pool.Aggregation, nil
		} else {
			// If the legacy pool is V6 we can return that
			ip, _, err := net.ParseCIDR(s.Subnet)
			if err != nil {
				return "", err
			}
			if addrFamily(ip) != nl.FAMILY_V6 {
				return "", fmt.Errorf("no IPV6 aggregation has been configured")
			}
			return s.Aggregation, nil
		}
	}
	return "", fmt.Errorf("unable to find aggregation for family %d", family)
}

// Subnet returns this Spec's subnet value that corresponds to the
// provided address.
func (s *ServiceGroupLocalSpec) AddressSubnet(address net.IP) (string, error) {
	pool, err := s.PoolForAddress(address)
	if err != nil {
		return "", err
	}
	return pool.Subnet, nil
}

// Subnet returns this Spec's aggregation value that corresponds to
// the provided address.
func (s *ServiceGroupLocalSpec) AddressAggregation(address net.IP) (string, error) {
	pool, err := s.PoolForAddress(address)
	if err != nil {
		return "", err
	}
	return pool.Subnet, nil
}

// Subnet returns this Spec's Pool that corresponds to the provided
// address.
func (s *ServiceGroupLocalSpec) PoolForAddress(address net.IP) (*ServiceGroupAddressPool, error) {
	for _, spec := range s.V6Pools {
		pool, err := NewIPRange(spec.Pool)
		if err == nil && pool.Contains(address) {
			return spec, nil
		}
	}
	for _, spec := range s.V4Pools {
		pool, err := NewIPRange(spec.Pool)
		if err == nil && pool.Contains(address) {
			return spec, nil
		}
	}
	if s.V6Pool != nil {
		pool, err := NewIPRange(s.V6Pool.Pool)
		if err == nil && pool.Contains(address) {
			return s.V6Pool, nil
		}
	}
	if s.V4Pool != nil {
		pool, err := NewIPRange(s.V4Pool.Pool)
		if err == nil && pool.Contains(address) {
			return s.V4Pool, nil
		}
	}
	if s.Pool != "" && s.Subnet != "" {
		return &ServiceGroupAddressPool{
			Pool:        s.Pool,
			Subnet:      s.Subnet,
			Aggregation: s.Aggregation,
		}, nil
	}
	return nil, fmt.Errorf("unable to find pool for address %+v", address)
}

// addrFamily returns whether lbIP is an IPV4 or IPV6 address.  The
// return value will be nl.FAMILY_V6 if the address is an IPV6
// address, nl.FAMILY_V4 if it's IPV4, or 0 if the family can't be
// determined.
func addrFamily(lbIP net.IP) (lbIPFamily int) {
	if nil != lbIP.To16() {
		lbIPFamily = nl.FAMILY_V6
	}

	if nil != lbIP.To4() {
		lbIPFamily = nl.FAMILY_V4
	}

	return
}

// ServiceGroupNetboxSpec configures the allocator to request
// addresses from a Netbox IPAM system.
type ServiceGroupNetboxSpec struct {
	URL         string `json:"url"`
	Tenant      string `json:"tenant"`
	Aggregation string `json:"aggregation"`
}

// ServiceGroupAddressPool specifies a pool of addresses that belong
// to a ServiceGroupLocalSpec.
type ServiceGroupAddressPool struct {
	// Pool specifies a pool of addresses that PureLB manages. It can be
	// a CIDR or a from-to range of addresses, e.g.,
	// 'fd53:9ef0:8683::-fd53:9ef0:8683::3'.
	Pool string `json:"pool"`

	// Subnet specifies the subnet that contains all of the addresses in
	// the Pool. It's specified with CIDR notation, e.g.,
	// 'fd53:9ef0:8683::/120'. All of the addresses in the Pool must be
	// contained within the Subnet.
	Subnet string `json:"subnet"`

	// Aggregation changes the address mask of the allocated address
	// from the subnet mask to the specified mask. It can be "default"
	// or an integer in the range 8-128.
	Aggregation string `json:"aggregation"`
}

// ServiceGroupStatus is currently unused.
type ServiceGroupStatus struct {
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LBNodeAgent is the top-level custom resource for configuring node
// agents. It contains the usual CRD metadata, and the agent spec and
// status.
// +kubebuilder:resource:shortName=lbna;lbnas
type LBNodeAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LBNodeAgentSpec `json:"spec"`
	// +optional
	Status LBNodeAgentStatus `json:"status"`
}

// LBNodeAgentSpec configures the node agents.  It will have one Local
// configuration to announce service addresses locally. For examples,
// see the "config/" directory in the PureLB source tree.
type LBNodeAgentSpec struct {
	Local *LBNodeAgentLocalSpec `json:"local"`
}

// LBNodeAgentLocalSpec configures the announcers to announce service
// addresses by configuring the underlying operating
// system. LocalInterface is unimplemented but will be optional. If it
// is not provided then the agents will add the service address to
// whichever interface carries the default route. ExtLBInterface is
// also unimplemented.
type LBNodeAgentLocalSpec struct {
	// LocalInterface allows the user to specify the interface to use
	// for announcement of local addresses. This field is optional but
	// the default is "default" which will make PureLB use the interface
	// that has the default route, which works in most cases.
	// +kubebuilder:default="default"
	// +optional
	LocalInterface string `json:"localint"`

	// ExtLBInterface specifies the name of the interface to use for
	// announcement of non-local routes. This field is optional but the
	// default is "kube-lb0" which works in most cases.
	// +kubebuilder:default="kube-lb0"
	// +optional
	ExtLBInterface string `json:"extlbint"`

	// SendGratuitousARP determines whether or not the node agent should
	// send Gratuitous ARP messages when it adds an IP address to the
	// local interface. This can be used to alert network equipment that
	// the IP-to-MAC binding has changed.
	// +kubebuilder:default=false
	SendGratuitousARP bool `json:"sendgarp"`
}

// LBNodeAgentStatus is currently unused.
type LBNodeAgentStatus struct {
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceGroupList holds a list of ServiceGroup.
type ServiceGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []ServiceGroup `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LBNodeAgentList holds a list of LBNodeAgent.
type LBNodeAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []LBNodeAgent `json:"items"`
}
