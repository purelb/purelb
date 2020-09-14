package allocator

import (
	"k8s.io/api/core/v1"
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
	return svc.Annotations[sharingAnnotation]
}
