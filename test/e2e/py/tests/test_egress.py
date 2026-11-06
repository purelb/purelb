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

What Phase 6 shows, with the CNI plugin (D34, D35, design 8.1):

* purelb-cni chains the plugin on every egress node and installs its
  binary; each node says it is programmed, and keeps a snapshot;
* a FailClosed pod starting on a node with no slice of its address waits
  in its ADD, so from its first packet its traffic leaves from the
  address, never the node's; its sandbox is recorded;
* an agent start reports a sandbox with no record, and the next selected
  pod's ADD sends the stage probe, which never reaches the router;
* with no agent answering, the snapshot holds back a FailClosed pod and
  lets an unselected one start;
* the Helm pre-delete hook's subcommand disables egress and waits for
  every node; teardown takes the plugin out of the chain first;
* a node rebooted with a pod that talks from its first moment: the router
  never sees its traffic from the node's address.

WHAT THIS DOES TO THE CLUSTER (Phase 6 also: moves an agent's socket
aside for a while, runs the uninstall hook's Job with its own
ServiceAccount, and reboots a worker node): egress on every node through the `default`
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
import subprocess
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


def on_ebpf_stage(cluster: Cluster, node: str) -> bool:
    """The node runs the eBPF pod stage (design 6.1, 6.4)."""
    st = ((node_states(cluster).get(node) or {}).get("status") or {}).get("capability") or {}
    return st.get("podNodeStage") == "EBPF"


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


CNI_CONF, CNI_BIN, PLUGIN = "/etc/cni/net.d", "/opt/cni/bin", "purelb-egress"
RECORDS = "/run/purelb/sandboxes"
UNRECORDED = "EgressSandboxUnrecorded"

# A pod that tries a TCP connection to argv[1] port 22 every 0.2 s from
# the moment it starts: its first packets are what the router sees.
TALK = ("import socket,sys,time\na=sys.argv[1]\nwhile True:\n"
        " s=socket.socket(socket.AF_INET6 if ':' in a else socket.AF_INET)\n s.settimeout(1)\n"
        " try: s.connect((a,22))\n except Exception: pass\n s.close()\n time.sleep(0.2)")


def cni_chain(host: str) -> Tuple[str, List[str]]:
    """The node's primary CNI configuration (libcni's: the lexically first
    .conf, .conflist or .json) and its chain's plugin types."""
    names = sorted(n for n in ssh(host, f"sudo ls {CNI_CONF}").split()
                   if n.endswith((".conf", ".conflist", ".json")))
    assert names, f"{host}: no CNI configuration"
    conf = json.loads(ssh(host, f"sudo cat {CNI_CONF}/{names[0]}"))
    return names[0], [p.get("type", "") for p in conf.get("plugins", [conf])]


def has_plugin_binary(host: str) -> bool:
    return ssh(host, f"sudo test -x {CNI_BIN}/{PLUGIN} && echo yes || true", check=False).strip() == "yes"


def node_json(host: str, path: str) -> Optional[dict]:
    """A JSON file on the node, None when it isn't there."""
    out = ssh(host, f"sudo cat {path} 2>/dev/null || true", check=False).strip()
    return json.loads(out) if out else None


def records_of(host: str, pod: str) -> List[str]:
    """The plugin's sandbox records on the node naming `pod` (a DNS label,
    so safe unquoted; the directory is root's)."""
    return ssh(host, f"sudo sh -c 'grep -l -F {pod} {RECORDS}/* 2>/dev/null' || true", check=False).split()


def unrecorded_events(cluster: Cluster, namespace: str, pod: str) -> List[client.CoreV1Event]:
    return cluster.core.list_namespaced_event(
        namespace, field_selector=f"involvedObject.name={pod},reason={UNRECORDED}").items


def talker(cluster: Cluster, name: str, namespace: str, node: str, dst: str, labels: Dict[str, str]) -> None:
    """A pod on `node` connecting to dst from its first moment (TALK)."""
    cluster.core.create_namespaced_pod(namespace, {
        "apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": labels},
        "spec": {"terminationGracePeriodSeconds": 1, "nodeSelector": {"kubernetes.io/hostname": node},
                 "tolerations": [{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists",
                                  "effect": "NoSchedule"}],
                 "containers": [{"name": "c", "image": backend.IMAGE, "command": ["python3", "-c", TALK, dst]}]}})


def running(cluster: Cluster, namespace: str, name: str) -> Optional[client.V1Pod]:
    try:
        p = cluster.core.read_namespaced_pod(name, namespace)
    except ApiException as exc:
        if exc.status == 404:
            return None
        raise
    return p if p.status.phase == "Running" and p.status.pod_ip else None


def delete_pod_now(cluster: Cluster, namespace: str, name: str) -> None:
    try:
        cluster.core.delete_namespaced_pod(name, namespace, grace_period_seconds=0)
    except ApiException as exc:
        if exc.status != 404:
            raise
    wait_until(lambda: running(cluster, namespace, name) is None and not any(
        p.metadata.name == name for p in cluster.core.list_namespaced_pod(namespace).items),
        timeout=120, description=f"pod {namespace}/{name} gone")


def my_address(host: str) -> str:
    """This host's source address toward `host` (the harness's own ssh)."""
    out = subprocess.run(["ip", "route", "get", host], capture_output=True, text=True, check=True).stdout  # noqa: S603,S607
    return re.search(r" src (\S+)", out).group(1)


def run_uninstall_hook(cluster: Cluster, image: str, timeout: str = "3m") -> str:
    """The Helm pre-delete hook as the chart has it (D26): its own
    ServiceAccount, a ClusterRole listing and patching only, and a Job
    running `lbnodeagent egress-uninstall`; returns the Job's log once it
    succeeds. Everything it creates is removed."""
    name, ns = "egress-e2e-uninstall", cluster.purelb_namespace
    rbac, batch = client.RbacAuthorizationV1Api(), client.BatchV1Api()
    cluster.core.create_namespaced_service_account(ns, {"metadata": {"name": name}})
    rbac.create_cluster_role({"metadata": {"name": name}, "rules": [
        {"apiGroups": ["purelb.io"], "resources": ["lbnodeagents"], "verbs": ["list", "patch"]},
        {"apiGroups": ["purelb.io"], "resources": ["egressnodestates"], "verbs": ["list"]}]})
    rbac.create_cluster_role_binding({"metadata": {"name": name},
                                      "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": name},
                                      "subjects": [{"kind": "ServiceAccount", "name": name, "namespace": ns}]})
    try:
        batch.create_namespaced_job(ns, {"metadata": {"name": name}, "spec": {"backoffLimit": 0, "template": {"spec": {
            "serviceAccountName": name, "restartPolicy": "Never",
            "containers": [{"name": "uninstall", "image": image, "imagePullPolicy": "Always",
                            "args": ["egress-uninstall", f"--timeout={timeout}"],
                            "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]},
                                                "readOnlyRootFilesystem": True, "runAsNonRoot": True, "runAsUser": 65534}}]}}}})

        def finished():  # noqa: ANN202
            st = batch.read_namespaced_job(name, ns).status
            if st.failed:
                raise AssertionError("the uninstall hook's Job failed")
            return st.succeeded
        wait_until(finished, timeout=300, interval=3, description="the uninstall hook's Job")
        pod = cluster.core.list_namespaced_pod(ns, label_selector=f"job-name={name}").items[0]
        return cluster.pod_log_text(ns, pod.metadata.name, "uninstall")
    finally:
        batch.delete_namespaced_job(name, ns, propagation_policy="Foreground")
        rbac.delete_cluster_role_binding(name)
        rbac.delete_cluster_role(name)
        cluster.core.delete_namespaced_service_account(name, ns)


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
    # purelb-cni answers within its poll, a few seconds after the session starts.
    wait_until(lambda: all(node_condition(cluster, n, "DatapathCompatible")[0] == "True" for n in node_ips), timeout=60,
               description="the plugin chained on every node")

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


def test_cni_plugin_chained_on_every_node(cluster: Cluster, node_ips: Dict[str, str], env: Env, agent_metrics) -> None:
    """purelb-cni chains the plugin on every egress node (D34): its entry
    last in the primary conflist's chain and its binary installed, or,
    where the CNI's own configuration chains it, the entry there; the
    installer reports it Present, no node reports the chain missing; each
    node says what release programmed it and keeps a fresh snapshot for
    the plugin (D35)."""
    for n, ip in node_ips.items():
        wait_until(lambda ip=ip: PLUGIN in cni_chain(ip)[1] and has_plugin_binary(ip), timeout=120,
                   description=f"{n}: the plugin chained and installed")
        name, chain = cni_chain(ip)
        st = node_json(ip, "/run/purelb/cni-status.json") or {}
        assert st.get("chain") == "Present" and st.get("conflist") == name, f"{n}: installer status {st}"
        if st.get("reason") != "CNIOwned":
            assert chain[-1] == PLUGIN, f"{n}: {name} chains {chain}: the plugin goes last"
        status, reason, msg = node_condition(cluster, n, "DatapathCompatible")
        assert reason not in ("ChainMissing", "CNIChainMissing"), f"{n}: {reason}: {msg}"
        assert (node_states(cluster)[n].get("status") or {}).get("programmed"), f"{n}: status.programmed"
        snap = node_json(ip, "/var/lib/purelb/egress-snapshot.json")
        assert snap and snap["version"] == 1 and snap["maxAgeSeconds"] == 3600, f"{n}: snapshot {snap}"
        age = agent_metrics(n).value("purelb_egress_cni_snapshot_age_seconds")
        assert 0 < age < 120, f"{n}: snapshot {age} s old"


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


def test_pod_traffic_leaves_from_its_slice(cluster: Cluster, router: Router, env: Env, agent_metrics,
                                           kube_context: str) -> None:
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
        # The flow log is conntrack's, so netfilter-stage nodes' (design 12.2);
        # on the eBPF stage the flows are in its NAT table.
        ebpf = on_ebpf_stage(cluster, node)
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
            if ebpf:
                continue
            translated = f"[{src}]:{port}" if ":" in src else f"{src}:{port}"
            line = next((f for f in flows if f["event"] == "new" and f["egress"] == translated), None)
            assert line is not None, f"no flow-log line for {translated}: {flows}"
            assert line["pod"] == f"{NS}/{pod.metadata.name}" and line["policy"].endswith(f"{NS}/{pol}"), line
        if ebpf:
            # The gauge moves at each reconcile: a flow that stays open meets one.
            ping = in_background(lambda: pod_exec(cluster, pod.metadata.name, f"ping -i 0.5 -w 60 {env.local_dst4}", timeout=90))
            wait_until(lambda: agent_metrics(node).value("purelb_egress_nat_flows") >= 1, timeout=60,
                       description=f"{node}'s flow in the NAT table's metric")
            ping()
        else:
            assert agent_metrics(node).counter("purelb_egress_flow_log_events_total", event="new") >= new0 + len(cases)
        # Each address's traffic is counted on the pod's node, and kubectl
        # purelb says the same: read at the next reconcile.
        for dst, addr, pol in cases:
            wait_until(lambda addr=addr: agent_metrics(node).counter("purelb_egress_packets_total", ip=addr, direction="out",
                                                                     role="pod_node") >= 1,
                       timeout=60, description=f"{addr}'s packets counted on {node}")
            target = f"[{dst}]:22/tcp" if ":" in dst else f"{dst}:22/tcp"
            trace = guard.plugin_json(kube_context, "egress", "trace", f"{NS}/{pod.metadata.name}", "--dst", target, "--live")
            assert trace["address"] == addr and trace["winner"].endswith(f"{NS}/{pol}"), trace
            assert trace["route"] == ("direct" if node in trace["holders"] else "tunnel"), trace
            assert trace["live"][0]["traffic"]["outPod"]["packets"] >= 1, trace
        dump = guard.plugin_json(kube_context, "egress", "dump", "--node", node)[0]
        assert dump["datapath"]["pinned"], dump
        assert {a for _, a, _ in cases} <= {a["address"] for a in dump["datapath"]["addresses"]}, dump
        assert dump["datapath"]["stage"] == ("EBPF" if ebpf else "Netfilter"), dump
    finally:
        since = utcnow()
        apply_agent(cluster, True)
    wait_until(lambda: '"flow log off"' in agent_log(cluster, node, since), timeout=60, description="flow log off")


def test_a_new_pods_first_packets_leave_as_egress(cluster: Cluster, node_ips: Dict[str, str], router: Router, env: Env,
                                                   agent_metrics) -> None:
    """A FailClosed pod starting on a node with no slice of its policy's
    address (design 8.1): its ADD waits for the node's slice and classifier
    (or ends held), so from its very first packet the router sees its
    traffic only from the policy's address, never from the node's; the
    agent answered the ADD, and the plugin recorded the sandbox, so the
    agent doesn't report it."""
    busy = client_pod(cluster).spec.node_name
    n = next((x for x in sorted(node_ips) if x != busy and not node_slice(cluster, x, env.r4)), None)
    if n is None:
        pytest.skip("every node has a slice of the routed address")
    me = my_address(router.host)
    adds0 = agent_metrics(n).counter("purelb_egress_cni_requests_total", op="ADD")
    since = utcnow()
    name = "egress-e2e-first"
    try:
        with router.capture("any", f"tcp dst port 22 and dst host {env.routed_dst4} and not src host {me}",
                            seconds=150) as cap:
            talker(cluster, name, NS, n, env.routed_dst4, {"app": "egress-e2e"})
            wait_until(lambda: running(cluster, NS, name), timeout=120, description=f"{name} running on {n}")
            time.sleep(5)
        syns = router_syns(cap.lines)
        assert any(src == env.r4 for src, _, _ in syns), f"no SYN from {env.r4}: {cap.lines[-20:]}"
        # Not classified, the pod's traffic leaves from its node's address
        # (the CNI's masquerade) or its own.
        leaks = set(node_ips.values()) | {running(cluster, NS, name).status.pod_ip}
        stray = sorted({src for src, _, _ in syns if src in leaks})
        assert not stray, f"the pod's SYNs left from {stray}, not only {env.r4}"
        assert agent_metrics(n).counter("purelb_egress_cni_requests_total", op="ADD") > adds0
        answers = sum(agent_metrics(n).counter("purelb_egress_cni_requests_total", op="ADD", result=r) for r in ("ready", "held"))
        assert answers > 0, f"{n}: no ADD answered ready or held"
        # What the plugin logged of it, to the runtime's journal.
        added = ssh(node_ips[n], f"sudo journalctl -u containerd --no-pager -n 400 | grep -F cniAdd | grep -F {NS}/{name} || true", check=False)
        assert re.search(r'"result":"(ready|held)"', added), f"{n}: the plugin's ADD: {added[-300:] or 'none'}"
        assert records_of(node_ips[n], name), f"{n}: no record of {name}'s sandbox"
        time.sleep(5)
        assert not unrecorded_events(cluster, NS, name), "reported as started without the plugin"
    finally:
        delete_pod_now(cluster, NS, name)
    # The pod can leave the API before kubelet tears its sandbox down.
    wait_until(lambda: not records_of(node_ips[n], name), timeout=60,
               description=f"{n}: {name}'s record removed at its DEL")


# A selected pod that serves: TCP echoes and UDP echoes, both families.
SERVE = """
import socket, threading
def only(s, fam):
    if fam == socket.AF_INET6:
        s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
def udp(fam, addr):
    s = socket.socket(fam, socket.SOCK_DGRAM)
    only(s, fam)
    s.bind((addr, 8081))
    while True:
        d, a = s.recvfrom(2048)
        s.sendto(d, a)
def tcp(fam, addr):
    s = socket.socket(fam, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    only(s, fam)
    s.bind((addr, 8080))
    s.listen(16)
    while True:
        c, _ = s.accept()
        c.sendall(c.recv(2048))
        c.close()
threads = [threading.Thread(target=f, args=(fam, addr), daemon=True) for f in (udp, tcp)
           for fam, addr in ((socket.AF_INET, "0.0.0.0"), (socket.AF_INET6, "::"))]
[t.start() for t in threads]
[t.join() for t in threads]
"""

# On the router: asks host:port over TCP and UDP. A connected UDP socket only
# hears the address it sent to, so a reply NATed to the egress address is lost.
ASK = """
import socket, sys
host, tp, up = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
fam = socket.AF_INET6 if ":" in host else socket.AF_INET
t = socket.socket(fam, socket.SOCK_STREAM); t.settimeout(5); t.connect((host, tp)); t.sendall(b"hi")
assert t.recv(16) == b"hi"
u = socket.socket(fam, socket.SOCK_DGRAM); u.settimeout(5); u.connect((host, up)); u.send(b"hi")
assert u.recv(16) == b"hi"
print("ok")
"""


def test_flows_into_a_selected_pod_keep_their_addresses(cluster: Cluster, node_ips: Dict[str, str], router: Router,
                                                        env: Env) -> None:
    """A selected pod serving behind a NodePort: its replies aren't new egress
    flows on either stage: netfilter's conntrack knows them; the eBPF stage,
    which the CNI's traffic into the pod bypasses at every host-side hook,
    takes TCP past its SYN and a recorded UDP flow as replies (user decision
    2026-11-04). The client sees its answers from the address it asked, both
    protocols, and both families where the node has IPv6."""
    node = client_pod(cluster).spec.node_name
    name = "egress-e2e-serve"
    cluster.core.create_namespaced_pod(NS, {
        "apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": {"app": "egress-e2e", "serve": name}},
        "spec": {"terminationGracePeriodSeconds": 1, "nodeSelector": {"kubernetes.io/hostname": node},
                 "tolerations": [{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists",
                                  "effect": "NoSchedule"}],
                 "containers": [{"name": "c", "image": backend.IMAGE, "command": ["python3", "-c", SERVE]}]}})
    svc = cluster.core.create_namespaced_service(NS, {
        "apiVersion": "v1", "kind": "Service", "metadata": {"name": name},
        "spec": {"type": "NodePort", "ipFamilyPolicy": "PreferDualStack", "selector": {"serve": name},
                 "ports": [{"name": "t", "port": 8080, "protocol": "TCP"}, {"name": "u", "port": 8081, "protocol": "UDP"}]}})
    try:
        wait_until(lambda: running(cluster, NS, name), timeout=180, description=f"{name} running")
        ports = {p.name: p.node_port for p in svc.spec.ports}
        hosts = [node_ips[node]]
        # The node's own: it may hold the egress addresses too.
        v6 = next((a for a in ssh(node_ips[node], "ip -6 -o addr show scope global | awk '{print $4}' | cut -d/ -f1",
                                  check=False).split() if a not in (env.v6, env.r6)), "")
        if v6 and any(":" in ip for ip in (svc.spec.cluster_ips or [])):
            hosts.append(v6)
        for host in hosts:
            def ask(h: str = host) -> bool:
                nonlocal last
                last = ssh(router.host, f"python3 - {h} {ports['t']} {ports['u']} 2>&1 <<'PY'\n{ASK}\nPY", timeout=30, check=False)
                return "ok" in last
            last = ""
            try:
                wait_until(ask, timeout=60, interval=3, description=f"answers from {host}, TCP and UDP")
            except Exception as exc:
                raise AssertionError(f"{host}: {last[-400:]}") from exc
    finally:
        cluster.core.delete_namespaced_service(name, NS)
        delete_pod_now(cluster, NS, name)
    # The Deployment's selector is app=egress-e2e: it isn't this pod's owner, so
    # the rollout helper must not count it. Gone before the next case.
    wait_until(lambda: not running(cluster, NS, name), timeout=60, description=f"{name} gone")


def test_mark_byte_moves_off_a_used_byte(cluster: Cluster, node_ips: Dict[str, str], env: Env, agent_metrics) -> None:
    """Another rule on the pod's node setting a bit of the mark byte after
    classify: with markField unset the node moves to the next free byte
    (which one depends on the CNI's own marks: Calico's are in
    0x00ff0000) and the pod's flows still go; set explicitly to that byte it stays and
    reports MarkConflict, on the node and the policies, until the rule
    goes; unset again, the byte in use stays (D18). Netfilter stage only: on
    the eBPF stage nothing reads the byte."""
    pod = client_pod(cluster)
    node, ip = pod.spec.node_name, node_ips[pod.spec.node_name]
    if on_ebpf_stage(cluster, node):
        pytest.skip(f"{node} runs the eBPF pod stage, past netfilter's rules")

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
        wait_nodes(cluster, lambda: mark_field() not in ("", "0x000000ff"), 120, f"{node} moving off 0x000000ff")
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
    (D25). Netfilter stage only: the eBPF stage's traffic never meets one
    (its own check is TestNetnsEBPFUpkeep)."""
    pod = client_pod(cluster)
    node, ip = pod.spec.node_name, node_ips[pod.spec.node_name]
    if on_ebpf_stage(cluster, node):
        pytest.skip(f"{node} runs the eBPF pod stage, past the forward hook")
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
        assert m.get("purelb_egress_forward_blocked", family="ipv4") == 1
        assert m.get("purelb_egress_forward_blocked", family="ipv6") == 1
        wait_until(lambda: policy_condition(cluster, "routed", "DatapathCompatible") == ("False", "ForwardBlocked"),
                   timeout=60, description="the routed policy not DatapathCompatible")
        assert "ForwardBlocked: pods' new egress flows" in agent_log(cluster, node, since)
    finally:
        ssh(ip, "sudo nft delete table inet purelb_e2e_fwd", check=False)
    try:
        wait_nodes(cluster, lambda: node_condition(cluster, node, "ForwardPath")[0] == "True", 120, "ForwardBlocked cleared")
        m = agent_metrics(node)
        assert m.get("purelb_egress_forward_blocked", family="ipv4") == 0
        assert m.get("purelb_egress_forward_blocked", family="ipv6") == 0
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


def test_agent_start_reports_and_probes(cluster: Cluster, node_ips: Dict[str, str], router: Router, env: Env,
                                        agent_metrics) -> None:
    """Design 8.1 and 6.1. The pod's sandbox record removed (as for one that
    started without the plugin) and its node's agent restarted: the new
    agent reports the pod, by a log line, a Warning event on it and a
    gauge. The next selected pod's ADD after the start sends the stage
    probe; the stage it finds is what the node reports and classifies by
    (flannel: Mark, classify by mark), and the probe never reaches the
    router."""
    pod = client_pod(cluster)
    n, ip = pod.spec.node_name, node_ips[pod.spec.node_name]
    removed = ssh(ip, f"sudo sh -c 'grep -l -F {pod.metadata.name} {RECORDS}/* | xargs -r rm -v' || true", check=False)
    assert removed.strip(), f"{n}: no record of {pod.metadata.name} to remove"
    restarted = utcnow()
    old = agent_pod(cluster, n)
    cluster.core.delete_namespaced_pod(old.metadata.name, cluster.purelb_namespace)
    wait_until(lambda: (lambda p: p is not None and p.metadata.name != old.metadata.name and
                        p.status.phase == "Running")(agent_pod(cluster, n)), timeout=120, description=f"{n}'s new agent")
    wait_until(lambda: unrecorded_events(cluster, NS, pod.metadata.name), timeout=120,
               description=f"{pod.metadata.name} reported as started without the plugin")
    assert agent_metrics(n).value("purelb_egress_cni_unrecorded_pods") >= 1
    assert "the pod's sandbox started without the CNI plugin" in agent_log(cluster, n)

    since = utcnow()
    try:
        with router.capture("any", f"udp dst port 9 and (dst host 198.51.100.254 or dst host 100::1)", seconds=150) as cap:
            pin_pod(cluster, node=n)
            wait_until(lambda: (lambda p: p if p.spec.node_name == n and p.metadata.name != pod.metadata.name else None)(
                client_pod(cluster)), timeout=180, description=f"a new pod on {n}")
            # A probe, or on the eBPF stage the earlier agent's stage kept: its
            # flows are in the pinned maps, and it may see no new pod.
            line = wait_until(lambda: next((ln for ln in agent_log(cluster, n, restarted).splitlines()
                                            if '"op":"egressStageProbe"' in ln and '"stage"' in ln), None),
                              timeout=60, description=f"{n}'s stage")
        assert not cap.lines, f"the probe reached the router: {cap.lines}"
    finally:
        pin_pod(cluster)
    stage = json.loads(line)["stage"]
    assert stage in ("Mark", "SourceIP", "Bypassed"), line
    if any(d.metadata.name == "kube-flannel-ds" for d in cluster.apps.list_daemon_set_for_all_namespaces().items):
        assert stage == "Mark", f"flannel: pl_pod_tag's mark reaches classify ({line})"
    want = "EBPF" if stage == "Bypassed" else "Netfilter"
    wait_until(lambda: ((node_states(cluster)[n].get("status") or {}).get("capability") or {}).get("podNodeStage") == want,
               timeout=60, description=f"{n} reporting podNodeStage {want}")
    status, reason, _ = node_condition(cluster, n, "DatapathCompatible")
    assert reason != "NetfilterBypassed", f"{n}: {status} {reason}: the eBPF stage serves these pods"
    if stage == "Bypassed":
        assert "pl_pod_out-" in " ".join(egress_pins(ip)), f"{n}: the stage's program isn't on an uplink: {egress_pins(ip)}"
        assert "classify" not in ssh(ip, "sudo nft list table inet purelb_egress", check=False), f"{n}: an nft pod stage too"
    else:
        rule = "vmap @podset" if stage == "Mark" else "vmap @pods4"
        wait_until(lambda: rule in ssh(ip, "sudo nft list chain inet purelb_egress classify", check=False), timeout=60,
                   description=f"{n} classifying with {rule}")


def test_the_snapshot_decides_while_no_agent_answers(cluster: Cluster, node_ips: Dict[str, str], env: Env) -> None:
    """D35: with no agent answering on a node (its socket moved aside), the
    plugin decides by the agent's snapshot. A pod a FailClosed policy
    selects doesn't start: its ADD fails, saying why, and kubelet retries
    until the agent answers again. A pod no policy selects starts, and
    its sandbox is recorded, so the agent doesn't report it."""
    n = sorted(node_ips)[-1]
    ip = node_ips[n]
    other = "egress-e2e-other"
    cluster.core.create_namespace(client.V1Namespace(metadata=client.V1ObjectMeta(name=other)))
    sock = "/run/purelb/egress.sock"
    ssh(ip, f"sudo mv {sock} {sock}.e2e")
    moved = True
    try:
        talker(cluster, "held", NS, n, env.routed_dst4, {"app": "egress-e2e"})
        talker(cluster, "free", other, n, env.routed_dst4, {"app": "free"})
        wait_until(lambda: running(cluster, other, "free"), timeout=120, description="the unselected pod running")
        assert records_of(ip, "free"), f"{n}: the unselected pod's sandbox not recorded"

        def refused() -> bool:
            evs = cluster.core.list_namespaced_event(NS, field_selector="involvedObject.name=held").items
            return any("the PureLB agent isn't running on this node" in (e.message or "") for e in evs)
        wait_until(refused, timeout=90, description="the FailClosed pod's ADD refused by the snapshot")
        assert running(cluster, NS, "held") is None, "a FailClosed pod started with no agent"
        ssh(ip, f"sudo mv {sock}.e2e {sock}")
        moved = False
        wait_until(lambda: running(cluster, NS, "held"), timeout=180, description="the FailClosed pod running")
        time.sleep(10)
        assert not unrecorded_events(cluster, other, "free"), "the snapshot-passed pod reported as unrecorded"
    finally:
        if moved:
            ssh(ip, f"sudo mv {sock}.e2e {sock}", check=False)
        delete_pod_now(cluster, NS, "held")
        cluster.core.delete_namespace(other)


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
                                                         env: Env, agent_metrics, kube_context: str) -> None:
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

    # The node's mark byte (an earlier case may have moved it, D18).
    field = int((((node_states(cluster).get(n) or {}).get("status") or {}).get("capability") or {})
                .get("markField", "0x000000ff"), 16)
    shift = (field & -field).bit_length() - 1

    def marked_flow() -> bool:
        """The ping's conntrack entry on n, carrying a slot (D18)."""
        out = ssh(node_ips[n], f"sudo grep 'icmp.*src={pod4} dst={env.local_dst4} ' /proc/net/nf_conntrack || true",
                  check=False)
        return any(0 < (int(m) >> shift) & 0xff < 0x80 for m in re.findall(r"mark=(\d+)", out))
    if on_ebpf_stage(cluster, n):
        time.sleep(3)  # in the NAT table, which nothing reads from outside (TestNetnsEBPFUpkeep)
    else:
        wait_until(marked_flow, timeout=15, interval=1, description=f"the pod's flow on {n}, marked with a slot")
    since = utcnow()
    # Off as the Helm pre-delete hook turns it off: it waits for every node.
    image = next(c.image for c in agent_pod(cluster, n).spec.containers if c.name == "lbnodeagent")
    hook = run_uninstall_hook(cluster, image)
    assert "every node has torn egress down" in hook, hook
    assert not any(egress.capable_nodes(cluster).values()), "egress off on every node once the hook is done"
    for node, ip in node_ips.items():
        st = (node_states(cluster).get(node) or {}).get("status") or {}
        assert not st.get("programmed"), f"{node}: still programmed"
        log = agent_log(cluster, node, since)
        # Known only once teardown tried: the CNI put the entry back.
        if (node_json(ip, "/run/purelb/cni-status.json") or {}).get("reason") == "CNIOwned":
            # The CNI's own configuration chains it: entry and binary stay.
            assert PLUGIN in cni_chain(ip)[1] and has_plugin_binary(ip), node
            assert "the CNI's own configuration chains the plugin" in log, node
            continue
        assert PLUGIN not in cni_chain(ip)[1], f"{node}: still chained"
        assert not has_plugin_binary(ip), f"{node}: plugin binary left"
        for f in ("/var/lib/purelb/egress-snapshot.json", "/var/lib/purelb/cni-desired.json", RECORDS):
            assert ssh(ip, f"sudo test -e {f} && echo yes || true", check=False).strip() != "yes", f"{node}: {f} left"
        assert (node_json(ip, "/run/purelb/cni-status.json") or {}).get("chain") == "Off", node
        assert log.index("the plugin is out of the CNI chain") < log.index("egress datapath removed"), \
            f"{node}: the plugin out of the chain before the datapath goes (D26)"
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
    # Nothing left over, and with no node announcing, the policy says so.
    wait_until(lambda: agent_metrics(n).value("purelb_egress_orphan_pins") == 0, timeout=30, description=f"{n}: no orphan pins")
    wait_until(lambda: policy_condition(cluster, "local", "Announced") == ("False", "NoAnnouncer"), timeout=60,
               description="the policy reporting NoAnnouncer")

    apply_agent(cluster, True)
    wait_until(lambda: all_capable(cluster, node_ips), timeout=180, description="egress back on every node")
    for n, ip in node_ips.items():
        assert TUNNEL in ssh(ip, f"ip -br link show {TUNNEL}", check=False), n
        wait_until(lambda ip=ip: PLUGIN in cni_chain(ip)[1] and has_plugin_binary(ip), timeout=60,
                   description=f"{n}: the plugin chained again")
        assert (node_states(cluster)[n].get("status") or {}).get("programmed"), n
    for a in (env.v4, env.v6):
        wait_until(lambda a=a: len(announcers(cluster, a)) == 1, timeout=120, description=f"one holder of {a}")
    wait_until(lambda: len(router.nexthops(f"{env.r4}/32")) == len(node_ips), timeout=120,
               description="the routed paths back")
    # Each node's settings check out.
    live = subprocess.run([str(guard.PLUGIN), "validate", "--live", "--context", kube_context, "-o", "json"],  # noqa: S603
                          capture_output=True, text=True, timeout=300)
    checks = json.loads(live.stdout)["checks"]
    assert any("bpffs is mounted" in c["message"] for c in checks), checks
    assert not [c for c in checks if c["status"] == "FAIL" and c["message"].startswith("Egress")], checks

    # On again during the teardown: the files the plugin reads are the new
    # session's, and the old teardown ending leaves them, so the installer
    # keeps the chain and no node reports it missing.
    since = utcnow()
    apply_agent(cluster, False)
    wait_until(lambda: "removing the egress datapath" in agent_log(cluster, n, since), timeout=60, interval=0.2,
               description=f"{n}: tearing down")
    apply_agent(cluster, True)
    for node, ip in node_ips.items():
        wait_until(lambda node=node: "egress datapath removed" in agent_log(cluster, node, since), timeout=60,
                   description=f"{node}: the old teardown over")
        wait_until(lambda node=node: "asked purelb-cni to chain the CNI plugin" in agent_log(cluster, node, since), timeout=120,
                   description=f"{node}: the new session asked for the plugin")
    wait_until(lambda: all_capable(cluster, node_ips), timeout=180, description="egress back on every node")
    time.sleep(10)  # past the installer's and the agent's next looks at the file
    for node, ip in node_ips.items():
        assert ssh(ip, "sudo test -e /var/lib/purelb/cni-desired.json && echo yes || true", check=False).strip() == "yes", \
            f"{node}: the new session's desired state removed by the old teardown"
        assert (node_json(ip, "/run/purelb/cni-status.json") or {}).get("chain") == "Present", node
        assert PLUGIN in cni_chain(ip)[1] and has_plugin_binary(ip), node
        wait_until(lambda node=node: agent_metrics(node).counter(
            "purelb_egress_node_conditions_false", condition="DatapathCompatible", reason="ChainMissing") == 0,
            timeout=30, description=f"{node}: not reporting ChainMissing")

    # Break-glass: the node's agent is gone and egress is still programmed.
    # cleanup refuses while the LBNodeAgent enables egress, removes all of it
    # with --force, and the agent coming back rebuilds it.
    n = next(w.metadata.name for w in cluster.nodes()
             if "node-role.kubernetes.io/control-plane" not in (w.metadata.labels or {}) and w.metadata.name in node_ips)
    ip = node_ips[n]
    assert egress_pins(ip) and PLUGIN in cni_chain(ip)[1], f"{n}: programmed before cleanup"
    keep_off = {"spec": {"template": {"spec": {"affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
        "nodeSelectorTerms": [{"matchExpressions": [{"key": "kubernetes.io/hostname", "operator": "NotIn", "values": [n]}]}]}}}}}}}
    cluster.apps.patch_namespaced_daemon_set("lbnodeagent", cluster.purelb_namespace, keep_off)
    try:
        wait_until(lambda: agent_pod(cluster, n) is None, timeout=240, interval=3, description=f"{n}'s agent gone")
        assert egress_pins(ip), f"{n}: a stopped agent leaves its state"
        refused = subprocess.run([str(guard.PLUGIN), "egress", "cleanup", "--node", n, "--context", kube_context],  # noqa: S603
                                 capture_output=True, text=True, timeout=60)
        assert refused.returncode != 0 and "enables egress" in refused.stderr, refused
        cleaned = subprocess.run([str(guard.PLUGIN), "egress", "cleanup", "--node", n, "--force", "--context", kube_context],  # noqa: S603
                                 capture_output=True, text=True, timeout=300)
        assert cleaned.returncode == 0 and "egress removed" in cleaned.stdout, cleaned
        assert egress_pins(ip) == [], f"{n}: egress pins left by cleanup"
        assert "does not exist" in ssh(ip, f"ip link show {TUNNEL} 2>&1 || true", check=False), f"{n}: tunnel left"
        assert "purelb_egress" not in ssh(ip, "sudo nft list tables", check=False), f"{n}: nft table left"
        assert PLUGIN not in cni_chain(ip)[1] and not has_plugin_binary(ip), f"{n}: plugin left"
        for f in ("/var/lib/purelb/egress-snapshot.json", "/var/lib/purelb/cni-desired.json", RECORDS):
            assert ssh(ip, f"sudo test -e {f} && echo yes || true", check=False).strip() != "yes", f"{n}: {f} left"
    finally:
        restore = {"spec": {"template": {"spec": {"affinity": None}}}}
        cluster.apps.patch_namespaced_daemon_set("lbnodeagent", cluster.purelb_namespace, restore)
    wait_until(lambda: agent_pod(cluster, n) is not None and all_capable(cluster, node_ips) and egress_pins(ip),
               timeout=420, interval=5, description=f"{n} rebuilt by the agent coming back")
    wait_until(lambda: PLUGIN in cni_chain(ip)[1] and has_plugin_binary(ip), timeout=120, description=f"{n}: chained again")
    for a in (env.v4, env.v6):
        wait_until(lambda a=a: len(announcers(cluster, a)) == 1, timeout=120, description=f"one holder of {a}")


def test_a_rebooted_nodes_pod_never_leaves_unclassified(cluster: Cluster, node_ips: Dict[str, str], router: Router,
                                                        env: Env) -> None:
    """A worker node rebooted with a FailClosed pod that talks from its
    first moment (design 8.1, D35, D12): through the boot window, while the
    CNI rewrites its configuration and before the agent runs, and after,
    the router never sees the pod's traffic from the node's address or the
    pod's own, only from the policy's; purelb-cni chains the plugin again;
    the pod's new sandbox went through the plugin (recorded, not reported);
    its traffic leaves from the address again."""
    workers = [nd.metadata.name for nd in cluster.nodes()
               if "node-role.kubernetes.io/control-plane" not in (nd.metadata.labels or {}) and nd.metadata.name in node_ips]
    if not workers:
        pytest.skip("no worker node to reboot")
    held = set(announcers(cluster, env.v4) + announcers(cluster, env.v6))
    n = next((w for w in sorted(workers) if w not in held), sorted(workers)[0])
    ip = node_ips[n]
    me = my_address(router.host)
    name = "egress-e2e-reboot"
    talker(cluster, name, NS, n, env.routed_dst4, {"app": "egress-e2e"})

    def probed() -> Optional[Dict[str, str]]:
        return next((json.loads(ln) for ln in agent_log(cluster, n).splitlines()
                     if '"op":"egressStageProbe"' in ln and '"stage"' in ln), None)

    try:
        before = wait_until(lambda: running(cluster, NS, name), timeout=120, description=f"{name} running on {n}")
        restarts = before.status.container_statuses[0].restart_count
        stage = wait_until(probed, timeout=60, description=f"{n}'s stage")["stage"]
        with router.capture("any", f"tcp dst port 22 and dst host {env.routed_dst4} and not src host {me}",
                            seconds=900) as cap:
            boot = ssh(ip, "cat /proc/sys/kernel/random/boot_id").strip()
            ssh(ip, "sudo systemctl reboot", check=False)
            wait_until(lambda: (lambda b: b and b != boot)(
                ssh(ip, "cat /proc/sys/kernel/random/boot_id", timeout=10, check=False).strip()),
                timeout=600, interval=10, description=f"{n} rebooted")
            wait_until(lambda: (lambda p: p if p and p.status.container_statuses and
                                p.status.container_statuses[0].restart_count > restarts and
                                p.status.container_statuses[0].ready else None)(running(cluster, NS, name)),
                       timeout=600, interval=5, description=f"{name} running again after the reboot")
            wait_until(lambda: egress.capable_nodes(cluster).get(n), timeout=300, interval=5,
                       description=f"{n} egress-capable again")
            time.sleep(20)
        after = running(cluster, NS, name)
        syns = router_syns(cap.lines)
        leaks = set(node_ips.values()) | {before.status.pod_ip, after.status.pod_ip}
        stray = sorted({src for src, _, _ in syns if src in leaks})
        # Antrea rewrites its conflist at boot, so a sandbox can start without
        # the plugin for some seconds: a window the user accepted (2026-11-03).
        # It must then be held and reported. Every other CNI closes it.
        boot_window = "antrea" in cni_chain(ip)[0] and bool(stray)
        assert boot_window or not stray, f"through the reboot the pod's SYNs left from {stray}, not only {env.r4}"
        assert any(src == env.r4 for src, _, _ in syns[-20:]), f"the pod's traffic not back on {env.r4}: {syns[-5:]}"
        assert PLUGIN in cni_chain(ip)[1] and has_plugin_binary(ip), f"{n}: the plugin not chained again"
        reported = [e for e in unrecorded_events(cluster, NS, name)
                    if e.last_timestamp is None or e.last_timestamp.timestamp() > time.time() - 900]
        if boot_window:
            assert reported, f"{n}: the pod's sandbox started without the plugin and wasn't reported"
        else:
            assert records_of(ip, name), f"{n}: the pod's new sandbox has no record: it bypassed the plugin"
            assert not reported, "reported as started without the plugin"
        # The table is rewritten many times during a boot: the probe still
        # counts every datagram it sent, and finds the same stage.
        probe = wait_until(probed, timeout=60, description=f"{n}'s stage after the reboot")
        assert probe["stage"] == stage, f"{n}: {stage} before the reboot: {probe}"
        if stage != "Bypassed":
            counts = re.findall(r"IPv[46] (\d+)/(\d+)/(\d+)", probe["detail"])
            assert counts and all(c == ("3", "3", "3") or (stage == "SourceIP" and c[0] == c[2] == "3") for c in counts), \
                f"{n}: the probe lost count: {probe}"
    finally:
        delete_pod_now(cluster, NS, name)
