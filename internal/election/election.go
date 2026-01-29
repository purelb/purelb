// Copyright 2020 Acnodal Inc.
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
// See the License for the sp

package election

import (
	"bytes"
	"crypto/sha256"
	"log"
	"sort"
	"time"

	"purelb.io/internal/k8s"

	gokitlog "github.com/go-kit/log"
	"github.com/hashicorp/memberlist"
)

// Config provides the configuration data that New() needs.
type Config struct {
	Namespace string
	Labels    string
	NodeName  string
	BindAddr  string
	BindPort  int
	Secret    []byte
	StopCh    chan struct{}
	Logger    *gokitlog.Logger
	Client    *k8s.Client
}

type Election struct {
	namespace  string
	labels     string
	Memberlist *memberlist.Memberlist
	logger     gokitlog.Logger
	stopCh     chan struct{}
	eventCh    chan memberlist.NodeEvent
	Client     *k8s.Client
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
	election.namespace = cfg.Namespace
	election.labels = cfg.Labels

	mlist, err := memberlist.Create(mconfig)
	election.Memberlist = mlist
	election.Client = cfg.Client

	return election, err
}

func (e *Election) Join(iplist []string) error {
	go e.watchEvents()

	// To minimize the system impact of joining the memberlist we limit
	// the number of initial peers to 5 no matter how many pods we have.
	podCount := len(iplist)
	if podCount > 5 {
		podCount = 5
	}

	n, err := e.Memberlist.Join(iplist[0:podCount])
	e.logger.Log("op", "startup", "msg", "Memberlist join", "hosts contacted", n)
	return err
}

func (e *Election) shutdown() error {
	err := e.Memberlist.Leave(1 * time.Second)
	e.Memberlist.Shutdown()
	e.logger.Log("op", "shutdown", "msg", "MemberList shut down", "error", err)

	return err
}

// Winner returns the node name of the "winning" node, i.e., the node
// that will announce the service represented by "key".
func (e *Election) Winner(key string) string {
	members := e.Memberlist.Members()
	pods, err := e.Client.GetPodsIPs(e.namespace, e.labels)
	if err != nil {
		e.logger.Log("op", "Election", "error", err, "msg", "failed to get Pod count")
	}

	// if the number of pods that k8s reports is different than the
	// number of members in the memberlist then something has gotten out
	// of sync.
	if len(members) != len(pods) {
		e.logger.Log("op", "Election", "error", "members/pods out of sync", "members", members, "pods", pods)
	}

	nodes := []string{}
	for _, node := range members {
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

func (e *Election) watchEvents() {
	for {
		select {
		case event := <-e.eventCh:
			e.logger.Log("msg", "Node event", "node addr", event.Node.Addr, "node name", event.Node.Name, "node event", event2String(event.Event))
			e.Client.ForceSync()
		case <-e.stopCh:
			e.shutdown()
			return
		}
	}
}
