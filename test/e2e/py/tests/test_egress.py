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

"""Egress, the announcer side (Phase 4).

One flow, which later phases extend inline (Phase 5 adds the pod-node
stage and with it traffic through these addresses). What Phase 4 can show
without any egress traffic:

* placement: a local pool's address on its holder's pool interface, as a
  Service address would be, IPv6 deprecated, and on no other node; a
  routed pool's on every egress node's dummy interface, so the router sees
  one equal-cost path per node (D7);
* a placed address removed by something else comes straight back;
* a takeover announces the address from the new holder on the wire;
* a blocked tunnel port is reported on the node, its peers and the
  policies (BPF peer probes);
* a duplicate address is reported and not fought: DAD fails, the kernel
  deletes the address, no announcement claims it, and it comes back once
  the duplicate goes;
* turning egress off removes everything it put on every node, and
  turning it on again rebuilds it.

WHAT THIS DOES TO THE CLUSTER: egress on every node through the `default`
LBNodeAgent (already so under --egress), with GARP/NA configured so the
takeover can be seen on the wire; two ServiceGroups (.221-.223 / ee:: and
the routed 10.252.0.0/24 / fd00:10:252::/64), a namespace with one pod
and two Egresses. All of it is put back afterwards. The duplicate-address
case puts a macvlan in a network namespace on another node of the
holder's subnet: a host on the same link that egress does not manage.
"""

from __future__ import annotations

import time
from dataclasses import dataclass
from typing import Dict, List, Optional, Tuple

import pytest
from kubernetes import client
from kubernetes.client.rest import ApiException

from purelb_e2e import backend, egress, guard, nodes, topology
from purelb_e2e.cluster import Cluster, utcnow
from purelb_e2e.nodes import Router, ssh
from purelb_e2e.wait import wait_until

pytestmark = pytest.mark.requires("router", "multi-node", "ipv6")

NS = "egress-e2e"
LOCAL_SG = "egress-e2e"
ROUTED_SG = "egress-e2e-routed"
ROUTED_V4 = ("10.252.0.10-10.252.0.12", "10.252.0.0/24")
ROUTED_V6 = ("fd00:10:252::10-fd00:10:252::12", "fd00:10:252::/64")
GROUP, VERSION = "purelb.io", "v2"
TUNNEL = "purelb-eg0"
NA_SENT = "purelb_lbnodeagent_na_sent_total"
# The GARP/NA sequence's default count (sendAnnouncements).
ANNOUNCEMENTS = 3


@dataclass
class Env:
    sub: topology.Subnet
    v4: str  # the local pool's addresses
    v6: str
    r4: str  # the routed pool's
    r6: str


# ---------------------------------------------------------------- helpers


def node_states(cluster: Cluster) -> Dict[str, dict]:
    objs = cluster.custom.list_cluster_custom_object(GROUP, VERSION, "egressnodestates")
    return {o["metadata"]["name"]: o for o in objs.get("items", [])}


def announcers(cluster: Cluster, address: str) -> List[str]:
    """The nodes listing `address` in status.announcing (D6 acks)."""
    out = []
    for name, ns in node_states(cluster).items():
        if any(a.get("address") == address for a in (ns.get("status") or {}).get("announcing") or []):
            out.append(name)
    return sorted(out)


def node_condition(cluster: Cluster, node: str, typ: str) -> Tuple[str, str, str]:
    st = (node_states(cluster).get(node) or {}).get("status") or {}
    for c in st.get("conditions") or []:
        if c["type"] == typ:
            return c["status"], c.get("reason", ""), c.get("message", "")
    return "", "", ""


def faults(cluster: Cluster, node: str) -> List[Tuple[str, str]]:
    st = (node_states(cluster).get(node) or {}).get("status") or {}
    return [(f["address"], f["reason"]) for f in st.get("addressFaults") or []]


def policy(cluster: Cluster, name: str) -> dict:
    return cluster.custom.get_namespaced_custom_object(GROUP, VERSION, NS, "egresses", name)


def policy_condition(cluster: Cluster, name: str, typ: str) -> Tuple[str, str]:
    for c in (policy(cluster, name).get("status") or {}).get("conditions") or []:
        if c["type"] == typ:
            return c["status"], c.get("reason", "")
    return "", ""


def policy_addresses(cluster: Cluster, name: str) -> Dict[str, str]:
    """family -> address, from the policy's status."""
    st = policy(cluster, name).get("status") or {}
    return {a["family"]: a["address"] for a in st.get("addresses") or []}


def agent_pod(cluster: Cluster, node: str) -> Optional[client.V1Pod]:
    return cluster.pod_on_node(cluster.purelb_namespace, "component=lbnodeagent", node)


def agent_log(cluster: Cluster, node: str, since=None) -> str:  # noqa: ANN001
    """The node's agent log, since `since` or the whole of it."""
    pod = agent_pod(cluster, node)
    if pod is None:
        return ""
    if since is None:
        return cluster.pod_log_text(cluster.purelb_namespace, pod.metadata.name, "lbnodeagent")
    return cluster.pod_logs(cluster.purelb_namespace, pod.metadata.name, since, "lbnodeagent")


def egress_pins(host: str) -> List[str]:
    out = ssh(host, "sudo ls /sys/fs/bpf/purelb 2>/dev/null || true", check=False).split()
    return [p for p in out if not p.startswith("addrguard")]


def apply_agent(cluster: Cluster, on: bool) -> None:
    """The `default` LBNodeAgent with egress on or off, GARP/NA configured
    and the session's address guard."""
    local = guard.local_spec({"localInterface": "default", "dummyInterface": "kube-lb0",
                              "garpConfig": {"enabled": True}})
    cluster.apply_cr({
        "apiVersion": "purelb.io/v2", "kind": "LBNodeAgent",
        "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
        "spec": {"local": local, "egress": {"enabled": on}},
    })


def all_capable(cluster: Cluster, node_ips: Dict[str, str]) -> bool:
    caps = egress.capable_nodes(cluster)
    return all(caps.get(n) for n in node_ips)


def sg_body(name: str, spec: dict) -> dict:
    return {"apiVersion": "purelb.io/v2", "kind": "ServiceGroup",
            "metadata": {"name": name, "namespace": "purelb-system"},
            "spec": {**spec, "egress": {"allowed": True, "affinityModes": ["Hash"], "sliceSize": 4096}}}


def egress_body(name: str, group: str) -> dict:
    return {"apiVersion": "purelb.io/v2", "kind": "Egress", "metadata": {"name": name, "namespace": NS},
            "spec": {"serviceGroup": group, "podSelector": {"matchLabels": {"app": "egress-e2e"}},
                     "destinations": {"include": [{"cidrs": ["0.0.0.0/0", "::/0"]}]}}}


# ---------------------------------------------------------------- fixture


@pytest.fixture(scope="module")
def env(cluster: Cluster, topo: topology.Topology, node_ips: Dict[str, str], lbnodeagent: str):
    sub = next((s for s in topo.subnets if len(s.nodes) >= 2 and s.v6), None)
    if sub is None:
        pytest.skip("needs a subnet with two nodes and IPv6")
    apply_agent(cluster, True)
    wait_until(lambda: all_capable(cluster, node_ips), timeout=180, description="egress up on every node")

    cluster.apply_cr(sg_body(LOCAL_SG, {"local": {
        "v4pool": {"name": "egress", "pool": sub.egress_pool, "subnet": sub.v4, "aggregation": "default"},
        "v6pool": {"name": "egress6", "pool": sub.egress_pool_v6, "subnet": sub.v6, "aggregation": "default"}}}))
    cluster.apply_cr(sg_body(ROUTED_SG, {"remote": {
        "v4pool": {"name": "routed4", "pool": ROUTED_V4[0], "subnet": ROUTED_V4[1], "aggregation": "/32"},
        "v6pool": {"name": "routed6", "pool": ROUTED_V6[0], "subnet": ROUTED_V6[1], "aggregation": "default"}}}))
    try:
        cluster.core.create_namespace(client.V1Namespace(metadata=client.V1ObjectMeta(name=NS)))
    except ApiException as exc:
        if exc.status != 409:
            raise
    cluster.apps.create_namespaced_deployment(NS, {
        "apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "egress-e2e"},
        "spec": {"replicas": 1, "selector": {"matchLabels": {"app": "egress-e2e"}},
                 "template": {"metadata": {"labels": {"app": "egress-e2e"}},
                              "spec": {"terminationGracePeriodSeconds": 1, "containers": [{
                                  "name": "c", "image": backend.IMAGE, "command": ["sleep", "infinity"]}]}}}})
    for name, group in (("local", LOCAL_SG), ("routed", ROUTED_SG)):
        cluster.custom.create_namespaced_custom_object(GROUP, VERSION, NS, "egresses", egress_body(name, group))

    local = wait_until(lambda: (lambda a: a if len(a) == 2 else None)(policy_addresses(cluster, "local")),
                       timeout=120, description="the local policy's two addresses")
    routed = wait_until(lambda: (lambda a: a if len(a) == 2 else None)(policy_addresses(cluster, "routed")),
                        timeout=120, description="the routed policy's two addresses")
    e = Env(sub=sub, v4=local["IPv4"], v6=local["IPv6"], r4=routed["IPv4"], r6=routed["IPv6"])
    for a in (e.v4, e.v6):
        wait_until(lambda a=a: len(announcers(cluster, a)) == 1, timeout=120, description=f"one holder of {a}")
    for a in (e.r4, e.r6):
        wait_until(lambda a=a: len(announcers(cluster, a)) == len(node_ips), timeout=120,
                   description=f"every node advertising {a}")
    yield e

    for name in ("local", "routed"):
        try:
            cluster.custom.delete_namespaced_custom_object(GROUP, VERSION, NS, "egresses", name)
        except ApiException as exc:
            if exc.status != 404:
                raise
    # The finalizer lets go once the policy's slices have drained, which
    # takes up to 5 minutes (design 5.6), and the pod's are Draining here.
    wait_until(lambda: not cluster.custom.list_namespaced_custom_object(GROUP, VERSION, NS, "egresses")["items"],
               timeout=420, interval=5, description="the Egresses gone (finalizers, after the drains)")
    cluster.core.delete_namespace(NS)
    cluster.delete_cr("servicegroup", LOCAL_SG)
    cluster.delete_cr("servicegroup", ROUTED_SG)
    # The default agent as the session has it.
    local_spec = guard.local_spec({"localInterface": "default", "dummyInterface": "kube-lb0"})
    spec = {"local": local_spec, **({"egress": {"enabled": True}} if egress.enabled() else {})}
    with egress.suspended():
        cluster.apply_cr({"apiVersion": "purelb.io/v2", "kind": "LBNodeAgent",
                          "metadata": {"name": "default", "namespace": cluster.purelb_namespace}, "spec": spec})


def conditions_summary(cluster: Cluster) -> str:
    """Every node's False conditions and faults, for a failure message."""
    out = []
    for name, ns in sorted(node_states(cluster).items()):
        st = ns.get("status") or {}
        bad = [f"{c['type']}={c.get('reason')}: {c.get('message', '')}" for c in st.get("conditions") or []
               if c["status"] != "True"]
        out.append(f"{name}: {bad} faults={st.get('addressFaults')}")
    return "\n  ".join(out)


def wait_nodes(cluster: Cluster, pred, timeout: float, description: str) -> None:  # noqa: ANN001
    """wait_until, failing with every node's conditions."""
    try:
        wait_until(pred, timeout=timeout, interval=2, description=description)
    except Exception as exc:
        raise AssertionError(f"{exc}\n  {conditions_summary(cluster)}") from None


def holder(cluster: Cluster, address: str) -> str:
    got = announcers(cluster, address)
    assert len(got) == 1, f"{address}: one holder, got {got}"
    return got[0]


# ---------------------------------------------------------------- tests


def test_local_addresses_placed_on_the_holder(cluster: Cluster, node_ips: Dict[str, str], env: Env, log_window) -> None:
    """A local pool's addresses are on their holder's pool interface as a
    Service address would be (finite lifetime, noprefixroute; IPv6
    deprecated, never a source) and on no other node; the holder's
    conditions are all True and it logged the placement."""
    for addr in (env.v4, env.v6):
        h = holder(cluster, addr)
        assert nodes.nodes_with_address(node_ips, addr) == [h], f"{addr} only on its holder"
        d = nodes.address_detail(node_ips[h], addr)
        assert d is not None and not d.permanent, f"{addr}: a renewed, finite lifetime"
        assert d.has_flag("noprefixroute")
        if ":" in addr:
            assert d.has_flag("deprecated"), "an IPv6 egress address is never a source"
        for typ in ("DatapathReady", "DatapathCompatible", "UnderlayEncrypted", "TunnelPeersReachable", "ForwardPath"):
            status, reason, msg = node_condition(cluster, h, typ)
            if typ == "DatapathReady" and reason == "ArpAnnounce":
                continue  # a host setting the agent reports, not one it makes (design 6.9)
            assert status == "True", f"{h} {typ}: {reason} {msg}"
    log = agent_log(cluster, holder(cluster, env.v4))
    assert f'"ip":"{env.v4}"' in log and '"op":"placeEgress"' in log, "the holder logged the placement"


def test_routed_addresses_one_path_per_node(cluster: Cluster, node_ips: Dict[str, str], router: Router, env: Env) -> None:
    """Every egress node advertises a routed address (D7): host-route mode
    for IPv4 (/32), aggregate mode for IPv6 (the pool's /64), each on the
    dummy interface, and the router has one equal-cost path per node."""
    for n, ip in node_ips.items():
        d4, d6 = nodes.address_detail(ip, env.r4), nodes.address_detail(ip, env.r6)
        assert d4 is not None and d4.interface == "kube-lb0" and d4.cidr.endswith("/32"), n
        assert d6 is not None and d6.interface == "kube-lb0" and d6.cidr.endswith("/64"), n
        assert d6.has_flag("deprecated"), n
    wait_until(lambda: len(router.nexthops(f"{env.r4}/32")) == len(node_ips), timeout=90,
               description="one IPv4 path per node at the router")
    wait_until(lambda: len(router.nexthops(ROUTED_V6[1])) == len(node_ips), timeout=90,
               description="one IPv6 path per node at the router")


def test_removed_address_comes_back(cluster: Cluster, node_ips: Dict[str, str], env: Env) -> None:
    """An egress address removed by something else is placed again at once
    (address watcher), and the agent says so."""
    h = holder(cluster, env.v4)
    d = nodes.address_detail(node_ips[h], env.v4)
    since = utcnow()
    start = time.monotonic()
    ssh(node_ips[h], f"sudo ip addr del {d.cidr} dev {d.interface}")
    wait_until(lambda: nodes.has_address(node_ips[h], env.v4), timeout=10, interval=0.2,
               description="the address back on its holder")
    assert time.monotonic() - start < 5, "back within seconds, not at the next periodic reconcile"
    assert "removed outside the agent" in agent_log(cluster, h, since)


def test_takeover_is_announced_on_the_wire(cluster: Cluster, node_ips: Dict[str, str], router: Router,
                                          env: Env) -> None:
    """When the holder can no longer hold the addresses -- here its group's
    announcerNodeSelector leaves it out, a move nothing reverses midway, as
    a restarting agent's would -- another node of the subnet takes over each
    address it held and announces it from its own MAC: a gratuitous ARP for
    IPv4, an unsolicited NA (after DAD) for IPv6; the old holder lets go."""
    old = holder(cluster, env.v4)
    moving = [a for a in (env.v4, env.v6) if holder(cluster, a) == old]
    iface = router.interface_for_subnet(env.v4)
    sg = cluster.get_cr("servicegroup", LOCAL_SG)
    sg["spec"]["egress"]["announcerNodeSelector"] = {"matchExpressions": [
        {"key": "kubernetes.io/hostname", "operator": "NotIn", "values": [old]}]}
    new: Dict[str, str] = {}
    try:
        with router.capture(iface, "arp or (icmp6 and ip6[40] == 136)", seconds=240) as cap:
            cluster.apply_cr(sg)
            for a in moving:
                new[a] = wait_until(lambda a=a: next((n for n in announcers(cluster, a) if n != old), None),
                                    timeout=120, description=f"another node holding {a}")
            if env.v6 in new:
                wait_until(lambda: (lambda d: d is not None and not d.has_flag("tentative"))(
                    nodes.address_detail(node_ips[new[env.v6]], env.v6)),
                    timeout=60, description="the IPv6 address past DAD on its new holder")
            time.sleep(5)  # the announcement sequence
        lines = [ln.lower() for ln in cap.lines]
        if env.v4 in new:
            mac = nodes.mac_for_address(node_ips[new[env.v4]], env.v4).lower()
            assert any(mac in ln and env.v4 in ln for ln in lines if " arp" in ln), \
                f"a GARP for {env.v4} from {new[env.v4]} ({mac})"
        if env.v6 in new:
            mac = nodes.mac_for_address(node_ips[new[env.v6]], env.v6).lower()
            assert any(mac in ln and f"tgt is {env.v6}" in ln for ln in lines), \
                f"an NA for {env.v6} from {new[env.v6]} ({mac})"
        for a in moving:
            assert not nodes.has_address(node_ips[old], a), f"{old} let go of {a}"
    finally:
        sg = cluster.get_cr("servicegroup", LOCAL_SG)
        sg["spec"]["egress"].pop("announcerNodeSelector", None)
        cluster.apply_cr(sg)
    for a in (env.v4, env.v6):
        wait_until(lambda a=a: len(announcers(cluster, a)) == 1, timeout=120, description=f"one holder of {a}")
    assert moving, "the holder held something to move"


def test_blocked_tunnel_port_is_reported(cluster: Cluster, node_ips: Dict[str, str], env: Env) -> None:
    """A firewall dropping the tunnel port on one node: that node reports
    ForwardPath=False TunnelPortBlocked, its peers report it unreachable,
    and a policy it serves is not DatapathCompatible; all clear once the
    rule goes."""
    node = sorted(node_ips)[-1]
    peer = sorted(node_ips)[0]
    ip = node_ips[node]
    ssh(ip, "sudo nft add table inet purelb_e2e_block && "
            "sudo nft 'add chain inet purelb_e2e_block in { type filter hook input priority 0; policy accept; }' && "
            "sudo nft add rule inet purelb_e2e_block in udp dport 6082 drop")
    try:
        wait_nodes(cluster, lambda: node_condition(cluster, node, "ForwardPath")[1] == "TunnelPortBlocked", 150,
                   f"{node} reporting TunnelPortBlocked")
        wait_nodes(cluster, lambda: node in node_condition(cluster, peer, "TunnelPeersReachable")[2], 60,
                   f"{peer} reporting {node} unreachable")
        wait_until(lambda: policy_condition(cluster, "routed", "DatapathCompatible") == ("False", "TunnelPortBlocked"),
                   timeout=60, description="the routed policy not DatapathCompatible")
    finally:
        ssh(ip, "sudo nft delete table inet purelb_e2e_block", check=False)
    wait_nodes(cluster, lambda: node_condition(cluster, node, "ForwardPath")[0] == "True" and
               node_condition(cluster, peer, "TunnelPeersReachable")[0] == "True", 150, "the conditions clear")
    wait_until(lambda: policy_condition(cluster, "routed", "DatapathCompatible")[0] == "True", timeout=60,
               description="the policy compatible again")


def test_duplicate_address_is_reported_not_fought(cluster: Cluster, node_ips: Dict[str, str], topo: topology.Topology,
                                                  agent_metrics, env: Env) -> None:
    """Another host takes the holder's IPv6 address: DAD fails and the
    kernel deletes it; the holder reports DADFailed and does not re-add or
    announce it (backoff), the policy says AddressUnique=False; once the
    duplicate goes, it is placed again and announced after DAD passes."""
    h = holder(cluster, env.v6)
    other = next(n for n in env.sub.nodes if n != h)
    d = nodes.address_detail(node_ips[h], env.v6)
    dup_iface = topo.node_iface[other]
    na0 = agent_metrics(h).value(NA_SENT)
    ssh(node_ips[other], f"sudo ip netns add purelb-dad && "
        f"sudo ip link add purelb-dad0 link {dup_iface} type macvlan mode bridge && "
        f"sudo ip link set purelb-dad0 netns purelb-dad && sudo ip -n purelb-dad link set purelb-dad0 up && "
        f"sudo ip -n purelb-dad addr add {env.v6}/128 dev purelb-dad0 nodad")
    try:
        since = utcnow()
        ssh(node_ips[h], f"sudo ip -6 addr del {d.cidr} dev {d.interface}")
        wait_nodes(cluster, lambda: (env.v6, "DADFailed") in faults(cluster, h), 30, f"{h} reporting DADFailed")
        failed = time.monotonic()
        placed = agent_log(cluster, h, since).count(f'"ip":"{env.v6}","key"')
        # Inside the 30 s backoff nothing may happen: no placing again, no
        # announcement, no address.
        time.sleep(20)
        assert time.monotonic() - failed < 30, "still inside the backoff"
        again = agent_log(cluster, h, since).count(f'"ip":"{env.v6}","key"')
        assert again == placed, f"placed {again - placed} more times inside the backoff: fought"
        assert agent_metrics(h).value(NA_SENT) == na0, "no NA for an address another host has"
        assert not nodes.has_address(node_ips[h], env.v6)
        wait_until(lambda: policy_condition(cluster, "local", "AddressUnique") == ("False", "DADFailed"), timeout=60,
                   description="the policy's AddressUnique=False")
    finally:
        ssh(node_ips[other], "sudo ip netns del purelb-dad", check=False)
    wait_until(lambda: (env.v6, "DADFailed") not in faults(cluster, h), timeout=180,
               description="placed again once the duplicate went")
    detail = nodes.address_detail(node_ips[h], env.v6)
    assert detail is not None and not detail.has_flag("tentative")
    wait_until(lambda: agent_metrics(h).value(NA_SENT) >= na0 + ANNOUNCEMENTS, timeout=30,
               description="announced once DAD passed")
    wait_until(lambda: policy_condition(cluster, "local", "AddressUnique")[0] == "True", timeout=60,
               description="the policy's AddressUnique=True")


def test_disable_removes_everything_and_enable_rebuilds(cluster: Cluster, node_ips: Dict[str, str], router: Router,
                                                         env: Env) -> None:
    """Egress off (D26): every node removes its tunnel, hardening table,
    pins and addresses, says so in its log, and reports egress off with no
    acks or conditions; the routed paths leave the router. Egress on again
    rebuilds all of it."""
    since = utcnow()
    apply_agent(cluster, False)
    wait_until(lambda: not any(egress.capable_nodes(cluster).values()), timeout=120,
               description="egress off on every node")
    for n, ip in node_ips.items():
        wait_until(lambda ip=ip: "does not exist" in ssh(ip, f"ip link show {TUNNEL} 2>&1 || true", check=False),
                   timeout=60, description=f"{n}: the tunnel removed")
        assert "purelb_egress" not in ssh(ip, "sudo nft list tables", check=False), n
        assert egress_pins(ip) == [], f"{n}: egress pins left"
        assert not nodes.has_address(ip, env.r4), n
        st = (node_states(cluster).get(n) or {}).get("status") or {}
        assert not st.get("announcing") and not st.get("conditions"), f"{n}: status cleared"
        assert "egress datapath removed" in agent_log(cluster, n, since), n
    assert nodes.nodes_with_address(node_ips, env.v4) == [], "the local address withdrawn"
    wait_until(lambda: not router.nexthops(f"{env.r4}/32"), timeout=90, description="the routed paths gone")

    apply_agent(cluster, True)
    wait_until(lambda: all_capable(cluster, node_ips), timeout=180, description="egress back on every node")
    for n, ip in node_ips.items():
        assert TUNNEL in ssh(ip, f"ip -br link show {TUNNEL}", check=False), n
    for a in (env.v4, env.v6):
        wait_until(lambda a=a: len(announcers(cluster, a)) == 1, timeout=120, description=f"one holder of {a}")
    wait_until(lambda: len(router.nexthops(f"{env.r4}/32")) == len(node_ips), timeout=120,
               description="the routed paths back")

