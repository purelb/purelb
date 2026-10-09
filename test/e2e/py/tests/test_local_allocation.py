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

from purelb_e2e import TEST_NAMESPACE, backend, guard, metrics, nodes, topology
from purelb_e2e.cluster import Cluster, utcnow
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


def guard_enforcing() -> bool:
    """The session runs with --address-guard in enforce mode."""
    return guard.enabled() and (guard.session_spec() or {}).get("mode") == "enforce"


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
    router,
    family: str,
):
    """One address per family, on a real interface, serving traffic."""
    if family != "v4" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6 on the node subnets")

    name = f"echo-lb-{family}"
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

        served = nodes.echo_json(topo.node_ips, address)
        assert served["pod"], f"{address} did not serve the backend; got {served!r}"

        # Under --address-guard (enforce) the same flow must still serve
        # from off the cluster, and only the Service port may answer.
        if guard_enforcing() and router is not None:
            assert router.http_status(address)[0] == 200, f"{address}:80 did not serve through the guard"
            # Counted, so "closed" can't be a port nothing listens on.
            before_drop = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=address, action="drop")
            assert not router.tcp_open(address, 22), f"{address}:22 answered with the guard enforcing"
            after_drop = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=address, action="drop")
            assert_guard_dropped(before_drop, after_drop, f"{address}:22", node)

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
    allocator_metrics, agent_metrics, log_window,
):
    """Deleting the Service frees the address and takes it off the node."""
    before = allocator_metrics()
    ips = lb_service("echo-lb-cleanup", ["IPv4"])
    vip = ips[0]
    holder, _ = wait_until(lambda: announced_on(topo, vip), timeout=45,
                           description=f"{vip} announced")
    agent_before = agent_metrics(holder)

    in_use = allocator_metrics().counter("purelb_address_pool_addresses_in_use", pool="default")
    cluster.delete_service(NAMESPACE, "echo-lb-cleanup")

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

    # The agent must record the withdrawal, not merely stop announcing.
    # An address that disappears without a withdrawal is the shape of a
    # crash, and it looks identical from the outside.
    holder_metrics = agent_metrics(holder)
    metrics.assert_increased(
        agent_before, holder_metrics, "purelb_lbnodeagent_address_withdrawals_total"
    )
    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", holder)
    logs = cluster.pod_logs(cluster.purelb_namespace, pod.metadata.name, log_window,
                            container="lbnodeagent")
    assert "withdrawAddress" in logs, (
        f"{holder} dropped {vip} without logging withdrawAddress"
    )


def test_specific_ip_request_is_honoured(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """spec.loadBalancerIP gets exactly that address, and it announces."""
    subnet = topo.subnets[0]
    wanted = f"{subnet.v4_octets}.210"
    ips = lb_service("echo-lb-specific", ["IPv4"], loadBalancerIP=wanted)
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
        "echo-foreign-lbclass", ["IPv4"], wait=False, loadBalancerClass="other.io/foreign-lb"
    )
    with pytest.raises(AssertionError):
        wait_until(
            lambda: cluster.service_ingress_ips(NAMESPACE, "echo-foreign-lbclass") or None,
            timeout=15,
            description="an address that should never arrive",
        )
    svc = cluster.service(NAMESPACE, "echo-foreign-lbclass")
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
        "echo-purelb-lbclass", ["IPv4"], loadBalancerClass="purelb.io/purelbv2"
    )
    address = ips[0]
    subnet = topo.subnet_holding(address)
    assert subnet is not None, f"{address} is in no node subnet"

    svc = cluster.service(NAMESPACE, "echo-purelb-lbclass")
    assert (svc.metadata.annotations or {}).get(ALLOCATED_BY) == "PureLB"

    wait_until(lambda: announced_on(topo, address), timeout=45,
               description=f"{address} to be announced")
    assert nodes.echo_json(topo.node_ips, address)["pod"]


def test_shared_ip_puts_two_services_on_one_address(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    router, agent_metrics,
):
    """Two Services with the same sharing key share one VIP.

    They must also use different ports: sharing is only legal when the
    services do not collide, and PureLB is what enforces that.
    """
    first = lb_service("echo-shared-http", ["IPv4"], annotations={SHARING: "webservers"})
    second = lb_service("echo-shared-https", ["IPv4"], annotations={SHARING: "webservers"},
                        ports=[{"port": 443, "targetPort": backend.PORT}])
    assert first == second, f"sharing key did not share: {first} vs {second}"

    vip = first[0]
    found = wait_until(lambda: announced_on(topo, vip), timeout=45,
                       description=f"{vip} to be announced")
    assert found is not None

    # Sharing is legal only while the services do not COLLIDE, and PureLB
    # is what enforces that. This has to run while echo-shared-http still
    # exists: my first version checked it after the delete, when port 80
    # was genuinely free, so the allocation correctly succeeded and the
    # test failed for the one reason that was not a bug.
    lb_service("echo-shared-conflict", ["IPv4"],
               annotations={SHARING: "webservers"}, wait=False)
    import time
    for _ in range(8):
        time.sleep(2)
        assert not cluster.service_ingress_ips(NAMESPACE, "echo-shared-conflict"), (
            "a service sharing a key AND port 80 with echo-shared-http was "
            "allocated anyway; both would answer on the same address and port"
        )
    conflict = [
        e.message for e in cluster.core.list_namespaced_event(
            NAMESPACE, field_selector="involvedObject.name=echo-shared-conflict"
        ).items if e.message
    ]
    assert any("already in use" in m for m in conflict), (
        f"no event explaining the port conflict: {conflict}"
    )
    # Done with it. Left pending, it would be allocated the shared address
    # -- port 80 included -- the moment echo-shared-http goes below.
    cluster.delete_service(NAMESPACE, "echo-shared-conflict")
    cluster.wait_service_gone(NAMESPACE, "echo-shared-conflict")

    # Deleting one holder must NOT withdraw the address: the other still
    # has it. Getting this wrong is a withdrawal-refcount bug, and it
    # takes down a live service.
    # The guard allows the union of the sharers' ports on the one address.
    guarded = guard_enforcing() and router is not None
    if guarded:
        assert router.http_status(vip, port=80)[0] == 200, f"{vip}:80 (echo-shared-http) blocked"
        assert router.http_status(vip, port=443)[0] == 200, f"{vip}:443 (echo-shared-https) blocked"

    cluster.delete_service(NAMESPACE, "echo-shared-http")
    cluster.wait_service_gone(NAMESPACE, "echo-shared-http")
    still = wait_until(lambda: announced_on(topo, vip), timeout=20,
                       description=f"{vip} to remain announced for the surviving service")
    assert still is not None, f"{vip} was withdrawn while echo-shared-https still holds it"

    if guarded:
        # Port 80 left with its Service: the guard now drops it (counted),
        # rather than kube-proxy refusing it, while 443 still serves.
        holder = still[0]
        before = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        assert not router.tcp_open(vip, 80), f"{vip}:80 still answers after its Service went"
        after = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        assert_guard_dropped(before, after, f"{vip}:80", holder)
        assert router.http_status(vip, port=443)[0] == 200

    # A DIFFERENT key must not share. Sharing every service that asked to
    # share anything would be a far worse bug than not sharing at all.
    other = lb_service("echo-shared-other", ["IPv4"],
                       annotations={SHARING: "other-key"})
    assert other != first, (
        f"services with different sharing keys both got {other}"
    )



def test_multiple_services_get_distinct_addresses(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """No two Services may be handed the same address.

    Without a sharing key, a duplicate is a double-allocation: two
    services with the same VIP and no port arbitration.
    """
    allocated = []
    for i in range(3):
        allocated.extend(lb_service(f"echo-lb-multi-{i}", ["IPv4"]))
    assert len(set(allocated)) == len(allocated), f"duplicate addresses: {allocated}"


def test_no_duplicate_vips_across_nodes(topo: topology.Topology, default_servicegroup: str, lb_service):
    """Exactly one node announces each address.

    Two nodes holding the same VIP is a split brain: ARP resolves to
    whichever replied last and traffic flaps between them.
    """
    ips = lb_service("echo-lb-unique", ["IPv4"])
    vip = ips[0]
    wait_until(lambda: announced_on(topo, vip), timeout=45, description=f"{vip} announced")
    holders = nodes.nodes_with_address(topo.node_ips, vip)
    assert len(holders) == 1, f"{vip} is announced by {holders}, expected exactly one node"


# ---------------------------------------------------------------- address guard
#
# The address guard lets only a VIP's Service ports through. Every probe
# comes from the router: a node reaching a VIP takes a local path that never
# crosses the uplink the guard is attached to. Each test enables the guard
# itself (the address_guard fixture), so these run with or without
# --address-guard, which instead runs the whole suite with it on.

GUARD_PACKETS = "purelb_address_guard_packets_total"
GUARD_VIP_PACKETS = "purelb_address_guard_vip_packets_total"
FAMILY_LABEL = {4: "ipv4", 6: "ipv6"}


def guard_drops(agent_metrics, topo: topology.Topology, name: str, **labels: str) -> Dict[str, float]:
    """A guard counter on every node. Every VIP is guarded on every node,
    and the network decides which one a packet reaches (ECMP for remote
    VIPs, a router's ARP entry for local ones), so "the guard dropped it"
    is a question about all of them."""
    return {n: agent_metrics(n).counter(name, **labels) for n in sorted(topo.node_ips)}


def assert_guard_dropped(before: Dict[str, float], after: Dict[str, float], what: str,
                         holder: str, min_delta: float = 1.0) -> None:
    deltas = {n: after[n] - before[n] for n in after}
    assert sum(deltas.values()) >= min_delta, (
        f"the guard did not drop {what} on any node (holder {holder}); per-node deltas {deltas}"
    )


def guarded_holders(topo: topology.Topology, lb_service, name: str, families, **kw) -> Dict[str, str]:
    """Create a local Service and return {vip: announcing node}."""
    holders: Dict[str, str] = {}
    for address in lb_service(name, families, **kw):
        found = wait_until(lambda a=address: announced_on(topo, a), timeout=45,
                           description=f"{address} to be announced")
        holders[address] = found[0]
    return holders


def uplinks(host: str) -> set:
    """The links the guard's automatic rule picks on this cluster's nodes:
    physical NICs (no link kind), never loopback. Every node's default
    route is on one of them here, so that part of the rule adds nothing."""
    import json as _json
    links = _json.loads(nodes.ssh(host, "ip -d -j link show"))
    return {
        ln["ifname"] for ln in links
        if "linkinfo" not in ln and ln.get("link_type") != "loopback"
    }


def router_v6(router) -> str:
    """A global IPv6 address of the router, for node-egress checks."""
    out = nodes.ssh(router.host, "ip -6 -o addr show scope global")
    for line in out.splitlines():
        addr = line.split()[3]
        if not addr.endswith("/128"):
            return addr.split("/")[0]
    raise AssertionError(f"router {router.host} has no global IPv6 address")


@pytest.mark.requires("router")
@pytest.mark.parametrize("family", ["v4", "v6"])
def test_address_guard_filters_host_ports(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    address_guard, router, agent_metrics, log_window, family: str,
):
    """Only the Service port reaches a VIP; host ports stay open on node IPs.

    Without the guard, sshd and the kubelet answer on every VIP (they
    listen on all addresses, and a VIP is a host address). The guard drops
    that traffic at the uplink, counts it, and leaves the node's own
    address alone.

    Malformed frames to the VIP are dropped and classified too -- the
    parser's verdict-from-what-it-read path, which no valid-header probe
    above can reach.
    """
    if family == "v6" and not topo.has_ipv6:
        pytest.skip("cluster has no IPv6 on the node subnets")
    # From off to on, so the attach and config logs asserted below are
    # produced inside this test's window even when --address-guard already
    # had the guard on (re-applying an identical config logs nothing).
    address_guard(None)
    address_guard("enforce")
    holders = guarded_holders(topo, lb_service, f"guard-ports-{family}", FAMILIES[family])

    for vip, node in holders.items():
        fam = FAMILY_LABEL[ipaddress.ip_address(vip).version]
        before = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="drop", reason="port_denied", family=fam)
        before_vip = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        assert router.http_status(vip)[0] == 200, f"{vip}:80 (the Service port) did not serve"
        # The node's own address in the VIP's family is the control.
        node_ip = topo.node_ips6.get(node) if fam == "ipv6" else topo.node_ips[node]
        assert node_ip, f"{node} has no IPv6 address of its own to check against"
        for port in (22, 10250):
            assert not router.tcp_open(vip, port), f"{vip}:{port} answered through the guard"
            assert router.tcp_open(node_ip, port), (
                f"{node_ip}:{port} ({node}) stopped answering on the node address: the guard must touch only VIPs"
            )
        assert router.ping(vip), f"ping to {vip} was dropped; ICMP must pass"
        after = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="drop", reason="port_denied", family=fam)
        after_vip = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        assert_guard_dropped(before, after, f"{vip}:22 and :10250 ({fam} port_denied)", node, min_delta=2)
        assert_guard_dropped(before_vip, after_vip, f"traffic to {vip}", node, min_delta=2)

        # Malformed frames are dropped and counted as such. Sent from the
        # router as raw L2 frames to the announcing node's uplink MAC: the
        # kernel won't source a bad-version IP packet and a router won't
        # forward one, so an on-link L2 send is the only way the malformation
        # reaches the guard intact (see Router.send_malformed).
        mac = nodes.mac_for_address(topo.node_ips[node], vip)
        assert mac, f"no uplink MAC for {vip} on {node}"
        before_bad = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="drop", reason="malformed", family=fam)
        before_bad_vip = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        sent = router.send_malformed(router.interface_for_subnet(vip), mac, vip, count=5)
        wait_until(
            lambda s=sent, b=before_bad: sum(guard_drops(
                agent_metrics, topo, GUARD_PACKETS, action="drop", reason="malformed", family=fam
            ).values()) >= sum(b.values()) + s,
            timeout=15, description=f"the guard to drop {sent} malformed {fam} frames to {vip}",
        )
        after_bad_vip = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        assert_guard_dropped(before_bad_vip, after_bad_vip, f"malformed frames to {vip}", node, min_delta=sent)

        if fam == "ipv6":
            detail = nodes.address_detail(topo.node_ips[node], vip)
            assert detail is not None and detail.has_flag("deprecated"), (
                f"{vip} is not deprecated ({detail}); the host could source traffic "
                f"from it and the guard would drop the replies"
            )

    # Node-originated traffic is unaffected in both families.
    node = sorted(set(holders.values()))[0]
    host = topo.node_ips[node]
    nodes.ssh(host, f"nc -z -w 3 {router.host} 22")
    if topo.has_ipv6:
        nodes.ssh(host, f"nc -z -w 3 {router_v6(router)} 22")

    # Attached exactly to the uplinks, everywhere, without errors.
    for name, ip in sorted(topo.node_ips.items()):
        snap = agent_metrics(name)
        assert set(guard.attached(snap)) == uplinks(ip), (
            f"{name}: guard on {sorted(guard.attached(snap))}, expected the uplinks {sorted(uplinks(ip))}"
        )
        assert snap.counter("purelb_address_guard_attach_errors_total") == 0, f"{name}: attach errors"
        # Attached at the head of each tcx chain, and reported there.
        for iface, hook in guard.attached(snap).items():
            if hook == "tcx":
                assert snap.get("purelb_address_guard_chain_position", interface=iface) == 1, (
                    f"{name}: the guard doesn't run first on {iface}"
                )

    logs = cluster.component_logs("lbnodeagent", log_window)
    assert any("guardConfigApplied" in t for t in logs.values())
    assert any("guardAttached" in t for t in logs.values())


@pytest.mark.requires("router")
def test_address_guard_passes_pmtud_and_filters_icmp(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    pinned_backend, address_guard, router, agent_metrics,
):
    """Path MTU discovery works through the guard; other ICMP is filtered.

    ICMP is kept for PMTUD above all: a reply sourced from the VIP that is
    too big for the path makes a router send "fragmentation needed" (v4) or
    "packet too big" (v6) back to the VIP. If the guard dropped it the
    transfer would stall. A client behind a 1280-MTU route on the router
    forces exactly that, and the 64 KiB transfer must complete.
    """
    address_guard("enforce")
    backend.ensure_configmap(cluster, NAMESPACE)  # the /bytes route
    families = ["IPv4", "IPv6"] if topo.has_ipv6 else ["IPv4"]
    holders = guarded_holders(
        topo, lb_service, "guard-pmtu", families, policy="RequireDualStack" if topo.has_ipv6 else None,
        selector={"app": pinned_backend("guard-pmtu-backend", sorted(topo.node_ips)[0])},
    )

    for vip, node in holders.items():
        v6 = ipaddress.ip_address(vip).version == 6
        fam = FAMILY_LABEL[6 if v6 else 4]
        before_deny = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="drop", reason="icmp_denied", family=fam)

        assert router.ping(vip)
        # Redirect (v6 137) and timestamp (v4 13) are not on the allow-list.
        router.send_icmp(vip, 137 if v6 else 13, count=3)
        # Taken after the ping: an echo request is passed ICMP too, and
        # would satisfy the check below without any PMTUD message.
        before_pass = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="pass", reason="icmp", family=fam)
        # A PMTUD message can arrive with its ICMP header outside the skb's
        # linear area (virtio does this some of the time); the guard must
        # pull it in rather than drop it as malformed. Any malformed drop
        # here is that, whether or not the transfer happened to survive it.
        before_bad = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="drop", reason="malformed", family=fam)

        bpf = "(icmp6 and ip6[40]==2)" if v6 else "(icmp and icmp[0]==3 and icmp[1]==4)"
        with router.capture("any", bpf, seconds=60) as cap:
            with router.pmtu_client() as ns:
                status, size = router.http_status(vip, "/bytes?n=65536", netns=ns, timeout=20)
        assert (status, size) == (200, 65536), (
            f"64 KiB from {vip} through a 1280 path: got {status}, {size} bytes. "
            f"If PMTUD messages were dropped the transfer stalls."
        )
        assert cap.count() > 0, f"the router sent no PMTUD message to {vip}; the test proved nothing"

        # One transfer brings only a couple of PMTUD messages, and only some
        # arrive non-linear: 50 full-size ones (the size of an IPv6 packet
        # too big) make one that does all but certain. A full-size packet
        # can arrive with even its IP header outside the linear area; one
        # the guard can't read passes uncounted -- unfiltered -- so 50
        # full-size disallowed ones must all be counted as dropped, too.
        # A real error quotes the VIP's own packet.
        router.send_icmp(vip, 2 if v6 else 3, count=50, payload=1232, quote_src=vip)
        router.send_icmp(vip, 137 if v6 else 13, count=50, payload=1232)
        # An error quoting the node's own traffic, sent to the VIP, is
        # forged: the node would act on it (path MTU, socket errors).
        own = topo.node_ips6.get(node) if v6 else topo.node_ips[node]
        assert own, f"{node} has no address of its own in {fam}"
        router.send_icmp(vip, 2 if v6 else 3, count=5, payload=1232, quote_src=own)
        after_pass = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="pass", reason="icmp", family=fam)
        after_bad = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="drop", reason="malformed", family=fam)
        after_deny = guard_drops(agent_metrics, topo, GUARD_PACKETS, action="drop", reason="icmp_denied", family=fam)
        assert after_bad == before_bad, (
            f"the guard dropped traffic to {vip} as malformed during PMTUD: before {before_bad}, after {after_bad}"
        )
        assert sum(after_pass.values()) >= sum(before_pass.values()) + 50, (
            f"the guard didn't pass the PMTUD ICMP for {vip} (holder {node}); before {before_pass}, after {after_pass}"
        )
        assert_guard_dropped(before_deny, after_deny, f"disallowed and forged ICMP to {vip}", node, min_delta=3 + 50 + 5)


@pytest.mark.requires("router")
def test_address_guard_monitor_mode_then_xdp(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    address_guard, router, agent_metrics, kube_context: str,
):
    """The program is loaded only when an agent starts with the guard
    configured. Enabled later, it waits for a restart -- without standing
    down, and saying so -- and disabled, it detaches at once.

    Then: monitor counts what enforce would drop and drops nothing; the
    opt-in XDP hook enforces the same rules. Both are switched live.

    The test cluster's virtio NICs take native XDP (checked when the guard
    was designed), so the hook is expected to be xdp, not a tcx fallback.
    """
    # Off at startup: nothing is loaded into the kernel.
    address_guard(None)
    since = utcnow()
    guard.restart_agents(cluster, topo.node_ips)
    for name in topo.node_ips:
        snap = wait_until(lambda n=name: (lambda s: s if s.get(guard.LOADED) is not None else None)(agent_metrics(n)),
                          timeout=60, description=f"{name}'s agent to answer")
        assert snap.get(guard.LOADED) == 0, f"{name} loaded the guard program although it isn't configured"
        assert snap.get(guard.RESTART_REQUIRED) == 0
    logs = cluster.component_logs("lbnodeagent", since)
    assert all('"event":"guardNotLoaded"' in t and '"event":"guardLoaded"' not in t for t in logs.values()), (
        "every agent started without the guard must say it didn't load it, and not load it"
    )
    families = ["IPv4", "IPv6"] if topo.has_ipv6 else ["IPv4"]
    holders = guarded_holders(topo, lb_service, "guard-modes", families,
                              policy="RequireDualStack" if topo.has_ipv6 else None)

    # Enabled live: nothing is filtered until a restart, the node keeps
    # announcing, and the agent says a restart is needed.
    since = utcnow()
    address_guard("monitor", wait=False)
    wait_until(lambda: all(agent_metrics(n).get(guard.RESTART_REQUIRED) == 1 for n in topo.node_ips),
               timeout=60, description="every agent to report that the guard needs a restart")
    for name in topo.node_ips:
        snap = agent_metrics(name)
        assert snap.get(guard.LOADED) == 0 and not guard.attached(snap), f"{name}: {guard.attached(snap)}"
        assert snap.get("purelb_address_guard_standing_down") == 0, f"{name} stood down for a guard it never loaded"
        assert (snap.get("purelb_address_guard_unguarded_vips", reason="restart_pending") or 0) >= len(holders)
    for vip in holders:
        assert announced_on(topo, vip), f"{vip} was withdrawn although the guard isn't loaded"
        assert router.tcp_open(vip, 22), f"{vip}:22 filtered before the restart that loads the guard"
    events = [e.message or "" for e in cluster.core.list_namespaced_event(
        cluster.purelb_namespace, field_selector="involvedObject.kind=LBNodeAgent,reason=AddressGuardRestartRequired").items]
    assert any("takes effect when lbnodeagent is restarted" in m for m in events), f"no restart Event: {events}"
    assert any('"event":"guardRestartRequired"' in t for t in cluster.component_logs("lbnodeagent", since).values())
    # The plugin reads the same state from the agents: the configured mode
    # alone would say "monitor" while nothing runs.
    live = {s["node"]: s for s in guard.plugin_json(kube_context, "guard")}
    assert set(live) == set(topo.node_ips), f"guard listed {sorted(live)}"
    for name, s in live.items():
        assert s["mode"] == "monitor" and s["state"] == "restart required" and not s["loaded"], f"{name}: {s}"
    status = guard.plugin_json(kube_context, "status")
    assert any("need an lbnodeagent restart for the address guard" in w for w in status["warnings"]), status["warnings"]

    # Restarted: loaded and attached.
    guard.restart_agents(cluster, topo.node_ips)
    wait_until(lambda: guard.applied_everywhere(agent_metrics, topo.node_ips, "tcx"), timeout=120,
               description="the guard to attach after the restart")
    for name in topo.node_ips:
        snap = agent_metrics(name)
        assert snap.get(guard.LOADED) == 1 and snap.get(guard.RESTART_REQUIRED) == 0, name
    for s in guard.plugin_json(kube_context, "guard"):
        assert s["state"] == "monitoring" and s["loaded"] and s["attached"], s
        assert set(s["attached"]) == uplinks(topo.node_ips[s["node"]]), s

    # Disabled live: detached at once; the program is unloaded at the next
    # restart, which is the startup path checked first above.
    address_guard(None)
    for name in topo.node_ips:
        snap = agent_metrics(name)
        assert snap.get(guard.RESTART_REQUIRED) == 1 and snap.get(guard.LOADED) == 1, name
    # No LBNodeAgent configures the guard now, but status still notices the
    # program loaded on every node.
    status = guard.plugin_json(kube_context, "status")
    assert any("need an lbnodeagent restart for the address guard" in w for w in status["warnings"]), status["warnings"]

    # Re-enabled while loaded: live, no restart.
    address_guard("monitor")
    assert all(agent_metrics(n).get(guard.RESTART_REQUIRED) == 0 for n in topo.node_ips)
    for vip in holders:
        node = wait_until(lambda v=vip: announced_on(topo, v), timeout=45, description=f"{vip} to be announced")[0]
        before = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="would_drop")
        assert router.tcp_open(vip, 22), f"monitor mode dropped {vip}:22"
        after = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="would_drop")
        assert_guard_dropped(before, after, f"{vip}:22 as would_drop", node)

    address_guard("enforce", hook="xdp")
    for vip, node in holders.items():
        assert router.http_status(vip)[0] == 200, f"{vip}:80 did not serve under the XDP hook"
        before = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        assert not router.tcp_open(vip, 22), f"{vip}:22 answered under the XDP hook"
        after = guard_drops(agent_metrics, topo, GUARD_VIP_PACKETS, ip=vip, action="drop")
        assert_guard_dropped(before, after, f"{vip}:22 under the XDP hook", node)
    for name in topo.node_ips:
        snap = agent_metrics(name)
        assert set(guard.attached(snap).values()) == {"xdp"}, f"{name}: {guard.attached(snap)}"
        assert snap.counter("purelb_address_guard_attach_errors_total") == 0


@pytest.mark.requires("multi-node")
def test_address_guard_configured_interfaces(
    cluster: Cluster, topo: topology.Topology, address_guard, agent_metrics, log_window,
):
    """extraInterfaces adds links and excludeInterfaces removes them, live,
    scoped to one node by a nodeSelector -- the canary pattern."""
    from kubernetes.client.rest import ApiException

    address_guard("enforce")
    node = sorted(topo.node_ips)[-1]
    host = topo.node_ips[node]
    uplink = sorted(uplinks(host))[0]
    canary = "guard-canary"
    nodes.ssh(host, "sudo ip link add ag-e2e0 type dummy && sudo ip link set ag-e2e0 up")
    try:
        cluster.apply_cr({
            "apiVersion": "purelb.io/v2",
            "kind": "LBNodeAgent",
            "metadata": {"name": canary, "namespace": cluster.purelb_namespace},
            "spec": {
                "nodeSelector": {"matchLabels": {"kubernetes.io/hostname": node}},
                "local": guard.local_spec(
                    {"localInterface": "default", "dummyInterface": "kube-lb0"},
                    {"mode": "enforce", "extraInterfaces": ["ag-e2e0"], "excludeInterfaces": [uplink]},
                ),
            },
        })
        got = wait_until(
            lambda: (lambda a: a if a == {"ag-e2e0": "tcx"} else None)(guard.attached(agent_metrics(node))),
            timeout=60, description=f"{node} to guard ag-e2e0 and not {uplink}",
        )
        assert got == {"ag-e2e0": "tcx"}
        other = next(n for n in sorted(topo.node_ips) if n != node)
        assert set(guard.attached(agent_metrics(other))) == uplinks(topo.node_ips[other]), (
            f"the canary's nodeSelector leaked: {other} changed too"
        )
        pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)
        logs = cluster.pod_logs(cluster.purelb_namespace, pod.metadata.name, log_window, container="lbnodeagent")
        assert "guardInterfaceExcluded" in logs, "excluding an interface must be logged loudly"

        # The CRD refuses a name in both lists.
        with pytest.raises(ApiException) as err:
            cluster.apply_cr({
                "apiVersion": "purelb.io/v2",
                "kind": "LBNodeAgent",
                "metadata": {"name": "guard-invalid", "namespace": cluster.purelb_namespace},
                "spec": {"local": {"localInterface": "default", "addressGuard": {
                    "extraInterfaces": ["eth9"], "excludeInterfaces": ["eth9"]}}},
            })
        assert err.value.status == 422
    finally:
        cluster.delete_cr("lbnodeagent", canary)
        # Only there if the CRD wrongly accepted it.
        cluster.delete_cr("lbnodeagent", "guard-invalid")
        nodes.ssh(host, "sudo ip link del ag-e2e0 2>/dev/null; true", check=False)
    wait_until(lambda: set(guard.attached(agent_metrics(node))) == uplinks(host),
               timeout=60, description=f"{node} back on its uplinks")
