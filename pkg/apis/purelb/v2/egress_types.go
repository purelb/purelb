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

package v2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Egress gives selected pods in its namespace stable source addresses
// from a PureLB pool. Users write it; the allocator reports in status.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:shortName=eg
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters: it is used in labels and the sidecar key"
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.serviceGroup`
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.selectedPods`
// +kubebuilder:printcolumn:name="Addresses",type=string,JSONPath=`.status.addresses[*].address`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Egress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EgressSpec `json:"spec"`
	// +optional
	Status EgressStatus `json:"status,omitempty"`
}

// EgressSpec selects pods in the Egress's namespace and the address they
// leave from.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.serviceGroup) && has(self.serviceRef))",message="serviceGroup and serviceRef are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.serviceRef) != has(self.podSelector)",message="exactly one of serviceRef and podSelector is required"
// +kubebuilder:validation:XValidation:rule="!has(self.serviceRef) || (!has(self.pool) && (!has(self.addresses) || size(self.addresses) == 0) && !has(self.skipIPv6DAD))",message="serviceRef uses the Service's address: pool, addresses and skipIPv6DAD are not allowed"
// +kubebuilder:validation:XValidation:rule="has(self.serviceRef) ? (!has(self.affinity) || !has(self.affinity.mode) || self.affinity.mode == 'Inherit') : (!has(self.affinity) || !has(self.affinity.mode) || self.affinity.mode != 'Inherit')",message="affinity mode Inherit is used exactly with serviceRef"
// +kubebuilder:validation:XValidation:rule="!has(self.affinity) || !has(self.affinity.mode) || self.affinity.mode != 'PerPod' || ((!has(self.addresses) || size(self.addresses) == 0) && (!has(self.ports) || !has(self.ports.reserved) || size(self.ports.reserved) == 0))",message="PerPod affinity allocates per pod: addresses and ports.reserved must be empty"
type EgressSpec struct {
	// ServiceGroup names a ServiceGroup in the PureLB namespace. Absent,
	// the namespace's default group is used.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	ServiceGroup string `json:"serviceGroup,omitempty"`

	// ServiceRef uses a LoadBalancer Service's address in this namespace
	// for the pods its selector selects. The Service keeps owning the
	// address.
	// +optional
	ServiceRef *EgressServiceRef `json:"serviceRef,omitempty"`

	// PodSelector selects the pods in this namespace. {} selects every
	// pod. Required unless serviceRef is set.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	EgressPolicy `json:",inline"`

	// Priority orders overlapping policies, higher first; ClusterEgresses
	// always rank above Egresses.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	// +kubebuilder:default=0
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// OptOut says whether pods annotated purelb.io/egress: "false" are
	// excluded (Allowed) or the annotation is ignored (Denied).
	// +kubebuilder:validation:Enum=Allowed;Denied
	// +kubebuilder:default=Allowed
	// +optional
	OptOut string `json:"optOut,omitempty"`
}

// EgressServiceRef names a LoadBalancer Service in the Egress's namespace.
type EgressServiceRef struct {
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// EgressPolicy holds the fields Egress and ClusterEgress share.
type EgressPolicy struct {
	// Pool names a pool entry of the group (its name, or its subnet when
	// unnamed). Absent, the group's pool selection applies.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Pool string `json:"pool,omitempty"`

	// Addresses pins the addresses to use, at most one per family.
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="self.all(a, isIP(a))",message="addresses must be IP addresses"
	// +kubebuilder:validation:XValidation:rule="size(self) < 2 || ip(self[0]).family() != ip(self[1]).family()",message="addresses must be in different families"
	// +optional
	Addresses []string `json:"addresses,omitempty"`

	// IPFamilyPolicy chooses the families. SingleStack uses the
	// cluster's primary family.
	// +kubebuilder:validation:Enum=SingleStack;PreferDualStack;RequireDualStack
	// +kubebuilder:default=PreferDualStack
	// +optional
	IPFamilyPolicy string `json:"ipFamilyPolicy,omitempty"`

	// SkipIPv6DAD skips IPv6 duplicate address detection for dedicated
	// local-pool addresses. Absent, the pool's setting applies.
	// +optional
	SkipIPv6DAD *bool `json:"skipIPv6DAD,omitempty"`

	// Destinations chooses which traffic leaves from the egress address.
	Destinations EgressDestinations `json:"destinations"`

	// +optional
	Affinity *EgressAffinity `json:"affinity,omitempty"`

	// +optional
	Ports *EgressPorts `json:"ports,omitempty"`

	// Helpers lists the conntrack helpers to run (PerPod only).
	// +kubebuilder:validation:MaxItems=3
	// +listType=set
	// +optional
	Helpers []EgressHelper `json:"helpers,omitempty"`

	// FailurePolicy says what selected pods do while their egress isn't
	// ready: FailClosed drops their egress traffic, FailOpen lets it
	// leave from the pod's own address.
	// +kubebuilder:validation:Enum=FailClosed;FailOpen
	// +kubebuilder:default=FailClosed
	// +optional
	FailurePolicy string `json:"failurePolicy,omitempty"`
}

// EgressHelper is a conntrack helper.
// +kubebuilder:validation:Enum=SIP;FTP;H323
type EgressHelper string

// EgressDestinations selects traffic by destination: a destination that
// matches an include entry and no exclude entry egresses. Cluster
// networks are always excluded.
type EgressDestinations struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Include []EgressDestination `json:"include"`

	// +kubebuilder:validation:MaxItems=32
	// +optional
	Exclude []EgressDestination `json:"exclude,omitempty"`
}

// EgressDestination is a set of networks and, optionally, ports.
type EgressDestination struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c))",message="cidrs must be CIDRs"
	CIDRs []string `json:"cidrs"`

	// Ports filters by destination port. Absent, every protocol and
	// port matches.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Ports []EgressPortRange `json:"ports,omitempty"`
}

// EgressPortRange is a protocol and a port or inclusive port range.
// +kubebuilder:validation:XValidation:rule="!has(self.endPort) || self.port <= self.endPort",message="endPort must not be below port"
type EgressPortRange struct {
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +kubebuilder:default=TCP
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// EndPort ends an inclusive range starting at Port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	EndPort *int32 `json:"endPort,omitempty"`
}

// EgressAffinity chooses how addresses and their holders are placed.
// +kubebuilder:validation:XValidation:rule="!has(self.identity) || (has(self.mode) && self.mode == 'PerPod')",message="identity applies only to PerPod"
type EgressAffinity struct {
	// Mode: Hash shares addresses across pods by port slices; PerPod
	// gives each pod its own address; Inherit follows a Service's
	// placement (serviceRef only). Absent: Inherit with serviceRef,
	// otherwise Hash.
	// +kubebuilder:validation:Enum=Hash;PerPod;Inherit
	// +optional
	Mode string `json:"mode,omitempty"`

	// Identity keys PerPod addresses.
	// +kubebuilder:validation:Enum=StatefulSetOrdinal
	// +optional
	Identity string `json:"identity,omitempty"`
}

// EgressPorts configures port slices and reserved ports.
type EgressPorts struct {
	// SliceSize is the number of source ports in each node's slice.
	// Absent, the group's sliceSize applies.
	// +kubebuilder:validation:Enum=64;128;256;512;1024;2048;4096;8192;16384
	// +optional
	SliceSize *int32 `json:"sliceSize,omitempty"`

	// Reserved keeps a pod's own source port on the egress address.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(r, self.exists_one(q, q.protocol == r.protocol && q.port == r.port))",message="reserved (protocol, port) pairs must be unique"
	// +optional
	Reserved []EgressReservedPort `json:"reserved,omitempty"`
}

// EgressReservedPort reserves one source port for one pod.
type EgressReservedPort struct {
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +kubebuilder:default=TCP
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// PodSelector must select exactly one pod.
	PodSelector metav1.LabelSelector `json:"podSelector"`
}

// EgressStatus reports an Egress's or ClusterEgress's selection,
// addresses and health.
type EgressStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	SelectedPods int32 `json:"selectedPods,omitempty"`

	// ExcludedPods lists pods the selector matched but egress excludes,
	// at most 100; ExcludedPodsTruncated counts the rest.
	// +kubebuilder:validation:MaxItems=100
	// +optional
	ExcludedPods []EgressExcludedPod `json:"excludedPods,omitempty"`
	// +optional
	ExcludedPodsTruncated int32 `json:"excludedPodsTruncated,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +optional
	Addresses []EgressAddressStatus `json:"addresses,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EgressExcludedPod is a pod the policy selects but egress excludes.
type EgressExcludedPod struct {
	// +kubebuilder:validation:MaxLength=253
	Pod string `json:"pod"`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:Enum=HostNetwork;OptedOut;NotStatefulSet;NodeUnsupported;NamespaceNotInScope;Outranked
	Reason string `json:"reason"`
}

// EgressAddressStatus reports one egress address. Under a group's
// statusDetail Redacted, announcers and slices are left empty.
type EgressAddressStatus struct {
	// +kubebuilder:validation:MaxLength=45
	Address string `json:"address"`
	// +kubebuilder:validation:Enum=IPv4;IPv6
	Family string `json:"family"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Pool string `json:"pool,omitempty"`
	// +kubebuilder:validation:Enum=SingleAnnouncer;MultiAnnouncer
	// +optional
	Placement string `json:"placement,omitempty"`
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	Announcers []string `json:"announcers,omitempty"`
	// +kubebuilder:validation:MaxItems=1008
	// +optional
	Slices []EgressSliceStatus `json:"slices,omitempty"`
	// +optional
	Capacity *EgressCapacity `json:"capacity,omitempty"`
}

// EgressSliceStatus reports one node's slice of an address.
type EgressSliceStatus struct {
	// +kubebuilder:validation:MaxLength=253
	Node string `json:"node"`
	// Ports is the slice's inclusive range, "first-last".
	// +kubebuilder:validation:MaxLength=11
	Ports string `json:"ports"`
	// +kubebuilder:validation:Enum=Pending;Active;Draining
	State string `json:"state"`
}

// EgressCapacity counts an address's slices.
type EgressCapacity struct {
	SlicesTotal int32 `json:"slicesTotal"`
	Active      int32 `json:"active"`
	Draining    int32 `json:"draining"`
	Free        int32 `json:"free"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EgressList is a list of Egresses.
type EgressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Egress `json:"items"`
}

// ClusterEgress is a platform admin's Egress across the namespaces its
// namespaceSelector selects, within the group's scope.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster,shortName=ceg
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters: it is used in labels and the sidecar key"
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.serviceGroup`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.selectedPods`
// +kubebuilder:printcolumn:name="Addresses",type=string,JSONPath=`.status.addresses[*].address`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ClusterEgress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterEgressSpec `json:"spec"`
	// +optional
	Status EgressStatus `json:"status,omitempty"`
}

// ClusterEgressSpec is an EgressSpec across namespaces, without
// serviceRef.
//
// +kubebuilder:validation:XValidation:rule="!has(self.affinity) || !has(self.affinity.mode) || self.affinity.mode != 'Inherit'",message="Inherit needs serviceRef, which a ClusterEgress doesn't have"
// +kubebuilder:validation:XValidation:rule="!has(self.affinity) || !has(self.affinity.mode) || self.affinity.mode != 'PerPod' || ((!has(self.addresses) || size(self.addresses) == 0) && (!has(self.ports) || !has(self.ports.reserved) || size(self.ports.reserved) == 0))",message="PerPod affinity allocates per pod: addresses and ports.reserved must be empty"
type ClusterEgressSpec struct {
	// ServiceGroup names a ServiceGroup in the PureLB namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ServiceGroup string `json:"serviceGroup"`

	// NamespaceSelector selects the namespaces. The scope must be bounded:
	// the selector constrains kubernetes.io/metadata.name, or the group
	// lists its namespaces.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// PodSelector selects pods in those namespaces. {} selects every pod.
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// NamespaceScope: Respect takes the namespaces the group serves;
	// AdmittedOnly also requires the namespace label
	// purelb.io/cluster-egress-admitted: "true".
	// +kubebuilder:validation:Enum=Respect;AdmittedOnly
	// +kubebuilder:default=Respect
	// +optional
	NamespaceScope string `json:"namespaceScope,omitempty"`

	EgressPolicy `json:",inline"`

	// Priority ranks a ClusterEgress above every Egress.
	// +kubebuilder:validation:Minimum=1000
	// +kubebuilder:default=1000
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// OptOut: as on Egress, but Denied by default.
	// +kubebuilder:validation:Enum=Allowed;Denied
	// +kubebuilder:default=Denied
	// +optional
	OptOut string `json:"optOut,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ClusterEgressList is a list of ClusterEgresses.
type ClusterEgressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterEgress `json:"items"`
}

// EgressEndpoints is one (policy, address): the address and its slices.
// The allocator writes it in the PureLB namespace; only the nodes
// announcing the address read it.
//
// +genclient
// +resourceName=egressendpoints
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:shortName=egep
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.serviceGroup`
// +kubebuilder:printcolumn:name="Placement",type=string,JSONPath=`.spec.placement`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EgressEndpoints struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EgressEndpointsSpec `json:"spec"`
	// +optional
	Status EgressEndpointsStatus `json:"status,omitempty"`
}

// EgressEndpointsSpec is one address of a policy and its slices.
type EgressEndpointsSpec struct {
	// State: Quarantined is a tombstone, held until Until.
	// +kubebuilder:validation:Enum=Active;Quarantined
	State string `json:"state"`

	// +optional
	Until *metav1.Time `json:"until,omitempty"`

	// Policy is what the address's nodes need of the policy.
	Policy EgressEndpointsPolicy `json:"policy"`

	// +kubebuilder:validation:MaxLength=45
	Address string `json:"address"`
	// +kubebuilder:validation:Enum=IPv4;IPv6
	Family string `json:"family"`

	// +kubebuilder:validation:MaxLength=63
	ServiceGroup string `json:"serviceGroup"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Pool string `json:"pool,omitempty"`
	// PoolType: Routed covers remote pools and external pools announced
	// remotely.
	// +kubebuilder:validation:Enum=Local;Routed
	PoolType string `json:"poolType"`

	// SharedWith names the Service that owns a shared address.
	// +optional
	SharedWith *EgressServiceRef `json:"sharedWith,omitempty"`

	// Identity is a PerPod address's StatefulSet ordinal identity.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Identity string `json:"identity,omitempty"`

	// +kubebuilder:validation:Enum=SingleAnnouncer;MultiAnnouncer
	Placement string `json:"placement"`

	// +kubebuilder:validation:Enum=64;128;256;512;1024;2048;4096;8192;16384
	SliceSize int32 `json:"sliceSize"`

	// ServicePorts are a shared address's Service ports, which no slice
	// covers.
	// +kubebuilder:validation:MaxItems=1024
	// +optional
	ServicePorts []EgressServicePort `json:"servicePorts,omitempty"`

	// Slices lists the address's allocated slices; free ones are absent.
	// +kubebuilder:validation:MaxItems=1008
	// +optional
	Slices []EgressSlice `json:"slices,omitempty"`

	// +kubebuilder:validation:MaxItems=16
	// +optional
	Reserved []EgressReservedAssignment `json:"reserved,omitempty"`

	// ExcludedNodes are left out of gating for lagging acks.
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	ExcludedNodes []string `json:"excludedNodes,omitempty"`
}

// EgressEndpointsPolicy is the part of a policy the pod-node stage needs.
type EgressEndpointsPolicy struct {
	Priority int32 `json:"priority"`
	// +kubebuilder:validation:Enum=FailClosed;FailOpen
	FailurePolicy string `json:"failurePolicy"`
	// +kubebuilder:validation:Enum=Hash;PerPod;Inherit
	AffinityMode string `json:"affinityMode"`
	// +kubebuilder:validation:MaxItems=3
	// +listType=set
	// +optional
	Helpers      []EgressHelper     `json:"helpers,omitempty"`
	Destinations EgressDestinations `json:"destinations"`
}

// EgressServicePort is a Service port on a shared address.
type EgressServicePort struct {
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	Protocol string `json:"protocol"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// EgressSlice assigns ports [index × sliceSize, (index+1) × sliceSize − 1]
// to a node. An empty node is owner 0: replies drop.
type EgressSlice struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1023
	Index int32 `json:"index"`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Node string `json:"node,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Active;Draining
	State string `json:"state"`
}

// EgressReservedAssignment maps a reserved port to its node and pod.
type EgressReservedAssignment struct {
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	Protocol string `json:"protocol"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Node string `json:"node,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Pod string `json:"pod,omitempty"`
}

// EgressEndpointsStatus reports what the allocator observes.
type EgressEndpointsStatus struct {
	// ObservedAnnouncers are the nodes announcing the address, from the
	// Leases and EgressNodeStates. Agents never read it.
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	ObservedAnnouncers []string `json:"observedAnnouncers,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EgressEndpointsList is a list of EgressEndpoints.
type EgressEndpointsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressEndpoints `json:"items"`
}

// EgressAddress makes one egress address known to every egress node:
// what a node needs to elect the address's owner, advertise it, or guard
// it, without its slice table. The allocator writes it with the address's
// EgressEndpoints, which owns it, and changes it only when these fields
// change, so it costs one event per node per address change rather than
// per slice change.
//
// +genclient
// +resourceName=egressaddresses
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:shortName=egaddr
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.serviceGroup`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.poolType`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EgressAddress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EgressAddressSpec `json:"spec"`
}

// EgressAddressSpec is one egress address as every egress node sees it.
type EgressAddressSpec struct {
	// State: Quarantined is a tombstone; no node announces it.
	// +kubebuilder:validation:Enum=Active;Quarantined
	State string `json:"state"`

	// +kubebuilder:validation:MaxLength=45
	Address string `json:"address"`
	// +kubebuilder:validation:Enum=IPv4;IPv6
	Family string `json:"family"`

	// +kubebuilder:validation:MaxLength=63
	ServiceGroup string `json:"serviceGroup"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Pool string `json:"pool,omitempty"`
	// +kubebuilder:validation:Enum=Local;Routed
	PoolType string `json:"poolType"`
	// +kubebuilder:validation:Enum=SingleAnnouncer;MultiAnnouncer
	Placement string `json:"placement"`

	// SharedWith names the Service that owns a shared address, which
	// egress never places (D31).
	// +optional
	SharedWith *EgressServiceRef `json:"sharedWith,omitempty"`

	// PreferredNodes are the nodes a local pool's election prefers: a
	// PerPod address's pod node, or with preferPodNodes the nodes holding
	// an Active or Draining slice (§5.5).
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	PreferredNodes []string `json:"preferredNodes,omitempty"`

	// SkipIPv6DAD is the policy's skipIPv6DAD for a dedicated address
	// (D31). Absent, the pool's setting applies.
	// +optional
	SkipIPv6DAD *bool `json:"skipIPv6DAD,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EgressAddressList is a list of EgressAddresses.
type EgressAddressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressAddress `json:"items"`
}

// EgressMembers lists one policy's selected pods per node, at most 500
// pods per object. Only the allocator reads it.
//
// +genclient
// +resourceName=egressmembers
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:shortName=egm
type EgressMembers struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EgressMembersSpec `json:"spec"`
}

// EgressMembersSpec is a set of nodes and their selected pods.
// +kubebuilder:validation:XValidation:rule="!has(self.nodes) || self.nodes.map(n, size(n.pods)).sum() <= 500",message="at most 500 pods per object"
type EgressMembersSpec struct {
	// +kubebuilder:validation:MaxItems=500
	// +optional
	Nodes []EgressMemberNode `json:"nodes,omitempty"`
}

// EgressMemberNode is a node with selected pods.
type EgressMemberNode struct {
	// +kubebuilder:validation:MaxLength=253
	Node string `json:"node"`
	// +kubebuilder:validation:MaxItems=500
	Pods []EgressMemberPod `json:"pods"`
}

// EgressMemberPod is one selected pod.
type EgressMemberPod struct {
	// Namespace is set for a ClusterEgress's pods, which span namespaces.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=36
	UID string `json:"uid"`
	// Identity is the pod's PerPod identity.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Identity string `json:"identity,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EgressMembersList is a list of EgressMembers.
type EgressMembersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressMembers `json:"items"`
}

// EgressNodeState is one node's egress state: the allocator writes its
// own slices in spec, the node's agent its acks, claims and capability
// in status. Named after the node, with an ownerReference to it.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster,shortName=egns
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.capability.podNodeStage`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="DatapathReady")].status`
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=`.status.capability.slots.used`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EgressNodeState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec EgressNodeStateSpec `json:"spec,omitempty"`
	// +optional
	Status EgressNodeStateStatus `json:"status,omitempty"`
}

// EgressNodeStateSpec is the node's own slices.
type EgressNodeStateSpec struct {
	// +kubebuilder:validation:MaxItems=4096
	// +optional
	Slices []EgressNodeSlice `json:"slices,omitempty"`
}

// EgressNodeSlice is one of the node's slices.
type EgressNodeSlice struct {
	// +kubebuilder:validation:MaxLength=253
	Endpoints string `json:"endpoints"`
	// +kubebuilder:validation:MaxLength=45
	Address string `json:"address"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1023
	Index int32 `json:"index"`
	// +kubebuilder:validation:Enum=Pending;Active;Draining
	State string `json:"state"`
}

// EgressNodeStateStatus is the node agent's report.
type EgressNodeStateStatus struct {
	// ObservedGeneration is the spec generation fully installed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Announcing lists the addresses announced now, each with the
	// EgressEndpoints generation fully installed.
	// +kubebuilder:validation:MaxItems=4096
	// +optional
	Announcing []EgressAnnouncing `json:"announcing,omitempty"`

	// Claims asks for a slice of each listed address.
	// +kubebuilder:validation:MaxItems=1000
	// +optional
	Claims []EgressClaim `json:"claims,omitempty"`

	// Drained lists Draining slices with no flows left.
	// +kubebuilder:validation:MaxItems=1008
	// +optional
	Drained []EgressDrained `json:"drained,omitempty"`

	// +optional
	Capability *EgressCapability `json:"capability,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EgressAnnouncing acks one address's EgressEndpoints generation.
type EgressAnnouncing struct {
	// +kubebuilder:validation:MaxLength=253
	Endpoints string `json:"endpoints"`
	// +kubebuilder:validation:MaxLength=45
	Address    string `json:"address"`
	Generation int64  `json:"generation"`
}

// EgressClaim asks for a slice of one address.
type EgressClaim struct {
	// +kubebuilder:validation:MaxLength=253
	Endpoints string `json:"endpoints"`
}

// EgressDrained reports a drained slice.
type EgressDrained struct {
	// +kubebuilder:validation:MaxLength=253
	Endpoints string `json:"endpoints"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1023
	Index int32 `json:"index"`
}

// EgressCapability is what the node's datapath supports.
type EgressCapability struct {
	EgressCapable bool `json:"egressCapable"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Kernel string `json:"kernel,omitempty"`
	// +kubebuilder:validation:Enum=Netfilter;EBPF
	// +optional
	PodNodeStage string `json:"podNodeStage,omitempty"`
	// +kubebuilder:validation:Enum=L3;L2
	// +optional
	TunnelEncapsulation string `json:"tunnelEncapsulation,omitempty"`
	// +kubebuilder:validation:Enum="0x000000ff";"0x0000ff00";"0x00ff0000";"0xff000000"
	// +optional
	MarkField string `json:"markField,omitempty"`
	// +optional
	Slots *EgressSlots `json:"slots,omitempty"`
	// +optional
	Underlay *EgressUnderlay `json:"underlay,omitempty"`
}

// EgressSlots counts the node's mark slots.
type EgressSlots struct {
	Used     int32 `json:"used"`
	Capacity int32 `json:"capacity"`
}

// EgressUnderlay is the node's tunnel underlay addresses.
type EgressUnderlay struct {
	// +kubebuilder:validation:MaxLength=45
	// +optional
	IPv4 string `json:"ipv4,omitempty"`
	// +kubebuilder:validation:MaxLength=45
	// +optional
	IPv6 string `json:"ipv6,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EgressNodeStateList is a list of EgressNodeStates.
type EgressNodeStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressNodeState `json:"items"`
}

// ServiceGroupEgressSpec lets a ServiceGroup serve egress, and bounds it.
type ServiceGroupEgressSpec struct {
	// Allowed false refuses new Egresses and withdraws existing ones.
	// +kubebuilder:default=false
	// +optional
	Allowed bool `json:"allowed,omitempty"`

	// AllowSharedServiceAddress permits serviceRef.
	// +kubebuilder:default=false
	// +optional
	AllowSharedServiceAddress bool `json:"allowSharedServiceAddress,omitempty"`

	// AffinityModes are the modes a policy may use.
	// +kubebuilder:validation:MaxItems=3
	// +listType=set
	// +kubebuilder:validation:items:Enum=Hash;PerPod;Inherit
	// +kubebuilder:default={Hash,PerPod,Inherit}
	// +optional
	AffinityModes []string `json:"affinityModes,omitempty"`

	// Helpers are the conntrack helpers a policy may request.
	// +kubebuilder:validation:MaxItems=3
	// +listType=set
	// +optional
	Helpers []EgressHelper `json:"helpers,omitempty"`

	// AnnouncerNodeSelector restricts the nodes that announce (local
	// pools) or advertise (routed pools) egress addresses. Absent, every
	// egress-capable node.
	// +optional
	AnnouncerNodeSelector *metav1.LabelSelector `json:"announcerNodeSelector,omitempty"`

	// PreferPodNodes makes local pools prefer nodes holding an Active
	// slice.
	// +kubebuilder:default=false
	// +optional
	PreferPodNodes bool `json:"preferPodNodes,omitempty"`

	// SliceSize is the default number of source ports per slice.
	// +kubebuilder:validation:Enum=64;128;256;512;1024;2048;4096;8192;16384
	// +kubebuilder:default=4096
	// +optional
	SliceSize int32 `json:"sliceSize,omitempty"`

	// ReserveSlices is how many unassigned slices each address keeps
	// ready for new nodes, as free slices allow.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=64
	// +kubebuilder:default=4
	// +optional
	ReserveSlices *int32 `json:"reserveSlices,omitempty"`

	// ReleaseQuarantine holds a released address before reuse.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s') && duration(self) <= duration('720h')",message="releaseQuarantine must be between 0s and 720h"
	// +kubebuilder:default="1h"
	// +optional
	ReleaseQuarantine *metav1.Duration `json:"releaseQuarantine,omitempty"`

	// +optional
	Tenancy *EgressTenancy `json:"tenancy,omitempty"`
}

// EgressTenancy bounds what tenants of a group may do.
type EgressTenancy struct {
	// +kubebuilder:default=true
	// +optional
	AllowClusterEgress *bool `json:"allowClusterEgress,omitempty"`

	// PriorityCeiling clamps namespaced priorities.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	// +kubebuilder:default=999
	// +optional
	PriorityCeiling *int32 `json:"priorityCeiling,omitempty"`

	// StatusDetail Redacted hides nodes, slices and holders in status,
	// sends node-bearing events to the PureLB namespace and suppresses
	// per-address metrics.
	// +kubebuilder:validation:Enum=Full;Redacted
	// +kubebuilder:default=Full
	// +optional
	StatusDetail string `json:"statusDetail,omitempty"`

	// OnRevocation applies when a policy stops being contained.
	// +kubebuilder:validation:Enum=Withdraw;RetainUntilDelete
	// +kubebuilder:default=Withdraw
	// +optional
	OnRevocation string `json:"onRevocation,omitempty"`

	// MaxEgressesPerNamespace caps Egresses per namespace. Absent,
	// unlimited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxEgressesPerNamespace *int32 `json:"maxEgressesPerNamespace,omitempty"`
}

// LBNodeAgentEgressSpec enables and configures egress on the nodes the
// LBNodeAgent selects.
type LBNodeAgentEgressSpec struct {
	// Enabled false leaves the node exactly as without egress.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ExcludedNetworks are destinations egress leaves alone, added to the
	// detected cluster networks.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c))",message="excludedNetworks must be CIDRs"
	// +optional
	ExcludedNetworks []string `json:"excludedNetworks,omitempty"`

	// PodNetworks are pod ranges that discovery can't see, added to the
	// cluster networks.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c))",message="podNetworks must be CIDRs"
	// +optional
	PodNetworks []string `json:"podNetworks,omitempty"`

	// TunnelEncapsulation: Auto uses L3 where the kernel supports it,
	// else L2. Every node must agree.
	// +kubebuilder:validation:Enum=Auto;L3;L2
	// +kubebuilder:default=Auto
	// +optional
	TunnelEncapsulation string `json:"tunnelEncapsulation,omitempty"`

	// TunnelPort is the Geneve UDP port; it must differ from the CNI's.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=6082
	// +optional
	TunnelPort int32 `json:"tunnelPort,omitempty"`

	// MarkField is the byte of the packet and conntrack marks egress
	// uses.
	// +kubebuilder:validation:Enum="0x000000ff";"0x0000ff00";"0x00ff0000";"0xff000000"
	// +kubebuilder:default="0x000000ff"
	// +optional
	MarkField string `json:"markField,omitempty"`

	// ReservedClusterSlots reserves per-node slots for ClusterEgresses.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=127
	// +kubebuilder:default=0
	// +optional
	ReservedClusterSlots int32 `json:"reservedClusterSlots,omitempty"`

	// FlowLog logs egress flows from conntrack events (netfilter-stage
	// nodes).
	// +kubebuilder:default=false
	// +optional
	FlowLog bool `json:"flowLog,omitempty"`
}
