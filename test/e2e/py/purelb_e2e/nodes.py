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

"""SSH to cluster nodes and to the upstream router.

This is the ~6% of the bash suite that Python does not improve on: we are
still shelling out to ssh. It is kept small and in one place so the rest
of the harness never constructs a command line.
"""

from __future__ import annotations

import contextlib
import json
import ipaddress
import shlex
import subprocess
import time
from dataclasses import dataclass
from typing import Dict, Iterator, List, Optional, Tuple

SSH_OPTS = [
    "-o", "BatchMode=yes",
    "-o", "ConnectTimeout=10",
    "-o", "StrictHostKeyChecking=accept-new",
]


class SSHError(AssertionError):
    pass


def ssh(host: str, command: str, timeout: float = 60.0, check: bool = True) -> str:
    proc = subprocess.run(  # noqa: S603
        ["ssh", *SSH_OPTS, host, command],
        capture_output=True, text=True, timeout=timeout,
    )
    if check and proc.returncode != 0:
        raise SSHError(
            f"ssh {host} {command!r} exited {proc.returncode}: {proc.stderr.strip()}"
        )
    return proc.stdout


def addresses_on(host: str) -> List[str]:
    """Every address configured on the host, as 'addr/prefix' strings."""
    out = ssh(host, "ip -br addr show")
    found: List[str] = []
    for line in out.splitlines():
        found.extend(tok for tok in line.split() if "/" in tok)
    return found


def has_address(host: str, address: str) -> bool:
    """Whether `address` is configured on `host`, prefix-length agnostic."""
    want = ipaddress.ip_address(address)
    for tok in addresses_on(host):
        try:
            if ipaddress.ip_interface(tok).ip == want:
                return True
        except ValueError:
            continue
    return False


def interface_for_address(host: str, address: str) -> Optional[str]:
    want = ipaddress.ip_address(address)
    for line in ssh(host, "ip -br addr show").splitlines():
        parts = line.split()
        if not parts:
            continue
        for tok in parts[2:]:
            try:
                if ipaddress.ip_interface(tok).ip == want:
                    return parts[0].split("@")[0]
            except ValueError:
                continue
    return None


def mac_for_address(host: str, address: str) -> Optional[str]:
    """The MAC of the interface holding `address`.

    Gratuitous announcements carry the announcing node's MAC -- that is
    their entire payload, the "this address now lives behind me" claim.
    Checking it is what distinguishes a real announcement from the frame
    count merely being non-zero because some other node on the segment
    was talking about the same address.
    """
    iface = interface_for_address(host, address)
    if iface is None:
        return None
    for line in ssh(host, "ip -br link show").splitlines():
        parts = line.split()
        if parts and parts[0].split("@")[0] == iface and len(parts) >= 3:
            return parts[2].lower()
    return None


@dataclass(frozen=True)
class AddressDetail:
    """One configured address as `ip -o addr show` describes it.

    The flags and lifetimes are the whole point of the address-lifetime
    tests: PureLB gives a local VIP a FINITE lifetime so that an orphaned
    address expires by itself if the agent dies without withdrawing it,
    and `noprefixroute` so the kernel does not install a subnet route that
    would blackhole the rest of the subnet.
    """

    interface: str
    cidr: str
    flags: Tuple[str, ...]
    valid_lft: Optional[int]      # seconds; None means "forever"
    preferred_lft: Optional[int]

    @property
    def permanent(self) -> bool:
        return self.valid_lft is None

    def has_flag(self, flag: str) -> bool:
        return flag in self.flags


def address_detail(host: str, address: str) -> Optional[AddressDetail]:
    """Find `address` on `host` and describe it.

    Parses `ip -o addr show`, whose one-line-per-address form keeps the
    lifetimes on the same line as the address -- the multi-line form makes
    them a separate record that shell parsers routinely mis-associate with
    a neighbouring address.
    """
    want = ipaddress.ip_address(address)
    out = ssh(host, "ip -o addr show")
    for line in out.splitlines():
        parts = line.split()
        if len(parts) < 4:
            continue
        iface = parts[1].split("@")[0]
        try:
            cidr = ipaddress.ip_interface(parts[3])
        except ValueError:
            continue
        if cidr.ip != want:
            continue

        rest = parts[4:]
        flags: List[str] = []
        valid = preferred = None
        i = 0
        while i < len(rest):
            token = rest[i]
            # Key/value attributes, whose VALUE must not be mistaken for a
            # flag. Real output carries "metric 100 brd 172.30.250.255",
            # and treating those values as flags makes has_flag("100")
            # true and the flag set meaningless.
            if token in _ADDR_ATTRS:
                if token == "valid_lft":
                    valid = _lifetime(rest[i + 1]) if i + 1 < len(rest) else None
                elif token == "preferred_lft":
                    preferred = _lifetime(rest[i + 1]) if i + 1 < len(rest) else None
                i += 2
                continue
            # ip(8) repeats the interface name at the end of the line,
            # sometimes with the line-continuation backslash attached.
            if token.rstrip("\\") in (iface, ""):
                i += 1
                continue
            flags.append(token)
            i += 1
        return AddressDetail(
            interface=iface, cidr=parts[3], flags=tuple(flags),
            valid_lft=valid, preferred_lft=preferred,
        )
    return None


# Attributes that take a value. Their value is not a flag.
_ADDR_ATTRS = frozenset({
    "valid_lft", "preferred_lft", "metric", "brd", "broadcast",
    "scope", "proto", "label", "peer", "link-netnsid",
})


def _lifetime(token: str) -> Optional[int]:
    """"60sec" -> 60; "forever" -> None."""
    if token == "forever":
        return None
    return int(token.removesuffix("sec"))


def curl_via_node(node_ips: Dict[str, str], address: str, path: str = "/",
                  timeout: float = 30.0,
                  headers: Optional[Dict[str, str]] = None) -> str:
    """HTTP GET a VIP from a cluster node, returning the body.

    From a node rather than from the workstation. The bash suite curled
    from the workstation, which silently exercises nothing when the
    workstation has no route to the VIP subnet -- and then reports the
    empty result as a connectivity failure, or worse, skips the check.

    IPv6 literals are bracketed, which the bash suite's IPv4-shaped URL
    construction did not do.
    """
    host = ipaddress.ip_address(address)
    url = f"http://[{address}]{path}" if host.version == 6 else f"http://{address}{path}"
    node = sorted(node_ips)[0]
    hdr = "".join(f" -H {shlex.quote(f'{k}: {v}')}" for k, v in (headers or {}).items())
    return ssh(node_ips[node], f"curl -s --max-time 5{hdr} {url}", timeout=timeout)


def echo_json(node_ips: Dict[str, str], address: str, path: str = "/",
              timeout: float = 30.0) -> dict:
    """GET a VIP from a node and parse the echo backend's JSON.

    The backend answers text by default -- that form is annotated and is
    what you want when curling by hand -- so the harness asks for JSON
    explicitly. Parsing prose in an assertion is how a test ends up
    matching on a substring and passing against the wrong thing.

    Raises rather than returning a sentinel when the body is not the
    backend's: "the VIP served something" and "the VIP served OUR pod"
    are different claims, and a test asserting the second must not be
    satisfiable by the first.
    """
    body = curl_via_node(node_ips, address, path=path, timeout=timeout,
                         headers={"Accept": "application/json"})
    try:
        parsed = json.loads(body)
    except json.JSONDecodeError as exc:
        raise AssertionError(
            f"{address} did not return the echo backend's JSON. Got "
            f"{body[:200]!r}. Something answered, but not our pod."
        ) from exc
    if "pod" not in parsed:
        raise AssertionError(f"{address} returned JSON without a pod field: {parsed}")
    return parsed


def announcing_node(node_ips: Dict[str, str], address: str) -> Optional[Tuple[str, str]]:
    """The (node, interface) announcing `address`, or None.

    Asks every node rather than trusting the announcing annotation, which
    is what makes this an independent check: the annotation is PureLB
    reporting on itself, the interface is the thing that actually carries
    traffic. A node we cannot reach is skipped rather than fatal, so a
    single unreachable node degrades the search instead of failing it --
    but see the caller, which must not read None as "correctly withdrawn"
    when a node was skipped.
    """
    for name, ip in node_ips.items():
        try:
            iface = interface_for_address(ip, address)
        except (SSHError, subprocess.SubprocessError, OSError):
            continue
        if iface:
            return name, iface
    return None


def nodes_with_address(node_ips: Dict[str, str], address: str) -> List[str]:
    """Every node carrying `address`. Unreachable nodes RAISE.

    Withdrawal assertions need this rather than announcing_node: proving
    an address is gone by asking nodes that did not answer proves nothing,
    so an unreachable node has to be an error, not an empty result.
    """
    found: List[str] = []
    for name, ip in sorted(node_ips.items()):
        if has_address(ip, address):
            found.append(name)
    return found


@dataclass
class Router:
    """The upstream router, for on-the-wire verification.

    A metric proves PureLB called its send path; a capture here proves the
    frame actually reached the gateway, which is what GARP exists to do.
    """

    host: str

    def available(self) -> bool:
        try:
            ssh(self.host, "command -v tcpdump", timeout=15)
            return True
        except (SSHError, subprocess.SubprocessError, OSError):
            return False

    # ------------------------------------------------------------ FRR

    def vtysh(self, command: str) -> str:
        """Run a vtysh command, preferring sudo only if needed."""
        try:
            return ssh(self.host, f"vtysh -c {shlex.quote(command)}", timeout=30)
        except SSHError:
            return ssh(self.host, f"sudo vtysh -c {shlex.quote(command)}", timeout=30)

    def vtysh_json(self, command: str) -> dict:
        """Run a vtysh command with `json` appended and parse the result.

        FRR speaks JSON for every show command that matters here, which
        replaces the bash suite's `grep -oP '\\*\\s+\\K[0-9.]+'`
        next-hop scraping. That regex depends on vtysh's column layout
        and on which next-hop FRR marks with an asterisk; both are
        presentation details that change between FRR releases and would
        silently start matching nothing.
        """
        raw = self.vtysh(f"{command} json").strip()
        if not raw:
            return {}
        try:
            return json.loads(raw)
        except json.JSONDecodeError as exc:
            raise AssertionError(
                f"vtysh {command!r} did not return JSON: {raw[:200]!r}"
            ) from exc

    def route(self, prefix: str) -> Optional[dict]:
        """The RIB entry for `prefix`, or None if the router has no route.

        `prefix` must carry its length ("10.255.0.100/32"). FRR keys the
        response by prefix, and asking for a bare address returns the
        covering route -- which would make a /24 aggregate look like a
        successfully advertised host route.
        """
        family = "ipv6" if ":" in prefix else "ip"
        got = self.vtysh_json(f"show {family} route {prefix}")
        if not got:
            return None
        entries = got.get(prefix) or next(iter(got.values()), None)
        if not entries:
            return None
        return entries[0] if isinstance(entries, list) else entries

    def nexthops(self, prefix: str) -> List[str]:
        """Active next-hop addresses for `prefix`.

        Only next-hops FRR reports as fib/active count: a route can list
        next-hops it has not installed, and those forward nothing.
        """
        entry = self.route(prefix)
        if not entry:
            return []
        out: List[str] = []
        for hop in entry.get("nexthops", []):
            if hop.get("active") or hop.get("fib"):
                address = hop.get("ip") or hop.get("ipv6")
                if address:
                    out.append(address)
        return sorted(set(out))

    def bgp_peers(self) -> Dict[str, dict]:
        """Established BGP peers, keyed by peer address."""
        out: Dict[str, dict] = {}
        for family in ("ipv4Unicast", "ipv6Unicast"):
            summary = self.vtysh_json("show bgp summary").get(family, {})
            for addr, peer in (summary.get("peers") or {}).items():
                if peer.get("state") == "Established" or peer.get("peerState") == "Established":
                    out[addr] = peer
        return out

    def interface_for_subnet(self, address: str) -> str:
        """Which router interface faces the subnet containing `address`."""
        want = ipaddress.ip_address(address)
        for line in ssh(self.host, "ip -br addr show").splitlines():
            parts = line.split()
            if not parts or parts[0] == "lo":
                continue
            for tok in parts[2:]:
                try:
                    iface = ipaddress.ip_interface(tok)
                except ValueError:
                    continue
                if iface.version == want.version and want in iface.network:
                    return parts[0].split("@")[0]
        raise AssertionError(f"no router interface faces {address}")

    @contextlib.contextmanager
    def capture(self, iface: str, bpf: str, settle: float = 1.0,
                seconds: int = 120) -> Iterator["Capture"]:
        """Run tcpdump for the duration of the block.

        Waits `settle` seconds after starting so the trigger inside the
        block cannot race the sniffer coming up -- the single most likely
        way for a wire assertion to be flaky.

        `seconds` caps how long tcpdump lives, so a crashed test cannot
        leave a sniffer running on the router. It MUST exceed everything
        the block waits for: a block that waits 180s for a failover under
        a 120s capture stops recording partway through and reports an
        empty capture, which reads as "the product announced nothing"
        rather than "the sniffer had already exited".
        """
        path = f"/tmp/purelb-capture-{int(time.time() * 1000)}.txt"
        # -e prints the ethernet header. Gratuitous announcements are only
        # meaningful with their source MAC: it is what proves the frame came
        # from the node that won the election rather than from the previous
        # holder still shouting about an address it has given up.
        #
        # The PID goes to a file so teardown can kill exactly this tcpdump.
        # `pkill -f <pattern>` cannot be used: the ssh command line carrying
        # the pattern matches the pattern, so pkill kills its own shell.
        cmd = (
            f"sudo sh -c {shlex.quote(f'nohup timeout {seconds} tcpdump -l -n -e -i {shlex.quote(iface)} {shlex.quote(bpf)} > {path} 2>/dev/null & echo $! > {path}.pid')}"
        )
        ssh(self.host, cmd)
        time.sleep(settle)
        cap = Capture(router=self, path=path)
        try:
            yield cap
        finally:
            # timeout(1) forwards the signal to tcpdump, and tcpdump flushes
            # on exit -- so read the file only after it has gone.
            ssh(self.host,
                f"sudo sh -c 'kill $(cat {path}.pid) 2>/dev/null; sleep 1' || true",
                check=False)
            cap.text = ssh(self.host, f"cat {path} 2>/dev/null || true", check=False)
            ssh(self.host, f"sudo rm -f {path} {path}.pid", check=False)


@dataclass
class Capture:
    router: Router
    path: str
    text: str = ""

    @property
    def lines(self) -> List[str]:
        return [ln for ln in self.text.splitlines() if ln.strip()]

    def count(self) -> int:
        return len(self.lines)
