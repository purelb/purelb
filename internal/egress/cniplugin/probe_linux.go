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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"

	"github.com/vishvananda/netns"

	"purelb.io/internal/egress/cnisock"
)

// probePayload marks the datagram for anyone who captures it.
var probePayload = []byte("purelb egress stage probe")

// sendProbe sends one probe datagram from each of ips, inside the network
// namespace at netnsPath, and returns the addresses it sent from.
func sendProbe(netnsPath string, ips []string) ([]string, error) {
	target, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", netnsPath, err)
	}
	defer target.Close()
	// The namespace belongs to the thread: keep this goroutine on it, and
	// let the thread go only once it is back in its own.
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	defer orig.Close()
	if err := netns.Set(target); err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("entering %s: %w", netnsPath, err)
	}
	defer func() {
		if netns.Set(orig) == nil {
			runtime.UnlockOSThread()
		}
	}()
	var sent []string
	var errs []error
	for _, s := range ips {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		to := cnisock.ProbeV4
		if ip.Is6() {
			to = cnisock.ProbeV6
		}
		conn, err := net.DialUDP("udp", &net.UDPAddr{IP: ip.AsSlice()}, &net.UDPAddr{IP: to.AsSlice(), Port: cnisock.ProbePort})
		if err == nil {
			_, err = conn.Write(probePayload)
			_ = conn.Close()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("from %s: %w", s, err))
			continue
		}
		sent = append(sent, s)
	}
	return sent, errors.Join(errs...)
}
