package node

import (
	"net"

	v1 "k8s.io/api/core/v1"
	"purelb.io/internal/config"
)

// Announces service IP addresses
type Announcer interface {
	SetConfig(*config.Config) error
	SetBalancer(string, net.IP, string) error
	DeleteBalancer(string, string) error
	SetNode(*v1.Node) error
	CheckLocal(net.IP) (net.IPNet, int, error)
}
