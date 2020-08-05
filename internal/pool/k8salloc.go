package pool

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Ports turns a service definition into a set of allocator ports.
func Ports(svc *v1.Service) []Port {
	var ret []Port
	for _, port := range svc.Spec.Ports {
		ret = append(ret, Port{
			Proto: string(port.Protocol),
			Port:  int(port.Port),
		})
	}
	return ret
}

// SharingKey extracts the sharing key for a service.
func SharingKey(svc *v1.Service) string {
	return svc.Annotations["metallb.universe.tf/allow-shared-ip"]
}

// BackendKey extracts the backend key for a service.
func BackendKey(svc *v1.Service) string {
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		return labels.Set(svc.Spec.Selector).String()
	}
	// Cluster traffic policy can share services regardless of backends.
	return ""
}
