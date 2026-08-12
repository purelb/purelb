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

"""Allocation policy, pool edge cases and end-to-end traffic.

The last group ported from test/e2e/local/test-local-allocation.sh:
ETP handling, the re-evaluate trigger, pools with no eligible node, idle
ServiceGroup status, graceful shutdown, and cross-node connectivity.

Two of these assert that something does NOT happen, which is where the
value is concentrated:

* A pool whose subnet matches NO node gets an address allocated and then
  announced NOWHERE. Falling back to "some node" would be worse than
  failing: the address would be up, serving nothing, on a wire its
  gateway cannot reach.

* ExternalTrafficPolicy: Local on a LOCAL pool is overridden to Cluster
  unless purelb.io/allow-local says otherwise. Honouring it would strand
  traffic on the announcing node whenever that node has no endpoint --
  the announcer and the endpoints are elected independently.
"""

from __future__ import annotations

import ipaddress
from typing import Dict, List

import pytest

from purelb_e2e import TEST_NAMESPACE, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
ALLOW_LOCAL = "purelb.io/allow-local"
RE_EVALUATE = "purelb.io/re-evaluate"
SERVICE_GROUP = "purelb.io/service-group"
AGENT_SELECTOR = "component=lbnodeagent"


# ------------------------------------------------------ no eligible node


def test_pool_with_no_matching_subnet_allocates_but_announces_nowhere(
    cluster: Cluster, topo: topology.Topology, lb_service, log_window
):
    """Subnet-aware election with no candidate must not fall back.

    The address is allocated -- the allocator has a pool and hands one
    out -- but no node has that subnet, so no node may announce it.
    Picking "some node" would put the VIP on a wire whose gateway will
    never ARP for it: up, elected, and unreachable.
    """
    unmatched = "10.255.0.0/24"
    assert topo.subnet_holding("10.255.0.10") is None, (
        f"{unmatched} unexpectedly matches a node subnet on this cluster"
    )
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "no-match-subnet", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "v4pools": [
                        {"aggregation": "default", "pool": "10.255.0.10-10.255.0.20",
                         "subnet": unmatched}
                    ]
                }
            },
        }
    )
    try:
        ips = lb_service(
            "nginx-lb-no-match", ["IPv4"],
            annotations={SERVICE_GROUP: "no-match-subnet"},
        )
        vip = ips[0]
        assert ipaddress.ip_address(vip) in ipaddress.ip_network(unmatched)

        # Give the announcers time to get it wrong, then confirm they did
        # not. A single immediate check would pass simply by being early.
        import time
        for _ in range(6):
            time.sleep(2)
            holders = nodes.nodes_with_address(topo.node_ips, vip)
            assert not holders, (
                f"{vip} is from {unmatched}, which no node is on, but "
                f"{holders} announced it anyway"
            )

        # Silence is the right behaviour but a poor diagnostic, so the
        # agents say why. Without this the test cannot tell "correctly
        # declined" from "never saw the Service at all".
        logs = cluster.component_logs("lbnodeagent", log_window)
        assert any("noLocalInterface" in text for text in logs.values()), (
            "no agent logged noLocalInterface; the address was not announced, "
            "but nothing shows that subnet filtering is the reason"
        )
    finally:
        cluster.delete_service(NAMESPACE, "nginx-lb-no-match")
        cluster.delete_cr("servicegroup", "no-match-subnet")


# --------------------------------------------------------- idle SG status


def test_idle_servicegroup_publishes_status(
    cluster: Cluster, topo: topology.Topology, allocator_metrics
):
    """A pool reports its capacity before anything is allocated from it.

    Status that only appeared after the first allocation would make an
    empty pool indistinguishable from a broken one.
    """
    subnet = topo.subnets[0]
    local: Dict[str, object] = {
        "v4pools": [
            {"aggregation": "default", "pool": subnet.test_pool, "subnet": subnet.v4}
        ]
    }
    if subnet.v6:
        local["v6pools"] = [
            {"aggregation": "default", "pool": subnet.test_pool_v6, "subnet": subnet.v6}
        ]

    before = allocator_metrics()
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "idle-pool", "namespace": cluster.purelb_namespace},
            "spec": {"local": local},
        }
    )
    try:
        status = wait_until(
            lambda: (cluster.get_cr("servicegroup", "idle-pool") or {}).get("status") or None,
            timeout=45, interval=2.0,
            description="the idle pool to publish status",
        )
        assert status.get("addresses"), f"status.addresses empty: {status}"
        assert status.get("allocatedIPv4", 0) == 0, "an idle pool cannot have allocations"
        assert status.get("availableIPv4", 0) > 0, "an idle pool must report capacity"
        if subnet.v6:
            assert status.get("allocatedIPv6", 0) == 0
            assert status.get("availableIPv6", 0) > 0

        after = allocator_metrics()
        metrics.assert_increased(before, after, "purelb_allocator_sg_status_writes_total",
                                 outcome="success")
        # Errors are compared as a DELTA. Asserting the absolute count is
        # zero fails forever after any historical error -- deleting a
        # ServiceGroup makes the next status write 404, which is a normal
        # consequence of a normal action.
        for outcome in ("forbidden", "other"):
            metrics.assert_not_increased(
                before, after, "purelb_allocator_sg_status_writes_total", outcome=outcome
            )
    finally:
        cluster.delete_cr("servicegroup", "idle-pool")


# -------------------------------------------------------------------- ETP


def test_etp_local_is_overridden_for_a_local_pool(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """ExternalTrafficPolicy: Local is forced to Cluster on local pools.

    The announcing node and the nodes with endpoints are elected
    independently, so honouring Local would blackhole traffic whenever
    the announcer happens to have no endpoint. Local is meaningful for
    REMOTE pools, where every node announces.
    """
    vip = lb_service(
        "nginx-etp-local", ["IPv4"], externalTrafficPolicy="Local"
    )[0]
    svc = cluster.service(NAMESPACE, "nginx-etp-local")
    assert svc.spec.external_traffic_policy == "Cluster", (
        f"ETP is {svc.spec.external_traffic_policy!r}; a local pool without "
        f"{ALLOW_LOCAL} must be overridden to Cluster"
    )
    wait_until(lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
               description=f"{vip} to be announced")
    assert "Pod:" in nodes.curl_via_node(topo.node_ips, vip)


def test_allow_local_annotation_preserves_etp_local(
    cluster: Cluster, default_servicegroup: str, lb_service
):
    """The opt-out is honoured, so the override is a policy not a bug."""
    lb_service(
        "nginx-etp-allow", ["IPv4"],
        annotations={ALLOW_LOCAL: "true"}, externalTrafficPolicy="Local",
    )
    svc = cluster.service(NAMESPACE, "nginx-etp-allow")
    assert svc.spec.external_traffic_policy == "Local", (
        f"{ALLOW_LOCAL} was set but ETP is {svc.spec.external_traffic_policy!r}"
    )


# ------------------------------------------------------------ re-evaluate


def test_re_evaluate_annotation_is_consumed(
    cluster: Cluster, default_servicegroup: str, lb_service
):
    """The one-shot trigger is cleared, and the address survives it.

    A trigger that is not cleared re-fires on every resync. An address
    that does not survive would make the annotation a re-allocation, and
    a live service would change address under a support command.
    """
    before = lb_service("nginx-reeval", ["IPv4"])[0]
    cluster.core.patch_namespaced_service(
        "nginx-reeval", NAMESPACE,
        {"metadata": {"annotations": {RE_EVALUATE: "true"}}},
    )
    wait_until(
        lambda: cluster.annotation(NAMESPACE, "nginx-reeval", RE_EVALUATE) in (None, "")
        or None,
        timeout=45, interval=2.0,
        description="the re-evaluate annotation to be consumed",
    )
    after = cluster.service_ingress_ips(NAMESPACE, "nginx-reeval")
    assert after == [before], f"address changed across re-evaluate: {before} -> {after}"


# ------------------------------------------------------ graceful shutdown


@pytest.mark.requires("multi-node")
def test_graceful_shutdown_releases_the_lease(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    log_window,
):
    """An agent stopped cleanly gives its lease up rather than expiring.

    Waiting for expiry costs the lease duration; releasing on shutdown
    lets the next winner take over immediately. The difference is visible
    as failover latency, which is the number that matters in an outage.
    """
    vip = lb_service("nginx-lb-graceful", ["IPv4"])[0]
    holder, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
        description=f"{vip} to be announced",
    )

    pod = cluster.pod_on_node(cluster.purelb_namespace, AGENT_SELECTOR, holder)
    assert pod is not None
    departing = pod.metadata.name
    # Capture the departing agent's log BEFORE it goes: once the pod is
    # gone its logs go with it, so a shutdown assertion has to read them
    # while the container is still terminating.
    shutdown_log = ""

    # No taint: the DaemonSet reschedules immediately, which is what a
    # rolling restart or a node drain looks like.
    cluster.delete_pod(cluster.purelb_namespace, departing, grace_seconds=30)
    for _ in range(20):
        try:
            shutdown_log = cluster.pod_logs(
                cluster.purelb_namespace, departing, log_window, container="lbnodeagent"
            )
        except Exception:  # noqa: BLE001 - the pod is on its way out
            break
        if "withdrawAddress" in shutdown_log or "ForceSync" in shutdown_log:
            break
        import time
        time.sleep(1)
    assert "withdrawAddress" in shutdown_log or "ForceSync" in shutdown_log, (
        f"the agent on {holder} exited without logging a withdrawal; a "
        f"graceful stop must give the address up rather than abandoning it"
    )

    moved = wait_until(
        lambda: (lambda f: f if f and f[0] != holder else None)(
            nodes.announcing_node(topo.node_ips, vip)
        ),
        timeout=90, interval=2.0,
        description=f"{vip} to move off {holder} after a graceful stop",
    )
    assert "Pod:" in nodes.curl_via_node(topo.node_ips, vip), (
        f"{vip} moved to {moved[0]} but stopped serving"
    )

    wait_until(
        lambda: cluster.daemonset_ready(
            cluster.purelb_namespace, "lbnodeagent", expect_nodes=len(topo.node_ips)
        ) or None,
        timeout=180, interval=3.0,
        description="the DaemonSet to be whole again",
    )


# ------------------------------------------------------------ connectivity


@pytest.mark.requires("multi-node")
def test_traffic_reaches_pods_on_nodes_other_than_the_announcer(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
):
    """The VIP load-balances beyond the node holding it.

    With ETP Cluster the announcing node forwards to endpoints anywhere
    in the cluster, which requires IP forwarding to be on and the CNI to
    be healthy. If every response came from the announcer's own node the
    VIP would still look fine while being a single point of failure.
    """
    replicas = max(3, len(topo.node_ips))
    cluster.apps.patch_namespaced_deployment_scale(
        "nginx", NAMESPACE, {"spec": {"replicas": replicas}}
    )
    try:
        cluster.wait_rollout(NAMESPACE, "nginx", timeout=180)
        vip = lb_service("nginx-lb-spread", ["IPv4"])[0]
        holder, _ = wait_until(
            lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
            description=f"{vip} to be announced",
        )

        # The backend reports its pod name; map that to a node.
        pod_node = {
            p.metadata.name: p.spec.node_name
            for p in cluster.pods(NAMESPACE, "app=nginx")
        }
        seen: set = set()
        for _ in range(30):
            body = nodes.curl_via_node(topo.node_ips, vip)
            for name, node in pod_node.items():
                if name in body:
                    seen.add(node)
            if len(seen) > 1:
                break
        assert seen, "no response identified a backend pod"
        assert seen - {holder}, (
            f"every response came from a pod on the announcing node {holder} "
            f"({seen}); traffic is not being forwarded off it, so IP "
            f"forwarding or the CNI overlay is broken"
        )
    finally:
        cluster.apps.patch_namespaced_deployment_scale(
            "nginx", NAMESPACE, {"spec": {"replicas": 1}}
        )


@pytest.mark.requires("ipv6")
def test_ipv6_vip_serves_traffic(
    topo: topology.Topology, default_servicegroup: str, lb_service
):
    """The IPv6 data path, end to end.

    Separate from the IPv4 case because an IPv6 VIP can be announced
    correctly and still not carry traffic -- forwarding is a per-family
    sysctl, and ip6_forward being off is a distinct failure.
    """
    vip = lb_service("nginx-lb-v6-traffic", ["IPv6"])[0]
    wait_until(lambda: nodes.announcing_node(topo.node_ips, vip), timeout=45,
               description=f"{vip} to be announced")
    assert "Pod:" in nodes.curl_via_node(topo.node_ips, vip), (
        f"IPv6 VIP {vip} is announced but serves nothing; check ip6 forwarding"
    )
