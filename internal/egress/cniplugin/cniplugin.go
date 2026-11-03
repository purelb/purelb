// Copyright 2026 Acnodal Inc.
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
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cniplugin is the purelb-egress CNI plugin (design 8.1): the
// lbnodeagent binary run under that name (D34). It is chained after the
// node's primary CNI, passes the primary's result through unchanged, and
// asks the node's agent over its socket whether the pod may start, or,
// with no agent, the agent's last snapshot (D35). It holds no API
// credential.
package cniplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/go-kit/log"

	"purelb.io/internal/egress/cnisock"
	"purelb.io/internal/logging"
)

// Name is the plugin's binary name, and the conflist entry's type.
const Name = cnisock.PluginType

// Supported are the CNI versions the plugin accepts: every version with a
// prevResult. A chained plugin is called with its conflist's version
// (flannel's is 0.3.1), and returns the result in that version.
var Supported = version.PluginSupports("0.3.0", "0.3.1", "0.4.0", "1.0.0", "1.1.0")

// Timeouts for the agent's answer. ADD waits out the agent's hold (25 s)
// with room to spare; the others are answered at once.
const (
	addTimeout    = 30 * time.Second
	systemTimeout = 2 * time.Second
	otherTimeout  = 5 * time.Second
)

// defaultNamespace is PureLB's namespace when the conflist entry names
// none.
const defaultNamespace = "purelb-system"

// Conf is the plugin's conflist entry.
type Conf struct {
	types.PluginConf
	// Socket is the agent's socket; empty means cnisock.DefaultPath.
	Socket string `json:"socket,omitempty"`
	// Snapshot is the agent's snapshot; empty means
	// cnisock.SnapshotPath.
	Snapshot string `json:"snapshot,omitempty"`
	// PureLBNamespace is the namespace whose pods, with kube-system's,
	// always pass (D35); empty means purelb-system.
	PureLBNamespace string `json:"purelbNamespace,omitempty"`
	// LogLevel is info (the default) or debug.
	LogLevel string `json:"logLevel,omitempty"`
}

// Main runs the plugin: the CNI command comes from the environment, the
// configuration from stdin, the result goes to stdout and the log to
// stderr (the runtime's log).
func Main() {
	p := plugin{out: os.Stdout, log: os.Stderr, now: time.Now}
	skel.PluginMainFuncs(skel.CNIFuncs{Add: p.add, Del: p.del, Check: p.check, GC: p.gc, Status: p.status},
		Supported, "CNI plugin "+Name+" "+logging.Release())
}

type plugin struct {
	out io.Writer // the CNI result
	log io.Writer // the log
	now func() time.Time
}

// call is one request: the pod's identity, its addresses, and the agent's
// answer.
type call struct {
	conf   *Conf
	logger log.Logger
	req    cnisock.Request
}

// parse reads the configuration and the pod's identity, and the pod's
// addresses from the primary's result.
func (p plugin) parse(args *skel.CmdArgs, op string) (*call, error) {
	conf := &Conf{}
	if err := json.Unmarshal(args.StdinData, conf); err != nil {
		return nil, fmt.Errorf("parsing the configuration: %w", err)
	}
	if err := version.ParsePrevResult(&conf.PluginConf); err != nil {
		return nil, err
	}
	if conf.Socket == "" {
		conf.Socket = cnisock.DefaultPath
	}
	if conf.Snapshot == "" {
		conf.Snapshot = cnisock.SnapshotPath
	}
	if conf.PureLBNamespace == "" {
		conf.PureLBNamespace = defaultNamespace
	}
	c := &call{conf: conf, logger: logging.New(p.log, logging.ParseLevel(conf.LogLevel))}
	k8s := podArgs(args.Args)
	c.req = cnisock.Request{Op: op, ContainerID: args.ContainerID, Netns: args.Netns, IfName: args.IfName,
		Namespace: k8s["K8S_POD_NAMESPACE"], Name: k8s["K8S_POD_NAME"], UID: k8s["K8S_POD_UID"]}
	if conf.PrevResult != nil {
		ips, err := podIPs(conf.PrevResult)
		if err != nil {
			return nil, err
		}
		c.req.IPs = ips
	}
	return c, nil
}

// podArgs reads CNI_ARGS (KEY=value;...) leniently: a runtime adding a
// key never fails a pod.
func podArgs(s string) map[string]string {
	m := map[string]string{}
	for kv := range strings.SplitSeq(s, ";") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// podIPs are the addresses the primary gave the pod: those on its
// sandbox's interfaces, or on no named interface.
func podIPs(prev types.Result) ([]string, error) {
	r, err := current.NewResultFromResult(prev)
	if err != nil {
		return nil, fmt.Errorf("reading prevResult: %w", err)
	}
	var ips []string
	for _, ip := range r.IPs {
		if i := ip.Interface; i != nil && *i >= 0 && *i < len(r.Interfaces) && r.Interfaces[*i].Sandbox == "" {
			continue
		}
		ips = append(ips, ip.Address.IP.String())
	}
	return ips, nil
}

// system is a pod that always passes (D35).
func (c *call) system() bool {
	return c.req.Namespace == "kube-system" || c.req.Namespace == c.conf.PureLBNamespace
}

func (c *call) pod() string {
	return c.req.Namespace + "/" + c.req.Name
}

// ask sends the request to the agent within d. noAgent reports that no
// agent was listening.
func (c *call) ask(d time.Duration) (resp cnisock.Response, noAgent bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	resp, err = cnisock.NewClient(c.conf.Socket).Call(ctx, c.req)
	var op *net.OpError
	noAgent = errors.As(err, &op) && op.Op == "dial"
	return resp, noAgent, err
}

func (p plugin) add(args *skel.CmdArgs) error {
	start := time.Now()
	c, err := p.parse(args, cnisock.OpAdd)
	if err != nil {
		return err
	}
	if c.conf.PrevResult == nil {
		return errors.New(Name + " has no prevResult: it must follow the primary CNI plugin in the chain")
	}
	result := func() error { return p.pass(c.conf) }
	if c.req.Namespace == "" || c.req.Name == "" {
		logging.Info(c.logger, "op", "cniAdd", "container", args.ContainerID, "result", cnisock.ResultPass,
			"msg", "the sandbox names no Kubernetes pod; passed")
		return result()
	}
	logging.Debug(c.logger, "op", "cniAdd", "pod", c.pod(), "uid", c.req.UID, "container", args.ContainerID,
		"ips", strings.Join(c.req.IPs, ","), "msg", "asking the agent")
	if c.system() {
		// Always passes (D35); the agent records the sandbox if it is
		// there, and applies policies once it knows the pod.
		_, _, err := c.ask(systemTimeout)
		logging.Info(c.logger, "op", "cniAdd", "pod", c.pod(), "result", cnisock.ResultPass,
			"agentError", errString(err), "duration", time.Since(start).String(), "msg", "system namespace; passed")
		return result()
	}
	resp, noAgent, err := c.ask(addTimeout)
	switch {
	case noAgent:
		pass, why := c.fromSnapshot(p.now())
		logging.Info(c.logger, "op", "cniAdd", "pod", c.pod(), "agentError", err, "pass", pass,
			"duration", time.Since(start).String(), "msg", "no agent: "+why)
		if !pass {
			return fmt.Errorf("the PureLB agent isn't running on this node, and %s; kubelet retries", why)
		}
		return result()
	case err != nil:
		logging.Info(c.logger, "op", "cniAdd", "pod", c.pod(), "error", err, "msg", "the agent didn't answer; ADD fails and kubelet retries")
		return fmt.Errorf("asking the PureLB agent: %w", err)
	case resp.Error != "":
		logging.Info(c.logger, "op", "cniAdd", "pod", c.pod(), "error", resp.Error, "duration", time.Since(start).String(),
			"msg", "the agent refused the pod for now; kubelet retries")
		return errors.New(resp.Error)
	}
	switch resp.Result {
	case cnisock.ResultReady, cnisock.ResultHeld, cnisock.ResultPass:
	default:
		return fmt.Errorf("the PureLB agent answered %q", resp.Result)
	}
	if resp.Probe {
		c.probe(args.Netns)
	}
	logging.Info(c.logger, "op", "cniAdd", "pod", c.pod(), "result", resp.Result, "duration", time.Since(start).String(), "msg", "pod admitted")
	return result()
}

// probe sends the stage probe the agent asked for from the pod's network
// namespace and reports it (design 6.1). It never fails the ADD: without
// a report the agent asks another pod.
func (c *call) probe(netnsPath string) {
	sent, err := sendProbe(netnsPath, c.req.IPs)
	if err != nil {
		logging.Info(c.logger, "op", "cniProbe", "pod", c.pod(), "error", err, "msg", "the stage probe wasn't sent in every family")
	}
	if len(sent) == 0 {
		return
	}
	report := *c
	report.req.Op, report.req.IPs = cnisock.OpProbe, sent
	resp, _, err := report.ask(otherTimeout)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		logging.Info(c.logger, "op", "cniProbe", "pod", c.pod(), "error", err, "msg", "the stage probe wasn't reported")
		return
	}
	logging.Info(c.logger, "op", "cniProbe", "pod", c.pod(), "ips", strings.Join(sent, ","), "stage", resp.Stage, "msg", "stage probe")
}

// fromSnapshot decides the pod by the agent's snapshot (§8.1), saying
// why.
func (c *call) fromSnapshot(now time.Time) (bool, string) {
	s, err := cnisock.ReadSnapshot(c.conf.Snapshot)
	if err != nil {
		return false, fmt.Sprintf("it has no usable snapshot (%v)", err)
	}
	maxAge := time.Duration(s.MaxAgeSeconds) * time.Second
	if age := now.Sub(s.Written); age > maxAge {
		// A forgotten uninstall never stops pods starting (D35).
		return true, fmt.Sprintf("its snapshot is %s old, past its %s bound: the pod starts without egress",
			age.Round(time.Second), maxAge)
	}
	for _, p := range s.Pods {
		if p.Namespace != c.req.Namespace || p.Name != c.req.Name || (p.UID != "" && c.req.UID != "" && p.UID != c.req.UID) {
			continue
		}
		if p.FailClosed {
			return false, "a FailClosed policy selects the pod in its snapshot"
		}
		return true, "its snapshot has the pod, which no FailClosed policy selects"
	}
	if slices.Contains(s.FailClosedNamespaces, c.req.Namespace) {
		return false, "its snapshot doesn't have the pod, whose namespace a FailClosed policy covers"
	}
	return true, "its snapshot doesn't have the pod, and no FailClosed policy covers its namespace"
}

// pass prints the primary's result, in the configuration's version.
func (p plugin) pass(conf *Conf) error {
	r, err := conf.PrevResult.GetAsVersion(conf.CNIVersion)
	if err != nil {
		return err
	}
	return r.PrintTo(p.out)
}

// del tells the agent the sandbox is gone. It always succeeds (D26): an
// agent not told removes the pod when it leaves the API.
func (p plugin) del(args *skel.CmdArgs) error {
	c, err := p.parse(args, cnisock.OpDel)
	if err != nil {
		// Nothing to tell the agent with; the pod still goes.
		logging.Info(logging.New(p.log, logging.LevelInfo), "op", "cniDel", "container", args.ContainerID, "error", err,
			"msg", "configuration unreadable; deleted without telling the agent")
		return nil
	}
	if c.req.Namespace == "" || c.req.Name == "" {
		return nil
	}
	resp, _, err := c.ask(otherTimeout)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		logging.Info(c.logger, "op", "cniDel", "pod", c.pod(), "container", args.ContainerID, "error", err,
			"msg", "the agent wasn't told; it removes the pod when the pod leaves the API")
		return nil
	}
	logging.Info(c.logger, "op", "cniDel", "pod", c.pod(), "container", args.ContainerID, "msg", "pod removed")
	return nil
}

// check asks the agent whether it has the sandbox. With no agent there is
// nothing to check against, and the sandbox stands.
func (p plugin) check(args *skel.CmdArgs) error {
	c, err := p.parse(args, cnisock.OpCheck)
	if err != nil {
		return err
	}
	if c.req.Namespace == "" || c.req.Name == "" {
		return nil
	}
	resp, noAgent, err := c.ask(otherTimeout)
	switch {
	case noAgent:
		logging.Debug(c.logger, "op", "cniCheck", "pod", c.pod(), "msg", "no agent to check against")
		return nil
	case err != nil:
		return fmt.Errorf("asking the PureLB agent: %w", err)
	case resp.Error != "":
		logging.Info(c.logger, "op", "cniCheck", "pod", c.pod(), "error", resp.Error, "msg", "check failed")
		return errors.New(resp.Error)
	}
	logging.Debug(c.logger, "op", "cniCheck", "pod", c.pod(), "result", resp.Result, "msg", "check passed")
	return nil
}

// gc has nothing to collect: the agent prunes sandboxes whose pods have
// left the API.
func (p plugin) gc(*skel.CmdArgs) error { return nil }

// status always succeeds: a failing STATUS marks the node's network not
// ready, and the agent being down must never do that (D35).
func (p plugin) status(*skel.CmdArgs) error { return nil }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
