#!/bin/bash
#
# Stress test for graceful failover to find race conditions
# Runs the failover test multiple times with varied timing parameters
#
# Features:
# - Multiple VIPs (creates election contention)
# - Force kill (--grace-period=0) to test hard crashes
# - Cascading failover (kill new winner immediately)
# - Election noise (random pod deletions)
# - Node tainting (prevents pod rescheduling, simulates true node failure)
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="test"
PURELB_NS="purelb-system"
ITERATIONS=${1:-10}
LOG_DIR="/tmp/failover-stress-$(date +%Y%m%d-%H%M%S)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

mkdir -p "$LOG_DIR"

echo "=========================================="
echo "Failover Stress Test (Enhanced)"
echo "=========================================="
echo "Iterations: $ITERATIONS"
echo "Log directory: $LOG_DIR"
echo ""
echo "Test modes:"
echo "  - Basic failover (graceful)"
echo "  - Force kill (--grace-period=0)"
echo "  - Cascading failover (kill new winner)"
echo "  - Election noise (random pod deletions)"
echo "  - Multiple VIPs (election contention)"
echo "  - Node tainting (prevents pod rescheduling)"
echo ""

# Counters
PASS=0
FAIL=0
TOTAL=0

# Track created services for cleanup
EXTRA_SERVICES=()

# Helper functions
ts() { date "+%H:%M:%S.%3N"; }

get_vip_holder() {
    local IP=$1
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        if ssh $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo $node
            return 0
        fi
    done
    echo "NONE"
}

get_pod_on_node() {
    local NODE=$1
    kubectl get pods -n $PURELB_NS -o wide 2>/dev/null | grep lbnodeagent | grep "$NODE" | awk '{print $1}'
}

capture_state() {
    local LABEL=$1
    local LOGFILE=$2

    echo "=== $LABEL ===" >> "$LOGFILE"
    echo "Timestamp: $(date)" >> "$LOGFILE"
    echo "" >> "$LOGFILE"

    echo "--- Leases ---" >> "$LOGFILE"
    kubectl get leases -n $PURELB_NS 2>/dev/null >> "$LOGFILE" || true
    echo "" >> "$LOGFILE"

    echo "--- Pods ---" >> "$LOGFILE"
    kubectl get pods -n $PURELB_NS -o wide 2>/dev/null >> "$LOGFILE" || true
    echo "" >> "$LOGFILE"

    echo "--- VIP Locations ---" >> "$LOGFILE"
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        echo -n "$node: " >> "$LOGFILE"
        ssh $node "ip -o addr show eth0 2>/dev/null | grep '172.30.255' | awk '{print \$4}'" 2>/dev/null >> "$LOGFILE" || echo "unreachable" >> "$LOGFILE"
    done
    echo "" >> "$LOGFILE"
}

create_election_noise() {
    local VIP_HOLDER=$1
    local LOGFILE=$2
    local NOISE_DURATION=$3

    echo "=== NOISE GENERATOR STARTED ===" >> "$LOGFILE"

    local END_TIME=$(($(date +%s) + NOISE_DURATION))

    while [ $(date +%s) -lt $END_TIME ]; do
        # Pick a random non-VIP-holder node
        local NODES=(purelb1 purelb2 purelb3 purelb4 purelb5)
        local CANDIDATE=""
        local CANDIDATE_POD=""

        # Shuffle and pick first non-holder with a running pod
        for node in $(echo "${NODES[@]}" | tr ' ' '\n' | shuf); do
            if [ "$node" != "$VIP_HOLDER" ]; then
                local pod=$(get_pod_on_node "$node")
                if [ -n "$pod" ]; then
                    CANDIDATE=$node
                    CANDIDATE_POD=$pod
                    break
                fi
            fi
        done

        if [ -n "$CANDIDATE" ]; then
            echo "  $(ts) [NOISE] Deleting pod on $CANDIDATE" >> "$LOGFILE"
            kubectl delete pod -n $PURELB_NS "$CANDIDATE_POD" --grace-period=3 >> "$LOGFILE" 2>&1 &

            # Random sleep 0.5-2 seconds (faster noise)
            sleep "0.$((5 + RANDOM % 15))"
        else
            sleep 0.5
        fi
    done

    echo "=== NOISE GENERATOR STOPPED ===" >> "$LOGFILE"
}

# Create multiple VIP services for election contention
setup_multiple_vips() {
    echo "Setting up multiple VIPs for election contention..."

    # Create 4 additional services (total 5 VIPs)
    for i in 2 3 4 5; do
        SVC_NAME="nginx-lb-stress-$i"
        EXTRA_SERVICES+=("$SVC_NAME")

        cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Service
metadata:
  name: $SVC_NAME
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies: [IPv4]
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF
    done

    # Wait for all VIPs to be allocated
    echo "Waiting for VIPs to be allocated..."
    for svc in "${EXTRA_SERVICES[@]}"; do
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/$svc -n $NAMESPACE --timeout=30s 2>/dev/null || true
    done

    sleep 3  # Let announcements settle

    echo "Multiple VIPs ready:"
    kubectl get svc -n $NAMESPACE -o wide 2>/dev/null | grep -E "nginx-lb" | while read line; do
        echo "  $line"
    done
}

cleanup_extra_vips() {
    echo "Cleaning up extra VIP services..."
    for svc in "${EXTRA_SERVICES[@]}"; do
        kubectl delete svc "$svc" -n $NAMESPACE 2>/dev/null || true
    done
}

run_single_test() {
    local ITERATION=$1
    local GRACE_PERIOD=$2
    local WAIT_BEFORE=$3
    local WITH_NOISE=$4
    local CASCADE=$5
    local WITH_TAINT=$6
    local LOGFILE="$LOG_DIR/iteration-${ITERATION}.log"

    # Build test description
    local DESC="grace=$GRACE_PERIOD"
    [ "$GRACE_PERIOD" = "0" ] && DESC="FORCE_KILL"
    [ "$WITH_NOISE" = "1" ] && DESC="$DESC +NOISE"
    [ "$CASCADE" = "1" ] && DESC="$DESC +CASCADE"
    [ "$WITH_TAINT" = "1" ] && DESC="$DESC +TAINT"

    echo ""
    echo -e "${BLUE}--- Iteration $ITERATION: $DESC ---${NC}"

    # Get current VIP and holder
    local IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
    if [ -z "$IPV4" ]; then
        echo -e "${RED}  ERROR: No VIP found${NC}"
        echo "ERROR: No VIP found" >> "$LOGFILE"
        return 1
    fi

    local ORIGINAL=$(get_vip_holder "$IPV4")
    if [ "$ORIGINAL" = "NONE" ]; then
        echo -e "${RED}  ERROR: VIP not on any node${NC}"
        capture_state "PRE-TEST (VIP MISSING)" "$LOGFILE"
        return 1
    fi

    echo "  VIP: $IPV4 on $ORIGINAL"
    capture_state "PRE-FAILOVER" "$LOGFILE"

    # Find pod to delete
    local POD=$(get_pod_on_node "$ORIGINAL")
    if [ -z "$POD" ]; then
        echo -e "${RED}  ERROR: No pod found on $ORIGINAL${NC}"
        return 1
    fi

    # Taint the node to prevent pod rescheduling (simulates true node failure)
    if [ "$WITH_TAINT" = "1" ]; then
        echo "  $(ts) Tainting $ORIGINAL to prevent rescheduling..."
        kubectl taint nodes "$ORIGINAL" stress-test=true:NoSchedule --overwrite >> "$LOGFILE" 2>&1
    fi

    # Start noise generator in background if enabled
    local NOISE_PID=""
    if [ "$WITH_NOISE" = "1" ]; then
        echo "  $(ts) Starting election noise generator..."
        create_election_noise "$ORIGINAL" "$LOGFILE" 20 &
        NOISE_PID=$!
    fi

    # Optional wait before deletion (simulates varied timing)
    if [ "$WAIT_BEFORE" -gt 0 ]; then
        sleep "$WAIT_BEFORE"
    fi

    # Capture pod logs during shutdown (only for graceful)
    local LOG_PID=""
    if [ "$GRACE_PERIOD" != "0" ]; then
        kubectl logs -n $PURELB_NS "$POD" -f --tail=0 >> "$LOGFILE" 2>&1 &
        LOG_PID=$!
    fi

    # Delete pod
    local START=$(date +%s.%N)
    if [ "$GRACE_PERIOD" = "0" ]; then
        echo -e "  $(ts) ${YELLOW}FORCE KILLING${NC} pod $POD (no graceful shutdown)..."
    else
        echo "  $(ts) Deleting VIP holder pod $POD (grace=$GRACE_PERIOD)..."
    fi
    kubectl delete pod -n $PURELB_NS "$POD" --grace-period=$GRACE_PERIOD >> "$LOGFILE" 2>&1 &
    local DELETE_PID=$!

    # Wait for VIP to move
    local TIMEOUT=30
    local ELAPSED=0
    local NEW_HOLDER=""
    local CHECK_INTERVAL=1

    while [ $ELAPSED -lt $TIMEOUT ]; do
        NEW_HOLDER=$(get_vip_holder "$IPV4")

        # Log current state
        echo "  $(ts) [$ELAPSED s] VIP holder: $NEW_HOLDER" >> "$LOGFILE"

        if [ "$NEW_HOLDER" != "NONE" ] && [ "$NEW_HOLDER" != "$ORIGINAL" ]; then
            break
        fi

        sleep $CHECK_INTERVAL
        ELAPSED=$((ELAPSED + CHECK_INTERVAL))
    done

    # Wait for delete to finish
    wait $DELETE_PID 2>/dev/null || true
    [ -n "$LOG_PID" ] && kill $LOG_PID 2>/dev/null || true

    local FIRST_MOVE_TIME=$ELAPSED
    local FIRST_NEW_HOLDER=$NEW_HOLDER

    # Cascading failover: kill the new winner too
    if [ "$CASCADE" = "1" ] && [ "$NEW_HOLDER" != "NONE" ] && [ "$NEW_HOLDER" != "$ORIGINAL" ]; then
        echo -e "  ${CYAN}CASCADE${NC}: Killing new winner $NEW_HOLDER immediately..."
        echo "=== CASCADE: Killing new winner $NEW_HOLDER ===" >> "$LOGFILE"

        local CASCADE_POD=$(get_pod_on_node "$NEW_HOLDER")
        if [ -n "$CASCADE_POD" ]; then
            # Use short grace period for cascade
            kubectl delete pod -n $PURELB_NS "$CASCADE_POD" --grace-period=3 >> "$LOGFILE" 2>&1 &
            local CASCADE_PID=$!

            # Wait for VIP to move again
            local CASCADE_TIMEOUT=20
            local CASCADE_ELAPSED=0
            local SECOND_HOLDER=""

            while [ $CASCADE_ELAPSED -lt $CASCADE_TIMEOUT ]; do
                SECOND_HOLDER=$(get_vip_holder "$IPV4")
                echo "  $(ts) [CASCADE $CASCADE_ELAPSED s] VIP holder: $SECOND_HOLDER" >> "$LOGFILE"

                if [ "$SECOND_HOLDER" != "NONE" ] && [ "$SECOND_HOLDER" != "$NEW_HOLDER" ] && [ "$SECOND_HOLDER" != "$ORIGINAL" ]; then
                    break
                fi

                sleep 1
                CASCADE_ELAPSED=$((CASCADE_ELAPSED + 1))
            done

            wait $CASCADE_PID 2>/dev/null || true

            if [ "$SECOND_HOLDER" != "NONE" ] && [ "$SECOND_HOLDER" != "$NEW_HOLDER" ]; then
                echo -e "  ${GREEN}CASCADE OK${NC}: VIP moved to $SECOND_HOLDER"
                NEW_HOLDER=$SECOND_HOLDER
            else
                echo -e "  ${RED}CASCADE FAIL${NC}: VIP stuck on $SECOND_HOLDER"
                NEW_HOLDER="NONE"
            fi
        fi
    fi

    # Stop noise generator
    [ -n "$NOISE_PID" ] && kill $NOISE_PID 2>/dev/null || true

    # Remove taint if applied
    if [ "$WITH_TAINT" = "1" ]; then
        echo "  $(ts) Removing taint from $ORIGINAL..."
        kubectl taint nodes "$ORIGINAL" stress-test=true:NoSchedule- >> "$LOGFILE" 2>&1 || true
    fi

    local END=$(date +%s.%N)
    local DURATION=$(echo "$END - $START" | bc)

    capture_state "POST-FAILOVER" "$LOGFILE"

    # Evaluate result
    # Success criteria depends on test mode:
    # - TAINTED: VIP MUST move to different node (original node can't run pods)
    # - Non-tainted: VIP can stay on same node (new pod takes over) OR move
    # - All modes: Service MUST be reachable

    local RESULT="UNKNOWN"
    local REACHABLE=false

    # For tainted tests, use NEW_HOLDER captured during wait loop (when taint was active)
    # For non-tainted tests, re-check current state (new pod may have taken over)
    local FINAL_HOLDER
    if [ "$WITH_TAINT" = "1" ]; then
        # Use the holder we observed while taint was active
        FINAL_HOLDER="$NEW_HOLDER"
    else
        # Re-check current state (new pod may have taken over)
        FINAL_HOLDER=$(get_vip_holder "$IPV4")
    fi

    if [ "$FINAL_HOLDER" = "NONE" ]; then
        RESULT="FAIL_NO_VIP"
        echo -e "  ${RED}FAIL${NC}: VIP not announced anywhere"
        echo "RESULT: FAIL - VIP not announced on any node" >> "$LOGFILE"
    elif [ "$WITH_TAINT" = "1" ] && [ "$FINAL_HOLDER" = "$ORIGINAL" ]; then
        # Tainted test: VIP must move to different node
        RESULT="FAIL_NO_MOVE"
        echo -e "  ${RED}FAIL${NC}: VIP stuck on tainted node $ORIGINAL"
        echo "RESULT: FAIL - VIP stuck on tainted node $ORIGINAL" >> "$LOGFILE"
    else
        # VIP is announced - check reachability
        sleep 1
        if curl -s --connect-timeout 3 "http://$IPV4/" | grep -q "nginx\|Pod:\|Welcome"; then
            REACHABLE=true
        fi

        if [ "$REACHABLE" = true ]; then
            if [ "$FINAL_HOLDER" = "$ORIGINAL" ]; then
                # Non-tainted: same node with new pod - valid
                RESULT="PASS_SAME_NODE"
                echo -e "  ${GREEN}PASS${NC}: New pod on $ORIGINAL took over VIP (${DURATION}s)"
                echo "RESULT: PASS - New pod on same node $ORIGINAL" >> "$LOGFILE"
            elif [ "$CASCADE" = "1" ]; then
                RESULT="PASS_CASCADE"
                echo -e "  ${GREEN}PASS${NC}: VIP survived cascade ($ORIGINAL -> $FIRST_NEW_HOLDER -> $FINAL_HOLDER)"
                echo "RESULT: PASS - VIP survived cascade to $FINAL_HOLDER" >> "$LOGFILE"
            else
                RESULT="PASS_MOVED"
                echo -e "  ${GREEN}PASS${NC}: VIP moved to $FINAL_HOLDER in ${FIRST_MOVE_TIME}s (total: ${DURATION}s)"
                echo "RESULT: PASS - VIP moved from $ORIGINAL to $FINAL_HOLDER" >> "$LOGFILE"
            fi
            echo -e "  ${GREEN}Service reachable${NC}"
        else
            RESULT="FAIL_UNREACHABLE"
            echo -e "  ${RED}FAIL${NC}: VIP on $FINAL_HOLDER but service not reachable"
            echo "RESULT: FAIL - Service not reachable at $IPV4" >> "$LOGFILE"
        fi
    fi

    # Return based on result
    case $RESULT in
        PASS_*)
            return 0
            ;;
        *)
            # Capture additional debug info on failure
            echo "" >> "$LOGFILE"
            echo "=== DEBUG: All node logs ===" >> "$LOGFILE"
            for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
                local npod=$(get_pod_on_node "$node")
                if [ -n "$npod" ]; then
                    echo "--- $node ($npod) ---" >> "$LOGFILE"
                    kubectl logs -n $PURELB_NS "$npod" --tail=50 2>/dev/null >> "$LOGFILE" || true
                fi
            done
            return 1
            ;;
    esac
}

wait_for_all_pods() {
    local MAX_WAIT=${1:-30}
    for j in $(seq 1 $MAX_WAIT); do
        POD_COUNT=$(kubectl get pods -n $PURELB_NS 2>/dev/null | grep -c "lbnodeagent.*Running" || echo 0)
        if [ "$POD_COUNT" -eq 5 ]; then
            return 0
        fi
        sleep 1
    done
    echo -e "  ${YELLOW}WARNING: Only $POD_COUNT/5 pods running${NC}"
    return 1
}

# Cleanup handler
cleanup() {
    echo ""
    echo "Cleaning up..."
    cleanup_extra_vips
    # Remove any lingering taints
    for node in purelb1 purelb2 purelb3 purelb4 purelb5; do
        kubectl taint nodes "$node" stress-test=true:NoSchedule- 2>/dev/null || true
    done
    # Kill any remaining background processes
    jobs -p | xargs -r kill 2>/dev/null || true
}
trap cleanup EXIT

# Ensure test service exists
echo "Ensuring primary test service exists..."
IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
if [ -z "$IPV4" ]; then
    echo "Creating test service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s
    sleep 5  # Wait for announcement
fi

# Setup multiple VIPs for election contention
setup_multiple_vips

echo ""
echo "Starting stress test..."
echo ""

# Run iterations with varied parameters
for i in $(seq 1 $ITERATIONS); do
    TOTAL=$((TOTAL + 1))

    # Vary parameters to catch race conditions:
    # - grace_period: 0, 5, 10, 15 (0 = force kill)
    # - wait_before: 0, 1, 2 seconds
    # - with_noise: 0 or 1
    # - cascade: 0 or 1 (kill new winner)
    # - with_taint: 0 or 1 (prevent pod rescheduling)

    # Mix of test modes
    case $((i % 8)) in
        0) GRACE=0  ; CASCADE=0 ; TAINT=0 ;;  # Force kill
        1) GRACE=10 ; CASCADE=0 ; TAINT=0 ;;  # Normal graceful
        2) GRACE=5  ; CASCADE=1 ; TAINT=0 ;;  # Cascade with short grace
        3) GRACE=15 ; CASCADE=0 ; TAINT=1 ;;  # Longer grace + taint
        4) GRACE=0  ; CASCADE=1 ; TAINT=0 ;;  # Force kill + cascade
        5) GRACE=10 ; CASCADE=1 ; TAINT=1 ;;  # Normal + cascade + taint
        6) GRACE=0  ; CASCADE=0 ; TAINT=1 ;;  # Force kill + taint (true node failure)
        7) GRACE=10 ; CASCADE=0 ; TAINT=1 ;;  # Normal + taint
    esac

    WAIT_BEFORE=$((i % 3))

    # Enable noise on ~50% of iterations (but not with taint to keep it cleaner)
    WITH_NOISE=$(( TAINT == 0 && (i + 1) % 2 == 0 ? 1 : 0 ))

    if run_single_test $i $GRACE $WAIT_BEFORE $WITH_NOISE $CASCADE $TAINT; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))

        # On failure, wait longer for cluster to stabilize
        echo "  Waiting 15s for cluster to stabilize..."
        sleep 15
    fi

    # Wait for replacement pods
    echo "  Waiting for replacement pods..."
    if [ "$CASCADE" = "1" ] || [ "$WITH_NOISE" = "1" ]; then
        sleep 5
        wait_for_all_pods 45
        sleep 3
    else
        sleep 3
        wait_for_all_pods 25
    fi
done

# Summary
echo ""
echo "=========================================="
echo "STRESS TEST RESULTS"
echo "=========================================="
echo "Total iterations: $TOTAL"
echo -e "Passed: ${GREEN}$PASS${NC}"
echo -e "Failed: ${RED}$FAIL${NC}"
echo "Success rate: $(echo "scale=1; $PASS * 100 / $TOTAL" | bc)%"
echo ""
echo "Logs saved to: $LOG_DIR"

if [ $FAIL -gt 0 ]; then
    echo ""
    echo "Failed iterations:"
    grep -l "RESULT: FAIL" "$LOG_DIR"/*.log 2>/dev/null | while read f; do
        echo "  - $(basename $f)"
    done
    exit 1
fi

exit 0
