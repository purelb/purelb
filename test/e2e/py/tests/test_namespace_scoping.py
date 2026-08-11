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

"""Namespace-scoped ServiceGroups.

The v0.17.0 headline feature, and it had no end-to-end coverage at all.

Three fields, and the interesting behaviour is in how they combine:

* `spec.namespaces` is ELIGIBILITY, not ownership. A Service in a listed
  namespace with no annotation allocates from here -- but an annotation
  still overrides it in both directions. Empty means every namespace,
  which is how every ServiceGroup behaved before the field existed.

* `spec.enforceNamespaces` turns that default into a BOUNDARY, and the
  boundary belongs to the NAMESPACE rather than to the group that set
  it. Setting it on any one of a namespace's groups fences that namespace
  in both directions -- outsiders cannot reach in, and insiders cannot
  reach out. Otherwise fencing a tenant's L2 pool would leave its BGP
  pool reachable from everywhere, which is a fence with a gate in it.

* `spec.namespaceDefault` picks WHICH group an unannotated Service gets
  where several serve one namespace. That is the normal case, not an
  exotic one: a ServiceGroup is exactly one of local, remote or external,
  so a tenant wanting both L2 and BGP addresses necessarily has two.

The subtlest rule is what happens when several groups serve a namespace
and none is marked as its default. There is deliberately no implicit
tie-break. Not enforcing, it falls back to the "default" pool and warns,
because that is what the namespace did before any binding existed and an
additive config must not break it. Enforcing, it is denied, because
falling through would breach the boundary the namespace just declared.
Both directions are tested below; getting either wrong is silent.

This is an allocation control, NOT RBAC: PureLB ships no admission
webhook, so a refused Service is created quite happily and simply never
receives an address. The tests assert exactly that shape -- the object
exists, the ingress does not.
"""

from __future__ import annotations

import ipaddress
from typing import Dict, Iterator, List, Optional

import pytest

from purelb_e2e import TEST_NAMESPACE, metrics, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until

TENANT_NS = "test-tenant"
L2_GROUP = "tenant-l2"
ALT_GROUP = "tenant-alt"
SERVICE_GROUP = "purelb.io/service-group"
# Subsystem is address_pool, NOT allocator. Asserting the wrong name
# reads as a counter stuck at zero, which is why assert_increased now
# rejects an absent metric name outright.
REJECTED = "purelb_address_pool_allocation_rejected_total"


@pytest.fixture(scope="module")
def tenant_namespace(cluster: Cluster) -> Iterator[str]:
    """A scratch namespace with its own backend.

    Its own nginx, because a Service only reaches endpoints in its own
    namespace -- a VIP allocated here and pointed at the backend in
    `test` would allocate fine and serve nothing, which would look like
    an announcement bug.

    Module-scoped, and that is not merely an optimisation. Namespace
    deletion is ASYNCHRONOUS: a per-test fixture deleted it and the next
    test then failed to create anything, because a terminating namespace
    accepts no new content ("unable to create new content in namespace
    test-tenant because it is being terminated"). What varies between
    these tests is the ServiceGroups, not the namespace, so the namespace
    is created once and the groups are per-test.
    """
    from kubernetes.client.rest import ApiException

    # A previous run may have left it terminating; wait it out rather
    # than failing at setup.
    wait_until(
        lambda: _namespace_usable(cluster, TENANT_NS) or None,
        timeout=180, interval=3.0,
        description=f"namespace {TENANT_NS} to be absent or Active",
    )
    try:
        cluster.core.create_namespace({"metadata": {"name": TENANT_NS}})
    except ApiException as exc:
        if exc.status != 409:
            raise

    labels = {"app": "nginx"}
    try:
        cluster.apps.create_namespaced_deployment(
            TENANT_NS,
            {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {"name": "nginx", "namespace": TENANT_NS},
                "spec": {
                    "replicas": 1,
                    "selector": {"matchLabels": labels},
                    "template": {
                        "metadata": {"labels": labels},
                        "spec": {
                            "containers": [
                                {"name": "nginx", "image": "nginx:alpine",
                                 "ports": [{"containerPort": 80}]}
                            ]
                        },
                    },
                },
            },
        )
    except ApiException as exc:
        if exc.status != 409:
            raise
    cluster.wait_rollout(TENANT_NS, "nginx", timeout=180)

    yield TENANT_NS

    cluster.core.delete_namespace(TENANT_NS)
    # Wait for it to actually go. Leaving a terminating namespace behind
    # makes the NEXT run fail at setup, which reads as a broken test
    # rather than a slow teardown.
    wait_until(
        lambda: _namespace_gone(cluster, TENANT_NS) or None,
        timeout=180, interval=3.0,
        description=f"namespace {TENANT_NS} to finish terminating",
    )


def _namespace_gone(cluster: Cluster, name: str) -> bool:
    from kubernetes.client.rest import ApiException

    try:
        cluster.core.read_namespace(name)
    except ApiException as exc:
        return exc.status == 404
    return False


@pytest.fixture
def tenant_group(cluster: Cluster, topo: topology.Topology):
    """Namespace-scoped ServiceGroups, removed afterwards."""
    created: List[str] = []

    def make(
        name: str,
        namespaces: Optional[List[str]] = None,
        enforce: bool = False,
        namespace_default: bool = False,
        alt_band: bool = False,
    ) -> str:
        subnet = topo.subnets[0]
        v4 = subnet.tenant_pool_b if alt_band else subnet.tenant_pool
        v6 = subnet.tenant_pool_b_v6 if alt_band else subnet.tenant_pool_v6
        local: Dict[str, object] = {
            "v4pools": [{"aggregation": "default", "pool": v4, "subnet": subnet.v4}]
        }
        if v6:
            local["v6pools"] = [
                {"aggregation": "default", "pool": v6, "subnet": subnet.v6}
            ]
        spec: Dict[str, object] = {"local": local}
        if namespaces is not None:
            spec["namespaces"] = namespaces
        if enforce:
            spec["enforceNamespaces"] = True
        if namespace_default:
            spec["namespaceDefault"] = True

        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": spec,
            }
        )
        created.append(name)

        # WAIT for the allocator to have processed it. Creating the group
        # and a Service back to back is a race: until the allocator has
        # indexed the new ServiceGroup the namespace has no binding, so
        # the Service falls through to the "default" pool -- and every
        # assertion then fails with "the namespace binding did not take
        # effect", which is true but says nothing about the feature.
        #
        # status.boundNamespaces is the right signal because it is the
        # allocator reporting what IT parsed, not what we submitted.
        wait_until(
            lambda: (
                (cluster.get_cr("servicegroup", name) or {}).get("status", {})
                .get("boundNamespaces") == (namespaces or [])
                or (not namespaces and (cluster.get_cr("servicegroup", name) or {}).get("status"))
            ) or None,
            timeout=60, interval=2.0,
            description=f"the allocator to bind ServiceGroup {name} to {namespaces}",
        )
        return name

    yield make

    for name in reversed(created):
        cluster.delete_cr("servicegroup", name)


def _namespace_usable(cluster: Cluster, name: str) -> bool:
    """Absent, or present and Active. Terminating is neither."""
    from kubernetes.client.rest import ApiException

    try:
        ns = cluster.core.read_namespace(name)
    except ApiException as exc:
        if exc.status == 404:
            return True
        raise
    return ns.status.phase == "Active"


def events_for(cluster: Cluster, namespace: str, name: str) -> List[str]:
    """Event messages for one Service, newest first."""
    got = cluster.core.list_namespaced_event(
        namespace, field_selector=f"involvedObject.name={name}"
    )
    return [e.message for e in got.items if e.message]


def assert_never_allocates(cluster: Cluster, namespace: str, name: str, seconds: int = 20) -> None:
    """The Service exists and never receives an address.

    An allocation refused by the namespace boundary is not rejected at
    admission -- PureLB has no webhook -- so the object is created and
    simply stays without an ingress. Proving that means WAITING, because
    "no address yet" and "no address ever" look identical at t=0.
    """
    import time

    for _ in range(seconds // 2):
        time.sleep(2)
        ips = cluster.service_ingress_ips(namespace, name)
        assert not ips, f"{namespace}/{name} was allocated {ips}, but the boundary should have refused it"
    assert cluster.service(namespace, name) is not None, (
        f"{namespace}/{name} disappeared; PureLB has no admission webhook and "
        f"must never delete a Service it declined to serve"
    )


# ------------------------------------------------- eligibility (no fence)


def test_bound_namespaces_is_published_in_status(
    cluster: Cluster, tenant_namespace: str, tenant_group
):
    """status.boundNamespaces is how an operator sees what was parsed."""
    group = tenant_group(L2_GROUP, namespaces=[TENANT_NS])
    status = wait_until(
        lambda: (cluster.get_cr("servicegroup", group) or {}).get("status") or None,
        timeout=45, interval=2.0,
        description="the tenant ServiceGroup to publish status",
    )
    assert status.get("boundNamespaces") == [TENANT_NS], (
        f"status.boundNamespaces = {status.get('boundNamespaces')!r}"
    )


def test_unannotated_service_in_the_namespace_uses_its_group(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    tenant_namespace: str, tenant_group, lb_service, allocator_metrics,
):
    """The whole point: no annotation, and it still lands in the tenant pool."""
    group = tenant_group(L2_GROUP, namespaces=[TENANT_NS])
    before = allocator_metrics()

    ips = lb_service("tenant-svc", ["IPv4"], namespace=TENANT_NS, annotations={})
    pool = topo.subnets[0].tenant_pool
    low, high = pool.split("-")
    assert ipaddress.ip_address(low) <= ipaddress.ip_address(ips[0]) <= ipaddress.ip_address(high), (
        f"{ips[0]} is not from the tenant pool {pool}; the namespace binding "
        f"did not take effect"
    )
    metrics.assert_increased(
        before, allocator_metrics(), "purelb_address_pool_addresses_in_use", pool=group
    )


def test_a_service_elsewhere_still_gets_the_default_group(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    tenant_namespace: str, tenant_group, lb_service,
):
    """Binding a namespace must not change any other namespace.

    An additive config that quietly re-pointed existing Services would be
    the worst possible upgrade behaviour.
    """
    tenant_group(L2_GROUP, namespaces=[TENANT_NS])
    ips = lb_service("outside-svc", ["IPv4"], namespace=TEST_NAMESPACE, annotations={})
    subnet = topo.subnet_holding(ips[0])
    low, high = subnet.default_pool.split("-")
    assert ipaddress.ip_address(low) <= ipaddress.ip_address(ips[0]) <= ipaddress.ip_address(high), (
        f"a Service in {TEST_NAMESPACE} got {ips[0]}, which is not from the "
        f"default pool {subnet.default_pool}"
    )


def test_without_enforcement_an_annotation_still_reaches_in(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    tenant_namespace: str, tenant_group, lb_service,
):
    """namespaces alone is a DEFAULT, not a boundary.

    This is the distinction the two fields exist to draw, and the only
    way to see it is to try to cross the line and succeed.
    """
    group = tenant_group(L2_GROUP, namespaces=[TENANT_NS])
    ips = lb_service(
        "outsider-by-annotation", ["IPv4"], namespace=TEST_NAMESPACE,
        annotations={SERVICE_GROUP: group},
    )
    pool = topo.subnets[0].tenant_pool
    low, high = pool.split("-")
    assert ipaddress.ip_address(low) <= ipaddress.ip_address(ips[0]) <= ipaddress.ip_address(high), (
        f"{ips[0]} is not from {pool}: an annotation should reach a "
        f"non-enforcing group from any namespace"
    )


@pytest.mark.requires("ipv6")
def test_dual_stack_from_a_namespace_scoped_group(
    cluster: Cluster, topo: topology.Topology, tenant_namespace: str,
    tenant_group, lb_service,
):
    """One address per family, both from the tenant's own pools."""
    tenant_group(L2_GROUP, namespaces=[TENANT_NS])
    ips = lb_service(
        "tenant-dual", ["IPv4", "IPv6"], namespace=TENANT_NS, annotations={}, timeout=90
    )
    assert len(ips) == 2, f"expected one address per family, got {ips}"
    subnet = topo.subnets[0]
    v4 = [i for i in ips if ipaddress.ip_address(i).version == 4]
    v6 = [i for i in ips if ipaddress.ip_address(i).version == 6]
    assert len(v4) == 1 and len(v6) == 1, ips
    lo4, hi4 = subnet.tenant_pool.split("-")
    lo6, hi6 = subnet.tenant_pool_v6.split("-")
    assert ipaddress.ip_address(lo4) <= ipaddress.ip_address(v4[0]) <= ipaddress.ip_address(hi4)
    assert ipaddress.ip_address(lo6) <= ipaddress.ip_address(v6[0]) <= ipaddress.ip_address(hi6)


# --------------------------------------------------------- the boundary


def test_enforcement_refuses_an_outsider(
    cluster: Cluster, default_servicegroup: str, tenant_namespace: str,
    tenant_group, lb_service, allocator_metrics,
):
    """Outsiders cannot reach in, even by naming the group explicitly."""
    group = tenant_group(L2_GROUP, namespaces=[TENANT_NS], enforce=True)
    before = allocator_metrics()

    lb_service(
        "outsider-denied", ["IPv4"], namespace=TEST_NAMESPACE,
        annotations={SERVICE_GROUP: group}, wait=False,
    )
    assert_never_allocates(cluster, TEST_NAMESPACE, "outsider-denied")

    metrics.assert_increased(
        before, allocator_metrics(), REJECTED, pool=group, reason="namespace_denied"
    )
    messages = events_for(cluster, TEST_NAMESPACE, "outsider-denied")
    assert any("namespace denied" in m for m in messages), (
        f"no AllocationFailed event explaining the refusal: {messages}"
    )


def test_enforcement_also_refuses_an_insider_reaching_out(
    cluster: Cluster, default_servicegroup: str, tenant_namespace: str,
    tenant_group, lb_service, allocator_metrics,
):
    """The other direction, which is the half that is easy to forget.

    A fence that only stops outsiders coming in is not a fence: the
    tenant could allocate from the cluster-wide default pool and take an
    address nobody expected it to have.
    """
    tenant_group(L2_GROUP, namespaces=[TENANT_NS], enforce=True)
    before = allocator_metrics()

    lb_service(
        "insider-reaching-out", ["IPv4"], namespace=TENANT_NS,
        annotations={SERVICE_GROUP: "default"}, wait=False,
    )
    assert_never_allocates(cluster, TENANT_NS, "insider-reaching-out")

    metrics.assert_increased(
        before, allocator_metrics(), REJECTED, pool="default", reason="namespace_denied"
    )


def test_enforcement_on_one_group_fences_the_whole_namespace(
    cluster: Cluster, default_servicegroup: str, tenant_namespace: str,
    tenant_group, lb_service,
):
    """Enforcement is a property of the NAMESPACE, not of the group.

    Two groups serve the tenant and only ONE sets enforceNamespaces. The
    namespace is fenced completely regardless -- otherwise fencing a
    tenant's L2 pool would leave its BGP pool reachable from everywhere,
    which is a fence with a gate in it.
    """
    tenant_group(L2_GROUP, namespaces=[TENANT_NS], enforce=True, namespace_default=True)
    unenforced = tenant_group(ALT_GROUP, namespaces=[TENANT_NS], alt_band=True)

    lb_service(
        "outsider-via-unenforced", ["IPv4"], namespace=TEST_NAMESPACE,
        annotations={SERVICE_GROUP: unenforced}, wait=False,
    )
    assert_never_allocates(cluster, TEST_NAMESPACE, "outsider-via-unenforced")


# ------------------------------------------------------ ambiguous default


def test_namespace_default_picks_between_two_groups(
    cluster: Cluster, topo: topology.Topology, tenant_namespace: str,
    tenant_group, lb_service,
):
    """Where two groups serve a namespace, the marked one wins."""
    tenant_group(L2_GROUP, namespaces=[TENANT_NS])
    tenant_group(ALT_GROUP, namespaces=[TENANT_NS], namespace_default=True, alt_band=True)

    ips = lb_service("tenant-picks-default", ["IPv4"], namespace=TENANT_NS, annotations={})
    lo, hi = topo.subnets[0].tenant_pool_b.split("-")
    assert ipaddress.ip_address(lo) <= ipaddress.ip_address(ips[0]) <= ipaddress.ip_address(hi), (
        f"{ips[0]} did not come from the group marked namespaceDefault"
    )


def test_ambiguous_default_without_enforcement_falls_back_and_warns(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str,
    tenant_namespace: str, tenant_group, lb_service, allocator_metrics,
):
    """Two groups, neither marked: fall through to "default" and warn.

    Falling back is deliberate -- it is what this namespace did before
    any binding existed, so an additive config cannot break it. The
    warning is what stops that being silent, and there must be NO
    rejection metric, because nothing was rejected.
    """
    tenant_group(L2_GROUP, namespaces=[TENANT_NS])
    tenant_group(ALT_GROUP, namespaces=[TENANT_NS], alt_band=True)
    before = allocator_metrics()

    ips = lb_service("tenant-ambiguous", ["IPv4"], namespace=TENANT_NS, annotations={})
    subnet = topo.subnet_holding(ips[0])
    lo, hi = subnet.default_pool.split("-")
    assert ipaddress.ip_address(lo) <= ipaddress.ip_address(ips[0]) <= ipaddress.ip_address(hi), (
        f"{ips[0]} is not from the default pool; an ambiguous binding should "
        f"fall back rather than guess"
    )

    messages = events_for(cluster, TENANT_NS, "tenant-ambiguous")
    assert any("none sets namespaceDefault" in m for m in messages), (
        f"no ConfigurationWarning naming the candidates: {messages}"
    )
    metrics.assert_not_increased(
        before, allocator_metrics(), REJECTED, reason="ambiguous_namespace_default"
    )


def test_ambiguous_default_with_enforcement_is_denied(
    cluster: Cluster, default_servicegroup: str, tenant_namespace: str,
    tenant_group, lb_service, allocator_metrics,
):
    """Enforcing, the same ambiguity is a denial rather than a fallback.

    Falling through to "default" here would breach the boundary the
    namespace just declared, so the only correct answer is to refuse and
    say why.
    """
    tenant_group(L2_GROUP, namespaces=[TENANT_NS], enforce=True)
    tenant_group(ALT_GROUP, namespaces=[TENANT_NS], alt_band=True)
    before = allocator_metrics()

    lb_service("tenant-ambiguous-fenced", ["IPv4"], namespace=TENANT_NS,
               annotations={}, wait=False)
    assert_never_allocates(cluster, TENANT_NS, "tenant-ambiguous-fenced")

    metrics.assert_increased(
        before, allocator_metrics(), REJECTED, reason="ambiguous_namespace_default"
    )
