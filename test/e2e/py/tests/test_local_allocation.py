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

"""Local-mode allocation: the address lands on a real interface.

Ported from test/e2e/local/test-local-allocation.sh, the Core Functionality
group. "Local" means the VIP is configured on the node's physical
interface and announced with ARP/NDP, as opposed to remote mode where it
sits on the kube-lb0 dummy and is advertised by BGP.

The IPv4, IPv6 and dual-stack cases were three near-identical 110-line
bash functions. They are one parametrized test here, which is not merely
shorter: the bash copies had DRIFTED, and only the IPv4 one checked that
the address had not landed on kube-lb0. Parametrizing makes IPv4/IPv6
parity structural rather than something to remember.
"""

from __future__ import annotations

import ipaddress
from typing import Dict, List

import pytest

from purelb_e2e import TEST_NAMESPACE, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
DUMMY_IFACE = "kube-lb0"

ALLOCATED_BY = "purelb.io/allocated-by"
ALLOCATED_FROM = "purelb.io/allocated-from"
SERVICE_GROUP = "purelb.io/service-group"
SHARING = "purelb.io/allow-shared-ip"

FAMILIES = {
    "v4": ["IPv4"],
    "v6": ["IPv6"],
    "dual": ["IPv4", "IPv6"],
}


# ---------------------------------------------------------------- helpers


def announced_on(topo: topology.Topology, address: str):
    """(node, interface) carrying `address`, or None."""
    return nodes.announcing_node(topo.node_ips, address)


def assert_not_on_dummy(topo: topology.Topology, address: str) -> None:
    """The VIP must NOT be on kube-lb0.

    This is the difference between local and remote mode, and it was
    asserted only in the IPv4 bash test -- so an IPv6 address landing on
    the dummy interface, which is a real failure mode of the local
    announcer, would have gone unnoticed.
    """
    for node, ip in sorted(topo.node_ips.items()):
        iface = nodes.interface_for_address(ip, address)
        assert iface != DUMMY_IFACE, (
            f"{address} is on {DUMMY_IFACE} on {node}; in local mode it "
            f"belongs on the physical interface"
        )


def curl_from_node(topo: topology.Topology, address: str, timeout: float = 30.0) -> str:
    """HTTP GET the VIP from a cluster node. See nodes.curl_via_node."""
    return nodes.curl_via_node(topo.node_ips, address, timeout=timeout)


# ------------------------------------------------------------ allocation


@pytest.mark.parametrize("family", ["v4", "v6", "dual"])
def test_allocates_announces_and_serves(
    cluster: Cluster,
    topo: topology.Topology,
    default_servicegroup: str,
    lb_service,
    allocator_metrics,
    agent_metrics,
    log_window,
    family: str,
):
    """One address per family, on a real interface, serving traffic."""
    if family != "v4" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6 on the node subnets")

    name = f"nginx-lb-{family}"
    before = allocator_metrics()
    agent_before = {n: agent_metrics(n) for n in topo.node_ips}

    ips = lb_service(name, FAMILIES[family])
    assert len(ips) == len(FAMILIES[family]), f"expected {FAMILIES[family]}, got {ips}"

    # Captured as each address is confirmed, not re-resolved afterwards:
    # re-asking could race a move and hand back None.
    winners: set[str] = set()

    for address in ips:
        subnet = topo.subnet_holding(address)
        assert subnet is not None, f"{address} is in no node subnet, so nothing can announce it"
        pool = subnet.default_pool if ipaddress.ip_address(address).version == 4 else subnet.default_pool_v6
        low, high = pool.split("-")
        assert ipaddress.ip_address(low) <= ipaddress.ip_address(address) <= ipaddress.ip_address(high), (
            f"{address} is outside the default pool {pool}"
        )

        found = wait_until(
            lambda a=address: announced_on(topo, a),
            timeout=45,
            description=f"{address} to appear on a node interface",
        )
        node, iface = found
        winners.add(node)
        assert iface != DUMMY_IFACE, f"{address} landed on {DUMMY_IFACE} on {node}"
        assert_not_on_dummy(topo, address)

        # On a multi-subnet cluster the announcing node must be ON the
        # subnet the address came from. Announcing 172.30.251.x from a
        # node on 172.30.250.0/24 puts the VIP where its gateway cannot
        # reach it.
        if topo.multi_subnet:
            assert node in subnet.nodes, (
                f"{address} is from {subnet.v4} but is announced by {node}, "
                f"which is on {topo.subnet_of(node).v4}"
            )

        body = curl_from_node(topo, address)
        assert "Pod:" in body, f"{address} did not serve the backend; got {body[:200]!r}"

    # Annotations PureLB sets on a service it allocated for.
    svc = cluster.service(NAMESPACE, name)
    annotations = svc.metadata.annotations or {}
    assert annotations.get(ALLOCATED_BY) == "PureLB", (
        f"{ALLOCATED_BY} = {annotations.get(ALLOCATED_BY)!r}"
    )
    assert annotations.get(ALLOCATED_FROM), f"{ALLOCATED_FROM} is missing"

    # ipMode is K8s 1.30+. Absent is acceptable; wrong is not.
    for ingress in svc.status.load_balancer.ingress or []:
        if ingress.ip_mode is not None:
            assert ingress.ip_mode == "VIP", f"ipMode = {ingress.ip_mode!r}, expected VIP"

    after = allocator_metrics()
    assert after.counter("purelb_address_pool_size", pool="default") > 0
    assert after.counter("purelb_address_pool_addresses_in_use", pool="default") >= len(ips)

    # Every announcing node must show its OWN counters advancing. A
    # cluster-wide "some node won an election" is satisfied by any earlier
    # test, which is what `gt 0` on a monotonic counter actually asked.
    for node in winners:
        now = agent_metrics(node)
        metrics.assert_increased(
            agent_before[node], now, "purelb_lbnodeagent_election_wins_total"
        )
        metrics.assert_increased(
            agent_before[node], now, "purelb_lbnodeagent_address_additions_total"
        )
        announced = {k: v for k, v in now.samples.items()
                     if k.startswith("purelb_lbnodeagent_announced") and name in k}
        assert announced, f"announced gauge on {node} does not mention {name}"

    # The election log, scoped to this test's window and to the winner.
    for node in winners:
        pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)
        assert pod is not None
        logs = cluster.pod_logs(
            cluster.purelb_namespace, pod.metadata.name, log_window, container="lbnodeagent"
        )
        assert "electionWon" in logs, (
            f"{node} announced {name} but logged no electionWon in this test's window"
        )


def test_release_withdraws_the_address(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    allocator_metrics,
):
    """Deleting the Service frees the address and takes it off the node."""
    before = allocator_metrics()
    ips = lb_service("nginx-lb-cleanup", ["IPv4"])
    vip = ips[0]
    wait_until(lambda: announced_on(topo, vip), timeout=45, description=f"{vip} announced")

    in_use = allocator_metrics().counter("purelb_address_pool_addresses_in_use", pool="default")
    cluster.delete_service(NAMESPACE, "nginx-lb-cleanup")

    # nodes_with_address raises on an unreachable node, so "gone" cannot
    # be concluded from a node that never answered.
    wait_while(
        lambda: bool(nodes.nodes_with_address(topo.node_ips, vip)),
        timeout=45,
        interval=2.0,
        description=f"{vip} to be withdrawn from every node",
    )
    wait_until(
        lambda: allocator_metrics().counter(
            "purelb_address_pool_addresses_in_use", pool="default"
        ) < in_use,
        timeout=30,
        description="the pool's in-use count to drop",
    )


def test_specific_ip_request_is_honoured(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """spec.loadBalancerIP gets exactly that address, and it announces."""
    subnet = topo.subnets[0]
    wanted = f"{subnet.v4_octets}.210"
    ips = lb_service("nginx-lb-specific", ["IPv4"], loadBalancerIP=wanted)
    assert ips == [wanted], f"asked for {wanted}, got {ips}"
    wait_until(
        lambda: announced_on(topo, wanted),
        timeout=45,
        description=f"{wanted} to be announced",
    )


def test_foreign_loadbalancer_class_is_ignored(
    cluster: Cluster, default_servicegroup: str, lb_service
):
    """A Service for another controller's class must be left alone.

    PureLB claiming a Service that belongs to a different load-balancer
    implementation is worse than not allocating: two controllers then
    fight over the same status field.
    """
    lb_service(
        "nginx-foreign-lbclass", ["IPv4"], wait=False, loadBalancerClass="other.io/foreign-lb"
    )
    with pytest.raises(AssertionError):
        wait_until(
            lambda: cluster.service_ingress_ips(NAMESPACE, "nginx-foreign-lbclass") or None,
            timeout=15,
            description="an address that should never arrive",
        )
    svc = cluster.service(NAMESPACE, "nginx-foreign-lbclass")
    assert (svc.metadata.annotations or {}).get(ALLOCATED_BY) is None, (
        "PureLB annotated a Service whose loadBalancerClass names another controller"
    )


def test_explicit_purelb_loadbalancer_class_allocates(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """The other half: naming PureLB's own class explicitly must work.

    Only rejecting foreign classes would be satisfied by a build that
    ignores loadBalancerClass entirely and allocates for nothing.
    """
    ips = lb_service(
        "nginx-purelb-lbclass", ["IPv4"], loadBalancerClass="purelb.io/purelbv2"
    )
    address = ips[0]
    subnet = topo.subnet_holding(address)
    assert subnet is not None, f"{address} is in no node subnet"

    svc = cluster.service(NAMESPACE, "nginx-purelb-lbclass")
    assert (svc.metadata.annotations or {}).get(ALLOCATED_BY) == "PureLB"

    wait_until(lambda: announced_on(topo, address), timeout=45,
               description=f"{address} to be announced")
    assert "Pod:" in curl_from_node(topo, address)


def test_shared_ip_puts_two_services_on_one_address(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """Two Services with the same sharing key share one VIP.

    They must also use different ports: sharing is only legal when the
    services do not collide, and PureLB is what enforces that.
    """
    first = lb_service("nginx-shared-http", ["IPv4"], annotations={SHARING: "webservers"})
    second = lb_service("nginx-shared-https", ["IPv4"], annotations={SHARING: "webservers"},
                        ports=[{"port": 443, "targetPort": 80}])
    assert first == second, f"sharing key did not share: {first} vs {second}"

    vip = first[0]
    found = wait_until(lambda: announced_on(topo, vip), timeout=45,
                       description=f"{vip} to be announced")
    assert found is not None

    # Deleting one holder must NOT withdraw the address: the other still
    # has it. Getting this wrong is a withdrawal-refcount bug, and it
    # takes down a live service.
    cluster.delete_service(NAMESPACE, "nginx-shared-http")
    cluster.wait_service_gone(NAMESPACE, "nginx-shared-http")
    still = wait_until(lambda: announced_on(topo, vip), timeout=20,
                       description=f"{vip} to remain announced for the surviving service")
    assert still is not None, f"{vip} was withdrawn while nginx-shared-https still holds it"


def test_multiple_services_get_distinct_addresses(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """No two Services may be handed the same address.

    Without a sharing key, a duplicate is a double-allocation: two
    services with the same VIP and no port arbitration.
    """
    allocated = []
    for i in range(3):
        allocated.extend(lb_service(f"nginx-lb-multi-{i}", ["IPv4"]))
    assert len(set(allocated)) == len(allocated), f"duplicate addresses: {allocated}"


def test_no_duplicate_vips_across_nodes(topo: topology.Topology, default_servicegroup: str, lb_service):
    """Exactly one node announces each address.

    Two nodes holding the same VIP is a split brain: ARP resolves to
    whichever replied last and traffic flaps between them.
    """
    ips = lb_service("nginx-lb-unique", ["IPv4"])
    vip = ips[0]
    wait_until(lambda: announced_on(topo, vip), timeout=45, description=f"{vip} announced")
    holders = nodes.nodes_with_address(topo.node_ips, vip)
    assert len(holders) == 1, f"{vip} is announced by {holders}, expected exactly one node"
