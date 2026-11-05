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

// Package cnisock is the protocol between the purelb-egress CNI plugin and
// its node's agent (design 8.1): one JSON request and one JSON response
// per connection on a Unix socket. It is versioned so a plugin and an
// agent one release apart interoperate during a rollout (N±1): each side
// speaks a range of versions, and a client refused a version retries with
// the newest the server speaks, if it speaks that one too.
package cnisock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"
)

// DefaultPath is the agent's socket on the node (hostPath /run/purelb).
const DefaultPath = "/run/purelb/egress.sock"

// Version is the newest version this build speaks, MinVersion the oldest:
// the one before Version once there is one (N±1).
const (
	Version    = 1
	MinVersion = 1
)

// Operations: the CNI's, and PROBE, the plugin's report that it sent the
// stage probe an ADD's answer asked for (design 6.1).
const (
	OpAdd   = "ADD"
	OpDel   = "DEL"
	OpCheck = "CHECK"
	OpProbe = "PROBE"
)

// The stage probe (design 6.1): one UDP datagram per pod family, from the
// new pod's network namespace to a destination that is never used (a
// TEST-NET-2 host, and the RFC 6666 discard-only prefix), which the
// agent's table counts as it passes and drops before it leaves the node.
var (
	ProbeV4 = netip.MustParseAddr("198.51.100.254")
	ProbeV6 = netip.MustParseAddr("100::1")
)

// ProbePort is the probe's UDP destination port (discard).
const ProbePort = 9

// Stages a probe finds: pl_pod_tag's mark reaches classify (Mark), the
// packet reaches it without the mark (SourceIP), or it never reaches
// netfilter (Bypassed: the eBPF stage's case).
const (
	StageMark     = "Mark"
	StageSourceIP = "SourceIP"
	StageBypassed = "Bypassed"
)

// Results of a request the agent answered. Each lets the pod start.
const (
	// ResultReady: the pod's egress is in place, or it has none.
	ResultReady = "ready"
	// ResultHeld: a FailClosed policy selects the pod and its slice isn't
	// ready; its traffic leaving the cluster is dropped until it is.
	ResultHeld = "held"
	// ResultPass: the pod starts without egress for now (FailOpen, or not
	// known to be selected); the agent attaches it later.
	ResultPass = "pass"
)

// ioTimeout bounds reading a request and writing a response; the
// agent's handling (ADD's wait) is the caller's deadline.
const ioTimeout = 5 * time.Second

// Request is one plugin call: the pod's sandbox, and for ADD and DEL its
// addresses from the primary CNI's result.
type Request struct {
	Version     int      `json:"version"`
	Op          string   `json:"op"`
	ContainerID string   `json:"containerID"`
	Netns       string   `json:"netns,omitempty"`
	IfName      string   `json:"ifName,omitempty"`
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	UID         string   `json:"uid,omitempty"`
	IPs         []string `json:"ips,omitempty"`
}

// Response is the agent's answer: a Result, or an Error that fails the
// call. A refused version carries the versions the agent speaks. Probe
// asks the plugin to send the stage probe and report it (ADD).
type Response struct {
	Version int    `json:"version"`
	Result  string `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	Probe   bool   `json:"probe,omitempty"`
	// Retry, on a PROBE's answer: fail the ADD, the pod isn't to start yet.
	Retry      bool `json:"retry,omitempty"`
	MinVersion int  `json:"minVersion,omitempty"`
	MaxVersion int  `json:"maxVersion,omitempty"`
}

// Listen opens the agent's socket at path, replacing one an earlier agent
// left; only root may connect.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing the old socket %s: %w", path, err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// Server answers requests with Handle, speaking versions Min to Max.
type Server struct {
	Min, Max int
	Handle   func(Request) Response
}

// Serve answers each connection on l until l is closed.
func (s Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.answer(c)
	}
}

func (s Server) answer(c net.Conn) {
	defer c.Close()
	var req Request
	_ = c.SetReadDeadline(time.Now().Add(ioTimeout))
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		return
	}
	var resp Response
	if req.Version < s.Min || req.Version > s.Max {
		resp = Response{Error: fmt.Sprintf("protocol version %d not spoken: the agent speaks %d to %d", req.Version, s.Min, s.Max),
			MinVersion: s.Min, MaxVersion: s.Max}
	} else {
		resp = s.Handle(req)
	}
	resp.Version = req.Version
	_ = c.SetWriteDeadline(time.Now().Add(ioTimeout))
	_ = json.NewEncoder(c).Encode(resp)
}

// Client calls the agent at Path, speaking versions Min to Max.
type Client struct {
	Path     string
	Min, Max int
}

// NewClient is a client of this build's versions.
func NewClient(path string) Client {
	return Client{Path: path, Min: MinVersion, Max: Version}
}

// Call sends req at the client's newest version, and again at the
// agent's newest if the agent refuses that one and the client speaks it.
// An error is the call not reaching the agent (no socket: the agent isn't
// running) or a malformed answer; resp.Error is the agent's own.
func (cl Client) Call(ctx context.Context, req Request) (Response, error) {
	req.Version = cl.Max
	resp, err := cl.call(ctx, req)
	if err != nil || resp.Error == "" || resp.MaxVersion == 0 {
		return resp, err
	}
	if v := resp.MaxVersion; v < req.Version && v >= cl.Min {
		req.Version = v
		return cl.call(ctx, req)
	}
	return resp, nil
}

func (cl Client) call(ctx context.Context, req Request) (Response, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", cl.Path)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, fmt.Errorf("sending to the agent: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading the agent's answer: %w", err)
	}
	return resp, nil
}
