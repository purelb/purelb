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

"""Multi-subnet placement and multi-pool allocation.

Ported from the Multi-Subnet Validation group of
test/e2e/local/test-local-allocation.sh.

Two features, and the second only makes sense given the first:

* SUBNET PLACEMENT. A VIP is only reachable from a node on its own
  subnet, because that is where its gateway will ARP for it. So every
  allocation, failover and re-placement has to stay inside the subnet the
  address came from -- announcing 172.30.251.x from a node on
  172.30.250.0/24 is announced-and-unreachable, which is worse than
  unallocated because it looks like it worked.

* MULTI-POOL. A Service gets one address from EACH subnet range that has
  live nodes, so it is reachable on every subnet rather than only the one
  that happened to win. Enabled by `multiPool: true` on a ServiceGroup or
  per-Service by the purelb.io/multi-pool annotation, with the annotation
  overriding the group in both directions.

The exhaustion test is the sharpest thing here, and the only one that
asserts an ABSENCE: when every node on a subnet is gone, the VIP must
have no home at all. Migrating it to a node on another subnet would
"work" by every liveness check and be unreachable in practice.
"""

from __future__ import annotations

import ipaddress
from typing import Dict, List

import pytest

from purelb_e2e import TEST_NAMESPACE, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
MULTI_POOL = "purelb.io/multi-pool"
SERVICE_GROUP = "purelb.io/service-group"

pytestmark = pytest.mark.requires("multi-subnet")


def v4_of(ips: List[str]) -> List[str]:
    return [i for i in ips if ipaddress.ip_address(i).version == 4]


def v6_of(ips: List[str]) -> List[str]:
    return [i for i in ips if ipaddress.ip_address(i).version == 6]


def assert_one_per_subnet(topo: topology.Topology, ips: List[str], family: int) -> None:
    """Exactly one address per subnet, and each from that subnet's range."""
    seen: Dict[str, str] = {}
    for address in ips:
        subnet = topo.subnet_holding(address)
        assert subnet is not None, f"{address} belongs to no node subnet"
        assert subnet.v4 not in seen, (
            f"two addresses from {subnet.v4}: {seen[subnet.v4]} and {address}; "
            f"multi-pool allocates one per range, not several from one"
        )
        seen[subnet.v4] = address
    expected = [s for s in topo.subnets if (s.v4 if family == 4 else s.v6)]
    assert len(seen) == len(expected), (
        f"got {len(seen)} address(es) for {len(expected)} subnet(s): {ips}"
    )


@pytest.fixture
def multipool_group(cluster: Cluster, topo: topology.Topology):
    """A ServiceGroup spanning every subnet, multiPool configurable.

    Uses the .230-.240 / b:: per-test band. Overlapping the default pool
    would have the allocator reject the group outright, which reads as a
    product bug rather than a test bug.
    """
    created: List[str] = []

    def make(name: str, multi_pool: bool) -> str:
        local: Dict[str, object] = {
            "v4pools": [
                {"aggregation": "default", "pool": s.test_pool, "subnet": s.v4}
                for s in topo.subnets
            ]
        }
        v6 = [
            {"aggregation": "default", "pool": s.test_pool_v6, "subnet": s.v6}
            for s in topo.subnets if s.v6
        ]
        if v6:
            local["v6pools"] = v6
        if multi_pool:
            local["multiPool"] = True
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {"local": local},
            }
        )
        created.append(name)
        return name

    yield make

    for name in reversed(created):
        cluster.delete_cr("servicegroup", name)


# ----------------------------------------------------------- placement


def test_default_group_places_each_vip_on_its_own_subnet(
    topo: topology.Topology, default_servicegroup: str, lb_service
):
    """The default group spans both subnets; placement must still match.

    Allocating several services exercises the choice: with one pool per
    subnet the allocator may hand out either, and each one has to land on
    a node that can actually announce it.
    """
    for i in range(4):
        vip = lb_service(f"echo-lb-multipool-{i}", ["IPv4"])[0]
        subnet = topo.subnet_holding(vip)
        assert subnet is not None, f"{vip} is in no node subnet"
        holder, _ = wait_until(
            lambda v=vip: nodes.announcing_node(topo.node_ips, v),
            timeout=45, description=f"{vip} to be announced",
        )
        assert holder in subnet.nodes, (
            f"{vip} is from {subnet.v4} but is announced by {holder} on "
            f"{topo.subnet_of(holder).v4}; its gateway cannot reach it there"
        )


@pytest.mark.requires("ipv6")
def test_ipv6_vips_are_placed_on_their_own_subnet(
    topo: topology.Topology, default_servicegroup: str, lb_service
):
    """Same rule for IPv6, which NDP makes just as subnet-local."""
    for i in range(2):
        vip = lb_service(f"echo-lb-v6-place-{i}", ["IPv6"])[0]
        subnet = topo.subnet_holding(vip)
        assert subnet is not None, f"{vip} is in no node subnet"
        holder, _ = wait_until(
            lambda v=vip: nodes.announcing_node(topo.node_ips, v),
            timeout=45, description=f"{vip} to be announced",
        )
        assert holder in subnet.nodes, (
            f"{vip} is from {subnet.v6} but is announced by {holder}"
        )


def test_leases_carry_the_subnet_of_their_node(cluster: Cluster, topo: topology.Topology,
                                               default_servicegroup: str):
    """Election is subnet-aware, and the leases are how it knows.

    A lease annotated with the wrong subnet would elect a winner that
    cannot announce the address, so this is the input to every placement
    assertion above.
    """
    seen = 0
    for lease in cluster.leases():
        annotations = (lease.metadata.annotations or {})
        subnets = [v for k, v in annotations.items() if "subnet" in k.lower()]
        holder = lease.spec.holder_identity or ""
        # Exact match, NOT startswith. The holder identity is the node
        # name, and a prefix test cannot tell purelb2-1 from purelb2-10 --
        # it would silently compare one node's lease against another
        # node's subnet, which on this cluster is a different subnet
        # entirely. Same bug the bash suite had in pod resolution.
        node = holder if holder in topo.node_ips else None
        if node is None or not subnets:
            continue
        seen += 1
        expected = topo.subnet_of(node)
        joined = " ".join(subnets)
        assert expected.v4 in joined or (expected.v6 or "!") in joined, (
            f"lease {lease.metadata.name} held by {node} is annotated {joined!r}, "
            f"but {node} is on {expected.v4}"
        )
    assert seen > 0, "no lease carried a subnet annotation; election cannot be subnet-aware"


# ---------------------------------------------------------- multi-pool


@pytest.mark.parametrize("family", ["v4", "v6", "dual"])
def test_servicegroup_multipool_gives_one_address_per_subnet(
    topo: topology.Topology, multipool_group, lb_service, family: str
):
    """multiPool: true -> one address per subnet range, per family."""
    if family != "v4" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6 on the node subnets")

    group = multipool_group("multipool-test", multi_pool=True)
    families = {"v4": ["IPv4"], "v6": ["IPv6"], "dual": ["IPv4", "IPv6"]}[family]
    ips = lb_service(
        f"echo-mp-sg-{family}", families,
        annotations={SERVICE_GROUP: group}, timeout=90,
    )

    if "IPv4" in families:
        assert_one_per_subnet(topo, v4_of(ips), 4)
    if "IPv6" in families:
        assert_one_per_subnet(topo, v6_of(ips), 6)

    # Every one of them has to actually work, not merely exist.
    for address in ips:
        subnet = topo.subnet_holding(address)
        holder, _ = wait_until(
            lambda a=address: nodes.announcing_node(topo.node_ips, a),
            timeout=45, description=f"{address} to be announced",
        )
        assert holder in subnet.nodes, f"{address} announced by {holder}, off its subnet"
        assert nodes.echo_json(topo.node_ips, address)["pod"], (
            f"multi-pool address {address} is announced but does not serve"
        )


def test_multipool_annotation_enables_it_on_a_single_pool_group(
    topo: topology.Topology, default_servicegroup: str, lb_service
):
    """The annotation turns multi-pool ON for a group that does not set it."""
    ips = lb_service(
        "echo-mp-ann-v4", ["IPv4"],
        annotations={SERVICE_GROUP: "default", MULTI_POOL: "true"},
        timeout=90,
    )
    assert_one_per_subnet(topo, v4_of(ips), 4)


def test_multipool_annotation_overrides_the_group_and_turns_it_off(
    topo: topology.Topology, multipool_group, lb_service
):
    """...and OFF again, which is the direction that could silently fail.

    A build that ignored the annotation entirely would still pass the
    enabling test whenever the group already had multiPool set, so the
    override has to be checked in both directions.
    """
    group = multipool_group("multipool-override", multi_pool=True)
    ips = lb_service(
        "echo-mp-override", ["IPv4"],
        annotations={SERVICE_GROUP: group, MULTI_POOL: "false"},
    )
    assert len(ips) == 1, (
        f"multi-pool=false on a multiPool group gave {len(ips)} addresses: {ips}"
    )


# ---------------------------------------------------------- exhaustion


@pytest.mark.requires("multi-node")
def test_vip_has_no_home_when_its_subnet_runs_out_of_nodes(
    cluster: Cluster, topo: topology.Topology, subnet_servicegroup, lb_service,
    tainted_nodes,
):
    """Exhaust a subnet's nodes; the VIP must go nowhere, not elsewhere.

    The only assertion in this group about something NOT happening, and
    the most important. Migrating the address to a node on another subnet
    passes every liveness check -- it is announced, an interface has it,
    the election has a winner -- and is unreachable, because the gateway
    that would ARP for it is on the subnet the address came from.
    """
    smallest = min(topo.subnets, key=lambda s: len(s.nodes))
    group = subnet_servicegroup("exhaustion-pool", smallest)
    vip = lb_service(
        "echo-lb-small-subnet", ["IPv4"], annotations={SERVICE_GROUP: group}
    )[0]
    assert topo.subnet_holding(vip).v4 == smallest.v4

    wait_until(lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
               description=f"{vip} to be announced")

    for node in smallest.nodes:
        tainted_nodes(node)
        pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)
        if pod is not None:
            cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)

    wait_while(
        lambda: bool(nodes.nodes_with_address(topo.node_ips, vip)),
        timeout=90, interval=3.0,
        description=f"{vip} to be withdrawn from every node once {smallest.v4} is exhausted",
    )

    # Hold it there: "gone" must be a steady state, not a moment between
    # the old announcer dropping it and a wrong one picking it up.
    import time
    for _ in range(5):
        time.sleep(2)
        holders = nodes.nodes_with_address(topo.node_ips, vip)
        assert not holders, (
            f"{vip} is from {smallest.v4}, whose nodes are all gone, but it "
            f"migrated to {holders} -- announced there and unreachable"
        )
