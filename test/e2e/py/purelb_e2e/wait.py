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

"""Polling with a deadline.

The bash suite contained 216 fixed `sleep` calls. A fixed sleep is wrong
in both directions: too short and the test is flaky, too long and the
suite takes an hour. Every one of them becomes a predicate here.
"""

import time
from typing import Callable, Optional, TypeVar

T = TypeVar("T")


class WaitTimeout(AssertionError):
    """Raised when a predicate did not become true before the deadline.

    Subclasses AssertionError so pytest reports it as a test failure
    rather than an error: a condition that never came true is a failed
    expectation, not a broken harness.
    """


def wait_until(
    predicate: Callable[[], Optional[T]],
    timeout: float = 60.0,
    interval: float = 1.0,
    description: str = "condition",
) -> T:
    """Poll `predicate` until it returns something truthy, then return it.

    The predicate returns the value it found rather than a bool, so a
    caller can wait for and capture a result in one step:

        ip = wait_until(lambda: svc_ingress_ip(...), description="VIP assigned")

    Exceptions from the predicate are treated as "not yet": a resource
    that does not exist for the first few polls is normal. The last one
    is re-raised in the timeout message so a persistent error is not
    hidden behind a generic timeout.
    """
    deadline = time.monotonic() + timeout
    last_error: Optional[BaseException] = None
    while True:
        try:
            result = predicate()
            if result:
                return result
            last_error = None
        except Exception as exc:  # noqa: BLE001 - deliberately broad, see docstring
            last_error = exc
        if time.monotonic() >= deadline:
            msg = f"timed out after {timeout:.0f}s waiting for {description}"
            if last_error is not None:
                msg += f"; last error: {last_error!r}"
            raise WaitTimeout(msg)
        time.sleep(interval)


def wait_while(
    predicate: Callable[[], bool],
    timeout: float = 60.0,
    interval: float = 1.0,
    description: str = "condition to clear",
) -> None:
    """Poll until `predicate` is false. For withdrawal and teardown."""
    wait_until(
        lambda: not predicate(),
        timeout=timeout,
        interval=interval,
        description=description,
    )
