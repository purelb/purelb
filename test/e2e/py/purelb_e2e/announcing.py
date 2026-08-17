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

"""The purelb.io/announcing-<family> annotation.

Wire form is space-separated "node,interface,ip" entries, one per
announced IP:

    node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11

The IP is the SLOT KEY: for any given IP exactly one node is the election
winner and owns that entry. That is the v0.17.0 fix -- the annotation used
to be append-only, so entries accumulated, misreported the announcing node
and broke service-affinity observation. A test that reads it must
therefore check per-IP, not "does this node appear anywhere".

This mirrors ParseAnnouncements in pkg/apis/purelb/v2/announcing.go, and
mirroring it exactly matters: an entry is split on its FIRST comma (node)
and its LAST comma (ip), because Linux interface names may legally contain
commas. A naive split(",") would disagree with the product about what an
annotation means, and the test would be wrong rather than the code.

Parsing is lenient in the same way, too: malformed entries are dropped
rather than raising, because that is what the product does and a test
should describe the product's behaviour, not a stricter invention.
"""

from __future__ import annotations

import ipaddress
from dataclasses import dataclass
from typing import Dict, List, Optional


@dataclass(frozen=True)
class Announcement:
    node: str
    interface: str
    ip: str

    @property
    def address(self) -> ipaddress._BaseAddress:
        return ipaddress.ip_address(self.ip)


def parse(value: Optional[str]) -> List[Announcement]:
    """Decode an annotation value. Unparseable entries are dropped."""
    if not value:
        return []
    out: List[Announcement] = []
    for field in value.split():
        first = field.find(",")
        last = field.rfind(",")
        # Two distinct commas are required for node,interface,ip. This also
        # drops the legacy one-field ("kube-lb0") and two-field
        # ("node,iface") forms, whose IP does not parse.
        if first < 0 or first == last:
            continue
        node = field[:first]
        iface = field[first + 1:last]
        raw = field[last + 1:]
        if not node or not iface:
            continue
        try:
            addr = ipaddress.ip_address(raw)
        except ValueError:
            continue
        # A zoned address (fe80::1%eth0) is rejected by the Go parser, and
        # ipaddress refuses to parse one at all, so the try/except covers it.
        out.append(Announcement(node=node, interface=iface, ip=str(addr)))
    return out


def annotation_key(family: str) -> str:
    """family is "IPv4" or "IPv6"."""
    return f"purelb.io/announcing-{family}"


def by_ip(value: Optional[str]) -> Dict[str, Announcement]:
    """Slots keyed by IP.

    Duplicate IPs collapse to the last entry, which is deliberate: a
    well-formed annotation has exactly one slot per IP, and `len(by_ip) ==
    len(parse)` is how a test detects that the append-only bug has
    returned.
    """
    return {a.ip: a for a in parse(value)}


def announcer_of(value: Optional[str], address: str) -> Optional[str]:
    """Which node announces `address`, per the annotation."""
    slot = by_ip(value).get(str(ipaddress.ip_address(address)))
    return slot.node if slot else None


def has_node(value: Optional[str], node: str) -> bool:
    """Whether `node` announces anything here.

    Entry-wise, comparing the node field exactly. The bash helper this
    replaces was written entry-wise for the same reason: a substring test
    matches "purelb2-1" against a "purelb2-10" entry.
    """
    return any(a.node == node for a in parse(value))
