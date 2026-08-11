# Copyright 2020-2026 Acnodal Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Cluster topology: which nodes are on which subnets, and the pool bands.

Replaces common.sh's discover_nodes / discover_ipv6 / compute_network and
the pool-range helpers. `ipaddress` does the arithmetic that common.sh did
with shell bit-twiddling and a special case per prefix length -- and that
matters beyond tidiness: the shell version fell back to "assume /24" when
it could not read a prefix, which silently produces a pool in a subnet
that does not exist.

The POOL BANDS are load-bearing and must not overlap. Two ServiceGroups
whose ranges overlap are rejected outright by the allocator, so a test
that picks the wrong band fails in a way that looks like a product bug:

    .200-.220   default ServiceGroup          a::1-a::20
    .230-.240   per-test ServiceGroups        b::1-b::20
    .241-.250   timing suite                  (v4 only)
    .244-.247   multi-interface               c::1-c::20
    .248-.250   namespace-scoped tenants      d::1-d::20
    .224/28     external (sidecar) IPAM       e::/120

.241-.250 and .244-.250 do overlap on paper; they belong to suites that
never run concurrently, which is exactly the kind of constraint that
should be written down rather than rediscovered.
"""

from __future__ import annotations

import ipaddress
import re
from dataclasses import dataclass, field
from typing import Dict, List, Optional

from .nodes import ssh


@dataclass
class Subnet:
    """One node subnet, with the IPv6 prefix on the same interface."""

    v4: str                       # "172.30.250.0/24"
    nodes: List[str] = field(default_factory=list)
    v6: Optional[str] = None      # "2001:470:b8f3:250::/64"
    v6_prefix: Optional[str] = None  # "2001:470:b8f3:250::"

    @property
    def v4_octets(self) -> str:
        """First three octets, e.g. "172.30.250"."""
        return ".".join(self.v4.split("/")[0].split(".")[:3])

    def band(self, first: int, last: int) -> str:
        return f"{self.v4_octets}.{first}-{self.v4_octets}.{last}"

    def v6_band(self, group: str, first: int = 1, last: int = 0x20) -> Optional[str]:
        """A v6 range inside `group`, e.g. group "a" -> "…:a::1-…:a::20".

        Formatted in hex because the bash helpers wrote the bounds
        literally as ::1-::20, and ::20 is hex 32, not decimal 20. A
        decimal reading would silently produce a 12-address-smaller pool.
        """
        if not self.v6_prefix:
            return None
        base = self.v6_prefix.rstrip(":")
        return f"{base}:{group}::{first:x}-{base}:{group}::{last:x}"

    # The named bands, so no test writes an octet literal.
    @property
    def default_pool(self) -> str:
        return self.band(200, 220)

    @property
    def test_pool(self) -> str:
        return self.band(230, 240)

    @property
    def default_pool_v6(self) -> Optional[str]:
        return self.v6_band("a")

    @property
    def test_pool_v6(self) -> Optional[str]:
        return self.v6_band("b")


@dataclass
class Topology:
    subnets: List[Subnet]
    node_ips: Dict[str, str]
    node_iface: Dict[str, str]

    @property
    def has_ipv6(self) -> bool:
        return any(s.v6 for s in self.subnets)

    @property
    def multi_subnet(self) -> bool:
        return len(self.subnets) >= 2

    def subnet_of(self, node: str) -> Subnet:
        for s in self.subnets:
            if node in s.nodes:
                return s
        raise AssertionError(f"node {node} is on no known subnet")

    def subnet_holding(self, address: str) -> Optional[Subnet]:
        """The subnet an address belongs to, either family."""
        addr = ipaddress.ip_address(address)
        for s in self.subnets:
            cidr = s.v4 if addr.version == 4 else s.v6
            if cidr and addr in ipaddress.ip_network(cidr):
                return s
        return None


_IFACE_LINE = re.compile(r"^(?P<iface>\S+)\s+\S+\s+(?P<addrs>.*)$")


def discover(node_ips: Dict[str, str]) -> Topology:
    """Probe every node for the interface carrying its InternalIP.

    The interface is found by looking for the node's OWN InternalIP rather
    than by name. common.sh guessed by name, which broke when cloud-init
    renamed purelb2-3's second NIC to eth0.
    """
    by_v4: Dict[str, Subnet] = {}
    node_iface: Dict[str, str] = {}

    for node, ip in sorted(node_ips.items()):
        out = ssh(ip, "ip -br addr show")
        iface, cidr = _iface_carrying(out, ip)
        if iface is None or cidr is None:
            raise AssertionError(
                f"could not find the interface carrying {ip} on {node}; "
                f"`ip -br addr show` said:\n{out}"
            )
        node_iface[node] = iface
        network = str(ipaddress.ip_interface(cidr).network)
        subnet = by_v4.setdefault(network, Subnet(v4=network))
        subnet.nodes.append(node)

        if subnet.v6 is None:
            v6 = _global_v6_on(out, iface)
            if v6:
                subnet.v6 = str(ipaddress.ip_interface(v6).network)
                # The /64 prefix: first four hextets. Derived from the
                # network rather than by string-slicing the address.
                net = ipaddress.ip_network(subnet.v6)
                subnet.v6_prefix = ":".join(str(net.network_address).split(":")[:4]) + "::"

    return Topology(
        subnets=[by_v4[k] for k in sorted(by_v4)],
        node_ips=dict(node_ips),
        node_iface=node_iface,
    )


def _iface_carrying(brief: str, ip: str):
    """(interface, cidr) for the interface holding `ip`."""
    want = ipaddress.ip_address(ip)
    for line in brief.splitlines():
        parts = line.split()
        if not parts:
            continue
        iface = parts[0].split("@")[0]
        for tok in parts[2:]:
            try:
                candidate = ipaddress.ip_interface(tok)
            except ValueError:
                continue
            if candidate.ip == want:
                return iface, tok
    return None, None


def _global_v6_on(brief: str, iface: str) -> Optional[str]:
    """A global-scope IPv6 /64 on `iface`, if any.

    Prefers a /64 over a /128: a /128 is a single address (often a
    VIP or a loopback assignment), and deriving a pool prefix from one
    produces a pool nothing can route to.
    """
    fallback = None
    for line in brief.splitlines():
        parts = line.split()
        if not parts or parts[0].split("@")[0] != iface:
            continue
        for tok in parts[2:]:
            try:
                candidate = ipaddress.ip_interface(tok)
            except ValueError:
                continue
            if candidate.version != 6:
                continue
            if candidate.ip.is_link_local:
                continue
            if candidate.network.prefixlen == 64:
                return tok
            fallback = fallback or tok
    return fallback
