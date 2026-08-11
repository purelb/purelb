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

"""Multi-interface nodes and interface selection.

Ported from the Subnet-Aware Election group of
test/e2e/local/test-local-allocation.sh.

A node with two NICs announces, by default, only on the interface
carrying its lowest-metric default route -- so its lease advertises one
subnet and the other NIC is invisible to the election. An LBNodeAgent
scoped by nodeSelector can widen that, and then the node can win
elections for addresses on the second subnet and announce them there.

**This group has never run.** The bash version required six environment
variables to be set by hand (MULTI_IF_NODE, _IFACE, _SUBNET, _SUBNET6,
_POOL_V4, _POOL_V6) and skipped itself otherwise, so in practice nobody
set them. Everything here -- including every selector_state assertion --
was dead code. topology.dual_homed discovers the node and both subnets
instead, so the tests run wherever the cluster can support them and skip,
visibly, where it cannot.

On selector_state: `configured` is a WEAK assertion, because any working
LBNodeAgent with a local spec produces it -- the default agent included.
The state worth testing is `deselected`, which is what a node reports
when CRs exist and none of them selects it. That node must then announce
nothing and advertise nothing, because a default fallback there would win
elections it cannot serve: a blackhole.
"""

from __future__ import annotations

import time
from typing import Dict, Iterator, List

import pytest

from purelb_e2e import TEST_NAMESPACE, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
SCOPED_AGENT = "multi-if-test"
SCOPED_GROUP = "multi-if-pool"
LABEL = "purelb-test"

# validLifetime 60 puts the renewal interval at its 30s floor
# (ValidLft/2, minimum 30 -- announcer_local.go scheduleRenewal), which is
# what makes "still absent after 40s" prove the renewal timer died. At the
# default 300 the first re-add is at 150s, so a short wait would pass with
# the bug fully present.
SHORT_LIFETIME = 60
RENEWAL_INTERVAL = 30


@pytest.fixture
def dual_homed(topo: topology.Topology) -> topology.SecondaryInterface:
    found = topo.dual_homed
    if found is None:
        pytest.skip(
            "no node has a second NIC on a different subnet; "
            f"secondaries discovered: {topo.secondaries}"
        )
    return found


@pytest.fixture
def scoped_agent(cluster: Cluster, topo: topology.Topology):
    """A nodeSelector-scoped LBNodeAgent, plus the label it selects on.

    Teardown removes both and waits for the lease to shrink back, so the
    next test starts from the documented baseline rather than from
    whatever this one left. The bash suite had to open with an explicit
    "clean up leftovers from a previous run" block precisely because its
    teardown could be skipped.
    """
    created: List[str] = []
    labelled: List[str] = []

    def make(name: str, node: str, interfaces: List[str]) -> str:
        cluster.label_node(node, LABEL, "multi-if")
        labelled.append(node)
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "LBNodeAgent",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {
                    "nodeSelector": {"matchLabels": {LABEL: "multi-if"}},
                    "local": {
                        "localInterface": "default",
                        "dummyInterface": "kube-lb0",
                        "interfaces": interfaces,
                        "garpConfig": {"enabled": True},
                        "addressConfig": {
                            "localInterface": {
                                "validLifetime": SHORT_LIFETIME,
                                "preferredLifetime": SHORT_LIFETIME,
                            }
                        },
                    },
                },
            }
        )
        created.append(name)
        return name

    yield make

    for name in reversed(created):
        cluster.delete_cr("lbnodeagent", name)
    for node in reversed(labelled):
        cluster.label_node(node, LABEL, None)

    # WAIT for the lease to narrow again. Deleting the CR only starts the
    # process; the agent has to notice, reconfigure and re-annotate. This
    # was a flake: the next test captured its "before" counters while the
    # lease was still wide, applied its own scoped agent, saw
    # local_subnet_count unchanged and failed an assert_increased. It
    # passed in isolation and failed in a full run, which is the worst
    # shape a test failure can take.
    for node in labelled:
        primary = topo.subnet_of(node)
        wait_while(
            lambda n=node, p=primary: any(
                s not in (p.v4, p.v6) for s in cluster.node_lease_subnets(n)
            ),
            timeout=90, interval=2.0,
            description=f"{node}'s lease to narrow back to {primary.v4}",
        )


def lease_has(cluster: Cluster, node: str, subnet: str) -> bool:
    return subnet in cluster.node_lease_subnets(node)


# ------------------------------------------------------------- baseline


def test_second_nic_is_not_advertised_by_default(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    dual_homed: topology.SecondaryInterface,
):
    """The baseline every other test in this module depends on.

    A node advertises the interface carrying its lowest-metric default
    route. If the second NIC's subnet were already in the lease, the
    "gained it" assertions below would pass without the scoped CR doing
    anything.
    """
    subnets = cluster.node_lease_subnets(dual_homed.node)
    primary = topo.subnet_of(dual_homed.node)
    assert primary.v4 in subnets, (
        f"{dual_homed.node} does not advertise its own subnet {primary.v4}: {subnets}"
    )
    assert dual_homed.v4 not in subnets, (
        f"{dual_homed.node} already advertises {dual_homed.v4} without a scoped "
        f"LBNodeAgent, so a leftover CR is in place: {subnets}"
    )


# -------------------------------------------------------------- widening


def test_scoped_agent_adds_the_second_nics_subnets(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    dual_homed: topology.SecondaryInterface, scoped_agent, agent_metrics, log_window,
):
    """A scoped LBNodeAgent widens exactly one node's lease."""
    node = dual_homed.node
    before = agent_metrics(node)
    others = {n: cluster.node_lease_subnets(n) for n in topo.node_ips if n != node}

    scoped_agent(SCOPED_AGENT, node, [topo.node_iface[node], dual_homed.interface])

    wait_until(
        lambda: lease_has(cluster, node, dual_homed.v4) or None,
        timeout=60, interval=2.0,
        description=f"{node}'s lease to gain {dual_homed.v4}",
    )
    if dual_homed.v6:
        wait_until(
            lambda: lease_has(cluster, node, dual_homed.v6) or None,
            timeout=60, interval=2.0,
            description=f"{node}'s lease to gain {dual_homed.v6}",
        )

    after = agent_metrics(node)
    metrics.assert_increased(before, after, "purelb_election_local_subnet_count")
    assert after.value("purelb_lbnodeagent_selector_state", state="configured") == 1.0

    # Scoped means scoped: the nodeSelector matches one node, so no other
    # node's lease may change. A selector that silently matched everything
    # would pass every assertion above.
    for other, subnets in others.items():
        assert cluster.node_lease_subnets(other) == subnets, (
            f"{other}'s lease changed after a CR scoped to {node}"
        )

    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)
    logs = cluster.pod_logs(cluster.purelb_namespace, pod.metadata.name, log_window,
                            container="lbnodeagent")
    assert "subnetsChanged" in logs, f"{node} widened its lease without logging subnetsChanged"


def test_vip_on_the_second_subnet_announces_on_that_nic(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    dual_homed: topology.SecondaryInterface, scoped_agent, lb_service, agent_metrics,
    pinned_backend,
):
    """A dual-stack VIP from the second subnet lands on the second NIC.

    The interface matters as much as the node. An address announced from
    the right node but the wrong interface is not on the second subnet's
    wire at all.

    The announcer is forced with a pinned backend plus node-affinity,
    because the second subnet's OWN nodes are equally valid winners --
    my first version asserted the dual-homed node had won and failed
    against purelb2-1, which was correct behaviour and the wrong
    expectation.
    """
    node = dual_homed.node
    scoped_agent(SCOPED_AGENT, node, [topo.node_iface[node], dual_homed.interface])
    wait_until(
        lambda: lease_has(cluster, node, dual_homed.v4) or None,
        timeout=60, interval=2.0, description=f"{node}'s lease to gain {dual_homed.v4}",
    )

    local: Dict[str, object] = {
        "v4pools": [
            {"aggregation": "default", "pool": dual_homed.pool_v4, "subnet": dual_homed.v4}
        ]
    }
    families = ["IPv4"]
    if dual_homed.v6:
        local["v6pools"] = [
            {"aggregation": "default", "pool": dual_homed.pool_v6, "subnet": dual_homed.v6}
        ]
        families.append("IPv6")
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": SCOPED_GROUP, "namespace": cluster.purelb_namespace},
            "spec": {"local": local},
        }
    )
    try:
        before = agent_metrics(node)
        backend = pinned_backend("multi-if-backend", node)
        ips = lb_service(
            "multi-if-lb", families,
            annotations={
                "purelb.io/service-group": SCOPED_GROUP,
                "purelb.io/node-affinity": "service-endpoints",
            },
            selector={"app": backend},
            timeout=90,
        )
        assert len(ips) == len(families), f"expected {families}, got {ips}"

        for address in ips:
            holder = wait_until(
                lambda a=address: nodes.announcing_node(topo.node_ips, a),
                timeout=60, interval=2.0,
                description=f"{address} to be announced",
            )
            assert holder[0] == node, (
                f"{address} is from the second subnet but is announced by "
                f"{holder[0]}, not the dual-homed node {node}"
            )
            assert holder[1] == dual_homed.interface, (
                f"{address} is on {holder[1]} on {node}, not on "
                f"{dual_homed.interface} which faces {dual_homed.v4}"
            )

        # NA/GARP is asserted in test_affinity_placed_address_announces_itself
        # below, which is xfail: this address is affinity-placed, and
        # affinity-placed addresses currently never send either.
    finally:
        cluster.delete_service(NAMESPACE, "multi-if-lb")
        cluster.delete_cr("servicegroup", SCOPED_GROUP)


def test_config_shrink_withdraws_and_kills_the_renewal_timer(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    dual_homed: topology.SecondaryInterface, scoped_agent, lb_service, log_window,
):
    """Removing the scoped CR withdraws the address, timer and all.

    The sharp assertion in this module. Withdrawal has to CANCEL the
    renewal timer, not merely remove the address once: a surviving timer
    re-adds it moments later, on an interface the agent no longer
    believes it owns, and nothing withdraws it again.

    The wait is sized against the renewal interval on purpose. With
    validLifetime 60 the interval is at its 30s floor, so absence after
    40s means the timer is gone. At the default 300 the first re-add is at
    150s and this would pass with the bug fully present -- which is why
    the fixture sets the lifetime rather than leaving it to chance.
    """
    node = dual_homed.node
    scoped_agent(SCOPED_AGENT, node, [topo.node_iface[node], dual_homed.interface])
    wait_until(
        lambda: lease_has(cluster, node, dual_homed.v4) or None,
        timeout=60, interval=2.0, description=f"{node}'s lease to gain {dual_homed.v4}",
    )

    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": SCOPED_GROUP, "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "v4pools": [
                        {"aggregation": "default", "pool": dual_homed.pool_v4,
                         "subnet": dual_homed.v4}
                    ]
                }
            },
        }
    )
    try:
        vip = lb_service(
            "multi-if-shrink", ["IPv4"],
            annotations={"purelb.io/service-group": SCOPED_GROUP}, timeout=90,
        )[0]
        wait_until(
            lambda: nodes.announcing_node(topo.node_ips, vip),
            timeout=60, interval=2.0, description=f"{vip} to be announced",
        )

        # Shrink the config out from under it.
        cluster.delete_cr("lbnodeagent", SCOPED_AGENT)

        wait_while(
            lambda: lease_has(cluster, node, dual_homed.v4),
            timeout=60, interval=2.0,
            description=f"{node}'s lease to lose {dual_homed.v4}",
        )
        wait_while(
            lambda: nodes.has_address(topo.node_ips[node], vip),
            timeout=60, interval=2.0,
            description=f"{vip} to be withdrawn from {node}",
        )

        pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)
        logs = cluster.pod_logs(cluster.purelb_namespace, pod.metadata.name, log_window,
                                container="lbnodeagent")
        assert "withdrawAddress" in logs, (
            f"{node} dropped {vip} without logging withdrawAddress"
        )

        # The timer must be dead, not merely between ticks.
        deadline = time.monotonic() + RENEWAL_INTERVAL + 10
        while time.monotonic() < deadline:
            time.sleep(3)
            assert not nodes.has_address(topo.node_ips[node], vip), (
                f"{vip} reappeared on {node} after the config shrank: the "
                f"renewal timer survived the withdrawal and is re-adding an "
                f"address the agent no longer owns"
            )
    finally:
        cluster.delete_service(NAMESPACE, "multi-if-shrink")
        cluster.delete_cr("servicegroup", SCOPED_GROUP)


# ----------------------------------------------------- the blackhole guard


@pytest.mark.requires("multi-node")
def test_a_deselected_node_advertises_nothing(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    dual_homed: topology.SecondaryInterface, scoped_agent, lbnodeagent: str,
):
    """A node no CR selects must announce nothing at all.

    This is the state worth testing -- `configured` is produced by any
    working agent, the default one included, so asserting it proves
    nothing. When CRs exist and none selects a node, falling back to
    default detection would have that node win elections it cannot serve:
    a blackhole. So it reports `deselected` and advertises an empty
    lease.

    New coverage: the bash suite asserted only `configured`, and only
    inside the group that never ran.
    """
    node = dual_homed.node
    scoped_agent(SCOPED_AGENT, node, [topo.node_iface[node], dual_homed.interface])
    # With the default agent gone, the scoped CR is the ONLY one, so every
    # other node is deselected.
    cluster.delete_cr("lbnodeagent", "default")
    try:
        victim = next(n for n in sorted(topo.node_ips) if n != node)
        wait_until(
            lambda: metrics.scrape_node(topo.node_ips[victim]).value(
                "purelb_lbnodeagent_selector_state", state="deselected"
            ) == 1.0 or None,
            timeout=90, interval=3.0,
            description=f"{victim} to report selector_state=deselected",
        )
        wait_until(
            lambda: cluster.node_lease_subnets(victim) == [] or None,
            timeout=60, interval=2.0,
            description=f"{victim}'s lease to advertise nothing",
        )
        # And the selected node is unaffected.
        assert metrics.scrape_node(topo.node_ips[node]).value(
            "purelb_lbnodeagent_selector_state", state="configured"
        ) == 1.0
    finally:
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "LBNodeAgent",
                "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
                "spec": {"local": {"localInterface": "default", "dummyInterface": "kube-lb0"}},
            }
        )


# ------------------------------------------------- gratuitous announcement


@pytest.mark.xfail(
    strict=True,
    reason=(
        "BUG: affinity-placed addresses never send GARP or unsolicited NA. "
        "Placement uses election.WinnerWithPreference (announcer_local.go:491) "
        "but sendGARPSequence's verify-before-send uses election.Winner "
        "(announcer_local.go:1248). With purelb.io/node-affinity the two "
        "disagree, so every packet is skipped with 'no longer winner'. "
        "strict=True: when this is fixed the test fails until the marker is "
        "removed, so the fix cannot land silently."
    ),
)
def test_affinity_placed_address_announces_itself(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    lb_service, pinned_backend, agent_metrics,
):
    """A node that announces an address must gratuitously announce it.

    GARP (IPv4) and unsolicited NA (IPv6) are what update the upstream
    switch and router caches. Without them a VIP that moves keeps
    receiving no traffic until those caches age out -- minutes of
    blackhole for a failover that PureLB itself completed in seconds.

    Verified by hand against this cluster:
      - no affinity: announcer purelb2-2, garp_sent_total 0 -> 3
      - with affinity: announcer purelb2-1, garp_sent_total stays 0, and
        the agent logs "skipping announcement, no longer winner,
        winner: purelb2-2"
    """
    # garpConfig is +optional with no struct-level default, so an agent
    # without the block sends nothing at all -- the counter is
    # structurally zero and any assertion on it would be vacuous.
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "LBNodeAgent",
            "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "localInterface": "default",
                    "dummyInterface": "kube-lb0",
                    "garpConfig": {"enabled": True},
                }
            },
        }
    )
    try:
        # Pin the backend to a node, then let affinity place the address
        # there. Which node does not matter, only that affinity is what
        # chose it.
        target = sorted(topo.node_ips)[0]
        backend = pinned_backend("garp-backend", target)
        before = agent_metrics(target)
        vip = lb_service(
            "garp-affinity", ["IPv4"],
            annotations={"purelb.io/node-affinity": "service-endpoints"},
            selector={"app": backend}, timeout=90,
        )[0]
        holder, _ = wait_until(
            lambda: nodes.announcing_node(topo.node_ips, vip),
            timeout=60, interval=2.0, description=f"{vip} to be announced",
        )
        assert holder == target, f"affinity did not place {vip} on {target}"

        wait_until(
            lambda: agent_metrics(holder).counter("purelb_lbnodeagent_garp_sent_total")
            > before.counter("purelb_lbnodeagent_garp_sent_total") or None,
            timeout=45, interval=3.0,
            description=f"{holder} to send a gratuitous ARP for {vip}",
        )
    finally:
        cluster.delete_service(NAMESPACE, "garp-affinity")
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "LBNodeAgent",
                "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
                "spec": {"local": {"localInterface": "default", "dummyInterface": "kube-lb0"}},
            }
        )
