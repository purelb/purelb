#!/bin/bash
#
# Run a bash e2e suite and its pytest port against the same cluster, then
# diff their verdicts assertion by assertion.
#
# This is the gate for deleting a bash suite. The migration's real risk is
# not a port that breaks loudly -- it is a port that quietly covers less
# than the thing it replaces, because 5000 lines of bash collapse into a
# few hundred lines of parametrized Python and no amount of reading tells
# you reliably whether an assertion went missing. So each bash assertion
# must name the pytest test that replaces it in test/e2e/dualrun-map.toml,
# and an unmapped one is an error: deleting bash code alone can never make
# this run clean.
#
# The suites are run SEQUENTIALLY and the cluster is reset between them.
# They fight over the same ServiceGroups, the same address pools and the
# same node taints, so running them concurrently would produce
# disagreements that are artefacts of the harness rather than the port.
#
# Usage:
#   scripts/e2e-dualrun.sh --suite ipam-external --context prox-purelb2
#
# Options:
#   --suite NAME      suite to compare (a table in dualrun-map.toml)
#   --context NAME    kubectl context (required, no default)
#   --no-reset        skip the cluster reset between the two runs
#   --keep-logs       do not delete the run directory on success
#   --bash-args "…"   extra args passed through to the bash suite
#   --pytest-args "…" extra args passed through to pytest
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY_DIR="${REPO_ROOT}/test/e2e/py"
MAP="${REPO_ROOT}/test/e2e/dualrun-map.toml"

SUITE=""
CONTEXT=""
RESET=true
KEEP_LOGS=false
BASH_ARGS=""
PYTEST_ARGS=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --suite)       SUITE="$2"; shift 2 ;;
        --context)     CONTEXT="$2"; shift 2 ;;
        --no-reset)    RESET=false; shift ;;
        --keep-logs)   KEEP_LOGS=true; shift ;;
        --bash-args)   BASH_ARGS="$2"; shift 2 ;;
        --pytest-args) PYTEST_ARGS="$2"; shift 2 ;;
        -h|--help)     tail -n +2 "$0" | grep '^#' | sed 's/^# \{0,1\}//' | head -31; exit 0 ;;
        *)             echo "Unknown option: $1 (use -h for help)" >&2; exit 2 ;;
    esac
done

[ -n "$SUITE" ]   || { echo "ERROR: --suite is required" >&2; exit 2; }
# Deliberately no default. Defaulting a destructive suite to the current
# context is how it eventually runs against the wrong cluster.
[ -n "$CONTEXT" ] || { echo "ERROR: --context is required" >&2; exit 2; }

# The venv is the harness's own; falling back to the ambient python3 would
# work until it silently did not have the kubernetes client.
PYTHON="${PY_DIR}/.venv/bin/python"
if [ ! -x "$PYTHON" ]; then
    echo "ERROR: ${PYTHON} not found. Create it with:" >&2
    echo "  python3 -m venv ${PY_DIR}/.venv && ${PY_DIR}/.venv/bin/pip install -e ${PY_DIR}" >&2
    exit 2
fi

SCRIPT_REL="$("$PYTHON" -m purelb_e2e.dualrun --suite "$SUITE" --map "$MAP" --print-script)"
BASH_SCRIPT="${REPO_ROOT}/${SCRIPT_REL}"
[ -x "$BASH_SCRIPT" ] || { echo "ERROR: ${BASH_SCRIPT} is not executable" >&2; exit 2; }

RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/purelb-dualrun-${SUITE}-XXXXXX")"
BASH_LOG="${RUN_DIR}/bash.log"
JUNIT="${RUN_DIR}/pytest.xml"
PYTEST_LOG="${RUN_DIR}/pytest.log"

cleanup() {
    if [ "$KEEP_LOGS" = true ] || [ "${DUALRUN_FAILED:-0}" = 1 ]; then
        echo "logs: ${RUN_DIR}"
    else
        rm -rf "$RUN_DIR"
    fi
}
trap cleanup EXIT

reset_cluster() {
    [ "$RESET" = true ] || { echo "→ skipping reset (--no-reset)"; return 0; }
    echo "→ resetting ${CONTEXT}"
    "${REPO_ROOT}/scripts/reset-test-cluster.sh" --context "$CONTEXT" --yes
}

echo "=== dual-run: ${SUITE} on ${CONTEXT} ==="

# --- bash -----------------------------------------------------------------
reset_cluster
echo "→ bash: ${SCRIPT_REL}"
BASH_EXIT=0
# `|| BASH_EXIT=$?` rather than letting set -e kill us: a failing bash run
# is data for the comparison, not a reason to stop. The comparison then
# reports it as fatal, because a suite that exits early leaves its
# remaining assertions silent rather than failing.
# shellcheck disable=SC2086  # BASH_ARGS is a deliberate word-split passthrough
"$BASH_SCRIPT" --context "$CONTEXT" $BASH_ARGS 2>&1 | tee "$BASH_LOG" || true
BASH_EXIT=${PIPESTATUS[0]}
echo "→ bash exited ${BASH_EXIT}"

# --- pytest ---------------------------------------------------------------
reset_cluster
MODULES="$("$PYTHON" -m purelb_e2e.dualrun --suite "$SUITE" --map "$MAP" --print-modules)"
echo "→ pytest: ${MODULES}"
# shellcheck disable=SC2086  # MODULES and PYTEST_ARGS are deliberate word-split passthroughs
(cd "$PY_DIR" && "${PY_DIR}/.venv/bin/pytest" \
    --context "$CONTEXT" \
    --junitxml="$JUNIT" \
    $MODULES $PYTEST_ARGS 2>&1) | tee "$PYTEST_LOG" || true
PYTEST_EXIT=${PIPESTATUS[0]}
echo "→ pytest exited ${PYTEST_EXIT}"

[ -f "$JUNIT" ] || { DUALRUN_FAILED=1; echo "ERROR: pytest produced no junit XML" >&2; exit 1; }

# --- compare --------------------------------------------------------------
echo
if "$PYTHON" -m purelb_e2e.dualrun \
        --suite "$SUITE" --map "$MAP" \
        --bash-log "$BASH_LOG" --bash-exit "$BASH_EXIT" --junit "$JUNIT"; then
    echo
    echo "dual-run CLEAN: every bash assertion has a passing pytest counterpart."
    exit 0
fi

DUALRUN_FAILED=1
echo
echo "dual-run NOT CLEAN — do not delete the bash suite." >&2
exit 1
