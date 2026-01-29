#!/bin/bash
set -e

# PureLB Timing Behavior Test Suite
#
# This test suite characterizes timing behavior in PureLB's lbnodeagent,
# specifically for ETP Local (externalTrafficPolicy: Local) scenarios.
#
# Purpose:
# - Document and verify timing guarantees PureLB provides
# - Characterize delays at each stage of the service lifecycle
# - Establish baselines for expected timing behavior
# - Provide evidence for test timeout adjustments
#
# Unlike pass/fail E2E tests, this suite outputs timing measurements
# for analysis. Run multiple iterations to establish baselines.

CONTEXT="proxmox"
NAMESPACE="test"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Output file for timing data
TIMING_LOG="${SCRIPT_DIR}/timing-results-$(date +%Y%m%d-%H%M%S).csv"

pass() { echo -e "${GREEN}✓${NC} $1"; }
warn() { echo -e "${YELLOW}!${NC} $1" >&2; }
info() { echo -e "${CYAN}→${NC} $1" >&2; }
fail() { echo -e "${RED}✗${NC} $1"; }

kubectl() { command kubectl --context "$CONTEXT" "$@"; }

# Get node list dynamically
NODES=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
NODE_COUNT=$(echo $NODES | wc -w)

#---------------------------------------------------------------------
# Timing Helper Functions
#---------------------------------------------------------------------

# Timestamp with milliseconds
now_ms() {
    date +%s%3N
}

# Record timing event to log
record_event() {
    local TEST=$1
    local EVENT=$2
    local VALUE=$3
    echo "$TEST,$EVENT,$VALUE" >> "$TIMING_LOG"
}

# SSH helper with error handling
ssh_cmd() {
    local NODE=$1
    shift
    ssh "$NODE" "$@" 2>/dev/null
}

# Poll for VIP on node with timing measurement
# Returns: milliseconds elapsed, or -1 on timeout
poll_for_vip() {
    local NODE=$1
    local IP=$2
    local IFACE=$3
    local TIMEOUT=${4:-60000}  # milliseconds
    local START=$(now_ms)

    while true; do
        local NOW=$(now_ms)
        local ELAPSED=$((NOW - START))

        if ssh_cmd $NODE "ip -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'"; then
            echo "$ELAPSED"
            return 0
        fi

        if [ $ELAPSED -gt $TIMEOUT ]; then
            echo "-1"
            return 1
        fi

        sleep 0.1
    done
}

# Poll for VIP removal from node
# Returns: milliseconds elapsed, or -1 on timeout
poll_for_vip_removal() {
    local NODE=$1
    local IP=$2
    local IFACE=$3
    local TIMEOUT=${4:-60000}
    local START=$(now_ms)

    while true; do
        local NOW=$(now_ms)
        local ELAPSED=$((NOW - START))

        if ! ssh_cmd $NODE "ip -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'"; then
            echo "$ELAPSED"
            return 0
        fi

        if [ $ELAPSED -gt $TIMEOUT ]; then
            echo "-1"
            return 1
        fi

        sleep 0.1
    done
}

# Poll nftables rules with timing
poll_for_nftables() {
    local NODE=$1
    local IP=$2
    local PORT=$3
    local TIMEOUT=${4:-30000}
    local START=$(now_ms)

    while true; do
        local NOW=$(now_ms)
        local ELAPSED=$((NOW - START))

        if ssh_cmd $NODE "sudo nft list map ip kube-proxy service-ips 2>/dev/null | grep -q '$IP.*$PORT'"; then
            echo "$ELAPSED"
            return 0
        fi

        if [ $ELAPSED -gt $TIMEOUT ]; then
            echo "-1"
            return 1
        fi

        sleep 0.1
    done
}

# Poll for traffic connectivity
poll_for_traffic() {
    local IP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-30000}
    local START=$(now_ms)

    while true; do
        local NOW=$(now_ms)
        local ELAPSED=$((NOW - START))

        if kubectl exec -n $NAMESPACE curl-test -- curl -s --connect-timeout 2 "http://$IP:$PORT" >/dev/null 2>&1; then
            echo "$ELAPSED"
            return 0
        fi

        if [ $ELAPSED -gt $TIMEOUT ]; then
            echo "-1"
            return 1
        fi

        sleep 0.2
    done
}

# Wait for service IP allocation
wait_for_ip_allocation() {
    local SVC=$1
    local TIMEOUT=${2:-60}

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/$SVC -n $NAMESPACE --timeout=${TIMEOUT}s >/dev/null 2>&1 || return 1

    kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
}

# Ensure curl test pod exists
ensure_curl_pod() {
    if kubectl get pod curl-test -n $NAMESPACE -o jsonpath='{.status.phase}' 2>/dev/null | grep -q Running; then
        return 0
    fi

    info "Creating curl test pod..."
    kubectl delete pod curl-test -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl run curl-test -n $NAMESPACE --image=curlimages/curl:latest \
        --restart=Never --command -- sleep 3600 >/dev/null 2>&1
    kubectl wait --for=condition=Ready pod/curl-test -n $NAMESPACE --timeout=60s >/dev/null 2>&1
}

#---------------------------------------------------------------------
# Statistics Functions
#---------------------------------------------------------------------

# Calculate statistics from array of values
calc_stats() {
    local -a VALUES=("$@")
    local COUNT=${#VALUES[@]}

    if [ $COUNT -eq 0 ]; then
        echo "count=0,min=N/A,max=N/A,avg=N/A,p95=N/A"
        return
    fi

    # Sort values
    local SORTED=($(printf '%s\n' "${VALUES[@]}" | sort -n))

    local MIN=${SORTED[0]}
    local MAX=${SORTED[$((COUNT-1))]}

    # Calculate average
    local SUM=0
    for v in "${VALUES[@]}"; do
        SUM=$((SUM + v))
    done
    local AVG=$((SUM / COUNT))

    # Calculate P95 index
    local P95_IDX=$(( (COUNT * 95) / 100 ))
    [ $P95_IDX -ge $COUNT ] && P95_IDX=$((COUNT - 1))
    local P95=${SORTED[$P95_IDX]}

    echo "count=$COUNT,min=$MIN,max=$MAX,avg=$AVG,p95=$P95"
}

#---------------------------------------------------------------------
# Test B1: EndpointSlice Cache Sync Latency
#---------------------------------------------------------------------
test_b1_cache_sync_latency() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST B1: EndpointSlice Cache Sync Latency"
    echo "=========================================="
    echo "Measures time from pod Ready to VIP placement on endpoint node"
    echo ""

    local -a RESULTS=()

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        # Scale to 0
        kubectl scale deployment nginx -n $NAMESPACE --replicas=0 >/dev/null
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        sleep 3

        # Create a fresh ETP Local service for this test
        local SVC_NAME="timing-b1-$i"
        cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: $SVC_NAME
  namespace: $NAMESPACE
  labels:
    test-suite: timing
  annotations:
    purelb.io/service-group: remote
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
EOF

        # Wait for IP allocation
        local IP=$(wait_for_ip_allocation $SVC_NAME 30)
        if [ -z "$IP" ]; then
            warn "  Failed to get IP for $SVC_NAME"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        info "  Allocated IP: $IP"

        # Scale to 1 and measure time to VIP placement
        local T_SCALE=$(now_ms)
        kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null

        # Wait for pod ready
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        local T_POD_READY=$(now_ms)

        # Get endpoint node
        local ENDPOINT_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].spec.nodeName}')
        info "  Endpoint node: $ENDPOINT_NODE"

        # Poll for VIP
        local VIP_DELAY=$(poll_for_vip $ENDPOINT_NODE $IP kube-lb0 30000)
        local T_VIP=$(now_ms)

        local POD_TO_VIP=$((T_VIP - T_POD_READY))
        local SCALE_TO_VIP=$((T_VIP - T_SCALE))

        if [ "$VIP_DELAY" = "-1" ]; then
            warn "  TIMEOUT: VIP not placed within 30s"
            record_event "B1" "iteration_$i" "TIMEOUT"
        else
            echo "  Pod ready to VIP: ${POD_TO_VIP}ms (poll detected: ${VIP_DELAY}ms)"
            echo "  Scale to VIP: ${SCALE_TO_VIP}ms"
            RESULTS+=($POD_TO_VIP)
            record_event "B1" "iteration_$i" "$POD_TO_VIP"
        fi

        # Cleanup
        kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
        sleep 2
    done

    echo ""
    echo "--- B1 Summary ---"
    if [ ${#RESULTS[@]} -gt 0 ]; then
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Pod Ready → VIP Placed: $STATS ms"
        record_event "B1" "summary" "$STATS"
    else
        echo "No successful measurements"
    fi
}

#---------------------------------------------------------------------
# Test B3: Endpoint Termination (VIP Withdrawal) Timing
#---------------------------------------------------------------------
test_b3_endpoint_termination() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST B3: Endpoint Termination Timing"
    echo "=========================================="
    echo "Measures time from pod deletion to VIP withdrawal"
    echo ""

    local -a RESULTS=()

    # Ensure we have 1 replica running
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
    sleep 3

    # Get or create ETP Local service
    local IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
    if [ -z "$IP" ]; then
        warn "Creating ETP Local service..."
        cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local
  namespace: $NAMESPACE
  labels:
    test-suite: timing
  annotations:
    purelb.io/service-group: remote
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
EOF
        sleep 5
        IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    fi
    info "Using ETP Local service IP: $IP"

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        # Ensure 1 replica
        kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        sleep 3

        # Get current endpoint node
        local ENDPOINT_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].spec.nodeName}')
        info "  Endpoint node: $ENDPOINT_NODE"

        # Verify VIP is on that node
        if ! ssh_cmd $ENDPOINT_NODE "ip -o addr show kube-lb0 | grep -q ' $IP/'"; then
            warn "  VIP not on endpoint node, waiting..."
            poll_for_vip $ENDPOINT_NODE $IP kube-lb0 30000 >/dev/null
        fi

        # Delete pod and measure withdrawal time
        local T_DELETE=$(now_ms)
        kubectl delete pod -n $NAMESPACE -l app=nginx --grace-period=1 >/dev/null

        # Poll for VIP removal
        local REMOVAL_DELAY=$(poll_for_vip_removal $ENDPOINT_NODE $IP kube-lb0 60000)
        local T_REMOVED=$(now_ms)

        local TOTAL_TIME=$((T_REMOVED - T_DELETE))

        if [ "$REMOVAL_DELAY" = "-1" ]; then
            warn "  TIMEOUT: VIP not removed within 60s"
            record_event "B3" "iteration_$i" "TIMEOUT"
        else
            echo "  Pod delete to VIP removal: ${TOTAL_TIME}ms (poll detected: ${REMOVAL_DELAY}ms)"
            RESULTS+=($TOTAL_TIME)
            record_event "B3" "iteration_$i" "$TOTAL_TIME"
        fi

        sleep 2
    done

    echo ""
    echo "--- B3 Summary ---"
    if [ ${#RESULTS[@]} -gt 0 ]; then
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Pod Delete → VIP Removed: $STATS ms"
        record_event "B3" "summary" "$STATS"
    else
        echo "No successful measurements"
    fi

    # Restore 1 replica
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
}

#---------------------------------------------------------------------
# Test D1: nftables Rule Programming Latency
#---------------------------------------------------------------------
test_d1_nftables_latency() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST D1: nftables Rule Programming Latency"
    echo "=========================================="
    echo "Measures time from VIP placement to nftables rules ready"
    echo ""

    local -a RESULTS=()
    local FIRST_NODE=${NODES%% *}

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        # Create a new service
        local SVC_NAME="timing-d1-$i"
        cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: $SVC_NAME
  namespace: $NAMESPACE
  labels:
    test-suite: timing
  annotations:
    purelb.io/service-group: remote
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
EOF

        local T_CREATE=$(now_ms)

        # Wait for IP
        local IP=$(wait_for_ip_allocation $SVC_NAME 30)
        if [ -z "$IP" ]; then
            warn "  Failed to get IP"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        local T_ALLOCATED=$(now_ms)
        info "  IP: $IP"

        # Wait for VIP on any node
        local VIP_DELAY=$(poll_for_vip $FIRST_NODE $IP kube-lb0 30000)
        local T_VIP=$(now_ms)

        # Wait for nftables rules
        local NFT_DELAY=$(poll_for_nftables $FIRST_NODE $IP 80 30000)
        local T_NFT=$(now_ms)

        local VIP_TO_NFT=$((T_NFT - T_VIP))
        local CREATE_TO_NFT=$((T_NFT - T_CREATE))

        if [ "$NFT_DELAY" = "-1" ]; then
            warn "  TIMEOUT: nftables rules not ready within 30s"
            record_event "D1" "iteration_$i" "TIMEOUT"
        else
            echo "  VIP placed to nftables ready: ${VIP_TO_NFT}ms"
            echo "  Service create to nftables ready: ${CREATE_TO_NFT}ms"
            RESULTS+=($VIP_TO_NFT)
            record_event "D1" "iteration_$i" "$VIP_TO_NFT"
        fi

        # Cleanup
        kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
        sleep 2
    done

    echo ""
    echo "--- D1 Summary ---"
    if [ ${#RESULTS[@]} -gt 0 ]; then
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "VIP → nftables ready: $STATS ms"
        record_event "D1" "summary" "$STATS"
    else
        echo "No successful measurements"
    fi
}

#---------------------------------------------------------------------
# Test D3: End-to-End Traffic Latency
#---------------------------------------------------------------------
test_d3_end_to_end() {
    local ITERATIONS=${1:-5}
    echo ""
    echo "=========================================="
    echo "TEST D3: End-to-End Traffic Latency"
    echo "=========================================="
    echo "Measures time from service creation to first successful curl"
    echo ""

    ensure_curl_pod

    # Ensure nginx is running
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1

    local -a RESULTS=()

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        local SVC_NAME="timing-d3-$i"
        local T_CREATE=$(now_ms)

        cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: $SVC_NAME
  namespace: $NAMESPACE
  labels:
    test-suite: timing
  annotations:
    purelb.io/service-group: remote
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
EOF

        # Wait for IP
        local IP=$(wait_for_ip_allocation $SVC_NAME 30)
        if [ -z "$IP" ]; then
            warn "  Failed to get IP"
            kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
            continue
        fi
        local T_ALLOCATED=$(now_ms)
        info "  IP: $IP (allocated in $((T_ALLOCATED - T_CREATE))ms)"

        # Wait for traffic
        local TRAFFIC_DELAY=$(poll_for_traffic $IP 80 60000)
        local T_TRAFFIC=$(now_ms)

        local TOTAL=$((T_TRAFFIC - T_CREATE))

        if [ "$TRAFFIC_DELAY" = "-1" ]; then
            warn "  TIMEOUT: Traffic not working within 60s"
            record_event "D3" "iteration_$i" "TIMEOUT"
        else
            echo "  Service create to traffic OK: ${TOTAL}ms"
            RESULTS+=($TOTAL)
            record_event "D3" "iteration_$i" "$TOTAL"
        fi

        # Cleanup
        kubectl delete svc $SVC_NAME -n $NAMESPACE --ignore-not-found >/dev/null
        sleep 2
    done

    echo ""
    echo "--- D3 Summary ---"
    if [ ${#RESULTS[@]} -gt 0 ]; then
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Service Create → Traffic OK: $STATS ms"
        record_event "D3" "summary" "$STATS"
    else
        echo "No successful measurements"
    fi
}

#---------------------------------------------------------------------
# Test E1: ETP Local Rapid Scaling
#---------------------------------------------------------------------
test_e1_rapid_scaling() {
    echo ""
    echo "=========================================="
    echo "TEST E1: ETP Local Rapid Scaling"
    echo "=========================================="
    echo "Verifies VIP placement correctness under rapid scaling: 0→2→0→1→3→1→0"
    echo ""

    # Get or create ETP Local service
    local IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
    if [ -z "$IP" ]; then
        cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local
  namespace: $NAMESPACE
  labels:
    test-suite: timing
  annotations:
    purelb.io/service-group: remote
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
EOF
        sleep 5
        IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    fi
    info "Using ETP Local service IP: $IP"

    local SCALE_SEQUENCE=(0 2 0 1 3 1 0)
    local ERRORS=0

    for REPLICAS in "${SCALE_SEQUENCE[@]}"; do
        info "Scaling to $REPLICAS replicas..."
        local T_START=$(now_ms)

        kubectl scale deployment nginx -n $NAMESPACE --replicas=$REPLICAS >/dev/null
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1

        if [ $REPLICAS -gt 0 ]; then
            kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        fi

        # Wait for VIP placement to stabilize
        sleep 5

        # Get endpoint nodes
        local ENDPOINT_NODES=""
        if [ $REPLICAS -gt 0 ]; then
            ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].spec.nodeName}')
        fi

        # Count nodes with VIP
        local VIP_COUNT=0
        local VIP_NODES=""
        for node in $NODES; do
            if ssh_cmd $node "ip -o addr show kube-lb0 | grep -q ' $IP/'"; then
                VIP_COUNT=$((VIP_COUNT + 1))
                VIP_NODES="$VIP_NODES $node"
            fi
        done

        local T_END=$(now_ms)
        local DURATION=$((T_END - T_START))

        # Verify correctness
        if [ $REPLICAS -eq 0 ]; then
            if [ $VIP_COUNT -eq 0 ]; then
                pass "Replicas=0: No nodes have VIP (correct) - ${DURATION}ms"
            else
                fail "Replicas=0: $VIP_COUNT nodes have VIP (should be 0)"
                ERRORS=$((ERRORS + 1))
            fi
        else
            # For ETP Local, VIP should be on endpoint nodes only
            local ENDPOINT_COUNT=$(echo $ENDPOINT_NODES | wc -w)
            if [ $VIP_COUNT -eq $ENDPOINT_COUNT ]; then
                pass "Replicas=$REPLICAS: $VIP_COUNT nodes have VIP (matches $ENDPOINT_COUNT endpoints) - ${DURATION}ms"
            else
                fail "Replicas=$REPLICAS: $VIP_COUNT nodes have VIP but $ENDPOINT_COUNT endpoints"
                echo "    VIP nodes: $VIP_NODES"
                echo "    Endpoint nodes: $ENDPOINT_NODES"
                ERRORS=$((ERRORS + 1))
            fi
        fi

        record_event "E1" "scale_to_$REPLICAS" "$DURATION"
    done

    echo ""
    echo "--- E1 Summary ---"
    if [ $ERRORS -eq 0 ]; then
        pass "All scaling transitions correct"
    else
        fail "$ERRORS errors during scaling transitions"
    fi

    # Restore 1 replica
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
}

#---------------------------------------------------------------------
# Test E3: Endpoint Migration
#---------------------------------------------------------------------
test_e3_endpoint_migration() {
    local ITERATIONS=${1:-3}
    echo ""
    echo "=========================================="
    echo "TEST E3: Endpoint Migration"
    echo "=========================================="
    echo "Measures VIP migration timing when pod moves to different node"
    echo ""

    local -a RESULTS=()

    # Get or create ETP Local service
    local IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
    if [ -z "$IP" ]; then
        warn "Creating ETP Local service..."
        cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local
  namespace: $NAMESPACE
  labels:
    test-suite: timing
  annotations:
    purelb.io/service-group: remote
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  selector:
    app: nginx
  ports:
  - port: 80
EOF
        sleep 5
        IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    fi
    info "Using ETP Local service IP: $IP"

    for i in $(seq 1 $ITERATIONS); do
        info "Iteration $i/$ITERATIONS"

        # Ensure 1 replica
        kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        sleep 3

        # Get current node
        local NODE_A=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].spec.nodeName}')
        info "  Initial node: $NODE_A"

        # Verify VIP on NODE_A
        poll_for_vip $NODE_A $IP kube-lb0 30000 >/dev/null

        # Cordon and delete to force migration
        local T_START=$(now_ms)
        kubectl cordon $NODE_A >/dev/null
        kubectl delete pod -n $NAMESPACE -l app=nginx --grace-period=1 >/dev/null

        # Wait for new pod
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1
        kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s >/dev/null 2>&1

        local NODE_B=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].spec.nodeName}')
        info "  New node: $NODE_B"

        # Poll for VIP on new node
        local VIP_NEW_DELAY=$(poll_for_vip $NODE_B $IP kube-lb0 30000)

        # Poll for VIP removal from old node
        local VIP_OLD_DELAY=$(poll_for_vip_removal $NODE_A $IP kube-lb0 30000)

        local T_END=$(now_ms)
        local TOTAL=$((T_END - T_START))

        # Uncordon
        kubectl uncordon $NODE_A >/dev/null

        # Check for duplicate VIPs during migration
        local DUPLICATE=false
        if [ "$VIP_OLD_DELAY" != "-1" ] && [ "$VIP_NEW_DELAY" != "-1" ]; then
            if [ $VIP_NEW_DELAY -lt $VIP_OLD_DELAY ]; then
                warn "  VIP appeared on new node before removed from old (potential brief duplicate)"
                DUPLICATE=true
            fi
        fi

        if [ "$NODE_A" = "$NODE_B" ]; then
            warn "  Pod did not migrate (same node)"
        elif [ "$VIP_NEW_DELAY" = "-1" ]; then
            fail "  TIMEOUT: VIP did not appear on new node"
        elif [ "$VIP_OLD_DELAY" = "-1" ]; then
            fail "  TIMEOUT: VIP not removed from old node"
        else
            echo "  Migration time: ${TOTAL}ms (VIP on new: ${VIP_NEW_DELAY}ms, removed from old: ${VIP_OLD_DELAY}ms)"
            RESULTS+=($TOTAL)
            record_event "E3" "iteration_$i" "$TOTAL"
        fi

        sleep 2
    done

    echo ""
    echo "--- E3 Summary ---"
    if [ ${#RESULTS[@]} -gt 0 ]; then
        local STATS=$(calc_stats "${RESULTS[@]}")
        echo "Migration time: $STATS ms"
        record_event "E3" "summary" "$STATS"
    else
        echo "No successful measurements"
    fi

    # Restore
    for node in $NODES; do
        kubectl uncordon $node 2>/dev/null || true
    done
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 >/dev/null
}

#---------------------------------------------------------------------
# Cleanup
#---------------------------------------------------------------------
cleanup() {
    info "Cleaning up timing test resources..."
    kubectl delete svc -n $NAMESPACE -l test-suite=timing --ignore-not-found 2>/dev/null || true
    for node in $NODES; do
        kubectl uncordon $node 2>/dev/null || true
    done
}

#---------------------------------------------------------------------
# Main
#---------------------------------------------------------------------
main() {
    echo "=============================================="
    echo "PureLB Timing Behavior Test Suite"
    echo "=============================================="
    echo "Cluster: $NODE_COUNT nodes ($NODES)"
    echo "Timing log: $TIMING_LOG"
    echo ""

    # Initialize timing log
    echo "test,event,value" > "$TIMING_LOG"

    # Ensure prerequisites
    kubectl get deployment nginx -n $NAMESPACE >/dev/null 2>&1 || {
        echo "ERROR: nginx deployment not found in $NAMESPACE"
        exit 1
    }

    # Run tests
    local ITERATIONS=${1:-3}

    test_b1_cache_sync_latency $ITERATIONS
    test_b3_endpoint_termination $ITERATIONS
    test_d1_nftables_latency $ITERATIONS
    test_d3_end_to_end $ITERATIONS
    test_e1_rapid_scaling
    test_e3_endpoint_migration $ITERATIONS

    cleanup

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

# Run with optional iteration count
main "${1:-3}"
