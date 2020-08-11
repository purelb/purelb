// Copyright 2020 Acnodal Inc.  All rights reserved.

package local

import (
	"net"

	"purelb.io/internal/config"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/go-kit/kit/log"
)

type announcer struct {
	logger     log.Logger
	myNode     string
	nodeLabels labels.Set
	config     *config.Config
}

func NewAnnouncer(l log.Logger, node string) *announcer {
	return &announcer{logger: l, myNode: node}
}

func (c *announcer) SetConfig(cfg *config.Config) error {
	c.logger.Log("event", "newConfig")

	c.config = cfg

	return nil
}

func (c *announcer) SetBalancer(name string, lbIP net.IP) error {
	c.logger.Log("event", "updatedNodes", "msg", "Announce balancer", "service", name)
	return nil
}

func (c *announcer) DeleteBalancer(name, reason string) error {
	c.logger.Log("event", "updatedNodes", "msg", "Delete balancer", "service", name, "reason", reason)
	return nil
}

func (c *announcer) SetNode(node *v1.Node) error {
	c.logger.Log("event", "updatedNodes", "msg", "Node announced", "name", node.Name)
	return nil
}
