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

"""Gratuitous announcements, verified on the wire.

Every other test in this suite asks the cluster about itself: does the
address appear in `ip addr`, does the annotation name the right node,
does the counter go up. This module is the only one that asks the
NETWORK, by capturing frames on the router.

That distinction is the point. A gratuitous ARP exists solely to change
what other machines believe; one that is counted but never leaves the
node is indistinguishable, from inside the node, from one that worked.
The observable failure is a VIP that has moved and stays unreachable
until neighbour caches age out -- minutes of blackhole after a failover
that every in-cluster assertion calls successful.

IPv6 is where it matters most. The kernel does NOT notify neighbours
when an address is added: it runs DAD, whose probes are sourced from ::
and update nobody's cache, and the skip-DAD annotation suppresses even
those. Without PureLB's unsolicited Neighbor Advertisement a moved IPv6
VIP converges only via NUD timeouts. These are the only tests in the
repo that prove the NA reaches the wire.

WHAT THIS DOES TO THE CLUSTER: GARPConfig is `+optional` with no
default and sendGARPSequence returns immediately when it is nil, so on a
default install nothing is ever sent and both counters are structurally
zero. The feature cannot be observed without turning it on, so the
fixture patches the `default` LBNodeAgent and puts it back. Worth
stating plainly rather than burying in a fixture: PureLB as shipped does
not announce gratuitously unless configured to.

Facts established by capturing on the router BEFORE these assertions
were written, none of them guessable from the source:

* One sendGARP call puts TWO frames on the wire -- an ARP Request and an
  ARP Reply, both broadcast, both with sender == target == the VIP. Belt
  and braces: some equipment honours only the request form, some only
  the reply. Frame counts therefore run at 2x the metric, and an
  assertion written as "frames == count" fails on correct behaviour.
  Measured: garp_sent_total 12 <-> 24 ARP frames, na_sent_total 12 <->
  12 NA frames.

* A `host <vip>` BPF filter captures the IPv4 frames but NOTHING for
  IPv6. The NA travels fe80::<node> -> ff02::1 and the VIP appears only
  inside the ICMPv6 payload as the target address, never in the IPv6
  header that `host` inspects. Both families are captured broadly here
  and matched on the decoded target instead.

* announceLocal has no already-announced short circuit, so every
  reconcile of an announced address starts another sequence. Creating
  one dual-stack Service produced four overlapping sequences. Nothing is
  harmed -- gratuitous announcements are idempotent -- but every count
  assertion here is `>=`, and one written as `==` would be flaky by
  construction.

The addresses are pinned to a chosen subnet with a dedicated
ServiceGroup rather than taken from the default pool. The capture has to
start before the Service exists, so the router interface must be known
before the address is allocated -- and the default group spans every
subnet, so its allocation could land on the interface the sniffer is not
watching. That failure would present as "no frames", i.e. as a product
bug.
"""

from __future__ import annotations

import ipaddress
import re
import time
from dataclasses import dataclass
from typing import Dict, List, Optional

import pytest

from purelb_e2e import nodes, topology
from purelb_e2e.cluster import Cluster
from purelb_e2e.nodes import Router
from purelb_e2e.wait import wait_until

SERVICE_GROUP = "purelb.io/service-group"

GARP_COUNT = 3
GARP_SENT = "purelb_lbnodeagent_garp_sent_total"
GARP_ERRORS = "purelb_lbnodeagent_garp_errors_total"
NA_SENT = "purelb_lbnodeagent_na_sent_total"
NA_ERRORS = "purelb_lbnodeagent_na_errors_total"
ADDITIONS = "purelb_lbnodeagent_address_additions_total"

# initialDelay + (count - 1) * interval, with room to spare. Only used
# where absence is the assertion: proving something did NOT happen means
# waiting out the window in which it would have.
SEQUENCE_WINDOW = 5.0

# Broad on purpose -- see the module docstring on why `host <vip>` cannot
# be used for IPv6. ip6[40] == 136 is the ICMPv6 type byte for Neighbor
# Advertisement.
BPF = "arp or (icmp6 and ip6[40] == 136)"

_MAC = r"[0-9a-fA-F:]{17}"
_REQUEST = re.compile(
    rf"^\S+\s+(?P<src>{_MAC}) > (?P<dst>{_MAC}),.*"
    rf"Request who-has (?P<who>\S+?)[\s(].*tell (?P<tell>[^,\s]+)"
)
_REPLY = re.compile(
    rf"^\S+\s+(?P<src>{_MAC}) > (?P<dst>{_MAC}),.*"
    rf"Reply (?P<ip>\S+) is-at (?P<mac>{_MAC})"
)
_NA = re.compile(
    rf"^\S+\s+(?P<src>{_MAC}) > (?P<dst>{_MAC}),.*"
    rf"neighbor advertisement, tgt is (?P<tgt>[^,\s]+)"
)


@dataclass(frozen=True)
class Frame:
    kind: str      # garp-request | garp-reply | unsolicited-na
    src_mac: str
    target: str


def _same(a: str, b: str) -> bool:
    """Address equality that survives text-form differences.

    IPv6 has many spellings of one address and tcpdump does not
    necessarily pick the one Kubernetes reported.
    """
    try:
        return ipaddress.ip_address(a) == ipaddress.ip_address(b)
    except ValueError:
        return False


def announcements(lines: List[str], vip: str) -> List[Frame]:
    """Gratuitous announcements of `vip`, and only those.

    Anything not gratuitous is dropped. An ordinary ARP request for the
    VIP from another host, or a solicited reply, would otherwise be
    counted as PureLB announcing -- and then these tests would pass
    against a build that sent nothing, because a live VIP attracts ARP
    traffic by existing. The gratuitous property IS the assertion.
    """
    found: List[Frame] = []
    for line in lines:
        m = _REQUEST.search(line)
        if m:
            # Gratuitous: the sender asks for the address it already
            # claims to own -- who-has == tell == the VIP.
            if _same(m.group("who"), vip) and _same(m.group("tell"), vip):
                found.append(Frame("garp-request", m.group("src").lower(), vip))
            continue
        m = _REPLY.search(line)
        if m:
            if _same(m.group("ip"), vip):
                found.append(Frame("garp-reply", m.group("src").lower(), vip))
            continue
        m = _NA.search(line)
        if m and _same(m.group("tgt"), vip):
            found.append(Frame("unsolicited-na", m.group("src").lower(), vip))
    return found


def _apply_agent(cluster: Cluster, topo: topology.Topology,
                 garp: Optional[Dict[str, object]], scrape) -> None:
    """Set the agent config and wait until it is definitely live.

    apply_cr replaces rather than merges, so garp=None removes
    garpConfig instead of leaving a stale one behind.

    The restart is not cosmetic. The CR controller pushes config into a
    running announcer, but nothing the agent exports says which
    generation is live, so a test that patched the CR and carried on
    would be racing an informer -- and would fail in the shape of "the
    feature does not work".

    Readiness is then waited for TWICE, on purpose. The DaemonSet
    reporting ready is not sufficient: the lbnodeagent container has no
    readiness probe (only the k8gobgp sidecar does, on 7474), so a pod
    counts as Ready the moment the sidecar is, while the announcer's
    metrics listener on 7472 may still be unbound. That gap is real --
    it failed two tests here with a connection refused on one node --
    and it is a race the DaemonSet's own status cannot express. So the
    second wait asks the precondition these tests actually have: every
    agent answering a scrape.
    """
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "LBNodeAgent",
            "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
            "spec": {
                "local": {
                    "localInterface": "default",
                    "dummyInterface": "kube-lb0",
                    **({"garpConfig": garp} if garp is not None else {}),
                }
            },
        }
    )
    cluster.restart_daemonset(cluster.purelb_namespace, "lbnodeagent")
    wait_until(
        lambda: cluster.daemonset_ready(
            cluster.purelb_namespace, "lbnodeagent", expect_nodes=len(topo.node_ips)
        ) or None,
        timeout=240, interval=3.0,
        description="every lbnodeagent to come back with the new GARP config",
    )

    def all_scrapable() -> Optional[bool]:
        for node in topo.node_ips:
            try:
                scrape(node)
            except Exception:  # noqa: BLE001 - any scrape failure means not yet
                return None
        return True

    wait_until(
        all_scrapable, timeout=180, interval=3.0,
        description="every lbnodeagent to answer a metrics scrape",
    )


@pytest.fixture(scope="module")
def garp_pool(cluster: Cluster, topo: topology.Topology):
    """One ServiceGroup for the whole module, on a band nothing else uses.

    Created once rather than per test, and that is not tidiness. The
    obvious per-test fixture creates and deletes four groups with
    IDENTICAL ranges in quick succession, so when the allocator has not
    yet reconciled a deletion the next group is rejected as overlapping
    and its Service simply never gets an address. It surfaces as an
    allocation timeout in whichever test drew the short straw -- the same
    test passing alone and failing in a full run, which is the worst
    shape a failure can take.

    The .241-.243 / e:: band sits outside the default (.200-.220),
    per-test (.230-.240), multi-interface (.244-.247) and tenant
    (.248-.250) bands, so it cannot collide with another module either.

    One subnet serves every test here: the capture has to start before
    the Service exists, so the router interface must be known before the
    address is allocated. It is chosen to carry IPv6 and at least two
    nodes, because the v6 and failover tests need those and an address
    can only move between nodes on its own subnet.
    """
    subnet = next(
        (s for s in topo.subnets if s.v6 and len(s.nodes) >= 2),
        topo.subnets[0],
    )
    spec: Dict[str, object] = {
        "local": {
            "v4pools": [
                {"aggregation": "default", "pool": subnet.band(241, 243),
                 "subnet": subnet.v4}
            ]
        }
    }
    if subnet.v6:
        spec["local"]["v6pools"] = [  # type: ignore[index]
            {"aggregation": "default", "pool": subnet.v6_band("e", 1, 4),
             "subnet": subnet.v6}
        ]
    cluster.apply_cr(
        {
            "apiVersion": "purelb.io/v2",
            "kind": "ServiceGroup",
            "metadata": {"name": "garp-pool", "namespace": cluster.purelb_namespace},
            "spec": spec,
        }
    )
    yield "garp-pool", subnet
    cluster.delete_cr("servicegroup", "garp-pool")


@pytest.fixture(scope="module")
def garp_enabled(cluster: Cluster, topo: topology.Topology, lbnodeagent: str,
                 agent_metrics):
    """Turn gratuitous announcements on, then restore the shipped agent.

    Yields a setter so the negative control can reconfigure without a
    second fixture; teardown restores the plain spec either way.

    Module-scoped, and it reconfigures only when the config actually
    changes. Each change costs a DaemonSet rollout -- ~100s, since pods
    are replaced one at a time and the wait is now for the rollout to
    genuinely complete -- and a function-scoped version paid that twice
    per test even though three of the four tests here want identical
    settings. Eight rollouts became three, with no loss of isolation: the
    only test that wants something different asks for it, and the
    comparison applies it.
    """
    applied: Dict[str, object] = {}

    def configure(**overrides: object) -> int:
        cfg: Dict[str, object] = {
            "enabled": True,
            "count": GARP_COUNT,
            "initialDelay": "100ms",
            "interval": "200ms",
        }
        cfg.update(overrides)
        if cfg != applied.get("cfg"):
            _apply_agent(cluster, topo, cfg, agent_metrics)
            applied["cfg"] = cfg
        return GARP_COUNT

    configure()
    yield configure
    _apply_agent(cluster, topo, None, agent_metrics)


def _announced(topo: topology.Topology, vip: str, timeout: float = 90.0):
    return wait_until(
        lambda: nodes.announcing_node(topo.node_ips, vip),
        timeout=timeout, interval=2.0,
        description=f"{vip} to be announced",
    )


def _assert_from_holder(frames: List[Frame], holder: str, holder_mac: Optional[str],
                        vip: str, what: str) -> None:
    """Every frame must carry the announcing node's MAC.

    This is what makes these announcement tests rather than frame-count
    tests: the MAC is the entire payload of the claim, and a frame
    bearing anyone else's is a different node talking about the address.

    The MAC is passed in rather than read here, because it has to be
    read while the node still holds the address. Reading it at assertion
    time cost a run: the failover test restores the tainted node first,
    the election promptly handed the address back to it, and the MAC of
    the node under test could no longer be found -- a failure of the
    test's ordering that reads exactly like a product bug.
    """
    assert holder_mac, f"could not read the MAC of the interface holding {vip} on {holder}"
    wrong = sorted({f.src_mac for f in frames if f.src_mac != holder_mac})
    assert not wrong, (
        f"{what} for {vip} came from {wrong}, not from the announcing node "
        f"{holder} ({holder_mac}). Neighbours send traffic for {vip} to "
        f"whichever MAC announced last."
    )


# ------------------------------------------------------------------ IPv4


@pytest.mark.requires("router")
def test_announcing_an_ipv4_address_puts_gratuitous_arp_on_the_wire(
    topo: topology.Topology, lb_service, router: Router,
    agent_metrics, garp_enabled, garp_pool,
):
    """The frames reach the router, in both forms, from the right node.

    Three separate claims, and the counter can support none of them: it
    increments when the send call returns, which says nothing about
    whether a frame left the host, what it contained, or who it came
    from. That is the entire reason this module exists.
    """
    count = garp_enabled()
    group, subnet = garp_pool
    iface = router.interface_for_subnet(subnet.band(241, 243).split("-")[0])
    before = {n: agent_metrics(n) for n in topo.node_ips}

    # The capture must already be running: the first GARP goes out 100ms
    # after the address lands.
    with router.capture(iface, BPF) as cap:
        vip = lb_service("garp-v4", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
        holder, _ = _announced(topo, vip)
        wait_until(
            lambda: (agent_metrics(holder).counter(GARP_SENT)
                     - before[holder].counter(GARP_SENT)) >= count or None,
            timeout=60, interval=2.0,
            description=f"{holder} to report {count} GARPs sent",
        )
        holder_mac = nodes.mac_for_address(topo.node_ips[holder], vip)

    frames = announcements(cap.lines, vip)
    assert frames, (
        f"no gratuitous ARP for {vip} reached the router on {iface}, even "
        f"though {holder} reports having sent some. The counter increments "
        f"on the send call, so a VIP can look announced from inside the "
        f"cluster and be invisible to every neighbour on the segment.\n"
        f"captured {len(cap.lines)} frame(s) in total:\n"
        + "\n".join(cap.lines[:15])
    )
    _assert_from_holder(frames, holder, holder_mac, vip, "gratuitous ARP")

    kinds = {f.kind for f in frames}
    assert kinds == {"garp-request", "garp-reply"}, (
        f"expected both the request and reply forms of gratuitous ARP, got "
        f"{sorted(kinds)}. Equipment in the field differs in which it "
        f"honours, so sending only one form silently halves what this works "
        f"on -- and the counter cannot tell you, because it counts calls."
    )

    after = agent_metrics(holder)
    assert after.counter(GARP_SENT) - before[holder].counter(GARP_SENT) >= count
    assert after.counter(GARP_ERRORS) - before[holder].counter(GARP_ERRORS) == 0, (
        f"{GARP_ERRORS} increased on {holder}: some sends failed. Success is "
        f"logged at Debug only and the suite runs at Info, so this counter "
        f"is the signal -- never a success line."
    )


# ------------------------------------------------------------------ IPv6


@pytest.mark.requires("router", "ipv6")
def test_announcing_an_ipv6_address_puts_an_unsolicited_na_on_the_wire(
    topo: topology.Topology, lb_service, router: Router,
    agent_metrics, garp_enabled, garp_pool,
):
    """The IPv6 counterpart, and the only on-wire proof it exists.

    The kernel will not do this for us. If the NA is missing, a moved
    IPv6 VIP is unreachable until NUD times out -- while every
    in-cluster assertion still passes, because the address really is on
    the interface.
    """
    count = garp_enabled()
    group, subnet = garp_pool
    assert subnet.v6, "the ipv6 capability was detected but the pool subnet has no v6 prefix"
    iface = router.interface_for_subnet(subnet.v6_band("e", 1, 4).split("-")[0])
    before = {n: agent_metrics(n) for n in topo.node_ips}

    with router.capture(iface, BPF) as cap:
        vip = lb_service("garp-v6", ["IPv6"], annotations={SERVICE_GROUP: group})[0]
        holder, _ = _announced(topo, vip)
        wait_until(
            lambda: (agent_metrics(holder).counter(NA_SENT)
                     - before[holder].counter(NA_SENT)) >= count or None,
            timeout=60, interval=2.0,
            description=f"{holder} to report {count} unsolicited NAs sent",
        )
        holder_mac = nodes.mac_for_address(topo.node_ips[holder], vip)

    frames = announcements(cap.lines, vip)
    assert frames, (
        f"no unsolicited Neighbor Advertisement for {vip} reached the router "
        f"on {iface}. Without it the address is announced only to the kernel "
        f"that owns it, and neighbours keep sending to the old MAC until NUD "
        f"expires.\ncaptured {len(cap.lines)} frame(s) in total:\n"
        + "\n".join(cap.lines[:15])
    )
    assert {f.kind for f in frames} == {"unsolicited-na"}, sorted({f.kind for f in frames})
    _assert_from_holder(frames, holder, holder_mac, vip, "the Neighbor Advertisement")

    after = agent_metrics(holder)
    assert after.counter(NA_SENT) - before[holder].counter(NA_SENT) >= count
    assert after.counter(NA_ERRORS) - before[holder].counter(NA_ERRORS) == 0, (
        f"{NA_ERRORS} increased on {holder}: some sends failed"
    )


# --------------------------------------------------------------- failover


@pytest.mark.requires("router", "multi-node")
def test_a_moved_address_is_re_announced_by_the_new_winner(
    cluster: Cluster, topo: topology.Topology, lb_service, router: Router,
    agent_metrics, garp_enabled, tainted_nodes, garp_pool,
):
    """Failover is the case gratuitous announcement exists for.

    On a first announcement nobody holds a cached binding, so a missing
    GARP costs nothing and the test would pass by luck. After a move
    every neighbour has a stale entry pointing at a node that no longer
    has the address, and the announcement is the only thing that
    corrects them.

    The capture starts after the address has settled on its first node,
    so the window is about the move. It is not exclusively about it: the
    announcer re-runs its whole announce path on every reconcile, so the
    outgoing node can legitimately put frames on the wire right up until
    it is tainted. The assertion is therefore that the NEW winner
    announced, not that nobody else did.
    """
    count = garp_enabled()
    group, subnet = garp_pool
    # Failover needs somewhere to fail over TO on the same subnet: the
    # election only considers nodes whose own subnet contains the VIP.
    if len(subnet.nodes) < 2:
        pytest.skip("no subnet carries two nodes, so the address cannot move within one")

    vip = lb_service("garp-failover", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
    original, _ = _announced(topo, vip)

    iface = router.interface_for_subnet(vip)
    before = {n: agent_metrics(n) for n in topo.node_ips}

    try:
        # 420s: this block waits up to 180s for the move plus 60s for
        # the counter, and a capture that ends first records silence.
        with router.capture(iface, BPF, seconds=420) as cap:
            tainted_nodes(original)
            pod = cluster.pod_on_node(
                cluster.purelb_namespace, "component=lbnodeagent", original
            )
            assert pod is not None, f"no lbnodeagent on {original}"
            cluster.delete_pod(cluster.purelb_namespace, pod.metadata.name, grace_seconds=0)

            # Wait for a DIFFERENT node to take the address, not for the
            # old one to give it up. An agent killed with grace 0 never
            # runs its withdrawal, so the address can linger on the old
            # node's interface -- and announcing_node returns the first
            # node it finds carrying it, which would then be `original`
            # for ever. That is a false "it never moved", and whether it
            # happens turns on a race between the NoExecute eviction and
            # the delete, which is why it passed alone and failed in a
            # full run.
            new_holder = wait_until(
                lambda: next(
                    (n for n in nodes.nodes_with_address(topo.node_ips, vip)
                     if n != original),
                    None,
                ),
                timeout=180, interval=2.0,
                description=f"a node other than {original} to announce {vip}",
            )
            wait_until(
                lambda: (agent_metrics(new_holder).counter(GARP_SENT)
                         - before[new_holder].counter(GARP_SENT)) >= count or None,
                timeout=60, interval=2.0,
                description=f"{new_holder} to announce {vip} gratuitously",
            )
            # Read the MAC while this node still holds the address. The
            # taint removal below lets the original node back into the
            # election, which can hand the address straight back to it.
            new_mac = nodes.mac_for_address(topo.node_ips[new_holder], vip)
    finally:
        cluster.remove_taint(original, "purelb-test")
        wait_until(
            lambda: cluster.daemonset_ready(
                cluster.purelb_namespace, "lbnodeagent", expect_nodes=len(topo.node_ips)
            ) or None,
            timeout=240, interval=3.0,
            description="the DaemonSet to recover",
        )

    frames = announcements(cap.lines, vip)
    assert frames, (
        f"{vip} moved from {original} to {new_holder} and NOTHING was "
        f"announced on {iface}. Every neighbour still has {vip} at "
        f"{original}'s MAC, so the address is unreachable until those "
        f"entries age out -- while the cluster reports a completed failover."
    )
    # Presence, not exclusivity. announceLocal re-announces on every
    # reconcile, so `original` may legitimately have put frames on the
    # wire earlier in the capture window, before it was tainted. What
    # must be true is that the NEW winner announced.
    assert new_mac, f"could not read the MAC of the interface holding {vip} on {new_holder}"
    senders = sorted({f.src_mac for f in frames})
    assert new_mac in senders, (
        f"{vip} is now announced by {new_holder} ({new_mac}) but the only "
        f"gratuitous frames on {iface} came from {senders}. The address moved "
        f"without telling the network, so every neighbour still points at "
        f"{original} until its cache ages out."
    )


# -------------------------------------------------------- negative control


@pytest.mark.requires("router")
def test_disabling_garp_stops_the_announcements(
    topo: topology.Topology, lb_service, router: Router,
    agent_metrics, garp_enabled, garp_pool,
):
    """The control that makes the tests above mean anything.

    Every assertion above is of the form "frames appeared". If other
    traffic on the segment produced matching frames, or if the parser
    were loose enough to match an ordinary ARP exchange, those tests
    would pass against a build that sent nothing. This one turns the
    feature off and requires silence, so a false positive in the parser
    or the capture surfaces as a failure here instead of as undeserved
    confidence there.
    """
    garp_enabled(enabled=False)
    group, subnet = garp_pool
    iface = router.interface_for_subnet(subnet.band(241, 243).split("-")[0])
    before = {n: agent_metrics(n) for n in topo.node_ips}

    with router.capture(iface, BPF) as cap:
        vip = lb_service("garp-disabled", ["IPv4"], annotations={SERVICE_GROUP: group})[0]
        holder, _ = _announced(topo, vip)
        # Absence is only meaningful once the announcement has actually
        # happened: wait for the address add, then out the window in
        # which the sequence would have run.
        wait_until(
            lambda: (agent_metrics(holder).counter(ADDITIONS)
                     - before[holder].counter(ADDITIONS)) >= 1 or None,
            timeout=60, interval=2.0,
            description=f"{holder} to finish adding {vip}",
        )
        time.sleep(SEQUENCE_WINDOW)

    frames = announcements(cap.lines, vip)
    assert not frames, (
        f"garpConfig.enabled=false but {len(frames)} gratuitous announcement(s) "
        f"for {vip} still reached the router:\n"
        + "\n".join(f"  {f.kind} from {f.src_mac}" for f in frames[:10])
    )
    after = agent_metrics(holder)
    assert after.counter(GARP_SENT) - before[holder].counter(GARP_SENT) == 0, (
        f"{GARP_SENT} increased on {holder} with GARP disabled"
    )
