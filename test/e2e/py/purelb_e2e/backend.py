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

"""The echo backend, defined once.

Three fixtures create backend pods -- the shared one in `test`, the
per-test pinned ones, and the tenant one in `test-tenant` -- and before
this they were three hand-copied nginx pod specs that had already drifted
apart on grace period. One definition means a change to how the backend
behaves cannot land in two of the three.

The server itself is `test/echo-server/server.py`, mounted from a
ConfigMap rather than baked into an image, so there is nothing to build
and nothing to push. `scripts/reset-test-cluster.sh` provisions it in
`test`; `ensure_configmap` below is for the other namespaces.
"""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any, Dict, Optional

IMAGE = "python:3.12-alpine"
PORT = 8080
LABEL = "echo"
CONFIGMAP = "echo-server"

# test/e2e/py/purelb_e2e/backend.py -> test/echo-server/server.py
SERVER_PY = Path(__file__).resolve().parents[3] / "echo-server" / "server.py"


def server_checksum() -> str:
    """Short hash of server.py, stamped on the pod template.

    A ConfigMap has no tag and a running interpreter never re-reads the
    file it started from, so without this an edited server sits in the
    ConfigMap while every pod keeps serving the old code -- and the suite
    tests something that is not in the tree.
    """
    return hashlib.sha256(SERVER_PY.read_bytes()).hexdigest()[:16]


def ensure_configmap(cluster, namespace: str) -> None:
    """Create or update the server ConfigMap in `namespace`.

    `test` is provisioned by reset-test-cluster.sh; this is for fixtures
    that build a backend somewhere else, which today means the tenant
    namespace in the namespace-scoping module.
    """
    from kubernetes import client
    from kubernetes.client.rest import ApiException

    body = client.V1ConfigMap(
        metadata=client.V1ObjectMeta(name=CONFIGMAP, namespace=namespace),
        data={"server.py": SERVER_PY.read_text()},
    )
    try:
        cluster.core.create_namespaced_config_map(namespace, body)
    except ApiException as exc:
        if exc.status != 409:
            raise
        cluster.core.replace_namespaced_config_map(CONFIGMAP, namespace, body)


def pod_spec(node: Optional[str] = None) -> Dict[str, Any]:
    """The pod spec for a backend pod, optionally pinned to `node`.

    `nodeName` rather than a nodeSelector: the fixtures need the pod on a
    known node deterministically, not subject to the scheduler changing
    its mind.
    """
    spec: Dict[str, Any] = {
        # 3s plus the SIGTERM handler in server.py: the pod is gone in
        # about a second, so a moved VIP is not pinned to the old node by
        # an endpoint that is still Serving. The default 30s was the
        # dominant term in every affinity move.
        "terminationGracePeriodSeconds": 3,
        "containers": [
            {
                "name": LABEL,
                "image": IMAGE,
                "command": ["python3", "/app/server.py"],
                "ports": [{"containerPort": PORT}],
                "env": [
                    {"name": "POD_NAME",
                     "valueFrom": {"fieldRef": {"fieldPath": "metadata.name"}}},
                    {"name": "NODE_NAME",
                     "valueFrom": {"fieldRef": {"fieldPath": "spec.nodeName"}}},
                    {"name": "HOST_IP",
                     "valueFrom": {"fieldRef": {"fieldPath": "status.hostIP"}}},
                    {"name": "POD_IP",
                     "valueFrom": {"fieldRef": {"fieldPath": "status.podIP"}}},
                ],
                "volumeMounts": [{"name": "app", "mountPath": "/app"}],
                # Python has to start an interpreter before it binds, where
                # nginx was listening almost immediately. Without a probe
                # the endpoint is Ready before the socket exists and the
                # first request is refused. 1s keeps it off the timings.
                "readinessProbe": {
                    "httpGet": {"path": "/healthz", "port": PORT},
                    "periodSeconds": 1,
                },
            }
        ],
        "volumes": [{"name": "app", "configMap": {"name": CONFIGMAP}}],
    }
    if node:
        spec["nodeName"] = node
    return spec
