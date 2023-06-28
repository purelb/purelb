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
// See the License for the specific language governing permissions and
// limitations under the License.

package v1

import (
	"bytes"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"

	go_cidr "github.com/apparentlymart/go-cidr/cidr"
	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink/nl"
)

type IPRange struct {
	from net.IP
	to   net.IP
}

// NewIPRange parses a string representation of an IP address range
// and returns the corresponding IPRange.  The representation can be
// in either of two forms: CIDR or from-to.  CIDR looks like
// "192.168.1.0/24" and from-to looks like "192.168.1.0 -
// 192.168.1.255". The error return value will be non-nil if the
// representation couldn't be parsed.
func NewIPRange(raw string) (IPRange, error) {
	if strings.Contains(raw, "-") {
		// "from-to" notation
		return parseFromTo(raw)
	}

	// CIDR notation
	return parseCIDR(raw)
}

var IPRangeComparer = cmp.Comparer(func(x, y IPRange) bool {
	return reflect.DeepEqual(x.from, y.from) && reflect.DeepEqual(x.to, y.to)
})

// Overlaps indicates whether the other IPRange overlaps with this one
// (i.e., has any addresses in common).  It returns true if there are
// any common addresses and false if there aren't.
func (r IPRange) Overlaps(other IPRange) bool {
	if (bytes.Compare(other.from.To16(), r.from.To16()) >= 0 && bytes.Compare(other.from.To16(), r.to.To16()) <= 0) ||
		(bytes.Compare(other.to.To16(), r.from.To16()) >= 0 && bytes.Compare(other.to.To16(), r.to.To16()) <= 0) {
		return true
	}

	return false
}

// Contains indicates whether the provided net.IP represents an
// address within this IPRange.  It returns true if so, false
// otherwise.
func (r IPRange) Contains(ip net.IP) bool {
	if bytes.Compare(ip.To16(), r.from.To16()) >= 0 && bytes.Compare(ip.To16(), r.to.To16()) <= 0 {
		return true
	}

	return false
}

// ContainedBy indicates whether this range's addresses are contained
// by cidr.
func (p IPRange) ContainedBy(cidr net.IPNet) bool {
	return cidr.Contains(p.from) && cidr.Contains(p.to)
}

// Family returns the IP family of the addresses in this range. The
// return value will be nl.FAMILY_V6 if this is an IPV6 range,
// nl.FAMILY_V4 if it's IPV4, or 0 if the family can't be determined.
func (r IPRange) Family() int {
	return AddrFamily(r.from)
}

// First returns the first (i.e., lowest-valued) net.IP within this
// IPRange.
func (r IPRange) First() net.IP {
	return dup(r.from)
}

// Next returns the next net.IP within this IPRange, or nil if the
// provided net.IP is the last address in the range or is not
// contained by this range.
func (r IPRange) Next(ip net.IP) net.IP {
	if bytes.Compare(ip.To16(), r.from.To16()) < 0 || bytes.Compare(ip.To16(), r.to.To16()) >= 0 {
		return nil
	}
	next := dup(ip)
	inc(next)
	return next
}

// Size returns the count of net.IPs contained in this IPRange.  If
// the count is too large to be represented by a uint64 then the
// return value will be math.MaxUint64.
func (r IPRange) Size() uint64 {
	// if it's an IPV6 then the range might be too big to represent in
	// an int64
	if r.from.To4() == nil {
		// if any of the first 8 bytes of the address are different then
		// the size will be too large to fit in an int64 so just return
		// the biggest number we can
		for i := 0; i < 8; i++ {
			if r.to[i] != r.from[i] {
				return math.MaxUint64
			}
		}
	}

	// if we're here then the size should be representable in an int64

	// We add 1 because the range is inclusive, i.e., the addresses at
	// both ends are available for allocation.  So, for example, if the
	// IPRange is 1.1.1.1/32 there's one address available.
	return 1 + toInt(r.to.To16()) - toInt(r.from.To16())
}

// String returns a human-readable representation of this range.
func (r IPRange) String() string {
	return fmt.Sprintf("(%s - %s)", r.from.String(), r.to.String())
}

func parseCIDR(cidr string) (IPRange, error) {
	var (
		err     error
		sn      *net.IPNet
		iprange = IPRange{}
	)
	iprange.from, sn, err = net.ParseCIDR(cidr)
	if err != nil {
		return iprange, fmt.Errorf("invalid CIDR %q", cidr)
	}
	_, iprange.to = go_cidr.AddressRange(sn)

	return iprange, nil
}

func parseFromTo(rawrange string) (IPRange, error) {
	fs := strings.SplitN(rawrange, "-", 2)
	if len(fs) != 2 {
		return IPRange{}, fmt.Errorf("invalid IP range %q: need two addresses", rawrange)
	}
	from := net.ParseIP(strings.TrimSpace(fs[0]))
	if from == nil {
		return IPRange{}, fmt.Errorf("invalid IP range %q: invalid start IP %q", rawrange, fs[0])
	}
	to := net.ParseIP(strings.TrimSpace(fs[1]))
	if to == nil {
		return IPRange{}, fmt.Errorf("invalid IP range %q: invalid end IP %q", rawrange, fs[1])
	}

	return IPRange{from: from, to: to}, nil
}

// toInt converts the provided address into a uint64.
func toInt(ip net.IP) uint64 {
	var n uint64
	for i := 0; i < len(ip); i++ {
		n *= 256
		n = n + uint64(ip[i])
	}
	return n
}

func inc(ip net.IP) {
	// https://gist.github.com/kotakanbe/d3059af990252ba89a82
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func dup(ip net.IP) net.IP {
	// https://stackoverflow.com/a/29732469/5967960
	dup := make(net.IP, len(ip))
	copy(dup, ip)
	return dup
}

// AddrFamily returns whether lbIP is an IPV4 or IPV6 address.  The
// return value will be nl.FAMILY_V6 if the address is an IPV6
// address, nl.FAMILY_V4 if it's IPV4, or 0 if the family can't be
// determined.
func AddrFamily(lbIP net.IP) (lbIPFamily int) {
	if nil != lbIP.To16() {
		lbIPFamily = nl.FAMILY_V6
	}

	if nil != lbIP.To4() {
		lbIPFamily = nl.FAMILY_V4
	}

	return
}
