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

"""Egress: the announcer side (Phase 4) and the pod-node stage (Phase 5).

One flow, which later phases extend inline. What Phase 4 shows without
any egress traffic:

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

What Phase 5 shows, with the pod's traffic to the router (the routed
policy, ranked higher, takes the router's own addresses; the local policy
everything else):

* the pod's flows leave from its policy's address, in its node's slice,
  in both families and on both paths, as the router sees them and as the
  flow log writes them;
* another rule using the mark byte moves an unset markField; set
  explicitly, it is reported MarkConflict instead (D18);
* a forward filter dropping the pod's flows is reported ForwardBlocked
  (D25);
* an agent restart repoints the node's slots through the tunnel first, so
  the pod's flows on a local and a routed address keep going (D7);
* a pod gone has its flows reset and its node's slice drained and freed;
* turning egress off resets egress's flows and src_valid_mark.

WHAT THIS DOES TO THE CLUSTER: egress on every node through the `default`
LBNodeAgent (already so under --egress), with GARP/NA configured so the
takeover can be seen on the wire; any other LBNodeAgent enabling egress
is set aside for the module and put back; two ServiceGroups (.221-.223 /
ee:: and the routed 10.252.0.0/24 / fd00:10:252::/64), a namespace with
one pod and two Egresses. All of it is put back afterwards. The
duplicate-address case puts a macvlan in a network namespace on another
node of the holder's subnet: a host on the same link that egress does not
manage. The Phase 5 cases add nft tables on a node (removed after) and
move the pod between nodes.
"""

from __future__ import annotations

import json
import re
import threading
import time
from dataclasses import dataclass
from typing import Callable, Dict, List, Optional, Tuple

import pytest
from kubernetes import client
from kubernetes.client.rest import ApiException
from kubernetes.stream import stream

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
    # Destinations: the router's own addresses take the routed policy, its
    # address on the local pool's subnet the local one.
    routed_dst4: str
    routed_dst6: str
    local_dst4: str
    local_dst6: str


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


def apply_agent(cluster: Cluster, on: bool, mark_field: str = "", flow_log: bool = False) -> None:
    """The `default` LBNodeAgent with egress on or off, GARP/NA configured
    and the session's address guard; markField and flowLog as given."""
    local = guard.local_spec({"localInterface": "default", "dummyInterface": "kube-lb0",
                              "garpConfig": {"enabled": True}})
    eg: dict = {"enabled": on}
    if mark_field:
        eg["markField"] = mark_field
    if flow_log:
        eg["flowLog"] = True
    cluster.apply_cr({
        "apiVersion": "purelb.io/v2", "kind": "LBNodeAgent",
        "metadata": {"name": "default", "namespace": cluster.purelb_namespace},
        "spec": {"local": local, "egress": eg},
    })


def all_capable(cluster: Cluster, node_ips: Dict[str, str]) -> bool:
    caps = egress.capable_nodes(cluster)
    return all(caps.get(n) for n in node_ips)


def sg_body(name: str, spec: dict) -> dict:
    return {"apiVersion": "purelb.io/v2", "kind": "ServiceGroup",
            "metadata": {"name": name, "namespace": "purelb-system"},
            "spec": {**spec, "egress": {"allowed": True, "affinityModes": ["Hash"], "sliceSize": 4096}}}


def egress_body(name: str, group: str, cidrs: List[str], priority: int = 0) -> dict:
    return {"apiVersion": "purelb.io/v2", "kind": "Egress", "metadata": {"name": name, "namespace": NS},
            "spec": {"serviceGroup": group, "podSelector": {"matchLabels": {"app": "egress-e2e"}},
                     "priority": priority, "destinations": {"include": [{"cidrs": cidrs}]}}}


def router_addresses(router: Router, iface: Optional[str] = None) -> Tuple[str, str]:
    """The router's IPv4 and a global IPv6 address on `iface`, or on the
    interface holding its management address."""
    for line in ssh(router.host, "ip -br addr show").splitlines():
        parts = line.split()
        if len(parts) < 3:
            continue
        addrs = parts[2:]
        if (iface and parts[0].split("@")[0] != iface) or (not iface and f"{router.host}/" not in " ".join(addrs)):
            continue
        v4 = next(a.split("/")[0] for a in addrs if "." in a)
        v6 = sorted((a.split("/")[0] for a in addrs if ":" in a and a.endswith("/64") and not a.startswith("fe80")),
                    key=len)
        return v4, v6[0]
    raise AssertionError(f"no router addresses on {iface or router.host}")


def client_pod(cluster: Cluster) -> client.V1Pod:
    """The test pod, once its Deployment's rollout is complete: the one
    pod, Running. A pod picked mid-rollout is the old one, about to go."""
    def ready():  # noqa: ANN202
        d = cluster.apps.read_namespaced_deployment("egress-e2e", NS)
        st = d.status
        if (st.observed_generation or 0) < d.metadata.generation or st.updated_replicas != 1 or \
                st.replicas != 1 or st.available_replicas != 1:
            return None
        pods = cluster.core.list_namespaced_pod(NS, label_selector="app=egress-e2e").items
        if len(pods) != 1:
            return None
        p = pods[0]
        if p.status.phase == "Running" and p.metadata.deletion_timestamp is None and p.status.pod_ip:
            return p
        return None
    return wait_until(ready, timeout=180, description="the egress-e2e rollout complete, its pod running")


def pod_exec(cluster: Cluster, pod: str, command: str, timeout: int = 60) -> str:
    """Run `command` (sh) in the test pod, returning its output. On an API
    client of its own: stream() swaps its client's request function while
    it runs, which breaks every other call made on that client meanwhile."""
    own = client.CoreV1Api(client.ApiClient(cluster.core.api_client.configuration))
    return stream(own.connect_get_namespaced_pod_exec, pod, NS, command=["sh", "-c", command],
                  stderr=True, stdin=False, stdout=True, tty=False, _request_timeout=timeout)


def in_background(fn: Callable[[], str]) -> Callable[[], str]:
    """Run fn on a thread; the returned call waits for its result."""
    out: List[str] = []
    t = threading.Thread(target=lambda: out.append(fn()), daemon=True)
    t.start()

    def result() -> str:
        t.join(timeout=300)
        return out[0] if out else ""
    return result


# A TCP connection from the pod; its SYN is what the router captures.
CONNECT = ("python3 -c \"import socket,sys; a=sys.argv[1]; "
           "s=socket.socket(socket.AF_INET6 if ':' in a else socket.AF_INET); s.settimeout(3); "
           "s.connect((a, 22)); s.recv(8); s.close()\" {}")


def ping_lost(output: str) -> int:
    """Packets a ping summary says were lost."""
    m = re.search(r"(\d+) packets transmitted, (\d+) (?:packets )?received", output)
    assert m, f"no ping summary in: {output[-300:]}"
    return int(m.group(1)) - int(m.group(2))


def ping_gaps(output: str) -> List[Tuple[int, int]]:
    """The runs of sequence numbers a ping got no reply to."""
    got = {int(x) for x in re.findall(r"seq=(\d+)", output)}
    sent = int(re.search(r"(\d+) packets transmitted", output).group(1))
    gaps: List[Tuple[int, int]] = []
    for i in range(sent):
        if i in got:
            continue
        if gaps and gaps[-1][1] == i - 1:
            gaps[-1] = (gaps[-1][0], i)
        else:
            gaps.append((i, i))
    return gaps


def router_syns(lines: List[str]) -> List[Tuple[str, int, str]]:
    """(source, port, destination) of each SYN to port 22 a router capture
    shows, IPv4 and IPv6."""
    out = []
    for ln in lines:
        # With -e the IP / IP6 keyword may not precede the addresses.
        m = re.search(r" (\S+)\.(\d+) > (\S+)\.22: Flags \[S\],", ln)
        if m:
            out.append((m.group(1), int(m.group(2)), m.group(3)))
    return out


def node_slice(cluster: Cluster, node: str, address: str) -> Optional[dict]:
    """The node's Active slice of `address`, from its EgressNodeState spec."""
    ns = node_states(cluster).get(node) or {}
    for sl in (ns.get("spec") or {}).get("slices") or []:
        if sl.get("address") == address and sl.get("state") == "Active":
            return sl
    return None


def follow_log(cluster: Cluster, node: str) -> Callable[[], str]:
    """Follow the node's agent log from now until its pod ends; the call
    returns what was read."""
    pod = agent_pod(cluster, node)

    def read() -> str:
        resp = cluster.core.read_namespaced_pod_log(pod.metadata.name, cluster.purelb_namespace, container="lbnodeagent",
                                                    follow=True, since_seconds=5, _preload_content=False)
        return b"".join(resp.stream()).decode(errors="replace")
    return in_background(read)


def flow_lines(log: str) -> List[dict]:
    """The flow log's lines in an agent log."""
    out = []
    for ln in log.splitlines():
        if '"op":"egressFlow"' in ln and '"egress flow"' in ln:
            out.append(json.loads(ln))
    return out


def pin_pod(cluster: Cluster, node: str = "", avoid: str = "") -> None:
    """Move the test pod onto `node`, or off `avoid`, or let it go anywhere."""
    expr = []
    if node:
        expr = [{"key": "kubernetes.io/hostname", "operator": "In", "values": [node]}]
    elif avoid:
        expr = [{"key": "kubernetes.io/hostname", "operator": "NotIn", "values": [avoid]}]
    affinity = {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
        "nodeSelectorTerms": [{"matchExpressions": expr}]}}} if expr else None
    cluster.apps.patch_namespaced_deployment("egress-e2e", NS, {"spec": {"template": {"spec": {"affinity": affinity}}}})


# ---------------------------------------------------------------- fixture


@pytest.fixture(scope="module")
def env(request: pytest.FixtureRequest, cluster: Cluster, topo: topology.Topology, node_ips: Dict[str, str],
        router: Router, lbnodeagent: str):
    sub = next((s for s in topo.subnets if len(s.nodes) >= 2 and s.v6), None)
    if sub is None:
        pytest.skip("needs a subnet with two nodes and IPv6")
    # Another LBNodeAgent enabling egress would keep it on, and its own
    # settings, on its nodes: set aside for the module.
    aside = [a for a in cluster.list_crs("lbnodeagent")
             if a["metadata"]["name"] != "default" and ((a.get("spec") or {}).get("egress") or {}).get("enabled")]
    for a in aside:
        cluster.delete_cr("lbnodeagent", a["metadata"]["name"])

    def put_back() -> None:
        for a in aside:
            cluster.apply_cr({"apiVersion": a["apiVersion"], "kind": a["kind"],
                              "metadata": {"name": a["metadata"]["name"], "namespace": a["metadata"]["namespace"],
                                           "labels": a["metadata"].get("labels") or {}},
                              "spec": a["spec"]})
    request.addfinalizer(put_back)
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
                                  "name": "c", "image": backend.IMAGE, "command": ["sleep", "infinity"]}],
                                  # So a case can place it on any node, the
                                  # control plane's too.
                                  "tolerations": [{"key": "node-role.kubernetes.io/control-plane",
                                                   "operator": "Exists", "effect": "NoSchedule"}]}}}})
    routed_dst4, routed_dst6 = router_addresses(router)
    local_dst4, local_dst6 = router_addresses(router, router.interface_for_subnet(sub.v4.split("/")[0]))
    cluster.custom.create_namespaced_custom_object(GROUP, VERSION, NS, "egresses",
                                                   egress_body("local", LOCAL_SG, ["0.0.0.0/0", "::/0"]))
    cluster.custom.create_namespaced_custom_object(GROUP, VERSION, NS, "egresses", egress_body(
        "routed", ROUTED_SG, [f"{routed_dst4}/32", f"{routed_dst6}/128"], priority=10))

    local = wait_until(lambda: (lambda a: a if len(a) == 2 else None)(policy_addresses(cluster, "local")),
                       timeout=120, description="the local policy's two addresses")
    routed = wait_until(lambda: (lambda a: a if len(a) == 2 else None)(policy_addresses(cluster, "routed")),
                        timeout=120, description="the routed policy's two addresses")
    e = Env(sub=sub, v4=local["IPv4"], v6=local["IPv6"], r4=routed["IPv4"], r6=routed["IPv6"],
            routed_dst4=routed_dst4, routed_dst6=routed_dst6, local_dst4=local_dst4, local_dst6=local_dst6)
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


CT_DELETED = "purelb_egress_conntrack_deleted_total"


def test_pod_traffic_leaves_from_its_slice(cluster: Cluster, router: Router, env: Env, agent_metrics) -> None:
    """The pod's flows leave from its policy's address and from its node's
    slice of it, both families, on the local and the routed path, as the
    router sees them; the node acks its spec; the flow log, turned on,
    writes each with the same translated source, its pod and policy."""
    pod = client_pod(cluster)
    node = pod.spec.node_name
    since = utcnow()
    apply_agent(cluster, True, flow_log=True)
    wait_until(lambda: '"flow log on"' in agent_log(cluster, node, since), timeout=60, description="flow log on")
    new0 = agent_metrics(node).counter("purelb_egress_flow_log_events_total", event="new")
    cases = [(env.local_dst4, env.v4, "local"), (env.local_dst6, env.v6, "local"),
             (env.routed_dst4, env.r4, "routed"), (env.routed_dst6, env.r6, "routed")]
    try:
        wait_until(lambda: all(node_slice(cluster, node, a) for _, a, _ in cases), timeout=120,
                   description=f"{node}'s Active slices of every address")
        wait_until(lambda: (lambda ns: ns["metadata"]["generation"] == (ns.get("status") or {}).get("observedGeneration"))(
            node_states(cluster)[node]), timeout=60, description=f"{node} acking its spec")
        # Only the egress addresses: the harness's own ssh to the router
        # is SYNs to port 22 too.
        sources = " or ".join(f"src host {a}" for _, a, _ in cases)
        with router.capture("any", f"tcp dst port 22 and ({sources})", seconds=90) as cap:
            for dst, _, _ in cases:
                out = pod_exec(cluster, pod.metadata.name, CONNECT.format(dst))
                assert "Traceback" not in out, f"connecting to {dst}: {out}"
            time.sleep(2)
        syns = router_syns(cap.lines)
        flows = flow_lines(agent_log(cluster, node, since))
        for dst, addr, pol in cases:
            got = [(src, port) for src, port, d in syns if d == dst]
            assert got, f"no SYN to {dst} at the router: {cap.lines}"
            src, port = got[-1]
            assert src == addr, f"to {dst}: from {src}, not the {pol} policy's {addr}"
            sl = node_slice(cluster, node, addr)
            size = sl.get("sliceSize") or 4096
            assert sl["index"] * size <= port <= (sl["index"] + 1) * size - 1, \
                f"{addr} port {port} outside {node}'s slice {sl['index']} ({size})"
            translated = f"[{src}]:{port}" if ":" in src else f"{src}:{port}"
            line = next((f for f in flows if f["event"] == "new" and f["egress"] == translated), None)
            assert line is not None, f"no flow-log line for {translated}: {flows}"
            assert line["pod"] == f"{NS}/{pod.metadata.name}" and line["policy"].endswith(f"{NS}/{pol}"), line
        assert agent_metrics(node).counter("purelb_egress_flow_log_events_total", event="new") >= new0 + len(cases)
    finally:
        since = utcnow()
        apply_agent(cluster, True)
    wait_until(lambda: '"flow log off"' in agent_log(cluster, node, since), timeout=60, description="flow log off")


def test_mark_byte_moves_off_a_used_byte(cluster: Cluster, node_ips: Dict[str, str], env: Env, agent_metrics) -> None:
    """Another rule on the pod's node setting a bit of the mark byte after
    classify: with markField unset the node moves to the next free byte
    and the pod's flows still go; set explicitly to that byte it stays and
    reports MarkConflict, on the node and the policies, until the rule
    goes; unset again, the byte in use stays (D18)."""
    pod = client_pod(cluster)
    node, ip = pod.spec.node_name, node_ips[pod.spec.node_name]

    def mark_field() -> str:
        return (((node_states(cluster).get(node) or {}).get("status") or {}).get("capability") or {}).get("markField", "")
    assert mark_field() == "0x000000ff"
    moves0 = agent_metrics(node).counter("purelb_egress_mark_moves_total")
    since = utcnow()
    # A rule that matches nothing, so no traffic changes; it still uses
    # the byte.
    ssh(ip, "sudo nft add table ip purelb_e2e_mark && "
            "sudo nft 'add chain ip purelb_e2e_mark post { type filter hook postrouting priority 0; }' && "
            "sudo nft add rule ip purelb_e2e_mark post ip daddr 192.0.2.255 meta mark set meta mark or 0x1")
    try:
        wait_nodes(cluster, lambda: mark_field() == "0x00ff0000", 120, f"{node} moving off 0x000000ff")
        assert "moved egress's mark byte" in agent_log(cluster, node, since)
        assert agent_metrics(node).counter("purelb_egress_mark_moves_total") == moves0 + 1
        for dst in (env.routed_dst4, env.local_dst6):
            out = pod_exec(cluster, pod.metadata.name, CONNECT.format(dst))
            assert "Traceback" not in out, f"after the move, to {dst}: {out}"

        apply_agent(cluster, True, mark_field="0x000000ff")
        wait_nodes(cluster, lambda: node_condition(cluster, node, "DatapathCompatible")[1] == "MarkConflict", 120,
                   f"{node} reporting MarkConflict")
        assert mark_field() == "0x000000ff", "an explicit markField is never moved"
        assert "ip purelb_e2e_mark post (sets mark 0x1)" in node_condition(cluster, node, "DatapathCompatible")[2]
        assert agent_metrics(node).get("purelb_egress_mark_conflict") == 1
        wait_until(lambda: policy_condition(cluster, "routed", "DatapathCompatible") == ("False", "MarkConflict"),
                   timeout=60, description="the routed policy not DatapathCompatible")
    finally:
        ssh(ip, "sudo nft delete table ip purelb_e2e_mark", check=False)
    wait_nodes(cluster, lambda: node_condition(cluster, node, "DatapathCompatible")[0] == "True", 120,
               "MarkConflict cleared")
    assert agent_metrics(node).get("purelb_egress_mark_conflict") == 0
    wait_until(lambda: policy_condition(cluster, "routed", "DatapathCompatible")[0] == "True", timeout=60,
               description="the policy compatible again")
    apply_agent(cluster, True)
    time.sleep(35)  # past a reconcile, which would move it if it moved
    assert mark_field() == "0x000000ff", "unset again, the byte in use stays while it is free"


def test_forward_blocked_is_reported(cluster: Cluster, node_ips: Dict[str, str], env: Env, agent_metrics) -> None:
    """A forward filter on the pod's node dropping the pod's flows: the
    node reports ForwardPath=False ForwardBlocked for both families, the
    policy is not DatapathCompatible; all clear once the filter goes
    (D25)."""
    pod = client_pod(cluster)
    node, ip = pod.spec.node_name, node_ips[pod.spec.node_name]
    p4 = next(a.ip for a in pod.status.pod_ips if "." in a.ip)
    p6 = next(a.ip for a in pod.status.pod_ips if ":" in a.ip)
    since = utcnow()
    # New flows all along: one ping process each, each a new flow.
    loop = in_background(lambda: pod_exec(cluster, pod.metadata.name,
                                          "rm -f /tmp/stop; while [ ! -f /tmp/stop ]; do "
                                          f"ping -c1 -W1 {env.routed_dst4} >/dev/null; "
                                          f"ping -6 -c1 -W1 {env.routed_dst6} >/dev/null; sleep 1; done; echo stopped",
                                          timeout=600))
    ssh(ip, "sudo nft add table inet purelb_e2e_fwd && "
            "sudo nft 'add chain inet purelb_e2e_fwd f { type filter hook forward priority 0; }' && "
            f"sudo nft add rule inet purelb_e2e_fwd f ip saddr {p4} drop && "
            f"sudo nft add rule inet purelb_e2e_fwd f ip6 saddr {p6} drop")
    try:
        wait_nodes(cluster, lambda: node_condition(cluster, node, "ForwardPath")[1] == "ForwardBlocked" and
                   "IPv4, IPv6" in node_condition(cluster, node, "ForwardPath")[2], 240, f"{node} reporting ForwardBlocked")
        m = agent_metrics(node)
        assert m.get("purelb_egress_forward_blocked", family="IPv4") == 1
        assert m.get("purelb_egress_forward_blocked", family="IPv6") == 1
        wait_until(lambda: policy_condition(cluster, "routed", "DatapathCompatible") == ("False", "ForwardBlocked"),
                   timeout=60, description="the routed policy not DatapathCompatible")
        assert "ForwardBlocked: pods' new egress flows" in agent_log(cluster, node, since)
    finally:
        ssh(ip, "sudo nft delete table inet purelb_e2e_fwd", check=False)
    try:
        wait_nodes(cluster, lambda: node_condition(cluster, node, "ForwardPath")[0] == "True", 120, "ForwardBlocked cleared")
        m = agent_metrics(node)
        assert m.get("purelb_egress_forward_blocked", family="IPv4") == 0
        assert m.get("purelb_egress_forward_blocked", family="IPv6") == 0
        assert "ForwardBlocked cleared" in agent_log(cluster, node, since)
        wait_until(lambda: policy_condition(cluster, "routed", "DatapathCompatible")[0] == "True", timeout=60,
                   description="the policy compatible again")
    finally:
        pod_exec(cluster, pod.metadata.name, "touch /tmp/stop")
        assert "stopped" in loop()


def test_flows_survive_an_agent_restart(cluster: Cluster, node_ips: Dict[str, str], env: Env) -> None:
    """The pod on the node holding the local IPv4 address, which also
    advertises the routed ones, its flows direct: restarting that node's
    agent repoints its slots through the tunnel to the holders that remain
    before withdrawing (D7); the pod's routed pings keep going, its local
    ones lose only the address's two takeovers (off the node with its
    Lease, back with the next agent); the next agent takes the node's
    addresses and direct paths back."""
    h = holder(cluster, env.v4)
    pin_pod(cluster, node=h)
    try:
        pod = wait_until(lambda: (lambda p: p if p.spec.node_name == h else None)(client_pod(cluster)), timeout=180,
                         description=f"the pod on {h}")
        wait_until(lambda: all(node_slice(cluster, h, a) for a in (env.v4, env.r4, env.r6)), timeout=120,
                   description=f"{h}'s Active slices")
        log = follow_log(cluster, h)
        pings = {name: in_background(lambda cmd=cmd: pod_exec(cluster, pod.metadata.name, cmd, timeout=90))
                 for name, cmd in (("local IPv4", f"ping -i 0.2 -w 25 {env.local_dst4}"),
                                   ("routed IPv4", f"ping -i 0.2 -w 25 {env.routed_dst4}"),
                                   ("routed IPv6", f"ping -6 -i 0.2 -w 25 {env.routed_dst6}"))}
        time.sleep(5)
        old = agent_pod(cluster, h)
        cluster.core.delete_namespaced_pod(old.metadata.name, cluster.purelb_namespace)
        text = log()
        repointed = {}
        for ln in text.splitlines():
            if '"op":"egressRepoint"' in ln and '"slot"' in ln:
                m = json.loads(ln)
                repointed[m["address"]] = m["holders"]
        for a in (env.v4, env.r4, env.r6):
            assert a in repointed, f"{a}'s slot not repointed: {repointed}\n{text[-2000:]}"
            assert repointed[a] and h not in repointed[a], f"{a} repointed to {repointed[a]}"
        for name, result in pings.items():
            out = result()
            gaps = ping_gaps(out)
            if name.startswith("routed"):
                # A routed address doesn't move: the repoint carries it.
                assert ping_lost(out) <= 1, f"{name}: lost {gaps} (0.2 s apart; agent deleted at seq ~25)"
            else:
                # A local address moves off the node with its Lease and back
                # when the next agent rejoins: two takeovers, as an LB
                # address's (user, 2026-11-02), each bounded.
                assert len(gaps) <= 2 and all(b - a + 1 <= 15 for a, b in gaps), \
                    f"{name}: lost {gaps} (0.2 s apart): more than two takeovers of up to 3 s"
        wait_until(lambda: (lambda p: p is not None and p.metadata.name != old.metadata.name and
                            p.status.phase == "Running")(agent_pod(cluster, h)), timeout=120,
                   description=f"{h}'s new agent")
        wait_until(lambda: announcers(cluster, env.v4) == [h], timeout=120, description=f"{h} holding {env.v4} again")
    finally:
        pin_pod(cluster)


def test_deleted_pods_flows_are_reset_and_drained(cluster: Cluster, env: Env, agent_metrics) -> None:
    """The pod moved off its node with a flow open: the node deletes the
    flow's conntrack entry and says so, its slice drains (no flow left) and
    is freed, long before the drain maximum."""
    pod = client_pod(cluster)
    n = pod.spec.node_name
    assert node_slice(cluster, n, env.v4)
    deleted0 = agent_metrics(n).counter(CT_DELETED, reason="podDeleted")
    since = utcnow()

    def ping() -> str:
        try:
            return pod_exec(cluster, pod.metadata.name, f"ping -i 0.5 -w 60 {env.local_dst4}", timeout=90)
        except Exception as exc:  # noqa: BLE001 - the pod goes under it
            return str(exc)
    in_background(ping)
    time.sleep(3)
    pin_pod(cluster, avoid=n)
    try:
        wait_until(lambda: (lambda p: p if p.spec.node_name != n else None)(client_pod(cluster)), timeout=180,
                   description=f"the pod off {n}")
        wait_until(lambda: agent_metrics(n).counter(CT_DELETED, reason="podDeleted") > deleted0, timeout=90,
                   description=f"{n} deleting the pod's flows")
        assert "deleted the flows of pods gone from this node" in agent_log(cluster, n, since)
        wait_until(lambda: not any(sl.get("address") == env.v4 for sl in
                                   ((node_states(cluster).get(n) or {}).get("spec") or {}).get("slices") or []),
                   timeout=150, interval=3, description=f"{n}'s slice of {env.v4} drained and freed")
    finally:
        pin_pod(cluster)


def test_disable_removes_everything_and_enable_rebuilds(cluster: Cluster, node_ips: Dict[str, str], router: Router,
                                                         env: Env, agent_metrics) -> None:
    """Egress off (D26): every node removes its tunnel, hardening table,
    pins and addresses, says so in its log, and reports egress off with no
    acks or conditions; the routed paths leave the router; the pod's open
    flow is reset and the holder's pool interface has src_valid_mark off
    again. Egress on again rebuilds all of it."""
    pod = client_pod(cluster)
    n = pod.spec.node_name
    # A pod just moved: its flows are egress's once its node's datapath
    # has it, which its traffic leaving from the address shows.
    def leaves_as_egress() -> bool:
        with router.capture("any", f"tcp dst port 22 and src host {env.v4}", seconds=30) as cap:
            pod_exec(cluster, pod.metadata.name, CONNECT.format(env.local_dst4))
            time.sleep(1)
        return any(src == env.v4 for src, _, _ in router_syns(cap.lines))
    wait_until(leaves_as_egress, timeout=120, interval=5, description=f"the pod on {n} leaving from {env.v4}")
    h = holder(cluster, env.v4)
    iface = nodes.address_detail(node_ips[h], env.v4).interface
    svm = f"cat /proc/sys/net/ipv4/conf/{iface}/src_valid_mark"
    assert ssh(node_ips[h], svm).strip() == "1", f"{h} {iface}: src_valid_mark on while it holds {env.v4}"
    torn0 = agent_metrics(n).counter(CT_DELETED, reason="teardown")
    pod4 = next(a.ip for a in pod.status.pod_ips if "." in a.ip)
    ping = in_background(lambda: pod_exec(cluster, pod.metadata.name, f"ping -i 0.5 -w 20 {env.local_dst4}",
                                          timeout=60))

    def marked_flow() -> bool:
        """The ping's conntrack entry on n, carrying a slot (D18)."""
        out = ssh(node_ips[n], f"sudo grep 'icmp.*src={pod4} dst={env.local_dst4} ' /proc/net/nf_conntrack || true",
                  check=False)
        return any(0 < int(m) & 0xff < 0x80 for m in re.findall(r"mark=(\d+)", out))
    wait_until(marked_flow, timeout=15, interval=1, description=f"the pod's flow on {n}, marked with a slot")
    since = utcnow()
    apply_agent(cluster, False)
    wait_until(lambda: not any(egress.capable_nodes(cluster).values()), timeout=120,
               description="egress off on every node")
    for node, ip in node_ips.items():
        wait_until(lambda ip=ip: "does not exist" in ssh(ip, f"ip link show {TUNNEL} 2>&1 || true", check=False),
                   timeout=60, description=f"{node}: the tunnel removed")
        assert "purelb_egress" not in ssh(ip, "sudo nft list tables", check=False), node
        assert egress_pins(ip) == [], f"{node}: egress pins left"
        assert not nodes.has_address(ip, env.r4), node
        st = (node_states(cluster).get(node) or {}).get("status") or {}
        assert not st.get("announcing") and not st.get("conditions"), f"{node}: status cleared"
        assert "egress datapath removed" in agent_log(cluster, node, since), node
    assert nodes.nodes_with_address(node_ips, env.v4) == [], "the local address withdrawn"
    wait_until(lambda: not router.nexthops(f"{env.r4}/32"), timeout=90, description="the routed paths gone")
    assert agent_metrics(n).counter(CT_DELETED, reason="teardown") > torn0, f"{n} reset the pod's flow: {ping()[-400:]}"
    assert "reset egress's flows" in agent_log(cluster, n, since)
    assert ssh(node_ips[h], svm).strip() == "0", f"{h} {iface}: src_valid_mark off again"

    apply_agent(cluster, True)
    wait_until(lambda: all_capable(cluster, node_ips), timeout=180, description="egress back on every node")
    for n, ip in node_ips.items():
        assert TUNNEL in ssh(ip, f"ip -br link show {TUNNEL}", check=False), n
    for a in (env.v4, env.v6):
        wait_until(lambda a=a: len(announcers(cluster, a)) == 1, timeout=120, description=f"one holder of {a}")
    wait_until(lambda: len(router.nexthops(f"{env.r4}/32")) == len(node_ips), timeout=120,
               description="the routed paths back")

