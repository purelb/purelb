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
SERVICE_GROUP = "purelb.io/service-group"
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


@pytest.fixture
def selector_probe(cluster: Cluster, topo: topology.Topology):
    """A nodeSelector-scoped LBNodeAgent whose spec the caller chooses.

    scoped_agent always builds a VALID local spec, so it can only ever
    produce selector_state=configured. The states that matter for the
    blackhole guard come from a spec that is broken or absent, so this
    one takes the spec verbatim.

    Only the labelled node is affected: a specific nodeSelector outranks
    the catch-all `default` agent, so every other node keeps that one.
    The tests assert that containment rather than assume it -- a change
    that let one bad CR reconfigure the whole cluster would otherwise
    look exactly like a pass.
    """
    created: List[str] = []
    labelled: List[str] = []

    def make(name: str, node: str, spec: Dict[str, object]) -> str:
        cluster.label_node(node, LABEL, "multi-if")
        labelled.append(node)
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "LBNodeAgent",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {"nodeSelector": {"matchLabels": {LABEL: "multi-if"}}, **spec},
            }
        )
        created.append(name)
        return name

    yield make

    for name in reversed(created):
        cluster.delete_cr("lbnodeagent", name)
    for node in reversed(labelled):
        cluster.label_node(node, LABEL, None)
    # Do not leave until the node is announcing again. Both states this
    # fixture produces stop the node announcing, so returning early would
    # hand the next test a cluster with a silent node and a failure that
    # points anywhere but here.
    for node in labelled:
        wait_until(
            lambda n=node: metrics.scrape_node(topo.node_ips[n]).value(
                "purelb_lbnodeagent_selector_state", state="configured"
            ) == 1.0 or None,
            timeout=120, interval=3.0,
            description=f"{node} to be configured by the default agent again",
        )


def agent_events(cluster: Cluster, name: str) -> List[str]:
    """Event messages recorded against one LBNodeAgent.

    Deliberately unsorted. Sorting on `last_timestamp or event_time or 0`
    mixes datetimes with an int whenever an event carries neither, which
    raises TypeError inside the assertion and reports as a broken test
    rather than a missing event -- and the caller only asks whether a
    message is present.
    """
    got = cluster.core.list_namespaced_event(
        cluster.purelb_namespace, field_selector=f"involvedObject.name={name}"
    )
    return [e.message for e in got.items if e.message]


def lease_has(cluster: Cluster, node: str, subnet: str) -> bool:
    return subnet in cluster.node_lease_subnets(node)


def wait_for_settled_leases(cluster: Cluster, topo: topology.Topology) -> Dict[str, List[str]]:
    """Every node advertising its own subnets, and nothing more.

    A precondition, not an assertion. An agent that has just restarted --
    which the failover tests leave behind when they run first -- has an
    empty lease for a few seconds. Snapshotting that as "before" made the
    scoping check compare [] against the correct value and report that a
    CR scoped to another node had changed this one's lease: a real
    assertion, failing on a garbage baseline.
    """
    def settled():
        current = {}
        for node in topo.node_ips:
            subnets = set(cluster.node_lease_subnets(node))
            expected = {s for s in (topo.subnet_of(node).v4, topo.subnet_of(node).v6) if s}
            if subnets != expected:
                return None
            current[node] = sorted(subnets)
        return current

    return wait_until(
        settled, timeout=120, interval=3.0,
        description="every node's lease to settle on exactly its own subnets",
    )


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
    wait_for_settled_leases(cluster, topo)
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
    settled = wait_for_settled_leases(cluster, topo)
    before = agent_metrics(node)
    others = {n: v for n, v in settled.items() if n != node}

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
        assert sorted(cluster.node_lease_subnets(other)) == subnets, (
            f"{other}'s lease changed after a CR scoped to {node}: "
            f"{subnets} -> {sorted(cluster.node_lease_subnets(other))}"
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

        # An unsolicited Neighbour Advertisement is the IPv6 counterpart
        # of a gratuitous ARP: without it the upstream neighbour caches
        # keep pointing at whoever held the address before. This address
        # is affinity-placed, which is precisely the case that used to
        # send nothing at all.
        if dual_homed.v6:
            wait_until(
                lambda: agent_metrics(node).counter("purelb_lbnodeagent_na_sent_total")
                > before.counter("purelb_lbnodeagent_na_sent_total") or None,
                timeout=45, interval=3.0,
                description="an unsolicited NA for the IPv6 VIP",
            )
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


@pytest.mark.requires("multi-node")
def test_an_agent_with_no_local_spec_leaves_the_node_on_default_detection(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    lbnodeagent: str, selector_probe,
):
    """selector_state=default: agents matched, none configured anything.

    LBNodeAgentSpec carries only nodeSelector and local, so an agent with
    no local block asks for nothing and the node keeps default interface
    detection. That is the documented behaviour, and the reason this is
    `default` rather than `deselected`.

    The `default` LBNodeAgent has to go for the duration, and the reason
    is the whole subtlety of the state. AgentsForNode returns specific
    agents THEN catch-alls -- it does not drop the catch-all -- and
    FirstLocalAgent then picks the first agent that HAS a local spec. So
    a no-local agent scoped to a node does not suppress the catch-all's
    local spec; it is simply skipped, and the node reports `configured`.
    Only when no matching agent has a local spec at all does the selector
    come back nil.

    The distinction from `deselected` is what makes this worth a test and
    it is invisible in the gauge name: `default` still advertises the
    node's own subnets and still wins elections, `deselected` advertises
    nothing. Collapsing them one way is a blackhole and the other way a
    silent node, so the lease is asserted, not just the gauge.
    """
    node = sorted(topo.node_ips)[0]
    own = topo.subnet_of(node)

    selector_probe("selector-no-local", node, {})
    # Now the ONLY agent matching this node is the one with no local spec.
    cluster.delete_cr("lbnodeagent", "default")
    try:
        wait_until(
            lambda: metrics.scrape_node(topo.node_ips[node]).value(
                "purelb_lbnodeagent_selector_state", state="default"
            ) == 1.0 or None,
            timeout=120, interval=3.0,
            description=f"{node} to report selector_state=default",
        )

        # Still advertising: this is what separates `default` from
        # `deselected`, and a regression collapsing them would leave the
        # gauge assertion above passing untouched.
        advertised = wait_until(
            lambda: cluster.node_lease_subnets(node) or None,
            timeout=90, interval=3.0,
            description=f"{node}'s lease to still advertise its own subnets",
        )
        assert own.v4 in advertised, (
            f"{node} reports selector_state=default but advertises "
            f"{advertised}, which does not include its own subnet {own.v4}. "
            f"Default detection is meant to be the fallback, not silence."
        )
    finally:
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "LBNodeAgent",
                "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
                "spec": {"local": {"localInterface": "default", "dummyInterface": "kube-lb0"}},
            }
        )


@pytest.mark.requires("multi-node")
def test_an_uncompilable_local_interface_silences_only_that_node(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    lbnodeagent: str, selector_probe,
):
    """selector_state=invalid: the announcer refused the config.

    localInterface is compiled as a regex and has no CRD pattern, so "["
    is schema-valid and uncompilable -- a typo an operator can really
    make. The node must then advertise NOTHING: an agent that could not
    configure itself but kept advertising would keep winning elections
    for addresses it cannot announce, which is the blackhole this state
    exists to prevent.

    And it must say so. The failure is otherwise invisible: the CR
    applies cleanly, `kubectl get lbnodeagent` shows it, and the only
    symptom is one node quietly announcing nothing.
    """
    node = sorted(topo.node_ips)[0]
    other = next(n for n in sorted(topo.node_ips) if n != node)

    selector_probe(
        "selector-bad-regex", node,
        {"local": {"localInterface": "[", "dummyInterface": "kube-lb0"}},
    )

    wait_until(
        lambda: metrics.scrape_node(topo.node_ips[node]).value(
            "purelb_lbnodeagent_selector_state", state="invalid"
        ) == 1.0 or None,
        timeout=120, interval=3.0,
        description=f"{node} to report selector_state=invalid",
    )

    wait_until(
        lambda: cluster.node_lease_subnets(node) == [] or None,
        timeout=90, interval=2.0,
        description=f"{node}'s lease to advertise nothing",
    )

    messages = agent_events(cluster, "selector-bad-regex")
    assert any("announcing nothing" in m for m in messages), (
        f"no ConfigError event on the offending LBNodeAgent, so the only "
        f"signal that {node} has stopped announcing is a gauge nobody is "
        f"watching. Events: {messages}"
    )

    assert metrics.scrape_node(topo.node_ips[other]).value(
        "purelb_lbnodeagent_selector_state", state="configured"
    ) == 1.0, (
        f"one uncompilable CR scoped to {node} also broke {other}"
    )


# ------------------------------------------------- gratuitous announcement


def test_affinity_placed_address_announces_itself(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    lb_service, pinned_backend, agent_metrics,
):
    """A node that announces an address must gratuitously announce it.

    GARP (IPv4) and unsolicited NA (IPv6) are what update the upstream
    switch and router caches. Without them a VIP that moves keeps
    receiving no traffic until those caches age out -- minutes of
    blackhole for a failover that PureLB itself completed in seconds.

    This is the regression test for a bug this migration found: placement
    used election.WinnerWithPreference but verify-before-send used plain
    election.Winner, and those disagree exactly when a Service opts into
    node-affinity -- so the node holding the address skipped every packet.
    Measured before the fix: with affinity, garp_sent_total stayed 0 while
    the agent logged "skipping announcement, no longer winner". Without
    affinity the same setup sent 3.
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


# ------------------------------------------------------------ node affinity


AFFINITY_FALLBACK = "purelb_election_affinity_fallback_total"
AFFINITY = "purelb.io/node-affinity"


@pytest.fixture(scope="module")
def affinity_pool(cluster: Cluster, topo: topology.Topology):
    """One ServiceGroup for the affinity tests, on its own band.

    Module-scoped and on .251-.253 / f:: rather than the shared per-test
    band, so these tests cannot collide with each other or with another
    module. Two tests that each create and delete a group with identical
    ranges race the allocator's reconcile of the deletion, and the loser
    is rejected as overlapping -- which surfaces as its Service never
    getting an address.

    The subnet is pinned because the fallback test needs to know which
    nodes are NOT eligible to announce the address.
    """
    subnet = next(
        (s for s in topo.subnets if s.v6 and len(s.nodes) >= 2), topo.subnets[0]
    )
    spec: Dict[str, object] = {
        "local": {
            "v4pools": [
                {"aggregation": "default", "pool": subnet.band(251, 253),
                 "subnet": subnet.v4}
            ]
        }
    }
    if subnet.v6:
        spec["local"]["v6pools"] = [  # type: ignore[index]
            {"aggregation": "default", "pool": subnet.v6_band("f", 1, 4),
             "subnet": subnet.v6}
        ]
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "affinity-pool", "namespace": cluster.purelb_namespace},
            "spec": spec,
        }
    )
    yield "affinity-pool", subnet
    cluster.delete_cr("servicegroup", "affinity-pool")


def _fallbacks(agent_metrics, topo: topology.Topology, address: str) -> float:
    """Affinity fallbacks recorded for one ADDRESS, summed over all nodes.

    Note the label value: the counter is declared with a `service` label
    but the announcer calls WinnerWithPreference(lbIP.String(), ...), so
    what lands in it is the IP. Anyone querying
    affinity_fallback_total{service="ns/name"} in an alert gets nothing,
    for ever, and reads it as "no fallbacks".

    Summed across nodes because every node evaluates the election
    independently, so the node that records the fallback need not be the
    one that ends up announcing.
    """
    total = 0.0
    for node in topo.node_ips:
        try:
            total += agent_metrics(node).counter(AFFINITY_FALLBACK, service=address)
        except Exception:  # noqa: BLE001 - an unreachable node contributes nothing
            continue
    return total


@pytest.mark.requires("multi-node")
def test_affinity_follows_the_endpoints_for_every_family(
    cluster: Cluster, topo: topology.Topology, lb_service, pinned_backend,
    agent_metrics, affinity_pool, lbnodeagent: str,
):
    """The address lands where the endpoints are -- IPv4 AND IPv6.

    node-affinity biases the election toward nodes hosting Ready
    endpoints so traffic is not hairpinned across the cluster. The two
    families are placed by separate election rounds and nothing makes the
    second follow the first, so a v6 VIP announced from a node with no
    endpoints is exactly the hairpin the annotation exists to prevent --
    and it is invisible unless asserted per family.

    Then the endpoints move, and both addresses have to follow. Placement
    at creation time is the easy half; following a backend that has been
    rescheduled is the half that matters in a running cluster.
    """
    group, subnet = affinity_pool
    first, second = sorted(subnet.nodes)[:2]
    families = ["IPv4", "IPv6"] if subnet.v6 else ["IPv4"]

    backend = pinned_backend("affinity-backend", first)
    vips = lb_service(
        "affinity-follow", families,
        annotations={AFFINITY: "service-endpoints", SERVICE_GROUP: group},
        selector={"app": backend}, timeout=120,
    )
    assert len(vips) == len(families), f"expected {families}, allocated {vips}"

    for vip in vips:
        holder, _ = wait_until(
            lambda v=vip: nodes.announcing_node(topo.node_ips, v),
            timeout=120, interval=2.0, description=f"{vip} to be announced",
        )
        assert holder == first, (
            f"{vip} is announced by {holder} but the only Ready endpoint is on "
            f"{first}. Traffic for it hairpins across the cluster, which is "
            f"what {AFFINITY} was set to prevent."
        )

    # Baselined AFTER placement has settled, and held. A reading taken at
    # creation can still be climbing from an evaluation that ran before
    # the endpoint was Ready, which would fail this on correct behaviour;
    # what the assertion is about is the settled state.
    settled = {vip: _fallbacks(agent_metrics, topo, vip) for vip in vips}
    time.sleep(10)
    for vip in vips:
        assert _fallbacks(agent_metrics, topo, vip) == settled[vip], (
            f"{AFFINITY_FALLBACK} is still rising for {vip} while {first} hosts "
            f"a Ready endpoint and is eligible. The preference is being computed "
            f"and then not used -- and the address landing correctly anyway "
            f"hides it completely."
        )

    # Move the endpoints; both families must follow.
    cluster.patch_deployment(
        NAMESPACE, backend, {"spec": {"template": {"spec": {"nodeName": second}}}}
    )
    cluster.wait_rollout(NAMESPACE, backend, timeout=180)
    for vip in vips:
        wait_until(
            lambda v=vip: (lambda f: f if f and f[0] == second else None)(
                nodes.announcing_node(topo.node_ips, v)
            ),
            timeout=180, interval=3.0,
            description=f"{vip} to follow its endpoints to {second}",
        )


@pytest.mark.requires("multi-node", "multi-subnet")
def test_affinity_falls_back_and_counts_it_when_no_preferred_node_can_announce(
    cluster: Cluster, topo: topology.Topology, lb_service, pinned_backend,
    agent_metrics, affinity_pool, lbnodeagent: str,
):
    """Endpoints somewhere the address cannot be announced from.

    The election only considers nodes whose own subnet contains the
    address, so pinning the backend to a node on another subnet makes
    every preferred node ineligible. PureLB must then announce the
    address anyway, from a node that CAN serve it, and record that it did
    so -- a silent fallback is affinity quietly not happening.

    Then the endpoints are removed entirely. That is deliberately NOT a
    fallback: WinnerWithPreference documents an empty preferred set as
    opt-out rather than a degraded state, so the counter must stay put.
    Asserting it pins the distinction, which is otherwise one `len()`
    away from being lost.
    """
    group, subnet = affinity_pool
    # Ineligible means "its lease does not advertise the address's
    # subnet", which is the property the election actually consults --
    # not "it is not in topo's list for that subnet". A dual-homed node
    # can be physically on the subnet while advertising only its primary,
    # and picking it here would make the address land on a node this test
    # expects to be ineligible, failing for a reason that is not the one
    # under test.
    outsider = next(
        (n for n in sorted(topo.node_ips)
         if subnet.v4 not in cluster.node_lease_subnets(n)),
        None,
    )
    if outsider is None:
        pytest.skip("every node advertises the address's subnet, so none is ineligible")

    # Baseline every address the pool can hand out, BEFORE allocating. The
    # counter is labelled by IP and this pool recycles three addresses, so
    # an address reused from the previous test can arrive with a non-zero
    # count -- and then "> 0" passes without PureLB having done anything.
    candidates = [f"{subnet.v4_octets}.{i}" for i in range(251, 254)]
    baseline = {a: _fallbacks(agent_metrics, topo, a) for a in candidates}

    backend = pinned_backend("affinity-outsider", outsider)
    vip = lb_service(
        "affinity-fallback", ["IPv4"],
        annotations={AFFINITY: "service-endpoints", SERVICE_GROUP: group},
        selector={"app": backend}, timeout=120,
    )[0]

    holder, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip),
        timeout=120, interval=2.0,
        description=f"{vip} to be announced despite its endpoints being ineligible",
    )
    assert holder in subnet.nodes, (
        f"{vip} is announced by {holder}, which is not on {subnet.v4}. Affinity "
        f"is a preference, and it must never override the subnet rule -- a node "
        f"off the subnet cannot answer ARP for the address."
    )

    wait_until(
        lambda: _fallbacks(agent_metrics, topo, vip) > baseline.get(vip, 0) or None,
        timeout=120, interval=3.0,
        description=f"{AFFINITY_FALLBACK} to rise above {baseline.get(vip, 0)} for {vip}",
    )
    counted = _fallbacks(agent_metrics, topo, vip)

    # No endpoints at all is opt-out, not degradation: still announced,
    # and the counter must not move.
    cluster.patch_deployment(NAMESPACE, backend, {"spec": {"replicas": 0}})
    wait_until(
        lambda: (not cluster.pods(NAMESPACE, f"app={backend}")) or None,
        timeout=120, interval=3.0, description="the backend to scale to zero",
    )
    still, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip),
        timeout=120, interval=3.0,
        description=f"{vip} to still be announced with no endpoints at all",
    )
    assert still in subnet.nodes, (
        f"{vip} stopped being announced once its endpoints went away. No "
        f"backends must degrade to an ordinary election, not to no address."
    )

    # And the counter stops. While the ineligible backend existed every
    # reconcile recorded a fallback, so this has to be measured from a
    # fresh reading AFTER the pod is gone, then held: an equality against
    # `counted` (taken while it was still climbing) would fail on correct
    # behaviour.
    settled = _fallbacks(agent_metrics, topo, vip)
    time.sleep(15)
    assert _fallbacks(agent_metrics, topo, vip) == settled, (
        f"{AFFINITY_FALLBACK} is still climbing for {vip} with no endpoints "
        f"at all (was {counted} while they existed, {settled} after they "
        f"went). An empty preferred set is documented as opt-out, not as a "
        f"degraded state, so counting it turns every Service with no "
        f"backends into a permanent affinity alert."
    )
