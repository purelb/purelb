package speaker

import (
	"net"

	"purelb.io/internal/config"

	v1 "k8s.io/api/core/v1"
	gokitlog "github.com/go-kit/kit/log"
)

// An Announcer can announce an IP address
type Announcer interface {
	SetConfig(gokitlog.Logger, *config.Config) error
	ShouldAnnounce(gokitlog.Logger, string, *v1.Service, *v1.Endpoints) string
	SetBalancer(gokitlog.Logger, string, net.IP) error
	DeleteBalancer(gokitlog.Logger, string, string) error
	SetNode(gokitlog.Logger, *v1.Node) error
}
