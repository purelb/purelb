package election

import (
	"bytes"
	"crypto/sha256"
	"log"
	"sort"
	"time"

	"purelb.io/internal/k8s"

	gokitlog "github.com/go-kit/kit/log"
	"github.com/hashicorp/memberlist"
)

// Config provides the configuration data that New() needs.
type Config struct {
	Namespace string
	NodeName  string
	Labels    string
	BindAddr  string
	BindPort  int
	Secret    []byte
	StopCh    chan struct{}
	Logger    *gokitlog.Logger
}

type Election struct {
	Memberlist *memberlist.Memberlist
	logger     gokitlog.Logger
	stopCh     chan struct{}
	eventCh    chan memberlist.NodeEvent
}

func New(cfg *Config) (Election, error) {
	election := Election{stopCh: cfg.StopCh, logger: *cfg.Logger}

	mconfig := memberlist.DefaultLANConfig()
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

// Winner returns the node name of the "winning" node, i.e., the node
// that will announce the service represented by "key".
func (e *Election) Winner(key string) string {
	nodes := []string{}
	for _, node := range e.Memberlist.Members() {
		nodes = append(nodes, node.Name)
	}

	return election(key, nodes)[0]
}

// election conducts an election among the candidates based on the
// provided key. The order of the candidates in the return array is
// the result of the election.
func election(key string, candidates []string) []string {
	// Sort the slice by the hash of candidate name + service key. This
	// produces an ordering of ready candidates that is unique to this
	// service.
	sort.Slice(candidates, func(i, j int) bool {
		hi := sha256.Sum256([]byte(candidates[i] + "#" + key))
		hj := sha256.Sum256([]byte(candidates[j] + "#" + key))

		return bytes.Compare(hi[:], hj[:]) < 0
	})

	return candidates
}

func event2String(e memberlist.NodeEventType) string {
	return [...]string{"NodeJoin", "NodeLeave", "NodeUpdate"}[e]
}

func (e *Election) watchEvents(client *k8s.Client) {
	for {
		select {
		case event := <-e.eventCh:
			e.logger.Log("msg", "Node event", "node addr", event.Node.Addr, "node name", event.Node.Name, "node event", event2String(event.Event))
			client.ForceSync()
		case <-e.stopCh:
			e.Shutdown()
			return
		}
	}
}
