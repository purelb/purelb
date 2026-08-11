#!/bin/bash
#
# Pure, side-effect-free helpers shared by every PureLB e2e suite.
#
# Split out of common.sh so the suites that do NOT want cluster discovery
# (router/, single-node/) can still share one definition of the logging and
# assertion helpers. common.sh sources this file and adds SSH-based node,
# subnet and IPv6 discovery on top; sourcing common.sh therefore reaches out
# to every node, which is why the workstation-driven suites source lib.sh
# directly instead.
#
# Nothing in here runs at source time except variable assignment.
#

if [[ ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "ERROR: Bash 4+ required for associative arrays"
    exit 1
fi

# Guard against double-sourcing (common.sh sources this, and a suite may
# source both). Written as an if rather than `[ ... ] && return 0`: every
# suite runs under `set -e`, where a && list that evaluates false at the
# top level of a sourced file aborts the caller.
if [ -n "${PURELB_LIB_SOURCED:-}" ]; then
    return 0
fi
PURELB_LIB_SOURCED=1

# Consumed by the suites that source this file, so shellcheck cannot see
# the uses from here.
# shellcheck disable=SC2034
{
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'
}

pass() { echo -e "${GREEN}✓ PASS:${NC} $1"; }

# dump_debug_state is called by fail() just before exiting. This default is
# a no-op; a suite that can say something useful about why it failed
# overrides it after sourcing. Reconciled here because the copies had
# diverged: remote/, router/ and single-node/ all dumped state on failure
# while common.sh -- used by the flagship local suite -- did not, so the
# most-run suite produced the least diagnostic output.
dump_debug_state() { :; }

fail() { echo -e "${RED}✗ FAIL:${NC} $1"; dump_debug_state; exit 1; }
info() { echo -e "${YELLOW}→${NC} $1"; }
detail() { echo -e "${CYAN}     ${NC} $1"; }
ts() { date '+%H:%M:%S.%3N'; }
# Return "nodename (IP)" for human-readable display
node_label() { local n=$1; echo "$n (${NODE_IPS[$n]})"; }

# =====================================================================
# Context and CLI Arguments
# =====================================================================

# Default context: use current kubectl context if not set by caller.
# ${CONTEXT:-} rather than $CONTEXT: no suite runs under `set -u` today, but
# lib.sh is now sourced by scripts that might, and an unbound-variable abort
# here would be reported as a sourcing failure with no obvious cause.
if [ -z "${CONTEXT:-}" ]; then
    CONTEXT=$(command kubectl config current-context 2>/dev/null)
fi

# PureLB install namespace. Overridable so a suite can target a
# non-default install; the promoted assertion helpers below all honour it.
PURELB_NS="${PURELB_NS:-purelb-system}"

# Override kubectl to always use the configured context
kubectl() { command kubectl --context "$CONTEXT" "$@"; }

# Parse --context from remaining args (call from your script after sourcing)
# Usage: parse_common_args "$@" ; set -- "${REMAINING_ARGS[@]}"
REMAINING_ARGS=()
parse_common_args() {
    REMAINING_ARGS=()
    while [[ $# -gt 0 ]]; do
        case $1 in
            --context)
                CONTEXT="$2"
                shift 2
                ;;
            *)
                REMAINING_ARGS+=("$1")
                shift
                ;;
        esac
    done
}

# =====================================================================
# Metrics & Logging Assertions
#
# Promoted here from the local suite. These had drifted into five
# separate copies (four distinct implementations of scrape_pod_metrics
# alone), so a fix applied to one suite silently left the others
# asserting the old way.
# =====================================================================

#---------------------------------------------------------------------

# scrape_metrics PORT_FORWARD_TARGET
# Scrapes /metrics from a pod via kubectl port-forward.
# Writes raw metrics to stdout. Caller should capture output.
# Usage: OUTPUT=$(scrape_pod_metrics <pod-name>)
scrape_pod_metrics() {
    local pod=$1
    local local_port=$((30000 + RANDOM % 5000))
    # Start port-forward in background
    kubectl port-forward -n "$PURELB_NS" "$pod" ${local_port}:7472 >/dev/null 2>&1 &
    local pf_pid=$!
    # Retry curl until port-forward is ready (up to 5 attempts)
    local metrics=""
    local attempt
    # shellcheck disable=SC2034  # loop counter, body does not read it
    for attempt in 1 2 3 4 5; do
        sleep 1
        # A dead port-forward will never come good -- most often the local
        # port was already bound. Stop retrying and let the caller report
        # it, rather than burning 5s to produce the same empty string.
        if ! kill -0 "$pf_pid" 2>/dev/null; then
            break
        fi
        metrics=$(curl -s --connect-timeout 3 "http://127.0.0.1:${local_port}/metrics" 2>/dev/null || true)
        if [ -n "$metrics" ]; then
            break
        fi
    done
    kill $pf_pid 2>/dev/null || true
    wait $pf_pid 2>/dev/null || true
    echo "$metrics"
}

# require_metrics VALUE DESCRIPTION
#
# Fails the suite when a scrape came back empty.
#
# Callers used to guard their assertion blocks with `if [ -n "$X_METRICS" ]`,
# which reads as defensive but inverts the semantics: a failed scrape SKIPPED
# every assertion in the block and the test passed. At one site there was no
# else branch at all, so five assertions vanished silently.
#
# This is a separate helper rather than a `fail` inside scrape_pod_metrics
# because the scrape is always called as `X=$(scrape_pod_metrics ...)`, and
# `exit` inside a command substitution only kills the subshell -- the script
# would print FAIL and carry on.
require_metrics() {
    local value="$1"
    local what="$2"
    [ -n "$value" ] || fail "metrics scrape failed for ${what} (port-forward or :7472 unreachable) -- cannot assert"
}

# assert_metric_increased BEFORE AFTER METRIC [MIN_DELTA]
#
# Prometheus counters are monotonic since process start, so `assert_metric
# ... gt 0` asks "has this ever happened", not "did it happen just now". A
# failover that produced zero new election wins passed, and on the second
# iteration of a -n run the assertion was true no matter what.
assert_metric_increased() {
    local before_text="$1" after_text="$2" metric="$3" min_delta="${4:-1}"
    local b a delta
    b=$(extract_metric "$before_text" "$metric"); b=${b:-0}
    a=$(extract_metric "$after_text" "$metric"); a=${a:-0}
    delta=$((a - b))
    [ "$delta" -ge "$min_delta" ] \
        || fail "Metric $metric increased by $delta (${b} -> ${a}), expected >= $min_delta"
    detail "$metric +${delta} (${b} -> ${a})"
}

# snapshot_lbnodeagent_metrics
#
# Captures every node's lbnodeagent metrics into LBNA_SNAPSHOT, keyed by
# node name. Call it before an action whose winner is not known in
# advance (a failover, an election): afterwards you can recover the
# pre-action counters for whichever node actually won.
declare -gA LBNA_SNAPSHOT=()
snapshot_lbnodeagent_metrics() {
    LBNA_SNAPSHOT=()
    local node
    for node in "${NODES[@]}"; do
        LBNA_SNAPSHOT["$node"]=$(scrape_lbnodeagent_metrics "$node" || true)
    done
}

# assert_node_metric_increased NODE METRIC [MIN_DELTA]
#
# Compares NODE's current counter against the snapshot taken earlier.
# This is what makes "did this failover produce an election win" a real
# question -- `assert_metric ... gt 0` only asked whether the agent had
# ever won anything since it started.
assert_node_metric_increased() {
    local node="$1" metric="$2" min_delta="${3:-1}"
    local after
    after=$(scrape_lbnodeagent_metrics "$node")
    require_metrics "$after" "lbnodeagent on $node"
    assert_metric_increased "${LBNA_SNAPSHOT[$node]:-}" "$after" "$metric" "$min_delta"
}

# assert_metric_not_increased BEFORE AFTER METRIC
#
# The same bug in the other direction. Asserting a cumulative error counter
# is `== 0` fails forever once anything has ever incremented it, including
# something legitimate: deleting a ServiceGroup makes the allocator's next
# status write 404, which lands in sg_status_writes_total{outcome="other"}
# and then fails every subsequent run until the allocator restarts.
assert_metric_not_increased() {
    local before_text="$1" after_text="$2" metric="$3"
    local b a
    b=$(extract_metric "$before_text" "$metric"); b=${b:-0}
    a=$(extract_metric "$after_text" "$metric"); a=${a:-0}
    [ "$a" -le "$b" ] || fail "Metric $metric increased by $((a - b)) (${b} -> ${a}), expected no increase"
}

# scrape_allocator_metrics
# Scrapes metrics from the allocator deployment pod.
scrape_allocator_metrics() {
    local pod
    pod=$(kubectl get pods -n "$PURELB_NS" -l component=allocator -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -z "$pod" ]; then
        echo ""
        return
    fi
    scrape_pod_metrics "$pod"
}

# scrape_lbnodeagent_metrics [node]
# Scrapes metrics from an lbnodeagent pod. LBNodeAgent uses hostPort 7472,
# so we curl the node IP directly instead of using port-forward.
# If node is specified, scrapes that node. Otherwise scrapes the first node.
scrape_lbnodeagent_metrics() {
    local node=${1:-}
    local node_ip
    if [ -n "$node" ]; then
        node_ip="${NODE_IPS[$node]:-}"
    else
        # Use first node
        for n in $NODES; do
            node_ip="${NODE_IPS[$n]:-}"
            break
        done
    fi
    if [ -z "$node_ip" ]; then
        echo ""
        return
    fi
    curl -s --connect-timeout 5 "http://${node_ip}:7472/metrics" 2>/dev/null || true
}

# assert_metric METRICS_OUTPUT METRIC_NAME COMPARISON VALUE
# Checks that a metric exists and satisfies the comparison.
# COMPARISON: "ge" (>=), "gt" (>), "eq" (==), "exists" (just present)
# For gauge/counter without labels, extracts the bare metric line.
# For labeled metrics, pass the full metric name with labels.
# Example: assert_metric "$OUT" "purelb_election_lease_healthy" "eq" "1"
# Example: assert_metric "$OUT" 'purelb_address_pool_size{pool="default"}' "gt" "0"
assert_metric() {
    local metrics="$1"
    local metric_name="$2"
    local comparison="$3"
    local expected="${4:-}"

    # Extract the metric value - handle both labeled and unlabeled
    local value
    if echo "$metric_name" | grep -q '{'; then
        # Labeled metric: use grep -F for fixed-string (literal) match
        value=$(echo "$metrics" | grep -F "$metric_name" | head -1 | awk '{print $NF}')
    else
        # Unlabeled metric: match exactly (no labels)
        value=$(echo "$metrics" | grep "^${metric_name} " | head -1 | awk '{print $NF}')
    fi

    if [ -z "$value" ]; then
        if [ "$comparison" = "exists" ]; then
            fail "Metric $metric_name not found in output"
        fi
        fail "Metric $metric_name not found in output"
    fi

    if [ "$comparison" = "exists" ]; then
        return 0
    fi

    # Convert scientific notation and floats to integers for comparison
    local int_value
    int_value=$(printf '%.0f' "$value" 2>/dev/null || echo "0")
    local int_expected
    int_expected=$(printf '%.0f' "$expected" 2>/dev/null || echo "0")

    case "$comparison" in
        ge)
            [ "$int_value" -ge "$int_expected" ] || fail "Metric $metric_name: expected >= $expected, got $value"
            ;;
        gt)
            [ "$int_value" -gt "$int_expected" ] || fail "Metric $metric_name: expected > $expected, got $value"
            ;;
        eq)
            [ "$int_value" -eq "$int_expected" ] || fail "Metric $metric_name: expected == $expected, got $value"
            ;;
        *)
            fail "Unknown comparison: $comparison"
            ;;
    esac
}

# assert_log_contains COMPONENT PATTERN DESCRIPTION
# Checks that recent logs from a component contain the given pattern.
# COMPONENT: "allocator" or "lbnodeagent"
# For lbnodeagent, checks ALL pods (any match counts as success).
# log_window_start
#
# Emits an RFC3339 timestamp for use as the LOG_SINCE of a later
# assert_log_contains. Call it immediately before the action whose log
# output you intend to assert on.
#
# Without a window the assertions below scanned `--tail=200` across every
# lbnodeagent pod and returned success on the first match anywhere, so a
# line emitted by an EARLIER test on a DIFFERENT node satisfied them. With
# -n iterations that is close to guaranteed.
log_window_start() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# _logs_since NAMESPACE POD CONTAINER SINCE
# Reads logs from SINCE if given, otherwise falls back to the old
# --tail=200 behaviour so an un-migrated caller still works.
_logs_since() {
    local pod="$1" container="$2" since="$3"
    if [ -n "$since" ]; then
        kubectl logs -n "$PURELB_NS" "$pod" ${container:+-c "$container"} --since-time="$since" --tail=-1 2>/dev/null
    else
        kubectl logs -n "$PURELB_NS" "$pod" ${container:+-c "$container"} --tail=200 2>/dev/null
    fi
}

# assert_log_contains COMPONENT PATTERN DESCRIPTION [SINCE]
#
# SINCE (from log_window_start) scopes the search in time. Pass it whenever
# the assertion is about something this test just caused.
#
# NOTE ON PATTERNS: match a field that identifies the OUTCOME, not just the
# op name. PureLB logs `"op":"updateSGStatus"` on both the success and the
# error path, so grepping the op alone asserts "this code ran", not "it
# worked" -- an assertion labelled "status write logged" was passing on
# lines reading {"error":"servicegroups.purelb.io \"default\" not found"}.
assert_log_contains() {
    local component="$1"
    local pattern="$2"
    local description="$3"
    local since="${4:-}"

    if [ "$component" = "allocator" ]; then
        if _logs_since "deployment/allocator" "" "$since" | grep -q "$pattern"; then
            return 0
        fi
        fail "Allocator logs missing: $description (pattern: $pattern${since:+, since $since})"
    elif [ "$component" = "lbnodeagent" ]; then
        local pods
        pods=$(kubectl get pods -n "$PURELB_NS" -l component=lbnodeagent -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)
        for pod in $pods; do
            if _logs_since "$pod" "lbnodeagent" "$since" | grep -q "$pattern"; then
                return 0
            fi
        done
        fail "LBNodeAgent logs missing on all pods: $description (pattern: $pattern${since:+, since $since})"
    else
        fail "Unknown component: $component"
    fi
}

# assert_log_contains_on_node NODE PATTERN DESCRIPTION [SINCE]
#
# Resolves the pod by field selector on spec.nodeName. The previous
# implementation piped `kubectl get pods -o wide` through `grep "$node"`,
# which also matched the NAME and IP columns and could not tell purelb2-1
# from purelb2-10 -- two matches produced a two-word $pod and a confusing
# `kubectl logs` error, one wrong match asserted against the wrong node.
# common.sh:announcing_has_node already had the entry-wise fix for exactly
# this hazard; it was never carried here.
assert_log_contains_on_node() {
    local node="$1"
    local pattern="$2"
    local description="$3"
    local since="${4:-}"

    local pod
    pod=$(kubectl get pods -n "$PURELB_NS" -l component=lbnodeagent \
            --field-selector "spec.nodeName=$node" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -z "$pod" ]; then
        fail "No lbnodeagent pod found on node $node"
    fi
    if _logs_since "$pod" "lbnodeagent" "$since" | grep -q "$pattern"; then
        return 0
    fi
    fail "LBNodeAgent logs on $node missing: $description (pattern: $pattern${since:+, since $since})"
}

# scrape_allocator_metric METRIC_NAME
# Quick helper: scrapes allocator and returns just the value for one metric.
scrape_allocator_metric() {
    local metric_name="$1"
    local metrics
    metrics=$(scrape_allocator_metrics)
    if echo "$metric_name" | grep -q '{'; then
        echo "$metrics" | grep -F "$metric_name" | head -1 | awk '{print $NF}'
    else
        echo "$metrics" | grep "^${metric_name} " | head -1 | awk '{print $NF}'
    fi
}

# extract_metric METRICS_TEXT METRIC_NAME
# Extracts the numeric value of a metric from raw Prometheus text.
# Returns empty string if not found.
extract_metric() {
    local metrics="$1"
    local metric_name="$2"
    local value
    if echo "$metric_name" | grep -q '{'; then
        value=$(echo "$metrics" | grep -F "$metric_name" | head -1 | awk '{print $NF}')
    else
        value=$(echo "$metrics" | grep "^${metric_name} " | head -1 | awk '{print $NF}')
    fi
    # Convert to integer for display (metrics are often floats like 3.0)
    if [ -n "$value" ]; then
        printf '%.0f' "$value" 2>/dev/null || echo "$value"
    fi
}

# print_allocator_metrics METRICS_TEXT [POOL_NAME]
# Displays key allocator metrics in a compact summary.
# POOL_NAME defaults to "default".
print_allocator_metrics() {
    local metrics="$1"
    local pool="${2:-default}"
    if [ -z "$metrics" ]; then
        detail "allocator metrics: (unavailable)"
        return
    fi
    local pool_size in_use config_loaded
    config_loaded=$(extract_metric "$metrics" "purelb_k8s_client_config_loaded_bool")
    pool_size=$(extract_metric "$metrics" "purelb_address_pool_size{pool=\"${pool}\"}")
    in_use=$(extract_metric "$metrics" "purelb_address_pool_addresses_in_use{pool=\"${pool}\"}")
    echo -e "${CYAN}    ── Metrics ────────────────────────────────────────────────${NC}"
    detail "allocator │ config_loaded=${config_loaded:-?}  pool_size(${pool})=${pool_size:-?}  in_use(${pool})=${in_use:-?}"
}

# print_lbnodeagent_metrics METRICS_TEXT NODE_NAME
# Displays key lbnodeagent metrics in a compact summary.
print_lbnodeagent_metrics() {
    local metrics="$1"
    local node="${2:-?}"
    if [ -z "$metrics" ]; then
        detail "lbnodeagent metrics on $node: (unavailable)"
        return
    fi
    local lease_healthy member_count subnet_count wins adds withdrawals garp renewals
    lease_healthy=$(extract_metric "$metrics" "purelb_election_lease_healthy")
    member_count=$(extract_metric "$metrics" "purelb_election_member_count")
    subnet_count=$(extract_metric "$metrics" "purelb_election_subnet_count")
    wins=$(extract_metric "$metrics" "purelb_lbnodeagent_election_wins_total")
    adds=$(extract_metric "$metrics" "purelb_lbnodeagent_address_additions_total")
    withdrawals=$(extract_metric "$metrics" "purelb_lbnodeagent_address_withdrawals_total")
    garp=$(extract_metric "$metrics" "purelb_lbnodeagent_garp_sent_total")
    renewals=$(extract_metric "$metrics" "purelb_election_lease_renewals_total")
    echo -e "${CYAN}    ── Metrics ────────────────────────────────────────────────${NC}"
    detail "lbnodeagent($node) │ lease_healthy=${lease_healthy:-?}  members=${member_count:-?}  subnets=${subnet_count:-?}"
    detail "  counters │ wins=${wins:-0}  adds=${adds:-0}  withdrawals=${withdrawals:-0}  garp=${garp:-0}  lease_renewals=${renewals:-0}"
}


# =====================================================================
