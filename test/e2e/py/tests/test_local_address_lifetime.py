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

"""How a local VIP is configured on the interface, and why.

Ported from the Address Lifetime & CNI Compatibility group of
test/e2e/local/test-local-allocation.sh. Three properties, each defending
against a specific failure:

* A FINITE lifetime. If an agent dies without withdrawing, the address is
  orphaned on a dead node's interface; a finite valid_lft means the kernel
  removes it eventually instead of blackholing the VIP forever. The agent
  renews it on a timer for as long as it still owns the address, so a
  live VIP never actually expires.

* `noprefixroute`. Without it the kernel installs a route for the whole
  subnet via this address, which would capture traffic for every other
  host on that subnet.

* NOT `nodad` by default, for IPv6. Duplicate Address Detection is what
  stops two nodes announcing the same address after a split brain.
  skipIPv6DAD exists for environments where DAD is too slow, and it must
  be opt-in -- silently skipping DAD is how a duplicate goes unnoticed.

The renewal test's timing is the subtle part, and it is worth stating
because the bash suite worked it out and a reader would not guess it:
these assertions only prove anything if the renewal interval is SHORT
relative to the wait. The multi-interface tests set validLifetime 60 --
making the interval 30s -- precisely so that "still absent after 40s"
proves the renewal timer is dead. At the default 300 the first re-add is
at 150s, so a short wait passes whether or not the timer was cancelled.
"""

from __future__ import annotations

import time
from typing import Dict, List

import pytest

from purelb_e2e import TEST_NAMESPACE, metrics, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until

NAMESPACE = TEST_NAMESPACE
DAD_ANNOTATION = "purelb.io/skip-ipv6-dad"

# PureLB's default local address lifetime. Anything much larger suggests a
# custom addressConfig rather than a bug, so the test reports rather than
# fails outside the range.
DEFAULT_LIFETIME_MAX = 300


def detail_of(topo: topology.Topology, address: str) -> "nodes.AddressDetail":
    """The AddressDetail for `address` wherever it is announced."""
    holder, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, address),
        timeout=45, description=f"{address} to be announced",
    )
    got = nodes.address_detail(topo.node_ips[holder], address)
    assert got is not None, f"{address} vanished from {holder} between locating and reading it"
    return got


def test_local_vip_has_a_finite_lifetime(
    topo: topology.Topology, default_servicegroup: str, lb_service
):
    """A permanent VIP would survive the death of the node announcing it."""
    vip = lb_service("echo-lb-flags", ["IPv4"])[0]
    detail = detail_of(topo, vip)
    assert not detail.permanent, (
        f"{vip} has valid_lft forever; if its agent dies without "
        f"withdrawing, the address is stranded on that interface"
    )
    assert detail.valid_lft and detail.valid_lft > 0
    if detail.valid_lft > DEFAULT_LIFETIME_MAX:
        pytest.skip(
            f"valid_lft {detail.valid_lft}s exceeds the {DEFAULT_LIFETIME_MAX}s "
            f"default, so this cluster has a custom addressConfig"
        )


def test_local_vip_has_noprefixroute(
    topo: topology.Topology, default_servicegroup: str, lb_service
):
    """Without it the kernel routes the whole subnet via the VIP."""
    vip = lb_service("echo-lb-noprefixroute", ["IPv4"])[0]
    detail = detail_of(topo, vip)
    assert detail.has_flag("noprefixroute"), (
        f"{vip} lacks noprefixroute (flags: {detail.flags}); the kernel will "
        f"install a subnet route through it and capture traffic for every "
        f"other host on that subnet"
    )


@pytest.mark.requires("ipv6")
def test_ipv6_vip_does_dad_unless_told_otherwise(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """DAD is on by default, and the annotation reflects that.

    Skipping DAD silently is how two nodes end up announcing the same
    address after a split brain without anything noticing.
    """
    vip = lb_service("echo-lb-dad-default", ["IPv6"])[0]
    detail = detail_of(topo, vip)
    assert not detail.has_flag("nodad"), (
        f"{vip} has the nodad flag but skipIPv6DAD was never enabled "
        f"(flags: {detail.flags})"
    )
    annotation = cluster.annotation(NAMESPACE, "echo-lb-dad-default", DAD_ANNOTATION)
    assert annotation in (None, "", "false"), (
        f"{DAD_ANNOTATION} = {annotation!r} on a service that did not ask for it"
    )


@pytest.mark.requires("ipv6")
def test_skip_ipv6_dad_sets_nodad_when_enabled(
    cluster: Cluster, topo: topology.Topology, lb_service
):
    """The opt-in works, and is visible both on the wire and in the API."""
    subnet = next((s for s in topo.subnets if s.v6), None)
    assert subnet is not None

    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "dad-skip", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    # skipIPv6DAD is a property of the local SPEC, not of an
                    # individual pool -- ServiceGroupLocalSpec.SkipIPv6DAD.
                    # Nesting it inside the pool leaves it silently dropped
                    # and the test failing for a reason that looks like the
                    # feature not working.
                    "skipIPv6DAD": True,
                    "v6pools": [
                        {
                            "aggregation": "default",
                            "pool": subnet.test_pool_v6,
                            "subnet": subnet.v6,
                        }
                    ],
                }
            },
        }
    )
    try:
        vip = lb_service(
            "echo-dad-test", ["IPv6"],
            annotations={"purelb.io/service-group": "dad-skip"},
        )[0]
        assert cluster.annotation(NAMESPACE, "echo-dad-test", DAD_ANNOTATION) == "true", (
            f"{DAD_ANNOTATION} was not set on a service from a skipIPv6DAD pool"
        )
        detail = detail_of(topo, vip)
        assert detail.has_flag("nodad"), (
            f"{vip} is from a skipIPv6DAD pool but lacks the nodad flag "
            f"(flags: {detail.flags}); the setting did not reach the interface"
        )
    finally:
        cluster.delete_cr("servicegroup", "dad-skip")


def test_address_lifetime_is_renewed_rather_than_expiring(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    short_address_lifetime, agent_metrics, log_window,
):
    """The VIP survives, and the agent is what keeps it alive.

    Both halves matter. The address still being present proves the VIP
    works; the renewal counter advancing proves it is present BECAUSE the
    agent renewed it, rather than because the lifetime simply had not run
    out yet.
    """
    # Renewal is at ValidLft/2 with a 30s floor, so at the default 300s
    # lifetime the first one lands 150s in. Waiting less than that and
    # concluding anything about renewal measures nothing: the same
    # observation holds with the timer removed entirely.
    interval = short_address_lifetime(60)
    vip = lb_service("echo-lb-renewal", ["IPv4"])[0]
    holder, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip),
        timeout=45, description=f"{vip} to be announced",
    )
    first = nodes.address_detail(topo.node_ips[holder], vip)
    assert first is not None and not first.permanent, (
        "a permanent address is never renewed, so this test would prove nothing"
    )
    before = agent_metrics(holder)

    # One renewal interval plus slack.
    wait = interval + 12.0
    time.sleep(wait)

    second = nodes.address_detail(topo.node_ips[holder], vip)
    assert second is not None, f"{vip} disappeared from {holder} during the wait"
    assert second.valid_lft is not None

    renewed = second.valid_lft > first.valid_lft - wait + 2
    counted = agent_metrics(holder).counter(
        "purelb_lbnodeagent_address_renewals_total"
    ) > before.counter("purelb_lbnodeagent_address_renewals_total")
    assert renewed or counted, (
        f"{vip} on {holder}: valid_lft went {first.valid_lft} -> {second.valid_lft} "
        f"over {wait:.0f}s (renewal interval {interval:.0f}s) and the renewal "
        f"counter did not move, so the address is running down rather than "
        f"being renewed"
    )


def test_cni_does_not_adopt_a_vip_as_the_node_address(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """A CNI must not pick a VIP as the node's own address.

    Flannel chooses a node's public IP by scanning its interfaces. If it
    picks a PureLB VIP, the node's tunnel endpoint moves the moment the
    VIP fails over, and pod networking breaks for reasons that look
    nothing like a load-balancer problem. That is what the finite lifetime
    and noprefixroute flags are for.

    A real skip when flannel is absent. The bash suite emitted
    `pass "Test skipped (not Flannel)"` -- a PASSING assertion for a test
    that did not run, which is the untallied-skip problem in its purest
    form.
    """
    vips = lb_service("echo-lb-cni", ["IPv4"])
    if topo.has_ipv6:
        vips += lb_service("echo-lb-cni6", ["IPv6"])

    annotated = 0
    for node in sorted(topo.node_ips):
        obj = cluster.core.read_node(node)
        annotations = obj.metadata.annotations or {}
        public = [v for k, v in annotations.items() if k.endswith("/public-ip")
                  or k.endswith("/public-ipv6")]
        if not public:
            continue
        annotated += 1
        for value in public:
            assert value not in vips, (
                f"{node}: the CNI adopted PureLB VIP {value} as the node's "
                f"public address; it will move on the next failover"
            )
    if annotated == 0:
        pytest.skip("no CNI public-ip node annotations found (flannel not in use)")
