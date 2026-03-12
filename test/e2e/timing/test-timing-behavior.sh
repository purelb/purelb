#!/bin/bash
set -e

# PureLB Timing Behavior Test Suite
#
# Characterizes timing behavior for local-pool clusters where VIPs land on
# the subnet interface (eth1/NODE_IFACE) of the election-winning node, NOT on
# kube-lb0 (which is only used for remote/BGP pools).
#
# Tests:
#   B1  VIP Placement Latency:     service create → VIP on eth1 of election winner
#   B3  VIP Withdrawal Latency:    service delete → VIP removed from eth1
#   D1  nftables Latency:          VIP on eth1 → kube-proxy nftables rules ready
#   D3  End-to-End Traffic:        service create → first successful curl (in-cluster)
#   E1  VIP Stability Under Scale: VIP stays on correct subnet node across replica changes
#   E3  Election Re-convergence:   lbnodeagent killed on winner → VIP on new node
#
# Run with optional iteration count:
#   bash test-timing-behavior.sh [ITERATIONS]   # default 3

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="test"
TIMING_SG="timing-test"
PURELB_NS="${PURELB_NS:-purelb-system}"

# Source common cluster topology helpers (provides CONTEXT, NODES, NODE_COUNT,
# NODE_IPS, NODE_IFACE, SUBNET_NODES, node_ssh, get_vip_holder, generate_default_servicegroup, etc.)
source "$SCRIPT_DIR/../common.sh"

# warn is not in common.sh; add it here so callers can flag non-fatal issues
warn() { echo -e "${YELLOW}!${NC} $1" >&2; }

# Output file for timing data
TIMING_LOG="${SCRIPT_DIR}/timing-results-$(date +%Y%m%d-%H%M%S).csv"

#---------------------------------------------------------------------
# Timing Primitives
#---------------------------------------------------------------------

now_ms() { date +%s%3N; }

record_event() {
    local TEST=$1 EVENT=$2 VALUE=$3
    echo "$TEST,$EVENT,$VALUE" >> "$TIMING_LOG"
}

# All poll_* functions output their result to stdout and always return 0.
# The caller checks for "-1" in the output to detect a timeout.
# This is important because the script runs with set -e — returning non-zero
# from a command substitution $(...) would immediately exit the script.

# Poll for VIP on a specific node+interface. Outputs ms elapsed or "-1".
poll_for_vip() {
    local NODE=$1 IP=$2 IFACE=$3 TIMEOUT=${4:-60000}
    local START=$(now_ms)
    while true; do
        local NOW=$(now_ms); local ELAPSED=$((NOW - START))
        if node_ssh "$NODE" "ip -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo "$ELAPSED"; return 0
        fi
        [ $ELAPSED -gt $TIMEOUT ] && { echo "-1"; return 0; }
        sleep 0.1
    done
}

# Poll for VIP removal from a specific node+interface. Outputs ms elapsed or "-1".
poll_for_vip_removal() {
    local NODE=$1 IP=$2 IFACE=$3 TIMEOUT=${4:-60000}
    local START=$(now_ms)
    while true; do
        local NOW=$(now_ms); local ELAPSED=$((NOW - START))
        if ! node_ssh "$NODE" "ip -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo "$ELAPSED"; return 0
        fi
        [ $ELAPSED -gt $TIMEOUT ] && { echo "-1"; return 0; }
        sleep 0.1
    done
}

# Poll across ALL nodes on their NODE_IFACE until one holds the VIP.
# Outputs "ELAPSED NODE" or "-1 NONE".
poll_for_vip_any_node() {
    local IP=$1 TIMEOUT=${2:-60000}
    local START=$(now_ms)
    while true; do
        local NOW=$(now_ms); local ELAPSED=$((NOW - START))
        for node in $NODES; do
            local iface="${NODE_IFACE[$node]}"
            if node_ssh "$node" "ip -o addr show $iface 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                echo "$ELAPSED $node"; return 0
            fi
        done
        [ $ELAPSED -gt $TIMEOUT ] && { echo "-1 NONE"; return 0; }
        sleep 0.1
    done
}

# Poll until NO node holds the VIP on its NODE_IFACE. Outputs ms elapsed or "-1".
poll_for_vip_removal_any_node() {
    local IP=$1 TIMEOUT=${2:-60000}
    local START=$(now_ms)
    while true; do
        local NOW=$(now_ms); local ELAPSED=$((NOW - START))
        local found=false
        for node in $NODES; do
            local iface="${NODE_IFACE[$node]}"
            if node_ssh "$node" "ip -o addr show $iface 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                found=true; break
            fi
        done
        [ "$found" = "false" ] && { echo "$ELAPSED"; return 0; }
        [ $ELAPSED -gt $TIMEOUT ] && { echo "-1"; return 0; }
        sleep 0.1
    done
}

# Poll nftables service-ips map on any node (kube-proxy runs cluster-wide).
# Outputs ms elapsed or "-1".
poll_for_nftables() {
    local NODE=$1 IP=$2 PORT=$3 TIMEOUT=${4:-30000}
    local START=$(now_ms)
    while true; do
        local NOW=$(now_ms); local ELAPSED=$((NOW - START))
        if node_ssh "$NODE" "sudo nft list map ip kube-proxy service-ips 2>/dev/null | grep -q '$IP.*$PORT'"; then
            echo "$ELAPSED"; return 0
        fi
        [ $ELAPSED -gt $TIMEOUT ] && { echo "-1"; return 0; }
        sleep 0.1
    done
}

# Poll for traffic from in-cluster curl-test pod. Outputs ms elapsed or "-1".
poll_for_traffic() {
    local IP=$1 PORT=${2:-80} TIMEOUT=${3:-30000}
    local START=$(now_ms)
    while true; do
        local NOW=$(now_ms); local ELAPSED=$((NOW - START))
        if kubectl exec -n $NAMESPACE curl-test -- curl -s --connect-timeout 2 "http://$IP:$PORT" >/dev/null 2>&1; then
            echo "$ELAPSED"; return 0
        fi
        [ $ELAPSED -gt $TIMEOUT ] && { echo "-1"; return 0; }
        sleep 0.2
    done
}

wait_for_ip_allocation() {
    local SVC=$1 TIMEOUT=${2:-60}
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/$SVC -n $NAMESPACE --timeout=${TIMEOUT}s >/dev/null 2>&1 || return 1
    kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
}

ensure_curl_pod() {
    kubectl get pod curl-test -n $NAMESPACE -o jsonpath='{.status.phase}' 2>/dev/null | grep -q Running && return 0
    info "Creating curl test pod..."
    kubectl delete pod curl-test -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl run curl-test -n $NAMESPACE --image=curlimages/curl:latest \
        --restart=Never --command -- sleep 3600 >/dev/null 2>&1
    kubectl wait --for=condition=Ready pod/curl-test -n $NAMESPACE --timeout=60s >/dev/null 2>&1
}

#---------------------------------------------------------------------
# ServiceGroup Setup
# Creates a dedicated "timing-test" ServiceGroup using non-overlapping
# IP ranges (.241-.250 for IPv4, c:: for IPv6) so timing tests can run
# concurrently with other tests without pool conflicts.
#---------------------------------------------------------------------
setup_timing_servicegroup() {
    info "Creating dedicated '$TIMING_SG' ServiceGroup for timing tests..."

    local v4pools=""
    local v6pools=""

    for s in $SUBNETS; do
        local pool
        pool=$(subnet_timing_pool_range "$s")
        v4pools="${v4pools}
    - aggregation: default
      pool: ${pool}
      subnet: ${s}"

        local v6sub="${SUBNET_V6[$s]}"
        local v6prefix="${SUBNET_V6_PREFIX[$s]}"
        if [ -n "$v6sub" ]; then
            local v6pool
            v6pool=$(v6_timing_pool_range "$v6prefix")
            v6pools="${v6pools}
    - aggregation: default
      pool: ${v6pool}
      subnet: ${v6sub}"
        fi
    done

    local yaml="apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: ${TIMING_SG}
  namespace: purelb-system
spec:
  local:
    v4pools:${v4pools}"

    if [ -n "$v6pools" ]; then
        yaml="${yaml}
    v6pools:${v6pools}"
    fi

    echo "$yaml" | kubectl apply -f - 2>&1
    pass "ServiceGroup '$TIMING_SG' applied"
}

# Create a Cluster ETP LoadBalancer service using the dedicated timing pool.
create_timing_svc() {
    local NAME=$1
    kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $NAME
  namespace: $NAMESPACE
  labels:
    test-suite: timing
  annotations:
    purelb.io/service-group: $TIMING_SG
spec:
  type: LoadBalancer
  externalTrafficPolicy: Cluster
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
EOF
}

#---------------------------------------------------------------------
# Statistics
#---------------------------------------------------------------------
calc_stats() {
    local -a VALUES=("$@")
    local COUNT=${#VALUES[@]}
    [ $COUNT -eq 0 ] && { echo "count=0,min=N/A,max=N/A,avg=N/A,p95=N/A"; return; }
    local SORTED=($(printf '%s\n' "${VALUES[@]}" | sort -n))
    local MIN=${SORTED[0]} MAX=${SORTED[$((COUNT-1))]}
    local SUM=0; for v in "${VALUES[@]}"; do SUM=$((SUM + v)); done
    local AVG=$((SUM / COUNT))
    local P95_IDX=$(( (COUNT * 95) / 100 ))
    [ $P95_IDX -ge $COUNT ] && P95_IDX=$((COUNT - 1))
    echo "count=$COUNT,min=$MIN,max=$MAX,avg=$AVG,p95=${SORTED[$P95_IDX]}"
}

#---------------------------------------------------------------------
# TEST B1: VIP Placement Latency
# Measures time from service creation to VIP on eth1 of the election winner.
#---------------------------------------------------------------------
test_b1_vip_placement() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST B1: VIP Placement Latency"
    echo "=========================================="
    echo "Service create → VIP on eth1 of election-winning node"
    echo ""

    local -a RESULTS=()

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        local SVC_NAME="timing-b1-$i"
        local T_CREATE=$(now_ms)
        create_timing_svc "$SVC_NAME"

        # Time to IP allocation (by allocator)
        local IP=$(wait_for_ip_allocation $SVC_NAME 30)
        if [ -z "$IP" ]; then
            warn "  Failed to get IP for $SVC_NAME"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        local T_ALLOCATED=$(now_ms)
        info "  IP: $IP (allocated in $((T_ALLOCATED - T_CREATE))ms)"

        # Time until VIP appears on election winner's eth1
        local RESULT_STR; RESULT_STR=$(poll_for_vip_any_node "$IP" 30000)
        local VIP_DELAY="${RESULT_STR%% *}"
        local VIP_NODE="${RESULT_STR##* }"
        local T_VIP=$(now_ms)
        local CREATE_TO_VIP=$((T_VIP - T_CREATE))

        if [ "$VIP_DELAY" = "-1" ]; then
            warn "  TIMEOUT: VIP not placed on any node's eth1 within 30s"
            record_event "B1" "iteration_$i" "TIMEOUT"
        else
            echo "  Service create → VIP on ${NODE_IFACE[$VIP_NODE]} of $(node_label $VIP_NODE) [${NODE_SUBNET[$VIP_NODE]}]: ${CREATE_TO_VIP}ms"
            echo "    (allocator: $((T_ALLOCATED - T_CREATE))ms, lbnodeagent: $((T_VIP - T_ALLOCATED))ms)"
            RESULTS+=($CREATE_TO_VIP)
            record_event "B1" "iteration_$i" "$CREATE_TO_VIP"
        fi

        kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
        sleep 2
    done

    echo ""
    echo "--- B1 Summary ---"
    [ ${#RESULTS[@]} -gt 0 ] && {
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Service Create → VIP on eth1: $STATS ms"
        record_event "B1" "summary" "$STATS"
    } || echo "No successful measurements"
}

#---------------------------------------------------------------------
# TEST B3: VIP Withdrawal Latency
# Measures time from service deletion to VIP removed from eth1.
# For local pools, VIP stays as long as the service exists (not endpoint-dependent).
#---------------------------------------------------------------------
test_b3_vip_withdrawal() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST B3: VIP Withdrawal Latency"
    echo "=========================================="
    echo "Service delete → VIP removed from eth1 of election winner"
    echo ""

    local -a RESULTS=()

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        # Create service and wait for VIP to be placed
        local SVC_NAME="timing-b3-$i"
        create_timing_svc "$SVC_NAME"
        local IP=$(wait_for_ip_allocation $SVC_NAME 30)
        if [ -z "$IP" ]; then
            warn "  Failed to get IP"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi

        local RESULT_STR; RESULT_STR=$(poll_for_vip_any_node "$IP" 30000)
        local VIP_DELAY="${RESULT_STR%% *}"
        local VIP_NODE="${RESULT_STR##* }"
        if [ "$VIP_DELAY" = "-1" ]; then
            warn "  TIMEOUT: VIP not placed; skipping withdrawal measurement"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        info "  VIP $IP on $(node_label $VIP_NODE) [${NODE_SUBNET[$VIP_NODE]}]"

        # Delete service and measure withdrawal time
        local T_DELETE=$(now_ms)
        kubectl delete svc $SVC_NAME -n $NAMESPACE >/dev/null

        local REMOVAL_DELAY=$(poll_for_vip_removal "$VIP_NODE" "$IP" "${NODE_IFACE[$VIP_NODE]}" 60000)
        local T_REMOVED=$(now_ms)
        local TOTAL=$((T_REMOVED - T_DELETE))

        if [ "$REMOVAL_DELAY" = "-1" ]; then
            warn "  TIMEOUT: VIP not removed within 60s"
            record_event "B3" "iteration_$i" "TIMEOUT"
        else
            echo "  Service delete → VIP removed from ${NODE_IFACE[$VIP_NODE]}: ${TOTAL}ms"
            RESULTS+=($TOTAL)
            record_event "B3" "iteration_$i" "$TOTAL"
        fi

        sleep 2
    done

    echo ""
    echo "--- B3 Summary ---"
    [ ${#RESULTS[@]} -gt 0 ] && {
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Service Delete → VIP Removed: $STATS ms"
        record_event "B3" "summary" "$STATS"
    } || echo "No successful measurements"
}

#---------------------------------------------------------------------
# TEST D1: nftables Rule Programming Latency
# Measures time from VIP on eth1 (election winner) to kube-proxy nftables rules ready.
#---------------------------------------------------------------------
test_d1_nftables_latency() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST D1: nftables Rule Programming Latency"
    echo "=========================================="
    echo "VIP on eth1 of election winner → kube-proxy nftables rules ready on any node"
    echo ""

    local -a RESULTS=()
    # kube-proxy runs everywhere, probe the first node for nftables checks
    local NFT_PROBE_NODE="${NODES%% *}"

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        local SVC_NAME="timing-d1-$i"
        local T_CREATE=$(now_ms)
        create_timing_svc "$SVC_NAME"

        local IP=$(wait_for_ip_allocation $SVC_NAME 30)
        if [ -z "$IP" ]; then
            warn "  Failed to get IP"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        local T_ALLOCATED=$(now_ms)
        info "  IP: $IP (allocated in $((T_ALLOCATED - T_CREATE))ms)"

        # Wait for VIP on election winner's eth1
        local RESULT_STR; RESULT_STR=$(poll_for_vip_any_node "$IP" 30000)
        local VIP_DELAY="${RESULT_STR%% *}"
        local VIP_NODE="${RESULT_STR##* }"
        local T_VIP=$(now_ms)

        if [ "$VIP_DELAY" = "-1" ]; then
            warn "  TIMEOUT: VIP not placed within 30s"
            record_event "D1" "iteration_$i" "TIMEOUT"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        info "  VIP on $(node_label $VIP_NODE) [${NODE_SUBNET[$VIP_NODE]}] in ${VIP_DELAY}ms"

        # Wait for nftables rules (kube-proxy programs all nodes)
        local NFT_DELAY=$(poll_for_nftables "$NFT_PROBE_NODE" "$IP" 80 30000)
        local T_NFT=$(now_ms)
        local VIP_TO_NFT=$((T_NFT - T_VIP))

        if [ "$NFT_DELAY" = "-1" ]; then
            warn "  TIMEOUT: nftables rules not ready within 30s"
            record_event "D1" "iteration_$i" "TIMEOUT"
        else
            echo "  VIP on eth1 → nftables ready: ${VIP_TO_NFT}ms  (service create → nftables: $((T_NFT - T_CREATE))ms)"
            RESULTS+=($VIP_TO_NFT)
            record_event "D1" "iteration_$i" "$VIP_TO_NFT"
        fi

        kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
        sleep 2
    done

    echo ""
    echo "--- D1 Summary ---"
    [ ${#RESULTS[@]} -gt 0 ] && {
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "VIP on eth1 → nftables ready: $STATS ms"
        record_event "D1" "summary" "$STATS"
    } || echo "No successful measurements"
}

#---------------------------------------------------------------------
# TEST D3: End-to-End Traffic Latency
# Total time from service creation to first successful curl (in-cluster pod).
#---------------------------------------------------------------------
test_d3_end_to_end() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST D3: End-to-End Traffic Latency"
    echo "=========================================="
    echo "Service create → first successful curl from in-cluster pod"
    echo ""

    ensure_curl_pod
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1

    local -a RESULTS=()

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        local SVC_NAME="timing-d3-$i"
        local T_CREATE=$(now_ms)
        create_timing_svc "$SVC_NAME"

        local IP=$(wait_for_ip_allocation $SVC_NAME 30)
        if [ -z "$IP" ]; then
            warn "  Failed to get IP"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        local T_ALLOCATED=$(now_ms)
        info "  IP: $IP (allocated in $((T_ALLOCATED - T_CREATE))ms)"

        local TRAFFIC_DELAY=$(poll_for_traffic $IP 80 60000)
        local T_TRAFFIC=$(now_ms)
        local TOTAL=$((T_TRAFFIC - T_CREATE))

        if [ "$TRAFFIC_DELAY" = "-1" ]; then
            warn "  TIMEOUT: Traffic not working within 60s"
            record_event "D3" "iteration_$i" "TIMEOUT"
        else
            echo "  Service create → traffic OK: ${TOTAL}ms"
            RESULTS+=($TOTAL)
            record_event "D3" "iteration_$i" "$TOTAL"
        fi

        kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
        sleep 2
    done

    echo ""
    echo "--- D3 Summary ---"
    [ ${#RESULTS[@]} -gt 0 ] && {
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Service Create → Traffic OK: $STATS ms"
        record_event "D3" "summary" "$STATS"
    } || echo "No successful measurements"
}

#---------------------------------------------------------------------
# TEST E1: VIP Stability Under Scaling
# For local-pool Cluster ETP, the VIP stays on exactly 1 subnet-matching node
# regardless of replica count (election-based, not endpoint-based).
#---------------------------------------------------------------------
test_e1_vip_stability() {
    echo ""
    echo "=========================================="
    echo "TEST E1: VIP Stability Under Scaling"
    echo "=========================================="
    echo "Verifies VIP stays on exactly 1 subnet-matching node across replica changes"
    echo "Scale sequence: 0→2→0→1→3→1→0"
    echo ""

    local SVC="timing-e1-stability"
    create_timing_svc "$SVC"
    local IP=$(wait_for_ip_allocation $SVC 30)
    [ -z "$IP" ] && { warn "Failed to get IP for E1 service"; kubectl delete svc $SVC -n $NAMESPACE --ignore-not-found >/dev/null; return; }
    info "VIP: $IP"

    # Determine which subnet the IP belongs to
    local IP_SUBNET=""
    for s in $SUBNETS; do
        if echo "$IP" | grep -q "^${s%.*}\."; then
            IP_SUBNET="$s"
            break
        fi
    done
    info "VIP subnet: $IP_SUBNET (eligible nodes: ${SUBNET_NODES[$IP_SUBNET]})"

    local SCALE_SEQUENCE=(0 2 0 1 3 1 0)
    local ERRORS=0

    for REPLICAS in "${SCALE_SEQUENCE[@]}"; do
        info "Scaling nginx to $REPLICAS replicas..."
        local T_START=$(now_ms)

        kubectl scale deployment nginx -n $NAMESPACE --replicas=$REPLICAS >/dev/null
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        if [ $REPLICAS -gt 0 ]; then
            kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        fi
        sleep 5

        # Count nodes with the VIP on their eth1 interface
        local VIP_COUNT=0
        local VIP_NODES="" WRONG_SUBNET_NODES=""
        for node in $NODES; do
            local iface="${NODE_IFACE[$node]}"
            if node_ssh "$node" "ip -o addr show $iface 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                VIP_COUNT=$((VIP_COUNT + 1))
                VIP_NODES="$VIP_NODES $(node_label $node)"
                if [ -n "$IP_SUBNET" ] && [ "${NODE_SUBNET[$node]}" != "$IP_SUBNET" ]; then
                    WRONG_SUBNET_NODES="$WRONG_SUBNET_NODES $(node_label $node)"
                fi
            fi
        done

        local T_END=$(now_ms)
        local DURATION=$((T_END - T_START))

        # For local pool, VIP should always be on exactly 1 subnet-matching node
        if [ $VIP_COUNT -eq 1 ] && [ -z "$WRONG_SUBNET_NODES" ]; then
            pass "Replicas=$REPLICAS: VIP on 1 subnet-matching node [$VIP_NODES] - ${DURATION}ms"
        elif [ $VIP_COUNT -eq 0 ]; then
            fail "Replicas=$REPLICAS: No node has VIP (should stay regardless of replica count)"
            ERRORS=$((ERRORS + 1))
        elif [ $VIP_COUNT -gt 1 ]; then
            fail "Replicas=$REPLICAS: SPLIT BRAIN — VIP on $VIP_COUNT nodes:$VIP_NODES"
            ERRORS=$((ERRORS + 1))
        else
            fail "Replicas=$REPLICAS: VIP on wrong subnet node(s):$WRONG_SUBNET_NODES"
            ERRORS=$((ERRORS + 1))
        fi

        record_event "E1" "scale_to_$REPLICAS" "$DURATION"
    done

    echo ""
    echo "--- E1 Summary ---"
    [ $ERRORS -eq 0 ] && pass "VIP remained stable on correct subnet node throughout all scaling transitions" \
        || fail "$ERRORS errors: VIP placement incorrect during scaling"

    kubectl delete svc $SVC -n $NAMESPACE --ignore-not-found >/dev/null
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
}

#---------------------------------------------------------------------
# TEST E3: Election Re-convergence Time
# Kills lbnodeagent pod on the current VIP winner and measures time for
# the VIP to appear on a new node's eth1 (new election winner).
#---------------------------------------------------------------------
test_e3_election_reconvergence() {
    local ITERATIONS=${1:-3}
    echo ""
    echo "=========================================="
    echo "TEST E3: Election Re-convergence Time"
    echo "=========================================="
    echo "lbnodeagent killed on winner → VIP on new node's eth1"
    echo ""

    local -a RESULTS=()

    local SVC="timing-e3-reconverge"
    create_timing_svc "$SVC"
    local IP=$(wait_for_ip_allocation $SVC 30)
    [ -z "$IP" ] && { warn "Failed to get IP for E3 service"; kubectl delete svc $SVC -n $NAMESPACE --ignore-not-found >/dev/null; return; }
    info "VIP: $IP"

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        # Find current winner (poll with short timeout in case VIP is still settling)
        local RESULT_STR; RESULT_STR=$(poll_for_vip_any_node "$IP" 30000)
        local VIP_DELAY="${RESULT_STR%% *}"
        local WINNER="${RESULT_STR##* }"
        if [ "$VIP_DELAY" = "-1" ]; then
            warn "  VIP not on any node; skipping iteration"
            continue
        fi
        info "  Current winner: $(node_label $WINNER) [${NODE_SUBNET[$WINNER]}]"

        # Determine which other nodes in the same subnet could take over
        local SUBNET="${NODE_SUBNET[$WINNER]}"
        local CANDIDATES=""
        for n in ${SUBNET_NODES[$SUBNET]}; do
            [ "$n" != "$WINNER" ] && CANDIDATES="$CANDIDATES $n"
        done
        if [ -z "$CANDIDATES" ]; then
            warn "  Only 1 node in subnet $SUBNET — cannot re-elect; skipping"
            continue
        fi

        # Taint the winner so its DaemonSet pod cannot restart there.
        # Without this, lbnodeagent restarts in ~3s — faster than the 10s lease
        # expiry — and re-wins before any candidate gets the VIP.
        kubectl taint node "$WINNER" e3-timing=true:NoSchedule 2>/dev/null || true

        local T_KILL=$(now_ms)
        kubectl delete pod -n $PURELB_NS -l component=lbnodeagent \
            --field-selector "spec.nodeName=$WINNER" --force --grace-period=0 2>/dev/null || true

        # Poll ONLY the candidate nodes for VIP appearance — do NOT use poll_for_vip_any_node
        # because that would immediately find the stale VIP still on the killed winner's eth1.
        # Keep the taint active throughout this polling window.
        local FOUND=false TOTAL=0
        local POLL_DEADLINE=$(($(now_ms) + 60000))
        while [ $(now_ms) -lt $POLL_DEADLINE ]; do
            for cand in $CANDIDATES; do
                local iface="${NODE_IFACE[$cand]}"
                if node_ssh "$cand" "ip -o addr show $iface 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                    TOTAL=$(( $(now_ms) - T_KILL ))
                    echo "  Re-convergence: ${TOTAL}ms (VIP on $(node_label $cand) [${NODE_SUBNET[$cand]}])"
                    if [ "${NODE_SUBNET[$cand]}" != "$SUBNET" ]; then
                        warn "  Candidate is in wrong subnet! (${NODE_SUBNET[$cand]} != $SUBNET)"
                    fi
                    RESULTS+=($TOTAL)
                    record_event "E3" "iteration_$i" "$TOTAL"
                    FOUND=true
                    break 2
                fi
            done
            sleep 0.2
        done

        # Remove taint AFTER polling so lbnodeagent can return to original winner
        kubectl taint node "$WINNER" e3-timing=true:NoSchedule- 2>/dev/null || true

        [ "$FOUND" = "false" ] && {
            warn "  TIMEOUT: No candidate got the VIP within 60s"
            record_event "E3" "iteration_$i" "TIMEOUT"
        }

        # Wait for lbnodeagent to restart on the original winner and re-stabilize
        sleep 20
    done

    echo ""
    echo "--- E3 Summary ---"
    [ ${#RESULTS[@]} -gt 0 ] && {
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Re-convergence time: $STATS ms"
        record_event "E3" "summary" "$STATS"
    } || echo "No successful measurements"

    kubectl delete svc $SVC -n $NAMESPACE --ignore-not-found >/dev/null
}

#---------------------------------------------------------------------
# Cleanup
#---------------------------------------------------------------------
cleanup() {
    info "Cleaning up timing test resources..."
    kubectl delete svc -n $NAMESPACE -l test-suite=timing --ignore-not-found 2>/dev/null || true
    kubectl delete svc timing-e1-stability timing-e3-reconverge \
        -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete pod curl-test -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup "$TIMING_SG" -n purelb-system --ignore-not-found 2>/dev/null || true
    for node in $NODES; do
        kubectl uncordon $node 2>/dev/null || true
    done
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 2>/dev/null || true
}
trap cleanup EXIT

#---------------------------------------------------------------------
# Main
#---------------------------------------------------------------------
main() {
    echo "=============================================="
    echo "PureLB Timing Behavior Test Suite"
    echo "=============================================="
    echo "Cluster: $NODE_COUNT nodes, $SUBNET_COUNT subnet(s)"
    echo "Timing log: $TIMING_LOG"
    echo ""

    echo "Subnet topology:"
    for s in $SUBNETS; do
        local_nodes="${SUBNET_NODES[$s]}"
        local_count=$(echo "$local_nodes" | wc -w)
        node_list=""
        for n in $local_nodes; do
            node_list="$node_list  $(node_label $n)"
        done
        echo "  $s ($local_count nodes):$node_list"
    done
    echo ""

    echo "test,event,value" > "$TIMING_LOG"

    kubectl get deployment nginx -n $NAMESPACE >/dev/null 2>&1 || {
        echo "ERROR: nginx deployment not found in namespace '$NAMESPACE'"
        exit 1
    }

    # Create dedicated timing-test ServiceGroup with non-overlapping IP ranges
    setup_timing_servicegroup

    local ITERATIONS=${1:-3}

    test_b1_vip_placement $ITERATIONS
    test_b3_vip_withdrawal $ITERATIONS
    test_d1_nftables_latency $ITERATIONS
    test_d3_end_to_end $ITERATIONS
    test_e1_vip_stability
    test_e3_election_reconvergence $ITERATIONS

    echo ""
    echo "=============================================="
    echo "Timing Test Suite Complete"
    echo "=============================================="
    echo "Results saved to: $TIMING_LOG"
    echo ""
    echo "Key findings:"
    grep "summary" "$TIMING_LOG" | while IFS=, read TEST EVENT VALUE; do
        echo "  $TEST: $VALUE"
    done
}

main "${1:-3}"
