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

"""balancePools: spread allocations evenly across ranges.

With `balancePools: true` a new allocation takes the range with the
FEWEST addresses currently in use, per family independently. Without it
the allocator fills the first range before touching the second, so on a
two-subnet cluster every service lands on one subnet until that pool is
exhausted -- concentrating both the failure domain and the traffic.

Mutually exclusive with multiPool, and that pairing is the sharp edge:
multiPool gives a service one address from EVERY range and balancePools
gives it one address from the emptiest, so a group asking for both is
asking for two contradictory things. The allocator refuses rather than
picking one, and refuses via the annotation route too.

I missed this feature entirely on the first pass through the local suite
and the dual-run mapping is what surfaced it: 15 assertions in the bash
suite with no pytest counterpart. That is the gate working -- the port
looked complete and was not.
"""

from __future__ import annotations

import ipaddress
from collections import Counter
from typing import Dict, List, Optional

import pytest

from purelb_e2e import TEST_NAMESPACE, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until

NAMESPACE = TEST_NAMESPACE
SERVICE_GROUP = "purelb.io/service-group"
MULTI_POOL = "purelb.io/multi-pool"
BALANCED = "balanced-test"

pytestmark = pytest.mark.requires("multi-subnet")


def range_of(topo: topology.Topology, address: str) -> str:
    """Which subnet range an address came from."""
    subnet = topo.subnet_holding(address)
    assert subnet is not None, f"{address} belongs to no node subnet"
    return subnet.v4


@pytest.fixture
def balanced_group(cluster: Cluster, topo: topology.Topology):
    """A ServiceGroup spanning every subnet, balancePools configurable."""
    created: List[str] = []

    def make(name: str, balance: bool = True, multi_pool: bool = False) -> str:
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
        if balance:
            local["balancePools"] = True
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


def events_for(cluster: Cluster, namespace: str, name: str) -> List[str]:
    got = cluster.core.list_namespaced_event(
        namespace, field_selector=f"involvedObject.name={name}"
    )
    return [e.message for e in got.items if e.message]


# --------------------------------------------------------------- balance


@pytest.mark.parametrize("family", ["v4", "v6", "dual"])
def test_allocations_are_spread_evenly_across_ranges(
    cluster: Cluster, topo: topology.Topology, balanced_group, lb_service,
    allocator_metrics, family: str,
):
    """Each range gets its fair share, per family independently.

    Two services per subnet, so a correct result is exactly fair and any
    imbalance is a whole service in the wrong place -- no rounding to
    argue about.
    """
    if family != "v4" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6 on the node subnets")

    group = balanced_group(BALANCED)
    families = {"v4": ["IPv4"], "v6": ["IPv6"], "dual": ["IPv4", "IPv6"]}[family]
    ranges = topo.subnets
    per_range = 2
    count = len(ranges) * per_range

    before = allocator_metrics()
    got: List[str] = []
    for i in range(count):
        got.extend(
            lb_service(
                f"nginx-bal-{family}-{i}", families,
                annotations={SERVICE_GROUP: group}, timeout=60,
            )
        )

    for version in (4, 6):
        addresses = [a for a in got if ipaddress.ip_address(a).version == version]
        if not addresses:
            continue
        spread = Counter(range_of(topo, a) for a in addresses)
        assert len(spread) == len(ranges), (
            f"IPv{version} addresses used {len(spread)} of {len(ranges)} ranges: "
            f"{dict(spread)}; balancePools should have touched every range"
        )
        for subnet_v4, n in spread.items():
            assert n == per_range, (
                f"IPv{version}: range {subnet_v4} got {n} addresses, fair share "
                f"is {per_range} ({dict(spread)})"
            )

    metrics.assert_increased(
        before, allocator_metrics(), "purelb_address_pool_balance_pools_allocations_total",
        pool=group,
    )


def test_balanced_addresses_are_announced_and_serve(
    cluster: Cluster, topo: topology.Topology, balanced_group, lb_service,
):
    """A balanced address is a normal address: announced, and it works.

    Spreading allocations across ranges is only useful if the range that
    was chosen is one this cluster can actually announce on.
    """
    group = balanced_group(BALANCED)
    for i in range(len(topo.subnets)):
        vip = lb_service(
            f"nginx-bal-reach-{i}", ["IPv4"], annotations={SERVICE_GROUP: group}
        )[0]
        holder, _ = wait_until(
            lambda v=vip: nodes.announcing_node(topo.node_ips, v),
            timeout=45, description=f"{vip} to be announced",
        )
        assert holder in topo.subnet_holding(vip).nodes, (
            f"{vip} announced by {holder}, which is off its subnet"
        )
        assert nodes.echo_json(topo.node_ips, vip)["pod"]


# ----------------------------------------------------- mutual exclusion


def test_balance_pools_and_multi_pool_together_are_refused(
    cluster: Cluster, topo: topology.Topology, balanced_group, lb_service,
):
    """A group asking for both is asking for two contradictory things.

    multiPool wants one address from EVERY range; balancePools wants one
    from the emptiest. Silently picking either would give an operator a
    layout they did not ask for, so the allocator refuses and says so.
    """
    group = balanced_group("balanced-multipool-conflict", balance=True, multi_pool=True)
    lb_service(
        "nginx-bal-conflict", ["IPv4"], annotations={SERVICE_GROUP: group}, wait=False
    )
    import time

    for _ in range(8):
        time.sleep(2)
        assert not cluster.service_ingress_ips(NAMESPACE, "nginx-bal-conflict"), (
            "a group with both multiPool and balancePools allocated anyway"
        )
    messages = events_for(cluster, NAMESPACE, "nginx-bal-conflict")
    assert any("mutually exclusive" in m for m in messages), (
        f"no event explaining the refusal: {messages}"
    )


def test_multi_pool_annotation_on_a_balanced_group_is_refused(
    cluster: Cluster, balanced_group, lb_service,
):
    """The same conflict via the annotation route.

    Refusing it only in the spec would leave the contradiction reachable
    per-Service, which is the easier mistake for a user to make.
    """
    group = balanced_group(BALANCED)
    lb_service(
        "nginx-bal-annoverride", ["IPv4"],
        annotations={SERVICE_GROUP: group, MULTI_POOL: "true"}, wait=False,
    )
    import time

    for _ in range(8):
        time.sleep(2)
        assert not cluster.service_ingress_ips(NAMESPACE, "nginx-bal-annoverride"), (
            "multi-pool=true on a balancePools group allocated anyway"
        )
    messages = events_for(cluster, NAMESPACE, "nginx-bal-annoverride")
    assert any("mutually exclusive" in m for m in messages), (
        f"no event explaining the refusal: {messages}"
    )


def test_naming_a_servicegroup_that_does_not_exist_is_refused(
    cluster: Cluster, default_servicegroup: str, lb_service, allocator_metrics,
):
    """An unknown group is a distinct failure from an unusable one.

    Telling an operator their ServiceGroup does not exist when it plainly
    does sends them looking in the wrong place, so the allocator keeps
    the two apart and the metric label does too.
    """
    before = allocator_metrics()
    lb_service(
        "nginx-bal-nosuchsg", ["IPv4"],
        annotations={SERVICE_GROUP: "does-not-exist"}, wait=False,
    )
    import time

    for _ in range(8):
        time.sleep(2)
        assert not cluster.service_ingress_ips(NAMESPACE, "nginx-bal-nosuchsg")

    messages = events_for(cluster, NAMESPACE, "nginx-bal-nosuchsg")
    assert any("unknown pool" in m for m in messages), (
        f"no event naming the missing ServiceGroup: {messages}"
    )
    metrics.assert_increased(
        before, allocator_metrics(),
        "purelb_address_pool_allocation_rejected_total",
        pool="does-not-exist", reason="unknown_pool",
    )
