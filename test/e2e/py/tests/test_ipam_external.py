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

"""External (sidecar) IPAM.

A sidecar process in the allocator pod hands out addresses over the gRPC
IPAM contract (api/ipam/v1), PureLB programs them onto Services and
announces them, and the ServiceGroup status reflects the sidecar's Stats.

Ported from test/e2e/ipam-external/test-ipam-external.sh. Two things
changed on the way across, both deliberate:

* RPC counters are asserted as DELTAS against a baseline taken after the
  sidecar rollout. The bash suite asserted `>= 1` on monotonic counters,
  which asks whether an RPC has ever happened rather than whether this
  test caused one.

* The IPv6 and dual-stack cases are new. The product side has always been
  dual-stack -- the proto carries want_ipv6 and SidecarPool sends it --
  but the sample sidecar was IPv4-only, so the bash suite pinned
  ipFamilies: [IPv4] and no IPv6 external-IPAM path was ever exercised.

Tests in this module run in file order and share module-scoped fixtures:
allocation happens once, several tests assert on it, and the release test
tears it down last. That mirrors the bash flow, which was one sequential
script.
"""

from __future__ import annotations

import os
from typing import Dict, Iterator, List

import pytest

from purelb_e2e import metrics, nodes
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = "test"
SG_NAME = "ipam-external-test"
SVC_NAME = "ext-lb"
SIDECAR_CONTAINER = "test-sidecar"
SOCKET_VOLUME = "ipam-socket"

# This module doubles as the ACCEPTANCE TEST for anyone building a sidecar
# against api/ipam/v1, so every knob the bash suite exposed is preserved.
# Dropping one would not show up in a dual-run -- that compares assertions,
# not options -- but it would silently remove the ability to point the
# suite at a third-party sidecar, which is most of why it exists.
IMAGE = os.environ.get("SIDECAR_IMAGE", "ghcr.io/purelb/purelb/test-sidecar:latest")
PROVIDER = os.environ.get("SIDECAR_PROVIDER", "sample-ipam")
SOCKET = os.environ.get("SIDECAR_SOCKET", "/var/run/purelb/ipam.sock")
SOCKET_DIR = os.path.dirname(SOCKET)
ANNOUNCE = os.environ.get("ANNOUNCE", "local")
PULL_SECRET = os.environ.get("SIDECAR_PULL_SECRET", "")
# Unset means "derive from a node subnet"; set means use exactly this.
POOL_CIDR = os.environ.get("SIDECAR_POOL_CIDR", "")
POOL_CIDR6 = os.environ.get("SIDECAR_POOL_CIDR6", "")
# `--no-deploy-sidecar` / `--keep-sidecar` from the bash suite.
DEPLOY = os.environ.get("SIDECAR_DEPLOY", "true").lower() != "false"
KEEP = os.environ.get("SIDECAR_KEEP", "false").lower() == "true"

RPC = "purelb_allocator_sidecar_rpc_total"
ALLOCATE = "/purelb.ipam.v1.IPAM/Allocate"
RELEASE = "/purelb.ipam.v1.IPAM/Release"
STATS = "/purelb.ipam.v1.IPAM/Stats"


# --------------------------------------------------------------- fixtures


@pytest.fixture(scope="module")
def pool_cidrs(cluster: Cluster) -> Dict[str, str]:
    """Sidecar pool CIDRs: SIDECAR_POOL_CIDR[6], else derived.

    Derived from a node subnet by default, because an address outside
    every node subnet is allocated perfectly happily and then cannot be
    reached. .224/28 sits above the e2e pool bands (.200-.220 default,
    .230-.240 per-test, .244-.247 multi-interface, .248-.250 tenant).
    """
    if POOL_CIDR or POOL_CIDR6:
        # Explicit CIDRs are the third-party case: the caller knows what
        # their IPAM serves, and deriving one would point the test at a
        # range their sidecar does not own.
        assert POOL_CIDR, (
            "SIDECAR_POOL_CIDR6 was set without SIDECAR_POOL_CIDR. The IPv4 "
            "pool is not optional here -- the core assertions allocate an "
            "IPv4 Service, and only the dual-stack tests need the v6 range."
        )
        out = {"v4": POOL_CIDR}
        if POOL_CIDR6:
            out["v6"] = POOL_CIDR6
        return out

    v4_prefix = None
    v6_prefix = None
    for ip in cluster.node_ips().values():
        if ":" not in ip:
            v4_prefix = ".".join(ip.split(".")[:3])
            break
    assert v4_prefix, "no IPv4 node subnet detected; cannot derive a sidecar pool"

    # The v6 prefix is not on the Node object (nodes report a v4
    # InternalIP here), so take it from the interface that carries the
    # node's v4 address.
    first_node, first_ip = sorted(cluster.node_ips().items())[0]
    for tok in nodes.addresses_on(first_ip):
        addr = tok.split("/")[0]
        if ":" in addr and not addr.startswith("fe80") and tok.split("/")[1] == "64":
            v6_prefix = ":".join(addr.split(":")[:4])
            break

    out = {"v4": f"{v4_prefix}.224/28"}
    if v6_prefix:
        out["v6"] = f"{v6_prefix}:e::/120"
    return out


@pytest.fixture(scope="module")
def sidecar(cluster: Cluster, pool_cidrs: Dict[str, str]) -> Iterator[Dict[str, str]]:
    """Run the sample sidecar alongside the allocator, then remove it.

    Teardown addresses the container, the volume and the mount by their
    merge KEYS. The bash version computed JSON-patch indices with
    `grep -n | cut -d: -f1` and arithmetic, which removes the wrong
    container the moment the allocator gains another one.
    """
    if not DEPLOY:
        # SIDECAR_DEPLOY=false is the bash suite's --no-deploy-sidecar: the
        # caller has already added their own sidecar to the allocator, and
        # we must not patch over it.
        dep = cluster.deployment(cluster.purelb_namespace, "allocator")
        assert dep is not None, "no allocator Deployment"
        names = {c.name for c in dep.spec.template.spec.containers}
        assert SIDECAR_CONTAINER in names, (
            f"SIDECAR_DEPLOY=false but no {SIDECAR_CONTAINER!r} container in the "
            f"allocator pod; containers are {sorted(names)}"
        )
        yield pool_cidrs
        return

    env = [
        {"name": "SIDECAR_SOCKET", "value": SOCKET},
        {"name": "SIDECAR_PROVIDER", "value": PROVIDER},
    ]
    if "v4" in pool_cidrs:
        env.append({"name": "SIDECAR_POOL_CIDR", "value": pool_cidrs["v4"]})
    if "v6" in pool_cidrs:
        env.append({"name": "SIDECAR_POOL_CIDR6", "value": pool_cidrs["v6"]})
    pull_secrets = [{"name": PULL_SECRET}] if PULL_SECRET else []

    cluster.patch_deployment(
        cluster.purelb_namespace,
        "allocator",
        {
            "spec": {
                "template": {
                    "spec": {
                        "imagePullSecrets": pull_secrets,
                        "volumes": [{"name": SOCKET_VOLUME, "emptyDir": {}}],
                        "containers": [
                            {
                                "name": "allocator",
                                "volumeMounts": [
                                    {"name": SOCKET_VOLUME, "mountPath": SOCKET_DIR}
                                ],
                            },
                            {
                                "name": SIDECAR_CONTAINER,
                                "image": IMAGE,
                                # Always: the tag is mutable, and a cached
                                # older image is the classic way to test
                                # something other than what was built.
                                "imagePullPolicy": "Always",
                                "env": env,
                                "volumeMounts": [
                                    {"name": SOCKET_VOLUME, "mountPath": SOCKET_DIR}
                                ],
                            },
                        ],
                    }
                }
            }
        },
    )
    cluster.wait_rollout(cluster.purelb_namespace, "allocator")
    yield pool_cidrs

    if KEEP:
        # SIDECAR_KEEP=true is the bash suite's --keep-sidecar, for
        # debugging a sidecar after a failure.
        return

    # All three in ONE patch. Removing the volume while the allocator
    # container still mounts it is rejected 422 by the apiserver:
    #   containers[0].volumeMounts[0].name: Not found: "ipam-socket"
    # The bash cleanup removed only the container and the volume, and sent
    # both patches through `|| true` with output discarded -- so it failed
    # exactly this way and reported "removed test-sidecar from allocator"
    # regardless, leaving the volume and mount behind on the allocator.
    cluster.patch_deployment(
        cluster.purelb_namespace,
        "allocator",
        {
            "spec": {
                "template": {
                    "spec": {
                        "containers": [
                            {
                                "name": "allocator",
                                # volumeMounts merges on mountPath, NOT on
                                # name -- a delete keyed on name is
                                # rejected outright ("does not contain
                                # declared merge key: mountPath").
                                "volumeMounts": [{"mountPath": SOCKET_DIR, "$patch": "delete"}],
                            },
                            {"name": SIDECAR_CONTAINER, "$patch": "delete"},
                        ],
                        "volumes": [{"name": SOCKET_VOLUME, "$patch": "delete"}],
                    }
                }
            }
        },
    )
    cluster.wait_rollout(cluster.purelb_namespace, "allocator")
    # Assert the teardown worked. An unasserted cleanup is how the bash
    # suite leaked this for as long as it did.
    dep = cluster.deployment(cluster.purelb_namespace, "allocator")
    assert dep is not None
    spec = dep.spec.template.spec
    assert SIDECAR_CONTAINER not in {c.name for c in spec.containers}
    assert SOCKET_VOLUME not in {v.name for v in (spec.volumes or [])}


@pytest.fixture(scope="module")
def external_sg(cluster: Cluster, sidecar: Dict[str, str]) -> Iterator[str]:
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": SG_NAME, "namespace": cluster.purelb_namespace},
            "spec": {"external": {"provider": PROVIDER, "socket": SOCKET, "announce": ANNOUNCE}},
        }
    )
    yield SG_NAME
    cluster.delete_cr("servicegroup", SG_NAME)


@pytest.fixture(scope="module")
def rpc_baseline(sidecar: Dict[str, str], allocator_metrics) -> metrics.Snapshot:
    """Counters immediately after the sidecar rollout.

    Taken after the rollout on purpose: the allocator pod is replaced by
    the patch, so this is a genuine zero rather than a carried-over total.
    Every RPC assertion below is a delta from here.
    """
    return allocator_metrics()


@pytest.fixture(scope="module")
def allocated(cluster: Cluster, external_sg: str, rpc_baseline) -> Iterator[List[str]]:
    """The Service under test and the addresses it received."""
    cluster.apply_service(
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "name": SVC_NAME,
                "namespace": NAMESPACE,
                "annotations": {"purelb.io/service-group": SG_NAME},
            },
            "spec": {
                "type": "LoadBalancer",
                "ipFamilyPolicy": "SingleStack",
                "ipFamilies": ["IPv4"],
                "selector": {"app": "nginx"},
                "ports": [{"port": 80, "targetPort": 80}],
            },
        }
    )
    ips = wait_until(
        lambda: cluster.service_ingress_ips(NAMESPACE, SVC_NAME) or None,
        timeout=30,
        description=f"{SVC_NAME} to be allocated an address",
    )
    yield ips
    cluster.delete_service(NAMESPACE, SVC_NAME)


# ------------------------------------------------------------ prerequisites


def test_allocator_can_write_servicegroup_status(cluster: Cluster):
    """The v0.17.0 status subresource must be writable.

    Without it every status write 404s and the ServiceGroup silently
    stops reporting what the sidecar said.
    """
    from kubernetes import client

    review = client.AuthorizationV1Api().create_subject_access_review(
        client.V1SubjectAccessReview(
            spec=client.V1SubjectAccessReviewSpec(
                user=f"system:serviceaccount:{cluster.purelb_namespace}:allocator",
                resource_attributes=client.V1ResourceAttributes(
                    group="purelb.io",
                    resource="servicegroups",
                    subresource="status",
                    verb="update",
                    namespace=cluster.purelb_namespace,
                ),
            )
        )
    )
    assert review.status.allowed, (
        "allocator lacks servicegroups/status RBAC; apply the v0.17.0 CRD + RBAC"
    )


def test_sidecar_is_running_alongside_the_allocator(cluster: Cluster, sidecar):
    pods = cluster.pods(cluster.purelb_namespace, "component=allocator")
    assert pods, "no allocator pod"
    pod = pods[0]
    names = {c.name for c in pod.spec.containers}
    assert SIDECAR_CONTAINER in names, f"containers are {sorted(names)}"
    ready = {s.name: s.ready for s in (pod.status.container_statuses or [])}
    assert all(ready.values()), f"not all containers ready: {ready}"


def test_external_servicegroup_is_accepted(cluster: Cluster, external_sg: str):
    sg = cluster.get_cr("servicegroup", external_sg)
    assert sg is not None, f"ServiceGroup {external_sg} was not created"
    assert sg["spec"]["external"]["provider"] == PROVIDER


def test_backend_is_ready(cluster: Cluster):
    dep = cluster.deployment(NAMESPACE, "nginx")
    assert dep is not None, (
        f"no nginx Deployment in {NAMESPACE}; apply test/e2e/local/nginx-test.yaml"
    )
    assert (dep.status.available_replicas or 0) >= 1, "nginx backend has no ready replica"


# ---------------------------------------------------------------- allocation


def test_address_comes_from_the_sidecar_pool(allocated: List[str], pool_cidrs: Dict[str, str]):
    import ipaddress

    assert len(allocated) == 1, f"expected one address, got {allocated}"
    net = ipaddress.ip_network(pool_cidrs["v4"])
    assert ipaddress.ip_address(allocated[0]) in net, (
        f"{allocated[0]} is outside the sidecar pool {net}"
    )


def test_pool_type_annotation_matches_the_announce_mode(cluster: Cluster, allocated):
    assert cluster.annotation(NAMESPACE, SVC_NAME, "purelb.io/pool-type") == ANNOUNCE


@pytest.mark.skipif(ANNOUNCE != "local", reason="remote announces on kube-lb0 across all nodes")
def test_address_is_announced_on_a_node_interface(node_ips: Dict[str, str], allocated):
    found = wait_until(
        lambda: nodes.announcing_node(node_ips, allocated[0]),
        timeout=30,
        description=f"{allocated[0]} to appear on a node interface",
    )
    node, iface = found
    assert iface not in ("lo",), f"{allocated[0]} announced on {iface}, which carries no traffic"


@pytest.mark.skipif(ANNOUNCE != "local", reason="reachability is checked from a node on the VIP subnet")
def test_vip_serves_the_backend(cluster: Cluster, node_ips: Dict[str, str], allocated):
    """HTTP from a node, which is what a client would do.

    The bash suite treated a failure here as non-fatal ("SSH or curl
    unavailable") and printed an info line, so a VIP that was announced
    but did not carry traffic passed. If we can reach the node at all we
    can run curl on it, so this asserts.
    """
    vip = allocated[0]
    node = sorted(node_ips)[0]
    body = nodes.ssh(node_ips[node], f"curl -s --max-time 5 http://{vip}/", timeout=30)
    assert "Pod:" in body, f"VIP {vip} did not serve the nginx backend; got {body[:200]!r}"


# -------------------------------------------------------------------- status


def test_status_reflects_sidecar_stats(cluster: Cluster, allocated):
    status = wait_until(
        lambda: (cluster.get_cr("servicegroup", SG_NAME) or {}).get("status") or None,
        timeout=30,
        description="ServiceGroup status to be populated from the sidecar",
    )
    assert status.get("ipam") == PROVIDER, f"status.ipam = {status.get('ipam')!r}"
    assert status.get("allocatedIPv4", 0) >= 1, f"status.allocatedIPv4 = {status.get('allocatedIPv4')!r}"
    # size() saturates rather than reporting 0 for a large prefix, so a
    # known capacity must actually be a capacity.
    if "availableIPv4" in status:
        assert status["availableIPv4"] >= 0


# ------------------------------------------------------------------- metrics


def test_allocate_rpc_was_made(allocator_metrics, rpc_baseline, allocated):
    metrics.assert_increased(
        rpc_baseline, allocator_metrics(), RPC, method=ALLOCATE, code="OK"
    )


def test_stats_rpc_was_made(allocator_metrics, rpc_baseline, allocated):
    metrics.assert_increased(
        rpc_baseline, allocator_metrics(), RPC, method=STATS, code="OK"
    )


def test_no_sidecar_rpc_failed(allocator_metrics, rpc_baseline, allocated):
    """No non-OK RPC, compared as a delta.

    Asserting the absolute count is zero is the mirror of the counter bug:
    one historical error then fails the check forever, even after the
    cause is fixed.
    """
    after = allocator_metrics()
    total = after.counter(RPC) - rpc_baseline.counter(RPC)
    ok = after.counter(RPC, code="OK") - rpc_baseline.counter(RPC, code="OK")
    assert total == ok, (
        f"{total - ok:g} of {total:g} sidecar RPCs this run were not OK; series now: "
        + repr({k: v for k, v in after.samples.items() if k.startswith(RPC + "{")})
    )


# ------------------------------------------------------------------- release


def test_release_withdraws_the_address_and_calls_the_sidecar(
    cluster: Cluster, node_ips: Dict[str, str], allocator_metrics, rpc_baseline, allocated
):
    vip = allocated[0]
    cluster.delete_service(NAMESPACE, SVC_NAME)

    if ANNOUNCE == "local":
        # nodes_with_address raises on an unreachable node rather than
        # returning an empty list: proving an address is gone by asking
        # nodes that never answered would prove nothing.
        wait_while(
            lambda: bool(nodes.nodes_with_address(node_ips, vip)),
            timeout=30,
            interval=2.0,
            description=f"{vip} to be withdrawn from every node",
        )

    # Polled rather than read once: the allocator may release lazily, so
    # a single scrape immediately after the delete can legitimately miss
    # it. The bash suite downgraded that race to an info line, which meant
    # a sidecar that never released also passed.
    base = rpc_baseline.counter(RPC, method=RELEASE, code="OK")
    wait_until(
        lambda: allocator_metrics().counter(RPC, method=RELEASE, code="OK") > base,
        timeout=30,
        description="a successful Release RPC",
    )


# ------------------------------------------------- new coverage: IPv6/dual
#
# No bash counterpart: the sample sidecar was IPv4-only, so the suite
# pinned ipFamilies: [IPv4]. The product side has always sent want_ipv6.


@pytest.fixture
def dual_stack_service(cluster: Cluster, external_sg: str, pool_cidrs: Dict[str, str]):
    if "v6" not in pool_cidrs:
        pytest.skip("no IPv6 subnet detected on the nodes")

    def _make(name: str, families: List[str], policy: str):
        cluster.apply_service(
            {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {
                    "name": name,
                    "namespace": NAMESPACE,
                    "annotations": {"purelb.io/service-group": SG_NAME},
                },
                "spec": {
                    "type": "LoadBalancer",
                    "ipFamilyPolicy": policy,
                    "ipFamilies": families,
                    "selector": {"app": "nginx"},
                    "ports": [{"port": 80, "targetPort": 80}],
                },
            }
        )
        return wait_until(
            lambda: cluster.service_ingress_ips(NAMESPACE, name) or None,
            timeout=30,
            description=f"{name} to be allocated {'+'.join(families)}",
        )

    created: List[str] = []

    def make(name: str, families: List[str], policy: str = "SingleStack"):
        created.append(name)
        return _make(name, families, policy)

    yield make
    for name in created:
        cluster.delete_service(NAMESPACE, name)


def test_ipv6_only_allocation_from_the_sidecar(dual_stack_service, pool_cidrs):
    import ipaddress

    ips = dual_stack_service("ext-lb-v6", ["IPv6"])
    assert len(ips) == 1, f"expected one address, got {ips}"
    net = ipaddress.ip_network(pool_cidrs["v6"])
    assert ipaddress.ip_address(ips[0]) in net, f"{ips[0]} is outside {net}"


def test_dual_stack_allocation_from_the_sidecar(dual_stack_service, pool_cidrs):
    """One address per family, each from its own pool.

    This is the case the old sidecar could not serve at all: it ignored
    the FamilyRequest and always returned a single IPv4 address.
    """
    import ipaddress

    ips = dual_stack_service("ext-lb-dual", ["IPv4", "IPv6"], policy="RequireDualStack")
    assert len(ips) == 2, f"expected two addresses, got {ips}"
    v4 = [i for i in ips if ipaddress.ip_address(i).version == 4]
    v6 = [i for i in ips if ipaddress.ip_address(i).version == 6]
    assert len(v4) == 1 and len(v6) == 1, f"expected one of each family, got {ips}"
    assert ipaddress.ip_address(v4[0]) in ipaddress.ip_network(pool_cidrs["v4"])
    assert ipaddress.ip_address(v6[0]) in ipaddress.ip_network(pool_cidrs["v6"])


def test_dual_stack_status_counts_both_families(cluster: Cluster, dual_stack_service):
    dual_stack_service("ext-lb-dual-status", ["IPv4", "IPv6"], policy="RequireDualStack")
    status = wait_until(
        lambda: (cluster.get_cr("servicegroup", SG_NAME) or {}).get("status") or None,
        timeout=30,
        description="ServiceGroup status",
    )
    assert status.get("allocatedIPv6", 0) >= 1, (
        f"status.allocatedIPv6 = {status.get('allocatedIPv6')!r}; "
        f"the sidecar's in_use_v6 is not reaching the ServiceGroup"
    )


# ------------------------------------------------------------------- residue


def test_cleanup_leaves_no_residue(cluster: Cluster, node_ips: Dict[str, str], pool_cidrs):
    """Every Service this module created is gone, and so are its addresses.

    This replaces the bash suite's "cleanup complete", which was printed
    unconditionally at the end of a cleanup function whose every delete
    was `|| true`. It could not fail unless bash itself died, so it
    asserted that the function reached its last line -- not that anything
    had actually been cleaned up.

    Runs last in the module on purpose. The sidecar container and its
    volume come off in module teardown, which is necessarily after this,
    so their removal is guaranteed structurally rather than asserted here.
    """
    import ipaddress

    for name in (SVC_NAME, "ext-lb-v6", "ext-lb-dual", "ext-lb-dual-status"):
        assert cluster.service(NAMESPACE, name) is None, f"Service {name} survived its test"

    pools = [ipaddress.ip_network(c) for c in pool_cidrs.values()]
    for node, ip in sorted(node_ips.items()):
        for token in nodes.addresses_on(ip):
            try:
                addr = ipaddress.ip_interface(token).ip
            except ValueError:
                continue
            assert not any(addr in pool for pool in pools), (
                f"{node} still carries {addr} from a sidecar pool after cleanup"
            )
