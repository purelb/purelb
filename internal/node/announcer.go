package node

import (
	"net"

	"purelb.io/internal/config"

	"k8s.io/api/core/v1"
)

// Announces service IP addresses
type Announcer interface {
	SetConfig(*config.Config) error
	SetBalancer(string, net.IP) error
	DeleteBalancer(string, string) error
	SetNode(*v1.Node) error
}
