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

"""Remote pools, election losers, and traffic from inside the cluster.

The last gaps the dual-run mapping found in the local-suite port. Each of
these had bash assertions with no pytest counterpart, and none of them
was obvious from reading my own modules -- which is the argument for the
mapping being a gate rather than paperwork.

The remote-pool case is the counterpart to everything in
test_local_allocation: a remote address goes on the kube-lb0 dummy on
EVERY node rather than on one node's physical NIC, because it is reached
by routing rather than by ARP. Its pool need not be in any node subnet,
which is exactly why subnet-aware election must not apply to it.
"""

from __future__ import annotations

import ipaddress
from collections import Counter
from typing import Dict, List

import pytest

from purelb_e2e import TEST_NAMESPACE, announcing, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until

NAMESPACE = TEST_NAMESPACE
DUMMY_IFACE = "kube-lb0"
SERVICE_GROUP = "purelb.io/service-group"
AGENT_SELECTOR = "component=lbnodeagent"

REMOTE_GROUP = "remote-pool"
REMOTE_V4 = "10.255.1.100-10.255.1.110"
REMOTE_V4_SUBNET = "10.255.1.0/24"
REMOTE_V6 = "fc00:255:1::100-fc00:255:1::110"
REMOTE_V6_SUBNET = "fc00:255:1::/64"


# ------------------------------------------------------------- remote pool


@pytest.fixture
def remote_group(cluster: Cluster):
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": REMOTE_GROUP, "namespace": cluster.purelb_namespace},
            "spec": {
                "remote": {
                    "v4pools": [{"pool": REMOTE_V4, "subnet": REMOTE_V4_SUBNET}],
                    # No Service allocates from the v6 range; it exists so
                    # the idle-pool status assertions cover both families.
                    "v6pools": [{"pool": REMOTE_V6, "subnet": REMOTE_V6_SUBNET}],
                }
            },
        }
    )
    yield REMOTE_GROUP
    cluster.delete_cr("servicegroup", REMOTE_GROUP)


def test_remote_address_lands_on_the_dummy_interface_on_every_node(
    cluster: Cluster, topo: topology.Topology, remote_group: str, lb_service, log_window,
):
    """Remote is the opposite of local: everywhere, on kube-lb0.

    A remote address is reached by routing, so every node carries it on
    the dummy interface and no election picks a single winner. The pool
    is deliberately outside every node subnet -- if subnet-aware election
    applied here, nothing could announce it at all.
    """
    vip = lb_service(
        "nginx-remote", ["IPv4"], annotations={SERVICE_GROUP: remote_group}
    )[0]
    assert ipaddress.ip_address(vip) in ipaddress.ip_network(REMOTE_V4_SUBNET)
    assert topo.subnet_holding(vip) is None, (
        f"{vip} is inside a node subnet, so this test is not exercising the "
        f"remote path"
    )

    for node, ip in sorted(topo.node_ips.items()):
        found = wait_until(
            lambda h=ip: nodes.interface_for_address(h, vip),
            timeout=60, interval=2.0,
            description=f"{vip} to appear on {node}",
        )
        assert found == DUMMY_IFACE, (
            f"{vip} is on {found} on {node}; a remote address belongs on "
            f"{DUMMY_IFACE}, not a physical NIC"
        )

    # Every node announces it, so no announcing-<family> slot claims one
    # winner -- that annotation is a local-mode concept.
    value = cluster.annotation(NAMESPACE, "nginx-remote", announcing.annotation_key("IPv4"))
    assert not announcing.parse(value), (
        f"a remote address should claim no announcing slot, got {value!r}"
    )

    logs = cluster.component_logs("lbnodeagent", log_window)
    assert any("announcingNonLocal" in text for text in logs.values()), (
        "no agent logged announcingNonLocal for a remote pool"
    )


def test_idle_remote_pool_publishes_status_for_both_families(
    cluster: Cluster, remote_group: str
):
    """Capacity is reported before anything allocates, per family."""
    status = wait_until(
        lambda: (cluster.get_cr("servicegroup", remote_group) or {}).get("status") or None,
        timeout=45, interval=2.0,
        description="the remote pool to publish status",
    )
    assert status.get("addresses"), f"status.addresses empty: {status}"
    assert status.get("availableIPv4", 0) > 0, f"no IPv4 capacity: {status}"
    assert status.get("availableIPv6", 0) > 0, (
        f"no IPv6 capacity reported for a pool that has a v6 range: {status}"
    )


# --------------------------------------------------------- election losers


def test_losers_report_losing_and_nothing_panics(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    lb_service, log_window, agent_metrics,
):
    """The nodes that did NOT win must say so, and stay healthy.

    Only ever checking the winner leaves the majority of the cluster
    unobserved. A loser that never logs the loss, or whose lease goes
    unhealthy, is how a split brain starts.
    """
    vip = lb_service("nginx-lb-election", ["IPv4"])[0]
    winner, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip),
        timeout=45, description=f"{vip} to be announced",
    )

    holders = nodes.nodes_with_address(topo.node_ips, vip)
    assert holders == [winner], (
        f"{vip} is on {holders}; exactly one node must hold a local address"
    )

    losers = [n for n in topo.node_ips if n != winner]
    assert losers, "single-node cluster: there is no loser to observe"
    for node in losers:
        assert agent_metrics(node).value("purelb_election_lease_healthy") == 1.0, (
            f"{node} lost the election and its lease is unhealthy"
        )

    logs = cluster.component_logs("lbnodeagent", log_window)
    losing = [
        pod for pod, text in logs.items()
        if "notWinner" in text or "lostElection" in text
    ]
    assert losing, "no agent logged notWinner or lostElection for a contested address"

    # A panic anywhere in the fleet invalidates every other assertion in
    # the suite, so it is worth one cheap check.
    for pod, text in logs.items():
        for bad in ("panic:", "fatal error:", "runtime error:"):
            assert bad not in text, f"{pod} logged {bad!r} during this test"


# -------------------------------------------------------- load balancing


def test_one_vip_spreads_across_backend_pods(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
):
    """A single VIP reaches several backends, not just the nearest one.

    Distinct from "traffic leaves the announcing node": this is about the
    Service load-balancing at all. A VIP that always answers from one pod
    is a working address and a broken load balancer.
    """
    replicas = 3
    cluster.apps.patch_namespaced_deployment_scale(
        "nginx", NAMESPACE, {"spec": {"replicas": replicas}}
    )
    try:
        cluster.wait_rollout(NAMESPACE, "nginx", timeout=180)
        vip = lb_service("nginx-lb-spread-pods", ["IPv4"])[0]
        wait_until(lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
                   description=f"{vip} to be announced")

        pods = {p.metadata.name for p in cluster.pods(NAMESPACE, "app=nginx")}
        seen: Counter = Counter()
        for _ in range(30):
            body = nodes.curl_via_node(topo.node_ips, vip)
            for name in pods:
                if name in body:
                    seen[name] += 1
            if len(seen) >= 2:
                break
        assert len(seen) >= 2, (
            f"all {sum(seen.values())} responses came from {dict(seen)}; the VIP "
            f"is reaching only one of {len(pods)} backend pods"
        )
    finally:
        cluster.apps.patch_namespaced_deployment_scale(
            "nginx", NAMESPACE, {"spec": {"replicas": 1}}
        )


def test_a_pod_can_reach_a_vip(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
):
    """From inside the cluster network, not only from a node.

    Pod-to-VIP takes a different path from node-to-VIP: out through the
    CNI, to the announcing node, and back. It is the path an in-cluster
    client actually uses, and it breaks independently -- hairpin and
    overlay problems show up here and nowhere else.
    """
    vip = lb_service("nginx-lb-frompod", ["IPv4"])[0]
    wait_until(lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
               description=f"{vip} to be announced")

    name = "vip-client"
    cluster.core.create_namespaced_pod(
        NAMESPACE,
        {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {"name": name, "namespace": NAMESPACE},
            "spec": {
                "restartPolicy": "Never",
                "containers": [
                    {
                        "name": "client",
                        "image": "curlimages/curl:latest",
                        "command": ["sh", "-c", f"curl -s --max-time 10 http://{vip}/ || true"],
                    }
                ],
            },
        },
    )
    try:
        wait_until(
            lambda: cluster.core.read_namespaced_pod(name, NAMESPACE).status.phase
            in ("Succeeded", "Failed") or None,
            timeout=180, interval=3.0,
            description="the client pod to finish",
        )
        body = cluster.core.read_namespaced_pod_log(name=name, namespace=NAMESPACE)
        assert "Pod:" in body, (
            f"a pod could not reach VIP {vip} (got {body[:200]!r}); node-to-VIP "
            f"works, so this is the CNI path rather than the announcement"
        )
    finally:
        cluster.core.delete_namespaced_pod(name, NAMESPACE, grace_period_seconds=0)


# ------------------------------------------------------ incremental pools


@pytest.mark.requires("multi-subnet")
def test_a_service_picks_up_a_range_added_later(
    cluster: Cluster, topo: topology.Topology, lb_service,
):
    """multiPool is not only evaluated at creation time.

    An operator who adds a subnet to a running cluster expects existing
    multi-pool Services to gain an address there. If that only happened
    for Services created afterwards, the fleet would silently split into
    two generations.
    """
    first, second = topo.subnets[0], topo.subnets[1]
    name = "incremental-pool"

    def apply(subnets) -> None:
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {
                    "local": {
                        "multiPool": True,
                        "v4pools": [
                            {"aggregation": "default", "pool": s.test_pool, "subnet": s.v4}
                            for s in subnets
                        ],
                    }
                },
            }
        )

    apply([first])
    try:
        ips = lb_service(
            "nginx-incremental", ["IPv4"], annotations={SERVICE_GROUP: name}
        )
        assert len(ips) == 1, f"one range configured, expected one address, got {ips}"

        # Add the second range to the SAME ServiceGroup.
        apply([first, second])
        grown = wait_until(
            lambda: (lambda got: got if len(got) > 1 else None)(
                cluster.service_ingress_ips(NAMESPACE, "nginx-incremental")
            ),
            timeout=90, interval=3.0,
            description="the existing Service to pick up an address from the new range",
        )
        assert {topo.subnet_holding(a).v4 for a in grown} == {first.v4, second.v4}, (
            f"after adding a range the Service holds {grown}, which does not "
            f"cover both {first.v4} and {second.v4}"
        )
    finally:
        cluster.delete_service(NAMESPACE, "nginx-incremental")
        cluster.delete_cr("servicegroup", name)


# ------------------------------------------------- cross-subnet traffic


@pytest.mark.requires("multi-subnet")
def test_a_node_can_reach_a_vip_on_the_other_subnet(
    cluster: Cluster, topo: topology.Topology, subnet_servicegroup, lb_service,
):
    """Traffic crosses the subnet boundary and back.

    The VIP is pinned to one subnet and reached from a node on the OTHER
    one, so the request leaves via the router, arrives at the announcing
    node, and the response finds its way home. Routing between the two
    subnets is a cluster property this suite otherwise assumes.
    """
    target, other = topo.subnets[0], topo.subnets[1]
    group = subnet_servicegroup("test-cross-subnet", target)
    vip = lb_service(
        "nginx-cross-subnet", ["IPv4"], annotations={SERVICE_GROUP: group}
    )[0]
    assert topo.subnet_holding(vip).v4 == target.v4

    holder, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
        description=f"{vip} to be announced",
    )
    assert holder in target.nodes

    source = other.nodes[0]
    body = nodes.ssh(
        topo.node_ips[source], f"curl -s --max-time 10 http://{vip}/", timeout=40
    )
    assert "Pod:" in body, (
        f"{source} on {other.v4} could not reach {vip} on {target.v4}; the VIP "
        f"is announced on {holder}, so this is routing between the subnets"
    )
