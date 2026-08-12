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

"""`kubectl purelb validate`, the documented pre-upgrade gate.

It is what the release notes tell an operator to run before upgrading,
and until now nothing ran it. A gate that has never been exercised is a
gate whose failure modes are unknown -- and the ones that matter are not
"does it crash" but "does it stay silent about a real problem", because
that is what an operator acts on.

So these tests are mostly about MISCONFIGURATION. Each creates a config
that PureLB will quietly not serve, and asserts validate says so and
exits non-zero. A validator that passes everything is worse than none:
it converts "I did not check" into "I checked and it is fine".

The binary is driven as a subprocess, exactly as an operator runs it,
and asserted on its JSON output and its exit code. Asserting on the
table would be asserting on formatting.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path
from typing import Dict, List, Optional, Tuple

import pytest

from purelb_e2e import TEST_NAMESPACE, topology
from purelb_e2e.cluster import Cluster

NAMESPACE = TEST_NAMESPACE

# Built by `make plugin` into the repo root.
PLUGIN = Path(__file__).resolve().parents[4] / "kubectl-purelb"


@pytest.fixture(scope="module")
def plugin() -> Path:
    if not PLUGIN.exists():
        pytest.skip(f"{PLUGIN} not built; run `make plugin`")
    return PLUGIN


def validate(plugin: Path, context: str, *extra: str) -> Tuple[int, dict]:
    """Run the real binary and return (exit code, parsed JSON).

    The exit code is half the assertion: this is a CI gate, so a
    validator that prints FAIL and exits 0 stops nothing.
    """
    proc = subprocess.run(  # noqa: S603
        [str(plugin), "validate", "--context", context, "-o", "json", *extra],
        capture_output=True, text=True, timeout=120,
    )
    try:
        return proc.returncode, json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise AssertionError(
            f"validate did not emit JSON (exit {proc.returncode}):\n"
            f"stdout: {proc.stdout[:400]!r}\nstderr: {proc.stderr[:400]!r}"
        ) from exc


def messages(report: dict, status: Optional[str] = None) -> List[str]:
    return [
        c["message"] for c in report.get("checks", [])
        if status is None or c["status"] == status
    ]


# --------------------------------------------------------------- baseline


def test_a_healthy_cluster_validates_clean(
    plugin: Path, kube_context: str, cluster: Cluster, default_servicegroup: str
):
    """Zero failures on a working config, and exit 0.

    The control for everything below. Without it, a validator that
    reported FAIL unconditionally would pass every other test here.
    """
    code, report = validate(plugin, kube_context)
    assert report["fail"] == 0, (
        f"a healthy cluster reported {report['fail']} failure(s): "
        f"{messages(report, 'FAIL')}"
    )
    assert report["pass"] > 0, f"nothing was actually checked: {report}"
    assert code == 0, f"exit {code} with no failures"


# ---------------------------------------------------- misconfigurations


def test_a_servicegroup_outside_the_install_namespace_is_reported(
    plugin: Path, kube_context: str, cluster: Cluster, topo: topology.Topology,
    allocator_metrics,
):
    """PureLB ignores it entirely, which is invisible without this check.

    A ServiceGroup in the wrong namespace is not rejected by the API
    server and produces no event -- it simply never takes effect, and an
    operator sees Services that stay pending for no stated reason. This
    is the check that names it, and the allocator counts it too.
    """
    subnet = topo.subnets[0]
    cluster.custom.create_namespaced_custom_object(
        "purelb.io", "v2", NAMESPACE, "servicegroups",
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "out-of-scope", "namespace": NAMESPACE},
            "spec": {
                "local": {
                    "v4pools": [
                        {"aggregation": "default", "pool": subnet.test_pool,
                         "subnet": subnet.v4}
                    ]
                }
            },
        },
    )
    try:
        code, report = validate(plugin, kube_context)
        assert report["fail"] >= 1, (
            f"a ServiceGroup in {NAMESPACE!r} was not reported: {report}"
        )
        assert any("not the PureLB install namespace" in m
                   for m in messages(report, "FAIL")), messages(report)
        assert code != 0, "a FAIL must be a non-zero exit or it gates nothing"

        # The allocator sees it too, and labels it by type.
        found = allocator_metrics()
        assert found.counter(
            "purelb_servicegroups_out_of_scope", namespace=NAMESPACE, type="local"
        ) >= 1, "purelb_servicegroups_out_of_scope did not count it"
    finally:
        cluster.custom.delete_namespaced_custom_object(
            "purelb.io", "v2", NAMESPACE, "servicegroups", "out-of-scope"
        )


def test_an_ambiguous_namespace_binding_is_reported(
    plugin: Path, kube_context: str, cluster: Cluster, topo: topology.Topology
):
    """Two groups serve a namespace and neither is its default.

    The allocator resolves this by falling back and warning, so nothing
    breaks -- which is exactly why an operator needs telling. validate
    names the candidates so the fix is obvious.
    """
    subnet = topo.subnets[0]
    for name, pool in (("ambig-a", subnet.tenant_pool), ("ambig-b", subnet.tenant_pool_b)):
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {
                    "namespaces": ["test-tenant"],
                    "local": {
                        "v4pools": [
                            {"aggregation": "default", "pool": pool, "subnet": subnet.v4}
                        ]
                    },
                },
            }
        )
    try:
        code, report = validate(plugin, kube_context)
        assert any("exactly one must" in m for m in messages(report)), (
            f"an ambiguous binding was not reported: {messages(report)}"
        )
        assert any("is served by" in m for m in messages(report)), messages(report)
    finally:
        for name in ("ambig-a", "ambig-b"):
            cluster.delete_cr("servicegroup", name)


def test_overlapping_pool_ranges_are_reported(
    plugin: Path, kube_context: str, cluster: Cluster, topo: topology.Topology
):
    """Two groups claiming the same addresses.

    The allocator refuses the second group outright, so the symptom is a
    ServiceGroup that exists and does nothing. validate says which pair
    collide, which is the part that saves the time.
    """
    subnet = topo.subnets[0]
    shared = subnet.test_pool
    for name in ("overlap-a", "overlap-b"):
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": {
                    "local": {
                        "v4pools": [
                            {"aggregation": "default", "pool": shared, "subnet": subnet.v4}
                        ]
                    }
                },
            }
        )
    try:
        code, report = validate(plugin, kube_context)
        assert any("Overlapping ranges" in m for m in messages(report)), (
            f"two groups sharing {shared} were not reported: {messages(report)}"
        )
        assert code != 0
    finally:
        for name in ("overlap-a", "overlap-b"):
            cluster.delete_cr("servicegroup", name)


def test_a_pool_no_node_can_announce_is_reported(
    plugin: Path, kube_context: str, cluster: Cluster
):
    """A local pool whose subnet matches no node.

    This is the configuration that allocates addresses nobody can
    announce -- the e2e suite asserts the resulting silence elsewhere.
    Here the point is that an operator can find out BEFORE creating a
    Service, which is what a pre-upgrade gate is for.
    """
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "unreachable-pool", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "v4pools": [
                        {"aggregation": "default", "pool": "10.254.0.10-10.254.0.20",
                         "subnet": "10.254.0.0/24"}
                    ]
                }
            },
        }
    )
    try:
        code, report = validate(plugin, kube_context)
        assert any("not covered by any node" in m for m in messages(report)), (
            f"a pool no node can announce was not reported: {messages(report)}"
        )
    finally:
        cluster.delete_cr("servicegroup", "unreachable-pool")


def test_a_node_selector_matching_nothing_is_reported(
    plugin: Path, kube_context: str, cluster: Cluster
):
    """An LBNodeAgent whose selector matches no node has no effect.

    Silently. The CR exists, `kubectl get lbnodeagent` shows it, and it
    configures nothing -- which is the same observable as a typo in a
    label.
    """
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "LBNodeAgent",
            "metadata": {"name": "selects-nothing", "namespace": cluster.purelb_namespace},
            "spec": {
                "nodeSelector": {"matchLabels": {"purelb-nonexistent-label": "nope"}},
                "local": {"localInterface": "default", "dummyInterface": "kube-lb0"},
            },
        }
    )
    try:
        code, report = validate(plugin, kube_context)
        assert any("matches 0 of" in m for m in messages(report)), (
            f"a selector matching no node was not reported: {messages(report)}"
        )
    finally:
        cluster.delete_cr("lbnodeagent", "selects-nothing")


def test_strict_mode_turns_a_warning_into_a_failure(
    plugin: Path, kube_context: str, cluster: Cluster, topo: topology.Topology
):
    """--strict is what makes this usable in CI.

    A warning an operator can wave through interactively is useless in a
    pipeline. This uses a REAL warning -- a pool whose subnet no node
    carries, which is a WARN because the config may be intentional on a
    cluster that has not been built out yet -- because a --strict that
    only escalates hypothetical warnings escalates nothing.

    Note the pair of assertions. Without --strict the same config must
    exit 0, or the flag would be indistinguishable from the default and
    this test would pass against a build that failed on everything.
    """
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "strict-warn", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "v4pools": [
                        {"aggregation": "default", "pool": "10.253.0.10-10.253.0.20",
                         "subnet": "10.253.0.0/24"}
                    ]
                }
            },
        }
    )
    try:
        lenient_code, lenient = validate(plugin, kube_context)
        assert lenient["warn"] >= 1, (
            f"expected a warning for a pool no node covers: {messages(lenient)}"
        )
        assert lenient["fail"] == 0, (
            f"this should WARN, not FAIL -- a pool on a subnet no node carries "
            f"may be deliberate: {messages(lenient, 'FAIL')}"
        )
        assert lenient_code == 0, (
            "without --strict a warning must not fail the command, or the flag "
            "changes nothing"
        )

        strict_code, _ = validate(plugin, kube_context, "--strict")
        assert strict_code != 0, (
            f"--strict exited 0 with {lenient['warn']} warning(s), so it cannot "
            f"gate a pipeline"
        )
    finally:
        cluster.delete_cr("servicegroup", "strict-warn")
