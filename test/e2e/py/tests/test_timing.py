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

"""How long the data path takes to converge.

Ported from test/e2e/timing/test-timing-behavior.sh.

The bash suite originally CHARACTERISED latency and asserted nothing: it
printed a table of percentiles and always exited 0, so a change that
doubled failover time produced a green run and a slightly different
table. Ceilings were added to it during this migration; they are carried
across here as the whole point of the module.

The numbers are deliberately loose. These are not performance targets and
they are not tuned to this cluster -- they are order-of-magnitude tripwires
sized so that normal variation never trips them and a structural
regression always does. Something taking 40 seconds where it used to take
2 is a bug in kind, not in degree; something taking 2.5 seconds where it
used to take 2 is a Tuesday.

Each measurement is repeated and judged on its WORST sample rather than
its mean. A convergence path that is usually fast and occasionally awful
is exactly the shape of a race, and averaging is how you hide one.
"""

from __future__ import annotations

import datetime as _dt
import math
import os
import time
from pathlib import Path
from typing import Callable, Dict, List, Optional, Tuple

import pytest

from purelb_e2e import TEST_NAMESPACE, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE

# Where the series lives. The bash suite wrote here and nine baselines going
# back to 2026-01-21 are committed alongside; the migration deleted them and
# emitted nothing in their place, which is how a characterisation suite stops
# characterising without anyone noticing. `test/e2e/timing/` is now a data
# directory -- no bash remains.
RESULTS_DIR = Path(__file__).resolve().parents[2] / "timing"

# Collected by assert_within, written once on teardown.
_rows: List[Tuple[str, str, str]] = []

# Ceilings in seconds, overridable for a slower cluster. Same values and
# same names as the bash suite's TIMING_MAX_*_MS.
CEILING = {
    "B1": float(os.environ.get("TIMING_MAX_B1_MS", 5000)) / 1000,   # create -> VIP on the NIC
    "B3": float(os.environ.get("TIMING_MAX_B3_MS", 5000)) / 1000,   # delete -> VIP withdrawn
    "D1": float(os.environ.get("TIMING_MAX_D1_MS", 5000)) / 1000,   # VIP on NIC -> kube-proxy rules
    "D1v6": float(os.environ.get("TIMING_MAX_D1_MS", 5000)) / 1000,
    "D3": float(os.environ.get("TIMING_MAX_D3_MS", 15000)) / 1000,  # create -> first good curl
    "E3": float(os.environ.get("TIMING_MAX_E3_MS", 30000)) / 1000,  # re-convergence after node loss
}
SAMPLES = int(os.environ.get("TIMING_SAMPLES", 3))

# Poll interval for the allocation wait in the measurements below.
#
# The suite-wide default is 1.0s, which is fine when a test just needs the
# address and wrong when a test is timing how long it took: allocation
# completes in 88-162ms (measured at 20ms polling, idle and after agent
# churn alike), so the default rounded every sample up to a whole second.
# See _record for the full decomposition.
ALLOC_POLL = 0.05


def measure(action: Callable[[], None]) -> float:
    start = time.monotonic()
    action()
    return time.monotonic() - start


def _record(label: str, samples: List[float]) -> None:
    """Append this measurement to the series, in the bash suite's format.

    Milliseconds as integers, because that is what the nine committed
    baselines contain and a series is only worth keeping if the new rows
    are comparable to the old ones. The summary row deliberately carries
    commas inside its value field -- the original wrote
    `echo "$TEST,$EVENT,$VALUE"` with VALUE holding `count=..,min=..`, so a
    strict three-column reader would already choke on the historical files.
    Matching it exactly beats tidying it and breaking continuity.

    TWO DISCONTINUITIES, both this harness rather than the product, and
    the second one undoes the first. Anyone comparing across them needs
    to know which era a row came from.

    2026-08-13, the migration: B1 min ~370ms -> ~1600ms, D3 ~650ms ->
    ~1380ms, B3 unmoved. Diagnosed by splitting B1's two waits and timing
    each separately. Two independent artifacts:

      * The allocation wait polled at the wait_until default of 1.0s while
        allocation actually completes in 88-162ms (measured at 20ms
        polling, idle and after agent churn alike), so every sample
        carried a full second of pure rounding.
      * The announcement predicate SSHes each node in turn, and a fresh
        `ssh` costs ~950ms cold against ~270ms warm -- a whole 5-node
        sweep is ~4.2s cold, ~1.4s warm. Whichever iteration ran first
        paid that, which is why iteration 0 read ~1.4s slower than the
        two after it:

            cold   alloc 1087ms  announce 1961ms   total 3048ms
            warm   alloc 1067ms  announce  544ms   total 1611ms

    2026-08-17 12:41, the correction: ALLOC_POLL replaces the 1.0s
    rounding and _warm_ssh pays the cold sweep before anything is timed.
    B1 p95 2893ms -> 690ms, and the spread across three iterations
    collapses from 1286ms to 15ms, which is the artifact leaving rather
    than the cluster improving. B1 is comparable with the bash era again
    (bash 2026-03-12: 895/911ms; now 675-690ms).

    B3 stays above its bash numbers (~1450ms against ~400ms) and that is
    honest: it still contains one full sweep, because the withdrawal
    check has to ask EVERY node before it can conclude the address is
    gone. The bash suite timestamped before its sweep and so excluded it.

    D1 is the check on all of this: newly written here, it lands at
    340-777ms against the bash era's 295-702ms without being tuned to.

    The nginx -> echo backend switch moved none of them: the last nginx
    run (20260817-084636) and the echo runs that follow agree to within
    noise, including D3, where a readiness-probe step was predicted and
    did not appear.
    """
    ms = [int(round(s * 1000)) for s in samples]
    for i, value in enumerate(ms, start=1):
        _rows.append((label, f"iteration_{i}", str(value)))
    ordered = sorted(ms)
    # Nearest-rank p95, which reproduces the historical rows: for two
    # samples it is the maximum.
    p95 = ordered[max(0, math.ceil(0.95 * len(ordered)) - 1)]
    _rows.append((
        label, "summary",
        f"count={len(ms)},min={min(ms)},max={max(ms)},"
        f"avg={int(round(sum(ms) / len(ms)))},p95={p95}",
    ))


def assert_within(label: str, samples: List[float]) -> None:
    """Judge on the worst sample, and say what was measured either way.

    Reporting the numbers on success too is deliberate: the bash suite's
    table was genuinely useful for spotting drift, and a ceiling that only
    speaks when it fails throws that away.
    """
    # Record BEFORE asserting. The run where a ceiling is tripped is the
    # one whose numbers are most worth keeping, and an assert here would
    # discard them.
    _record(label, samples)

    ceiling = CEILING[label]
    worst = max(samples)
    formatted = ", ".join(f"{s:.2f}s" for s in samples)
    assert worst <= ceiling, (
        f"{label}: worst sample {worst:.2f}s exceeds the {ceiling:.0f}s ceiling "
        f"(samples: {formatted}). This is an order-of-magnitude tripwire, so "
        f"tripping it means the convergence path changed shape, not that the "
        f"cluster is having a slow day."
    )
    print(f"\n{label}: worst {worst:.2f}s of {len(samples)} (ceiling {ceiling:.0f}s) [{formatted}]")


@pytest.fixture(scope="module", autouse=True)
def _warm_ssh(topo: topology.Topology) -> None:
    """Pay the cold-SSH cost once, before anything is measured.

    Every measurement here observes the cluster by shelling out to `ssh`
    per node, and a fresh ssh costs ~950ms against ~270ms warm -- a whole
    5-node sweep is ~4.2s cold and ~1.4s warm. Whichever test ran first
    absorbed that, so its first iteration read ~1.4s slower than the two
    after it and the p95, taken from the worst sample, was the SSH
    handshake rather than the product.

    Measured, same cluster, minutes apart:

        cold   alloc 1087ms  announce 1961ms   total 3048ms
        warm   alloc 1067ms  announce  544ms   total 1611ms

    One warm-up sweep collapses the first-iteration excess from +1414ms
    to +24ms. It deliberately looks for an address nobody has, so it
    visits every node instead of stopping at the first match.
    """
    nodes.announcing_node(topo.node_ips, "203.0.113.1")


@pytest.fixture(scope="module", autouse=True)
def timing_results() -> object:
    """Write the run's measurements to the series on teardown.

    Module-scoped and autouse so it cannot be forgotten by a new test, and
    so a partial run (one module, `-k`, a failure part way) still emits
    what it managed to measure.
    """
    yield
    if not _rows:
        return  # every test skipped; an empty file would pollute the series
    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    stamp = _dt.datetime.now().strftime("%Y%m%d-%H%M%S")
    path = RESULTS_DIR / f"timing-results-{stamp}.csv"
    with path.open("w", encoding="utf-8") as fh:
        fh.write("test,event,value\n")
        for test, event, value in _rows:
            fh.write(f"{test},{event},{value}\n")
    print(f"\ntiming results: {path}")


def test_b1_service_creation_to_vip_on_the_interface(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """From creating a Service to the address existing on a node."""
    samples: List[float] = []
    for i in range(SAMPLES):
        name = f"timing-b1-{i}"

        def create_and_wait() -> None:
            ips = lb_service(name, ["IPv4"], interval=ALLOC_POLL)
            wait_until(
                lambda: nodes.announcing_node(topo.node_ips, ips[0]),
                timeout=CEILING["B1"] * 3, interval=0.2,
                description=f"{ips[0]} on an interface",
            )

        samples.append(measure(create_and_wait))
    assert_within("B1", samples)


def test_b3_service_deletion_to_vip_withdrawn(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """From deleting a Service to the address leaving every node.

    Slow withdrawal is worse than slow assignment: the address is still
    answering on a node that no longer owns it, so traffic goes somewhere
    that will stop serving it at an unpredictable moment.
    """
    samples: List[float] = []
    for i in range(SAMPLES):
        name = f"timing-b3-{i}"
        vip = lb_service(name, ["IPv4"], interval=ALLOC_POLL)[0]
        wait_until(lambda: nodes.announcing_node(topo.node_ips, vip),
                   timeout=CEILING["B1"] * 3, interval=0.2, description="announced")

        def delete_and_wait() -> None:
            cluster.delete_service(NAMESPACE, name)
            wait_while(
                lambda: bool(nodes.nodes_with_address(topo.node_ips, vip)),
                timeout=CEILING["B3"] * 3, interval=0.2,
                description=f"{vip} withdrawn everywhere",
            )

        samples.append(measure(delete_and_wait))
    assert_within("B3", samples)


def test_d3_service_creation_to_first_successful_request(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service
):
    """From creating a Service to it actually serving traffic.

    The end-to-end number a user experiences, and the one that includes
    everything PureLB does not control -- kube-proxy programming its
    rules, endpoints becoming ready. Its ceiling is looser for that
    reason.
    """
    samples: List[float] = []
    for i in range(SAMPLES):
        name = f"timing-d3-{i}"

        def create_and_serve() -> None:
            ips = lb_service(name, ["IPv4"], interval=ALLOC_POLL)
            wait_until(
                lambda: nodes.echo_json(topo.node_ips, ips[0])["pod"] or None,
                timeout=CEILING["D3"] * 3, interval=0.5,
                description=f"{ips[0]} to serve a request",
            )

        samples.append(measure(create_and_serve))
    assert_within("D3", samples)


@pytest.mark.requires("multi-node")
def test_e3_election_reconvergence_after_losing_a_node(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    tainted_nodes,
):
    """From an announcer disappearing to another node holding the address.

    The number that matters in an outage, and the only one here bounded
    by the lease duration rather than by how fast a controller reacts.
    Measured once: it taints a node and waits out an election, so
    repeating it three times would triple the slowest test in the suite
    for a sample that is dominated by a constant.
    """
    vip = lb_service("timing-e3", ["IPv4"])[0]
    original, _ = wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip),
        timeout=45, description=f"{vip} announced",
    )

    tainted_nodes(original)
    pod = cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", original)
    assert pod is not None

    def evict_and_reconverge() -> None:
        cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=0)
        wait_until(
            lambda: (lambda f: f if f and f[0] != original else None)(
                nodes.announcing_node(topo.node_ips, vip)
            ),
            timeout=CEILING["E3"] * 3, interval=0.5,
            description=f"{vip} to move off {original}",
        )

    assert_within("E3", [measure(evict_and_reconverge)])


@pytest.mark.parametrize(
    "family,label",
    [
        ("IPv4", "D1"),
        pytest.param("IPv6", "D1v6", marks=pytest.mark.requires("ipv6")),
    ],
)
def test_d1_vip_on_the_interface_to_kube_proxy_rules(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
    family: str, label: str,
):
    """From the VIP reaching a NIC to kube-proxy being able to forward it.

    Restores the D1 case, which the migration dropped and nothing else
    covers. It measures the OTHER half of a working VIP: PureLB puts the
    address on an interface, and kube-proxy programs the rules that
    actually forward it. Between those two moments the address is up and
    answers nothing, and every other assertion in the suite still passes
    -- B1 sees it on the NIC, the announcing annotation names a node, the
    election metrics move. Only a request notices, and D3 measures that
    end to end without saying which half was slow.

    Both families, unlike the cases inherited from bash: the v4 and v6
    kube-proxy tables are separate, so v6 rules can lag or be missing
    entirely without the v4 measurement noticing.
    """
    probe_node = sorted(topo.node_ips)[0]
    probe_ip = topo.node_ips[probe_node]
    if not nodes.kube_proxy_uses_nftables(probe_ip):
        pytest.skip(f"kube-proxy on {probe_node} is not in nftables mode")

    samples: List[float] = []
    for i in range(SAMPLES):
        name = f"timing-d1-{family.lower()}-{i}"
        vip = lb_service(name, [family], interval=ALLOC_POLL)[0]
        wait_until(
            lambda: nodes.announcing_node(topo.node_ips, vip),
            timeout=CEILING["B1"] * 3, interval=0.2,
            description=f"{vip} on an interface",
        )

        # The clock starts once the address is on a NIC, so this is the
        # kube-proxy half alone and not a second copy of B1.
        def wait_for_rules() -> None:
            wait_until(
                lambda: nodes.nftables_has_service(probe_ip, vip) or None,
                timeout=CEILING["D1"] * 3, interval=0.1,
                description=f"kube-proxy rules for {vip} on {probe_node}",
            )

        samples.append(measure(wait_for_rules))
    assert_within(label, samples)


@pytest.mark.requires("multi-node")
def test_e1_vip_stays_on_one_correct_node_across_scaling(
    cluster: Cluster, topo: topology.Topology, default_servicegroup: str, lb_service,
):
    """The VIP survives the backend scaling to zero and back, on ONE node.

    Restores the E1 case, dropped in the migration and recorded nowhere.
    Nothing else covers it: several tests scale the backend up and two
    scale it to zero, but none walks a sequence asserting where the
    address is at every step.

    A local-pool VIP belongs to the Service, not to its endpoints, so it
    must stay put through all of this. Two failures are being watched
    for, and they are opposite: the address being WITHDRAWN when the last
    endpoint goes (it should not be -- that is ETP Local's behaviour, and
    this Service is Cluster), and the address appearing on MORE than one
    node, which is a split brain that serves traffic from a node that
    does not own it. Both are invisible to a test that only checks the
    address exists somewhere.

    Dual-stack, and each family is judged separately: they elect
    independently, so v6 can split while v4 is perfectly stable.
    """
    families = ["IPv4", "IPv6"] if topo.has_ipv6 else ["IPv4"]
    vips = lb_service("echo-e1-stability", families, interval=ALLOC_POLL)
    for vip in vips:
        wait_until(lambda: nodes.announcing_node(topo.node_ips, vip),
                   timeout=45, description=f"{vip} announced")

    eligible = {}
    for vip in vips:
        subnet = topo.subnet_holding(vip)
        eligible[vip] = set(subnet.nodes) if subnet else set(topo.node_ips)

    # The bash sequence, kept exactly: it crosses zero twice, which is
    # where withdrawal bugs live, and steps up to 3 so endpoints land on
    # nodes that are not the announcer.
    sequence = [0, 2, 0, 1, 3, 1, 0]
    problems: List[str] = []
    samples: List[float] = []
    try:
        for replicas in sequence:
            start = time.monotonic()
            cluster.apps.patch_namespaced_deployment_scale(
                "echo", NAMESPACE, {"spec": {"replicas": replicas}}
            )
            cluster.wait_rollout(NAMESPACE, "echo", timeout=120)
            samples.append(time.monotonic() - start)

            for vip in vips:
                holders = set(nodes.nodes_with_address(topo.node_ips, vip))
                if not holders:
                    problems.append(
                        f"replicas={replicas}: {vip} is on NO node. A local-pool "
                        f"address belongs to the Service, not its endpoints, so "
                        f"scaling the backend must not withdraw it."
                    )
                elif len(holders) > 1:
                    problems.append(
                        f"replicas={replicas}: {vip} is on {sorted(holders)} -- "
                        f"split brain. More than one node answers for it and only "
                        f"one owns it."
                    )
                elif not holders <= eligible[vip]:
                    problems.append(
                        f"replicas={replicas}: {vip} is on {sorted(holders)}, which "
                        f"is not on the address's subnet {sorted(eligible[vip])}. "
                        f"Traffic to it cannot arrive."
                    )
    finally:
        cluster.apps.patch_namespaced_deployment_scale(
            "echo", NAMESPACE, {"spec": {"replicas": 1}}
        )
        cluster.wait_rollout(NAMESPACE, "echo", timeout=180)

    _record("E1", samples)
    assert not problems, (
        "the VIP did not stay on exactly one correct node across "
        + " -> ".join(str(r) for r in sequence)
        + ":\n  " + "\n  ".join(problems)
    )
