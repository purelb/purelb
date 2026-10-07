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

"""The address guard, as the suite configures it.

`--address-guard` runs the whole suite with the guard enforcing, so every
existing flow doubles as a regression test under it. That only works if
every LBNodeAgent the suite writes carries the setting: `apply_cr`
REPLACES the object, so a body built without it would silently switch the
guard off partway through the run. Every write goes through `local_spec`.
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional

from purelb_e2e import metrics
from purelb_e2e.wait import wait_until

# Set from --address-guard by conftest; None means the suite runs without it.
_SESSION: Optional[Dict[str, Any]] = None

ATTACHED = "purelb_address_guard_attached"
LOADED = "purelb_address_guard_loaded"
RESTART_REQUIRED = "purelb_address_guard_restart_required"


def configure(spec: Optional[Dict[str, Any]]) -> None:
    global _SESSION
    _SESSION = dict(spec) if spec is not None else None


def enabled() -> bool:
    """Whether the session runs with the guard on."""
    return _SESSION is not None


def session_spec() -> Optional[Dict[str, Any]]:
    return dict(_SESSION) if _SESSION is not None else None


def local_spec(local: Dict[str, Any], guard: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    """`local` plus an addressGuard: `guard` if given, else the session's."""
    spec = guard if guard is not None else _SESSION
    if spec is None:
        return dict(local)
    return {**local, "addressGuard": dict(spec)}


def attached(snap: metrics.Snapshot) -> Dict[str, str]:
    """interface -> hook, from one agent's scrape."""
    out: Dict[str, str] = {}
    for key, value in snap.samples.items():
        if not key.startswith(ATTACHED + "{") or value != 1:
            continue
        labels = dict(
            part.split("=", 1) for part in key[len(ATTACHED) + 1 : -1].split(",") if "=" in part
        )
        out[labels.get("interface", "").strip('"')] = labels.get("hook", "").strip('"')
    return out


def applied_everywhere(scrape, node_names: Iterable[str], hook: Optional[str]) -> bool:
    """Whether every node reports the guard attached with `hook` (or, for
    hook=None, attached nowhere). The config is applied live, without a
    restart, so this is the condition a test waits on after changing it."""
    for node in node_names:
        links = attached(scrape(node))
        if hook is None:
            if links:
                return False
        elif not links or any(h != hook for h in links.values()):
            return False
    return True


def needs_restart(snap: metrics.Snapshot) -> bool:
    """Whether this agent was configured with the guard only after it
    started: the program is loaded only at agent startup, so it isn't
    loaded, and turning the guard on takes a restart."""
    return snap.get(RESTART_REQUIRED) == 1 and snap.get(LOADED) == 0


def restart_agents(cluster, node_names: Iterable[str]) -> None:
    """Roll the lbnodeagent DaemonSet and wait for every node's agent."""
    count = len(list(node_names))
    cluster.restart_daemonset(cluster.purelb_namespace, "lbnodeagent")
    wait_until(
        lambda: cluster.daemonset_ready(cluster.purelb_namespace, "lbnodeagent", expect_nodes=count),
        timeout=900, interval=5.0, description="the lbnodeagent rollout",
    )


# Built by `make plugin` into the repo root, as test_plugin_validate uses it.
PLUGIN = Path(__file__).resolve().parents[4] / "kubectl-purelb"


def plugin_json(context: str, *args: str) -> Any:
    """Run the kubectl-purelb binary with -o json and return the parsed output."""
    assert PLUGIN.exists(), f"{PLUGIN} not built; run `make plugin`"
    proc = subprocess.run(  # noqa: S603
        [str(PLUGIN), *args, "--context", context, "-o", "json"],
        capture_output=True, text=True, timeout=120,
    )
    assert proc.returncode == 0, f"kubectl purelb {' '.join(args)} failed: {proc.stderr[:400]}"
    return json.loads(proc.stdout)
