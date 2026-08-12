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

"""Compare a bash suite's verdicts against its pytest replacement.

The migration's failure mode is not a port that breaks loudly; it is a port
that quietly covers less than the thing it replaces. 5321 lines of bash
become a few hundred lines of parametrized Python and it is genuinely hard
to tell, by reading, whether an assertion went missing.

So the port is not trusted, it is checked: run both harnesses against the
same cluster and compare, assertion by assertion, using a checked-in
mapping. Every bash assertion must name the pytest test that replaces it.
An unmapped bash assertion is an error, which is what stops a test being
dropped silently -- deleting bash code alone can never make the run green.

Note what this can and cannot show. It compares VERDICTS, so it catches an
assertion that vanished or that disagrees. It cannot catch an assertion
that was ported into something weaker but still passing; that is what the
false-pass fixes in the bash suite were for, and why they had to land
first. A lying oracle verifies nothing.
"""

from __future__ import annotations

import fnmatch
import re
import sys
import xml.etree.ElementTree as ET
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Sequence, Tuple

import yaml

# `pass()` writes "${GREEN}✓ PASS:${NC} message"; fail() writes "✗ FAIL:".
_ANSI = re.compile(r"\x1b\[[0-9;]*m")
_PASS = re.compile(r"^\s*✓ PASS:\s*(.*?)\s*$")
_FAIL = re.compile(r"^\s*✗ FAIL:\s*(.*?)\s*$")


class MappingError(Exception):
    """The mapping file is unusable, so no comparison can be made."""


# ------------------------------------------------------------------ bash


@dataclass(frozen=True)
class BashRun:
    """What a bash suite actually asserted.

    `completed` matters more than it looks. fail() calls exit, so a run
    that dies partway produces a PREFIX of its assertions and the rest are
    not failures but silence. Comparing against silence would report a
    flood of spurious disagreements, so an incomplete bash run invalidates
    the whole comparison rather than being partially interpreted.
    """

    passed: Tuple[str, ...]
    failed: Tuple[str, ...]
    exit_code: int

    @property
    def completed(self) -> bool:
        return self.exit_code == 0 and not self.failed


def parse_bash_output(text: str, exit_code: int) -> BashRun:
    passed: List[str] = []
    failed: List[str] = []
    for raw in text.splitlines():
        line = _ANSI.sub("", raw)
        m = _PASS.match(line)
        if m:
            passed.append(m.group(1))
            continue
        m = _FAIL.match(line)
        if m:
            failed.append(m.group(1))
    return BashRun(passed=tuple(passed), failed=tuple(failed), exit_code=exit_code)


# ---------------------------------------------------------------- pytest


def parse_junit(path: Path) -> Dict[str, str]:
    """Map pytest node id -> "passed" | "failed" | "error" | "skipped".

    Skipped is kept distinct and is NOT treated as success. A skip means
    the assertion was not made, so a bash assertion that passed against a
    pytest test that skipped is a coverage regression, not agreement.
    That distinction is the entire reason the bash suite's 17 untallied
    conditional skips went unnoticed.
    """
    root = ET.parse(path).getroot()
    verdicts: Dict[str, str] = {}
    for case in root.iter("testcase"):
        classname = case.get("classname") or ""
        name = case.get("name") or ""
        # pytest emits classname "tests.test_local" (plus ".ClassName" for
        # tests in a class). Rebuild "tests/test_local.py::test_x".
        parts = classname.split(".")
        if not parts or not name:
            continue
        # pytest joins the module path with dots and appends the class for
        # class-based tests. A trailing capitalised part is therefore a
        # class, not a directory: "tests.test_local.TestFoo" must become
        # "tests/test_local.py::TestFoo::test_x", and getting this wrong
        # would surface as a mapped test that "did not run".
        cls = ""
        if len(parts) > 1 and parts[-1][:1].isupper():
            cls = parts[-1]
            parts = parts[:-1]
        module = "/".join(parts) + ".py"
        node = f"{module}::{cls}::{name}" if cls else f"{module}::{name}"
        if case.find("failure") is not None:
            verdicts[node] = "failed"
        elif case.find("error") is not None:
            verdicts[node] = "error"
        elif case.find("skipped") is not None:
            verdicts[node] = "skipped"
        else:
            verdicts[node] = "passed"
    return verdicts


# --------------------------------------------------------------- mapping


@dataclass(frozen=True)
class Entry:
    """One bash assertion and the pytest test(s) that replace it."""

    pattern: str
    nodes: Tuple[str, ...]


@dataclass(frozen=True)
class SuiteMap:
    name: str
    script: str
    entries: Tuple[Entry, ...]


def load_map(path: Path) -> Dict[str, SuiteMap]:
    """Parse the mapping file.

    YAML because everything else a contributor reads in this repo is YAML
    -- manifests, CRDs, CI, test fixtures. safe_load, not load: the file
    is data, and nothing here should be able to construct Python objects.
    Insertion order is preserved (dicts are ordered, and PyYAML builds
    them in document order), which the first-match-wins rule relies on.
    """
    with path.open("r", encoding="utf-8") as fh:
        raw = yaml.safe_load(fh)
    if raw is None:  # an empty file is a valid empty mapping
        return {}
    if not isinstance(raw, dict):
        raise MappingError(f"{path} must contain a mapping of suite name to suite")

    suites: Dict[str, SuiteMap] = {}
    for name, body in raw.items():
        if not isinstance(body, dict):
            raise MappingError(f"{name}: must be a mapping")
        script = body.get("script")
        if not script:
            raise MappingError(f"{name}: has no `script` key")
        assertions = body.get("assertions") or {}
        if not isinstance(assertions, dict):
            raise MappingError(f"{name}.assertions: must be a mapping")
        entries = []
        for pattern, nodes in assertions.items():
            if not isinstance(pattern, str):
                # An unquoted glob can parse as a non-string -- `*` alone
                # is invalid, and a pattern with a colon splits. Catch it
                # here rather than failing to match at comparison time.
                raise MappingError(
                    f"{name}.assertions: pattern {pattern!r} is {type(pattern).__name__}, "
                    f"not a string. Quote it."
                )
            if isinstance(nodes, str):
                nodes = [nodes]
            if not isinstance(nodes, list) or not all(isinstance(n, str) for n in nodes):
                raise MappingError(
                    f"{name}.assertions: {pattern!r} must map to a node id "
                    f"or a list of node ids"
                )
            if not nodes:
                raise MappingError(
                    f"{name}.assertions: {pattern!r} maps to nothing. To drop "
                    f"an assertion deliberately, delete it from the bash suite "
                    f"in the same commit and remove this line."
                )
            entries.append(Entry(pattern=pattern, nodes=tuple(nodes)))
        suites[name] = SuiteMap(name=name, script=script, entries=tuple(entries))
    return suites


def match_entry(message: str, entries: Sequence[Entry]) -> Entry | None:
    """First entry whose glob matches.

    Globs, not exact strings, because bash assertion messages interpolate
    the values they just observed -- "service allocated 172.30.250.201".
    Matching on the literal would make the mapping depend on which
    addresses the pool happened to hand out.
    """
    for entry in entries:
        if fnmatch.fnmatchcase(message, entry.pattern):
            return entry
    return None


# ------------------------------------------------------------ comparison


def resolve_node(node: str, verdicts: Dict[str, str]) -> List[str]:
    """The node ids a mapping entry refers to.

    An exact id matches itself. A BASE name -- one without a [param]
    suffix -- matches every parametrization of that test, because
    `test_x` in the mapping means the test, not one of its cases. pytest
    reports parametrized tests only as `test_x[v4]`, so without this a
    mapping written against the readable name resolves to nothing and the
    dual-run reports 59 tests that "did not run" while they all passed.
    """
    if node in verdicts:
        return [node]
    prefix = node + "["
    return sorted(n for n in verdicts if n.startswith(prefix))


@dataclass
class Report:
    suite: str
    agreed: List[Tuple[str, str]] = field(default_factory=list)
    disagreed: List[str] = field(default_factory=list)
    unmapped: List[str] = field(default_factory=list)
    missing_nodes: List[str] = field(default_factory=list)
    pytest_only: List[str] = field(default_factory=list)
    fatal: List[str] = field(default_factory=list)

    @property
    def clean(self) -> bool:
        return not (self.disagreed or self.unmapped or self.missing_nodes or self.fatal)


def compare(
    suite: SuiteMap,
    bash: BashRun,
    pytest_verdicts: Dict[str, str],
) -> Report:
    """Diff the two harnesses' verdicts for one suite."""
    rep = Report(suite=suite.name)

    if not bash.completed:
        rep.fatal.append(
            f"bash run did not complete (exit {bash.exit_code}, "
            f"{len(bash.failed)} failure(s): {', '.join(bash.failed) or 'none reported'}). "
            f"Its unreached assertions are silent, not failing, so the "
            f"comparison would be meaningless. Fix the bash run first."
        )
        return rep

    # A run that asserted NOTHING is not a run that agreed with everything.
    # This reported CLEAN once: the suite rejected an argument, printed a
    # usage error, exited 0, and produced no assertions -- so there was
    # nothing to disagree with and the gate waved through a port it had
    # verified nothing about. That is the exact failure this tool exists
    # to catch, so it must not be possible here of all places.
    if not bash.passed:
        rep.fatal.append(
            "the bash run produced NO assertions at all. It exited 0, so it "
            "did not fail -- it never ran. Check the invocation: a suite that "
            "rejects an argument prints a usage error and exits cleanly. "
            "Nothing to compare is not the same as everything agreeing."
        )
        return rep

    seen_nodes: set[str] = set()
    for message in bash.passed:
        entry = match_entry(message, suite.entries)
        if entry is None:
            rep.unmapped.append(message)
            continue
        for node in entry.nodes:
            matched = resolve_node(node, pytest_verdicts)
            seen_nodes.update(matched)
            if not matched:
                rep.missing_nodes.append(f"{message!r} -> {node} (no such pytest test ran)")
                continue
            # A base name standing for a parametrized test requires EVERY
            # parametrization to have passed. "Any of them passed" would
            # let an IPv6 case cover an IPv4 assertion, which is precisely
            # the coverage this migration keeps almost losing.
            bad = {n: pytest_verdicts[n] for n in matched if pytest_verdicts[n] != "passed"}
            if bad:
                rep.disagreed.append(
                    f"bash passed {message!r} but " +
                    ", ".join(f"{n} {v}" for n, v in sorted(bad.items()))
                )
            else:
                rep.agreed.append((message, node))

    # Anything pytest ran that no mapping entry names. Not an error --
    # commit 10 adds coverage bash never had -- but it is listed so the
    # difference between "new" and "accidentally orphaned" stays visible.
    mapped = set()
    for entry in suite.entries:
        for node in entry.nodes:
            mapped.update(resolve_node(node, pytest_verdicts) or [node])
    for node, verdict in sorted(pytest_verdicts.items()):
        if node not in mapped:
            rep.pytest_only.append(f"{node} ({verdict})")

    return rep


def format_report(rep: Report) -> str:
    out: List[str] = [f"=== dual-run: {rep.suite} ==="]
    if rep.fatal:
        out.extend(f"  FATAL: {m}" for m in rep.fatal)
        return "\n".join(out)

    out.append(f"  agreed:      {len(rep.agreed)}")
    if rep.disagreed:
        out.append(f"  DISAGREED:   {len(rep.disagreed)}")
        out.extend(f"    - {m}" for m in rep.disagreed)
    if rep.unmapped:
        out.append(
            f"  UNMAPPED:    {len(rep.unmapped)} bash assertion(s) with no pytest "
            f"counterpart in the mapping"
        )
        out.extend(f"    - {m}" for m in rep.unmapped)
    if rep.missing_nodes:
        out.append(f"  MISSING:     {len(rep.missing_nodes)} mapped pytest test(s) did not run")
        out.extend(f"    - {m}" for m in rep.missing_nodes)
    if rep.pytest_only:
        out.append(f"  pytest-only: {len(rep.pytest_only)} (new coverage, informational)")
        out.extend(f"    - {m}" for m in rep.pytest_only)
    out.append(f"  => {'CLEAN' if rep.clean else 'NOT CLEAN'}")
    return "\n".join(out)


# -------------------------------------------------------------------- cli


def main(argv: Sequence[str] | None = None) -> int:
    import argparse

    ap = argparse.ArgumentParser(
        prog="python -m purelb_e2e.dualrun",
        description="Compare a bash e2e suite's verdicts against its pytest port.",
    )
    ap.add_argument("--suite", required=True, help="suite name, a table in the mapping file")
    ap.add_argument("--map", required=True, type=Path, help="path to dualrun-map.yaml")
    ap.add_argument("--bash-log", type=Path, help="captured bash suite output")
    ap.add_argument("--bash-exit", type=int, help="bash suite exit code")
    ap.add_argument("--junit", type=Path, help="pytest --junitxml output")
    # So the shell driver never has to parse TOML, and the mapping stays
    # the single source of truth for which script pairs with which suite.
    ap.add_argument("--print-script", action="store_true",
                    help="print the suite's bash script path and exit")
    ap.add_argument("--print-modules", action="store_true",
                    help="print the pytest module paths this suite maps to, and exit")
    args = ap.parse_args(argv)

    suites = load_map(args.map)
    if args.suite not in suites:
        # stderr: the --print-* modes are consumed by command
        # substitution, so an error on stdout would be swallowed into
        # the caller's variable and vanish.
        print(f"no `{args.suite}` entry in {args.map}; "
              f"known: {', '.join(sorted(suites)) or '(none)'}", file=sys.stderr)
        return 2

    if args.print_script:
        print(suites[args.suite].script)
        return 0

    if args.print_modules:
        # Derived from the mapped node ids rather than declared separately,
        # so the mapping cannot name tests in a module the runner never
        # collects -- which would show up as every test "not running".
        mods = {n.split("::", 1)[0] for e in suites[args.suite].entries for n in e.nodes}
        print(" ".join(sorted(mods)))
        return 0

    missing = [f for f in ("bash_log", "bash_exit", "junit") if getattr(args, f) is None]
    if missing:
        ap.error("required for a comparison: " + ", ".join("--" + m.replace("_", "-") for m in missing))

    bash = parse_bash_output(args.bash_log.read_text(errors="replace"), args.bash_exit)
    rep = compare(suites[args.suite], bash, parse_junit(args.junit))
    print(format_report(rep))
    return 0 if rep.clean else 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
