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
from typing import Dict, Iterator, List, Optional, Sequence

import pytest

from purelb_e2e import metrics, topology
from purelb_e2e.cluster import Cluster, utcnow
from purelb_e2e.nodes import Router, ssh
from purelb_e2e.wait import wait_until

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


@pytest.fixture(scope="session")
def topo(cluster: Cluster, node_ips: Dict[str, str]) -> topology.Topology:
    """Which nodes are on which subnets, and their IPv6 prefixes.

    Session-scoped: it costs one SSH per node and the answer cannot change
    during a run.
    """
    return topology.discover(node_ips)


@pytest.fixture(scope="session")
def lbnodeagent(cluster: Cluster) -> str:
    """The default LBNodeAgent, in local mode on the detected interface.

    Applied rather than assumed. reset-test-cluster.sh restores one, but a
    suite that depends on an object it did not assert the shape of will
    eventually run against a drifted one -- which is how the multi-interface
    tests left a `default` agent behind that pinned a single interface.
    """
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "LBNodeAgent",
            "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
            "spec": {"local": {"localInterface": "default", "dummyInterface": "kube-lb0"}},
        }
    )
    return "default"


@pytest.fixture(scope="session")
def default_servicegroup(cluster: Cluster, topo: topology.Topology, lbnodeagent: str) -> str:
    """The `default` ServiceGroup: one pool per subnet, per family.

    Built from discovered topology rather than checked in, because the
    pool has to sit inside a real node subnet to be announceable.
    """
    v4pools = [
        {"aggregation": "default", "pool": s.default_pool, "subnet": s.v4}
        for s in topo.subnets
    ]
    v6pools = [
        {"aggregation": "default", "pool": s.default_pool_v6, "subnet": s.v6}
        for s in topo.subnets
        if s.v6
    ]
    spec: Dict[str, object] = {"local": {"v4pools": v4pools}}
    if v6pools:
        spec["local"]["v6pools"] = v6pools  # type: ignore[index]
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
            "spec": spec,
        }
    )
    return "default"


@pytest.fixture
def lb_service(cluster: Cluster):
    """Factory for LoadBalancer Services, cleaned up in reverse order.

    Returns the allocated addresses, so the common "create it and find out
    what it got" is one call. Waiting is a predicate, not a sleep.
    """
    created: List[tuple] = []

    def make(
        name: str,
        families: Sequence[str] = ("IPv4",),
        namespace: str = "test",
        annotations: Optional[Dict[str, str]] = None,
        selector: Optional[Dict[str, str]] = None,
        policy: Optional[str] = None,
        wait: bool = True,
        timeout: float = 45.0,
        **spec_extra: object,
    ) -> List[str]:
        body = {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "name": name,
                "namespace": namespace,
                "annotations": {"purelb.io/service-group": "default", **(annotations or {})},
            },
            "spec": {
                "type": "LoadBalancer",
                "ipFamilyPolicy": policy or ("RequireDualStack" if len(families) > 1 else "SingleStack"),
                "ipFamilies": list(families),
                "selector": selector or {"app": "nginx"},
                "ports": [{"port": 80, "targetPort": 80}],
                **spec_extra,
            },
        }
        cluster.apply_service(body)
        created.append((namespace, name))
        if not wait:
            return []
        return wait_until(
            lambda: cluster.service_ingress_ips(namespace, name) or None,
            timeout=timeout,
            description=f"{namespace}/{name} to be allocated {'+'.join(families)}",
        )

    yield make

    for namespace, name in reversed(created):
        cluster.delete_service(namespace, name)


@pytest.fixture
def short_address_lifetime(cluster: Cluster, node_ips: Dict[str, str]):
    """Shorten the local address lifetime, then restore the default.

    Renewal happens at ValidLft/2 with a floor of 30s
    (announcer_local.go scheduleRenewal), so at the default 300s lifetime
    the first renewal is 150 SECONDS after the address is added. A test
    that waits less than that and concludes anything about renewal is
    measuring nothing -- it would pass identically with the renewal timer
    removed.

    Setting validLifetime to 60 puts the interval at its 30s floor, which
    is what makes a ~40s observation prove something. The bash suite
    worked this out for the multi-interface tests and wrote it down; this
    is the same lesson, applied where it belongs.

    Returns the renewal interval in seconds so a test can size its wait.
    """
    def apply(valid_lifetime: int = 60) -> float:
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "LBNodeAgent",
                "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
                "spec": {
                    "local": {
                        "localInterface": "default",
                        "dummyInterface": "kube-lb0",
                        "addressConfig": {
                            "localInterface": {
                                "validLifetime": valid_lifetime,
                                "preferredLifetime": valid_lifetime,
                            }
                        },
                    }
                },
            }
        )
        return max(valid_lifetime / 2.0, 30.0)

    yield apply

    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "LBNodeAgent",
            "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
            "spec": {"local": {"localInterface": "default", "dummyInterface": "kube-lb0"}},
        }
    )


@pytest.fixture
def subnet_servicegroup(cluster: Cluster):
    """A ServiceGroup with pools on ONE subnet, removed afterwards.

    This is how a test pins an address to a subnet. The obvious
    alternative -- purelb.io/addresses -- does not work for that:
    serviceAddresses() runs each comma-separated value through
    net.ParseIP, so it takes specific addresses and rejects a range
    outright, leaving the Service with no address at all.

    Uses the .230-.240 / b:: band, which does not overlap the default
    pool. Overlapping ranges are rejected by the allocator, so the wrong
    band fails in a way that reads as a product bug.
    """
    created: List[str] = []

    def make(name: str, subnet) -> str:
        spec: Dict[str, object] = {
            "local": {
                "v4pools": [
                    {"aggregation": "default", "pool": subnet.test_pool, "subnet": subnet.v4}
                ]
            }
        }
        if subnet.v6:
            spec["local"]["v6pools"] = [  # type: ignore[index]
                {"aggregation": "default", "pool": subnet.test_pool_v6, "subnet": subnet.v6}
            ]
        cluster.apply_cr(
            {
                "apiVersion": "purelb.io/v2",
                "kind": "ServiceGroup",
                "metadata": {"name": name, "namespace": cluster.purelb_namespace},
                "spec": spec,
            }
        )
        created.append(name)
        return name

    yield make

    for name in reversed(created):
        cluster.delete_cr("servicegroup", name)


@pytest.fixture
def tainted_nodes(cluster: Cluster):
    """Apply NoExecute taints and guarantee their removal.

    This is the whole A9 problem solved structurally. The bash suite
    applied `purelb-test=...:NoExecute` in three tests and had to untaint
    at SIX inline sites, one before every `fail` on the path -- and a fail
    raised inside a helper skipped them all, leaving a node permanently
    unschedulable for every later test and every later run. Here the
    teardown runs whatever the test does.
    """
    applied: List[tuple] = []

    def taint(node: str, key: str = "purelb-test", value: str = "failover") -> None:
        cluster.add_taint(node, key, value, "NoExecute")
        applied.append((node, key))

    yield taint

    for node, key in reversed(applied):
        cluster.remove_taint(node, key)


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
