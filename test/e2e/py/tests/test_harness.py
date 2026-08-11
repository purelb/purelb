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

import textwrap

import pytest

from purelb_e2e import announcing, dualrun, metrics
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


def test_counter_matches_a_label_subset():
    """A test must not have to name labels it does not care about.

    purelb_allocator_sidecar_rpc_total carries socket, method AND code.
    Requiring the full set makes an assertion match nothing the moment a
    label is added -- indistinguishable from the counter not moving, so it
    reads as "the RPC never happened".
    """
    snap = metrics.Snapshot.parse(
        'r_total{socket="/a.sock",method="Allocate",code="OK"} 3.0\n'
        'r_total{socket="/b.sock",method="Allocate",code="OK"} 4.0\n'
        'r_total{socket="/a.sock",method="Release",code="OK"} 1.0\n',
        source="unit",
    )
    assert snap.counter("r_total", method="Allocate", code="OK") == 7.0
    assert snap.counter("r_total", code="OK") == 8.0
    assert snap.counter("r_total") == 8.0
    assert snap.counter("r_total", method="Stats") == 0.0
    # Exact lookups stay exact.
    assert snap.get("r_total", method="Allocate") is None


def test_counter_does_not_match_a_label_value_by_prefix():
    """pool="default" must not aggregate pool="default-v6".

    A substring test on the rendered key would, and the resulting
    over-count looks exactly like a passing assertion.
    """
    snap = metrics.Snapshot.parse(
        'p_total{pool="default"} 2.0\np_total{pool="default-v6"} 5.0\n', source="unit"
    )
    assert snap.counter("p_total", pool="default") == 2.0


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


def test_increase_on_an_unknown_metric_name_is_a_typo_not_a_flat_counter():
    """A metric name that exists nowhere must not read as "did not move".

    Both render as 0 -> 0, and the difference matters: one means the
    product did nothing, the other means the test asked the wrong
    question. This cost real time -- an assertion on
    purelb_allocator_allocation_rejected_total (the subsystem is
    address_pool) reported a working feature as broken.
    """
    before = metrics.Snapshot.parse(
        'purelb_address_pool_allocation_rejected_total{reason="x"} 1.0\n', source="b"
    )
    after = metrics.Snapshot.parse(
        'purelb_address_pool_allocation_rejected_total{reason="x"} 1.0\n', source="a"
    )
    # Right name, genuinely flat: the ordinary delta failure.
    with pytest.raises(AssertionError, match="advanced by 0"):
        metrics.assert_increased(before, after, "purelb_address_pool_allocation_rejected_total")
    # Wrong name: says so, and suggests the real one.
    with pytest.raises(AssertionError, match="no metric named"):
        metrics.assert_increased(before, after, "purelb_allocator_allocation_rejected_total")


def test_nearest_names_help_find_a_renamed_metric():
    snap = metrics.Snapshot.parse(
        'purelb_address_pool_allocation_rejected_total{reason="x"} 1.0\n', source="s"
    )
    assert not snap.has_series("purelb_allocator_allocation_rejected_total")
    assert snap.has_series("purelb_address_pool_allocation_rejected_total")
    assert "purelb_address_pool_allocation_rejected_total" in snap.nearest(
        "purelb_allocator_allocation_rejected_total"
    )


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


# ------------------------------------------------------------ announcing
#
# This parser has to agree with ParseAnnouncements in
# pkg/apis/purelb/v2/announcing.go. If it does not, a failing test means
# the harness is wrong rather than the product -- the worst kind of test,
# because the natural response is to "fix" the product.


def test_announcing_splits_on_first_and_last_comma():
    """Interface names may legally contain commas.

    The product splits on the FIRST comma for the node and the LAST for
    the IP, leaving everything between as the interface. A naive
    split(",") would disagree, and disagree silently.
    """
    got = announcing.parse("node-a,eth0,192.0.2.10 node-b,br0,x,192.0.2.11")
    assert [(a.node, a.interface, a.ip) for a in got] == [
        ("node-a", "eth0", "192.0.2.10"),
        ("node-b", "br0,x", "192.0.2.11"),
    ]


def test_announcing_drops_malformed_entries_rather_than_raising():
    """Leniency is the product's behaviour, so it is the test's too.

    A malformed annotation must not stall reconciliation, so the product
    drops bad entries. The legacy one-field ("kube-lb0") and two-field
    ("node,iface") forms are exactly what this drops on upgrade.
    """
    value = "kube-lb0 node,iface node-a,eth0,not-an-ip ,eth0,192.0.2.1 node-b,,192.0.2.2 good,eth0,192.0.2.3"
    assert [a.node for a in announcing.parse(value)] == ["good"]
    assert announcing.parse("") == []
    assert announcing.parse(None) == []


def test_announcing_is_keyed_by_ip():
    """The IP is the slot key, which is the v0.17.0 fix.

    by_ip collapsing shorter than parse is how a test detects that the
    append-only bug has returned: entries accumulating for one address.
    """
    value = "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.11"
    assert announcing.announcer_of(value, "192.0.2.10") == "node-a"
    assert announcing.announcer_of(value, "192.0.2.11") == "node-b"
    assert announcing.announcer_of(value, "192.0.2.99") is None

    stale = "node-a,eth0,192.0.2.10 node-b,eth0,192.0.2.10"
    assert len(announcing.parse(stale)) == 2
    assert len(announcing.by_ip(stale)) == 1, "two entries for one IP must collapse"


def test_announcing_has_node_is_entry_wise():
    """Substring matching cannot tell purelb2-1 from purelb2-10."""
    value = "purelb2-10,eth0,192.0.2.10"
    assert announcing.has_node(value, "purelb2-10")
    assert not announcing.has_node(value, "purelb2-1")


def test_announcing_normalises_ipv6():
    """Comparisons are on parsed addresses, not on text.

    2001:DB8::0:1 and 2001:db8::1 are the same address, and an assertion
    that compared strings would call a correct annotation wrong.
    """
    value = "node-a,eth0,2001:DB8::0:1"
    assert announcing.announcer_of(value, "2001:db8::1") == "node-a"


# --------------------------------------------------------------- dualrun
#
# The dual-run comparison is what licenses deleting a bash suite, so it is
# tested harder than the thing it compares. Each case below is a way the
# port could lose coverage while every harness still reported success.


BASH_LOG = (
    "\x1b[0;33m→\x1b[0m starting\n"
    "\x1b[0;32m✓ PASS:\x1b[0m allocator has servicegroups/status RBAC\n"
    "\x1b[0;32m✓ PASS:\x1b[0m service allocated 172.30.250.201\n"
    "\x1b[0;36m     \x1b[0m some detail line\n"
)

JUNIT = textwrap.dedent("""\
    <?xml version="1.0" encoding="utf-8"?>
    <testsuites><testsuite name="pytest" tests="4">
      <testcase classname="tests.test_ipam" name="test_rbac" time="0.1"/>
      <testcase classname="tests.test_ipam" name="test_allocates" time="0.2"/>
      <testcase classname="tests.test_ipam" name="test_skipped" time="0.0">
        <skipped type="pytest.skip" message="cluster lacks capability: ipv6"/>
      </testcase>
      <testcase classname="tests.test_ipam.TestGrouped" name="test_in_class" time="0.1"/>
    </testsuite></testsuites>
""")


def _write_map(tmp_path, assertions: str):
    path = tmp_path / "map.yaml"
    path.write_text(
        "ipam:\n"
        "  script: test/e2e/ipam-external/test-ipam-external.sh\n"
        "  assertions:\n" + assertions
    )
    return dualrun.load_map(path)["ipam"]


def test_dualrun_strips_ansi_and_separates_verdicts():
    run = dualrun.parse_bash_output(
        BASH_LOG + "\x1b[0;31m✗ FAIL:\x1b[0m VIP unreachable\n", exit_code=1
    )
    assert run.passed == (
        "allocator has servicegroups/status RBAC",
        "service allocated 172.30.250.201",
    )
    assert run.failed == ("VIP unreachable",)
    assert not run.completed


def test_dualrun_junit_node_ids_include_classes(tmp_path):
    path = tmp_path / "j.xml"
    path.write_text(JUNIT)
    verdicts = dualrun.parse_junit(path)
    assert verdicts["tests/test_ipam.py::test_rbac"] == "passed"
    assert verdicts["tests/test_ipam.py::test_skipped"] == "skipped"
    # A capitalised trailing part is a class, not a directory. Getting this
    # wrong would report a mapped test as "did not run".
    assert verdicts["tests/test_ipam.py::TestGrouped::test_in_class"] == "passed"


def test_dualrun_agrees_when_both_pass(tmp_path):
    suite = _write_map(
        tmp_path,
        '    "allocator has * RBAC": tests/test_ipam.py::test_rbac\n'
        '    "service allocated *": tests/test_ipam.py::test_allocates\n',
    )
    j = tmp_path / "j.xml"
    j.write_text(JUNIT)
    rep = dualrun.compare(suite, dualrun.parse_bash_output(BASH_LOG, 0), dualrun.parse_junit(j))
    assert rep.clean, dualrun.format_report(rep)
    assert len(rep.agreed) == 2
    # Not an error, but surfaced: tests bash never had.
    assert any("test_skipped" in p for p in rep.pytest_only)


def test_dualrun_unmapped_bash_assertion_fails(tmp_path):
    """The guarantee that makes the port safe.

    Deleting a bash assertion's pytest counterpart -- or never writing it
    -- must not be able to produce a clean run.
    """
    suite = _write_map(tmp_path, '    "allocator has * RBAC": tests/test_ipam.py::test_rbac\n')
    j = tmp_path / "j.xml"
    j.write_text(JUNIT)
    rep = dualrun.compare(suite, dualrun.parse_bash_output(BASH_LOG, 0), dualrun.parse_junit(j))
    assert not rep.clean
    assert rep.unmapped == ["service allocated 172.30.250.201"]


def test_dualrun_skip_is_not_agreement(tmp_path):
    """A pytest skip means the assertion was not made.

    bash passing while its replacement skipped is a coverage regression.
    Folding skip into success is how the bash suite's 17 untallied
    conditional skips went unnoticed for so long.
    """
    suite = _write_map(
        tmp_path,
        '    "allocator has * RBAC": tests/test_ipam.py::test_rbac\n'
        '    "service allocated *": tests/test_ipam.py::test_skipped\n',
    )
    j = tmp_path / "j.xml"
    j.write_text(JUNIT)
    rep = dualrun.compare(suite, dualrun.parse_bash_output(BASH_LOG, 0), dualrun.parse_junit(j))
    assert not rep.clean
    assert any("test_skipped skipped" in d for d in rep.disagreed)


def test_dualrun_mapped_test_that_never_ran_fails(tmp_path):
    suite = _write_map(
        tmp_path,
        '    "allocator has * RBAC": tests/test_ipam.py::test_rbac\n'
        '    "service allocated *": tests/test_ipam.py::test_typo\n',
    )
    j = tmp_path / "j.xml"
    j.write_text(JUNIT)
    rep = dualrun.compare(suite, dualrun.parse_bash_output(BASH_LOG, 0), dualrun.parse_junit(j))
    assert not rep.clean
    assert any("test_typo" in m for m in rep.missing_nodes)


def test_dualrun_incomplete_bash_run_is_fatal(tmp_path):
    """An aborted bash run cannot be partially interpreted.

    fail() exits, so the assertions after it are silent rather than
    failing. Comparing against that silence would report a flood of
    disagreements that say nothing about the port.
    """
    suite = _write_map(tmp_path, '    "allocator has * RBAC": tests/test_ipam.py::test_rbac\n')
    j = tmp_path / "j.xml"
    j.write_text(JUNIT)
    bash = dualrun.parse_bash_output(BASH_LOG, exit_code=1)
    rep = dualrun.compare(suite, bash, dualrun.parse_junit(j))
    assert not rep.clean
    assert rep.fatal and "did not complete" in rep.fatal[0]


def test_dualrun_first_matching_pattern_wins(tmp_path):
    suite = _write_map(
        tmp_path,
        '    "service allocated 172.30.250.*": tests/test_ipam.py::test_allocates\n'
        '    "service allocated *": tests/test_ipam.py::test_rbac\n',
    )
    entry = dualrun.match_entry("service allocated 172.30.250.201", suite.entries)
    assert entry is not None and entry.nodes == ("tests/test_ipam.py::test_allocates",)


def test_dualrun_rejects_a_mapping_to_nothing(tmp_path):
    """Mapping an assertion to [] would silently retire it."""
    with pytest.raises(dualrun.MappingError, match="maps to nothing"):
        _write_map(tmp_path, '    "allocator has * RBAC": []\n')


def test_dualrun_rejects_an_unquoted_non_string_pattern(tmp_path):
    """Patterns must be strings, and the file says to quote them.

    YAML catches the two dangerous cases itself -- an unquoted glob
    starting with `*` is an alias reference, and an unquoted pattern
    containing `: ` splits into a nested mapping -- both loudly. What it
    does NOT catch is a pattern that happens to parse as a scalar of
    another type, which would then never match any assertion message.
    """
    with pytest.raises(dualrun.MappingError, match="not a string"):
        _write_map(tmp_path, "    12345: tests/test_ipam.py::test_rbac\n")


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
