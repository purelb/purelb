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

"""The upgrade hazard every existing user will hit.

`helm upgrade` ignores the chart's crds/ directory by design, so a cluster
coming from v0.16.x keeps the OLD ServiceGroup CRD -- which has no status
subresource. Every status write then fails and `kubectl get sg` stays
blank, while allocation and announcement carry on working perfectly.

That combination is what makes it worth a test. It is not an outage, so
nothing pages; it is a monitoring feature quietly absent, which is
exactly the sort of thing that ships and is discovered months later. The
release notes document it and the remedy, and until now nothing verified
either.

WHAT THIS TEST DOES TO THE CLUSTER: it removes the status subresource
from the live ServiceGroup CRD and puts it back. That is a real,
cluster-wide change for the duration -- every ServiceGroup loses status
reporting while it runs -- so the restore is in a finally block, is
verified, and the test is skipped unless PURELB_TEST_UPGRADE=1 is set. A
test that can leave a cluster degraded should be asked for rather than
assumed.
"""

from __future__ import annotations

import copy
import os
from typing import Dict, Optional

import pytest

from purelb_e2e import TEST_NAMESPACE, metrics, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until

NAMESPACE = TEST_NAMESPACE
CRD_NAME = "servicegroups.purelb.io"
SERVICE_GROUP = "purelb.io/service-group"
STATUS_WRITES = "purelb_allocator_sg_status_writes_total"

pytestmark = pytest.mark.skipif(
    os.environ.get("PURELB_TEST_UPGRADE") != "1",
    reason=(
        "modifies the live ServiceGroup CRD (removes and restores the status "
        "subresource), which degrades status reporting cluster-wide while it "
        "runs. Set PURELB_TEST_UPGRADE=1 to opt in."
    ),
)


@pytest.fixture
def crd_without_status_subresource(cluster: Cluster):
    """Strip the status subresource from the live CRD, then restore it.

    This reproduces the ONE property of a v0.16.x CRD that causes the
    hazard. It is not a full v0.16.x schema -- that would also lack the
    v0.17.0 spec fields and fail for unrelated reasons, testing a
    different thing.
    """
    from kubernetes import client

    api = client.ApiextensionsV1Api()
    original = api.read_custom_resource_definition(CRD_NAME)
    # Deep-copied BEFORE mutation so the restore cannot be affected by it.
    saved = copy.deepcopy(original.spec.versions)

    stripped = []
    for version in original.spec.versions:
        v = copy.deepcopy(version)
        v.subresources = None
        stripped.append(v)
    original.spec.versions = stripped
    api.replace_custom_resource_definition(CRD_NAME, original)

    yield

    current = api.read_custom_resource_definition(CRD_NAME)
    current.spec.versions = saved
    api.replace_custom_resource_definition(CRD_NAME, current)

    # Verify the restore. Leaving a cluster without status reporting
    # because a teardown quietly failed would be a worse outcome than the
    # bug this test is about.
    back = api.read_custom_resource_definition(CRD_NAME)
    assert any(v.subresources and v.subresources.status is not None
               for v in back.spec.versions), (
        "FAILED TO RESTORE the ServiceGroup CRD's status subresource. The "
        "cluster is left without ServiceGroup status reporting; re-apply "
        "deployments/crds/purelb.io_servicegroups.yaml"
    )


def test_a_crd_without_a_status_subresource_breaks_status_but_not_allocation(
    cluster: Cluster, topo: topology.Topology, lb_service, allocator_metrics,
    crd_without_status_subresource,
):
    """Allocation keeps working; only status reporting is lost.

    Both halves matter. If allocation broke, an upgrading user would
    notice immediately and fix it. Because it does not, the symptom is
    just an empty column in `kubectl get sg`, which is easy to read as
    "nothing has been allocated yet".
    """
    subnet = topo.subnets[0]
    before = allocator_metrics()
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "upgrade-test", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "v4pools": [
                        {"aggregation": "default", "pool": subnet.test_pool,
                         "subnet": subnet.v4}
                    ]
                }
            },
        }
    )
    try:
        # Allocation is unaffected: this is the half that hides the problem.
        ips = lb_service("upgrade-svc", ["IPv4"],
                         annotations={SERVICE_GROUP: "upgrade-test"})
        assert ips, "allocation should be unaffected by the missing subresource"

        # Status stays empty.
        sg = cluster.get_cr("servicegroup", "upgrade-test") or {}
        assert not sg.get("status"), (
            f"status was written despite the subresource being absent: "
            f"{sg.get('status')}"
        )

        # And the failure is visible in the metric the release notes name.
        wait_until(
            lambda: (
                allocator_metrics().counter(STATUS_WRITES)
                - before.counter(STATUS_WRITES) > 0
            ) or None,
            timeout=90, interval=3.0,
            description="the allocator to attempt a status write",
        )
        after = allocator_metrics()
        failed = (after.counter(STATUS_WRITES) - before.counter(STATUS_WRITES)
                  - (after.counter(STATUS_WRITES, outcome="success")
                     - before.counter(STATUS_WRITES, outcome="success")))
        assert failed > 0, (
            f"{STATUS_WRITES} shows no failed writes, so an operator has no "
            f"signal that status reporting is broken. Deltas: "
            f"total {after.counter(STATUS_WRITES) - before.counter(STATUS_WRITES)}, "
            f"success {after.counter(STATUS_WRITES, outcome='success') - before.counter(STATUS_WRITES, outcome='success')}"
        )
    finally:
        cluster.delete_service(NAMESPACE, "upgrade-svc")
        cluster.delete_cr("servicegroup", "upgrade-test")


def test_applying_the_new_crd_restores_status_reporting(
    cluster: Cluster, topo: topology.Topology, lb_service, allocator_metrics,
):
    """The documented remedy actually works.

    The release notes tell an upgrading user to apply the CRDs themselves
    with --server-side. Documenting a remedy nobody has run is how you
    find out at the worst moment that it does not work.
    """
    subnet = topo.subnets[0]
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "upgrade-fixed", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "v4pools": [
                        {"aggregation": "default", "pool": subnet.test_pool,
                         "subnet": subnet.v4}
                    ]
                }
            },
        }
    )
    try:
        status = wait_until(
            lambda: (cluster.get_cr("servicegroup", "upgrade-fixed") or {}).get("status")
            or None,
            timeout=60, interval=2.0,
            description="status to be populated with the current CRD in place",
        )
        assert status.get("addresses"), f"status present but empty: {status}"
        assert "availableIPv4" in status, status
    finally:
        cluster.delete_cr("servicegroup", "upgrade-fixed")
