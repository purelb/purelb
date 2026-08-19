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

"""Remote-mode allocation: every node announces, on kube-lb0.

Ported from test/e2e/remote/test-remote-allocation.sh.

Remote is the inverse of local in almost every respect. The address goes
on the kube-lb0 dummy interface on EVERY node rather than on one node's
physical NIC, because it is reached by routing (BGP, or a static route)
rather than by ARP. There is no election winner, the pool need not be
inside any node subnet, and no announcing-<family> slot is claimed.

That inversion is why ExternalTrafficPolicy: Local means something here
and is overridden for local pools. Every node announces, so honouring
Local is coherent: the nodes WITH endpoints announce and the rest do not,
and traffic therefore never lands on a node that would have to forward it
somewhere else. The bulk of this module is that behaviour, because it is
the part with real states -- endpoints appearing, moving and vanishing --
and each transition is a chance to strand an address.

The other recurring theme is REJECTION. A remote pool is a small explicit
range, so asking for an address outside it, or one already taken, or one
too many, are all things an operator does by accident. Each must fail
visibly rather than silently allocating something else.
"""

from __future__ import annotations

import json
import ipaddress
import time
from typing import Dict, Iterator, List, Optional

import pytest

from purelb_e2e import TEST_NAMESPACE, announcing, backend, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE
DUMMY_IFACE = "kube-lb0"
SERVICE_GROUP = "purelb.io/service-group"
SHARING = "purelb.io/allow-shared-ip"
ADDRESSES = "purelb.io/addresses"
RE_EVALUATE = "purelb.io/re-evaluate"

REMOTE_GROUP = "remote-pool"
# Deliberately outside every node subnet: a remote address is routed, not
# ARPed, so subnet-aware election must not apply to it. A pool that
# happened to sit inside a node subnet would let a local-mode bug pass.
POOL_V4 = "10.255.1.100-10.255.1.110"
SUBNET_V4 = "10.255.1.0/24"
POOL_V6 = "fc00:255:1::100-fc00:255:1::110"
SUBNET_V6 = "fc00:255:1::/64"
# A range of exactly two, for exhaustion.
SMALL_V4 = "10.255.9.1-10.255.9.2"
SMALL_SUBNET = "10.255.9.0/24"


def events_for(cluster: Cluster, name: str, namespace: str = NAMESPACE) -> List[str]:
    got = cluster.core.list_namespaced_event(
        namespace, field_selector=f"involvedObject.name={name}"
    )
    return [e.message for e in got.items if e.message]


def nodes_announcing(topo: topology.Topology, address: str) -> List[str]:
    """Nodes carrying `address` on the dummy interface."""
    found = []
    for node, ip in sorted(topo.node_ips.items()):
        if nodes.interface_for_address(ip, address) == DUMMY_IFACE:
            found.append(node)
    return found


def wait_every_node_announcing(
    topo: topology.Topology, address: str, timeout: float = 90.0,
    cluster: Optional[Cluster] = None,
) -> List[str]:
    """Wait for every node to carry `address`, and SAY WHAT IS MISSING.

    Eleven sites wait on this condition and the bare wait_until reports
    only "timed out ... on every node" -- not which node was short, and
    not whether its agent was even running. A 1-in-8 flake reported that
    way is unactionable after the fact: the cluster has moved on by the
    time anyone reads it.

    On timeout this names the nodes that had the address, the ones that
    did not, and (given a cluster) whether an lbnodeagent was running on
    each straggler -- which is what separates "the agent never got the
    event" from "there was no agent there to get it".
    """
    deadline = time.monotonic() + timeout
    got: List[str] = []
    last_error: Optional[BaseException] = None
    while True:
        # Exceptions are "not yet", exactly as wait_until treats them.
        # nodes_announcing SSHes every node, so a transient SSH failure
        # must not fail the test -- the wait_until this replaced retried,
        # and a helper that did not would turn a blip into a red run.
        try:
            got = nodes_announcing(topo, address)
            last_error = None
            if len(got) == len(topo.node_ips):
                return got
        except Exception as exc:  # noqa: BLE001 - deliberately broad, see above
            last_error = exc
        if time.monotonic() >= deadline:
            break
        time.sleep(3.0)

    missing = sorted(set(topo.node_ips) - set(got))
    detail = []
    for node in missing:
        state = "unknown"
        if cluster is not None:
            pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)
            state = "no agent pod" if pod is None else f"agent {pod.metadata.name} phase={pod.status.phase}"
        detail.append(f"{node} ({state})")
    raise AssertionError(
        f"after {timeout:.0f}s, {address} is on {len(got)}/{len(topo.node_ips)} nodes.\n"
        f"  announcing:     {sorted(got) or 'none'}\n"
        f"  NOT announcing: {detail}\n"
        f"Every node announces a remote address -- there is no election -- so a "
        f"node short here is either an agent that was not running or one that "
        f"missed the Service event."
        + (f"\nLast error while probing: {last_error!r}" if last_error else "")
    )


def assert_never_allocates(cluster: Cluster, name: str, seconds: int = 16) -> None:
    for _ in range(seconds // 2):
        time.sleep(2)
        ips = cluster.service_ingress_ips(NAMESPACE, name)
        assert not ips, f"{name} was allocated {ips} but should have been refused"


@pytest.fixture
def remote_group(cluster: Cluster):
    """Remote ServiceGroups, removed afterwards."""
    created: List[str] = []

    def make(name: str = REMOTE_GROUP, v4: str = POOL_V4, subnet: str = SUBNET_V4,
             v6: Optional[str] = POOL_V6, v6_subnet: str = SUBNET_V6) -> str:
        spec: Dict[str, object] = {"v4pools": [{"pool": v4, "subnet": subnet}]}
        if v6:
            spec["v6pools"] = [{"pool": v6, "subnet": v6_subnet}]
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {"remote": spec},
            }
        )
        created.append(name)
        return name

    yield make

    for name in reversed(created):
        cluster.delete_cr("servicegroup", name)


@pytest.fixture
def pinned_backend_remote(cluster: Cluster):
    """An echo backend Deployment pinned to chosen nodes, scalable to zero.

    ETP Local is about which nodes have ENDPOINTS, so these tests need to
    place and remove endpoints deliberately rather than take what the
    scheduler gives them.
    """
    created: List[str] = []

    def make(name: str, node: Optional[str], replicas: int = 1) -> str:
        labels = {"app": name}
        spec: Dict[str, object] = backend.pod_spec(node)
        cluster.apps.create_namespaced_deployment(
            NAMESPACE,
            {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {"name": name, "namespace": NAMESPACE},
                "spec": {
                    "replicas": replicas,
                    "selector": {"matchLabels": labels},
                    "template": {"metadata": {"labels": labels}, "spec": spec},
                },
            },
        )
        created.append(name)
        if replicas:
            cluster.wait_rollout(NAMESPACE, name, timeout=180)
        return name

    yield make

    for name in reversed(created):
        try:
            cluster.apps.delete_namespaced_deployment(name, NAMESPACE)
        except Exception:  # noqa: BLE001 - teardown must not mask a failure
            pass


# ------------------------------------------------------------ the basics


def test_every_node_announces_on_the_dummy_interface(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service, log_window,
):
    """No election, no winner: all nodes, on kube-lb0."""
    group = remote_group()
    vip = lb_service("remote-basic", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    assert topo.subnet_holding(vip) is None, (
        f"{vip} is inside a node subnet, so this is not exercising the remote path"
    )

    announced = wait_until(
        lambda: (lambda got: got if len(got) == len(topo.node_ips) else None)(
            nodes_announcing(topo, vip)
        ),
        timeout=90, interval=3.0,
        description=f"{vip} to appear on kube-lb0 on all {len(topo.node_ips)} nodes",
    )
    assert sorted(announced) == sorted(topo.node_ips)

    # It must NOT be on any physical interface. That is the whole
    # local/remote distinction, and it is the direction that fails
    # silently -- an address on both works, until the ARP conflicts.
    for node, ip in sorted(topo.node_ips.items()):
        assert nodes.interface_for_address(ip, vip) == DUMMY_IFACE

    value = cluster.annotation(NAMESPACE, "remote-basic", announcing.annotation_key("IPv4"))
    assert not announcing.parse(value), (
        f"a remote address claims no announcing slot; got {value!r}"
    )
    logs = cluster.component_logs("lbnodeagent", log_window)
    assert any("announcingNonLocal" in t for t in logs.values())


@pytest.mark.requires("ipv6")
def test_dual_stack_remote_allocation(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """One address per family, both on kube-lb0 everywhere."""
    group = remote_group()
    ips = lb_service(
        "remote-dual", ["IPv4", "IPv6"], annotations={SERVICE_GROUP: group}, timeout=90
    )
    assert len(ips) == 2, ips
    for address in ips:
        wait_until(
            lambda a=address: (lambda got: got if len(got) == len(topo.node_ips) else None)(
                nodes_announcing(topo, a)
            ),
            timeout=90, interval=3.0,
            description=f"{address} on kube-lb0 everywhere",
        )


def test_remote_address_flags_on_the_dummy_interface(
    topo: topology.Topology, remote_group, lb_service
):
    """A remote VIP is PERMANENT, unlike a local one.

    A local VIP gets a finite lifetime so that an address orphaned by a
    dead agent expires instead of blackholing the VIP. Remote is the
    opposite case: every node announces, no node ever wins an election,
    so there is nothing to be orphaned FROM -- and a finite lifetime
    would only create the risk of it expiring under a healthy agent.

    It also carries no noprefixroute, which is the other inversion:
    kube-lb0 exists precisely so the kernel installs a route that routing
    software can pick up and advertise. Suppressing that route is the
    whole point on a physical NIC and would defeat the purpose here.
    """
    group = remote_group()
    vip = lb_service("remote-flags", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    wait_until(lambda: nodes_announcing(topo, vip), timeout=90, interval=3.0,
               description=f"{vip} announced")
    node = sorted(topo.node_ips)[0]
    detail = nodes.address_detail(topo.node_ips[node], vip)
    assert detail is not None and detail.interface == DUMMY_IFACE
    assert detail.permanent, (
        f"{vip} has valid_lft {detail.valid_lft}s on {DUMMY_IFACE}; a remote "
        f"address is not renewed on a timer, so a finite lifetime would let "
        f"it expire under a perfectly healthy agent"
    )
    assert not detail.has_flag("noprefixroute"), (
        f"{vip} has noprefixroute on {DUMMY_IFACE} (flags: {detail.flags}); the "
        f"dummy interface exists so the kernel DOES install a route for "
        f"routing software to advertise"
    )


def test_local_and_remote_pools_do_not_contaminate_each_other(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    remote_group, lb_service,
):
    """Two services, two modes, each address only where it belongs.

    An address on the wrong interface still answers, so this fails
    silently: a local VIP on kube-lb0 is announced by every node at once,
    and a remote VIP on a physical NIC is ARPed for by one.
    """
    group = remote_group()
    remote_vip = lb_service("mixed-remote", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    local_vip = lb_service("mixed-local", ["IPv4"])[0]

    wait_until(lambda: nodes_announcing(topo, remote_vip), timeout=90, interval=3.0,
               description=f"{remote_vip} announced")
    wait_until(lambda: nodes.announcing_node(topo.node_ips, local_vip), timeout=60,
               description=f"{local_vip} announced")

    for node, ip in sorted(topo.node_ips.items()):
        assert nodes.interface_for_address(ip, remote_vip) in (None, DUMMY_IFACE), (
            f"remote {remote_vip} is on a physical interface on {node}"
        )
        iface = nodes.interface_for_address(ip, local_vip)
        assert iface != DUMMY_IFACE, (
            f"local {local_vip} is on {DUMMY_IFACE} on {node}; every node would "
            f"then answer for it"
        )


# ------------------------------------------------------------- ETP Local


def test_etp_local_restricts_announcement_to_endpoint_nodes(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service, pinned_backend_remote,
):
    """With ETP Local, only nodes holding an endpoint announce.

    This is what ETP Local buys on a remote pool and why it is honoured
    here and overridden for local pools: every node would otherwise
    announce, and traffic arriving at a node with no endpoint would have
    to be forwarded, losing the client address that ETP Local exists to
    preserve.
    """
    group = remote_group()
    target = sorted(topo.node_ips)[0]
    backend = pinned_backend_remote("etp-backend", target)

    vip = lb_service(
        "remote-etp", ["IPv4"], annotations={SERVICE_GROUP: group},
        selector={"app": backend}, externalTrafficPolicy="Local",
    )[0]

    announced = wait_until(
        lambda: (lambda got: got if got == [target] else None)(nodes_announcing(topo, vip)),
        timeout=120, interval=3.0,
        description=f"{vip} to be announced only by {target}",
    )
    assert announced == [target]

    svc = cluster.service(NAMESPACE, "remote-etp")
    assert svc.spec.external_traffic_policy == "Local", (
        "ETP Local must be preserved on a REMOTE pool, not overridden"
    )


def test_etp_local_withdraws_when_the_last_endpoint_goes(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service, pinned_backend_remote,
):
    """Zero endpoints means nowhere, then back when one returns.

    Announcing an ETP Local address from a node with no endpoint is a
    blackhole: the traffic arrives and has nothing local to answer it.
    """
    group = remote_group()
    target = sorted(topo.node_ips)[0]
    backend = pinned_backend_remote("etp-zero-backend", target)
    vip = lb_service(
        "remote-etp-zero", ["IPv4"], annotations={SERVICE_GROUP: group},
        selector={"app": backend}, externalTrafficPolicy="Local",
    )[0]
    wait_until(lambda: nodes_announcing(topo, vip) == [target] or None,
               timeout=120, interval=3.0, description=f"{vip} announced by {target}")

    cluster.apps.patch_namespaced_deployment_scale(
        backend, NAMESPACE, {"spec": {"replicas": 0}}
    )
    wait_while(
        lambda: bool(nodes_announcing(topo, vip)),
        timeout=120, interval=3.0,
        description=f"{vip} to be withdrawn everywhere once it has no endpoints",
    )

    cluster.apps.patch_namespaced_deployment_scale(
        backend, NAMESPACE, {"spec": {"replicas": 1}}
    )
    cluster.wait_rollout(NAMESPACE, backend, timeout=180)
    back = wait_until(
        lambda: nodes_announcing(topo, vip) or None,
        timeout=120, interval=3.0,
        description=f"{vip} to return once an endpoint exists",
    )
    assert back == [target], f"{vip} came back on {back}, expected only {target}"


@pytest.mark.requires("multi-node")
def test_etp_local_follows_the_endpoint_to_another_node(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service, pinned_backend_remote,
):
    """Move the endpoint; the announcement moves with it.

    The migration is the interesting part: for a moment both nodes could
    plausibly announce, and settling on the wrong one leaves the address
    on a node with no endpoint.
    """
    group = remote_group()
    first, second = sorted(topo.node_ips)[0], sorted(topo.node_ips)[1]
    backend = pinned_backend_remote("etp-move-backend", first)
    vip = lb_service(
        "remote-etp-move", ["IPv4"], annotations={SERVICE_GROUP: group},
        selector={"app": backend}, externalTrafficPolicy="Local",
    )[0]
    wait_until(lambda: nodes_announcing(topo, vip) == [first] or None,
               timeout=120, interval=3.0, description=f"{vip} announced by {first}")

    cluster.apps.patch_namespaced_deployment(
        backend, NAMESPACE,
        {"spec": {"template": {"spec": {"nodeName": second}}}},
    )
    cluster.wait_rollout(NAMESPACE, backend, timeout=180)

    moved = wait_until(
        lambda: (lambda got: got if got == [second] else None)(nodes_announcing(topo, vip)),
        timeout=180, interval=3.0,
        description=f"{vip} to follow its endpoint to {second}",
    )
    assert moved == [second]


def test_switching_a_service_to_etp_local_narrows_the_announcement(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service, pinned_backend_remote,
):
    """Cluster -> Local on a live Service, without losing the address."""
    group = remote_group()
    target = sorted(topo.node_ips)[0]
    backend = pinned_backend_remote("etp-transition-backend", target)
    vip = lb_service(
        "remote-etp-transition", ["IPv4"], annotations={SERVICE_GROUP: group},
        selector={"app": backend},
    )[0]
    wait_until(
        lambda: (lambda got: got if len(got) == len(topo.node_ips) else None)(
            nodes_announcing(topo, vip)
        ),
        timeout=120, interval=3.0, description=f"{vip} on every node under ETP Cluster",
    )

    cluster.core.patch_namespaced_service(
        "remote-etp-transition", NAMESPACE,
        {"spec": {"externalTrafficPolicy": "Local"}},
    )
    narrowed = wait_until(
        lambda: (lambda got: got if got == [target] else None)(nodes_announcing(topo, vip)),
        timeout=180, interval=3.0,
        description=f"{vip} to narrow to {target} after switching to ETP Local",
    )
    assert narrowed == [target]
    assert cluster.service_ingress_ips(NAMESPACE, "remote-etp-transition") == [vip], (
        "the address changed while only the traffic policy was edited"
    )


# ----------------------------------------------------------- rejections


def test_requesting_an_address_outside_the_pool_is_refused(
    cluster: Cluster, remote_group, lb_service, allocator_metrics
):
    """A typo in purelb.io/addresses must not silently allocate something else."""
    group = remote_group()
    before = allocator_metrics()
    lb_service(
        "remote-out-of-pool", ["IPv4"],
        annotations={SERVICE_GROUP: group, ADDRESSES: "10.255.99.99"}, wait=False,
    )
    assert_never_allocates(cluster, "remote-out-of-pool")
    assert any("10.255.99.99" in m or "not in" in m or "outside" in m
               for m in events_for(cluster, "remote-out-of-pool")), (
        f"no event explaining the refusal: {events_for(cluster, 'remote-out-of-pool')}"
    )


def test_requesting_an_address_already_in_use_is_refused(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """Two services on one address, without a sharing key, is a collision."""
    group = remote_group()
    taken = lb_service("remote-holder", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    lb_service(
        "remote-thief", ["IPv4"],
        annotations={SERVICE_GROUP: group, ADDRESSES: taken}, wait=False,
    )
    assert_never_allocates(cluster, "remote-thief")
    assert cluster.service_ingress_ips(NAMESPACE, "remote-holder") == [taken], (
        "the original holder lost its address to the second request"
    )


def test_pool_exhaustion_is_reported(
    cluster: Cluster, remote_group, lb_service, allocator_metrics
):
    """One more service than the pool has addresses.

    The pool is a range of exactly two, so the third request cannot be
    satisfied and must say so rather than waiting silently forever.
    """
    group = remote_group("remote-small", v4=SMALL_V4, subnet=SMALL_SUBNET, v6=None)
    got = []
    for i in range(2):
        got.extend(lb_service(f"remote-fill-{i}", ["IPv4"],
                              annotations={SERVICE_GROUP: group}))
    assert len(set(got)) == 2, f"expected two distinct addresses, got {got}"

    before = allocator_metrics()
    lb_service("remote-overflow", ["IPv4"], annotations={SERVICE_GROUP: group}, wait=False)
    assert_never_allocates(cluster, "remote-overflow")
    messages = events_for(cluster, "remote-overflow")
    assert any("no addresses" in m.lower() or "exhaust" in m.lower() or "full" in m.lower()
               for m in messages), f"no event explaining exhaustion: {messages}"


# ------------------------------------------------------ lifecycle edits


def test_sharing_key_added_to_a_live_service(
    cluster: Cluster, remote_group, lb_service
):
    """Sharing is decided at ALLOCATION time, not retroactively.

    The intuitive expectation -- annotate a running Service and watch it
    move onto the shared address -- is wrong, and quietly so: the
    annotation is accepted and nothing happens, because a Service that
    already holds an address keeps it. Adopting a shared address requires
    the holder to be re-allocated, which means delete and recreate.

    Worth pinning precisely because the wrong expectation is the natural
    one; my first version asserted the live migration and timed out.
    """
    group = remote_group()
    first = lb_service("remote-share-a", ["IPv4"],
                       annotations={SERVICE_GROUP: group})[0]
    second = lb_service("remote-share-b", ["IPv4"],
                        annotations={SERVICE_GROUP: group, SHARING: "late-sharing"},
                        ports=[{"port": 8080, "targetPort": backend.PORT}])[0]
    assert second != first, f"both services got {first} before either asked to share"

    # Annotate the holder, then re-create the other one.
    cluster.core.patch_namespaced_service(
        "remote-share-a", NAMESPACE,
        {"metadata": {"annotations": {SHARING: "late-sharing"}}},
    )
    assert cluster.service_ingress_ips(NAMESPACE, "remote-share-a") == [first], (
        "annotating a live Service changed its address; sharing is supposed to "
        "be decided at allocation time"
    )

    cluster.delete_service(NAMESPACE, "remote-share-b")
    cluster.wait_service_gone(NAMESPACE, "remote-share-b")
    again = lb_service("remote-share-b", ["IPv4"],
                       annotations={SERVICE_GROUP: group, SHARING: "late-sharing"},
                       ports=[{"port": 8080, "targetPort": backend.PORT}])[0]
    assert again == first, (
        f"recreated with a matching sharing key, {again} should have adopted "
        f"{first}"
    )


@pytest.mark.requires("ipv6")
def test_single_stack_service_upgraded_to_dual_stack(
    cluster: Cluster, remote_group, lb_service
):
    """Adding IPv6 to a live Service allocates the second family.

    The IPv4 address must survive: renumbering a running service because
    IPv6 was switched on would be an outage.
    """
    group = remote_group()
    v4 = lb_service("remote-upgrade", ["IPv4"], annotations={SERVICE_GROUP: group})[0]

    cluster.core.patch_namespaced_service(
        "remote-upgrade", NAMESPACE,
        {"spec": {"ipFamilyPolicy": "RequireDualStack", "ipFamilies": ["IPv4", "IPv6"]}},
    )
    both = wait_until(
        lambda: (lambda got: got if len(got) == 2 else None)(
            cluster.service_ingress_ips(NAMESPACE, "remote-upgrade")
        ),
        timeout=120, interval=3.0,
        description="the second family to be allocated",
    )
    assert v4 in both, f"the IPv4 address changed from {v4} to {both}"


def test_re_evaluate_on_a_remote_service(cluster: Cluster, remote_group, lb_service):
    """The trigger is consumed and the address survives."""
    group = remote_group()
    before = lb_service("remote-reeval", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    cluster.core.patch_namespaced_service(
        "remote-reeval", NAMESPACE,
        {"metadata": {"annotations": {RE_EVALUATE: "true"}}},
    )
    wait_until(
        lambda: cluster.annotation(NAMESPACE, "remote-reeval", RE_EVALUATE) in (None, "")
        or None,
        timeout=60, interval=2.0, description="the re-evaluate annotation to be consumed",
    )
    assert cluster.service_ingress_ips(NAMESPACE, "remote-reeval") == [before]


def test_deleting_a_remote_service_withdraws_it_from_every_node(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """Withdrawal has to reach every node, not just one.

    A local address is withdrawn from the single node that held it; a
    remote address was added everywhere, so a partial withdrawal leaves
    it stranded on the nodes that were missed.
    """
    group = remote_group()
    vip = lb_service("remote-delete", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    wait_every_node_announcing(topo, vip, timeout=90, cluster=cluster)
    cluster.delete_service(NAMESPACE, "remote-delete")
    wait_while(
        lambda: bool(nodes_announcing(topo, vip)),
        timeout=120, interval=3.0,
        description=f"{vip} to be withdrawn from every node",
    )


# ------------------------------------------------------------ resilience


@pytest.mark.requires("multi-node")
def test_a_remote_address_survives_losing_one_node(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service, tainted_nodes,
):
    """Every node announces, so losing one changes nothing for the rest.

    The local suite's failover test asks whether the address MOVES. Here
    the interesting question is the opposite: it must not move, and the
    surviving nodes must keep announcing it.
    """
    group = remote_group()
    vip = lb_service("remote-survive", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    wait_every_node_announcing(topo, vip, timeout=90, cluster=cluster)

    victim = sorted(topo.node_ips)[-1]
    tainted_nodes(victim)
    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", victim)
    if pod is not None:
        cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)

    survivors = [n for n in topo.node_ips if n != victim]
    wait_until(
        lambda: (lambda got: got if all(n in got for n in survivors) else None)(
            nodes_announcing(topo, vip)
        ),
        timeout=120, interval=3.0,
        description=f"{vip} to remain on every surviving node",
    )


def test_a_restarted_agent_re_announces(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """An agent that restarts must put the address back.

    Remote announcements are not renewed on a timer, so if a restarting
    agent failed to re-add the address nothing else would, and that node
    would drop out of the ECMP set silently.
    """
    group = remote_group()
    vip = lb_service("remote-restart", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    wait_every_node_announcing(topo, vip, timeout=90, cluster=cluster)

    node = sorted(topo.node_ips)[0]
    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)
    assert pod is not None
    cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)

    wait_until(
        lambda: cluster.daemonset_ready(
            cluster.purelb_namespace, "lbnodeagent", expect_nodes=len(topo.node_ips)
        ) or None,
        timeout=180, interval=3.0, description="the DaemonSet to be whole again",
    )
    wait_until(
        lambda: (lambda got: got if node in got else None)(nodes_announcing(topo, vip)),
        timeout=120, interval=3.0,
        description=f"{node} to re-announce {vip} after its agent restarted",
    )


# ------------------------------------------------------------ multi-pool


@pytest.mark.requires("multi-subnet")
def test_remote_multi_pool_allocates_from_every_range(
    cluster: Cluster, topo: topology.Topology, lb_service
):
    """multiPool works for remote groups too, and is not subnet-gated.

    A remote pool has no node on its subnet by construction, so if
    multi-pool filtered ranges by active nodes the way local mode does, a
    remote group would produce nothing at all.

    Both families, because they are balanced independently: an
    implementation that handled v4 ranges and ignored v6 ones would give
    a dual-stack Service one v4 per range and a single v6, which looks
    almost right.
    """
    name = "remote-multipool"
    spec = {
        "multiPool": True,
        "v4pools": [
            {"pool": "10.255.2.1-10.255.2.10", "subnet": "10.255.2.0/24"},
            {"pool": "10.255.3.1-10.255.3.10", "subnet": "10.255.3.0/24"},
        ],
    }
    families = ["IPv4"]
    if topo.has_ipv6:
        spec["v6pools"] = [
            {"pool": "fd00:10:2::1-fd00:10:2::10", "subnet": "fd00:10:2::/64"},
            {"pool": "fd00:10:3::1-fd00:10:3::10", "subnet": "fd00:10:3::/64"},
        ]
        families.append("IPv6")
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": name, "namespace": cluster.purelb_namespace},
            "spec": {"remote": spec},
        }
    )
    try:
        ips = lb_service(
            "remote-mp", families, annotations={SERVICE_GROUP: name}, timeout=120
        )
        assert len(ips) == 2 * len(families), (
            f"expected one address per range per family, got {ips}"
        )
        v4 = {str(ipaddress.ip_network(f"{a}/24", strict=False))
              for a in ips if ipaddress.ip_address(a).version == 4}
        assert v4 == {"10.255.2.0/24", "10.255.3.0/24"}, v4
        if "IPv6" in families:
            v6 = {str(ipaddress.ip_network(f"{a}/64", strict=False))
                  for a in ips if ipaddress.ip_address(a).version == 6}
            assert v6 == {"fd00:10:2::/64", "fd00:10:3::/64"}, (
                f"IPv6 ranges used: {v6}; multi-pool must cover v6 ranges too"
            )
    finally:
        cluster.delete_service(NAMESPACE, "remote-mp")
        cluster.delete_cr("servicegroup", name)


# ---------------------------------------------------------- aggregation
#
# `aggregation` sets the MASK the address is configured with on kube-lb0,
# and that mask is what the kernel turns into a route -- so it decides
# what a router learns. "32" advertises a host route for one address;
# "default" uses the pool's subnet mask and advertises the whole range.
# Getting it wrong does not break the VIP under test: it silently claims
# (or fails to claim) addresses NOBODY is testing, which is why this
# needs asserting rather than observing.


def prefix_len_on(topo: topology.Topology, node: str, address: str) -> Optional[int]:
    detail = nodes.address_detail(topo.node_ips[node], address)
    return None if detail is None else ipaddress.ip_interface(detail.cidr).network.prefixlen


@pytest.mark.parametrize(
    "family,aggregation,pool,subnet,expected",
    [
        ("IPv4", "/32", "10.255.4.1-10.255.4.10", "10.255.4.0/24", 32),
        ("IPv4", "default", "10.255.5.1-10.255.5.10", "10.255.5.0/24", 24),
        ("IPv6", "/128", "fd00:10:255::100-fd00:10:255::110", "fd00:10:255::/64", 128),
        ("IPv6", "default", "fd00:10:256::100-fd00:10:256::110", "fd00:10:256::/64", 64),
    ],
    ids=["v4-host-route", "v4-default-subnet", "v6-host-route", "v6-default-subnet"],
)
def test_aggregation_sets_the_configured_prefix_on_every_node(
    cluster: Cluster, topo: topology.Topology, lb_service,
    family: str, aggregation: str, pool: str, subnet: str, expected: int,
):
    """The mask is what was asked for, identically on every node.

    Note the LEADING SLASH in the aggregation values. addVirtualInt
    builds the mask with net.ParseCIDR("0.0.0.0" + aggregation), so "/32"
    is required and "32" produces "0.0.0.032", which does not parse. The
    field's own doc comment says "an integer in the range 8-128", the CRD
    validates nothing, and the announcement then fails at runtime -- see
    test_a_malformed_aggregation_is_not_reported_as_announced below.

    "On every node" is half the assertion: remote addresses are added by
    each agent independently, so one node disagreeing about the prefix
    advertises a different route from its peers and the resulting
    blackhole depends on which path a packet takes.
    """
    if family == "IPv6" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6")

    # The aggregation value contains a slash, which is not legal in a
    # Kubernetes object name.
    name = f"remote-agg-{aggregation.strip('/') or 'default'}-{family.lower()}"
    key = "v4pools" if family == "IPv4" else "v6pools"
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": name, "namespace": cluster.purelb_namespace},
            "spec": {
                "remote": {key: [{"pool": pool, "subnet": subnet, "aggregation": aggregation}]}
            },
        }
    )
    try:
        vip = lb_service(name, [family], annotations={SERVICE_GROUP: name}, timeout=90)[0]
        wait_until(
            lambda: (lambda got: got if len(got) == len(topo.node_ips) else None)(
                nodes_announcing(topo, vip)
            ),
            timeout=90, interval=3.0, description=f"{vip} on every node",
        )
        seen = {n: prefix_len_on(topo, n, vip) for n in sorted(topo.node_ips)}
        assert set(seen.values()) == {expected}, (
            f"aggregation {aggregation!r} should configure /{expected}; nodes "
            f"report {seen}. A node disagreeing here advertises a different "
            f"route from its peers."
        )
    finally:
        cluster.delete_service(NAMESPACE, name)
        cluster.delete_cr("servicegroup", name)


# --------------------------------------------------- remaining coverage


@pytest.mark.requires("ipv6")
def test_ipv6_only_remote_allocation(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """IPv6 on its own, not only as half of a dual-stack Service.

    A dual-stack test passes if IPv4 works and IPv6 merely tags along, so
    the single-stack case is the one that isolates the v6 path.
    """
    group = remote_group()
    vip = lb_service("remote-v6", ["IPv6"], annotations={SERVICE_GROUP: group})[0]
    assert ipaddress.ip_address(vip).version == 6, vip
    announced = wait_until(
        lambda: (lambda got: got if len(got) == len(topo.node_ips) else None)(
            nodes_announcing(topo, vip)
        ),
        timeout=90, interval=3.0, description=f"{vip} on kube-lb0 everywhere",
    )
    assert sorted(announced) == sorted(topo.node_ips)


def test_remote_addresses_can_be_shared(
    cluster: Cluster, remote_group, lb_service
):
    """Two services, one remote address, different ports."""
    group = remote_group()
    first = lb_service("remote-shared-a", ["IPv4"],
                       annotations={SERVICE_GROUP: group, SHARING: "remote-web"})[0]
    second = lb_service("remote-shared-b", ["IPv4"],
                        annotations={SERVICE_GROUP: group, SHARING: "remote-web"},
                        ports=[{"port": 443, "targetPort": backend.PORT}])[0]
    assert first == second, f"sharing key did not share: {first} vs {second}"


def test_a_specific_remote_address_can_be_requested(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """purelb.io/addresses gets exactly that address, and it announces.

    The rejection cases above are only meaningful if the accept case
    works: a build that refused every explicit request would pass them
    all.
    """
    group = remote_group()
    wanted = "10.255.1.105"
    ips = lb_service(
        "remote-specific", ["IPv4"],
        annotations={SERVICE_GROUP: group, ADDRESSES: wanted},
    )
    assert ips == [wanted], f"asked for {wanted}, got {ips}"
    wait_until(
        lambda: (lambda got: got if len(got) == len(topo.node_ips) else None)(
            nodes_announcing(topo, wanted)
        ),
        timeout=90, interval=3.0, description=f"{wanted} on every node",
    )


def test_an_agent_restart_during_etp_local_restores_the_narrow_set(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service, pinned_backend_remote,
):
    """Restarting an agent must not widen an ETP Local announcement.

    The failure worth catching is the restarting node re-announcing an
    address it has no endpoint for: the address comes back everywhere,
    ETP Local silently stops meaning anything, and traffic starts landing
    on nodes that must forward it.
    """
    group = remote_group()
    target = sorted(topo.node_ips)[0]
    other = sorted(topo.node_ips)[-1]
    backend = pinned_backend_remote("etp-restart-backend", target)
    vip = lb_service(
        "remote-etp-restart", ["IPv4"], annotations={SERVICE_GROUP: group},
        selector={"app": backend}, externalTrafficPolicy="Local",
    )[0]
    wait_until(lambda: nodes_announcing(topo, vip) == [target] or None,
               timeout=120, interval=3.0, description=f"{vip} announced only by {target}")

    # Restart the agent on a node that has NO endpoint.
    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", other)
    assert pod is not None
    cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=10)
    wait_until(
        lambda: cluster.daemonset_ready(
            cluster.purelb_namespace, "lbnodeagent", expect_nodes=len(topo.node_ips)
        ) or None,
        timeout=180, interval=3.0, description="the DaemonSet to be whole again",
    )

    # Hold it: the restarted node must NOT pick the address up.
    for _ in range(6):
        time.sleep(3)
        announcing_now = nodes_announcing(topo, vip)
        assert announcing_now == [target], (
            f"after restarting the agent on {other} (which has no endpoint), "
            f"{vip} is announced by {announcing_now}; ETP Local has stopped "
            f"restricting the announcement"
        )


def test_remote_service_picks_up_a_range_added_later(
    cluster: Cluster, lb_service
):
    """Incremental multi-pool for remote groups."""
    name = "remote-incremental"

    def apply(pools) -> None:
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {"remote": {"multiPool": True, "v4pools": pools}},
            }
        )

    first = {"pool": "10.255.6.1-10.255.6.10", "subnet": "10.255.6.0/24"}
    second = {"pool": "10.255.7.1-10.255.7.10", "subnet": "10.255.7.0/24"}
    apply([first])
    try:
        ips = lb_service("remote-inc", ["IPv4"], annotations={SERVICE_GROUP: name})
        assert len(ips) == 1, f"one range configured, expected one address, got {ips}"
        apply([first, second])
        grown = wait_until(
            lambda: (lambda got: got if len(got) > 1 else None)(
                cluster.service_ingress_ips(NAMESPACE, "remote-inc")
            ),
            timeout=120, interval=3.0,
            description="the live Service to pick up an address from the new range",
        )
        assert len(grown) == 2, grown
    finally:
        cluster.delete_service(NAMESPACE, "remote-inc")
        cluster.delete_cr("servicegroup", name)


def test_a_malformed_aggregation_is_rejected_at_admission(
    cluster: Cluster, topo: topology.Topology
):
    """The API server refuses an aggregation the announcer cannot parse.

    This is the first half of a two-part fix for a bug this port found.
    addVirtualInt builds the mask with net.ParseCIDR("0.0.0.0" +
    aggregation), so the value needs its leading slash -- but the field's
    doc comment said "an integer in the range 8-128" and nothing
    validated it. Following the documentation produced a ServiceGroup the
    API server stored happily, whose addresses were then never announced,
    while the Service reported that they were.

    The second half is that an announcement which fails is no longer
    evented as a success; that ordering is asserted by
    TestAggregationRequiresALeadingSlash in internal/local plus the
    AnnouncementFailed event the announcer now emits.
    """
    from kubernetes.client.rest import ApiException

    body = {
        "apiVersion": "purelb.io/v2",
        "kind": "ServiceGroup",
        "metadata": {"name": "remote-bad-aggregation", "namespace": cluster.purelb_namespace},
        "spec": {
            "remote": {
                "v4pools": [
                    {"pool": "10.255.8.1-10.255.8.10", "subnet": "10.255.8.0/24",
                     "aggregation": "32"}
                ]
            }
        },
    }
    with pytest.raises(ApiException) as caught:
        cluster.apply_cr(body)
    assert caught.value.status in (400, 422), caught.value.status
    assert "aggregation" in str(caught.value.body), caught.value.body

    # The documented form is accepted, so the rule rejects the mistake
    # rather than the feature.
    body["spec"]["remote"]["v4pools"][0]["aggregation"] = "/32"
    try:
        cluster.apply_cr(body)
    finally:
        cluster.delete_cr("servicegroup", "remote-bad-aggregation")


def test_a_remote_vip_serves_traffic_from_a_pod(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """Reachability for a remote VIP is tested from a POD, not a node.

    A remote address is routed, and this cluster has no route to
    10.255.1.0/24 -- that is the point of the pool being outside every
    node subnet. So a node cannot reach it and a node-sourced curl proves
    nothing. From inside the pod network kube-proxy DNATs the VIP to an
    endpoint, which exercises the part PureLB is responsible for: the
    address exists, the Service is programmed, and traffic to it lands on
    a backend.

    Without this the module asserts placement everywhere and function
    nowhere.
    """
    group = remote_group()
    vip = lb_service("remote-reach", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    wait_every_node_announcing(topo, vip, timeout=90, cluster=cluster)

    name = "remote-vip-client"
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
                        "command": ["sh", "-c",
                                    f"for i in 1 2 3 4 5; do "
                                    f"curl -s --max-time 10 'http://{vip}/?format=json' && break; "
                                    f"sleep 3; done"],
                    }
                ],
            },
        },
    )
    try:
        wait_until(
            lambda: cluster.core.read_namespaced_pod(name, NAMESPACE).status.phase
            in ("Succeeded", "Failed") or None,
            timeout=180, interval=3.0, description="the client pod to finish",
        )
        body = cluster.pod_log_text(NAMESPACE, name)
        assert json.loads(body)["pod"], (
            f"a pod could not reach remote VIP {vip}; the address is on kube-lb0 "
            f"on every node, so this is the Service programming rather than the "
            f"announcement. Got {body[:200]!r}"
        )
    finally:
        cluster.core.delete_namespaced_pod(name, NAMESPACE, grace_period_seconds=0)


@pytest.mark.requires("ipv6")
def test_a_remote_ipv6_vip_serves_traffic_from_a_pod(
    cluster: Cluster, topo: topology.Topology, remote_group, lb_service
):
    """Same for IPv6, which fails independently.

    Forwarding and Service programming are per-family, so an IPv6 remote
    VIP can be placed perfectly on every node and still carry nothing.
    """
    group = remote_group()
    vip = lb_service("remote-reach-v6", ["IPv6"], annotations={SERVICE_GROUP: group})[0]
    wait_every_node_announcing(topo, vip, timeout=90, cluster=cluster)

    name = "remote-vip-client-v6"
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
                        "command": ["sh", "-c",
                                    f"for i in 1 2 3 4 5; do "
                                    f"curl -s --max-time 10 'http://[{vip}]/?format=json' && break; "
                                    f"sleep 3; done"],
                    }
                ],
            },
        },
    )
    try:
        wait_until(
            lambda: cluster.core.read_namespaced_pod(name, NAMESPACE).status.phase
            in ("Succeeded", "Failed") or None,
            timeout=180, interval=3.0, description="the client pod to finish",
        )
        body = cluster.pod_log_text(NAMESPACE, name)
        assert json.loads(body)["pod"], (
            f"a pod could not reach remote IPv6 VIP {vip}; got {body[:200]!r}"
        )
    finally:
        cluster.core.delete_namespaced_pod(name, NAMESPACE, grace_period_seconds=0)
