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

"""Fixtures and capability gating.

Two things the bash suite could not do:

* Teardown STACKS. Each suite could register only one EXIT trap, so a
  suite that set its own silently replaced common.sh's and leaked every
  ServiceGroup it had created. Here every fixture owns its own cleanup
  and pytest runs all of them.

* Skips are COUNTED. The bash suite had 17+ conditional skips that each
  printed an `info` line and were never tallied, so a run could exercise
  a fraction of the suite and still end green. `--require` turns a
  missing capability into a failure for release runs.
"""

from __future__ import annotations

import datetime as _dt
import os
from typing import Dict, Iterator, List

import pytest

from purelb_e2e import metrics
from purelb_e2e.cluster import Cluster, utcnow
from purelb_e2e.nodes import Router, ssh

CAPABILITIES = ("multi-subnet", "ipv6", "dual-homed", "router", "multi-node")


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption("--context", action="store", default=os.environ.get("CONTEXT"),
                     help="kubectl context to test against (required)")
    parser.addoption("--purelb-namespace", action="store", default="purelb-system")
    parser.addoption("--router-host", action="store", default=os.environ.get("ROUTER_HOST"),
                     help="upstream router for on-the-wire assertions")
    parser.addoption("--require", action="store", default="",
                     help="comma-separated capabilities that MUST be present; "
                          "a missing one fails the run instead of skipping")


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line(
        "markers",
        "requires(*caps): skip unless the cluster has these capabilities "
        f"(one of {', '.join(CAPABILITIES)})",
    )


@pytest.fixture(scope="session")
def kube_context(pytestconfig: pytest.Config) -> str:
    ctx = pytestconfig.getoption("--context")
    if not ctx:
        pytest.fail(
            "--context is required. There is deliberately no default: "
            "defaulting to the current context is how a destructive suite "
            "eventually runs against the wrong cluster."
        )
    return ctx


@pytest.fixture(scope="session")
def cluster(kube_context: str, pytestconfig: pytest.Config) -> Cluster:
    return Cluster(
        context=kube_context,
        purelb_namespace=pytestconfig.getoption("--purelb-namespace"),
    )


@pytest.fixture(scope="session")
def node_ips(cluster: Cluster) -> Dict[str, str]:
    return cluster.node_ips()


@pytest.fixture(scope="session")
def router(pytestconfig: pytest.Config) -> Router | None:
    host = pytestconfig.getoption("--router-host")
    if not host:
        return None
    r = Router(host=host)
    return r if r.available() else None


@pytest.fixture(scope="session")
def capabilities(cluster: Cluster, node_ips: Dict[str, str], router: Router | None) -> Dict[str, bool]:
    """Probe what this cluster can actually exercise.

    Discovered once and reported, so "green" and "green having tested
    everything" are distinguishable.
    """
    subnets: set[str] = set()
    v6 = False
    dual_homed = False
    for name, ip in node_ips.items():
        subnets.add(".".join(ip.split(".")[:3]))
        try:
            out = ssh(ip, "ip -br addr show", timeout=20)
        except Exception:  # noqa: BLE001 - a node we cannot reach limits capability, not the run
            continue
        if ":" in out and "inet6" not in out:
            pass
        v6 = v6 or any(
            tok.count(":") >= 2 and not tok.startswith("fe80")
            for line in out.splitlines() for tok in line.split() if "/" in tok
        )
        phys = [
            line.split()[0]
            for line in out.splitlines()
            if line.split() and line.split()[0].startswith("eth")
        ]
        if len(phys) > 1:
            dual_homed = True

    caps = {
        "multi-subnet": len(subnets) >= 2,
        "ipv6": v6,
        "dual-homed": dual_homed,
        "router": router is not None,
        "multi-node": len(node_ips) >= 2,
    }
    return caps


@pytest.fixture(autouse=True)
def _capability_gate(request: pytest.FixtureRequest) -> None:
    marker = request.node.get_closest_marker("requires")
    if marker is None:
        return
    caps = request.getfixturevalue("capabilities")
    required = [c.strip() for c in (request.config.getoption("--require") or "").split(",") if c.strip()]
    for cap in marker.args:
        if cap not in CAPABILITIES:
            pytest.fail(f"unknown capability {cap!r}; known: {CAPABILITIES}")
        if caps.get(cap):
            continue
        if cap in required:
            pytest.fail(
                f"capability {cap!r} was required via --require but this "
                f"cluster does not have it, so this test cannot run"
            )
        pytest.skip(f"cluster lacks capability: {cap}")


@pytest.fixture
def log_window() -> _dt.datetime:
    """Opens a log window at the start of the test.

    Every log assertion is scoped to output produced after this moment.
    Without it, assertions matched lines emitted by earlier tests on other
    nodes.
    """
    return utcnow()


@pytest.fixture(scope="session")
def allocator_metrics(cluster: Cluster):
    """Callable returning a fresh allocator scrape.

    Returns a function rather than a value so a test can take a before and
    an after and assert on the delta. Session-scoped for the same reason:
    it holds no per-test state, and a module-scoped baseline fixture
    cannot consume a function-scoped one.
    """
    def _scrape() -> metrics.Snapshot:
        pods = cluster.pods(cluster.purelb_namespace, "component=allocator")
        if not pods:
            raise metrics.ScrapeError("no allocator pod found")
        # Via the apiserver proxy: the allocator is in the pod network and
        # a workstation cannot route to it. lbnodeagent is hostNetwork and
        # is scraped directly.
        return metrics.scrape_pod_via_apiserver(
            cluster.core, cluster.purelb_namespace, pods[0].metadata.name
        )
    return _scrape


@pytest.fixture(scope="session")
def agent_metrics(node_ips: Dict[str, str]):
    """Callable returning a fresh lbnodeagent scrape for a named node."""
    def _scrape(node: str) -> metrics.Snapshot:
        if node not in node_ips:
            raise AssertionError(f"unknown node {node!r}; known: {sorted(node_ips)}")
        return metrics.scrape_node(node_ips[node])
    return _scrape


@pytest.fixture
def created_service_groups(cluster: Cluster) -> Iterator[List[str]]:
    """Track ServiceGroups a test creates and remove exactly those.

    The bash fallback listed the cluster and deleted everything except
    'default', which also destroyed resources a human had made.
    """
    names: List[str] = []
    yield names
    for name in reversed(names):
        cluster.delete_cr("servicegroup", name)


def pytest_terminal_summary(terminalreporter, exitstatus, config) -> None:  # noqa: ANN001
    """Report skips prominently.

    A run that skipped half the suite and printed 'passed' is the
    suite-level version of an assertion that passes when it cannot observe.
    """
    skipped = terminalreporter.stats.get("skipped", [])
    if not skipped:
        return
    terminalreporter.write_sep("=", "SKIPPED - this run did NOT test the following")
    for report in skipped:
        reason = report.longrepr[2] if isinstance(report.longrepr, tuple) else report.longrepr
        terminalreporter.write_line(f"  {report.nodeid}: {reason}")
    terminalreporter.write_line(
        f"\n  {len(skipped)} test(s) skipped. Use --require to make missing "
        f"capabilities fail instead."
    )
