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

"""Prometheus scraping and assertions.

Two bugs from the bash suite are designed out here rather than fixed:

1. A failed scrape returned an empty string, and callers guarded their
   assertions with `if [ -n "$METRICS" ]`. Inability to observe became
   evidence of correctness. `scrape` raises; there is no falsy value to
   accidentally skip on.

2. Monotonic counters were asserted with `> 0`, which asks whether
   something has EVER happened rather than whether it just did. The
   comparison helpers below take a baseline `Snapshot`, so the only
   convenient thing to write is a delta.
"""

from __future__ import annotations

import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Dict, Mapping, Optional

from prometheus_client.parser import text_string_to_metric_families

METRICS_PORT = 7472


def _matches(key: str, name: str, labels: Mapping[str, str]) -> bool:
    """Whether a sample key is `name` carrying at least `labels`.

    Compares parsed label pairs rather than doing a substring test on the
    rendered key: `pool="default"` is a substring of `pool="default-v6"`,
    so the obvious `in` check would quietly aggregate unrelated series.
    """
    if not key.startswith(name):
        return False
    rest = key[len(name):]
    if not rest:
        return not labels
    if not (rest.startswith("{") and rest.endswith("}")):
        return False
    present = dict(
        pair.split("=", 1) for pair in rest[1:-1].split(",") if "=" in pair
    )
    return all(present.get(k) == f'"{v}"' for k, v in labels.items())


class ScrapeError(AssertionError):
    """A /metrics endpoint could not be read.

    AssertionError, not a bespoke exception: a scrape that fails during a
    test means the assertion cannot be made, and a test that cannot make
    its assertion has failed. Treating it as an infrastructure warning is
    how the bash suite ended up passing while checking nothing.
    """


@dataclass(frozen=True)
class Snapshot:
    """One scrape of one endpoint, parsed."""

    source: str
    samples: Dict[str, float]

    @staticmethod
    def _key(name: str, labels: Mapping[str, str]) -> str:
        if not labels:
            return name
        inner = ",".join(f'{k}="{v}"' for k, v in sorted(labels.items()))
        return f"{name}{{{inner}}}"

    @classmethod
    def parse(cls, text: str, source: str) -> "Snapshot":
        samples: Dict[str, float] = {}
        for family in text_string_to_metric_families(text):
            for sample in family.samples:
                samples[cls._key(sample.name, sample.labels)] = sample.value
        return cls(source=source, samples=samples)

    def get(self, name: str, **labels: str) -> Optional[float]:
        """Value of a metric, or None when it is absent.

        Absent and zero are deliberately distinguishable: `purelb_x_total`
        missing entirely usually means the feature never initialised,
        which is a different bug from it being initialised and idle.
        """
        return self.samples.get(self._key(name, labels))

    def value(self, name: str, **labels: str) -> float:
        """Value of a metric that must exist."""
        got = self.get(name, **labels)
        if got is None:
            raise AssertionError(
                f"metric {self._key(name, labels)} not present in {self.source}"
            )
        return got

    def has_series(self, name: str) -> bool:
        """Whether any sample of this metric name exists, labels aside."""
        return any(
            key == name or key.startswith(name + "{") for key in self.samples
        )

    def nearest(self, name: str, limit: int = 3) -> list:
        """Metric names closest to `name`, to make a typo obvious."""
        import difflib

        bare = {key.split("{", 1)[0] for key in self.samples}
        return difflib.get_close_matches(name, sorted(bare), n=limit, cutoff=0.5)

    def counter(self, name: str, **labels: str) -> float:
        """Total of every sample of `name` whose labels INCLUDE `labels`.

        Subset matching, unlike get/value which are exact. Two reasons:

        A counter that has never been incremented may not be exported at
        all, so for delta arithmetic absent and zero are the same thing.

        And a test should not have to name labels it does not care about.
        purelb_allocator_sidecar_rpc_total carries socket, method and code;
        asking "how many Allocate calls succeeded" means summing over
        sockets. Requiring the full set instead makes the assertion silently
        match nothing the moment a label is added to the metric -- which is
        indistinguishable from the counter not moving, and would have read
        as "the RPC never happened".
        """
        total = 0.0
        for key, value in self.samples.items():
            if _matches(key, name, labels):
                total += value
        return total


def scrape_url(url: str, timeout: float = 5.0) -> Snapshot:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:  # noqa: S310
            body = resp.read().decode("utf-8")
    except (urllib.error.URLError, OSError, TimeoutError) as exc:
        raise ScrapeError(f"could not scrape {url}: {exc}") from exc
    if not body.strip():
        raise ScrapeError(f"{url} returned an empty body")
    return Snapshot.parse(body, source=url)


def scrape_node(node_ip: str, port: int = METRICS_PORT) -> Snapshot:
    """Scrape an lbnodeagent, which serves on a hostPort."""
    return scrape_url(f"http://{node_ip}:{port}/metrics")


def scrape_pod_via_apiserver(core, namespace: str, pod: str, port: int = METRICS_PORT) -> Snapshot:
    """Scrape a pod that is only reachable on the cluster network.

    The allocator runs in the pod network, so a workstation cannot reach
    its :7472 directly. The bash suite solved this with `kubectl
    port-forward`, whose startup race is precisely what made its metric
    assertions flaky -- the retry loop returned an empty string, and
    callers treated that as "skip the assertions".

    The apiserver's pod proxy has no local port to bind, no subprocess and
    no race.

    _preload_content=False matters: with it enabled the generated client
    hands back `str(bytes)` -- literally the text `b'# HELP ...\\n...'` --
    which the Prometheus parser accepts as one meaningless line rather
    than rejecting, so the failure would be silent.
    """
    try:
        resp = core.connect_get_namespaced_pod_proxy_with_path(
            name=f"{pod}:{port}",
            namespace=namespace,
            path="metrics",
            _preload_content=False,
        )
        body = resp.data.decode("utf-8")
    except Exception as exc:  # noqa: BLE001 - any failure here means we cannot assert
        raise ScrapeError(f"could not proxy-scrape {namespace}/{pod}:{port}: {exc}") from exc
    if not body.strip():
        raise ScrapeError(f"{namespace}/{pod}:{port} returned an empty body")
    return Snapshot.parse(body, source=f"{namespace}/{pod}:{port}")


def assert_increased(
    before: Snapshot,
    after: Snapshot,
    name: str,
    min_delta: float = 1.0,
    **labels: str,
) -> float:
    """Assert a counter advanced by at least `min_delta`, and return the delta."""
    # A metric NAME that exists nowhere is almost always a typo or a
    # renamed metric, and it is indistinguishable from a counter that did
    # not move -- both read as 0 -> 0. That cost real time here:
    # purelb_allocator_allocation_rejected_total does not exist (the
    # subsystem is address_pool), so an assertion on it reported the
    # feature as broken when it was working.
    if not (before.has_series(name) or after.has_series(name)):
        raise AssertionError(
            f"no metric named {name!r} exists in {after.source}. This is a "
            f"typo or a rename, not a counter that failed to move -- check "
            f"the Prometheus subsystem. Nearest names: "
            f"{', '.join(after.nearest(name)) or '(none)'}"
        )

    b = before.counter(name, **labels)
    a = after.counter(name, **labels)
    delta = a - b
    assert delta >= min_delta, (
        f"{Snapshot._key(name, labels)} advanced by {delta:g} ({b:g} -> {a:g}), "
        f"expected at least {min_delta:g}"
    )
    return delta


def assert_not_increased(
    before: Snapshot, after: Snapshot, name: str, **labels: str
) -> None:
    """Assert a counter did not advance.

    For error counters. Asserting `== 0` instead is the mistake the bash
    suite made: any historical error then fails the check forever, and a
    legitimate operation can produce one. Deleting a ServiceGroup makes
    the allocator's next status write 404, which lands in
    sg_status_writes_total{outcome="other"} and poisoned every subsequent
    run until the allocator was restarted.
    """
    b = before.counter(name, **labels)
    a = after.counter(name, **labels)
    assert a <= b, (
        f"{Snapshot._key(name, labels)} advanced by {a - b:g} ({b:g} -> {a:g}), "
        f"expected no increase"
    )
