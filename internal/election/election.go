package election

import (
	"log"
	"time"

	"purelb.io/internal/k8s"

	gokitlog "github.com/go-kit/kit/log"
	"github.com/hashicorp/memberlist"
)

type Config struct {
	Namespace string
	NodeName string
	Labels string
	BindAddr string
	BindPort int
	Secret []byte
	StopCh chan struct{}
	Logger *gokitlog.Logger
}

type Election struct {
	Memberlist *memberlist.Memberlist
	logger gokitlog.Logger
	stopCh chan struct{}
	eventCh chan memberlist.NodeEvent
}

func New(cfg *Config) (Election, error) {
	election := Election{stopCh: cfg.StopCh, logger: *cfg.Logger}

	mconfig := memberlist.DefaultLANConfig()
	// mconfig.Name MUST be spec.nodeName, as we will match it against
	// Endpoints nodeName in usableNodes()
	mconfig.Name = cfg.NodeName
	mconfig.BindAddr = cfg.BindAddr
	mconfig.BindPort = cfg.BindPort
	mconfig.AdvertisePort = cfg.BindPort
	mconfig.SecretKey = cfg.Secret

	loggerout := gokitlog.NewStdlibAdapter(gokitlog.With(*cfg.Logger, "component", "MemberList"))
	mconfig.Logger = log.New(loggerout, "", log.Lshortfile)

	eventCh := make(chan memberlist.NodeEvent, 16)
	mconfig.Events = &memberlist.ChannelEventDelegate{Ch: eventCh}
	election.eventCh = eventCh

	mlist, err := memberlist.Create(mconfig)
	election.Memberlist = mlist
	return election, err
}

func (e *Election) Join(iplist []string, client *k8s.Client) error {
	go e.watchEvents(client)

	n, err := e.Memberlist.Join(iplist)
	e.logger.Log("op", "startup", "msg", "Memberlist join", "nb joined", n, "error", err)
	return err
}

func (e *Election) Shutdown() error {
	err := e.Memberlist.Leave(1 * time.Second)
	e.Memberlist.Shutdown()
	e.logger.Log("op", "shutdown", "msg", "MemberList shut down", "error", err)

	return err
}

func event2String(e memberlist.NodeEventType) string {
	return [...]string{"NodeJoin", "NodeLeave", "NodeUpdate"}[e]
}

func (e *Election) watchEvents(client *k8s.Client) {
	for {
		select {
		case event := <-e.eventCh:
			e.logger.Log("msg", "Node event", "node addr", event.Node.Addr, "node name", event.Node.Name, "node event", event2String(event.Event))
			for _, member := range e.Memberlist.Members() {
				e.logger.Log("member name", member.Name, "member addr", member.Addr)
			}
			client.ForceSync()
		case <-e.stopCh:
			e.Shutdown()
			return
		}
	}
}
