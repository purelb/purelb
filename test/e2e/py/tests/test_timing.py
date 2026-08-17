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
    "D3": float(os.environ.get("TIMING_MAX_D3_MS", 15000)) / 1000,  # create -> first good curl
    "E3": float(os.environ.get("TIMING_MAX_E3_MS", 30000)) / 1000,  # re-convergence after node loss
}
SAMPLES = int(os.environ.get("TIMING_SAMPLES", 3))


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

    KNOWN DISCONTINUITY, so nobody reads a harness change as a product
    regression: B1 and D3 step at the migration boundary and stay there.
    B1 min goes ~370ms (bash, through 2026-03-12) -> ~1600ms (pytest, from
    2026-08-13); D3 min goes ~650ms -> ~1380ms. B3 does not move.

    MEASURED CAUSE -- both terms are this harness, not the product. B1 was
    split into its two waits and each was timed:

      * The allocation wait polls at the wait_until default of 1.0s while
        allocation actually completes in 88-162ms (measured at 20ms
        polling, idle and after agent churn alike). Every B1 sample
        therefore carries a full second that is pure rounding.
      * The announcement wait's predicate SSHes each node in turn, and a
        fresh `ssh` costs ~950ms cold against ~270ms warm. A whole sweep
        is ~4.2s cold, ~1.4s warm.

    That also explains the first-iteration spike: iteration 0 pays the cold
    SSH premium and later iterations do not. Split measurement, same
    cluster, minutes apart:

        cold   alloc 1087ms  announce 1961ms   total 3048ms
        warm   alloc 1067ms  announce  544ms   total 1611ms

    Pre-warming one sweep collapses the first-iteration excess from
    +1414ms to +24ms and reproduces the 2026-08-13 numbers exactly, which
    is what that run was: the timing module alone, straight after topology
    discovery had already SSHed every node. In a full suite the preceding
    modules leave SSH idle long enough to go cold again.

    So B1 ~= 1000ms of poll rounding + one SSH sweep + the real latency,
    and the real latency is the smallest of the three. Read these as
    tripwires, which is all they claim to be, and not as convergence times.

    The nginx -> echo backend switch did NOT move any of them: the last
    nginx run (20260817-084636) and the echo runs that follow agree to
    within noise, including D3, where a readiness-probe step was predicted
    and did not appear.
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
            ips = lb_service(name, ["IPv4"])
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
        vip = lb_service(name, ["IPv4"])[0]
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
            ips = lb_service(name, ["IPv4"])
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
