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
	"fmt"
	"log"
	"sort"
	"time"

	"purelb.io/internal/k8s"

	gokitlog "github.com/go-kit/kit/log"
	"github.com/hashicorp/memberlist"
)

const (
	mlLabels = "app=purelb,component=lbnodeagent"
)

// Config provides the configuration data that New() needs.
type Config struct {
	Namespace string
	NodeName  string
	BindAddr  string
	BindPort  int
	Secret    []byte
	StopCh    chan struct{}
	Logger    *gokitlog.Logger
	client    k8s.Client
}

type Election struct {
	Memberlist *memberlist.Memberlist
	Namespace  string
	logger     gokitlog.Logger
	stopCh     chan struct{}
	eventCh    chan memberlist.NodeEvent
	client     k8s.Client
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
	election.Namespace = cfg.Namespace

	mlist, err := memberlist.Create(mconfig)
	election.Memberlist = mlist
	election.client = cfg.client

	return election, err
}

// SetClient configures this announcer to use the provided client.
func (e *Election) SetClient(client *k8s.Client) {
	e.client = client
}

func (e *Election) Join(iplist []string, client *k8s.Client) error {
	go e.watchEvents(client)

	n, err := e.Memberlist.Join(iplist)
	fmt.Println("*** Memberlist hosts found: ", n)
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

	members := e.Memberlist.NumMembers()
	pods, err := e.client.GetPodCount(e.Namespace, mlLabels)

	if err != nil {
		e.logger.Log("op", "Election", "error", err, "msg", "failed to get Pod count")
	}
	fmt.Println("***Members: ", members, "PODS: ", pods)

	nodes := []string{}
	for _, node := range e.Memberlist.Members() {
		nodes = append(nodes, node.Name)
	}

	fmt.Println("***LocalAddr: ", e.Memberlist.LocalNode().Addr.String())
	fmt.Println("***LocalPort: ", e.Memberlist.LocalNode().Port)
	fmt.Println("***LocalName: ", e.Memberlist.LocalNode().Name)
	fmt.Println("***Memberlist: ", e.Memberlist.Members())
	fmt.Println("*** nodes: ", nodes)
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

	fmt.Println("***Candidates :", candidates, "key: ", key)

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
