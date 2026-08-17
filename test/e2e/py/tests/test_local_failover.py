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

"""Failover: the VIP moves when its announcer goes away.

Ported from the Failover & High Availability group of
test/e2e/local/test-local-allocation.sh.

The taints these tests apply are the reason the bash suite had a
persistent cleanup hazard: `purelb-test=...:NoExecute` was applied in
three tests and removed at six inline sites, one before each `fail` on the
path. A fail raised inside a helper skipped every one of them and left a
node unschedulable for the rest of that run and every run after it. Here
the `tainted_nodes` fixture removes them whatever the test does.

The announcing-annotation assertions are the sharp end of this group.
Before v0.17.0 the annotation was append-only, so a failover left the
downed node listed alongside the new winner: the annotation misreported
who was announcing, and service-affinity observation broke. The IP is now
the slot key and the winner overwrites its slot, so the assertion is
per-IP -- "which node announces this address" -- not "does this node
appear anywhere".
"""

from __future__ import annotations

from typing import Dict, List

import pytest

from purelb_e2e import TEST_NAMESPACE, announcing, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
AGENT_SELECTOR = "component=lbnodeagent"


def announcing_annotation(cluster: Cluster, name: str, family: str = "IPv4") -> str:
    return cluster.annotation(NAMESPACE, name, announcing.annotation_key(family)) or ""


@pytest.mark.requires("multi-node")
def test_vip_fails_over_when_its_announcer_is_evicted(
    cluster: Cluster,
    topo: topology.Topology,
    default_servicegroup: str,
    lb_service,
    tainted_nodes,
    agent_metrics,
    log_window,
):
    """Evict the announcing agent; the VIP moves and keeps serving."""
    name = "nginx-lb-failover"
    vip = lb_service(name, ["IPv4"])[0]
    agent_before = {n: agent_metrics(n) for n in topo.node_ips}

    original, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
        description=f"{vip} to be announced",
    )
    assert nodes.echo_json(topo.node_ips, vip)["pod"], f"{vip} was not serving before failover"
    assert announcing.announcer_of(announcing_annotation(cluster, name), vip) == original, (
        "the annotation disagrees with the interface before we even start"
    )

    # NoExecute plus a graceful delete: the grace period is what lets the
    # agent withdraw the address rather than orphaning it on the interface.
    tainted_nodes(original)
    pod = cluster.pod_on_node(cluster.purelb_namespace, AGENT_SELECTOR, original)
    assert pod is not None, f"no lbnodeagent pod on {original}"
    cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)

    # Withdrawn from the old winner...
    wait_while(
        lambda: nodes.has_address(topo.node_ips[original], vip),
        timeout=60, interval=2.0,
        description=f"{vip} to be withdrawn from {original}",
    )

    # ...and picked up by a different one.
    new_winner, _ = wait_until(
        lambda: (lambda f: f if f and f[0] != original else None)(nodes.announcing_node(topo.node_ips, vip)),
        timeout=60, interval=2.0,
        description=f"{vip} to be announced by a node other than {original}",
    )

    if topo.multi_subnet:
        subnet = topo.subnet_holding(vip)
        assert new_winner in subnet.nodes, (
            f"failover moved {vip} to {new_winner}, which is on "
            f"{topo.subnet_of(new_winner).v4}, not {subnet.v4}"
        )

    assert nodes.echo_json(topo.node_ips, vip)["pod"], f"{vip} stopped serving after failover"

    # The annotation must converge while the old winner is still DOWN. Its
    # agent cannot clear its own slot, so the new winner has to overwrite
    # the IP's slot. An append-only annotation leaves both listed, which is
    # this bug's exact shape.
    wait_until(
        lambda: announcing.announcer_of(announcing_annotation(cluster, name), vip) == new_winner
        or None,
        timeout=45, interval=2.0,
        description=f"announcing-IPv4 to name {new_winner} for {vip}",
    )
    value = announcing_annotation(cluster, name)
    assert not announcing.has_node(value, original), (
        f"announcing-IPv4 still lists the downed node {original}: {value!r}"
    )
    # The new winner must have won on its OWN terms: its counters moved
    # and it logged the win. Asserting only that the address moved would
    # be satisfied by an address that drifted there without an election.
    metrics.assert_increased(
        agent_before[new_winner], agent_metrics(new_winner),
        "purelb_lbnodeagent_election_wins_total",
    )
    pod = cluster.pod_on_node(cluster.purelb_namespace, AGENT_SELECTOR, new_winner)
    assert pod is not None
    logs = cluster.pod_logs(cluster.purelb_namespace, pod.metadata.name, log_window,
                            container="lbnodeagent")
    assert "electionWon" in logs, (
        f"{new_winner} took over {vip} without logging electionWon"
    )

    slots = announcing.parse(value)
    assert len(slots) == len(announcing.by_ip(value)), (
        f"announcing-IPv4 has more entries than allocated IPs, so slots are "
        f"accumulating rather than being overwritten: {value!r}"
    )


@pytest.mark.requires("multi-node")
def test_agents_recover_after_the_taint_is_removed(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    tainted_nodes,
):
    """A tainted-out agent comes back, and the DaemonSet is whole again.

    Separate from the failover test on purpose: recovery is what makes the
    NEXT test's assumptions valid, so it deserves to fail on its own terms
    rather than as a late assertion in a test about something else.
    """
    vip = lb_service("nginx-lb-recover", ["IPv4"])[0]
    holder, _ = wait_until(lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
                           description=f"{vip} announced")

    tainted_nodes(holder)
    pod = cluster.pod_on_node(cluster.purelb_namespace, AGENT_SELECTOR, holder)
    assert pod is not None
    cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)
    wait_until(
        lambda: cluster.pod_on_node(cluster.purelb_namespace, AGENT_SELECTOR, holder) is None,
        timeout=60, interval=2.0,
        description=f"the lbnodeagent on {holder} to go away",
    )

    cluster.remove_taint(holder, "purelb-test")

    # expect_nodes is required here, not optional decoration: while the
    # taint is in effect the DaemonSet reports 4/4 ready on a 5-node
    # cluster and looks perfectly healthy.
    wait_until(
        lambda: cluster.daemonset_ready(
            cluster.purelb_namespace, "lbnodeagent", expect_nodes=len(topo.node_ips)
        ) or None,
        timeout=180, interval=3.0,
        description=f"the lbnodeagent DaemonSet to be ready on all {len(topo.node_ips)} nodes",
    )
    running = [
        p for p in cluster.pods(cluster.purelb_namespace, AGENT_SELECTOR)
        if p.status.phase == "Running"
    ]
    assert len(running) == len(topo.node_ips), (
        f"{len(running)} lbnodeagent pods running, expected {len(topo.node_ips)}"
    )


@pytest.mark.requires("multi-subnet")
def test_failover_stays_within_the_addresss_subnet(
    cluster: Cluster,
    topo: topology.Topology,
    default_servicegroup: str,
    lb_service,
    subnet_servicegroup,
    tainted_nodes,
):
    """A VIP may only move to a node on its own subnet.

    Moving 172.30.251.x onto a node in 172.30.250.0/24 puts the address
    where its gateway cannot ARP for it: the service is announced, and
    unreachable.
    """
    # Pick the subnet with the most nodes so there is somewhere to fail over to.
    subnet = max(topo.subnets, key=lambda s: len(s.nodes))
    if len(subnet.nodes) < 2:
        pytest.skip("no subnet has two nodes, so there is nowhere to fail over within one")

    # A ServiceGroup scoped to this subnet, not purelb.io/addresses: that
    # annotation takes specific addresses and rejects a range.
    group = subnet_servicegroup("subnet-failover-pool", subnet)
    name = "nginx-lb-subnet-failover"
    vip = lb_service(name, ["IPv4"], annotations={"purelb.io/service-group": group})[0]
    assert topo.subnet_holding(vip).v4 == subnet.v4, (
        f"{vip} did not come from {subnet.v4}"
    )

    original, _ = wait_until(lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
                             description=f"{vip} announced")
    assert original in subnet.nodes

    tainted_nodes(original)
    pod = cluster.pod_on_node(cluster.purelb_namespace, AGENT_SELECTOR, original)
    assert pod is not None
    cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)

    new_winner, _ = wait_until(
        lambda: (lambda f: f if f and f[0] != original else None)(nodes.announcing_node(topo.node_ips, vip)),
        timeout=60, interval=2.0,
        description=f"{vip} to move off {original}",
    )
    assert new_winner in subnet.nodes, (
        f"{vip} is from {subnet.v4} but failed over to {new_winner} on "
        f"{topo.subnet_of(new_winner).v4}; its gateway cannot reach it there"
    )
