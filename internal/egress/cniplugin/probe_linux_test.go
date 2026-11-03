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

package cniplugin

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"purelb.io/internal/egress/cnisock"
)

// TestNetnsProbe sends the stage probe as an ADD asked: from each pod
// address, inside the pod's namespace, then reports what it sent. The
// probe destinations are local in the test namespace, so the datagrams
// can be read. It needs CAP_SYS_ADMIN; PURELB_NETNS_TESTS=required turns
// a skip into a failure.
func TestNetnsProbe(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer orig.Close()
	name := fmt.Sprintf("purelb-probe-test-%d", os.Getpid())
	pod, err := netns.NewNamed(name)
	if err != nil {
		if os.Getenv("PURELB_NETNS_TESTS") == "required" {
			t.Fatalf("creating a network namespace: %v", err)
		}
		t.Skipf("creating a network namespace: %v", err)
	}
	defer func() { _ = netns.DeleteNamed(name) }()
	defer pod.Close()

	// In the pod's namespace: its addresses and the probe destinations on
	// lo, and a listener per family.
	lo, err := netlink.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(lo))
	for _, a := range []string{"10.244.2.9/32", "fd00:244:2::9/128", cnisock.ProbeV4.String() + "/32", cnisock.ProbeV6.String() + "/128"} {
		addr, err := netlink.ParseAddr(a)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, netlink.AddrAdd(lo, addr))
	}
	var listeners []*net.UDPConn
	for _, to := range []net.IP{cnisock.ProbeV4.AsSlice(), cnisock.ProbeV6.AsSlice()} {
		l, err := net.ListenUDP("udp", &net.UDPAddr{IP: to, Port: cnisock.ProbePort})
		require.NoError(t, err)
		defer l.Close()
		listeners = append(listeners, l)
	}
	require.NoError(t, netns.Set(orig))

	sock, reqs := agent(t, cnisock.Response{Result: cnisock.ResultReady, Probe: true, Stage: cnisock.StageMark})
	var out, log bytes.Buffer
	// A third address the namespace doesn't have: not sent, not reported.
	prev := `{"cniVersion":"1.0.0","ips":[{"address":"10.244.2.9/24"},{"address":"fd00:244:2::9/64"},{"address":"10.244.2.10/24"}]}`
	err = plugin{out: &out, log: &log, now: time.Now}.add(&skel.CmdArgs{ContainerID: "c1", Netns: "/var/run/netns/" + name,
		IfName: "eth0", Args: podArgsStr, StdinData: conf(t, "1.0.0", sock, prev)})
	require.NoError(t, err)
	assert.Contains(t, log.String(), "from 10.244.2.10")

	here, err := netns.Get()
	require.NoError(t, err)
	defer here.Close()
	assert.True(t, here.Equal(orig), "back in its own namespace")
	for i, from := range []string{"10.244.2.9", "fd00:244:2::9"} {
		require.NoError(t, listeners[i].SetReadDeadline(time.Now().Add(2*time.Second)))
		buf := make([]byte, 64)
		n, src, err := listeners[i].ReadFromUDP(buf)
		require.NoError(t, err, from)
		assert.Equal(t, from, src.IP.String())
		assert.Equal(t, string(probePayload), string(buf[:n]))
	}
	r := reqs.take()
	require.Len(t, r, 2)
	assert.Equal(t, cnisock.OpProbe, r[1].Op)
	assert.Equal(t, []string{"10.244.2.9", "fd00:244:2::9"}, r[1].IPs)
	assert.Contains(t, log.String(), `"stage":"Mark"`)
}
