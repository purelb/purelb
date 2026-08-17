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

"""Kubernetes access for the e2e suite.

Replaces 886 `kubectl` invocations and 307 jsonpath expressions with
typed object access.

Log reading deliberately REQUIRES a time window. The bash helper defaulted
to `--tail=200` across every pod and returned on the first match anywhere,
so a line emitted by an earlier test on a different node satisfied the
assertion. Making `since` a required argument means that cannot be
written by accident.
"""

from __future__ import annotations

import datetime as _dt
from dataclasses import dataclass
from typing import Any, Dict, Iterable, List, Optional

from kubernetes import client, config
from kubernetes.client.rest import ApiException

PURELB_GROUP = "purelb.io"
PURELB_VERSION = "v2"


def utcnow() -> _dt.datetime:
    return _dt.datetime.now(_dt.timezone.utc)


@dataclass
class Cluster:
    """A connected cluster, scoped to one kube context."""

    context: str
    purelb_namespace: str = "purelb-system"

    def __post_init__(self) -> None:
        config.load_kube_config(context=self.context)
        self.core = client.CoreV1Api()
        self.apps = client.AppsV1Api()
        self.custom = client.CustomObjectsApi()
        self.coordination = client.CoordinationV1Api()

    # ---------------------------------------------------------------- nodes

    def nodes(self) -> List[client.V1Node]:
        return self.core.list_node().items

    def node_names(self) -> List[str]:
        return [n.metadata.name for n in self.nodes()]

    def node_ip(self, name: str) -> str:
        for node in self.nodes():
            if node.metadata.name != name:
                continue
            for addr in node.status.addresses or []:
                if addr.type == "InternalIP":
                    return addr.address
        raise AssertionError(f"no InternalIP for node {name}")

    def node_ips(self) -> Dict[str, str]:
        out: Dict[str, str] = {}
        for node in self.nodes():
            for addr in node.status.addresses or []:
                if addr.type == "InternalIP":
                    out[node.metadata.name] = addr.address
        return out

    def add_taint(self, node: str, key: str, value: str, effect: str) -> None:
        current = [t.to_dict() for t in (self.core.read_node(node).spec.taints or [])]
        current = [t for t in current if t.get("key") != key]
        current.append({"key": key, "value": value, "effect": effect})
        self.core.patch_node(node, {"spec": {"taints": current}})

    def remove_taint(self, node: str, key: str) -> None:
        """Idempotent: removing an absent taint is not an error.

        Teardown must tolerate a taint that was never applied, or a test
        that fails before tainting turns one failure into two.
        """
        current = [t.to_dict() for t in (self.core.read_node(node).spec.taints or [])]
        remaining = [t for t in current if t.get("key") != key]
        if len(remaining) == len(current):
            return
        self.core.patch_node(node, {"spec": {"taints": remaining}})

    def taint_keys(self, node: str) -> List[str]:
        return [t.key for t in (self.core.read_node(node).spec.taints or [])]

    def label_node(self, node: str, key: str, value: Optional[str]) -> None:
        """Set or (with value=None) remove a node label."""
        self.core.patch_node(node, {"metadata": {"labels": {key: value}}})

    def node_lease_subnets(self, node: str) -> List[str]:
        """The subnets a node's election lease advertises.

        This is what the node tells the rest of the cluster it can
        announce on, so it is the observable for interface selection --
        an interface the agent did not accept never reaches the lease.
        """
        for lease in self.leases():
            if lease.metadata.name != f"purelb-node-{node}":
                continue
            raw = (lease.metadata.annotations or {}).get("purelb.io/subnets", "")
            return [s for s in raw.split(",") if s]
        return []

    # --------------------------------------------------------------- daemonset

    def daemonset_ready(self, namespace: str, name: str, expect_nodes: Optional[int] = None) -> bool:
        """Whether a DaemonSet is fully rolled out.

        `expect_nodes` matters more than it looks, and omitting it is a
        false pass. desiredNumberScheduled counts the nodes the DaemonSet
        can currently schedule ON, so while a node carries a NoExecute
        taint the DaemonSet is perfectly "ready" at 4/4 on a 5-node
        cluster: internally consistent, and missing an agent. Asserting
        recovery therefore has to compare against the NODE count, not
        against the DaemonSet's own idea of how many it wanted.
        """
        ds = self.apps.read_namespaced_daemon_set(name, namespace)
        st = ds.status
        desired = st.desired_number_scheduled or 0
        if (st.observed_generation or 0) < (ds.metadata.generation or 0):
            return False
        if desired <= 0 or (st.number_ready or 0) != desired:
            return False
        # updatedNumberScheduled is what makes this usable after a restart.
        # A rolling update replaces pods one at a time, so at the moment it
        # begins every OLD pod is still ready and number_ready already
        # equals desired -- this returned True before a single pod had been
        # replaced. Callers then scraped agents that were about to go away
        # and got connection refused partway through a test, which reads as
        # a broken node rather than as a rollout still in flight.
        if (st.updated_number_scheduled or 0) != desired:
            return False
        return expect_nodes is None or desired == expect_nodes

    def restart_daemonset(self, namespace: str, name: str) -> None:
        """Force a rollout so a changed CR is definitely in effect.

        The CR controller pushes new config into a running announcer, but
        nothing the agent exports says WHICH generation is live. A test
        that changed an LBNodeAgent and carried straight on would be
        racing an informer, and would fail in the shape of "the feature
        does not work" rather than "the config had not landed yet".
        Restarting replaces that race with a condition that can be waited
        on -- at the cost of a rollout, which is the right trade for a
        test whose whole subject is packets sent at announcement time.
        """
        self.apps.patch_namespaced_daemon_set(
            name,
            namespace,
            {
                "spec": {
                    "template": {
                        "metadata": {
                            "annotations": {
                                "purelb.io/e2e-restarted-at": _dt.datetime.now(
                                    _dt.timezone.utc
                                ).isoformat()
                            }
                        }
                    }
                }
            },
        )

    # -------------------------------------------------------------- services

    def service(self, namespace: str, name: str) -> Optional[client.V1Service]:
        try:
            return self.core.read_namespaced_service(name, namespace)
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise

    def service_ingress_ips(self, namespace: str, name: str) -> List[str]:
        svc = self.service(namespace, name)
        if svc is None or svc.status is None or svc.status.load_balancer is None:
            return []
        return [
            ing.ip
            for ing in (svc.status.load_balancer.ingress or [])
            if ing.ip
        ]

    def annotation(self, namespace: str, name: str, key: str) -> Optional[str]:
        svc = self.service(namespace, name)
        if svc is None:
            return None
        return (svc.metadata.annotations or {}).get(key)

    def apply_service(self, body: Dict[str, Any]) -> client.V1Service:
        """Create the Service, replacing any existing one of that name."""
        ns = body["metadata"]["namespace"]
        name = body["metadata"]["name"]
        if self.service(ns, name) is not None:
            self.delete_service(ns, name)
            # A LoadBalancer Service is not gone for the allocator until
            # the object is, and recreating it while the old one is
            # terminating gets the new one the old address -- which would
            # make an "allocated a fresh address" assertion pass for the
            # wrong reason.
            self.wait_service_gone(ns, name)
        return self.core.create_namespaced_service(ns, body)

    def delete_service(self, namespace: str, name: str) -> None:
        try:
            self.core.delete_namespaced_service(name, namespace)
        except ApiException as exc:
            if exc.status != 404:
                raise

    def wait_service_gone(self, namespace: str, name: str, timeout: float = 60.0) -> None:
        from .wait import wait_until

        wait_until(
            lambda: self.service(namespace, name) is None,
            timeout=timeout,
            description=f"Service {namespace}/{name} to disappear",
        )

    # ----------------------------------------------------------- deployments

    def deployment(self, namespace: str, name: str) -> Optional[client.V1Deployment]:
        try:
            return self.apps.read_namespaced_deployment(name, namespace)
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise

    def patch_deployment(self, namespace: str, name: str, patch: Dict[str, Any]) -> None:
        self.apps.patch_namespaced_deployment(name, namespace, patch)

    def wait_rollout(self, namespace: str, name: str, timeout: float = 180.0) -> None:
        """Wait for a Deployment to finish rolling out.

        Checks observed_generation as well as the replica counts. Without
        it, the very first poll can pass against the PREVIOUS generation's
        status -- the deployment controller has not yet reacted to the
        patch, so the old ReplicaSet still looks perfectly available. That
        race is why the bash suite's rollout waits occasionally let a test
        run against the pod it was trying to replace.
        """
        from .wait import wait_until

        def ready() -> bool:
            dep = self.deployment(namespace, name)
            if dep is None or dep.status is None:
                return False
            # `or 1` was wrong at exactly one value: 0 is falsy, so scaling
            # to ZERO made this wait for one replica that would never
            # arrive and the rollout timed out instead of succeeding at
            # once. An UNSET replicas really does default to 1, which is
            # why the idiom looked right; only the explicit zero is wrong.
            # Nothing caught it because no test scaled a Deployment to zero
            # and then waited for the rollout until E1 did.
            want = 1 if dep.spec.replicas is None else dep.spec.replicas
            st = dep.status
            return (
                (st.observed_generation or 0) >= (dep.metadata.generation or 0)
                and (st.updated_replicas or 0) == want
                and (st.available_replicas or 0) == want
                and (st.unavailable_replicas or 0) == 0
            )

        wait_until(ready, timeout=timeout, interval=2.0,
                   description=f"Deployment {namespace}/{name} rollout")

    # ------------------------------------------------------------------ pods

    def pods(self, namespace: str, label_selector: str) -> List[client.V1Pod]:
        return self.core.list_namespaced_pod(
            namespace, label_selector=label_selector
        ).items

    def delete_pod(self, namespace: str, name: str, grace_seconds: int = 30) -> None:
        """Delete a pod with a grace period.

        The grace period is load-bearing for the failover tests: it is what
        lets lbnodeagent withdraw its addresses on the way out instead of
        orphaning them on the interface, where they linger until valid_lft
        expires.
        """
        try:
            self.core.delete_namespaced_pod(
                name, namespace, grace_period_seconds=grace_seconds
            )
        except ApiException as exc:
            if exc.status != 404:
                raise

    def pod_on_node(
        self, namespace: str, label_selector: str, node: str
    ) -> Optional[client.V1Pod]:
        """The pod matching `label_selector` scheduled on `node`.

        Uses a field selector rather than matching text. The bash version
        piped `kubectl get pods -o wide` through `grep "$node"`, which also
        matched the NAME and IP columns and could not tell purelb2-1 from
        purelb2-10.
        """
        found = self.core.list_namespaced_pod(
            namespace,
            label_selector=label_selector,
            field_selector=f"spec.nodeName={node}",
        ).items
        return found[0] if found else None

    # ------------------------------------------------------------------ logs

    def pod_log_text(
        self,
        namespace: str,
        pod: str,
        container: Optional[str] = None,
        since_seconds: Optional[int] = None,
    ) -> str:
        """A pod's log, byte-for-byte as the pod wrote it.

        Every log read goes through here, because the obvious call does
        not return what the pod printed. `read_namespaced_pod_log` is
        declared as returning `str`, and the client's deserializer runs
        `json.loads` on the body before honouring that: when the log
        happens to BE valid JSON, it decodes to a dict and is then handed
        back as `str(dict)` -- a Python repr, with single quotes, `None`
        and `True`. The pod wrote `{"a": 1, "b": null}` and the caller
        receives `{'a': 1, 'b': None}`.

        Plain-text logs are unaffected, which is why this stayed hidden
        while the backend was nginx and appeared the moment it started
        answering in JSON. It cost three tests and a misdiagnosis: the
        corrupted body was read as evidence that the pod was serving
        stale code, which it was not.

        `_preload_content=False` skips the deserializer and gives us the
        raw response, which is all a log ever needed.
        """
        try:
            resp = self.core.read_namespaced_pod_log(
                name=pod,
                namespace=namespace,
                container=container,
                since_seconds=since_seconds,
                _preload_content=False,
            )
        except ApiException as exc:
            if exc.status == 404:
                return ""
            raise
        return resp.data.decode("utf-8", errors="replace")

    def pod_logs(
        self,
        namespace: str,
        pod: str,
        since: _dt.datetime,
        container: Optional[str] = None,
    ) -> str:
        """Logs from `pod` emitted at or after `since`.

        `since` is required. See the module docstring.
        """
        seconds = max(1, int((utcnow() - since).total_seconds()) + 1)
        return self.pod_log_text(
            namespace, pod, container=container, since_seconds=seconds
        )

    def component_logs(
        self, component: str, since: _dt.datetime
    ) -> Dict[str, str]:
        """Logs for every pod of a PureLB component, keyed by pod name."""
        container = component if component == "lbnodeagent" else None
        out: Dict[str, str] = {}
        for pod in self.pods(self.purelb_namespace, f"component={component}"):
            out[pod.metadata.name] = self.pod_logs(
                self.purelb_namespace, pod.metadata.name, since, container
            )
        return out

    # -------------------------------------------------------- custom objects

    def _plural(self, kind: str) -> str:
        return {"servicegroup": "servicegroups", "lbnodeagent": "lbnodeagents"}[kind]

    def get_cr(self, kind: str, name: str, namespace: Optional[str] = None) -> Optional[Dict[str, Any]]:
        try:
            return self.custom.get_namespaced_custom_object(
                PURELB_GROUP,
                PURELB_VERSION,
                namespace or self.purelb_namespace,
                self._plural(kind),
                name,
            )
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise

    def list_crs(self, kind: str, namespace: Optional[str] = None) -> List[Dict[str, Any]]:
        return self.custom.list_namespaced_custom_object(
            PURELB_GROUP,
            PURELB_VERSION,
            namespace or self.purelb_namespace,
            self._plural(kind),
        ).get("items", [])

    def apply_cr(self, body: Dict[str, Any], namespace: Optional[str] = None) -> Dict[str, Any]:
        """Create, or replace if it already exists."""
        kind = body["kind"].lower()
        ns = namespace or body.get("metadata", {}).get("namespace") or self.purelb_namespace
        name = body["metadata"]["name"]
        existing = self.get_cr(kind, name, ns)
        if existing is None:
            return self.custom.create_namespaced_custom_object(
                PURELB_GROUP, PURELB_VERSION, ns, self._plural(kind), body
            )
        body = dict(body)
        body.setdefault("metadata", {})["resourceVersion"] = existing["metadata"]["resourceVersion"]
        return self.custom.replace_namespaced_custom_object(
            PURELB_GROUP, PURELB_VERSION, ns, self._plural(kind), name, body
        )

    def delete_cr(self, kind: str, name: str, namespace: Optional[str] = None) -> None:
        try:
            self.custom.delete_namespaced_custom_object(
                PURELB_GROUP,
                PURELB_VERSION,
                namespace or self.purelb_namespace,
                self._plural(kind),
                name,
            )
        except ApiException as exc:
            if exc.status != 404:
                raise

    # ---------------------------------------------------------------- leases

    def leases(self) -> Iterable[Any]:
        return self.coordination.list_namespaced_lease(self.purelb_namespace).items
