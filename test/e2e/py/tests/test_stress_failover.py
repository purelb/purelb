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

"""Failover under repetition, with the timing varied to shake out races.

Ported from test/e2e/local/stress-failover.sh.

This is a soak test, and it is the only module here whose value comes
from being run more times than a normal run does. One failover working
proves the happy path; the same failover working twenty times with the
shutdown grace period varied, under background churn, is what catches the
window where a withdrawal and a re-announcement cross.

The variations matter more than the count. Each iteration changes the
grace period -- 0 forces an ungraceful kill, 15 gives the agent room to
withdraw cleanly -- and adds a different kind of noise, because the races
worth finding live in the overlap between one node giving an address up
and another taking it.

Default is 3 iterations so it fits an ordinary run. Set
PURELB_STRESS_ITERATIONS higher for a soak; the bash suite's own default
was 10.
"""

from __future__ import annotations

import os
import time
import urllib.request
from typing import Callable, Dict, List, TypeVar

import pytest

from purelb_e2e import TEST_NAMESPACE, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import WaitTimeout, wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
ITERATIONS = int(os.environ.get("PURELB_STRESS_ITERATIONS", 3))

# grace, and what else to do while the address is moving.
VARIATIONS = [
    (10, "quiet"),
    (0, "ungraceful"),     # SIGKILL: no chance to withdraw
    (15, "noisy"),         # a second Service churning alongside
    (5, "cascade"),        # a second node's agent restarts mid-failover
]

pytestmark = pytest.mark.requires("multi-node")


T = TypeVar("T")


def agent_diagnostics(cluster: Cluster, topo: topology.Topology, vip: str) -> str:
    """What every agent pod, node and the VIP look like now: attached to a
    timeout so a failure that only happens inside a full run explains
    itself, rather than needing its evidence (pods, Events) to survive."""
    out: List[str] = []
    ns = cluster.purelb_namespace
    for pod in cluster.pods(ns, "component=lbnodeagent"):
        md, st = pod.metadata, pod.status
        out.append(f"pod {md.name} on {pod.spec.node_name}: phase={st.phase}"
                   f"{' DELETING' if md.deletion_timestamp else ''}")
        for cs in st.container_statuses or []:
            state = cs.state
            now = ("running since " + str(state.running.started_at) if state.running else
                   f"waiting {state.waiting.reason}: {state.waiting.message}" if state.waiting else
                   f"terminated {state.terminated.reason} ({state.terminated.exit_code})" if state.terminated else "?")
            last = cs.last_state.terminated if cs.last_state else None
            out.append(f"  {cs.name}: ready={cs.ready} restarts={cs.restart_count} {now}"
                       + (f"; last terminated {last.reason} ({last.exit_code}) at {last.finished_at}" if last else ""))
            if not cs.ready:
                try:
                    log = cluster.pod_log_text(ns, md.name, container=cs.name, since_seconds=600)
                except Exception as exc:  # noqa: BLE001 - diagnostics must not raise
                    log = f"(log unavailable: {exc!r})"
                for line in log.strip().splitlines()[-20:]:
                    out.append(f"    | {line}")
    for name, ip in sorted(topo.node_ips.items()):
        try:
            with urllib.request.urlopen(f"http://{ip}:7474/readyz?verbose", timeout=3) as r:
                ready = f"{r.status} {r.read().decode(errors='replace').strip().splitlines()[-1:]}"
        except Exception as exc:  # noqa: BLE001
            ready = repr(exc)[:200]
        try:
            holds = nodes.has_address(ip, vip)
        except Exception as exc:  # noqa: BLE001
            holds = repr(exc)[:80]
        out.append(f"node {name}: taints={cluster.taint_keys(name)} holds {vip}={holds} k8gobgp readyz: {ready}")
    try:
        events = cluster.core.list_namespaced_event(ns, field_selector="type=Warning").items
        for e in sorted(events, key=lambda e: e.last_timestamp or e.event_time or e.metadata.creation_timestamp)[-10:]:
            out.append(f"event {e.last_timestamp} {e.involved_object.kind}/{e.involved_object.name} {e.reason}: {e.message}")
    except Exception as exc:  # noqa: BLE001
        out.append(f"events unavailable: {exc!r}")
    return "\n".join(out)


def wait_or_explain(cluster: Cluster, topo: topology.Topology, vip: str, predicate: Callable[[], T], **kw) -> T:
    """wait_until, with agent_diagnostics on a timeout."""
    try:
        return wait_until(predicate, **kw)
    except WaitTimeout as exc:
        raise WaitTimeout(f"{exc}\n--- state at the timeout ---\n{agent_diagnostics(cluster, topo, vip)}") from exc


@pytest.mark.requires("multi-node")
def test_repeated_failover_never_strands_or_duplicates_the_address(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    tainted_nodes,
):
    """Fail the announcer over and over, and check the invariants each time.

    Two invariants, checked after every iteration:

    * EXACTLY ONE node holds the address. Zero is an outage; two is a
      split brain where ARP resolves to whichever replied last. Both are
      states a single failover test can pass through invisibly and a
      repeated one eventually catches.

    * The address still serves traffic. A VIP present on an interface but
      not answering means the move completed at the network layer and not
      at the Service layer.
    """
    vip = lb_service("stress-failover", ["IPv4"])[0]
    holder, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip),
        timeout=60, description=f"{vip} to be announced",
    )

    history: List[str] = [holder]
    for i in range(ITERATIONS):
        grace, flavour = VARIATIONS[i % len(VARIATIONS)]
        current = history[-1]

        tainted_nodes(current)
        pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", current)
        assert pod is not None, f"iteration {i}: no agent on {current}"
        cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=grace)

        if flavour == "noisy":
            # Churn a second Service while the first is moving, so the
            # agent is handling two transitions at once.
            lb_service(f"stress-noise-{i}", ["IPv4"], wait=False)
        elif flavour == "cascade":
            # Restart another node's agent mid-failover: the node that is
            # about to win may be the one restarting.
            other = next((n for n in sorted(topo.node_ips) if n != current), None)
            if other:
                victim = cluster.pod_on_node(
                    cluster.purelb_namespace, "component=lbnodeagent", other
                )
                if victim is not None:
                    cluster.delete_pod(
                        cluster.purelb_namespace, victim.metadata.name, grace_seconds=0
                    )

        moved = wait_or_explain(
            cluster, topo, vip,
            lambda c=current: (lambda f: f if f and f[0] != c else None)(
                nodes.announcing_node(topo.node_ips, vip)
            ),
            timeout=120, interval=2.0,
            description=f"iteration {i} ({flavour}, grace={grace}): {vip} to move off {current}",
        )
        new_holder = moved[0]

        # Exactly one holder, settled rather than sampled: during a move
        # both the old and new node can briefly have it, and checking once
        # can catch that window and call it a split brain.
        holders = wait_until(
            lambda: (lambda got: got if len(got) == 1 else None)(
                nodes.nodes_with_address(topo.node_ips, vip)
            ),
            timeout=90, interval=3.0,
            description=f"iteration {i}: exactly one node to hold {vip}",
        )
        assert holders == [new_holder], (
            f"iteration {i} ({flavour}): {vip} is on {holders} but the announcer "
            f"is {new_holder}"
        )

        assert nodes.echo_json(topo.node_ips, vip)["pod"], (
            f"iteration {i} ({flavour}): {vip} moved to {new_holder} and stopped "
            f"serving; the move finished at the network layer but not at the "
            f"Service layer"
        )

        history.append(new_holder)
        cluster.remove_taint(current, "purelb-test")
        wait_or_explain(
            cluster, topo, vip,
            lambda: cluster.daemonset_ready(
                cluster.purelb_namespace, "lbnodeagent", expect_nodes=len(topo.node_ips)
            ) or None,
            timeout=180, interval=3.0,
            description=f"iteration {i}: the DaemonSet to recover",
        )

    # The address should have genuinely moved around rather than ping-ponging
    # between two nodes, which would mean most of the cluster was never
    # eligible and the stress was not stressing much.
    assert len(set(history)) >= 2, f"the address never moved: {history}"
    print(f"\nfailover path over {ITERATIONS} iterations: {' -> '.join(history)}")
