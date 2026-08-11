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

"""Harness self-checks.

These test the harness, not PureLB. They exist because the harness is
about to become the thing that decides whether PureLB works, and the bash
suite it replaces was wrong in ways nobody noticed for a long time.

The parse/assertion cases need no cluster; the rest do.
"""

from __future__ import annotations

import pytest

from purelb_e2e import metrics
from purelb_e2e.wait import WaitTimeout, wait_until

SAMPLE = """\
# HELP purelb_x_total help text
# TYPE purelb_x_total counter
purelb_x_total{pool="default"} 7.0
purelb_x_total{pool="other"} 2.0
# TYPE purelb_gauge gauge
purelb_gauge 1.0
"""


# --------------------------------------------------------------- offline


def test_parses_labels_and_values():
    snap = metrics.Snapshot.parse(SAMPLE, source="unit")
    assert snap.value("purelb_x_total", pool="default") == 7.0
    assert snap.value("purelb_x_total", pool="other") == 2.0
    assert snap.value("purelb_gauge") == 1.0


def test_absent_and_zero_are_distinguishable():
    """`get` returns None for absent; `counter` folds it to 0.

    A metric missing entirely usually means the feature never
    initialised, which is a different bug from it being idle -- so the
    distinction is available where it matters, and hidden where it does
    not (delta arithmetic).
    """
    snap = metrics.Snapshot.parse(SAMPLE, source="unit")
    assert snap.get("purelb_absent_total") is None
    assert snap.counter("purelb_absent_total") == 0.0
    with pytest.raises(AssertionError):
        snap.value("purelb_absent_total")


def test_increase_assertions():
    before = metrics.Snapshot.parse("purelb_x_total 5.0\n", source="before")
    after = metrics.Snapshot.parse("purelb_x_total 7.0\n", source="after")

    assert metrics.assert_increased(before, after, "purelb_x_total") == 2.0

    # The bug this replaces: `> 0` was true here, because the counter was
    # already nonzero before the action.
    with pytest.raises(AssertionError, match="advanced by 0"):
        metrics.assert_increased(before, before, "purelb_x_total")
    with pytest.raises(AssertionError):
        metrics.assert_increased(before, after, "purelb_x_total", min_delta=3)


def test_not_increased_tolerates_history():
    """A pre-existing nonzero error count must not fail a later run.

    Asserting `== 0` is what made a legitimate ServiceGroup deletion
    poison every subsequent run.
    """
    before = metrics.Snapshot.parse('purelb_e_total{outcome="other"} 4.0\n', source="b")
    same = metrics.Snapshot.parse('purelb_e_total{outcome="other"} 4.0\n', source="a")
    worse = metrics.Snapshot.parse('purelb_e_total{outcome="other"} 5.0\n', source="a")

    metrics.assert_not_increased(before, same, "purelb_e_total", outcome="other")
    with pytest.raises(AssertionError):
        metrics.assert_not_increased(before, worse, "purelb_e_total", outcome="other")


def test_scrape_failure_raises_rather_than_returning_empty():
    """The A1 bug, made unrepresentable.

    There is no falsy return value a caller could guard on and thereby
    skip its assertions.
    """
    with pytest.raises(metrics.ScrapeError):
        metrics.scrape_url("http://127.0.0.1:1/metrics", timeout=1.0)


def test_wait_until_returns_value_and_times_out():
    calls = {"n": 0}

    def eventually():
        calls["n"] += 1
        return "ready" if calls["n"] >= 2 else None

    assert wait_until(eventually, timeout=5, interval=0.05) == "ready"

    with pytest.raises(WaitTimeout, match="never"):
        wait_until(lambda: None, timeout=0.2, interval=0.05, description="never")


def test_wait_until_surfaces_the_last_error():
    """A predicate that keeps raising must not be reported as a bare timeout."""
    def broken():
        raise RuntimeError("apiserver said no")

    with pytest.raises(WaitTimeout, match="apiserver said no"):
        wait_until(broken, timeout=0.2, interval=0.05)


# --------------------------------------------------------------- cluster


def test_connects_and_sees_nodes(cluster):
    names = cluster.node_names()
    assert names, "cluster reports no nodes"
    ips = cluster.node_ips()
    assert set(ips) == set(names), "every node must have an InternalIP"


def test_pod_resolution_is_exact(cluster):
    """A5: one node resolves to exactly one pod, by field selector."""
    for node in cluster.node_names():
        pod = cluster.pod_on_node(
            cluster.purelb_namespace, "component=lbnodeagent", node
        )
        assert pod is not None, f"no lbnodeagent pod on {node}"
        assert pod.spec.node_name == node


def test_allocator_metrics_scrape(allocator_metrics):
    snap = allocator_metrics()
    assert snap.get("purelb_k8s_client_config_loaded_bool") is not None, (
        "allocator exports no config_loaded metric; is this really PureLB?"
    )


def test_agent_metrics_scrape(cluster, agent_metrics):
    node = cluster.node_names()[0]
    snap = agent_metrics(node)
    assert snap.value("purelb_election_lease_healthy") == 1.0


def test_logs_are_windowed(cluster, log_window):
    """Reading with a window must not return the whole history."""
    windowed = cluster.component_logs("allocator", log_window)
    assert windowed, "no allocator pods found"
    for pod, text in windowed.items():
        # The window opened microseconds ago, so at most a couple of lines
        # can have landed. Unbounded output would mean since is ignored.
        assert len(text.splitlines()) < 50, (
            f"{pod}: window appears not to be applied ({len(text.splitlines())} lines)"
        )


@pytest.mark.requires("multi-node")
def test_capability_probe_agrees_with_the_cluster(capabilities, cluster):
    assert capabilities["multi-node"] == (len(cluster.node_names()) >= 2)


@pytest.mark.requires("router")
def test_router_reachable_and_faces_the_pool_subnet(router, cluster):
    assert router is not None
    iface = router.interface_for_subnet(cluster.node_ip(cluster.node_names()[0]))
    assert iface and iface != "lo"
