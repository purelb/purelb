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

import os
import time
from typing import Callable, Dict, List, Optional

import pytest

from purelb_e2e import TEST_NAMESPACE, nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.wait import wait_until, wait_while

NAMESPACE = TEST_NAMESPACE

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


def assert_within(label: str, samples: List[float]) -> None:
    """Judge on the worst sample, and say what was measured either way.

    Reporting the numbers on success too is deliberate: the bash suite's
    table was genuinely useful for spotting drift, and a ceiling that only
    speaks when it fails throws that away.
    """
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
                lambda: "Pod:" in nodes.curl_via_node(topo.node_ips, ips[0]) or None,
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
