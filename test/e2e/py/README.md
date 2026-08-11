# PureLB e2e (pytest)

    python3 -m venv .venv && .venv/bin/pip install -e .
    .venv/bin/pytest --context prox-purelb2

Reset the cluster first, so a run starts from a known baseline:

    ../../../scripts/reset-test-cluster.sh --context prox-purelb2 --yes

## Options

| Flag | Meaning |
|------|---------|
| `--context` | kubectl context. **Required**, no default. |
| `--purelb-namespace` | install namespace (default `purelb-system`) |
| `--router-host` | upstream router, enables on-the-wire GARP/NA assertions |
| `--require a,b` | fail rather than skip when a capability is missing |

## Why the harness looks like this

Each of these is a bug the bash suite actually had, designed out rather
than fixed:

- `metrics.scrape` **raises** instead of returning empty. Callers used to
  write `if [ -n "$METRICS" ]`, so a failed scrape skipped the assertions
  and the test passed.
- Counter helpers take a **baseline**, so the natural thing to write is a
  delta. `> 0` on a monotonic counter asks whether something has ever
  happened, not whether it just did.
- `pod_logs` **requires** a time window. Without one, assertions matched
  lines emitted by earlier tests on other nodes.
- `pod_on_node` uses a **field selector**. Matching `kubectl get pods -o wide`
  text also matched the NAME and IP columns, and could not tell
  `purelb2-1` from `purelb2-10`.
- Fixtures **stack**, so cleanup cannot be lost. Bash keeps one EXIT trap,
  and a suite that set its own silently replaced the shared one.
- Skips are **counted and reported**, and `--require` makes them fatal.
