package allocator

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"purelb.io/internal/config"
)

// Ports turns a service definition into a set of allocator ports.
func Ports(svc *v1.Service) []config.Port {
	var ret []config.Port
	for _, port := range svc.Spec.Ports {
		ret = append(ret, config.Port{
			Proto: string(port.Protocol),
			Port:  int(port.Port),
		})
	}
	return ret
}

// SharingKey extracts the sharing key for a service.
func SharingKey(svc *v1.Service) string {
	return svc.Annotations[sharingAnnotation]
}

// BackendKey extracts the backend key for a service.
func BackendKey(svc *v1.Service) string {
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		return labels.Set(svc.Spec.Selector).String()
	}
	// Cluster traffic policy can share services regardless of backends.
	return ""
}
