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
    def capture(self, iface: str, bpf: str, settle: float = 1.0) -> Iterator["Capture"]:
        """Run tcpdump for the duration of the block.

        Waits `settle` seconds after starting so the trigger inside the
        block cannot race the sniffer coming up -- the single most likely
        way for a wire assertion to be flaky.
        """
        path = f"/tmp/purelb-capture-{int(time.time() * 1000)}.txt"
        cmd = (
            f"sudo nohup timeout 120 tcpdump -l -n -i {shlex.quote(iface)} "
            f"{shlex.quote(bpf)} > {path} 2>/dev/null & echo started"
        )
        ssh(self.host, cmd)
        time.sleep(settle)
        cap = Capture(router=self, path=path)
        try:
            yield cap
        finally:
            ssh(self.host, "sudo pkill -f 'tcpdump -l -n -i' || true", check=False)
            cap.text = ssh(self.host, f"cat {path} 2>/dev/null || true", check=False)
            ssh(self.host, f"sudo rm -f {path}", check=False)


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
