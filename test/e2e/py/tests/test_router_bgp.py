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

"""BGP: what the upstream router actually learns.

Ported from test/e2e/router/test-router-connectivity-frr.sh.

Every other module asks whether PureLB put an address on an interface.
This one asks the only question that matters for remote mode: does the
ROUTER have a route to it, via the right next-hops, with the right
prefix length. An address can be perfectly placed on kube-lb0 on every
node and advertise nothing, or advertise a /24 when it meant a /32 and
blackhole an entire subnet nobody was testing.

The route is read from FRR's RIB as JSON. The bash suite scraped vtysh
with `grep -oP '\\*\\s+\\K[0-9.]+'`, which depends on the column layout
and on which next-hop FRR marks with an asterisk -- both presentation
details that change between releases and would silently start matching
nothing, turning every route assertion into "no route found".

These tests need `--router-host`, and skip visibly without it. They also
need the nodes to be BGP-peered with that router; the session check is
the first test, so a peering problem reports itself rather than
surfacing as sixteen mysterious route failures.
"""

from __future__ import annotations

import ipaddress
import time
from typing import Dict, List, Optional

import pytest

from purelb_e2e import TEST_NAMESPACE, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
SERVICE_GROUP = "purelb.io/service-group"
SHARING = "purelb.io/allow-shared-ip"
DUMMY_IFACE = "kube-lb0"

BGP_GROUP = "router-bgp"
POOL_V4 = "10.255.0.100-10.255.0.150"
SUBNET_V4 = "10.255.0.0/24"
POOL_V6 = "fd00:10:255::100-fd00:10:255::150"
SUBNET_V6 = "fd00:10:255::/64"

# BGP convergence is not instant and is not under PureLB's control, so
# route assertions poll rather than sample.
CONVERGE = 45.0

pytestmark = pytest.mark.requires("router")


def host_prefix(address: str) -> str:
    """The /32 or /128 for an address, which is how FRR keys the RIB."""
    ip = ipaddress.ip_address(address)
    return f"{address}/{32 if ip.version == 4 else 128}"


@pytest.fixture
def bgp_group(cluster: Cluster):
    """Remote ServiceGroups whose addresses are advertised by BGP."""
    created: List[str] = []

    def make(name: str = BGP_GROUP, aggregation: Optional[str] = "/32",
             v6_aggregation: Optional[str] = "/128") -> str:
        v4pool: Dict[str, object] = {"pool": POOL_V4, "subnet": SUBNET_V4}
        v6pool: Dict[str, object] = {"pool": POOL_V6, "subnet": SUBNET_V6}
        if aggregation:
            v4pool["aggregation"] = aggregation
        if v6_aggregation:
            v6pool["aggregation"] = v6_aggregation
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {"remote": {"v4pools": [v4pool], "v6pools": [v6pool]}},
            }
        )
        created.append(name)
        return name

    yield make

    for name in reversed(created):
        cluster.delete_cr("servicegroup", name)


def wait_for_route(router, prefix: str, timeout: float = CONVERGE) -> dict:
    return wait_until(
        lambda: router.route(prefix),
        timeout=timeout, interval=3.0,
        description=f"FRR to learn a route to {prefix}",
    )


def wait_for_withdrawal(router, prefix: str, timeout: float = CONVERGE) -> None:
    wait_while(
        lambda: router.route(prefix) is not None,
        timeout=timeout, interval=3.0,
        description=f"FRR to withdraw the route to {prefix}",
    )


# ------------------------------------------------------------- peering


def test_the_router_is_peered_with_every_node(
    router, topo: topology.Topology
):
    """Established sessions with all of them, before anything else.

    Run first on purpose: without peering every route assertion in this
    module fails identically, and "no route found" is a poor way to learn
    that BGP was never up.
    """
    peers = router.bgp_peers()
    assert peers, "the router has no established BGP sessions at all"
    assert len(peers) >= len(topo.node_ips), (
        f"{len(peers)} established session(s) for {len(topo.node_ips)} nodes: "
        f"{sorted(peers)}"
    )


# ------------------------------------------------- routes and next-hops


@pytest.mark.parametrize("family", ["IPv4", "IPv6"])
def test_the_router_learns_a_host_route_via_every_node(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
    family: str,
):
    """A VIP becomes a /32 (or /128) in the router's RIB, ECMP over all nodes.

    Every node announces a remote address, so every node advertises it,
    and the router should end up with one next-hop per node. Fewer
    next-hops is not a cosmetic difference: it is capacity and redundancy
    silently missing.
    """
    if family == "IPv6" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6")

    group = bgp_group()
    vip = lb_service(
        f"router-{family.lower()}", [family],
        annotations={SERVICE_GROUP: group}, timeout=90,
    )[0]
    prefix = host_prefix(vip)

    wait_for_route(router, prefix)
    hops = wait_until(
        lambda: (lambda got: got if len(got) >= len(topo.node_ips) else None)(
            router.nexthops(prefix)
        ),
        timeout=CONVERGE, interval=3.0,
        description=f"{prefix} to have one next-hop per node",
    )
    assert len(hops) == len(topo.node_ips), (
        f"{prefix} has {len(hops)} next-hops for {len(topo.node_ips)} nodes: {hops}"
    )


@pytest.mark.parametrize(
    "aggregation,expected", [("/32", 32), ("default", 24)],
    ids=["host-route", "subnet-aggregate"],
)
def test_the_advertised_prefix_length_matches_the_aggregation(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
    aggregation: str, expected: int,
):
    """Aggregation decides what the ROUTER learns, which is the point of it.

    Asserting the mask on kube-lb0 shows PureLB configured what it meant
    to. Asserting the prefix in the RIB shows the router agreed -- and
    the gap between those two is where an aggregate that swallows a
    neighbouring subnet would hide.
    """
    group = bgp_group(f"router-aggr-{expected}", aggregation=aggregation)
    vip = lb_service(
        f"router-aggr-{expected}", ["IPv4"],
        annotations={SERVICE_GROUP: group}, timeout=90,
    )[0]

    network = ipaddress.ip_network(f"{vip}/{expected}", strict=False)
    prefix = str(network)
    wait_for_route(router, prefix)
    entry = router.route(prefix)
    assert entry is not None, f"FRR has no {prefix}"
    learned = entry.get("prefixLen") or int(str(entry.get("prefix", prefix)).split("/")[-1])
    assert learned == expected, (
        f"aggregation {aggregation!r} should be advertised as /{expected}, "
        f"FRR learned /{learned}"
    )


# ------------------------------------------------------------ withdrawal


def test_deleting_the_service_withdraws_the_route(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service
):
    """The route goes when the Service does.

    A route that outlives its Service is a blackhole: the router keeps
    sending traffic to nodes that no longer answer for the address.
    """
    group = bgp_group()
    vip = lb_service("router-withdraw", ["IPv4"],
                     annotations={SERVICE_GROUP: group}, timeout=90)[0]
    prefix = host_prefix(vip)
    wait_for_route(router, prefix)

    cluster.delete_service(NAMESPACE, "router-withdraw")
    wait_for_withdrawal(router, prefix)


@pytest.mark.requires("multi-node")
def test_losing_a_node_drops_only_its_next_hop(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
    tainted_nodes,
):
    """One node down means one next-hop fewer, not a withdrawn route.

    This is what ECMP buys and the assertion that proves it: the route
    must survive with the remaining next-hops. A route that disappears
    entirely when one of five nodes goes away would be an outage caused
    by redundancy.
    """
    group = bgp_group()
    vip = lb_service("router-nodefail", ["IPv4"],
                     annotations={SERVICE_GROUP: group}, timeout=90)[0]
    prefix = host_prefix(vip)
    wait_until(
        lambda: (lambda got: got if len(got) >= len(topo.node_ips) else None)(
            router.nexthops(prefix)
        ),
        timeout=CONVERGE, interval=3.0,
        description=f"{prefix} to have every node as a next-hop",
    )

    victim = sorted(topo.node_ips)[-1]
    victim_ip = topo.node_ips[victim]
    tainted_nodes(victim)
    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", victim)
    if pod is not None:
        cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)

    remaining = wait_until(
        lambda: (lambda got: got if victim_ip not in got else None)(
            router.nexthops(prefix)
        ),
        timeout=90, interval=3.0,
        description=f"FRR to drop {victim_ip} as a next-hop for {prefix}",
    )
    assert remaining, (
        f"{prefix} lost ALL next-hops when {victim} went away; one node "
        f"failing must not withdraw the route"
    )
    assert len(remaining) == len(topo.node_ips) - 1, (
        f"expected {len(topo.node_ips) - 1} next-hops after losing {victim}, "
        f"got {remaining}"
    )


# ------------------------------------------------------------- ETP Local


def test_etp_local_narrows_the_next_hops_to_endpoint_nodes(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
    pinned_backend,
):
    """ETP Local is visible in the RIB, not only on the interfaces.

    This is the assertion that ties the two halves together: PureLB
    withdraws the address from nodes without endpoints, and the router
    must therefore stop using them as next-hops. If it did not, traffic
    would keep arriving at a node that no longer answers -- which is the
    failure ETP Local exists to prevent.
    """
    group = bgp_group()
    target = sorted(topo.node_ips)[0]
    backend = pinned_backend("router-etp-backend", target)
    vip = lb_service(
        "router-etp", ["IPv4"], annotations={SERVICE_GROUP: group},
        selector={"app": backend}, externalTrafficPolicy="Local", timeout=90,
    )[0]
    prefix = host_prefix(vip)

    hops = wait_until(
        lambda: (lambda got: got if got == [topo.node_ips[target]] else None)(
            router.nexthops(prefix)
        ),
        timeout=90, interval=3.0,
        description=f"{prefix} to have only {target} as a next-hop",
    )
    assert hops == [topo.node_ips[target]], (
        f"{prefix} next-hops are {hops}; with ETP Local only {target} holds an "
        f"endpoint, so only it should be advertising"
    )


# ------------------------------------------------------------- sharing


def test_two_services_sharing_an_address_produce_one_route(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service
):
    """A shared address is one route, and it survives losing one holder.

    Withdrawing on the first delete would take down the service that is
    still using it, and the router is where that becomes an outage rather
    than a local mistake.
    """
    group = bgp_group()
    first = lb_service("router-share-a", ["IPv4"],
                       annotations={SERVICE_GROUP: group, SHARING: "router-key"},
                       timeout=90)[0]
    second = lb_service("router-share-b", ["IPv4"],
                        annotations={SERVICE_GROUP: group, SHARING: "router-key"},
                        ports=[{"port": 8443, "targetPort": 80}], timeout=90)[0]
    assert first == second, f"sharing key did not share: {first} vs {second}"

    prefix = host_prefix(first)
    wait_for_route(router, prefix)

    cluster.delete_service(NAMESPACE, "router-share-a")
    cluster.wait_service_gone(NAMESPACE, "router-share-a")
    # Hold: the route must NOT be withdrawn while the other holder lives.
    for _ in range(6):
        time.sleep(3)
        assert router.route(prefix) is not None, (
            f"{prefix} was withdrawn from the router while router-share-b "
            f"still holds the address"
        )


# -------------------------------------------------- external connectivity


def curl_external(address: str, timeout: float = 10.0) -> str:
    """GET the VIP from THIS machine, off-cluster.

    The whole point of BGP is that something outside the cluster can
    reach the address, and this is the only assertion in the suite that
    proves it end to end: the workstation has no special knowledge, it
    just follows the route the router learned.
    """
    import subprocess

    host = ipaddress.ip_address(address)
    url = f"http://[{address}]/" if host.version == 6 else f"http://{address}/"
    proc = subprocess.run(  # noqa: S603
        ["curl", "-s", "--connect-timeout", str(int(timeout)), url],
        capture_output=True, text=True, timeout=timeout + 10,
    )
    return proc.stdout


@pytest.mark.parametrize("family", ["IPv4", "IPv6"])
def test_the_vip_is_reachable_from_outside_the_cluster(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
    family: str,
):
    """Traffic from off-cluster follows the advertised route to a pod.

    Every other assertion here inspects state -- the RIB, the next-hops,
    the interfaces. This one is the only end-to-end proof that the state
    adds up to a working load balancer, and it is the reason the router
    tests exist at all.
    """
    if family == "IPv6" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6")

    group = bgp_group()
    vip = lb_service(
        f"router-external-{family.lower()}", [family],
        annotations={SERVICE_GROUP: group}, timeout=90,
    )[0]
    wait_for_route(router, host_prefix(vip))

    body = wait_until(
        lambda: curl_external(vip) or None,
        timeout=60, interval=5.0,
        description=f"{vip} to answer from off-cluster",
    )
    assert "Pod:" in body, (
        f"the router has a route to {vip} but it serves nothing from "
        f"off-cluster; got {body[:200]!r}"
    )


# ------------------------------------------------ ETP Local, full cycle


def test_etp_local_next_hops_track_the_endpoint_count(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
):
    """Scale the backend and the router's next-hops follow.

    The sharp phase is ZERO endpoints: the route must be withdrawn
    entirely, not left with no next-hops. A route present with nothing
    behind it is a blackhole that looks healthy in the RIB, and it is
    exactly what ETP Local is supposed to prevent.
    """
    group = bgp_group()
    vip = lb_service(
        "router-etp-cycle", ["IPv4"], annotations={SERVICE_GROUP: group},
        externalTrafficPolicy="Local", timeout=90,
    )[0]
    prefix = host_prefix(vip)

    def scale(replicas: int) -> None:
        cluster.apps.patch_namespaced_deployment_scale(
            "nginx", NAMESPACE, {"spec": {"replicas": replicas}}
        )

    original = cluster.deployment(NAMESPACE, "nginx").spec.replicas or 1
    try:
        # Two endpoints on (at most) two nodes.
        scale(2)
        cluster.wait_rollout(NAMESPACE, "nginx", timeout=180)
        endpoint_nodes = wait_until(
            lambda: {p.spec.node_name for p in cluster.pods(NAMESPACE, "app=nginx")
                     if p.status.phase == "Running"} or None,
            timeout=120, interval=3.0, description="endpoint nodes",
        )
        hops = wait_until(
            lambda: (lambda got: got if len(got) == len(endpoint_nodes) else None)(
                router.nexthops(prefix)
            ),
            timeout=120, interval=3.0,
            description=f"{prefix} next-hops to match {len(endpoint_nodes)} endpoint node(s)",
        )
        assert len(hops) == len(endpoint_nodes), (hops, endpoint_nodes)

        # Zero endpoints: the route goes away entirely.
        scale(0)
        wait_until(
            lambda: (not cluster.pods(NAMESPACE, "app=nginx")) or None,
            timeout=120, interval=3.0, description="every backend pod to go",
        )
        wait_for_withdrawal(router, prefix, timeout=120)

        # And comes back.
        scale(1)
        cluster.wait_rollout(NAMESPACE, "nginx", timeout=180)
        restored = wait_until(
            lambda: router.nexthops(prefix) or None,
            timeout=120, interval=3.0,
            description=f"{prefix} to be re-advertised once an endpoint exists",
        )
        assert len(restored) == 1, (
            f"one endpoint should give one next-hop, got {restored}"
        )
    finally:
        scale(original)
        cluster.wait_rollout(NAMESPACE, "nginx", timeout=180)


# ------------------------------------------------ aggregation exclusivity


@pytest.mark.parametrize(
    "aggregation,wanted,unwanted",
    [("/32", 32, 24), ("default", 24, 32)],
    ids=["host-route-only", "aggregate-only"],
)
def test_aggregation_advertises_one_prefix_and_not_the_other(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
    aggregation: str, wanted: int, unwanted: int,
):
    """The prefix that was asked for, and NOT the one that was not.

    Presence alone is only half the assertion and the weaker half. A build
    that advertised both a /32 and a /24 would pass every "route found"
    check while claiming an entire subnet it was never given -- addresses
    belonging to other systems, blackholed at this cluster. The absence is
    the property that actually protects the network.
    """
    group = bgp_group(f"router-excl-{wanted}", aggregation=aggregation)
    vip = lb_service(
        f"router-excl-{wanted}", ["IPv4"],
        annotations={SERVICE_GROUP: group}, timeout=90,
    )[0]

    want_prefix = str(ipaddress.ip_network(f"{vip}/{wanted}", strict=False))
    unwanted_prefix = str(ipaddress.ip_network(f"{vip}/{unwanted}", strict=False))
    wait_for_route(router, want_prefix)

    # Give the wrong one every chance to appear before concluding it did not.
    for _ in range(5):
        time.sleep(2)
        assert router.route(unwanted_prefix) is None, (
            f"aggregation {aggregation!r} advertised {want_prefix} as asked, but "
            f"ALSO {unwanted_prefix}. The extra prefix covers addresses this "
            f"cluster was never given."
        )


def test_an_aggregate_route_survives_losing_one_of_its_services(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service
):
    """With default aggregation, two services share one /24 route.

    The aggregate is derived from the pool, not from any one Service, so
    deleting one of two holders must leave it standing. Withdrawing it
    would take the surviving service off the network -- and because the
    route covers the whole pool, everything else in that pool too.
    """
    group = bgp_group("router-aggr-shared", aggregation="default")
    first = lb_service("router-aggr-a", ["IPv4"],
                       annotations={SERVICE_GROUP: group}, timeout=90)[0]
    second = lb_service("router-aggr-b", ["IPv4"],
                        annotations={SERVICE_GROUP: group}, timeout=90)[0]
    assert first != second, f"two services got the same address: {first}"

    aggregate = str(ipaddress.ip_network(f"{first}/24", strict=False))
    wait_for_route(router, aggregate)

    cluster.delete_service(NAMESPACE, "router-aggr-a")
    cluster.wait_service_gone(NAMESPACE, "router-aggr-a")
    for _ in range(6):
        time.sleep(3)
        assert router.route(aggregate) is not None, (
            f"{aggregate} was withdrawn when router-aggr-a went away, while "
            f"router-aggr-b still holds {second} inside it"
        )


def test_a_withdrawn_vip_stops_serving_from_outside(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service
):
    """Withdrawal has to actually stop traffic, not just tidy the RIB.

    The positive connectivity test proves the route works. This proves
    the withdrawal does, which is the half that matters during an
    outage: a route removed from the RIB while the address still answers
    means something else is carrying it.
    """
    group = bgp_group()
    vip = lb_service("router-unreach", ["IPv4"],
                     annotations={SERVICE_GROUP: group}, timeout=90)[0]
    prefix = host_prefix(vip)
    wait_for_route(router, prefix)
    assert "Pod:" in wait_until(
        lambda: curl_external(vip) or None,
        timeout=60, interval=5.0, description=f"{vip} to answer before withdrawal",
    )

    cluster.delete_service(NAMESPACE, "router-unreach")
    wait_for_withdrawal(router, prefix)
    wait_until(
        lambda: ("Pod:" not in curl_external(vip, timeout=5.0)) or None,
        timeout=60, interval=5.0,
        description=f"{vip} to stop answering once its route is withdrawn",
    )


@pytest.mark.requires("multi-node")
def test_next_hops_are_restored_when_a_node_comes_back(
    cluster: Cluster, topo: topology.Topology, router, bgp_group, lb_service,
    tainted_nodes,
):
    """Recovery, not just failure.

    A node that returns must be advertised again. If it were not, every
    node failure would permanently shrink the ECMP set and the cluster
    would quietly lose capacity with each incident.
    """
    group = bgp_group()
    vip = lb_service("router-recover", ["IPv4"],
                     annotations={SERVICE_GROUP: group}, timeout=90)[0]
    prefix = host_prefix(vip)
    full = len(topo.node_ips)
    wait_until(
        lambda: (lambda got: got if len(got) == full else None)(router.nexthops(prefix)),
        timeout=CONVERGE, interval=3.0, description=f"{prefix} with all {full} next-hops",
    )

    victim = sorted(topo.node_ips)[-1]
    tainted_nodes(victim)
    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", victim)
    if pod is not None:
        cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)
    wait_until(
        lambda: (lambda got: got if len(got) < full else None)(router.nexthops(prefix)),
        timeout=90, interval=3.0, description=f"{prefix} to lose {victim}",
    )

    cluster.remove_taint(victim, "purelb-test")
    wait_until(
        lambda: cluster.daemonset_ready(
            cluster.purelb_namespace, "lbnodeagent", expect_nodes=full
        ) or None,
        timeout=180, interval=3.0, description="the DaemonSet to be whole again",
    )
    restored = wait_until(
        lambda: (lambda got: got if len(got) == full else None)(router.nexthops(prefix)),
        timeout=120, interval=3.0,
        description=f"{prefix} to regain {victim} as a next-hop",
    )
    assert len(restored) == full, restored
