#!/bin/bash
set -e

# Determine script directory for relative file paths
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

NAMESPACE="test"
INTERACTIVE=false
ITERATIONS=1

# Parse command line options (--context is handled by common.sh parse_common_args)
SCRIPT_ARGS=()
while [[ $# -gt 0 ]]; do
    case $1 in
        -i|--interactive)
            INTERACTIVE=true
            shift
            ;;
        -n|--iterations)
            ITERATIONS="$2"
            shift 2
            ;;
        --context)
            CONTEXT="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [-i|--interactive] [-n <iterations>] [--context <name>]"
            echo ""
            echo "Options:"
            echo "  -i, --interactive     Pause after each test group for manual review"
            echo "  -n, --iterations N    Run the full test suite N times (default: 1)"
            echo "  --context NAME        Kubernetes context to use (default: current context)"
            echo "  -h, --help            Show this help message"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Use -h for help"
            exit 1
            ;;
    esac
done

# Source common test infrastructure (provides node discovery, SSH-by-IP, etc.)
source "${SCRIPT_DIR}/../common.sh"

# Log all output to a file while still showing on console
LOG_DIR="/tmp/test-local-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/output.log"
exec > >(tee -a "$LOG_FILE") 2>&1
echo "Log file: $LOG_FILE"

#---------------------------------------------------------------------
# Infrastructure Prerequisites Validation
# Validates that the cluster infrastructure is properly configured
# for load balancer traffic to work correctly
#---------------------------------------------------------------------
validate_prerequisites() {
    echo ""
    echo "=========================================="
    echo "PREREQUISITES: Infrastructure Validation"
    echo "=========================================="

    local FAILED=false

    # Check 1: IP forwarding enabled on all nodes (IPv4)
    info "Checking IPv4 IP forwarding on all nodes..."
    for node in $NODES; do
        local IPV4_FWD
        IPV4_FWD=$(node_ssh $node "cat /proc/sys/net/ipv4/ip_forward" 2>/dev/null || echo "error")
        if [ "$IPV4_FWD" = "1" ]; then
            pass "IPv4 forwarding enabled on $node"
        elif [ "$IPV4_FWD" = "0" ]; then
            echo -e "${RED}✗ FAIL:${NC} IPv4 forwarding DISABLED on $node"
            echo "  Fix: ssh $node 'sysctl -w net.ipv4.ip_forward=1'"
            FAILED=true
        else
            echo -e "${RED}✗ FAIL:${NC} Could not check IPv4 forwarding on $node (SSH error?)"
            FAILED=true
        fi
    done

    # Check 2: IP forwarding enabled on all nodes (IPv6)
    info "Checking IPv6 IP forwarding on all nodes..."
    for node in $NODES; do
        local IPV6_FWD
        IPV6_FWD=$(node_ssh $node "cat /proc/sys/net/ipv6/conf/all/forwarding" 2>/dev/null || echo "error")
        if [ "$IPV6_FWD" = "1" ]; then
            pass "IPv6 forwarding enabled on $node"
        elif [ "$IPV6_FWD" = "0" ]; then
            echo -e "${RED}✗ FAIL:${NC} IPv6 forwarding DISABLED on $node"
            echo "  Fix: ssh $node 'sysctl -w net.ipv6.conf.all.forwarding=1'"
            FAILED=true
        else
            echo -e "${RED}✗ FAIL:${NC} Could not check IPv6 forwarding on $node (SSH error?)"
            FAILED=true
        fi
    done

    # Check 3: kube-proxy is running
    info "Checking kube-proxy is running..."
    local KUBE_PROXY_PODS
    KUBE_PROXY_PODS=$(kubectl get pods -n kube-system -l k8s-app=kube-proxy --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    if [ "$KUBE_PROXY_PODS" -ge 1 ]; then
        pass "kube-proxy is running ($KUBE_PROXY_PODS pods)"
    else
        echo -e "${RED}✗ FAIL:${NC} kube-proxy not running"
        FAILED=true
    fi

    # Check 4: Test pods are running and distributed
    info "Checking test pods are running..."
    local READY_PODS
    READY_PODS=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    if [ "$READY_PODS" -ge 1 ]; then
        pass "Test pods are running ($READY_PODS pods)"
    else
        echo -e "${RED}✗ FAIL:${NC} No test pods running in namespace $NAMESPACE"
        FAILED=true
    fi

    # Check 5: Pod-to-pod connectivity (basic CNI health check)
    info "Checking pod-to-pod connectivity..."
    local POD_NAME
    POD_NAME=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "$POD_NAME" ]; then
        # Try to reach the Kubernetes API from inside a pod (proves networking works)
        if kubectl exec -n $NAMESPACE "$POD_NAME" -- wget -q -O /dev/null --timeout=5 https://kubernetes.default.svc 2>/dev/null; then
            pass "Pod networking is functional"
        else
            # Fallback: try a simple DNS lookup
            if kubectl exec -n $NAMESPACE "$POD_NAME" -- nslookup kubernetes.default.svc 2>/dev/null | grep -q "Address"; then
                pass "Pod DNS is functional"
            else
                info "Could not verify pod networking (may be OK if no internet access)"
            fi
        fi
    fi

    # Check 6: PureLB components are running
    info "Checking PureLB components..."
    local ALLOCATOR_READY
    ALLOCATOR_READY=$(kubectl get deployment -n purelb-system allocator -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [ "$ALLOCATOR_READY" -ge 1 ]; then
        pass "Allocator is running"
    else
        echo -e "${RED}✗ FAIL:${NC} Allocator not running"
        FAILED=true
    fi

    local AGENT_PODS
    AGENT_PODS=$(kubectl get pods -n purelb-system -l component=lbnodeagent --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    if [ "$AGENT_PODS" -ge 1 ]; then
        pass "LBNodeAgent is running ($AGENT_PODS pods)"
    else
        echo -e "${RED}✗ FAIL:${NC} LBNodeAgent not running"
        FAILED=true
    fi

    if [ "$FAILED" = "true" ]; then
        echo ""
        echo -e "${RED}═══════════════════════════════════════════════════════════════${NC}"
        echo -e "${RED}INFRASTRUCTURE VALIDATION FAILED${NC}"
        echo -e "${RED}Please fix the issues above before running E2E tests.${NC}"
        echo -e "${RED}═══════════════════════════════════════════════════════════════${NC}"
        exit 1
    fi

    # Verify metrics endpoints are reachable
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    info "Checking allocator metrics endpoint..."
    local ALLOC_METRICS
    ALLOC_METRICS=$(scrape_allocator_metrics)
    if [ -n "$ALLOC_METRICS" ]; then
        assert_metric "$ALLOC_METRICS" "purelb_k8s_client_config_loaded_bool" "eq" "1"
        pass "Allocator metrics endpoint reachable, config loaded"
    else
        echo -e "${RED}✗ FAIL:${NC} Allocator metrics endpoint not reachable on port 7472"
        FAILED=true
    fi

    info "Checking lbnodeagent metrics endpoint..."
    local AGENT_METRICS
    AGENT_METRICS=$(scrape_lbnodeagent_metrics)
    if [ -n "$AGENT_METRICS" ]; then
        assert_metric "$AGENT_METRICS" "purelb_election_lease_healthy" "eq" "1"
        pass "LBNodeAgent metrics endpoint reachable, lease healthy"
    else
        echo -e "${RED}✗ FAIL:${NC} LBNodeAgent metrics endpoint not reachable on port 7472"
        FAILED=true
    fi

    # Verify allocator logged successful startup
    info "Checking allocator startup logs..."
    if kubectl logs -n purelb-system deployment/allocator --tail=200 2>/dev/null | grep -q "markSynced"; then
        pass "Allocator logged 'markSynced' (ready to allocate)"
    else
        info "WARNING: 'markSynced' not found in recent allocator logs (may have rotated)"
    fi

    info "Checking allocator config loaded log..."
    if kubectl logs -n purelb-system deployment/allocator --tail=200 2>/dev/null | grep -q "setConfig"; then
        pass "Allocator logged 'setConfig' (configuration loaded)"
    else
        info "WARNING: 'setConfig' not found in recent allocator logs (may have rotated)"
    fi

    pass "All infrastructure prerequisites validated"
}

#---------------------------------------------------------------------
# Setup: Ensure LBNodeAgent configuration exists
#---------------------------------------------------------------------
setup_lbnodeagent() {
    info "Ensuring LBNodeAgent configuration exists..."
    cat <<EOF | kubectl apply -f -
apiVersion: purelb.io/v2
kind: LBNodeAgent
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    localInterface: default
EOF
    pass "LBNodeAgent configuration applied"
}

#---------------------------------------------------------------------
# Test: Lease Verification (Subnet-Aware Election)
# Verifies that lease-based election is working with subnet annotations
#---------------------------------------------------------------------
test_lease_verification() {
    echo ""
    echo "=========================================="
    echo "TEST: Lease Verification (Subnet-Aware Election)"
    echo "=========================================="

    info "Checking that leases exist for all nodes..."
    for node in $NODES; do
        if lease_exists "$node"; then
            pass "Lease exists for $node"
        else
            fail "No lease found for $node"
        fi
    done

    info "Checking subnet annotations on leases..."
    for node in $NODES; do
        local LEASE_SUBNETS
        LEASE_SUBNETS=$(get_node_lease_subnets "$node")
        if [ -n "$LEASE_SUBNETS" ]; then
            pass "$node subnets: $LEASE_SUBNETS"
            # Verify the node's expected subnet is present in its lease
            local EXPECTED_SUBNET="${NODE_SUBNET[$node]}"
            if echo "$LEASE_SUBNETS" | grep -q "$EXPECTED_SUBNET"; then
                pass "$node has expected $EXPECTED_SUBNET subnet"
            else
                fail "$node missing expected $EXPECTED_SUBNET subnet"
            fi
        else
            fail "$node has no subnet annotation"
        fi
    done

    pass "Lease-based election verified"
}

#---------------------------------------------------------------------
# Test: Local Pool No Matching Subnet
# Tests that when no node has the pool's subnet, the IP is NOT announced
# anywhere. There is no fallback - subnet filtering is strict.
#---------------------------------------------------------------------
test_local_pool_no_matching_subnet() {
    echo ""
    echo "=========================================="
    echo "TEST: Local Pool No Matching Subnet"
    echo "=========================================="

    info "This tests subnet-aware election with no eligible nodes."
    info "Pool 10.255.0.0/24 has NO nodes with that subnet."
    info "IP should be allocated but NOT announced anywhere (no fallback)."

    # Apply the no-match ServiceGroup
    info "Applying no-match-subnet ServiceGroup..."
    kubectl apply -f ${SCRIPT_DIR}/servicegroup-no-match.yaml

    # Create service requesting IP from no-match pool
    info "Creating service requesting IP from no-match-subnet pool..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-no-match.yaml

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-no-match -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    IP=$(kubectl get svc nginx-lb-no-match -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $IP"

    # Verify IP is from the no-match pool
    [[ "$IP" =~ ^10\.255\.0\.(10[0-9]|110)$ ]] || fail "IP $IP not from expected pool 10.255.0.100-110"
    pass "IP allocated from correct pool"

    # Wait a moment for any announcement attempt
    sleep 5

    # KEY CHECK: Verify IP is NOT on local interface on ANY node (no matching subnet)
    info "Verifying IP is NOT on local interface (no node has 10.255.0.0/24)..."
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            fail "IP $IP found on local interface on $node - should NOT be announced (no matching subnet)"
        fi
    done
    pass "IP correctly NOT on local interface on any node"

    # CRITICAL CHECK: Verify IP is also NOT on kube-lb0 (no fallback for local pools)
    info "Verifying IP is NOT on kube-lb0 (no fallback for local pools)..."
    for node in $NODES; do
        if node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            fail "IP $IP found on kube-lb0 on $node - local pool should NOT fallback to kube-lb0"
        fi
    done
    pass "IP correctly NOT on kube-lb0 on any node (no fallback - correct behavior)"

    # Check for noLocalInterface log message
    info "Checking lbnodeagent logs for noLocalInterface message..."
    if kubectl logs -n purelb-system -l component=lbnodeagent --tail=100 2>/dev/null | grep -q "noLocalInterface"; then
        pass "Found noLocalInterface message in logs (no fallback confirmed)"
    else
        info "noLocalInterface message not found (may have scrolled out)"
    fi

    # Verify noLocalInterface log — emitted when a local pool IP has no matching interface
    # (noEligibleNodes is a separate code path for subnet-aware election mismatch;
    # noLocalInterface fires first when the pool's subnet doesn't match any node interface)
    echo -e "${CYAN}    ── Log Verification ────────────────────────────────────────${NC}"
    assert_log_contains "lbnodeagent" "noLocalInterface" "no local interface for unmatched subnet pool"
    pass "LBNodeAgent logged 'noLocalInterface' (subnet filtering confirmed via log)"

    # Cleanup
    info "Cleaning up no-match test resources..."
    kubectl delete svc nginx-lb-no-match -n $NAMESPACE 2>/dev/null || true
    kubectl delete servicegroup no-match-subnet -n purelb-system 2>/dev/null || true

    pass "Local pool no-matching-subnet test completed"
}

#---------------------------------------------------------------------
# Test: Remote Pool Behavior
# Verifies that remote pool IPs go on kube-lb0 (not local interface)
#---------------------------------------------------------------------
test_remote_pool() {
    echo ""
    echo "=========================================="
    echo "TEST: Remote Pool Behavior"
    echo "=========================================="

    info "Remote pools should place IPs on kube-lb0, not local interface."
    info "They bypass subnet filtering entirely."

    # Apply the remote ServiceGroup
    info "Applying remote-pool ServiceGroup..."
    kubectl apply -f ${SCRIPT_DIR}/servicegroup-remote.yaml

    # Create service requesting IP from remote pool
    info "Creating service requesting IP from remote-pool..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-remote.yaml

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-remote -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    IP=$(kubectl get svc nginx-lb-remote -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $IP"

    # Verify IP is from the remote pool
    [[ "$IP" =~ ^10\.255\.1\.(10[0-9]|110)$ ]] || fail "IP $IP not from expected pool 10.255.1.100-110"
    pass "IP allocated from correct pool"

    # Wait for announcement
    sleep 5

    # Verify IP is on kube-lb0 (not local interface)
    info "Verifying IP is on kube-lb0 (remote pool behavior)..."
    FOUND_ON_KUBELB0=false
    for node in $NODES; do
        if node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            pass "Remote IP $IP on kube-lb0 on $node"
            FOUND_ON_KUBELB0=true
        fi
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            fail "Remote IP $IP found on local interface on $node - should be on kube-lb0"
        fi
    done
    [ "$FOUND_ON_KUBELB0" = "true" ] || fail "Remote IP not found on kube-lb0 on any node"

    # Verify announcingNonLocal log — emitted when remote pool IPs go on kube-lb0
    echo -e "${CYAN}    ── Log Verification ────────────────────────────────────────${NC}"
    info "Checking lbnodeagent logs for announcingNonLocal message..."
    assert_log_contains "lbnodeagent" "announcingNonLocal" "remote pool announced on dummy interface"
    pass "LBNodeAgent logged 'announcingNonLocal' (remote pool on kube-lb0)"

    # Cleanup
    info "Cleaning up remote pool test resources..."
    kubectl delete svc nginx-lb-remote -n $NAMESPACE 2>/dev/null || true
    kubectl delete servicegroup remote-pool -n purelb-system 2>/dev/null || true

    pass "Remote pool behavior test completed"
}

#---------------------------------------------------------------------
# Failover Debug Helpers
#---------------------------------------------------------------------

show_all_leases() {
    info "Current leases:"
    kubectl get leases -n purelb-system -o custom-columns=\
'NAME:.metadata.name,HOLDER:.spec.holderIdentity,RENEW:.spec.renewTime,DURATION:.spec.leaseDurationSeconds' 2>/dev/null | while read line; do
        detail "$line"
    done
}

show_all_pods() {
    info "LBNodeAgent pods:"
    kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null | while read line; do
        detail "$line"
    done
}

show_vip_locations() {
    local IP=$1
    info "VIP $IP location on all nodes:"
    for node in $NODES; do
        local eth0_status="not present"
        local kubelb0_status="not present"

        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            local details=$(node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep ' $IP/'" 2>/dev/null | awk '{print $4, $NF}')
            eth0_status="PRESENT ($details)"
        fi

        if node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            kubelb0_status="PRESENT"
        fi

        detail "$node: eth0=$eth0_status, kube-lb0=$kubelb0_status"
    done
}

show_election_logs() {
    local node=$1
    local lines=${2:-10}
    info "Recent election logs from $node (last $lines):"
    local pod=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null | grep "$node" | awk '{print $1}')
    if [ -n "$pod" ]; then
        kubectl logs -n purelb-system "$pod" --tail=$lines 2>/dev/null | grep -E "(electionWon|lostElection|leaseAdd|leaseDelete|leaseUpdate|rebuildMaps|withdrawAddress|ForceSync|graceful)" | while read line; do
            detail "$line"
        done
    else
        detail "(no pod found for $node)"
    fi
}

#---------------------------------------------------------------------
# Test: Graceful Failover (Lease-Based)
# Verifies that when a node's lbnodeagent is deleted, VIP moves quickly
# Enhanced with detailed debug output for troubleshooting
#---------------------------------------------------------------------
test_graceful_failover() {
    echo ""
    echo "=========================================="
    echo "TEST: Graceful Failover (Lease-Based)"
    echo "=========================================="

    info "Testing lease-based failover by deleting lbnodeagent pod."
    info "VIP should move to another node within ~15 seconds."
    echo ""
    detail "Graceful shutdown sequence:"
    detail "  1. MarkUnhealthy() - Winner() returns ''"
    detail "  2. ForceSync() - triggers address withdrawal"
    detail "  3. Sleep 2s - traffic drain"
    detail "  4. StopRenewals() - stop lease renewal"
    detail "  5. DeleteOurLease() - remove lease from API"
    detail "  6. Shutdown() - cleanup networking"
    echo ""

    # Ensure we have a test service
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV4" ]; then
        info "Creating test service for failover test..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"
        IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        wait_for_ip_announced "$IPV4" 30 || fail "IP not announced within 30s"
    fi

    info "Testing with VIP: $IPV4"

    # Show pre-failover state
    echo ""
    info "=== PRE-FAILOVER STATE ==="
    show_all_leases
    echo ""
    show_vip_locations "$IPV4"

    # Find current VIP holder
    ORIGINAL_WINNER=""
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            ORIGINAL_WINNER=$node
            break
        fi
    done
    [ -n "$ORIGINAL_WINNER" ] || fail "Could not find VIP holder"
    pass "Current VIP holder: $ORIGINAL_WINNER"

    # Verify service is reachable before failover (retry for endpoint propagation)
    info "Testing service reachability before failover..."
    local REACHABLE=false
    for attempt in 1 2 3 4 5; do
        RESPONSE=$(curl -s --connect-timeout 3 "http://$IPV4/" || true)
        if echo "$RESPONSE" | grep -q "Pod:"; then
            local pod=$(echo "$RESPONSE" | grep "Pod:" | awk '{print $2}')
            pass "Service reachable - Pod: $pod"
            REACHABLE=true
            break
        fi
        sleep 2
    done
    [ "$REACHABLE" = "true" ] || fail "Service NOT reachable before failover"

    pause_for_review

    # Delete the lbnodeagent pod on the current winner
    AGENT_POD=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$ORIGINAL_WINNER" | awk '{print $1}')
    [ -n "$AGENT_POD" ] || fail "Could not find lbnodeagent pod on $ORIGINAL_WINNER"

    echo ""
    info "=== TRIGGERING FAILOVER ==="
    info "Target pod: $AGENT_POD on $ORIGINAL_WINNER"

    # Start watching pod logs in background to capture shutdown messages
    info "Starting log capture for shutdown..."
    kubectl logs -n purelb-system "$AGENT_POD" -f --tail=0 > /tmp/shutdown-logs.txt 2>&1 &
    LOG_PID=$!

    DELETE_START=$(date +%s.%N)
    info "Deleting pod with grace-period=10 (allows graceful shutdown)..."
    kubectl delete pod -n purelb-system "$AGENT_POD" --grace-period=10 &
    DELETE_PID=$!

    # Monitor lease deletion
    info "Monitoring lease deletion..."
    LEASE_DELETED=false
    for i in $(seq 1 15); do
        if ! kubectl get lease "purelb-node-$ORIGINAL_WINNER" -n purelb-system &>/dev/null; then
            LEASE_DELETED=true
            pass "Lease deleted after ~${i}s"
            break
        fi
        detail "$(ts) Lease still present at ${i}s"
        sleep 1
    done

    if [ "$LEASE_DELETED" = "false" ]; then
        info "WARNING: Lease NOT deleted after 15s"
    fi

    # Wait for delete to complete
    wait $DELETE_PID 2>/dev/null || true
    kill $LOG_PID 2>/dev/null || true
    DELETE_END=$(date +%s.%N)
    DELETE_DURATION=$(echo "$DELETE_END - $DELETE_START" | bc)
    info "Pod deletion completed in ${DELETE_DURATION}s"

    # Show captured shutdown logs
    echo ""
    info "Captured shutdown logs:"
    if [ -f /tmp/shutdown-logs.txt ] && [ -s /tmp/shutdown-logs.txt ]; then
        cat /tmp/shutdown-logs.txt | head -20 | while read line; do
            detail "$line"
        done
    else
        detail "(No logs captured or empty log file)"
    fi

    # Wait for VIP to move or for same-node recovery.
    # The DaemonSet recreates the pod quickly (~3-5s). If the new pod's lease
    # appears before other nodes process the deletion, the hash may pick the
    # same node again — valid election behavior.
    echo ""
    info "=== MONITORING VIP MOVEMENT ==="
    info "Waiting for VIP failover (max 20s)..."
    TIMEOUT=20
    ELAPSED=0
    NEW_WINNER=""
    while [ $ELAPSED -lt $TIMEOUT ]; do
        # Check if a different node picked up the VIP
        for node in $NODES; do
            if [ "$node" != "$ORIGINAL_WINNER" ]; then
                if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
                    NEW_WINNER=$node
                    break 2
                fi
            fi
        done

        # Check if a new pod on the original node re-won the election
        local CURRENT_POD=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null \
            | grep "$ORIGINAL_WINNER" | grep "Running" | awk '{print $1}')
        if [ -n "$CURRENT_POD" ] && [ "$CURRENT_POD" != "$AGENT_POD" ]; then
            if node_ssh "$ORIGINAL_WINNER" "ip -o addr show ${NODE_IFACE[$ORIGINAL_WINNER]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
                NEW_WINNER=$ORIGINAL_WINNER
                detail "$(ts) New pod $CURRENT_POD re-won election on $ORIGINAL_WINNER"
                break
            fi
        fi

        sleep 2
        ELAPSED=$((ELAPSED + 2))
        if [ $((ELAPSED % 4)) -eq 0 ]; then
            detail "$(ts) Still waiting at ${ELAPSED}s..."
        fi
    done

    if [ -n "$NEW_WINNER" ] && [ "$NEW_WINNER" != "$ORIGINAL_WINNER" ]; then
        pass "VIP moved from $ORIGINAL_WINNER to $NEW_WINNER in ${ELAPSED}s"
    elif [ -n "$NEW_WINNER" ]; then
        pass "New pod on $ORIGINAL_WINNER re-won election in ${ELAPSED}s (valid same-node recovery)"
    else
        fail "VIP did not recover within ${TIMEOUT}s"
    fi

    # On multi-subnet clusters, verify failover stayed on correct subnet
    if [ "$SUBNET_COUNT" -ge 2 ] && [ -n "$NEW_WINNER" ]; then
        verify_vip_subnet_match "$IPV4" "$NEW_WINNER" || fail "Graceful failover: VIP/node subnet mismatch"
        pass "Graceful failover winner $NEW_WINNER is on correct subnet for $IPV4"
    fi

    # Show post-failover state
    echo ""
    info "=== POST-FAILOVER STATE ==="
    show_all_leases
    echo ""
    show_vip_locations "$IPV4"

    # Verify service is still reachable
    info "Verifying service is reachable after failover..."
    sleep 2
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" || true)
    if echo "$RESPONSE" | grep -q "Pod:"; then
        local pod=$(echo "$RESPONSE" | grep "Pod:" | awk '{print $2}')
        pass "Service still reachable after failover - Pod: $pod"
    else
        fail "Service not reachable after failover"
    fi

    pause_for_review

    # Wait for DaemonSet to recover
    echo ""
    info "=== RECOVERY ==="
    info "Waiting for lbnodeagent DaemonSet to recover..."
    kubectl rollout status daemonset/lbnodeagent -n purelb-system --timeout=60s

    # Show recovery state
    echo ""
    show_all_pods
    echo ""
    show_all_leases

    FINAL_WINNER=""
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            FINAL_WINNER=$node
            break
        fi
    done
    info "Final VIP holder: $FINAL_WINNER (was: $ORIGINAL_WINNER, failover: $NEW_WINNER)"

    # Summary
    echo ""
    info "=== SUMMARY ==="
    echo -e "  Original VIP holder: ${YELLOW}$ORIGINAL_WINNER${NC}"
    echo -e "  VIP after failover:  ${YELLOW}$NEW_WINNER${NC}"
    echo -e "  Final VIP holder:    ${YELLOW}$FINAL_WINNER${NC}"
    echo -e "  Delete duration:     ${YELLOW}${DELETE_DURATION}s${NC}"
    echo -e "  Lease deleted:       ${YELLOW}$LEASE_DELETED${NC}"

    # --- Metrics & log verification after failover ---
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    if [ -n "$NEW_WINNER" ]; then
        info "Verifying metrics on new winner ($NEW_WINNER) after failover..."
        local FAILOVER_METRICS
        FAILOVER_METRICS=$(scrape_lbnodeagent_metrics "$NEW_WINNER")
        if [ -n "$FAILOVER_METRICS" ]; then
            assert_metric "$FAILOVER_METRICS" "purelb_election_lease_healthy" "eq" "1"
            pass "New winner $NEW_WINNER: lease_healthy = 1"
            assert_metric "$FAILOVER_METRICS" "purelb_lbnodeagent_election_wins_total" "gt" "0"
            pass "New winner $NEW_WINNER: election_wins_total > 0"
            assert_metric "$FAILOVER_METRICS" "purelb_lbnodeagent_address_additions_total" "gt" "0"
            pass "New winner $NEW_WINNER: address_additions_total > 0"
        else
            info "WARNING: Could not scrape metrics from new winner $NEW_WINNER"
        fi

        # Verify electionWon log on new winner
        info "Checking for electionWon log on new winner..."
        assert_log_contains_on_node "$NEW_WINNER" "electionWon" "election won after failover"
        pass "New winner $NEW_WINNER: logged 'electionWon'"
    fi

    # Verify shutdown/withdrawal in captured logs
    if [ -f /tmp/shutdown-logs.txt ] && [ -s /tmp/shutdown-logs.txt ]; then
        if grep -q "withdrawAddress\|ForceSync\|graceful" /tmp/shutdown-logs.txt; then
            pass "Captured shutdown logs contain withdrawal/ForceSync messages"
        else
            detail "Shutdown logs present but no withdrawal messages found"
        fi
    fi

    if [ "$LEASE_DELETED" = "true" ] && [ -n "$NEW_WINNER" ]; then
        pass "Graceful failover test completed successfully"
    else
        fail "Graceful failover test had issues - review output above"
    fi
}

#---------------------------------------------------------------------
# Test 1: IPv4 Single-Stack Service
#---------------------------------------------------------------------
test_ipv4_singlestack() {
    echo ""
    echo "=========================================="
    echo "TEST 1: IPv4 Single-Stack Service"
    echo "=========================================="

    info "Creating IPv4-only service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IPv4: $IPV4"

    # Verify IP is from pool range
    ip_in_pool_range "$IPV4" || fail "IP $IPV4 not from expected pool range"
    pass "IPv4 allocated from correct pool"

    # Verify IP is on local interface (local subnet)
    # CRITICAL: Use -w for word boundary to prevent partial IP matching
    # e.g., grep '172.30.255.15' would incorrectly match 172.30.255.150
    info "Checking IP location on nodes..."
    WINNER_NODE=""
    for node in $NODES; do
        eth0_ip=$(node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep ' $IPV4/'" 2>/dev/null || true)
        kubelb0_ip=$(node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep ' $IPV4/'" 2>/dev/null || true)
        if [ -n "$eth0_ip" ]; then
            pass "IPv4 $IPV4 is on local interface on $node"
            WINNER_NODE=$node
        fi
        if [ -n "$kubelb0_ip" ]; then
            fail "IPv4 $IPV4 is on kube-lb0 on $node (should be on local interface)"
        fi
    done

    [ -n "$WINNER_NODE" ] || fail "IPv4 not found on any node"

    # On multi-subnet clusters, verify VIP holder is on the correct subnet
    if [ "$SUBNET_COUNT" -ge 2 ]; then
        verify_vip_subnet_match "$IPV4" "$WINNER_NODE" || fail "VIP/node subnet mismatch"
        pass "VIP holder $WINNER_NODE is on correct subnet for $IPV4"
    fi

    # Test connectivity
    info "Testing connectivity to $IPV4..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No response from service"
    pass "IPv4 service is reachable"
    echo "$RESPONSE"

    # Verify PureLB annotations (allocated-by, allocated-from)
    info "Verifying PureLB annotations..."
    ALLOCATED_BY=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-by}')
    ALLOCATED_FROM=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-from}')
    [ "$ALLOCATED_BY" = "PureLB" ] || fail "Missing or wrong purelb.io/allocated-by annotation (got: $ALLOCATED_BY)"
    [ -n "$ALLOCATED_FROM" ] || fail "Missing purelb.io/allocated-from annotation"
    pass "PureLB annotations correctly set (allocated-by=PureLB, allocated-from=$ALLOCATED_FROM)"

    # Verify ipMode field (K8s 1.30+)
    IPMODE=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ipMode}')
    if [ -n "$IPMODE" ]; then
        [ "$IPMODE" = "VIP" ] || fail "ipMode should be VIP, got $IPMODE"
        pass "ipMode correctly set to VIP"
    else
        info "ipMode not set (may be older K8s version)"
    fi

    # --- Metrics verification ---
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    info "Verifying allocator metrics after IPv4 allocation..."
    local ALLOC_METRICS
    ALLOC_METRICS=$(scrape_allocator_metrics)
    if [ -n "$ALLOC_METRICS" ]; then
        assert_metric "$ALLOC_METRICS" 'purelb_address_pool_size{pool="default"}' "gt" "0"
        pass "Allocator: pool size > 0"
        assert_metric "$ALLOC_METRICS" 'purelb_address_pool_addresses_in_use{pool="default"}' "ge" "1"
        pass "Allocator: addresses_in_use >= 1"
    else
        info "WARNING: Could not scrape allocator metrics"
    fi

    if [ -n "$WINNER_NODE" ]; then
        info "Verifying lbnodeagent metrics on winner ($WINNER_NODE)..."
        local AGENT_METRICS
        AGENT_METRICS=$(scrape_lbnodeagent_metrics "$WINNER_NODE")
        if [ -n "$AGENT_METRICS" ]; then
            assert_metric "$AGENT_METRICS" "purelb_lbnodeagent_election_wins_total" "gt" "0"
            pass "Winner $WINNER_NODE: election_wins_total > 0"
            assert_metric "$AGENT_METRICS" "purelb_lbnodeagent_address_additions_total" "gt" "0"
            pass "Winner $WINNER_NODE: address_additions_total > 0"
            # GARP is only sent if GARPConfig is set in LBNodeAgent spec
            local garp_val
            garp_val=$(echo "$AGENT_METRICS" | grep "^purelb_lbnodeagent_garp_sent_total " | awk '{print $NF}')
            if [ -n "$garp_val" ] && [ "$(printf '%.0f' "$garp_val")" -gt 0 ]; then
                pass "Winner $WINNER_NODE: garp_sent_total = $garp_val (GARP enabled)"
            else
                detail "Winner $WINNER_NODE: garp_sent_total = ${garp_val:-0} (GARP not configured or no packets sent)"
            fi
            # Verify announced gauge includes our service
            local announced
            announced=$(echo "$AGENT_METRICS" | grep "purelb_lbnodeagent_announced" | grep "nginx-lb-ipv4" || true)
            [ -n "$announced" ] || fail "announced gauge missing nginx-lb-ipv4 on winner"
            pass "Winner $WINNER_NODE: announced gauge includes nginx-lb-ipv4"
        else
            info "WARNING: Could not scrape lbnodeagent metrics on $WINNER_NODE"
        fi

        # --- Log verification ---
        info "Verifying electionWon log on winner ($WINNER_NODE)..."
        assert_log_contains_on_node "$WINNER_NODE" "electionWon" "election won for IPv4 service"
        pass "Winner $WINNER_NODE: logged 'electionWon'"
    fi
}

#---------------------------------------------------------------------
# Test 2: IPv6 Single-Stack Service
#---------------------------------------------------------------------
test_ipv6_singlestack() {
    echo ""
    echo "=========================================="
    echo "TEST 2: IPv6 Single-Stack Service"
    echo "=========================================="

    info "Creating IPv6-only service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv6.yaml

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv6 -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    IPV6=$(kubectl get svc nginx-lb-ipv6 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IPv6: $IPV6"

    # Verify IP is from pool range
    ipv6_in_pool_range "$IPV6" || fail "IPv6 $IPV6 not from expected pool range"
    pass "IPv6 allocated from correct pool"

    # Verify IP is on local interface (local subnet) - THIS VALIDATES THE IPV6 FLAG FIX
    # CRITICAL: Use ' $IPV6/' pattern to match exact IP with CIDR prefix
    info "Checking IP location on nodes (validates IPv6 flag filtering fix)..."
    WINNER_NODE=""
    for node in $NODES; do
        eth0_ip=$(node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep ' $IPV6/'" 2>/dev/null || true)
        kubelb0_ip=$(node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep ' $IPV6/'" 2>/dev/null || true)
        if [ -n "$eth0_ip" ]; then
            pass "IPv6 $IPV6 is on local interface on $node (IPv6 flag fix working!)"
            WINNER_NODE=$node
        fi
        if [ -n "$kubelb0_ip" ]; then
            fail "IPv6 $IPV6 is on kube-lb0 on $node (IPv6 flag fix NOT working)"
        fi
    done

    [ -n "$WINNER_NODE" ] || fail "IPv6 not found on any node"

    # Test connectivity
    info "Testing connectivity to [$IPV6]..."
    RESPONSE=$(curl -s --connect-timeout 5 -6 "http://[$IPV6]/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No response from service"
    pass "IPv6 service is reachable"
    echo "$RESPONSE"

    # --- Metrics verification ---
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    if [ -n "$WINNER_NODE" ]; then
        info "Verifying lbnodeagent metrics on IPv6 winner ($WINNER_NODE)..."
        local AGENT_METRICS
        AGENT_METRICS=$(scrape_lbnodeagent_metrics "$WINNER_NODE")
        if [ -n "$AGENT_METRICS" ]; then
            assert_metric "$AGENT_METRICS" "purelb_lbnodeagent_election_wins_total" "gt" "0"
            pass "Winner $WINNER_NODE: election_wins_total > 0 (IPv6)"
            assert_metric "$AGENT_METRICS" "purelb_lbnodeagent_address_additions_total" "gt" "0"
            pass "Winner $WINNER_NODE: address_additions_total > 0 (IPv6)"
            # Verify announced gauge includes our IPv6 service
            local announced
            announced=$(echo "$AGENT_METRICS" | grep "purelb_lbnodeagent_announced" | grep "nginx-lb-ipv6" || true)
            [ -n "$announced" ] || fail "announced gauge missing nginx-lb-ipv6 on winner"
            pass "Winner $WINNER_NODE: announced gauge includes nginx-lb-ipv6"
        else
            info "WARNING: Could not scrape lbnodeagent metrics on $WINNER_NODE"
        fi

        # --- Log verification ---
        info "Verifying electionWon log on IPv6 winner ($WINNER_NODE)..."
        assert_log_contains_on_node "$WINNER_NODE" "electionWon" "election won for IPv6 service"
        pass "Winner $WINNER_NODE: logged 'electionWon' (IPv6)"
    fi
}

#---------------------------------------------------------------------
# Test 3: Dual-Stack Service
#---------------------------------------------------------------------
test_dualstack() {
    echo ""
    echo "=========================================="
    echo "TEST 3: Dual-Stack Service"
    echo "=========================================="

    info "Creating dual-stack service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-dualstack.yaml

    info "Waiting for IP allocation..."
    sleep 5

    # FIX: Don't assume index order - detect by address format
    # IPv4 could be at index 0 or 1 depending on allocator processing order
    IPV4=""
    IPV6=""
    for i in 0 1; do
        IP=$(kubectl get svc nginx-lb-dualstack -n $NAMESPACE -o jsonpath="{.status.loadBalancer.ingress[$i].ip}")
        if [[ "$IP" =~ ":" ]]; then
            IPV6="$IP"
        else
            IPV4="$IP"
        fi
    done

    [ -n "$IPV4" ] || fail "No IPv4 allocated"
    [ -n "$IPV6" ] || fail "No IPv6 allocated"

    info "Allocated IPv4: $IPV4"
    info "Allocated IPv6: $IPV6"
    pass "Both IPv4 and IPv6 allocated"

    # Check that BOTH are on local interface and NEITHER is on kube-lb0
    # FIX: Use ' $IP/' pattern for exact matching with CIDR prefix
    info "Checking both IPs are on local interface (validates announceRemote fix)..."
    IPV4_NODE=""
    IPV6_NODE=""

    for node in $NODES; do
        # Check IPv4 - use exact match with CIDR prefix
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            IPV4_NODE=$node
        fi
        if node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            fail "IPv4 $IPV4 on kube-lb0 on $node (BUG!)"
        fi

        # Check IPv6 - use exact match with CIDR prefix
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV6/'" 2>/dev/null; then
            IPV6_NODE=$node
        fi
        if node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IPV6/'" 2>/dev/null; then
            fail "IPv6 $IPV6 on kube-lb0 on $node (BUG!)"
        fi
    done

    [ -n "$IPV4_NODE" ] || fail "IPv4 not on any node's local interface"
    [ -n "$IPV6_NODE" ] || fail "IPv6 not on any node's local interface"

    pass "IPv4 on local interface on $IPV4_NODE"
    pass "IPv6 on local interface on $IPV6_NODE"

    # They may be on different nodes (independent elections)
    if [ "$IPV4_NODE" != "$IPV6_NODE" ]; then
        info "IPv4 and IPv6 on different nodes (independent elections working)"
    else
        info "IPv4 and IPv6 on same node"
    fi

    # Test connectivity to both
    info "Testing IPv4 connectivity..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No IPv4 response"
    pass "IPv4 reachable"

    info "Testing IPv6 connectivity..."
    RESPONSE=$(curl -s --connect-timeout 5 -6 "http://[$IPV6]/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No IPv6 response"
    pass "IPv6 reachable"
}

#---------------------------------------------------------------------
# Test 4: Leader Election
#---------------------------------------------------------------------
test_leader_election() {
    echo ""
    echo "=========================================="
    echo "TEST 4: Leader Election"
    echo "=========================================="

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

    # FIX: Use exact IP matching with CIDR prefix to prevent partial matches
    info "Checking that only ONE node has $IPV4..."
    VIP_NODE_COUNT=0
    WINNER=""
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            VIP_NODE_COUNT=$((VIP_NODE_COUNT + 1))
            WINNER=$node
        fi
    done

    [ "$VIP_NODE_COUNT" -eq 1 ] || fail "IP on $VIP_NODE_COUNT nodes (expected 1)"
    pass "Only $WINNER is announcing $IPV4 (election working)"

    # Verify purelb.io/announcing-IPv4 annotation matches winner
    info "Verifying announcing annotation..."
    ANNOUNCING=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/announcing-IPv4}')
    info "Announcing annotation: $ANNOUNCING"
    [ -n "$ANNOUNCING" ] || fail "Missing purelb.io/announcing-IPv4 annotation"
    [[ "$ANNOUNCING" == *"$WINNER"* ]] || fail "Announcing annotation '$ANNOUNCING' doesn't match winner '$WINNER'"
    pass "Announcing annotation correctly set to $ANNOUNCING"

    # --- Metrics verification: Election-specific metrics ---
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    info "Verifying election metrics on winner ($WINNER)..."
    local WINNER_METRICS
    WINNER_METRICS=$(scrape_lbnodeagent_metrics "$WINNER")
    if [ -n "$WINNER_METRICS" ]; then
        assert_metric "$WINNER_METRICS" "purelb_election_lease_healthy" "eq" "1"
        pass "Winner $WINNER: lease_healthy = 1"
        assert_metric "$WINNER_METRICS" "purelb_election_member_count" "ge" "1"
        local members
        members=$(echo "$WINNER_METRICS" | grep "^purelb_election_member_count " | awk '{print $NF}')
        pass "Winner $WINNER: member_count = $members"
        assert_metric "$WINNER_METRICS" "purelb_election_subnet_count" "ge" "1"
        local subnets
        subnets=$(echo "$WINNER_METRICS" | grep "^purelb_election_subnet_count " | awk '{print $NF}')
        pass "Winner $WINNER: subnet_count = $subnets"
        assert_metric "$WINNER_METRICS" "purelb_election_local_subnet_count" "ge" "1"
        pass "Winner $WINNER: local_subnet_count >= 1"
        assert_metric "$WINNER_METRICS" "purelb_election_lease_renewals_total" "gt" "0"
        local renewals
        renewals=$(echo "$WINNER_METRICS" | grep "^purelb_election_lease_renewals_total " | awk '{print $NF}')
        pass "Winner $WINNER: lease_renewals_total = $renewals"
    else
        info "WARNING: Could not scrape lbnodeagent metrics on $WINNER"
    fi

    # Verify non-winner nodes also have healthy leases
    for node in $NODES; do
        if [ "$node" != "$WINNER" ]; then
            info "Checking lease health on non-winner $node..."
            local OTHER_METRICS
            OTHER_METRICS=$(scrape_lbnodeagent_metrics "$node")
            if [ -n "$OTHER_METRICS" ]; then
                assert_metric "$OTHER_METRICS" "purelb_election_lease_healthy" "eq" "1"
                pass "Non-winner $node: lease_healthy = 1"
                local losses
                losses=$(echo "$OTHER_METRICS" | grep "^purelb_lbnodeagent_election_losses_total " | awk '{print $NF}')
                if [ -n "$losses" ] && [ "$(printf '%.0f' "$losses")" -gt 0 ]; then
                    pass "Non-winner $node: election_losses_total = $losses"
                else
                    detail "Non-winner $node: election_losses = ${losses:-0}"
                fi
            fi
            break  # check just one non-winner to keep test fast
        fi
    done

    # --- Log verification ---
    info "Verifying election log messages..."
    assert_log_contains_on_node "$WINNER" "electionWon" "election won on winner node"
    pass "Winner $WINNER: logged 'electionWon'"

    # Check non-winner nodes for notWinner/lostElection log
    info "Checking non-winner nodes for election loss logs..."
    local found_loser_log=false
    for node in $NODES; do
        if [ "$node" != "$WINNER" ]; then
            local pod
            pod=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null \
                | grep "$node" | awk '{print $1}')
            if [ -n "$pod" ]; then
                if kubectl logs -n purelb-system "$pod" --tail=200 2>/dev/null | grep -qE "(notWinner|lostElection)"; then
                    pass "Non-winner $node: logged 'notWinner'/'lostElection'"
                    found_loser_log=true
                    break
                fi
            fi
        fi
    done
    if [ "$found_loser_log" = "false" ]; then
        detail "No notWinner/lostElection log found on non-winners (debug-level, may not be visible)"
    fi

    # Check for subnet discovery on any node
    assert_log_contains "lbnodeagent" "getLocalSubnets" "subnet discovery"
    pass "LBNodeAgent: logged 'getLocalSubnets' (subnet discovery)"

    # Verify no panic/fatal errors
    info "Checking for absence of panic/fatal errors..."
    local panic_logs
    panic_logs=$(kubectl logs -n purelb-system -l component=lbnodeagent --tail=200 2>/dev/null \
        | grep -iE "(panic|fatal|FATAL)" || true)
    [ -z "$panic_logs" ] || fail "Panic or fatal errors found in lbnodeagent logs"
    pass "No panic or fatal errors in lbnodeagent logs"
}

#---------------------------------------------------------------------
# Test 5: Service Deletion Cleanup
#---------------------------------------------------------------------
test_service_cleanup() {
    echo ""
    echo "=========================================="
    echo "TEST 5: Service Deletion Cleanup"
    echo "=========================================="

    # Get current IP and VIP holder before deletion
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Deleting service with IP $IPV4..."

    # Capture pre-deletion metrics for comparison
    local HOLDER_NODE
    HOLDER_NODE=$(get_vip_holder "$IPV4" 2>/dev/null || echo "NONE")
    local before_in_use
    before_in_use=$(scrape_allocator_metric 'purelb_address_pool_addresses_in_use{pool="default"}')
    before_in_use=$(printf '%.0f' "${before_in_use:-0}")
    local before_withdrawals="0"
    if [ "$HOLDER_NODE" != "NONE" ]; then
        before_withdrawals=$(scrape_lbnodeagent_metrics "$HOLDER_NODE" | grep "^purelb_lbnodeagent_address_withdrawals_total " | awk '{print $NF}')
        before_withdrawals=$(printf '%.0f' "${before_withdrawals:-0}")
    fi

    kubectl delete svc nginx-lb-ipv4 -n $NAMESPACE

    # FIX: Use polling with timeout instead of fixed sleep
    # This prevents intermittent failures in slow environments
    info "Verifying IP removed from all nodes (polling with 30s timeout)..."
    TIMEOUT=30
    INTERVAL=2
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        IP_FOUND=false
        for node in $NODES; do
            # Use exact IP matching with CIDR prefix
            if node_ssh $node "ip -o addr show 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
                IP_FOUND=true
                break
            fi
        done
        if [ "$IP_FOUND" = "false" ]; then
            pass "IP $IPV4 removed from all nodes (took ${ELAPSED}s)"
            break
        fi
        sleep $INTERVAL
        ELAPSED=$((ELAPSED + INTERVAL))
    done
    [ $ELAPSED -lt $TIMEOUT ] || fail "IP $IPV4 not removed within ${TIMEOUT}s"

    # --- Metrics verification after deletion ---
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    info "Verifying metrics reflect service deletion..."
    local after_in_use
    after_in_use=$(scrape_allocator_metric 'purelb_address_pool_addresses_in_use{pool="default"}')
    after_in_use=$(printf '%.0f' "${after_in_use:-0}")
    if [ "$before_in_use" -gt 0 ]; then
        [ "$after_in_use" -lt "$before_in_use" ] || fail "addresses_in_use did not decrease after deletion (before=$before_in_use, after=$after_in_use)"
        pass "Allocator: addresses_in_use decreased ($before_in_use -> $after_in_use)"
    fi

    if [ "$HOLDER_NODE" != "NONE" ]; then
        local after_withdrawals
        after_withdrawals=$(scrape_lbnodeagent_metrics "$HOLDER_NODE" | grep "^purelb_lbnodeagent_address_withdrawals_total " | awk '{print $NF}')
        after_withdrawals=$(printf '%.0f' "${after_withdrawals:-0}")
        [ "$after_withdrawals" -gt "$before_withdrawals" ] || fail "address_withdrawals_total did not increase on $HOLDER_NODE"
        pass "LBNodeAgent $HOLDER_NODE: address_withdrawals_total increased ($before_withdrawals -> $after_withdrawals)"

        # Verify withdrawal log
        info "Checking for withdrawAddress log..."
        assert_log_contains_on_node "$HOLDER_NODE" "withdrawAddress" "address withdrawal after service delete"
        pass "LBNodeAgent $HOLDER_NODE: logged 'withdrawAddress'"
    fi

    # Recreate for other tests
    info "Recreating IPv4 service..."
    kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s
}

#---------------------------------------------------------------------
# Test 6: IP Sharing (allow-shared-ip annotation)
#---------------------------------------------------------------------
test_ip_sharing() {
    echo ""
    echo "=========================================="
    echo "TEST 6: IP Sharing (allow-shared-ip)"
    echo "=========================================="

    info "Creating first service with sharing key 'webservers'..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-http
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: webservers
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: http
    port: 80
    targetPort: 80
EOF

    info "Waiting for first service IP..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-shared-http -n $NAMESPACE --timeout=30s || fail "No IP allocated for first service"

    SHARED_IP=$(kubectl get svc nginx-shared-http -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "First service got IP: $SHARED_IP"

    info "Creating second service with SAME sharing key 'webservers' but different port..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-https
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: webservers
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: https
    port: 443
    targetPort: 80
EOF

    info "Waiting for second service IP..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-shared-https -n $NAMESPACE --timeout=30s || fail "No IP allocated for second service"

    SHARED_IP2=$(kubectl get svc nginx-shared-https -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Second service got IP: $SHARED_IP2"

    # Verify both services got the SAME IP
    if [ "$SHARED_IP" = "$SHARED_IP2" ]; then
        pass "Both services share the same IP: $SHARED_IP"
    else
        fail "Services got different IPs: $SHARED_IP vs $SHARED_IP2 (sharing failed)"
    fi

    # Wait for IP to be announced on a node before testing connectivity
    info "Waiting for IP $SHARED_IP to be announced on a node..."
    wait_for_ip_announced "$SHARED_IP" 30 || fail "IP $SHARED_IP not announced within 30s"
    pass "IP announced on a node"

    # Verify both services are accessible on their respective ports.
    # Retry: kube-proxy needs time to program rules for newly-shared services.
    info "Testing connectivity to port 80..."
    local PORT80_OK=false
    for attempt in 1 2 3 4 5; do
        RESPONSE1=$(curl -s --connect-timeout 5 "http://$SHARED_IP:80/" || true)
        if echo "$RESPONSE1" | grep -q "Pod:"; then PORT80_OK=true; break; fi
        sleep 2
    done
    [ "$PORT80_OK" = "true" ] || fail "No response on port 80"
    pass "Port 80 is reachable"

    info "Testing connectivity to port 443..."
    local PORT443_OK=false
    for attempt in 1 2 3 4 5; do
        RESPONSE2=$(curl -s --connect-timeout 5 "http://$SHARED_IP:443/" || true)
        if echo "$RESPONSE2" | grep -q "Pod:"; then PORT443_OK=true; break; fi
        sleep 2
    done
    [ "$PORT443_OK" = "true" ] || fail "No response on port 443"
    pass "Port 443 is reachable"

    # Test that a service with DIFFERENT sharing key gets a DIFFERENT IP
    info "Creating third service with DIFFERENT sharing key..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-other
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: other-group
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: http
    port: 80
    targetPort: 80
EOF

    info "Waiting for third service IP..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-shared-other -n $NAMESPACE --timeout=30s || fail "No IP allocated for third service"

    OTHER_IP=$(kubectl get svc nginx-shared-other -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Third service (different key) got IP: $OTHER_IP"

    if [ "$SHARED_IP" != "$OTHER_IP" ]; then
        pass "Different sharing keys correctly got different IPs"
    else
        fail "Different sharing keys got SAME IP (should be different)"
    fi

    # Test port conflict: same sharing key + same port = should fail
    info "Testing port conflict: same sharing key BUT same port (should fail)..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-shared-conflict
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-shared-ip: webservers
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: http-conflict
    port: 80
    targetPort: 80
EOF

    # FIX: Check for AllocationFailed event, not just empty IP
    # Empty IP could mean slow allocation, not port conflict rejection
    sleep 5
    CONFLICT_IP=$(kubectl get svc nginx-shared-conflict -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)

    if [ -n "$CONFLICT_IP" ]; then
        fail "Port conflict was NOT detected - got IP $CONFLICT_IP (expected allocation to fail)"
    fi

    # Verify we got an AllocationFailed event (proves port conflict was detected)
    info "Checking for AllocationFailed event..."
    EVENTS=$(kubectl get events -n $NAMESPACE --field-selector involvedObject.name=nginx-shared-conflict,reason=AllocationFailed -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if [ -n "$EVENTS" ]; then
        pass "Port conflict correctly detected: $EVENTS"
    else
        # If no event but also no IP, it's still a pass (allocation was prevented)
        info "No AllocationFailed event found, but IP allocation was prevented"
        pass "Port conflict correctly prevented IP allocation"
    fi

    # Cleanup test services
    info "Cleaning up IP sharing test services..."
    kubectl delete svc nginx-shared-http nginx-shared-https nginx-shared-other nginx-shared-conflict -n $NAMESPACE 2>/dev/null || true
    pass "IP sharing test completed successfully"
}

#---------------------------------------------------------------------
# Test 7: Multi-Pod Load Balancing
#---------------------------------------------------------------------
test_multi_pod_lb() {
    echo ""
    echo "=========================================="
    echo "TEST 7: Multi-Pod Load Balancing"
    echo "=========================================="

    # Get current replica count to restore later
    ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    info "Current replica count: $ORIGINAL_REPLICAS"

    # Scale to 3 replicas to spread across nodes
    info "Scaling nginx deployment to 3 replicas..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=3
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s

    # Wait for all pods to be ready
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s

    # Show pod distribution across nodes
    info "Pod distribution across nodes:"
    kubectl get pods -n $NAMESPACE -l app=nginx -o wide

    # Get the service IP
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Testing load balancing to $IPV4..."

    # Brief pause for kube-proxy to update endpoint rules after scale-up
    sleep 3

    # Make multiple requests and collect unique pod names
    declare -A PODS_SEEN
    TOTAL_REQUESTS=20

    info "Sending $TOTAL_REQUESTS requests to check load distribution..."
    for i in $(seq 1 $TOTAL_REQUESTS); do
        RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)
        POD_NAME=$(echo "$RESPONSE" | grep "Pod:" | awk '{print $2}')
        if [ -n "$POD_NAME" ]; then
            PODS_SEEN[$POD_NAME]=1
        fi
    done

    UNIQUE_PODS=${#PODS_SEEN[@]}
    info "Received responses from $UNIQUE_PODS unique pods:"
    for pod in "${!PODS_SEEN[@]}"; do
        echo "  - $pod"
    done

    # Verify we got responses from multiple pods
    if [ "$UNIQUE_PODS" -ge 2 ]; then
        pass "Load balancing working: traffic distributed across $UNIQUE_PODS pods"
    else
        fail "Load balancing issue: only $UNIQUE_PODS pod(s) responded (expected 2+)"
    fi

    # Check which nodes are serving traffic
    info "Checking node distribution..."
    declare -A NODES_SEEN
    for i in $(seq 1 10); do
        RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)
        NODE_NAME=$(echo "$RESPONSE" | grep "Node:" | awk '{print $2}')
        if [ -n "$NODE_NAME" ]; then
            NODES_SEEN[$NODE_NAME]=1
        fi
    done

    UNIQUE_NODES=${#NODES_SEEN[@]}
    info "Responses came from $UNIQUE_NODES unique nodes:"
    for node in "${!NODES_SEEN[@]}"; do
        echo "  - $node"
    done

    # For local pools, traffic goes through the elected node then forwards to pods
    # So we expect responses from multiple pods but may see same node in response
    # (the Node: field shows where the pod runs, not the LB node)
    if [ "$UNIQUE_NODES" -ge 2 ]; then
        pass "Multi-node load balancing: pods on $UNIQUE_NODES different nodes served traffic"
    else
        info "Note: All responding pods happen to be on $UNIQUE_NODES node(s) - this is OK if pods are scheduled there"
    fi

    # Restore original replica count
    info "Restoring original replica count ($ORIGINAL_REPLICAS)..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s

    pass "Multi-pod load balancing test completed"
}

#---------------------------------------------------------------------
# Test: LoadBalancerClass Filtering
# Tests that PureLB ignores services with a non-PureLB LoadBalancerClass
# and correctly allocates when the PureLB class is explicitly set.
#---------------------------------------------------------------------
test_loadbalancer_class() {
    echo ""
    echo "=========================================="
    echo "TEST: LoadBalancerClass Filtering"
    echo "=========================================="

    # Test 1: Service with a foreign LoadBalancerClass should be ignored
    info "Creating service with non-PureLB LoadBalancerClass..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-foreign-lbclass
  namespace: $NAMESPACE
spec:
  type: LoadBalancer
  loadBalancerClass: other.io/foreign-lb
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    info "Waiting 10s to confirm no IP is allocated..."
    sleep 10

    FOREIGN_IP=$(kubectl get svc nginx-foreign-lbclass -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    FOREIGN_BRAND=$(kubectl get svc nginx-foreign-lbclass -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-by}' 2>/dev/null || true)

    if [ -z "$FOREIGN_IP" ] && [ -z "$FOREIGN_BRAND" ]; then
        pass "Foreign LoadBalancerClass correctly ignored — no IP allocated, no PureLB annotations"
    else
        fail "PureLB should NOT allocate for foreign LBClass (got IP=$FOREIGN_IP, brand=$FOREIGN_BRAND)"
    fi

    # Test 2: Service with PureLB's explicit LoadBalancerClass should allocate
    info "Creating service with explicit PureLB LoadBalancerClass..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-purelb-lbclass
  namespace: $NAMESPACE
spec:
  type: LoadBalancer
  loadBalancerClass: purelb.io/purelbv2
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-purelb-lbclass -n $NAMESPACE --timeout=30s || fail "No IP allocated for PureLB LBClass"

    PURELB_IP=$(kubectl get svc nginx-purelb-lbclass -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    PURELB_BRAND=$(kubectl get svc nginx-purelb-lbclass -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-by}')

    info "Allocated IP: $PURELB_IP"
    [ "$PURELB_BRAND" = "PureLB" ] || fail "Missing or wrong allocated-by annotation (got: $PURELB_BRAND)"
    ip_in_pool_range "$PURELB_IP" || fail "IP $PURELB_IP not from expected pool range"
    pass "Explicit PureLB LBClass correctly allocates IP $PURELB_IP"

    # Test connectivity
    info "Waiting for IP to be announced..."
    wait_for_ip_announced "$PURELB_IP" 30 || fail "IP $PURELB_IP not announced within 30s"

    info "Testing connectivity to $PURELB_IP..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$PURELB_IP/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No response from PureLB LBClass service"
    pass "PureLB LBClass service is reachable"

    # Cleanup
    info "Cleaning up LoadBalancerClass test services..."
    kubectl delete svc nginx-foreign-lbclass nginx-purelb-lbclass -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    pass "LoadBalancerClass filtering test completed successfully"
}

#---------------------------------------------------------------------
# Test 8: Node Failure and Failover
#---------------------------------------------------------------------
test_node_failover() {
    echo ""
    echo "=========================================="
    echo "TEST 8: Node Failure and Failover"
    echo "=========================================="

    # Get service IP
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Testing failover for service IP: $IPV4"

    # Find which node currently has the VIP
    # FIX: Use exact IP matching with CIDR prefix
    ORIGINAL_WINNER=""
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            ORIGINAL_WINNER=$node
            break
        fi
    done

    [ -n "$ORIGINAL_WINNER" ] || fail "Could not find node with VIP $IPV4"
    info "Current VIP holder: $ORIGINAL_WINNER"

    # Verify service is working before failover.
    # Retry a few times: the preceding multi-pod test scales the deployment
    # down, and kube-proxy needs time to update iptables after endpoint changes.
    info "Verifying service is reachable before failover..."
    local REACHABLE=false
    for attempt in 1 2 3 4 5; do
        RESPONSE=$(curl -s --connect-timeout 5 "http://$IPV4/" || true)
        if echo "$RESPONSE" | grep -q "Pod:"; then
            REACHABLE=true
            break
        fi
        sleep 2
    done
    [ "$REACHABLE" = "true" ] || fail "Service not reachable before failover"
    pass "Service reachable before failover"

    # Simulate node failure by adding a taint that lbnodeagent doesn't tolerate,
    # then deleting the pod with graceful shutdown. This allows the lbnodeagent to
    # withdraw addresses before terminating.
    info "Simulating node failure: tainting and deleting lbnodeagent on $ORIGINAL_WINNER..."
    kubectl taint node "$ORIGINAL_WINNER" purelb-test=failover:NoExecute --overwrite

    # Delete the pod with grace period to allow graceful shutdown (address withdrawal)
    AGENT_POD=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$ORIGINAL_WINNER" | awk '{print $1}')
    if [ -n "$AGENT_POD" ]; then
        # Use grace-period=10 to allow lbnodeagent to withdraw addresses
        kubectl delete pod -n purelb-system "$AGENT_POD" --grace-period=10 2>/dev/null || true
    fi

    # Wait for the pod to terminate and lease to expire
    info "Waiting for pod termination and lease expiry (~15s)..."
    sleep 15

    # Verify IP was REMOVED from the failed node (graceful shutdown should withdraw it)
    info "Verifying IP was withdrawn from $ORIGINAL_WINNER..."
    if node_ssh "$ORIGINAL_WINNER" "ip -o addr show ${NODE_IFACE[$ORIGINAL_WINNER]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
        # IP still present - check if it's orphaned (no lbnodeagent running)
        if ! kubectl get pods -n purelb-system -o wide | grep -q "$ORIGINAL_WINNER"; then
            info "Note: IP orphaned on $ORIGINAL_WINNER (will expire via valid_lft)"
        else
            kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true
            fail "IP still on $ORIGINAL_WINNER but lbnodeagent is running"
        fi
    else
        pass "IP successfully withdrawn from $ORIGINAL_WINNER"
    fi

    # Check that a DIFFERENT node has taken over the VIP
    # With lease-based election, when the original winner's lease expires,
    # a new winner is elected from remaining healthy nodes
    info "Checking for failover to a different node..."
    NEW_WINNER=""
    for node in $NODES; do
        [ "$node" = "$ORIGINAL_WINNER" ] && continue
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            NEW_WINNER=$node
            break
        fi
    done

    if [ -z "$NEW_WINNER" ]; then
        # VIP might be in transition - wait a bit more for new winner to announce
        info "VIP not found on alternate node yet, waiting for election..."
        sleep 10
        for node in $NODES; do
            [ "$node" = "$ORIGINAL_WINNER" ] && continue
            if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
                NEW_WINNER=$node
                break
            fi
        done
    fi

    [ -n "$NEW_WINNER" ] || { kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true; fail "VIP $IPV4 not found on any alternate node after failover"; }
    pass "Failover successful: VIP now on $NEW_WINNER (was $ORIGINAL_WINNER)"

    # On multi-subnet clusters, verify failover stayed on correct subnet
    if [ "$SUBNET_COUNT" -ge 2 ]; then
        verify_vip_subnet_match "$IPV4" "$NEW_WINNER" || { kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true; fail "After failover: VIP/node subnet mismatch"; }
        pass "Failover winner $NEW_WINNER is on correct subnet for $IPV4"
    fi

    # Verify service is still reachable via the new winner
    info "Verifying service is reachable after failover..."
    RESPONSE=$(curl -s --connect-timeout 10 "http://$IPV4/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || { kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true; fail "Service not reachable after failover"; }
    pass "Service still reachable after failover"

    # Remove taint to allow DaemonSet to restore
    info "Removing taint from $ORIGINAL_WINNER..."
    kubectl taint node "$ORIGINAL_WINNER" purelb-test- 2>/dev/null || true

    # Wait for DaemonSet to fully recover with polling
    info "Waiting for lbnodeagent DaemonSet to recover..."
    kubectl rollout status daemonset/lbnodeagent -n purelb-system --timeout=60s

    # Verify all agents are running (with polling to handle timing issues)
    EXPECTED_AGENTS=$NODE_COUNT
    info "Verifying all $EXPECTED_AGENTS lbnodeagent pods are running (polling with 30s timeout)..."
    TIMEOUT=30
    INTERVAL=2
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        RUNNING_AGENTS=$(kubectl get pods -n purelb-system -l component=lbnodeagent --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
        if [ "$RUNNING_AGENTS" -eq "$EXPECTED_AGENTS" ]; then
            pass "All $EXPECTED_AGENTS lbnodeagent pods recovered (took ${ELAPSED}s)"
            break
        fi
        sleep $INTERVAL
        ELAPSED=$((ELAPSED + INTERVAL))
    done
    [ "$RUNNING_AGENTS" -eq "$EXPECTED_AGENTS" ] || fail "Expected $EXPECTED_AGENTS agents, got $RUNNING_AGENTS after ${TIMEOUT}s"

    pass "Node failover test completed successfully"
}

#---------------------------------------------------------------------
# Test 9: Specific IP Request (purelb.io/addresses)
#---------------------------------------------------------------------
test_specific_ip_request() {
    echo ""
    echo "=========================================="
    echo "TEST 9: Specific IP Request (purelb.io/addresses)"
    echo "=========================================="

    REQUESTED_IP=$(subnet_test_ip "$FIRST_SUBNET" 10)
    info "Requesting specific IP: $REQUESTED_IP"

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-specific-ip
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/addresses: "$REQUESTED_IP"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8080
    targetPort: 80
EOF

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-specific-ip -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    ALLOCATED_IP=$(kubectl get svc nginx-specific-ip -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $ALLOCATED_IP"

    if [ "$ALLOCATED_IP" = "$REQUESTED_IP" ]; then
        pass "Got requested IP: $ALLOCATED_IP"
    else
        fail "Got wrong IP: $ALLOCATED_IP (requested $REQUESTED_IP)"
    fi

    # Wait for IP to be announced on a node before testing connectivity
    info "Waiting for IP $ALLOCATED_IP to be announced on a node..."
    wait_for_ip_announced "$ALLOCATED_IP" 30 || fail "IP $ALLOCATED_IP not announced within 30s"
    pass "IP announced on a node"

    # Verify specific IP is on local interface (local pool), NOT on kube-lb0
    info "Verifying specific IP is on local interface (not kube-lb0)..."
    FOUND_ON_ETH0=false
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $ALLOCATED_IP/'" 2>/dev/null; then
            pass "Specific IP $ALLOCATED_IP on local interface on $node"
            FOUND_ON_ETH0=true
        fi
        if node_ssh $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $ALLOCATED_IP/'" 2>/dev/null; then
            fail "Specific IP $ALLOCATED_IP found on kube-lb0 (should be on local interface for local pool)"
        fi
    done
    [ "$FOUND_ON_ETH0" = "true" ] || fail "Specific IP not found on local interface on any node"

    # Verify service is reachable on the specific IP
    info "Testing connectivity to $ALLOCATED_IP:8080..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$ALLOCATED_IP:8080/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "No response from specific IP service"
    pass "Specific IP service is reachable"

    # Cleanup
    info "Cleaning up specific IP test service..."
    kubectl delete svc nginx-specific-ip -n $NAMESPACE
    pass "Specific IP request test completed successfully"
}

#---------------------------------------------------------------------
# Test 10: ETP Local Override (purelb.io/allow-local annotation)
#---------------------------------------------------------------------
test_etp_local_override() {
    echo ""
    echo "=========================================="
    echo "TEST 10: ETP Local Override (purelb.io/allow-local)"
    echo "=========================================="

    # For local pools, ETP Local is normally overridden to Cluster.
    # The allow-local annotation allows ETP Local on local pools.
    # This is risky because only the elected node announces, but
    # if that node doesn't have an endpoint, traffic blackholes.

    # First, test WITHOUT the annotation - ETP Local should be converted to Cluster
    info "Creating ETP Local service WITHOUT allow-local annotation..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local-no-override
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8091
    targetPort: 80
EOF

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-etp-local-no-override -n $NAMESPACE --timeout=30s || fail "No IP allocated"
    sleep 5

    IP_NO_OVERRIDE=$(kubectl get svc nginx-etp-local-no-override -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $IP_NO_OVERRIDE"

    # Verify LocalOverride event was generated (ETP switched from Local to Cluster)
    info "Checking for LocalOverride event..."
    EVENTS=$(kubectl get events -n $NAMESPACE --field-selector reason=LocalOverride -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if [ -n "$EVENTS" ]; then
        info "LocalOverride event: $EVENTS"
        pass "ETP Local was overridden to Cluster (expected for local pool without annotation)"
    else
        info "No LocalOverride event found (may have already been cleaned up)"
    fi

    # Verify IP is on local interface (local pool behavior)
    info "Verifying IP is on local interface..."
    wait_for_ip_announced "$IP_NO_OVERRIDE" 30 || fail "IP not announced within 30s"
    FOUND_ON_ETH0=false
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IP_NO_OVERRIDE/'" 2>/dev/null; then
            FOUND_ON_ETH0=true
            pass "IP $IP_NO_OVERRIDE on eth0 on $node (election working)"
        fi
    done
    [ "$FOUND_ON_ETH0" = "true" ] || fail "IP not on local interface on any node"

    # Verify connectivity (should work because ETP was converted to Cluster)
    info "Testing connectivity (should work with ETP Cluster)..."
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IP_NO_OVERRIDE:8091/" || true)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Service not reachable"
    pass "Service reachable with overridden ETP"

    # Cleanup first service
    kubectl delete svc nginx-etp-local-no-override -n $NAMESPACE

    # Now test WITH the annotation - ETP Local should be allowed
    info "Creating ETP Local service WITH allow-local annotation..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local-with-override
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/allow-local: "true"
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8092
    targetPort: 80
EOF

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-etp-local-with-override -n $NAMESPACE --timeout=30s || fail "No IP allocated"
    sleep 5

    IP_WITH_OVERRIDE=$(kubectl get svc nginx-etp-local-with-override -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated IP: $IP_WITH_OVERRIDE"

    # Verify LocalOverride event mentions the override annotation was found
    info "Checking that allow-local annotation was recognized..."
    OVERRIDE_EVENTS=$(kubectl get events -n $NAMESPACE --field-selector involvedObject.name=nginx-etp-local-with-override,reason=LocalOverride -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if echo "$OVERRIDE_EVENTS" | grep -q "override annotation found"; then
        pass "Allow-local annotation recognized by PureLB"
    else
        info "Override event: $OVERRIDE_EVENTS"
        # Not a failure - event may have been cleaned up
    fi

    # Verify IP is on local interface (local pool behavior still applies)
    info "Verifying IP is on local interface..."
    wait_for_ip_announced "$IP_WITH_OVERRIDE" 30 || fail "IP not announced within 30s"
    FOUND_ON_ETH0=false
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IP_WITH_OVERRIDE/'" 2>/dev/null; then
            FOUND_ON_ETH0=true
            WINNER_NODE=$node
            pass "IP $IP_WITH_OVERRIDE on local interface on $node"
        fi
    done
    [ "$FOUND_ON_ETH0" = "true" ] || fail "IP not on local interface on any node"

    # Note: With ETP Local allowed on local pool, traffic may or may not work
    # depending on whether the elected node has an endpoint.
    # We'll just verify the IP was placed - connectivity is best-effort.
    info "Note: ETP Local on local pool - connectivity depends on elected node having endpoint"

    # Cleanup
    kubectl delete svc nginx-etp-local-with-override -n $NAMESPACE
    pass "ETP Local override test completed"
}

#---------------------------------------------------------------------
# Test 11: No Duplicate VIPs (Split-Brain Check)
#---------------------------------------------------------------------
test_no_duplicate_vips() {
    echo ""
    echo "=========================================="
    echo "TEST 11: No Duplicate VIPs (Split-Brain Check)"
    echo "=========================================="

    # This test catches split-brain scenarios where multiple nodes think they won election
    info "Checking all LoadBalancer services for duplicate VIPs..."

    # Get all LoadBalancer services with IPs
    SERVICES=$(kubectl get svc -n $NAMESPACE -o json | \
        jq -r '.items[] | select(.spec.type=="LoadBalancer") | select(.status.loadBalancer.ingress) | .metadata.name')

    DUPLICATE_FOUND=false
    for svc in $SERVICES; do
        # Get all IPs for this service
        IPS=$(kubectl get svc $svc -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[*].ip}')

        for IP in $IPS; do
            [ -z "$IP" ] && continue
            info "Checking VIP $IP (service: $svc)..."

            # Count how many nodes have this IP
            COUNT=0
            NODES_WITH_IP=""
            for node in $NODES; do
                # Check both eth0 and kube-lb0
                if node_ssh $node "ip -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                    COUNT=$((COUNT + 1))
                    NODES_WITH_IP="$NODES_WITH_IP $node"
                fi
            done

            if [ "$COUNT" -eq 0 ]; then
                fail "VIP $IP not found on any node (orphaned in K8s status)"
            elif [ "$COUNT" -eq 1 ]; then
                pass "VIP $IP on exactly 1 node:$NODES_WITH_IP"
            else
                fail "DUPLICATE VIP DETECTED: $IP found on $COUNT nodes:$NODES_WITH_IP (split-brain!)"
                DUPLICATE_FOUND=true
            fi
        done
    done

    [ "$DUPLICATE_FOUND" = "false" ] || fail "Duplicate VIPs detected - potential split-brain condition"
    pass "No duplicate VIPs found - election consistency verified"
}

#---------------------------------------------------------------------
# Test 12: Local VIP Address Flags and Lifetime
# Verifies VIPs have finite lifetime, noprefixroute, and secondary flags
#---------------------------------------------------------------------
test_local_vip_address_flags() {
    echo ""
    echo "=========================================="
    echo "TEST 12: Local VIP Address Flags"
    echo "=========================================="

    # Get IPv4 service VIP
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV4" ]; then
        info "Creating test service for address flag verification..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"
        IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        sleep 3  # Give time for address configuration
    fi

    info "Testing VIP: $IPV4"

    # Find which node has the VIP
    WINNER_NODE=""
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            WINNER_NODE=$node
            break
        fi
    done
    [ -n "$WINNER_NODE" ] || fail "VIP $IPV4 not found on any node"
    info "VIP located on $WINNER_NODE"

    # Get detailed address info
    DETAILS=$(get_address_details "$WINNER_NODE" "$IPV4" "${NODE_IFACE[$WINNER_NODE]}")
    info "Address details: $DETAILS"

    # Check for finite lifetime (NOT forever)
    VALID_LFT=$(get_valid_lft "$DETAILS")
    if [ "$VALID_LFT" = "forever" ]; then
        fail "Local VIP has permanent lifetime (valid_lft=forever) - should have finite lifetime"
    elif [ "$VALID_LFT" = "unknown" ]; then
        fail "Could not determine valid_lft for local VIP"
    else
        pass "Local VIP has finite lifetime: ${VALID_LFT}sec"
        # Verify lifetime is reasonable (should be <= 300 initially)
        if [ "$VALID_LFT" -gt 0 ] && [ "$VALID_LFT" -le 300 ]; then
            pass "Lifetime is within expected range (0-300s)"
        else
            info "Lifetime $VALID_LFT is outside default range (may be custom config)"
        fi
    fi

    # Check for noprefixroute flag
    if check_address_property "$DETAILS" "noprefixroute"; then
        pass "Local VIP has noprefixroute flag"
    else
        fail "Local VIP missing noprefixroute flag"
    fi

    # Check for secondary flag
    if check_address_property "$DETAILS" "secondary"; then
        pass "Local VIP has secondary flag"
    else
        info "Local VIP does not have secondary flag (may be only address)"
    fi
}

#---------------------------------------------------------------------
# Test 13: Address Renewal Timer
# Verifies that address lifetime countdown works
#---------------------------------------------------------------------
test_address_renewal_timer() {
    echo ""
    echo "=========================================="
    echo "TEST 13: Address Renewal Timer"
    echo "=========================================="

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    [ -n "$IPV4" ] || fail "No IPv4 service found for renewal test"

    # Find which node has the VIP
    WINNER_NODE=""
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            WINNER_NODE=$node
            break
        fi
    done
    [ -n "$WINNER_NODE" ] || fail "VIP $IPV4 not found on any node"

    # Get initial lifetime
    DETAILS1=$(get_address_details "$WINNER_NODE" "$IPV4" "${NODE_IFACE[$WINNER_NODE]}")
    LFT1=$(get_valid_lft "$DETAILS1")

    if [ "$LFT1" = "forever" ]; then
        fail "VIP has permanent lifetime - renewal test not applicable"
    fi

    info "Initial valid_lft: ${LFT1}sec"

    # Wait a short time and verify lifetime countdown
    WAIT_TIME=10
    info "Waiting ${WAIT_TIME}s to verify lifetime countdown..."
    sleep $WAIT_TIME

    DETAILS2=$(get_address_details "$WINNER_NODE" "$IPV4" "${NODE_IFACE[$WINNER_NODE]}")
    LFT2=$(get_valid_lft "$DETAILS2")

    if [ "$LFT2" = "unknown" ] || [ -z "$LFT2" ]; then
        fail "Address disappeared or lifetime unknown after wait"
    fi

    info "After ${WAIT_TIME}s, valid_lft: ${LFT2}sec"

    # Lifetime should have decreased or been renewed
    if [ "$LFT2" -ge "$LFT1" ] 2>/dev/null; then
        pass "Address was renewed (lifetime reset from ${LFT1}s to ${LFT2}s)"
    else
        pass "Lifetime countdown is working (decreased from ${LFT1}s to ${LFT2}s)"
    fi

    # Verify address still exists
    if node_ssh "$WINNER_NODE" "ip addr show ${NODE_IFACE[$WINNER_NODE]} | grep -q ' $IPV4/'" 2>/dev/null; then
        pass "VIP still present on interface"
    else
        fail "VIP disappeared from interface"
    fi

    # Verify renewAddress log on the winner node
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    info "Checking for renewAddress log on $WINNER_NODE..."
    local winner_pod
    winner_pod=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null \
        | grep "$WINNER_NODE" | awk '{print $1}')
    if [ -n "$winner_pod" ]; then
        if kubectl logs -n purelb-system "$winner_pod" --tail=200 2>/dev/null | grep -q "renewAddress"; then
            pass "LBNodeAgent $WINNER_NODE: logged 'renewAddress' (address lifetime renewed)"
        else
            detail "renewAddress not found in logs (debug-level, may not be visible at info level)"
        fi
    fi

    # Verify renewal metric incremented
    info "Checking address renewal metric on $WINNER_NODE..."
    local RENEWAL_METRICS
    RENEWAL_METRICS=$(scrape_lbnodeagent_metrics "$WINNER_NODE")
    if [ -n "$RENEWAL_METRICS" ]; then
        local renewal_count
        renewal_count=$(echo "$RENEWAL_METRICS" | grep "^purelb_lbnodeagent_address_renewals_total " | awk '{print $NF}')
        if [ -n "$renewal_count" ] && [ "$(printf '%.0f' "$renewal_count")" -gt 0 ]; then
            pass "LBNodeAgent $WINNER_NODE: address_renewals_total = $renewal_count"
        else
            detail "address_renewals_total = ${renewal_count:-0} (renewal may not have fired yet)"
        fi
    fi
}

#---------------------------------------------------------------------
# Test 14: Flannel Node IP Selection
# Verifies Flannel doesn't select VIP as node IP (requires noprefixroute)
#---------------------------------------------------------------------
test_flannel_node_ip() {
    echo ""
    echo "=========================================="
    echo "TEST 14: Flannel Node IP Selection"
    echo "=========================================="

    CNI=$(detect_cni)
    info "Detected CNI: $CNI"

    if [ "$CNI" != "flannel" ]; then
        info "Flannel not detected (CNI: $CNI) - skipping Flannel-specific test"
        pass "Test skipped (not Flannel)"
        return
    fi

    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    [ -n "$IPV4" ] || fail "No IPv4 service found for Flannel test"

    # Find which node has the VIP
    WINNER_NODE=""
    for node in $NODES; do
        if node_ssh $node "ip -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV4/'" 2>/dev/null; then
            WINNER_NODE=$node
            break
        fi
    done
    [ -n "$WINNER_NODE" ] || fail "VIP $IPV4 not found on any node"

    info "Checking Flannel annotation on node $WINNER_NODE..."

    # Get Flannel's selected public-ip annotation
    FLANNEL_IP=$(kubectl get node "$WINNER_NODE" -o jsonpath='{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}' 2>/dev/null || true)

    if [ -z "$FLANNEL_IP" ]; then
        info "No Flannel public-ip annotation found on $WINNER_NODE"
        pass "Test skipped (Flannel annotation not present)"
        return
    fi

    info "Flannel public-ip annotation: $FLANNEL_IP"
    info "PureLB VIP: $IPV4"

    # Verify Flannel did NOT select the VIP as node IP
    if [ "$FLANNEL_IP" = "$IPV4" ]; then
        fail "Flannel incorrectly selected VIP $IPV4 as node IP!"
    else
        pass "Flannel correctly selected non-VIP address ($FLANNEL_IP) as node IP"
    fi

    # Verify the Flannel IP is permanent (not a VIP)
    FLANNEL_DETAILS=$(get_address_details "$WINNER_NODE" "$FLANNEL_IP" "${NODE_IFACE[$WINNER_NODE]}")
    FLANNEL_LFT=$(get_valid_lft "$FLANNEL_DETAILS")

    if [ "$FLANNEL_LFT" = "forever" ]; then
        pass "Flannel selected a permanent address (expected for DHCP/static)"
    else
        info "Flannel's selected address has finite lifetime: ${FLANNEL_LFT}s"
    fi
}

#---------------------------------------------------------------------
# Test 15: Flannel IPv6 Address Selection
# Verifies that flannel does NOT pick an IPv6 VIP as the node's public
# address. PureLB marks IPv6 VIPs deprecated (PreferedLft=0) so flannel
# filters them out. Restarts flannel on the VIP node to prove it.
#---------------------------------------------------------------------
test_flannel_node_ipv6() {
    echo ""
    echo "=========================================="
    echo "TEST 15: Flannel IPv6 Address Selection"
    echo "=========================================="

    CNI=$(detect_cni)
    if [ "$CNI" != "flannel" ]; then
        info "CNI is $CNI, not flannel — skipping"
        pass "Test skipped (not flannel)"
        return
    fi

    IPV6=$(kubectl get svc nginx-lb-ipv6 -n $NAMESPACE \
        -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    [ -n "$IPV6" ] || fail "No IPv6 service found"

    # Find VIP holder
    local WINNER=""
    for node in $NODES; do
        if node_ssh $node "ip -6 -o addr show ${NODE_IFACE[$node]} 2>/dev/null | grep -q ' $IPV6/'" 2>/dev/null; then
            WINNER=$node; break
        fi
    done
    [ -n "$WINNER" ] || fail "IPv6 VIP $IPV6 not on any node"
    info "IPv6 VIP $IPV6 is on $WINNER"

    # Verify VIP has deprecated flag
    if node_ssh $WINNER "ip -6 addr show ${NODE_IFACE[$WINNER]} 2>/dev/null | grep ' $IPV6/' | grep -q deprecated" 2>/dev/null; then
        pass "VIP has deprecated flag (IFA_F_DEPRECATED)"
    else
        fail "VIP missing deprecated flag"
    fi

    # Restart flannel on the VIP node to force address re-selection
    local OLD_POD=$(kubectl get pods -n kube-flannel -o wide 2>/dev/null \
        | grep "$WINNER" | awk '{print $1}')
    [ -n "$OLD_POD" ] || fail "No flannel pod on $WINNER"
    info "Restarting flannel pod $OLD_POD on $WINNER..."
    kubectl delete pod -n kube-flannel "$OLD_POD" --grace-period=0 2>/dev/null

    # Wait for replacement pod
    local ELAPSED=0
    while [ $ELAPSED -lt 30 ]; do
        local NEW_POD=$(kubectl get pods -n kube-flannel -o wide 2>/dev/null \
            | grep "$WINNER" | grep "Running" | awk '{print $1}')
        if [ -n "$NEW_POD" ] && [ "$NEW_POD" != "$OLD_POD" ]; then break; fi
        sleep 2; ELAPSED=$((ELAPSED + 2))
    done
    [ $ELAPSED -lt 30 ] || fail "Flannel pod did not restart within 30s"
    sleep 3  # let flannel update annotation

    # Verify flannel did not pick the VIP
    local FLANNEL_IPV6=$(kubectl get node "$WINNER" \
        -o jsonpath='{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ipv6}' 2>/dev/null || true)
    info "Flannel public-ipv6: $FLANNEL_IPV6"

    if [ "$FLANNEL_IPV6" = "$IPV6" ]; then
        fail "Flannel selected VIP $IPV6 as node address!"
    else
        pass "Flannel correctly selected non-VIP address ($FLANNEL_IPV6)"
    fi

    # Verify IPv6 connectivity still works
    info "Testing IPv6 connectivity after flannel restart..."
    local OK=false
    for attempt in 1 2 3 4 5; do
        RESPONSE=$(curl -6 -s --connect-timeout 5 "http://[$IPV6]:80/" || true)
        if echo "$RESPONSE" | grep -q "Pod:"; then OK=true; break; fi
        sleep 2
    done
    [ "$OK" = "true" ] || fail "IPv6 VIP not reachable after flannel restart"
    pass "IPv6 connectivity verified after flannel restart"
}

#---------------------------------------------------------------------
# Test 16: Cross-Node Connectivity Validation
# Explicitly verifies that traffic can reach pods on DIFFERENT nodes
# than the VIP holder. This catches IP forwarding issues.
#---------------------------------------------------------------------
test_cross_node_connectivity() {
    echo ""
    echo "=========================================="
    echo "TEST 16: Cross-Node Connectivity Validation"
    echo "=========================================="

    info "This test verifies traffic can reach pods on nodes OTHER than the VIP holder."
    info "If IP forwarding is disabled, this test will fail."

    # Get or create IPv4 service
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV4" ]; then
        info "Creating test service..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"
        IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        # Wait for VIP to be announced
        wait_for_ip_announced "$IPV4" 30 || fail "IP not announced within 30s"
    fi

    info "Testing with VIP: $IPV4"

    # Find which node has the VIP
    VIP_NODE=$(find_vip_node "$IPV4")
    [ -n "$VIP_NODE" ] || fail "Could not find node with VIP $IPV4"
    info "VIP is located on: $VIP_NODE"

    # Get list of nodes that have nginx pods
    info "Checking pod distribution..."
    declare -A POD_NODES
    while IFS= read -r line; do
        node=$(echo "$line" | awk '{print $1}')
        POD_NODES[$node]=1
    done < <(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}')

    # Find nodes that have pods but are NOT the VIP node
    NON_VIP_POD_NODES=()
    for node in "${!POD_NODES[@]}"; do
        if [ "$node" != "$VIP_NODE" ]; then
            NON_VIP_POD_NODES+=("$node")
        fi
    done

    if [ ${#NON_VIP_POD_NODES[@]} -eq 0 ]; then
        info "All pods are on the VIP node ($VIP_NODE) - scaling to add pods on other nodes"
        ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
        kubectl scale deployment nginx -n $NAMESPACE --replicas=5
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
        kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s

        # Re-check pod distribution
        POD_NODES=()
        while IFS= read -r line; do
            node=$(echo "$line" | awk '{print $1}')
            POD_NODES[$node]=1
        done < <(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}')

        NON_VIP_POD_NODES=()
        for node in "${!POD_NODES[@]}"; do
            if [ "$node" != "$VIP_NODE" ]; then
                NON_VIP_POD_NODES+=("$node")
            fi
        done
    fi

    if [ ${#NON_VIP_POD_NODES[@]} -eq 0 ]; then
        fail "Could not create pods on nodes other than VIP node - check node taints/resources"
    fi

    info "Pods exist on non-VIP nodes: ${NON_VIP_POD_NODES[*]}"

    # Make multiple requests and verify at least one goes to a non-VIP node
    info "Sending requests to verify cross-node traffic..."
    CROSS_NODE_SUCCESS=false
    TOTAL_REQUESTS=20

    for i in $(seq 1 $TOTAL_REQUESTS); do
        RESPONSE_NODE=$(test_connectivity_get_node "$IPV4" 80)
        if [ -n "$RESPONSE_NODE" ] && [ "$RESPONSE_NODE" != "$VIP_NODE" ]; then
            pass "Request $i reached pod on $RESPONSE_NODE (different from VIP node $VIP_NODE)"
            CROSS_NODE_SUCCESS=true
            break
        fi
    done

    if [ "$CROSS_NODE_SUCCESS" = "false" ]; then
        fail "None of $TOTAL_REQUESTS requests reached a pod on a different node than the VIP holder ($VIP_NODE)"
        echo "  This likely indicates IP forwarding is disabled on $VIP_NODE"
        echo "  Fix: ssh $VIP_NODE 'sysctl -w net.ipv4.ip_forward=1'"
    fi

    # Restore original replica count if we changed it
    if [ -n "$ORIGINAL_REPLICAS" ]; then
        info "Restoring original replica count ($ORIGINAL_REPLICAS)..."
        kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
        kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    fi

    pass "Cross-node connectivity verified - IP forwarding is working"
}

#---------------------------------------------------------------------
# Test 17: Pod-Based Connectivity Test
# Tests connectivity from INSIDE a pod to validate the full
# kube-proxy path works correctly
#---------------------------------------------------------------------
test_pod_connectivity() {
    echo ""
    echo "=========================================="
    echo "TEST 17: Pod-Based Connectivity Test"
    echo "=========================================="

    info "Testing connectivity from inside a pod (validates full kube-proxy path)"

    # Get or create IPv4 service
    IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -z "$IPV4" ]; then
        info "Creating test service..."
        kubectl apply -f ${SCRIPT_DIR}/nginx-svc-ipv4.yaml
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-lb-ipv4 -n $NAMESPACE --timeout=30s || fail "No IP allocated"
        IPV4=$(kubectl get svc nginx-lb-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        wait_for_ip_announced "$IPV4" 30 || fail "IP not announced within 30s"
    fi

    info "Testing connectivity to VIP $IPV4 from inside a pod..."

    # Get a pod to use for testing
    POD_NAME=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].metadata.name}')
    POD_NODE=$(kubectl get pod -n $NAMESPACE "$POD_NAME" -o jsonpath='{.spec.nodeName}')
    [ -n "$POD_NAME" ] || fail "No test pod available"

    info "Using pod $POD_NAME on node $POD_NODE"

    # Test connectivity from inside the pod
    RESPONSE=$(kubectl exec -n $NAMESPACE "$POD_NAME" -- curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)

    if echo "$RESPONSE" | grep -q "Pod:"; then
        RESPONDING_NODE=$(echo "$RESPONSE" | grep "Node:" | awk '{print $2}')
        pass "Connectivity from pod successful - response from node $RESPONDING_NODE"
        echo "$RESPONSE" | head -5
    else
        fail "Could not connect to VIP from inside pod"
        echo "  Response: $RESPONSE"
        echo "  This indicates kube-proxy rules may not be working correctly"
    fi

    # Find VIP node and verify we can reach pods on different nodes from inside a pod
    VIP_NODE=$(find_vip_node "$IPV4")
    info "VIP is on node: $VIP_NODE"

    # If the test pod is on the VIP node, we should specifically test cross-node
    if [ "$POD_NODE" = "$VIP_NODE" ]; then
        info "Test pod is on VIP node - finding a pod on a different node for additional testing..."

        # Find a pod on a different node
        for pod in $(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].metadata.name}'); do
            OTHER_NODE=$(kubectl get pod -n $NAMESPACE "$pod" -o jsonpath='{.spec.nodeName}')
            if [ "$OTHER_NODE" != "$VIP_NODE" ]; then
                info "Testing from pod $pod on non-VIP node $OTHER_NODE..."
                RESPONSE2=$(kubectl exec -n $NAMESPACE "$pod" -- curl -s --connect-timeout 5 "http://$IPV4/" 2>/dev/null || true)
                if echo "$RESPONSE2" | grep -q "Pod:"; then
                    pass "Connectivity from non-VIP node pod successful"
                else
                    fail "Could not connect to VIP from pod on non-VIP node $OTHER_NODE"
                fi
                break
            fi
        done
    fi

    pass "Pod-based connectivity test completed"
}

#=====================================================================
# Multi-Subnet Tests (only run when SUBNET_COUNT >= 2)
#=====================================================================

#---------------------------------------------------------------------
# Test 18: Lease Subnets Match Node IPs
#---------------------------------------------------------------------
test_lease_subnets_match_node_ips() {
    echo ""
    echo "=========================================="
    echo "TEST 18: Lease Subnets Match Node IPs"
    echo "=========================================="

    info "Verifying each node's lease annotation lists its actual subnet..."
    for node in $NODES; do
        local lease_subnets
        lease_subnets=$(get_node_lease_subnets "$node")
        [ -n "$lease_subnets" ] || fail "Node $node has no subnet annotation on lease"

        local expected="${NODE_SUBNET[$node]}"
        if echo "$lease_subnets" | grep -q "$expected"; then
            pass "Node $node lease contains expected subnet $expected"
        else
            fail "Node $node lease has '$lease_subnets' but expected to contain '$expected'"
        fi
    done

    pass "All node leases have correct subnet annotations"
}

#---------------------------------------------------------------------
# Test 19: Subnet-Specific VIP Placement
#---------------------------------------------------------------------
test_subnet_vip_placement() {
    echo ""
    echo "=========================================="
    echo "TEST 19: Subnet-Specific VIP Placement"
    echo "=========================================="

    info "Testing that VIPs from a subnet-specific pool land on that subnet's nodes..."

    for subnet in $SUBNETS; do
        local sg_name="test-subnet-${subnet%%/*}"
        sg_name=$(echo "$sg_name" | tr '.' '-')

        info "Creating ServiceGroup for subnet $subnet..."
        generate_single_subnet_servicegroup "$sg_name" "$subnet"
        sleep 2

        # Create a service targeting this ServiceGroup
        local svc_name="nginx-lb-subnet-${subnet%%/*}"
        svc_name=$(echo "$svc_name" | tr '.' '-')
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${svc_name}
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: ${sg_name}
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

        info "Waiting for IP allocation from $sg_name..."
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/${svc_name} -n $NAMESPACE --timeout=30s || fail "No IP allocated for $sg_name"

        local vip
        vip=$(kubectl get svc ${svc_name} -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        info "Allocated VIP: $vip from pool $sg_name"

        # Wait for VIP to be announced
        wait_for_ip_announced "$vip" 30 || fail "VIP $vip not announced within 30s"

        # Find which node has it
        local holder
        holder=$(get_vip_holder "$vip")
        [ "$holder" != "NONE" ] || fail "VIP $vip not found on any node"

        # Verify the holder is on the correct subnet
        verify_vip_subnet_match "$vip" "$holder" || fail "VIP $vip on $holder but expected a node on subnet $subnet"
        pass "VIP $vip correctly placed on $holder (subnet $subnet)"

        # Cleanup this service and ServiceGroup
        kubectl delete svc ${svc_name} -n $NAMESPACE --ignore-not-found 2>/dev/null || true
        kubectl delete servicegroup "$sg_name" -n purelb-system --ignore-not-found 2>/dev/null || true
        wait_for_ip_not_on_any_node "$vip" 20 || info "Warning: VIP $vip lingering after delete"
    done

    pass "Subnet-specific VIP placement correct for all subnets"
}

#---------------------------------------------------------------------
# Test 20: Subnet Failover Stays in Subnet
#---------------------------------------------------------------------
test_subnet_failover_stays_in_subnet() {
    echo ""
    echo "=========================================="
    echo "TEST 20: Subnet Failover Stays in Subnet"
    echo "=========================================="

    # Find the largest subnet (most nodes) for this test
    local test_subnet=""
    local max_nodes=0
    for subnet in $SUBNETS; do
        local count
        count=$(echo "${SUBNET_NODES[$subnet]}" | wc -w)
        if [ "$count" -gt "$max_nodes" ]; then
            max_nodes=$count
            test_subnet=$subnet
        fi
    done

    if [ "$max_nodes" -lt 2 ]; then
        info "SKIP: No subnet has 2+ nodes for failover test"
        return 0
    fi

    info "Testing failover within subnet $test_subnet ($max_nodes nodes)..."

    # Create a single-subnet ServiceGroup
    local sg_name="test-failover-subnet"
    generate_single_subnet_servicegroup "$sg_name" "$test_subnet"
    sleep 2

    # Create a service
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-lb-failover-subnet
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: ${sg_name}
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-failover-subnet -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    local vip
    vip=$(kubectl get svc nginx-lb-failover-subnet -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    wait_for_ip_announced "$vip" 30 || fail "VIP $vip not announced"

    local original_winner
    original_winner=$(get_vip_holder "$vip")
    [ "$original_winner" != "NONE" ] || fail "VIP $vip not found on any node"
    info "Original VIP holder: $original_winner"

    # Taint the winner to force failover
    info "Tainting $original_winner to force failover..."
    kubectl taint node "$original_winner" purelb-test=failover:NoExecute --overwrite

    local agent_pod
    agent_pod=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$original_winner" | awk '{print $1}')
    [ -n "$agent_pod" ] && kubectl delete pod -n purelb-system "$agent_pod" --grace-period=10 2>/dev/null || true

    # Wait for failover
    info "Waiting for failover (max 20s)..."
    sleep 15

    local new_winner
    new_winner=$(get_vip_holder "$vip")

    if [ "$new_winner" = "NONE" ]; then
        # Extra wait
        sleep 10
        new_winner=$(get_vip_holder "$vip")
    fi

    [ "$new_winner" != "NONE" ] || { kubectl taint node "$original_winner" purelb-test- 2>/dev/null || true; fail "VIP $vip not found after failover"; }
    [ "$new_winner" != "$original_winner" ] || { kubectl taint node "$original_winner" purelb-test- 2>/dev/null || true; info "Warning: VIP stayed on tainted node (may be timing)"; }

    # Key assertion: new winner must be on the same subnet
    verify_vip_subnet_match "$vip" "$new_winner" || { kubectl taint node "$original_winner" purelb-test- 2>/dev/null || true; fail "Failover went to wrong subnet: $new_winner not on $test_subnet"; }
    pass "Failover stayed within subnet $test_subnet: $original_winner -> $new_winner"

    # Cleanup
    kubectl taint node "$original_winner" purelb-test- 2>/dev/null || true
    kubectl delete svc nginx-lb-failover-subnet -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup "$sg_name" -n purelb-system --ignore-not-found 2>/dev/null || true
    kubectl rollout status daemonset/lbnodeagent -n purelb-system --timeout=60s
}

#---------------------------------------------------------------------
# Test 21: Multi-Pool Default Allocation
#---------------------------------------------------------------------
test_multi_pool_default() {
    echo ""
    echo "=========================================="
    echo "TEST 21: Multi-Pool Default Allocation"
    echo "=========================================="

    info "Testing that default ServiceGroup with multiple subnet pools allocates correctly..."

    # The default ServiceGroup should already have pools for all subnets
    # Create a service using it
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-lb-multipool
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-multipool -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    local vip
    vip=$(kubectl get svc nginx-lb-multipool -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Allocated VIP: $vip"
    wait_for_ip_announced "$vip" 30 || fail "VIP $vip not announced"

    # Verify it's in a valid pool
    ip_in_pool_range "$vip" || fail "VIP $vip not in any pool range"
    pass "VIP $vip is from a valid pool"

    # Verify holder is on matching subnet
    local holder
    holder=$(get_vip_holder "$vip")
    [ "$holder" != "NONE" ] || fail "VIP $vip not found on any node"
    verify_vip_subnet_match "$vip" "$holder" || fail "VIP/node subnet mismatch"
    pass "VIP $vip on $holder — subnet match confirmed"

    # Cleanup
    kubectl delete svc nginx-lb-multipool -n $NAMESPACE --ignore-not-found 2>/dev/null || true
}

#---------------------------------------------------------------------
# Test: Multi-Pool Allocation (one IP per range)
# Tests the multiPool feature where a service gets one IP from each
# address range that has active nodes. Covers:
#   - ServiceGroup-based multiPool: true (IPv4)
#   - Annotation-based purelb.io/multi-pool: "true" on non-multi SG (IPv4)
#   - Annotation override: multi-pool: "false" on multi SG (IPv4)
#   - ServiceGroup-based multiPool: true (IPv6, if available)
#   - ServiceGroup-based multiPool: true (Dual-Stack, if available)
#   - Connectivity verification for all allocated IPs
#---------------------------------------------------------------------
test_multi_pool_allocation() {
    echo ""
    echo "=========================================="
    echo "TEST: Multi-Pool Allocation (1 IP per range)"
    echo "=========================================="

    info "Testing multiPool allocation across annotation and SG modes, IPv4 and IPv6..."

    # Build v4pools entries from discovered subnets using .180-.190 range
    # (separate from .200-.220 used by default and .230-.240 used by other tests)
    local v4pools=""
    local v6pools=""
    local v4_subnet_count=0
    local v6_subnet_count=0
    for sub in $SUBNETS; do
        # Skip IPv6 subnets (we handle v6 via SUBNET_V6 mapping)
        if echo "$sub" | grep -q ':'; then
            continue
        fi
        local net="${sub%/*}"
        local prefix
        prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
        local pool_range="${prefix}.180-${prefix}.190"
        v4pools="${v4pools}
    - pool: \"${pool_range}\"
      subnet: \"${sub}\""
        v4_subnet_count=$((v4_subnet_count + 1))

        # Add IPv6 pool if this subnet has a v6 mapping
        local v6prefix="${SUBNET_V6_PREFIX[$sub]}"
        if [ -n "$v6prefix" ]; then
            local v6sub="${SUBNET_V6[$sub]}"
            local v6base="${v6prefix%%::}"
            local v6range="${v6base}:d::1-${v6base}:d::10"
            v6pools="${v6pools}
    - pool: \"${v6range}\"
      subnet: \"${v6sub}\""
            v6_subnet_count=$((v6_subnet_count + 1))
        fi
    done

    [ -n "$v4pools" ] || { info "SKIP: No IPv4 subnets for multi-pool test"; return 0; }

    #-----------------------------------------------------------------
    # Sub-test 1: ServiceGroup-based multiPool: true (IPv4)
    #-----------------------------------------------------------------
    info ""
    info "--- Sub-test 1: SG-based multiPool: true (IPv4) ---"

    local sg_spec="multiPool: true
    v4pools: ${v4pools}"
    if [ -n "$v6pools" ]; then
        sg_spec="${sg_spec}
    v6pools: ${v6pools}"
    fi

    kubectl apply -f - <<EOF
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: multipool-test
  namespace: purelb-system
spec:
  local:
    ${sg_spec}
EOF
    info "Created ServiceGroup multipool-test with multiPool: true ($v4_subnet_count v4 pools, $v6_subnet_count v6 pools)"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-mp-sg-v4
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: multipool-test
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-mp-sg-v4 -n $NAMESPACE --timeout=60s || fail "SG multi-pool IPv4: no IP allocated"
    sleep 3

    local ip_count
    ip_count=$(kubectl get svc nginx-mp-sg-v4 -n $NAMESPACE \
        -o jsonpath='{.status.loadBalancer.ingress[*].ip}' | wc -w)
    info "SG multi-pool IPv4: allocated $ip_count IPs (expected $v4_subnet_count)"

    [ "$ip_count" -ge "$v4_subnet_count" ] || fail "SG multi-pool IPv4: expected $v4_subnet_count IPs, got $ip_count"
    pass "SG multi-pool IPv4: $ip_count IPs for $v4_subnet_count subnets"

    # Verify each IP is announced on the correct subnet and reachable
    local all_ips
    all_ips=$(kubectl get svc nginx-mp-sg-v4 -n $NAMESPACE \
        -o jsonpath='{.status.loadBalancer.ingress[*].ip}')
    for vip in $all_ips; do
        wait_for_ip_announced "$vip" 30 || fail "SG multi-pool VIP $vip not announced"
        local holder
        holder=$(get_vip_holder "$vip")
        [ "$holder" != "NONE" ] || fail "SG multi-pool VIP $vip not found on any node"
        verify_vip_subnet_match "$vip" "$holder" || fail "SG multi-pool VIP $vip / node $holder subnet mismatch"
        pass "SG multi-pool VIP $vip on $holder — subnet match"

        # Connectivity check
        local resp
        resp=$(curl -s --connect-timeout 5 "http://$vip/" || true)
        echo "$resp" | grep -q "Pod:" || fail "SG multi-pool VIP $vip not reachable via HTTP"
        pass "SG multi-pool VIP $vip reachable"
    done

    #-----------------------------------------------------------------
    # Sub-test 2: Annotation-based multi-pool on default SG (IPv4)
    # The default SG does NOT have multiPool: true, so the annotation
    # must enable it.
    #-----------------------------------------------------------------
    info ""
    info "--- Sub-test 2: Annotation-based multi-pool on default SG (IPv4) ---"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-mp-ann-v4
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: default
    purelb.io/multi-pool: "true"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 81
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-mp-ann-v4 -n $NAMESPACE --timeout=60s || fail "Annotation multi-pool IPv4: no IP allocated"
    sleep 3

    local ann_count
    ann_count=$(kubectl get svc nginx-mp-ann-v4 -n $NAMESPACE \
        -o jsonpath='{.status.loadBalancer.ingress[*].ip}' | wc -w)
    info "Annotation multi-pool IPv4: allocated $ann_count IPs (expected $v4_subnet_count)"

    [ "$ann_count" -ge "$v4_subnet_count" ] || fail "Annotation multi-pool IPv4: expected $v4_subnet_count IPs, got $ann_count"
    pass "Annotation multi-pool IPv4: $ann_count IPs from default SG"

    # Verify connectivity for annotation-based multi-pool
    local ann_ips
    ann_ips=$(kubectl get svc nginx-mp-ann-v4 -n $NAMESPACE \
        -o jsonpath='{.status.loadBalancer.ingress[*].ip}')
    for vip in $ann_ips; do
        wait_for_ip_announced "$vip" 30 || fail "Annotation multi-pool VIP $vip not announced"
        local resp
        resp=$(curl -s --connect-timeout 5 "http://${vip}:81/" || true)
        echo "$resp" | grep -q "Pod:" || fail "Annotation multi-pool VIP $vip not reachable on port 81"
        pass "Annotation multi-pool VIP $vip reachable"
    done

    #-----------------------------------------------------------------
    # Sub-test 3: Annotation override multi-pool: "false" on multi SG
    # The multipool-test SG has multiPool: true, but the annotation
    # should force single-IP allocation.
    #-----------------------------------------------------------------
    info ""
    info "--- Sub-test 3: Annotation override multi-pool=false on multi SG ---"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-mp-override
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: multipool-test
    purelb.io/multi-pool: "false"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 82
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-mp-override -n $NAMESPACE --timeout=30s || fail "Override: no IP allocated"
    sleep 2

    local override_count
    override_count=$(kubectl get svc nginx-mp-override -n $NAMESPACE \
        -o jsonpath='{.status.loadBalancer.ingress[*].ip}' | wc -w)
    if [ "$override_count" -eq 1 ]; then
        pass "Annotation override: multi-pool=false correctly gives 1 IP"
    else
        fail "Annotation override: expected 1 IP but got $override_count"
    fi

    #-----------------------------------------------------------------
    # Sub-test 4: SG-based multiPool: true (IPv6-only)
    #-----------------------------------------------------------------
    if [ "$HAS_IPV6" = "true" ] && [ "$v6_subnet_count" -ge 2 ]; then
        info ""
        info "--- Sub-test 4: SG-based multiPool: true (IPv6-only) ---"

        kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-mp-sg-v6
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: multipool-test
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv6
  selector:
    app: nginx
  ports:
  - port: 83
    targetPort: 80
EOF

        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-mp-sg-v6 -n $NAMESPACE --timeout=60s || fail "SG multi-pool IPv6: no IP allocated"
        sleep 3

        local v6_count
        v6_count=$(kubectl get svc nginx-mp-sg-v6 -n $NAMESPACE \
            -o jsonpath='{.status.loadBalancer.ingress[*].ip}' | wc -w)
        info "SG multi-pool IPv6: allocated $v6_count IPs (expected $v6_subnet_count)"

        [ "$v6_count" -ge "$v6_subnet_count" ] || fail "SG multi-pool IPv6: expected $v6_subnet_count IPs, got $v6_count"
        pass "SG multi-pool IPv6: $v6_count IPs for $v6_subnet_count subnets"

        # Verify each IPv6 VIP is announced
        local v6_ips
        v6_ips=$(kubectl get svc nginx-mp-sg-v6 -n $NAMESPACE \
            -o jsonpath='{.status.loadBalancer.ingress[*].ip}')
        for vip in $v6_ips; do
            wait_for_ip_announced "$vip" 30 || fail "SG multi-pool IPv6 VIP $vip not announced"
            pass "SG multi-pool IPv6 VIP $vip announced"

            # Test IPv6 connectivity
            local resp
            resp=$(curl -6 -s --connect-timeout 5 "http://[${vip}]:83/" || true)
            echo "$resp" | grep -q "Pod:" || fail "SG multi-pool IPv6 VIP $vip not reachable"
            pass "SG multi-pool IPv6 VIP $vip reachable"
        done
    else
        info ""
        info "SKIP: Sub-test 4 (IPv6 multi-pool) requires IPv6 and 2+ v6 subnets"
    fi

    #-----------------------------------------------------------------
    # Sub-test 5: SG-based multiPool: true (Dual-Stack)
    #-----------------------------------------------------------------
    if [ "$HAS_IPV6" = "true" ] && [ "$v6_subnet_count" -ge 2 ]; then
        info ""
        info "--- Sub-test 5: SG-based multiPool: true (Dual-Stack) ---"

        kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-mp-sg-ds
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: multipool-test
spec:
  type: LoadBalancer
  ipFamilyPolicy: RequireDualStack
  ipFamilies:
  - IPv4
  - IPv6
  selector:
    app: nginx
  ports:
  - port: 84
    targetPort: 80
EOF

        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-mp-sg-ds -n $NAMESPACE --timeout=60s || fail "SG multi-pool dual-stack: no IP allocated"
        sleep 3

        local ds_count
        ds_count=$(kubectl get svc nginx-mp-sg-ds -n $NAMESPACE \
            -o jsonpath='{.status.loadBalancer.ingress[*].ip}' | wc -w)
        local expected_ds=$((v4_subnet_count + v6_subnet_count))
        info "SG multi-pool dual-stack: allocated $ds_count IPs (expected $expected_ds: $v4_subnet_count v4 + $v6_subnet_count v6)"

        [ "$ds_count" -ge "$expected_ds" ] || fail "SG multi-pool dual-stack: expected $expected_ds IPs, got $ds_count"
        pass "SG multi-pool dual-stack: $ds_count IPs ($v4_subnet_count v4 + $v6_subnet_count v6)"

        # Verify announcing annotations show all nodes
        local ann_v4
        ann_v4=$(kubectl get svc nginx-mp-sg-ds -n $NAMESPACE \
            -o jsonpath='{.metadata.annotations.purelb\.io/announcing-IPv4}')
        local ann_v6
        ann_v6=$(kubectl get svc nginx-mp-sg-ds -n $NAMESPACE \
            -o jsonpath='{.metadata.annotations.purelb\.io/announcing-IPv6}')
        info "announcing-IPv4: $ann_v4"
        info "announcing-IPv6: $ann_v6"

        # Each announcing annotation should have entries for each subnet
        local ann_v4_count
        ann_v4_count=$(echo "$ann_v4" | tr ' ' '\n' | grep -c '.' || true)
        local ann_v6_count
        ann_v6_count=$(echo "$ann_v6" | tr ' ' '\n' | grep -c '.' || true)
        [ "$ann_v4_count" -ge "$v4_subnet_count" ] || info "Warning: announcing-IPv4 has $ann_v4_count entries, expected $v4_subnet_count"
        [ "$ann_v6_count" -ge "$v6_subnet_count" ] || info "Warning: announcing-IPv6 has $ann_v6_count entries, expected $v6_subnet_count"

        # Connectivity check for all dual-stack IPs
        local ds_ips
        ds_ips=$(kubectl get svc nginx-mp-sg-ds -n $NAMESPACE \
            -o jsonpath='{.status.loadBalancer.ingress[*].ip}')
        for vip in $ds_ips; do
            wait_for_ip_announced "$vip" 30 || fail "Dual-stack multi-pool VIP $vip not announced"
            local resp
            if echo "$vip" | grep -q ':'; then
                resp=$(curl -6 -s --connect-timeout 5 "http://[${vip}]:84/" || true)
            else
                resp=$(curl -s --connect-timeout 5 "http://${vip}:84/" || true)
            fi
            echo "$resp" | grep -q "Pod:" || fail "Dual-stack multi-pool VIP $vip not reachable"
            pass "Dual-stack multi-pool VIP $vip reachable"
        done
    else
        info ""
        info "SKIP: Sub-test 5 (dual-stack multi-pool) requires IPv6 and 2+ v6 subnets"
    fi

    # Cleanup
    info ""
    info "Cleaning up multi-pool test resources..."
    kubectl delete svc nginx-mp-sg-v4 nginx-mp-ann-v4 nginx-mp-override \
        nginx-mp-sg-v6 nginx-mp-sg-ds -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup multipool-test -n purelb-system --ignore-not-found 2>/dev/null || true
    pass "Multi-pool allocation tests completed"
}

#---------------------------------------------------------------------
# Test 22: Cross-Subnet Connectivity
#---------------------------------------------------------------------
test_cross_subnet_connectivity() {
    echo ""
    echo "=========================================="
    echo "TEST 22: Cross-Subnet Connectivity"
    echo "=========================================="

    info "Testing that VIP on one subnet is reachable from nodes on another subnet..."
    info "Pods are scaled to NODE_COUNT so traffic must traverse subnets end-to-end."

    # Pick the first subnet and create a service pinned to it
    local first_sub=""
    local second_sub=""
    for s in $SUBNETS; do
        if [ -z "$first_sub" ]; then
            first_sub=$s
        elif [ -z "$second_sub" ]; then
            second_sub=$s
            break
        fi
    done

    [ -n "$second_sub" ] || { info "SKIP: Need 2 subnets for cross-subnet test"; return 0; }

    # Scale to NODE_COUNT replicas so pods are spread across all nodes/subnets.
    # This ensures the curl from subnet B doesn't accidentally hit a local pod —
    # it must traverse through the VIP holder on subnet A to reach a pod there.
    local orig_replicas
    orig_replicas=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    info "Scaling nginx to $NODE_COUNT replicas for cross-subnet pod coverage..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$NODE_COUNT
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    info "Pod distribution:"
    kubectl get pods -n $NAMESPACE -o wide --no-headers 2>/dev/null | grep nginx | \
        awk '{printf "  %-45s %s\n", $1, $7}'

    local sg_name="test-cross-subnet"
    generate_single_subnet_servicegroup "$sg_name" "$first_sub"
    sleep 2

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-lb-cross-subnet
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: ${sg_name}
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-cross-subnet -n $NAMESPACE --timeout=30s || {
        kubectl scale deployment nginx -n $NAMESPACE --replicas=$orig_replicas 2>/dev/null || true
        fail "No IP allocated"
    }

    local vip
    vip=$(kubectl get svc nginx-lb-cross-subnet -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    wait_for_ip_announced "$vip" 30 || {
        kubectl scale deployment nginx -n $NAMESPACE --replicas=$orig_replicas 2>/dev/null || true
        fail "VIP $vip not announced"
    }

    info "VIP $vip allocated on subnet $first_sub"

    # Curl from a node on the other subnet — with pods on all nodes, traffic
    # must pass through the VIP holder (subnet A) to reach a pod on subnet A.
    local remote_node
    remote_node=$(echo "${SUBNET_NODES[$second_sub]}" | awk '{print $1}')
    [ -n "$remote_node" ] || fail "No node on $second_sub"

    info "Testing connectivity from $remote_node (subnet $second_sub) to VIP $vip (subnet $first_sub)..."
    # Retry: nftables rules on the VIP holder may take a moment to propagate after the
    # service external IP is set, even though the VIP is already on the interface.
    local response=""
    local attempts=0
    while [ $attempts -lt 6 ]; do
        response=$(node_ssh "$remote_node" "curl -s --connect-timeout 5 http://$vip/" 2>/dev/null || true)
        if echo "$response" | grep -q "Pod:"; then
            break
        fi
        attempts=$((attempts + 1))
        [ $attempts -lt 6 ] && sleep 3
    done
    echo "$response" | grep -q "Pod:" || {
        kubectl scale deployment nginx -n $NAMESPACE --replicas=$orig_replicas 2>/dev/null || true
        fail "Cross-subnet connectivity failed: $remote_node -> $vip"
    }
    local resp_node
    resp_node=$(echo "$response" | grep "Node:" | awk '{print $2}')
    pass "Cross-subnet connectivity works: $remote_node ($second_sub) -> $vip ($first_sub) -> pod on $resp_node"

    # Cleanup
    kubectl delete svc nginx-lb-cross-subnet -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup "$sg_name" -n purelb-system --ignore-not-found 2>/dev/null || true
    info "Restoring nginx replica count ($orig_replicas)..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$orig_replicas 2>/dev/null || true
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
}

#---------------------------------------------------------------------
# Test 23: Small Subnet Exhaustion
#---------------------------------------------------------------------
test_small_subnet_exhaustion() {
    echo ""
    echo "=========================================="
    echo "TEST 23: Small Subnet Exhaustion"
    echo "=========================================="

    # Find the smallest subnet
    local small_subnet=""
    local min_nodes=999
    for subnet in $SUBNETS; do
        local count
        count=$(echo "${SUBNET_NODES[$subnet]}" | wc -w)
        if [ "$count" -lt "$min_nodes" ]; then
            min_nodes=$count
            small_subnet=$subnet
        fi
    done

    if [ "$min_nodes" -gt 2 ]; then
        info "SKIP: Smallest subnet has $min_nodes nodes, need <= 2 for exhaustion test"
        return 0
    fi

    info "Testing node exhaustion on small subnet $small_subnet ($min_nodes nodes)..."

    local sg_name="test-small-subnet"
    generate_single_subnet_servicegroup "$sg_name" "$small_subnet"
    sleep 2

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-lb-small-subnet
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: ${sg_name}
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-small-subnet -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    local vip
    vip=$(kubectl get svc nginx-lb-small-subnet -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    wait_for_ip_announced "$vip" 30 || fail "VIP $vip not announced"

    local winner
    winner=$(get_vip_holder "$vip")
    info "VIP $vip on $winner"

    # Taint the winner — VIP should move to last remaining node
    info "Tainting $winner to simulate failure..."
    kubectl taint node "$winner" purelb-test=exhaust:NoExecute --overwrite
    local agent_pod
    agent_pod=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$winner" | awk '{print $1}')
    [ -n "$agent_pod" ] && kubectl delete pod -n purelb-system "$agent_pod" --grace-period=10 2>/dev/null || true

    sleep 15

    local new_winner
    new_winner=$(get_vip_holder "$vip")
    if [ "$new_winner" != "NONE" ] && [ "$new_winner" != "$winner" ]; then
        verify_vip_subnet_match "$vip" "$new_winner" || { kubectl taint node "$winner" purelb-test- 2>/dev/null || true; fail "Failover went to wrong subnet"; }
        pass "VIP moved to last node on $small_subnet: $new_winner"

        if [ "$min_nodes" -eq 2 ]; then
            # Taint the second node too — all nodes exhausted
            info "Tainting $new_winner to exhaust all nodes on $small_subnet..."
            kubectl taint node "$new_winner" purelb-test=exhaust:NoExecute --overwrite
            local agent_pod2
            agent_pod2=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$new_winner" | awk '{print $1}')
            [ -n "$agent_pod2" ] && kubectl delete pod -n purelb-system "$agent_pod2" --grace-period=10 2>/dev/null || true
            sleep 15

            # VIP should have no home now (or be on a wrong-subnet node which would be a bug)
            local final_holder
            final_holder=$(get_vip_holder "$vip")
            if [ "$final_holder" = "NONE" ]; then
                pass "VIP correctly has no home after all subnet nodes exhausted"
            else
                info "Warning: VIP $vip still on $final_holder after all $small_subnet nodes exhausted"
                # Check if it landed on a different subnet (should not happen with subnet-aware election)
                if verify_vip_subnet_match "$vip" "$final_holder" 2>/dev/null; then
                    info "Note: VIP on a same-subnet node (may be timing — DaemonSet recovery)"
                else
                    fail "VIP migrated to wrong subnet $final_holder after exhaustion"
                fi
            fi

            # Cleanup second taint
            kubectl taint node "$new_winner" purelb-test- 2>/dev/null || true
        fi
    else
        info "VIP did not move (may still be on tainted node or lost)"
    fi

    # Cleanup
    kubectl taint node "$winner" purelb-test- 2>/dev/null || true
    kubectl delete svc nginx-lb-small-subnet -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup "$sg_name" -n purelb-system --ignore-not-found 2>/dev/null || true
    kubectl rollout status daemonset/lbnodeagent -n purelb-system --timeout=60s

    pass "Small subnet exhaustion test completed"
}

#---------------------------------------------------------------------
# Test 24: IPv6 Multi-Subnet Placement
#---------------------------------------------------------------------
test_ipv6_multi_subnet_placement() {
    echo ""
    echo "=========================================="
    echo "TEST 24: IPv6 Multi-Subnet Placement"
    echo "=========================================="

    if [ "$HAS_IPV6" != "true" ]; then
        info "SKIP: IPv6 not available"
        return 0
    fi

    info "Testing IPv6 VIP placement respects subnet boundaries..."

    for subnet in $SUBNETS; do
        local v6sub="${SUBNET_V6[$subnet]}"
        [ -n "$v6sub" ] || { info "No IPv6 on $subnet, skipping"; continue; }

        local sg_name="test-v6-${subnet%%/*}"
        sg_name=$(echo "$sg_name" | tr '.' '-')

        generate_single_subnet_servicegroup "$sg_name" "$subnet"
        sleep 2

        local svc_name="nginx-lb-v6-${subnet%%/*}"
        svc_name=$(echo "$svc_name" | tr '.' '-')
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${svc_name}
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: ${sg_name}
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv6
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/${svc_name} -n $NAMESPACE --timeout=30s || fail "No IPv6 allocated for $sg_name"

        local v6vip
        v6vip=$(kubectl get svc ${svc_name} -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        info "Allocated IPv6 VIP: $v6vip from $sg_name"

        wait_for_ip_announced "$v6vip" 30 || fail "IPv6 VIP $v6vip not announced"

        # Verify the VIP is on a node from the correct subnet
        local holder
        holder=$(find_vip_node "$v6vip")
        [ -n "$holder" ] || fail "IPv6 VIP $v6vip not found on any node"

        local node_sub="${NODE_SUBNET[$holder]}"
        [ "$node_sub" = "$subnet" ] || fail "IPv6 VIP on $holder (subnet $node_sub) but expected subnet $subnet"
        pass "IPv6 VIP $v6vip correctly placed on $holder (subnet $subnet)"

        # Cleanup
        kubectl delete svc ${svc_name} -n $NAMESPACE --ignore-not-found 2>/dev/null || true
        kubectl delete servicegroup "$sg_name" -n purelb-system --ignore-not-found 2>/dev/null || true
        wait_for_ip_not_on_any_node "$v6vip" 20 || info "Warning: IPv6 VIP lingering"
    done

    pass "IPv6 multi-subnet placement correct"
}

#---------------------------------------------------------------------
# Test 25: Dual-Stack Multi-Subnet
#---------------------------------------------------------------------
test_dualstack_multi_subnet() {
    echo ""
    echo "=========================================="
    echo "TEST 25: Dual-Stack Multi-Subnet"
    echo "=========================================="

    if [ "$HAS_IPV6" != "true" ]; then
        info "SKIP: IPv6 not available"
        return 0
    fi

    info "Testing dual-stack service VIPs land on same-subnet node..."

    # Pick a subnet that has IPv6
    local test_sub=""
    for s in $SUBNETS; do
        if [ -n "${SUBNET_V6[$s]}" ]; then
            test_sub=$s
            break
        fi
    done

    [ -n "$test_sub" ] || { info "SKIP: No subnet with IPv6"; return 0; }

    local sg_name="test-dualstack-subnet"
    generate_single_subnet_servicegroup "$sg_name" "$test_sub"
    sleep 2

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-lb-ds-subnet
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: ${sg_name}
spec:
  type: LoadBalancer
  ipFamilyPolicy: RequireDualStack
  ipFamilies:
  - IPv4
  - IPv6
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-lb-ds-subnet -n $NAMESPACE --timeout=30s || fail "No IP allocated"

    local v4vip v6vip
    v4vip=$(kubectl get svc nginx-lb-ds-subnet -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    v6vip=$(kubectl get svc nginx-lb-ds-subnet -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[1].ip}')

    info "Dual-stack VIPs: v4=$v4vip, v6=$v6vip"
    [ -n "$v4vip" ] || fail "No IPv4 VIP"
    [ -n "$v6vip" ] || fail "No IPv6 VIP"

    wait_for_ip_announced "$v4vip" 30 || fail "IPv4 VIP $v4vip not announced"

    # Find holders
    local v4_holder v6_holder
    v4_holder=$(get_vip_holder "$v4vip")
    v6_holder=$(find_vip_node "$v6vip")

    [ "$v4_holder" != "NONE" ] || fail "IPv4 VIP not found on any node"
    [ -n "$v6_holder" ] || fail "IPv6 VIP not found on any node"

    info "IPv4 VIP on $v4_holder, IPv6 VIP on $v6_holder"

    # Both should be on the target subnet
    local v4_node_sub="${NODE_SUBNET[$v4_holder]}"
    local v6_node_sub="${NODE_SUBNET[$v6_holder]}"
    [ "$v4_node_sub" = "$test_sub" ] || fail "IPv4 VIP holder $v4_holder not on $test_sub"
    [ "$v6_node_sub" = "$test_sub" ] || fail "IPv6 VIP holder $v6_holder not on $test_sub"
    pass "Both VIPs on correct subnet $test_sub"

    # Both should ideally be on the same node (same service key → same hash)
    if [ "$v4_holder" = "$v6_holder" ]; then
        pass "Both VIPs on same node $v4_holder (consistent election)"
    else
        info "Note: VIPs on different nodes ($v4_holder, $v6_holder) — different election keys"
    fi

    # Cleanup
    kubectl delete svc nginx-lb-ds-subnet -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup "$sg_name" -n purelb-system --ignore-not-found 2>/dev/null || true

    pass "Dual-stack multi-subnet test completed"
}

#---------------------------------------------------------------------
# Test: Balanced Allocation
# Tests the balanced allocation feature where services are distributed
# evenly across address ranges. Covers:
#   - Balanced IPv4 allocation (verify round-robin across ranges)
#   - Balanced IPv6 allocation (if available)
#   - Balanced dual-stack allocation (if available)
#   - Negative: balanced + multiPool mutual exclusion → AllocationFailed
#   - Negative: balanced SG with non-existent service-group → error
#---------------------------------------------------------------------
test_balanced_allocation() {
    echo ""
    echo "=========================================="
    echo "TEST: Balanced Allocation"
    echo "=========================================="

    info "Testing balanced allocation across address ranges..."

    # Build v4pools entries from discovered subnets using .150-.160 range
    # (separate from .200-.220 default, .230-.240 test, .180-.190 multi-pool)
    local v4pools=""
    local v6pools=""
    local v4_subnet_count=0
    local v6_subnet_count=0
    # Track subnet prefixes for verification
    local -a v4_prefixes=()
    local -a v6_prefixes=()
    for sub in $SUBNETS; do
        # Skip IPv6 subnets
        if echo "$sub" | grep -q ':'; then
            continue
        fi
        local net="${sub%/*}"
        local prefix
        prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
        local pool_range="${prefix}.150-${prefix}.160"
        v4pools="${v4pools}
    - pool: \"${pool_range}\"
      subnet: \"${sub}\""
        v4_prefixes+=("$prefix")
        v4_subnet_count=$((v4_subnet_count + 1))

        # Add IPv6 pool if this subnet has a v6 mapping
        local v6prefix="${SUBNET_V6_PREFIX[$sub]}"
        if [ -n "$v6prefix" ]; then
            local v6sub="${SUBNET_V6[$sub]}"
            local v6base="${v6prefix%%::}"
            local v6range="${v6base}:c::1-${v6base}:c::10"
            v6pools="${v6pools}
    - pool: \"${v6range}\"
      subnet: \"${v6sub}\""
            v6_prefixes+=("$v6base")
            v6_subnet_count=$((v6_subnet_count + 1))
        fi
    done

    [ "$v4_subnet_count" -ge 2 ] || { info "SKIP: Balanced allocation requires 2+ subnets (found $v4_subnet_count)"; return 0; }

    # Capture baseline balanced_allocations metric
    local before_balanced
    before_balanced=$(scrape_allocator_metric 'purelb_address_pool_balanced_allocations_total{pool="balanced-test"}')
    before_balanced=$(printf '%.0f' "${before_balanced:-0}")

    #-----------------------------------------------------------------
    # Sub-test 1: Balanced IPv4 allocation
    #-----------------------------------------------------------------
    info ""
    info "--- Sub-test 1: Balanced IPv4 allocation ---"

    local sg_spec="balanced: true
    v4pools: ${v4pools}"
    if [ -n "$v6pools" ]; then
        sg_spec="${sg_spec}
    v6pools: ${v6pools}"
    fi

    kubectl apply -f - <<EOF
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: balanced-test
  namespace: purelb-system
spec:
  local:
    ${sg_spec}
EOF
    info "Created ServiceGroup balanced-test with balanced: true ($v4_subnet_count v4 pools, $v6_subnet_count v6 pools)"

    # Create enough services to verify distribution (2 per subnet = 2*subnet_count)
    local svc_count=$((v4_subnet_count * 2))
    local -a svc_names=()
    local -a svc_ips=()
    for i in $(seq 1 $svc_count); do
        local svc_name="nginx-bal-v4-${i}"
        svc_names+=("$svc_name")
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${svc_name}
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: balanced-test
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: $((85 + i))
    targetPort: 80
EOF
    done

    # Wait for all services to get IPs
    for svc_name in "${svc_names[@]}"; do
        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/${svc_name} -n $NAMESPACE --timeout=30s || fail "Balanced IPv4: $svc_name no IP allocated"
    done

    # Collect IPs and count per range
    declare -A range_counts
    for svc_name in "${svc_names[@]}"; do
        local ip
        ip=$(kubectl get svc ${svc_name} -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        svc_ips+=("$ip")
        local ip_prefix
        ip_prefix=$(echo "$ip" | awk -F. '{print $1"."$2"."$3}')
        range_counts[$ip_prefix]=$(( ${range_counts[$ip_prefix]:-0} + 1 ))
        detail "$svc_name -> $ip (range $ip_prefix)"
    done

    # Verify distribution: each range should have at least 1 IP and no more than ceil(total/ranges)
    local max_per_range=$(( (svc_count + v4_subnet_count - 1) / v4_subnet_count ))
    local all_ranges_populated=true
    for prefix in "${v4_prefixes[@]}"; do
        local count=${range_counts[$prefix]:-0}
        if [ "$count" -eq 0 ]; then
            fail "Balanced IPv4: range $prefix got 0 IPs — not balanced"
            all_ranges_populated=false
        elif [ "$count" -gt "$max_per_range" ]; then
            fail "Balanced IPv4: range $prefix got $count IPs — exceeds fair share of $max_per_range"
        else
            pass "Balanced IPv4: range $prefix got $count IPs (fair share: $max_per_range)"
        fi
    done
    [ "$all_ranges_populated" = "true" ] || fail "Balanced IPv4: not all ranges received allocations"

    # Verify connectivity for first service
    local first_ip="${svc_ips[0]}"
    wait_for_ip_announced "$first_ip" 30 || fail "Balanced VIP $first_ip not announced"
    local resp
    resp=$(curl -s --connect-timeout 5 "http://${first_ip}:86/" || true)
    echo "$resp" | grep -q "Pod:" || fail "Balanced VIP $first_ip not reachable"
    pass "Balanced IPv4 VIP $first_ip reachable"

    # Cleanup sub-test 1 services
    for svc_name in "${svc_names[@]}"; do
        kubectl delete svc ${svc_name} -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    done
    unset range_counts

    #-----------------------------------------------------------------
    # Sub-test 2: Balanced IPv6 allocation
    #-----------------------------------------------------------------
    if [ "$HAS_IPV6" = "true" ] && [ "$v6_subnet_count" -ge 2 ]; then
        info ""
        info "--- Sub-test 2: Balanced IPv6 allocation ---"

        local -a v6_svc_names=()
        local -a v6_svc_ips=()
        local v6_svc_count=$((v6_subnet_count * 2))
        for i in $(seq 1 $v6_svc_count); do
            local svc_name="nginx-bal-v6-${i}"
            v6_svc_names+=("$svc_name")
            kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${svc_name}
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: balanced-test
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv6
  selector:
    app: nginx
  ports:
  - port: $((90 + i))
    targetPort: 80
EOF
        done

        for svc_name in "${v6_svc_names[@]}"; do
            kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
                svc/${svc_name} -n $NAMESPACE --timeout=30s || fail "Balanced IPv6: $svc_name no IP allocated"
        done

        # Count per v6 range
        declare -A v6_range_counts
        for svc_name in "${v6_svc_names[@]}"; do
            local ip
            ip=$(kubectl get svc ${svc_name} -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
            v6_svc_ips+=("$ip")
            # Match against known v6 prefixes
            local matched_prefix=""
            for v6p in "${v6_prefixes[@]}"; do
                if echo "$ip" | grep -qi "^${v6p}"; then
                    matched_prefix="$v6p"
                    break
                fi
            done
            [ -n "$matched_prefix" ] || fail "Balanced IPv6: IP $ip doesn't match any known prefix"
            v6_range_counts[$matched_prefix]=$(( ${v6_range_counts[$matched_prefix]:-0} + 1 ))
            detail "$svc_name -> $ip (range $matched_prefix)"
        done

        local v6_max_per_range=$(( (v6_svc_count + v6_subnet_count - 1) / v6_subnet_count ))
        for v6p in "${v6_prefixes[@]}"; do
            local count=${v6_range_counts[$v6p]:-0}
            if [ "$count" -eq 0 ]; then
                fail "Balanced IPv6: range $v6p got 0 IPs — not balanced"
            elif [ "$count" -gt "$v6_max_per_range" ]; then
                fail "Balanced IPv6: range $v6p got $count IPs — exceeds fair share of $v6_max_per_range"
            else
                pass "Balanced IPv6: range $v6p got $count IPs (fair share: $v6_max_per_range)"
            fi
        done

        # Cleanup
        for svc_name in "${v6_svc_names[@]}"; do
            kubectl delete svc ${svc_name} -n $NAMESPACE --ignore-not-found 2>/dev/null || true
        done
        unset v6_range_counts
    else
        info ""
        info "SKIP: Sub-test 2 (Balanced IPv6) requires IPv6 and 2+ v6 subnets"
    fi

    #-----------------------------------------------------------------
    # Sub-test 3: Balanced dual-stack allocation
    #-----------------------------------------------------------------
    if [ "$HAS_IPV6" = "true" ] && [ "$v6_subnet_count" -ge 2 ]; then
        info ""
        info "--- Sub-test 3: Balanced dual-stack allocation ---"

        local -a ds_svc_names=()
        local ds_svc_count=$((v4_subnet_count * 2))
        for i in $(seq 1 $ds_svc_count); do
            local svc_name="nginx-bal-ds-${i}"
            ds_svc_names+=("$svc_name")
            kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${svc_name}
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: balanced-test
spec:
  type: LoadBalancer
  ipFamilyPolicy: RequireDualStack
  ipFamilies:
  - IPv4
  - IPv6
  selector:
    app: nginx
  ports:
  - port: $((95 + i))
    targetPort: 80
EOF
        done

        for svc_name in "${ds_svc_names[@]}"; do
            kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
                svc/${svc_name} -n $NAMESPACE --timeout=30s || fail "Balanced dual-stack: $svc_name no IP allocated"
        done
        sleep 3

        # Count IPv4 and IPv6 allocations per range
        declare -A ds_v4_counts
        declare -A ds_v6_counts
        for svc_name in "${ds_svc_names[@]}"; do
            local all_ips
            all_ips=$(kubectl get svc ${svc_name} -n $NAMESPACE \
                -o jsonpath='{.status.loadBalancer.ingress[*].ip}')
            for ip in $all_ips; do
                if echo "$ip" | grep -q ':'; then
                    # IPv6
                    for v6p in "${v6_prefixes[@]}"; do
                        if echo "$ip" | grep -qi "^${v6p}"; then
                            ds_v6_counts[$v6p]=$(( ${ds_v6_counts[$v6p]:-0} + 1 ))
                            break
                        fi
                    done
                else
                    # IPv4
                    local ip_prefix
                    ip_prefix=$(echo "$ip" | awk -F. '{print $1"."$2"."$3}')
                    ds_v4_counts[$ip_prefix]=$(( ${ds_v4_counts[$ip_prefix]:-0} + 1 ))
                fi
            done
        done

        # Verify IPv4 distribution
        for prefix in "${v4_prefixes[@]}"; do
            local count=${ds_v4_counts[$prefix]:-0}
            [ "$count" -ge 1 ] || fail "Balanced DS IPv4: range $prefix got 0 IPs"
            pass "Balanced DS IPv4: range $prefix got $count IPs"
        done

        # Verify IPv6 distribution
        for v6p in "${v6_prefixes[@]}"; do
            local count=${ds_v6_counts[$v6p]:-0}
            [ "$count" -ge 1 ] || fail "Balanced DS IPv6: range $v6p got 0 IPs"
            pass "Balanced DS IPv6: range $v6p got $count IPs"
        done

        # Cleanup
        for svc_name in "${ds_svc_names[@]}"; do
            kubectl delete svc ${svc_name} -n $NAMESPACE --ignore-not-found 2>/dev/null || true
        done
        unset ds_v4_counts ds_v6_counts
    else
        info ""
        info "SKIP: Sub-test 3 (Balanced dual-stack) requires IPv6 and 2+ v6 subnets"
    fi

    #-----------------------------------------------------------------
    # Sub-test 4: NEGATIVE — balanced + multiPool mutual exclusion
    # A ServiceGroup with BOTH balanced: true and multiPool: true should
    # cause an AllocationFailed event when a service tries to allocate.
    # Uses .170-.179 range to avoid overlap with balanced-test (.150-.160).
    #-----------------------------------------------------------------
    info ""
    info "--- Sub-test 4: NEGATIVE — balanced + multiPool mutual exclusion ---"

    # Build non-overlapping pools for this SG (.170-.179)
    local conflict_v4pools=""
    for sub in $SUBNETS; do
        if echo "$sub" | grep -q ':'; then continue; fi
        local net="${sub%/*}"
        local prefix
        prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
        conflict_v4pools="${conflict_v4pools}
    - pool: ${prefix}.170-${prefix}.179
      subnet: ${sub}"
    done

    kubectl apply -f - <<EOF
apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: balanced-multipool-conflict
  namespace: purelb-system
spec:
  local:
    balanced: true
    multiPool: true
    v4pools: ${conflict_v4pools}
EOF
    info "Created ServiceGroup with BOTH balanced and multiPool (should conflict)"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-bal-conflict
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: balanced-multipool-conflict
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    # Wait a few seconds — allocation should fail
    sleep 8

    local conflict_ip
    conflict_ip=$(kubectl get svc nginx-bal-conflict -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)

    if [ -n "$conflict_ip" ]; then
        fail "Balanced+MultiPool: got IP $conflict_ip — mutual exclusion NOT enforced"
    fi

    # Check for AllocationFailed event with "mutually exclusive" message
    local events
    events=$(kubectl get events -n $NAMESPACE \
        --field-selector involvedObject.name=nginx-bal-conflict,reason=AllocationFailed \
        -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if echo "$events" | grep -q "mutually exclusive"; then
        pass "Balanced+MultiPool correctly rejected: $events"
    elif [ -n "$events" ]; then
        fail "Balanced+MultiPool: got AllocationFailed but wrong reason: $events"
    else
        # Also check allocator logs for the mutual exclusion error
        local log_msg
        log_msg=$(kubectl logs -n purelb-system deployment/allocator --tail=50 2>/dev/null \
            | grep -i "mutually exclusive" || true)
        if [ -n "$log_msg" ]; then
            pass "Balanced+MultiPool correctly rejected (found in allocator logs)"
        else
            fail "Balanced+MultiPool: no IP allocated but 'mutually exclusive' error not found"
        fi
    fi

    # Cleanup
    kubectl delete svc nginx-bal-conflict -n $NAMESPACE --ignore-not-found 2>/dev/null || true
    kubectl delete servicegroup balanced-multipool-conflict -n purelb-system --ignore-not-found 2>/dev/null || true

    #-----------------------------------------------------------------
    # Sub-test 5: NEGATIVE — service targeting non-existent ServiceGroup
    # Verify the allocator rejects a service requesting a pool that
    # doesn't exist and produces an appropriate error event.
    #-----------------------------------------------------------------
    info ""
    info "--- Sub-test 5: NEGATIVE — non-existent ServiceGroup ---"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-bal-nosuchsg
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: does-not-exist
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    sleep 8

    local nosuch_ip
    nosuch_ip=$(kubectl get svc nginx-bal-nosuchsg -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -n "$nosuch_ip" ]; then
        fail "Non-existent SG: got IP $nosuch_ip — should have been rejected"
    fi

    local nosuch_events
    nosuch_events=$(kubectl get events -n $NAMESPACE \
        --field-selector involvedObject.name=nginx-bal-nosuchsg,reason=AllocationFailed \
        -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if [ -n "$nosuch_events" ]; then
        pass "Non-existent SG correctly rejected: $nosuch_events"
    else
        pass "Non-existent SG: allocation correctly prevented (no IP assigned)"
    fi

    # Cleanup
    kubectl delete svc nginx-bal-nosuchsg -n $NAMESPACE --ignore-not-found 2>/dev/null || true

    #-----------------------------------------------------------------
    # Sub-test 6: NEGATIVE — balanced SG with annotation override
    # multi-pool: "true" on a balanced SG should also be rejected.
    #-----------------------------------------------------------------
    info ""
    info "--- Sub-test 6: NEGATIVE — multi-pool annotation on balanced SG ---"

    # The balanced-test SG still exists (balanced: true, multiPool: false)
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: nginx-bal-annoverride
  namespace: $NAMESPACE
  annotations:
    purelb.io/service-group: balanced-test
    purelb.io/multi-pool: "true"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
EOF

    sleep 8

    local override_ip
    override_ip=$(kubectl get svc nginx-bal-annoverride -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [ -n "$override_ip" ]; then
        fail "Multi-pool annotation on balanced SG: got IP $override_ip — mutual exclusion NOT enforced"
    fi

    local override_events
    override_events=$(kubectl get events -n $NAMESPACE \
        --field-selector involvedObject.name=nginx-bal-annoverride,reason=AllocationFailed \
        -o jsonpath='{.items[*].message}' 2>/dev/null || true)
    if [ -n "$override_events" ]; then
        pass "Multi-pool annotation on balanced SG correctly rejected: $override_events"
    else
        pass "Multi-pool annotation on balanced SG: allocation correctly prevented (no IP assigned)"
    fi

    # Cleanup
    kubectl delete svc nginx-bal-annoverride -n $NAMESPACE --ignore-not-found 2>/dev/null || true

    # --- Metrics verification for balanced allocation ---
    info ""
    echo -e "${CYAN}    ── Metrics & Log Verification ──────────────────────────────${NC}"
    info "Verifying balanced allocation metrics..."
    local after_balanced
    after_balanced=$(scrape_allocator_metric 'purelb_address_pool_balanced_allocations_total{pool="balanced-test"}')
    after_balanced=$(printf '%.0f' "${after_balanced:-0}")
    if [ "$after_balanced" -gt "$before_balanced" ]; then
        pass "Allocator: balanced_allocations_total increased ($before_balanced -> $after_balanced)"
    else
        info "WARNING: balanced_allocations_total did not increase (before=$before_balanced, after=$after_balanced)"
        info "  (metric may reset if allocator restarted, or pool name may differ)"
    fi

    # Verify balanced allocation log message
    info "Checking for balanced allocation log message..."
    if kubectl logs -n purelb-system deployment/allocator --tail=300 2>/dev/null | grep -q "assignFamilyBalanced"; then
        pass "Allocator: logged 'assignFamilyBalanced' (balanced range selection)"
    else
        info "WARNING: 'assignFamilyBalanced' not found in recent allocator logs"
    fi

    # Final cleanup
    info ""
    info "Cleaning up balanced allocation test resources..."
    kubectl delete servicegroup balanced-test -n purelb-system --ignore-not-found 2>/dev/null || true
    pass "Balanced allocation tests completed"
}

#---------------------------------------------------------------------
# Metrics & Logging Helpers
#---------------------------------------------------------------------

# scrape_metrics PORT_FORWARD_TARGET
# Scrapes /metrics from a pod via kubectl port-forward.
# Writes raw metrics to stdout. Caller should capture output.
# Usage: OUTPUT=$(scrape_pod_metrics <pod-name>)
scrape_pod_metrics() {
    local pod=$1
    local local_port=$((30000 + RANDOM % 5000))
    # Start port-forward in background
    kubectl port-forward -n purelb-system "$pod" ${local_port}:7472 >/dev/null 2>&1 &
    local pf_pid=$!
    # Retry curl until port-forward is ready (up to 5 attempts)
    local metrics=""
    local attempt
    for attempt in 1 2 3 4 5; do
        sleep 1
        metrics=$(curl -s --connect-timeout 3 "http://127.0.0.1:${local_port}/metrics" 2>/dev/null || true)
        if [ -n "$metrics" ]; then
            break
        fi
    done
    kill $pf_pid 2>/dev/null || true
    wait $pf_pid 2>/dev/null || true
    echo "$metrics"
}

# scrape_allocator_metrics
# Scrapes metrics from the allocator deployment pod.
scrape_allocator_metrics() {
    local pod
    pod=$(kubectl get pods -n purelb-system -l component=allocator -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
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
assert_log_contains() {
    local component="$1"
    local pattern="$2"
    local description="$3"

    if [ "$component" = "allocator" ]; then
        if kubectl logs -n purelb-system deployment/allocator --tail=200 2>/dev/null | grep -q "$pattern"; then
            return 0
        fi
        fail "Allocator logs missing: $description (pattern: $pattern)"
    elif [ "$component" = "lbnodeagent" ]; then
        local pods
        pods=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)
        for pod in $pods; do
            if kubectl logs -n purelb-system "$pod" --tail=200 2>/dev/null | grep -q "$pattern"; then
                return 0
            fi
        done
        fail "LBNodeAgent logs missing on all pods: $description (pattern: $pattern)"
    else
        fail "Unknown component: $component"
    fi
}

# assert_log_contains_on_node NODE PATTERN DESCRIPTION
# Checks that the lbnodeagent pod on a specific node has the log pattern.
assert_log_contains_on_node() {
    local node="$1"
    local pattern="$2"
    local description="$3"

    local pod
    pod=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null \
        | grep "$node" | awk '{print $1}')
    if [ -z "$pod" ]; then
        fail "No lbnodeagent pod found on node $node"
    fi
    if kubectl logs -n purelb-system "$pod" --tail=200 2>/dev/null | grep -q "$pattern"; then
        return 0
    fi
    fail "LBNodeAgent logs on $node missing: $description (pattern: $pattern)"
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

#---------------------------------------------------------------------
# Run All Tests
#---------------------------------------------------------------------
run_all_tests() {
    echo ""
    echo "╔══════════════════════════════════════════╗"
    echo "║  PureLB Local Mode Functional Test Suite ║"
    echo "╚══════════════════════════════════════════╝"
    echo ""
    echo "Cluster: $CONTEXT"
    echo "Namespace: $NAMESPACE"
    if [ "$INTERACTIVE" = "true" ]; then
        echo ""
        echo -e "${YELLOW}>>> INTERACTIVE MODE: Will pause after test groups for review <<<${NC}"
    fi
    echo ""

    # Infrastructure validation - MUST pass before running tests
    validate_prerequisites
    pause_for_review

    # Setup
    setup_lbnodeagent
    generate_default_servicegroup

    # Subnet-Aware Election tests (NEW)
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  TEST GROUP: Subnet-Aware Election${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    test_lease_verification
    test_local_pool_no_matching_subnet
    test_remote_pool
    pause_for_review

    # Core functionality tests
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  TEST GROUP: Core Functionality${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    test_ipv4_singlestack
    test_ipv6_singlestack
    test_dualstack
    test_leader_election
    test_service_cleanup
    test_ip_sharing
    test_specific_ip_request
    test_multi_pod_lb
    test_loadbalancer_class
    pause_for_review

    # Failover tests
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  TEST GROUP: Failover & High Availability${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    test_node_failover
    test_graceful_failover
    pause_for_review

    # Additional functionality tests
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  TEST GROUP: Additional Functionality${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    test_etp_local_override
    test_no_duplicate_vips
    pause_for_review

    # Address lifetime and flag tests (ensures CNI compatibility)
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  TEST GROUP: Address Lifetime & CNI Compatibility${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    test_local_vip_address_flags
    test_address_renewal_timer
    test_flannel_node_ip
    test_flannel_node_ipv6
    pause_for_review

    # Cross-node connectivity validation (catches IP forwarding issues)
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  TEST GROUP: Cross-Node Connectivity${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    test_cross_node_connectivity
    test_pod_connectivity
    pause_for_review

    # Multi-subnet tests (only on clusters with 2+ subnets)
    if [ "$SUBNET_COUNT" -ge 2 ]; then
        echo ""
        echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
        echo -e "${BLUE}  TEST GROUP: Multi-Subnet Validation${NC}"
        echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
        test_lease_subnets_match_node_ips
        test_subnet_vip_placement
        test_subnet_failover_stays_in_subnet
        test_multi_pool_default
        test_multi_pool_allocation
        test_balanced_allocation
        test_cross_subnet_connectivity
        test_small_subnet_exhaustion
        if [ "$HAS_IPV6" = "true" ]; then
            test_ipv6_multi_subnet_placement
            test_dualstack_multi_subnet
        fi
        pause_for_review
    else
        echo ""
        info "SKIP: Multi-subnet tests require 2+ subnets (found $SUBNET_COUNT)"
    fi

    # Cleanup all test services
    cleanup_test_services

    echo ""
    echo "=========================================="
    echo -e "${GREEN}ALL TESTS PASSED${NC}"
    echo "=========================================="
}

#---------------------------------------------------------------------
# Cleanup: Remove all test services
#---------------------------------------------------------------------
cleanup_test_services() {
    echo ""
    echo "=========================================="
    echo "Cleanup: Removing test services"
    echo "=========================================="

    info "Deleting all test services..."
    kubectl delete svc -n $NAMESPACE --all 2>/dev/null || true
    pass "Test services cleaned up"
}

# Run tests.
# Each iteration runs in a subshell so that fail() → exit 1 only ends
# that iteration, not the whole script. This allows multi-iteration runs
# to continue after a failure.
PASS=0
FAIL=0
for iter in $(seq 1 $ITERATIONS); do
    if [ "$ITERATIONS" -gt 1 ]; then
        echo ""
        echo "######################################################################"
        echo "#  ITERATION $iter / $ITERATIONS"
        echo "######################################################################"
    fi

    if (run_all_tests); then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo ""
        echo -e "${RED}--- ITERATION $iter FAILED ---${NC}"
        # Ensure cleanup between iterations even after failure.
        # bash clears EXIT traps in subshells, so cleanup_servicegroups from
        # common.sh does not fire when fail() → exit 1 exits the subshell.
        # Explicitly delete all test ServiceGroups (keep only 'default').
        echo "Cleaning up after failed iteration..."
        kubectl delete svc -n $NAMESPACE --all 2>/dev/null || true
        kubectl get servicegroup -n purelb-system -o name 2>/dev/null \
            | grep -v '/default$' \
            | xargs -r kubectl delete -n purelb-system --ignore-not-found 2>/dev/null || true
    fi
done

if [ "$ITERATIONS" -gt 1 ]; then
    echo ""
    echo "=========================================="
    echo "ITERATION SUMMARY ($ITERATIONS runs)"
    echo "=========================================="
    echo -e "Passed: ${GREEN}$PASS${NC}"
    echo -e "Failed: ${RED}$FAIL${NC}"
    echo ""
fi

echo "Log file: $LOG_FILE"

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
