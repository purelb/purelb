# PureLB E2E Tests

The suite is [pytest](py/). There are no bash suites left; the last of
them was retired once `scripts/e2e-dualrun.sh` confirmed every one of its
assertions had a passing pytest counterpart.

    cd py
    python3 -m venv .venv && .venv/bin/pip install -e .
    ../../../scripts/reset-test-cluster.sh --context <ctx> --yes
    .venv/bin/pytest --context <ctx> [--router-host <frr-host>]

See [py/README.md](py/README.md) for the options, the BGP setup, and why
the harness is shaped the way it is.

## What is here

| Path | |
|------|--|
| [py/](py/) | the suite |
| [nginx-test.yaml](nginx-test.yaml) | the backend every module exercises, in namespace `test`; `reset-test-cluster.sh` applies it |
| [dualrun-map.yaml](dualrun-map.yaml) | empty, and that is its finished state |

## The dual-run

`scripts/e2e-dualrun.sh` ran a bash suite and its pytest port against the
same cluster and diffed their verdicts assertion by assertion, using the
mapping. Every bash assertion had to name the pytest test replacing it,
and an unmapped one failed the run -- so deleting bash code could never
by itself make the comparison clean.

It is kept, empty, because it is the tool for the next port rather than
scaffolding for the last one. Writing those mappings found real coverage
gaps in **every** suite it was applied to: `balancePools` (15 assertions,
no pytest counterpart at all), remote `aggregation`, remote reachability,
and four absence assertions in the router suite. None of them were
visible from reading the ports.

It also, once, reported CLEAN having compared nothing -- a suite rejected
an argument, printed a usage error, exited 0, and produced no assertions.
An empty assertion list is now fatal.
