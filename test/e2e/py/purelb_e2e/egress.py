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

"""Egress, as the suite configures it.

`--egress` runs the whole suite with egress enabled on every node, so every
LB flow doubles as a check that egress leaves load balancing alone. As with
the address guard, `apply_cr` REPLACES an LBNodeAgent: a body built
without the egress spec would switch egress off partway through the run,
so `agent_body` adds it to every LBNodeAgent the suite writes that doesn't
set one itself.
"""

from __future__ import annotations

import contextlib
from typing import Any, Dict, Iterator

# Set from --egress by conftest.
_SESSION = False
_SUSPENDED = False


def configure(on: bool) -> None:
    global _SESSION
    _SESSION = on


def enabled() -> bool:
    """Whether the session runs with egress on."""
    return _SESSION


def agent_body(body: Dict[str, Any]) -> Dict[str, Any]:
    """body, with egress enabled when the session runs with it and body
    is an LBNodeAgent that doesn't set egress itself."""
    if not _SESSION or _SUSPENDED or body.get("kind") != "LBNodeAgent":
        return body
    spec = body.get("spec") or {}
    if "egress" in spec:
        return body
    return {**body, "spec": {**spec, "egress": {"enabled": True}}}


@contextlib.contextmanager
def suspended() -> Iterator[None]:
    """Writes inside leave the egress spec as the body has it: for putting
    an object back as it was before the run."""
    global _SUSPENDED
    _SUSPENDED = True
    try:
        yield
    finally:
        _SUSPENDED = False


def capable_nodes(cluster) -> Dict[str, bool]:  # noqa: ANN001
    """Each node's EgressNodeState capability.egressCapable."""
    out: Dict[str, bool] = {}
    objs = cluster.custom.list_cluster_custom_object("purelb.io", "v2", "egressnodestates")
    for o in objs.get("items", []):
        cap = (o.get("status") or {}).get("capability") or {}
        out[o["metadata"]["name"]] = bool(cap.get("egressCapable"))
    return out
