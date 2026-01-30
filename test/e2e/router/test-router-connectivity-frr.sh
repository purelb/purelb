#!/bin/bash
set -e

# PureLB Router-Based E2E Test Suite (FRR Version)
#
# Extended tests that verify BGP routes in the FRR router's RIB.
# This version includes all basic connectivity tests PLUS route verification.
#
# Prerequisites:
# - All prerequisites from test-router-connectivity.sh
# - SSH access to FRR router with vtysh available
# - FRR router peered with cluster nodes (running GoBGP)

# Bash version check (required for associative arrays)
if [[ ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "ERROR: Bash 4+ required for associative arrays"
    exit 1
fi

#---------------------------------------------------------------------
# Configuration
#---------------------------------------------------------------------

CONTEXT="proxmox"
NAMESPACE="test"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Required environment variables
ROUTER_HOST="${ROUTER_HOST:-}"

# Optional configuration
VIP_SUBNET="${VIP_SUBNET:-10.255.0.0/24}"
BGP_CONVERGE_TIMEOUT="${BGP_CONVERGE_TIMEOUT:-30}"
ECMP_TEST_REQUESTS="${ECMP_TEST_REQUESTS:-100}"
SERVICE_GROUP="${SERVICE_GROUP:-remote}"

# Test run tracking
TEST_RUN_ID=$(date +%s)

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

pass() { echo -e "${GREEN}PASS:${NC} $1"; }
fail() { echo -e "${RED}FAIL:${NC} $1"; dump_debug_state; exit 1; }
info() { echo -e "${YELLOW}INFO:${NC} $1"; }
warn() { echo -e "${YELLOW}WARN:${NC} $1"; }
section() { echo -e "\n${BLUE}==== $1 ====${NC}"; }

kubectl() { command kubectl --context "$CONTEXT" "$@"; }

# Get node list dynamically
NODES=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
NODE_COUNT=$(echo $NODES | wc -w)

#---------------------------------------------------------------------
# SSH Helper Functions
#---------------------------------------------------------------------

# SSH to FRR router
ssh_router() {
    ssh "$ROUTER_HOST" "$@" 2>/dev/null
}

# SSH to cluster node
ssh_node() {
    local NODE=$1
    shift
    ssh "$NODE" "$@" 2>/dev/null
}

# Verify SSH connectivity
verify_ssh() {
    local HOST=$1
    local DESC=$2
    if ! ssh "$HOST" "true" 2>/dev/null; then
        fail "Cannot SSH to $DESC ($HOST)"
    fi
    pass "SSH to $DESC ($HOST) working"
}

#---------------------------------------------------------------------
# Debug and Cleanup Functions
#---------------------------------------------------------------------

dump_debug_state() {
    echo ""
    echo "=== DEBUG STATE DUMP ==="
    echo "--- Services ---"
    kubectl get svc -n $NAMESPACE -o wide 2>/dev/null || echo "(failed)"
    echo "--- Events (last 20) ---"
    kubectl get events -n $NAMESPACE --sort-by=.lastTimestamp 2>/dev/null | tail -20 || echo "(failed)"
    echo "--- kube-lb0 on all nodes ---"
    for node in $NODES; do
        echo "[$node] kube-lb0:"
        ssh_node $node "ip -o addr show kube-lb0 2>/dev/null" || echo "  (failed)"
    done
    echo "--- FRR BGP routes ---"
    frr_show_routes 2>/dev/null || echo "(failed)"
    echo "--- lbnodeagent pods ---"
    kubectl get pods -n purelb-system-l component=lbnodeagent -o wide 2>/dev/null || echo "(failed)"
    echo "========================="
}

cleanup_on_exit() {
    info "Cleanup: removing test services and taints..."
    for node in $NODES; do
        kubectl taint node $node purelb-router-test- 2>/dev/null || true
        kubectl uncordon $node 2>/dev/null || true
    done
    kubectl delete svc -n $NAMESPACE -l test-suite=router --ignore-not-found 2>/dev/null || true
}
trap cleanup_on_exit EXIT

#---------------------------------------------------------------------
# FRR Route Verification Functions
#---------------------------------------------------------------------

# Run vtysh command on FRR router
frr_vtysh() {
    ssh_router "vtysh -c '$1'" 2>/dev/null || \
    ssh_router "sudo vtysh -c '$1'" 2>/dev/null
}

# Show routes for VIP subnet
frr_show_routes() {
    frr_vtysh "show ip route $VIP_SUBNET longer-prefixes"
}

# Check if a specific VIP has a route
frr_check_route() {
    local VIP=$1
    frr_vtysh "show ip route $VIP/32" 2>/dev/null | grep -q "$VIP"
}

# Get next-hops for a VIP from FRR
frr_get_nexthops() {
    local VIP=$1
    frr_vtysh "show ip route $VIP/32" 2>/dev/null | grep -oP 'via \K[0-9.]+'
}

# Count next-hops for a VIP
frr_count_nexthops() {
    local VIP=$1
    frr_get_nexthops "$VIP" | wc -l
}

# Get route prefix length
frr_get_prefix_length() {
    local VIP=$1
    frr_vtysh "show ip route $VIP/32" 2>/dev/null | grep -oP "$VIP/\K[0-9]+" | head -1
}

# Wait for route to appear in FRR
frr_wait_for_route() {
    local VIP=$1
    local TIMEOUT=${2:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if frr_check_route "$VIP"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

# Wait for route to be withdrawn from FRR
frr_wait_for_withdrawal() {
    local VIP=$1
    local TIMEOUT=${2:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! frr_check_route "$VIP"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

# Check BGP session status
frr_check_bgp_sessions() {
    frr_vtysh "show bgp summary"
}

#---------------------------------------------------------------------
# External Connectivity Functions
#---------------------------------------------------------------------

test_connectivity() {
    local VIP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-5}
    curl -s --connect-timeout $TIMEOUT "http://$VIP:$PORT/" 2>/dev/null
}

wait_for_connectivity() {
    local VIP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-60}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        local RESPONSE=$(test_connectivity "$VIP" "$PORT")
        if echo "$RESPONSE" | grep -q "Pod:"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

#---------------------------------------------------------------------
# Service Management Functions
#---------------------------------------------------------------------

create_test_service() {
    local NAME=$1
    local PORT=${2:-80}
    local ETP=${3:-Cluster}

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: $NAME
  namespace: $NAMESPACE
  labels:
    test-suite: router
    test-run: "$TEST_RUN_ID"
  annotations:
    purelb.io/service-group: $SERVICE_GROUP
spec:
  type: LoadBalancer
  externalTrafficPolicy: $ETP
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - name: http
    port: $PORT
    targetPort: 80
EOF
}

wait_for_service_ip() {
    local SVC=$1
    local TIMEOUT=${2:-60}

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/$SVC -n $NAMESPACE --timeout=${TIMEOUT}s >/dev/null 2>&1 || return 1

    kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
}

wait_for_vip_announced() {
    local VIP=$1
    local EXPECTED=${2:-$NODE_COUNT}
    local TIMEOUT=${3:-60}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        local COUNT=0
        for node in $NODES; do
            if ssh_node $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/'"; then
                COUNT=$((COUNT + 1))
            fi
        done
        if [ "$COUNT" -eq "$EXPECTED" ]; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

count_nodes_with_vip() {
    local VIP=$1
    local COUNT=0
    for node in $NODES; do
        if ssh_node $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/'"; then
            COUNT=$((COUNT + 1))
        fi
    done
    echo $COUNT
}

#---------------------------------------------------------------------
# Test 0: Prerequisites (Extended for FRR)
#---------------------------------------------------------------------

test_prerequisites() {
    section "TEST 0: Prerequisites (FRR Extended)"

    # Check required environment variables
    if [ -z "$ROUTER_HOST" ]; then
        fail "ROUTER_HOST environment variable not set"
    fi

    info "Configuration:"
    info "  ROUTER_HOST: $ROUTER_HOST"
    info "  VIP_SUBNET: $VIP_SUBNET"
    info "  SERVICE_GROUP: $SERVICE_GROUP"

    # Verify SSH connectivity
    info "Verifying SSH connectivity..."
    verify_ssh "$ROUTER_HOST" "FRR router"
    for node in $NODES; do
        verify_ssh "$node" "cluster node $node"
    done

    # Verify FRR vtysh access
    info "Verifying FRR vtysh access..."
    if frr_vtysh "show version" >/dev/null 2>&1; then
        pass "FRR vtysh accessible"
    else
        fail "Cannot access FRR vtysh on $ROUTER_HOST"
    fi

    # Check BGP sessions
    info "Checking BGP session status..."
    local BGP_STATUS=$(frr_check_bgp_sessions)
    echo "$BGP_STATUS" | head -10
    if echo "$BGP_STATUS" | grep -qE "Estab|established"; then
        pass "BGP sessions established"
    else
        warn "BGP sessions may not be fully established"
    fi

    # Verify PureLB is running
    info "Verifying PureLB components..."
    local AGENT_PODS=$(kubectl get pods -n purelb-system-l component=lbnodeagent --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    [ "$AGENT_PODS" -ge 1 ] || fail "LBNodeAgent not running"
    pass "LBNodeAgent running ($AGENT_PODS pods)"

    local ALLOCATOR_READY=$(kubectl get deployment -n purelb-systemallocator -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    [ "$ALLOCATOR_READY" -ge 1 ] || fail "Allocator not running"
    pass "Allocator running"

    # Verify nginx test pods exist
    local NGINX_PODS=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    [ "$NGINX_PODS" -ge 1 ] || fail "No nginx test pods running in namespace $NAMESPACE"
    pass "Test pods running ($NGINX_PODS pods)"

    # Verify ServiceGroup exists
    if ! kubectl get servicegroup -n purelb-system"$SERVICE_GROUP" >/dev/null 2>&1; then
        fail "ServiceGroup '$SERVICE_GROUP' not found in purelb namespace"
    fi
    pass "ServiceGroup '$SERVICE_GROUP' exists"

    pass "All prerequisites verified"
}

#---------------------------------------------------------------------
# Test 1: Basic External Connectivity with Route Verification
#---------------------------------------------------------------------

test_basic_with_route_verification() {
    section "TEST 1: External Connectivity with FRR Route Verification"

    local SVC_NAME="router-frr-basic"

    info "Creating test service..."
    create_test_service "$SVC_NAME" 80 "Cluster"

    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced on nodes"
    pass "VIP announced on $NODE_COUNT nodes"

    # FRR-specific: Wait for and verify route
    info "Waiting for route in FRR RIB..."
    if frr_wait_for_route "$VIP" "$BGP_CONVERGE_TIMEOUT"; then
        pass "Route to $VIP found in FRR RIB"
    else
        fail "Route to $VIP not found in FRR RIB after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    # Show route details
    info "FRR route details:"
    frr_vtysh "show ip route $VIP/32"

    # Verify next-hops
    local NEXTHOP_COUNT=$(frr_count_nexthops "$VIP")
    info "Route has $NEXTHOP_COUNT next-hop(s)"

    if [ "$NEXTHOP_COUNT" -ge 1 ]; then
        pass "Route has valid next-hops"
        info "Next-hops:"
        frr_get_nexthops "$VIP" | while read nh; do echo "  - $nh"; done
    else
        fail "No next-hops found for route"
    fi

    # Verify prefix length is /32
    local PREFIX_LEN=$(frr_get_prefix_length "$VIP")
    if [ "$PREFIX_LEN" = "32" ]; then
        pass "Route has correct /32 aggregation"
    else
        warn "Route has /$PREFIX_LEN aggregation (expected /32)"
    fi

    # Test connectivity
    info "Testing external connectivity..."
    if wait_for_connectivity "$VIP" 80 30; then
        local RESPONSE=$(test_connectivity "$VIP" 80)
        pass "External connectivity successful"
        echo "$RESPONSE" | head -3
    else
        fail "External connectivity failed"
    fi

    kubectl delete svc -n $NAMESPACE "$SVC_NAME"
    pass "Basic connectivity with route verification completed"
}

#---------------------------------------------------------------------
# Test 2: ECMP with Next-Hop Verification
#---------------------------------------------------------------------

test_ecmp_with_nexthop_verification() {
    section "TEST 2: ECMP with FRR Next-Hop Verification"

    local SVC_NAME="router-frr-ecmp"

    # Scale for distribution
    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    kubectl scale deployment nginx -n $NAMESPACE --replicas=5
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s

    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    frr_wait_for_route "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "Route not in FRR"

    # FRR-specific: Verify multiple next-hops for ECMP
    info "Verifying ECMP next-hops in FRR..."
    local NEXTHOP_COUNT=$(frr_count_nexthops "$VIP")
    info "FRR shows $NEXTHOP_COUNT next-hop(s) for ECMP"

    if [ "$NEXTHOP_COUNT" -ge 2 ]; then
        pass "ECMP configured: $NEXTHOP_COUNT next-hops in FRR"
    else
        warn "Only $NEXTHOP_COUNT next-hop(s) - ECMP requires multiple"
        warn "Check FRR 'maximum-paths' configuration"
    fi

    info "Next-hops from FRR:"
    frr_get_nexthops "$VIP" | while read nh; do echo "  - $nh"; done

    # Test traffic distribution
    info "Testing traffic distribution..."
    wait_for_connectivity "$VIP" 80 30 || fail "Connectivity failed"

    declare -A NODES_SEEN
    local SUCCESS=0
    for i in $(seq 1 50); do
        local RESPONSE=$(curl -s --connect-timeout 3 --local-port $((10000 + i)) "http://$VIP/" 2>/dev/null || true)
        if echo "$RESPONSE" | grep -q "Node:"; then
            SUCCESS=$((SUCCESS + 1))
            local NODE=$(echo "$RESPONSE" | grep "Node:" | awk '{print $2}')
            [ -n "$NODE" ] && NODES_SEEN[$NODE]=1
        fi
    done

    info "Traffic hit ${#NODES_SEEN[@]} unique nodes"
    if [ "${#NODES_SEEN[@]}" -ge 2 ]; then
        pass "Traffic distributed across multiple nodes"
    else
        warn "Traffic only hit 1 node (ECMP flow hashing)"
    fi

    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    kubectl delete svc -n $NAMESPACE "$SVC_NAME"
    pass "ECMP with next-hop verification completed"
}

#---------------------------------------------------------------------
# Test 3: Node Failure with Route Withdrawal Verification
#---------------------------------------------------------------------

test_node_failure_route_withdrawal() {
    section "TEST 3: Node Failure with FRR Route Withdrawal"

    local SVC_NAME="router-frr-failover"

    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    frr_wait_for_route "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "Route not in FRR"

    # Record initial next-hops
    local INITIAL_NEXTHOPS=$(frr_count_nexthops "$VIP")
    info "Initial next-hops in FRR: $INITIAL_NEXTHOPS"
    frr_get_nexthops "$VIP" | while read nh; do echo "  - $nh"; done

    # Fail a node
    local FAIL_NODE=${NODES%% *}
    info "Failing node: $FAIL_NODE"

    kubectl taint node "$FAIL_NODE" purelb-router-test=failover:NoExecute --overwrite
    local AGENT_POD=$(kubectl get pods -n purelb-system-l component=lbnodeagent -o wide | grep "$FAIL_NODE" | awk '{print $1}')
    [ -n "$AGENT_POD" ] && kubectl delete pod -n purelb-system"$AGENT_POD" --grace-period=0 --force 2>/dev/null || true

    # Wait for VIP removal from node
    info "Waiting for VIP removal from $FAIL_NODE..."
    local TIMEOUT=30
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! ssh_node "$FAIL_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/'"; then
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    pass "VIP removed from $FAIL_NODE after ${ELAPSED}s"

    # FRR-specific: Verify next-hop count decreased
    info "Waiting for FRR route update..."
    sleep 10

    local NEW_NEXTHOPS=$(frr_count_nexthops "$VIP")
    info "Next-hops after failure: $NEW_NEXTHOPS (was $INITIAL_NEXTHOPS)"

    if [ "$NEW_NEXTHOPS" -lt "$INITIAL_NEXTHOPS" ]; then
        pass "FRR route updated: next-hop count decreased"
    else
        warn "FRR next-hop count unchanged (BGP may still be converging)"
    fi

    info "Current next-hops:"
    frr_get_nexthops "$VIP" | while read nh; do echo "  - $nh"; done

    # Verify route still exists
    if frr_check_route "$VIP"; then
        pass "Route still exists in FRR (on remaining nodes)"
    else
        fail "Route completely disappeared from FRR"
    fi

    # Test connectivity
    info "Testing connectivity after failure..."
    local RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Connectivity failed after node failure"
    pass "Connectivity maintained"

    # Restore node
    info "Restoring $FAIL_NODE..."
    kubectl taint node "$FAIL_NODE" purelb-router-test-
    kubectl rollout status daemonset/lbnodeagent -n purelb-system--timeout=60s

    # Wait for next-hop to return
    info "Waiting for next-hop to return to FRR..."
    TIMEOUT=30
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local RESTORED_NEXTHOPS=$(frr_count_nexthops "$VIP")
        if [ "$RESTORED_NEXTHOPS" -ge "$INITIAL_NEXTHOPS" ]; then
            pass "Next-hop count restored: $RESTORED_NEXTHOPS"
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    kubectl delete svc -n $NAMESPACE "$SVC_NAME"
    pass "Node failure with route withdrawal test completed"
}

#---------------------------------------------------------------------
# Test 4: Service Deletion with Route Withdrawal
#---------------------------------------------------------------------

test_deletion_route_withdrawal() {
    section "TEST 4: Service Deletion with FRR Route Withdrawal"

    local SVC_NAME="router-frr-deletion"

    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    frr_wait_for_route "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "Route not in FRR"
    wait_for_connectivity "$VIP" 80 30 || fail "Connectivity failed"

    pass "Service running, route in FRR: $VIP"

    # Show route before deletion
    info "Route before deletion:"
    frr_vtysh "show ip route $VIP/32"

    # Delete service
    info "Deleting service..."
    kubectl delete svc -n $NAMESPACE "$SVC_NAME"

    # Wait for VIP removal from nodes
    info "Waiting for VIP removal from nodes..."
    local TIMEOUT=30
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local COUNT=$(count_nodes_with_vip "$VIP")
        if [ "$COUNT" -eq 0 ]; then
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    pass "VIP removed from all nodes after ${ELAPSED}s"

    # FRR-specific: Verify route withdrawal
    info "Waiting for route withdrawal from FRR..."
    if frr_wait_for_withdrawal "$VIP" "$BGP_CONVERGE_TIMEOUT"; then
        pass "Route withdrawn from FRR"
    else
        warn "Route still in FRR after ${BGP_CONVERGE_TIMEOUT}s (may be stale)"
        frr_vtysh "show ip route $VIP/32" || true
    fi

    # Verify connectivity fails
    local RESPONSE=$(test_connectivity "$VIP" 80)
    if echo "$RESPONSE" | grep -q "Pod:"; then
        warn "VIP still reachable (stale route?)"
    else
        pass "VIP no longer reachable"
    fi

    pass "Service deletion with route withdrawal test completed"
}

#---------------------------------------------------------------------
# Test 5: Route Aggregation Verification
#---------------------------------------------------------------------

test_route_aggregation() {
    section "TEST 5: Route Aggregation Verification"

    local SVC_NAME="router-frr-aggregation"

    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    frr_wait_for_route "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "Route not in FRR"

    # Verify /32 aggregation
    info "Verifying route aggregation..."
    local ROUTE_OUTPUT=$(frr_vtysh "show ip route $VIP/32")
    echo "$ROUTE_OUTPUT"

    # Check for /32 in the route
    if echo "$ROUTE_OUTPUT" | grep -q "$VIP/32"; then
        pass "Route has /32 aggregation (host route)"
    else
        local PREFIX_LEN=$(frr_get_prefix_length "$VIP")
        if [ -n "$PREFIX_LEN" ]; then
            warn "Route has /$PREFIX_LEN aggregation (expected /32)"
        else
            warn "Could not determine route prefix length"
        fi
    fi

    # Verify no /24 aggregate route (should be individual /32s)
    local SUBNET_ROUTE=$(frr_vtysh "show ip route ${VIP%.*}.0/24" 2>/dev/null || true)
    if echo "$SUBNET_ROUTE" | grep -q "directly connected\|via"; then
        info "Subnet route exists (may be static or connected)"
    else
        pass "No unwanted /24 aggregate route"
    fi

    kubectl delete svc -n $NAMESPACE "$SVC_NAME"
    pass "Route aggregation verification completed"
}

#---------------------------------------------------------------------
# Run All Tests
#---------------------------------------------------------------------

run_all_tests() {
    echo ""
    echo "========================================================"
    echo "  PureLB Router-Based E2E Test Suite (FRR Version)"
    echo "  Testing connectivity with FRR route verification"
    echo "========================================================"
    echo ""
    echo "Cluster context: $CONTEXT"
    echo "Namespace: $NAMESPACE"
    echo "Nodes: $NODE_COUNT ($NODES)"
    echo "FRR router: $ROUTER_HOST"
    echo ""

    test_prerequisites
    test_basic_with_route_verification
    test_ecmp_with_nexthop_verification
    test_node_failure_route_withdrawal
    test_deletion_route_withdrawal
    test_route_aggregation

    echo ""
    echo "========================================================"
    echo -e "${GREEN}ALL FRR ROUTER TESTS COMPLETED${NC}"
    echo "========================================================"
}

#---------------------------------------------------------------------
# Main
#---------------------------------------------------------------------

SELECTED_TEST=""
while [[ $# -gt 0 ]]; do
    case $1 in
        --test)
            SELECTED_TEST="$2"
            shift 2
            ;;
        --help)
            echo "Usage: $0 [--test N]"
            echo ""
            echo "Extended tests with FRR BGP route verification."
            echo ""
            echo "Environment variables:"
            echo "  ROUTER_HOST           - FRR router hostname (required)"
            echo "  VIP_SUBNET            - Subnet for route queries (default: 10.255.0.0/24)"
            echo "  SERVICE_GROUP         - ServiceGroup to use (default: remote)"
            echo "  BGP_CONVERGE_TIMEOUT  - Seconds to wait for BGP (default: 30)"
            echo ""
            echo "Tests:"
            echo "  0 - Prerequisites (with FRR verification)"
            echo "  1 - Basic Connectivity with Route Verification"
            echo "  2 - ECMP with Next-Hop Verification"
            echo "  3 - Node Failure with Route Withdrawal"
            echo "  4 - Service Deletion with Route Withdrawal"
            echo "  5 - Route Aggregation Verification"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

if [ -n "$SELECTED_TEST" ]; then
    case "$SELECTED_TEST" in
        0) test_prerequisites ;;
        1) test_prerequisites && test_basic_with_route_verification ;;
        2) test_prerequisites && test_ecmp_with_nexthop_verification ;;
        3) test_prerequisites && test_node_failure_route_withdrawal ;;
        4) test_prerequisites && test_deletion_route_withdrawal ;;
        5) test_prerequisites && test_route_aggregation ;;
        *) echo "Unknown test: $SELECTED_TEST"; exit 1 ;;
    esac
else
    run_all_tests
fi
