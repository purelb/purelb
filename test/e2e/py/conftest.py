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

from purelb_e2e import backend, metrics, topology
from purelb_e2e.cluster import Cluster, utcnow
from purelb_e2e.nodes import Router, ssh
from purelb_e2e.wait import wait_until

CAPABILITIES = ("multi-subnet", "ipv6", "dual-homed", "router", "multi-node")

# Filled in by the capabilities probe; read by --report.
_detected_capabilities: Dict[str, bool] = {}


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption("--context", action="store", default=os.environ.get("CONTEXT"),
                     help="kubectl context to test against (required)")
    parser.addoption("--purelb-namespace", action="store", default="purelb-system")
    parser.addoption("--router-host", action="store", default=os.environ.get("ROUTER_HOST"),
                     help="upstream router for on-the-wire assertions")
    parser.addoption("--require", action="store", default="",
                     help="comma-separated capabilities that MUST be present; "
                          "a missing one fails the run instead of skipping")
    parser.addoption("--show-tests", action="store_true", default=False,
                     help="print every test with its result and what it checked, "
                          "instead of only the progress dots")
    parser.addoption("--report", action="store", default=None, metavar="PATH",
                     help="write a plain-text report of every test, its result "
                          "and any failure detail, to PATH")


_CONFIG: Dict[str, object] = {}


def pytest_configure(config: pytest.Config) -> None:
    _CONFIG["config"] = config
    if config.getoption("--show-tests") and config.option.verbose < 1:
        config.option.verbose = 1
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
    # Recorded for --report: a skip list is only actionable next to the
    # reason the capability was absent.
    _detected_capabilities.update(caps)
    return caps


@pytest.fixture(autouse=True)
def _capability_gate(request: pytest.FixtureRequest) -> None:
    # iter_markers, NOT get_closest_marker. `requires` is applied at both
    # module and function level, and get_closest_marker returns only the
    # nearest one -- so a test that declared its own requirement silently
    # DISCARDED its module's. test_router_bgp's module-level
    # requires("router") was lost by the two tests that add
    # requires("multi-node"): with no --router-host they ran anyway and
    # failed inside the router fixture with
    # `'NoneType' object has no attribute 'nexthops'`, which reads as a
    # BGP defect rather than a missing capability. Five tests across three
    # modules were affected.
    #
    # Capabilities are a union: a test needs everything asked of it by any
    # scope, so the markers accumulate rather than override.
    wanted: List[str] = []
    for marker in request.node.iter_markers("requires"):
        for arg in marker.args:
            if arg not in wanted:
                wanted.append(arg)
    if not wanted:
        return
    caps = request.getfixturevalue("capabilities")
    required = [c.strip() for c in (request.config.getoption("--require") or "").split(",") if c.strip()]
    for cap in wanted:
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
                # The service-group annotation is injected ONLY when the
                # caller says nothing about annotations. Injecting it
                # unconditionally made every Service explicitly name the
                # "default" group, which silently defeats namespace
                # binding -- an annotation outranks the namespace's
                # default, so the namespace-scoping tests were measuring
                # the annotation path while believing they were
                # measuring the binding. Passing annotations={} now means
                # exactly that: no annotations, so PureLB resolves the
                # group itself.
                "annotations": (
                    {"purelb.io/service-group": "default"}
                    if annotations is None
                    else dict(annotations)
                ),
            },
            "spec": {
                "type": "LoadBalancer",
                "ipFamilyPolicy": policy or ("RequireDualStack" if len(families) > 1 else "SingleStack"),
                "ipFamilies": list(families),
                "selector": selector or {"app": backend.LABEL},
                # The echo backend listens on 8080; the Service still
                # publishes 80, so nothing outside this line cares.
                #
                # Taken from backend rather than written out, because the
                # switch off nginx updated the selector here and left the
                # port at 80 in five other modules. kube-proxy DNATs to a
                # port nothing listens on, the pod RSTs, and the test fails
                # as "connection refused" -- which reads as an announcement
                # problem and is not one.
                "ports": [{"port": 80, "targetPort": backend.PORT}],
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
def pinned_backend(cluster: Cluster):
    """An echo backend Deployment pinned to one node, removed afterwards.

    Combined with purelb.io/node-affinity: service-endpoints, this is how
    a test makes a SPECIFIC node the announcer. Without it the election
    picks any node on the address's subnet, which is correct behaviour and
    useless for asserting that a particular node announced.
    """
    created: List[tuple] = []

    def make(name: str, node: str, namespace: str = "test") -> str:
        labels = {"app": name}
        cluster.apps.create_namespaced_deployment(
            namespace,
            {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {"name": name, "namespace": namespace},
                "spec": {
                    "replicas": 1,
                    "selector": {"matchLabels": labels},
                    "template": {
                        "metadata": {"labels": labels},
                        "spec": backend.pod_spec(node),
                    },
                },
            },
        )
        created.append((namespace, name))
        cluster.wait_rollout(namespace, name, timeout=120)
        return name

    yield make

    for namespace, name in reversed(created):
        try:
            cluster.apps.delete_namespaced_deployment(name, namespace)
        except Exception:  # noqa: BLE001 - teardown must not mask a test failure
            pass


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


# --------------------------------------------------------------- reporting
#
# pytest's own output is written for the person who wrote the tests: dots,
# or node ids like `test_local_multipool.py::test_a_vip_whose_subnet_no_node_
# carries_is_announced_by_nobody[IPv6]`. An operator running this before an
# upgrade wants a different thing -- what was checked, what the verdict was,
# and a file they can attach to a ticket. That is what --show-tests and
# --report produce.
#
# Descriptions come from each test's docstring first line, falling back to a
# humanised function name. 142 of 163 tests have a docstring; the rest are
# named well enough to read.

_descriptions: Dict[str, str] = {}
_results: Dict[str, Dict[str, object]] = {}


def _describe(item: pytest.Item) -> str:
    doc = getattr(getattr(item, "function", None), "__doc__", None)
    if doc and doc.strip():
        return doc.strip().splitlines()[0].strip()
    name = item.name.split("[")[0]
    return name.removeprefix("test_").replace("_", " ")


def _preflight_problems(config: pytest.Config, items: List[pytest.Item]) -> List[str]:
    """What is unreachable, checked once, before any test runs.

    Reports EVERY problem rather than the first. A bad context and a bad
    router together used to present as 158 identical cluster errors with
    no mention of the router at all, so you fixed the context, waited out
    a 23-minute run, and only then discovered the second fault.

    Only probes what the selected tests actually need: the harness's pure
    unit tests take no `cluster` fixture and must keep running with no
    cluster at all.
    """
    problems: List[str] = []
    needs = {name for item in items for name in getattr(item, "fixturenames", ())}

    if "cluster" in needs:
        ctx = config.getoption("--context")
        if not ctx:
            problems.append(
                "no kubectl context given. Pass --context <name> or export CONTEXT."
            )
        else:
            try:
                Cluster(context=ctx).nodes()
            except Exception as exc:  # noqa: BLE001 - any failure here is fatal
                problems.append(
                    f"cannot reach the cluster for context {ctx!r}:\n"
                    f"      {type(exc).__name__}: {str(exc).splitlines()[0][:200]}\n"
                    f"      Check `kubectl --context {ctx} get nodes`."
                )

    # A router named on the command line is REQUESTED, and a requested
    # capability that is missing is an error. Without this, pointing
    # --router-host at a dead host skipped 22 tests and exited 0 -- a green
    # run, indistinguishable from not passing the flag. The preflight that
    # was supposed to catch it was itself gated on the router capability,
    # so it skipped exactly when it should have fired.
    host = config.getoption("--router-host")
    if host and "router" in needs:
        if not Router(host=host).available():
            problems.append(
                f"--router-host {host} was given but the router is not usable:\n"
                f"      SSH must work and `tcpdump` must be present on it.\n"
                f"      Check `ssh {host} command -v tcpdump`.\n"
                f"      To run without the router modules instead, drop the flag\n"
                f"      (and unset ROUTER_HOST) -- 22 tests will then skip."
            )
    return problems


# trylast so this runs AFTER pytest's own -k/-m deselection: `items` must be
# the tests that will actually run. Without it, `-k` down to two pure unit
# tests still probed the cluster, because the unfiltered list contained tests
# that wanted one.
@pytest.hookimpl(trylast=True)
def pytest_collection_modifyitems(config: pytest.Config, items: List[pytest.Item]) -> None:
    for item in items:
        _descriptions[item.nodeid] = _describe(item)

    problems = _preflight_problems(config, items)
    if problems:
        lines = ["", "PREFLIGHT FAILED - nothing was tested.", ""]
        lines += [f"  {i}. {p}" for i, p in enumerate(problems, start=1)]
        n = len(items)
        lines += ["", f"{n} test{'' if n == 1 else 's'} collected, none run.", ""]
        pytest.exit("\n".join(lines), returncode=pytest.ExitCode.USAGE_ERROR)


def pytest_runtest_logreport(report) -> None:  # noqa: ANN001
    """Record one verdict per test, whichever phase decided it.

    A test that fails or skips in SETUP never reaches the call phase, so
    keying only on `when == "call"` silently drops every capability skip
    and every fixture error -- which are exactly the ones an operator
    needs to see.
    """
    entry = _results.setdefault(
        report.nodeid, {"outcome": "passed", "duration": 0.0, "detail": ""}
    )
    entry["duration"] = float(entry["duration"]) + report.duration
    if report.when == "call":
        if report.outcome != "passed":
            entry["outcome"] = report.outcome
    elif report.outcome != "passed":
        # setup/teardown failure or skip decides the verdict.
        entry["outcome"] = report.outcome
    if report.outcome != "passed" and not entry["detail"]:
        if isinstance(report.longrepr, tuple):        # skip: (file, line, reason)
            entry["detail"] = str(report.longrepr[2])
        elif report.longrepr is not None:
            entry["detail"] = str(report.longrepr)


_STATUS = {"passed": "PASS", "failed": "FAIL", "skipped": "SKIP", "error": "ERROR"}


def _headline(detail: str) -> str:
    """The one line worth showing in a terminal.

    pytest marks the assertion message with a leading `E`, and puts the
    file:line last. Taking the last line gives
    `test_local_garp.py:341: AssertionError` -- true, and useless, because
    every message these tests raise was written to explain what the
    failure MEANS. Prefer the first `E` line.
    """
    for line in detail.splitlines():
        stripped = line.strip()
        if stripped.startswith("E "):
            return stripped[2:].strip()
    lines = [l.strip() for l in detail.splitlines() if l.strip()]
    return lines[-1] if lines else ""


def pytest_report_teststatus(report, config):  # noqa: ANN001, ANN201
    """Put the description on pytest's own per-test verbose line.

    --show-tests forces verbose mode (below) so pytest prints one line per
    test as it finishes; this replaces the bare PASSED/FAILED word with the
    verdict plus what the test actually checked. Writing our own lines
    instead means fighting the terminal writer for the filename prefix and
    the percentage column, which is how the first attempt at this ended up
    breaking a line in the middle of every result.
    """
    if not config.getoption("--show-tests"):
        return None
    if report.when == "setup" and report.outcome == "passed":
        return None
    if report.when == "teardown" and report.outcome == "passed":
        return None
    status = _STATUS.get(report.outcome, report.outcome.upper())
    desc = _descriptions.get(report.nodeid, "")
    return report.outcome, status[0], f"{status}  {desc}" if desc else status


def _grouped() -> Dict[str, List[tuple]]:
    """Results by module, in collection order."""
    out: Dict[str, List[tuple]] = {}
    for nodeid, r in _results.items():
        module = nodeid.split("::")[0].split("/")[-1]
        name = nodeid.split("::", 1)[1] if "::" in nodeid else nodeid
        out.setdefault(module, []).append(
            (name, _STATUS.get(str(r["outcome"]), str(r["outcome"]).upper()),
             float(r["duration"]), _descriptions.get(nodeid, ""), str(r["detail"]))
        )
    return out


def _counts() -> Dict[str, int]:
    c: Dict[str, int] = {}
    for r in _results.values():
        key = str(r["outcome"])
        c[key] = c.get(key, 0) + 1
    return c


def _write_report(path: str, config) -> None:  # noqa: ANN001
    counts = _counts()
    total = sum(counts.values())
    summary = ", ".join(f"{n} {k}" for k, n in sorted(counts.items())) or "nothing ran"
    lines: List[str] = [
        "PureLB end-to-end test report",
        "=" * 70,
        f"Generated : {_dt.datetime.now().strftime('%Y-%m-%d %H:%M:%S')}",
        f"Context   : {config.getoption('--context') or '(none)'}",
        f"Namespace : {config.getoption('--purelb-namespace')}",
        f"Router    : {config.getoption('--router-host') or '(not provided)'}",
        f"Result    : {total} tests - {summary}",
        "",
    ]
    if _detected_capabilities:
        detected = ", ".join(
            f"{k}={'yes' if v else 'NO'}" for k, v in sorted(_detected_capabilities.items())
        )
        lines += [f"Cluster   : {detected}", ""]
    if counts.get("skipped"):
        lines += [
            "A skipped test verified NOTHING. Green with skips is not the same",
            "as green; use --require to turn a missing capability into a failure.",
            "",
        ]
    for module, rows in _grouped().items():
        lines += ["-" * 70, module, "-" * 70]
        for name, status, duration, desc, detail in rows:
            lines.append(f"  {status:<5} {duration:6.1f}s  {desc or name}")
            lines.append(f"        {' ' * 7}  [{name}]")
            if detail:
                for dl in detail.strip().splitlines():
                    lines.append(f"        {' ' * 7}  | {dl}")
        lines.append("")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")


def pytest_terminal_summary(terminalreporter, exitstatus, config) -> None:  # noqa: ANN001
    """Per-test results, the skip block, and the report file.

    The skip block is not optional and never has been: a run that skipped
    half the suite and printed 'passed' is the suite-level version of an
    assertion that passes when it cannot observe.
    """
    if config.getoption("--show-tests") and _results:
        terminalreporter.write_sep("=", "RESULTS")
        for module, rows in _grouped().items():
            terminalreporter.write_line(module)
            for name, status, duration, desc, detail in rows:
                colour = {"PASS": {"green": True}, "FAIL": {"red": True},
                          "ERROR": {"red": True}, "SKIP": {"yellow": True}}.get(status, {})
                terminalreporter.write(f"  {status:<5} ", **colour)
                terminalreporter.write_line(f"{duration:6.1f}s  {desc or name}")
                if status in ("FAIL", "ERROR") and detail:
                    terminalreporter.write_line(
                        f"          {' ' * 6}  {_headline(detail)[:200]}")

    report_path = config.getoption("--report")
    if report_path:
        _write_report(report_path, config)
        terminalreporter.write_line(f"\nreport written to {report_path}")

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
