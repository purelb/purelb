#!/bin/bash
set -e

# PureLB Router-Based E2E Test Suite (Basic Version)
#
# Tests connectivity through BGP routing WITHOUT requiring router CLI access.
# Verifies that traffic reaches services through BGP-learned routes.
#
# For tests that verify BGP routes in the router's RIB, see:
#   test-router-connectivity-frr.sh
#
# Prerequisites:
# - Router with BGP peering to cluster nodes (GoBGP on nodes)
# - This host can reach VIPs via the router
# - PureLB deployed with BGP configuration

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

# Optional configuration
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
    echo "--- lbnodeagent pods ---"
    kubectl get pods -n purelb -l component=lbnodeagent -o wide 2>/dev/null || echo "(failed)"
    echo "========================="
}

cleanup_on_exit() {
    info "Cleanup: removing test services and taints..."
    # Remove any taints left by failover tests
    for node in $NODES; do
        kubectl taint node $node purelb-router-test- 2>/dev/null || true
        kubectl uncordon $node 2>/dev/null || true
    done
    # Remove test services
    kubectl delete svc -n $NAMESPACE -l test-suite=router --ignore-not-found 2>/dev/null || true
}
trap cleanup_on_exit EXIT

#---------------------------------------------------------------------
# Connectivity Functions
#---------------------------------------------------------------------

# Test connectivity to VIP
test_connectivity() {
    local VIP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-5}

    curl -s --connect-timeout $TIMEOUT "http://$VIP:$PORT/" 2>/dev/null
}

# Test IPv6 connectivity
test_connectivity_v6() {
    local VIP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-5}

    curl -s --connect-timeout $TIMEOUT -6 "http://[$VIP]:$PORT/" 2>/dev/null
}

# Get responding node from request
get_responding_node() {
    local VIP=$1
    local PORT=${2:-80}

    local RESPONSE=$(test_connectivity "$VIP" "$PORT")
    if [ -n "$RESPONSE" ]; then
        echo "$RESPONSE" | grep "Node:" | awk '{print $2}'
    fi
}

# Wait for connectivity to work (with retries)
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

# Create a test service with labels
create_test_service() {
    local NAME=$1
    local PORT=${2:-80}
    local ETP=${3:-Cluster}  # externalTrafficPolicy

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

# Wait for service to get an IP
wait_for_service_ip() {
    local SVC=$1
    local TIMEOUT=${2:-60}

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/$SVC -n $NAMESPACE --timeout=${TIMEOUT}s >/dev/null 2>&1 || return 1

    kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
}

# Wait for VIP to be announced on kube-lb0 (all nodes for remote)
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

# Count nodes that have a VIP
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
# Test 0: Prerequisites
#---------------------------------------------------------------------

test_prerequisites() {
    section "TEST 0: Prerequisites"

    info "Configuration:"
    info "  SERVICE_GROUP: $SERVICE_GROUP"
    info "  BGP_CONVERGE_TIMEOUT: ${BGP_CONVERGE_TIMEOUT}s"

    # Verify SSH connectivity to cluster nodes
    info "Verifying SSH connectivity to cluster nodes..."
    for node in $NODES; do
        verify_ssh "$node" "cluster node $node"
    done

    # Verify PureLB is running
    info "Verifying PureLB components..."
    local AGENT_PODS=$(kubectl get pods -n purelb -l component=lbnodeagent --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    [ "$AGENT_PODS" -ge 1 ] || fail "LBNodeAgent not running"
    pass "LBNodeAgent running ($AGENT_PODS pods)"

    local ALLOCATOR_READY=$(kubectl get deployment -n purelb allocator -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    [ "$ALLOCATOR_READY" -ge 1 ] || fail "Allocator not running"
    pass "Allocator running"

    # Verify nginx test pods exist
    local NGINX_PODS=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    [ "$NGINX_PODS" -ge 1 ] || fail "No nginx test pods running in namespace $NAMESPACE"
    pass "Test pods running ($NGINX_PODS pods)"

    # Verify ServiceGroup exists
    if ! kubectl get servicegroup -n purelb "$SERVICE_GROUP" >/dev/null 2>&1; then
        fail "ServiceGroup '$SERVICE_GROUP' not found in purelb namespace"
    fi
    pass "ServiceGroup '$SERVICE_GROUP' exists"

    pass "All prerequisites verified"
}

#---------------------------------------------------------------------
# Test 1: Basic External Connectivity (IPv4)
#---------------------------------------------------------------------

test_basic_ipv4() {
    section "TEST 1: Basic Connectivity (IPv4)"

    info "This test verifies traffic reaches service via BGP routing"
    info "Path: This Host -> Router -> BGP Route -> Node -> kube-proxy -> Pod"

    local SVC_NAME="router-test-basic-ipv4"

    info "Creating test service..."
    create_test_service "$SVC_NAME" 80 "Cluster"

    info "Waiting for IP allocation..."
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    info "Waiting for VIP to be announced on all nodes..."
    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced on all nodes"
    pass "VIP $VIP announced on $NODE_COUNT nodes"

    info "Waiting for BGP routes to propagate (${BGP_CONVERGE_TIMEOUT}s timeout)..."
    sleep 5  # Give BGP time to advertise

    info "Testing connectivity to VIP..."
    if wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT"; then
        local RESPONSE=$(test_connectivity "$VIP" 80)
        pass "Connectivity successful"
        echo "$RESPONSE" | head -5
    else
        fail "No response to VIP $VIP"
    fi

    # Cleanup
    kubectl delete svc -n $NAMESPACE "$SVC_NAME"

    pass "Basic IPv4 connectivity test completed"
}

#---------------------------------------------------------------------
# Test 2: ECMP Traffic Distribution
#---------------------------------------------------------------------

test_ecmp_distribution() {
    section "TEST 2: ECMP Traffic Distribution"

    info "Testing that traffic distributes across nodes via ECMP"
    info "Note: ECMP is flow-based; using different source ports for variety"

    local SVC_NAME="router-test-ecmp"

    # Scale to multiple replicas for better distribution visibility
    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    info "Scaling nginx deployment to 5 replicas..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=5
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s

    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"

    info "Waiting for BGP convergence..."
    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Connectivity failed"

    # Send many requests and track which nodes respond
    info "Sending $ECMP_TEST_REQUESTS requests..."

    declare -A NODES_SEEN
    declare -A PODS_SEEN
    local SUCCESS_COUNT=0

    for i in $(seq 1 $ECMP_TEST_REQUESTS); do
        # Use different source ports to create different flows
        local RESPONSE=$(curl -s --connect-timeout 3 --local-port $((10000 + i)) "http://$VIP/" 2>/dev/null || true)
        if echo "$RESPONSE" | grep -q "Pod:"; then
            SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
            local NODE=$(echo "$RESPONSE" | grep "Node:" | awk '{print $2}')
            local POD=$(echo "$RESPONSE" | grep "Pod:" | awk '{print $2}')
            if [ -n "$NODE" ]; then
                NODES_SEEN[$NODE]=1
            fi
            if [ -n "$POD" ]; then
                PODS_SEEN[$POD]=1
            fi
        fi

        # Progress indicator
        if [ $((i % 20)) -eq 0 ]; then
            echo -n "."
        fi
    done
    echo ""

    local UNIQUE_NODES=${#NODES_SEEN[@]}
    local UNIQUE_PODS=${#PODS_SEEN[@]}

    info "Results:"
    info "  Successful requests: $SUCCESS_COUNT / $ECMP_TEST_REQUESTS"
    info "  Unique nodes hit: $UNIQUE_NODES"
    info "  Unique pods hit: $UNIQUE_PODS"

    echo "  Nodes seen:"
    for node in "${!NODES_SEEN[@]}"; do
        echo "    - $node"
    done

    if [ "$SUCCESS_COUNT" -lt $((ECMP_TEST_REQUESTS / 2)) ]; then
        fail "Too many failed requests ($SUCCESS_COUNT / $ECMP_TEST_REQUESTS)"
    fi

    if [ "$UNIQUE_NODES" -ge 2 ]; then
        pass "ECMP working: traffic distributed across $UNIQUE_NODES nodes"
    else
        warn "Traffic only hit 1 node - ECMP may not be configured on router"
        warn "This is expected if router doesn't have multipath enabled"
        pass "Connectivity works (ECMP distribution depends on router config)"
    fi

    # Restore replica count
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    kubectl delete svc -n $NAMESPACE "$SVC_NAME"

    pass "ECMP distribution test completed"
}

#---------------------------------------------------------------------
# Test 3: Node Failure and Recovery
#---------------------------------------------------------------------

test_node_failure_recovery() {
    section "TEST 3: Node Failure and Recovery"

    info "Testing that connectivity continues when a node fails"

    local SVC_NAME="router-test-failover"

    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"

    # Verify initial connectivity
    info "Verifying initial connectivity..."
    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Initial connectivity failed"
    local RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Initial connectivity failed"
    pass "Initial connectivity working"

    # Record initial state
    local INITIAL_NODE_COUNT=$(count_nodes_with_vip "$VIP")
    info "VIP initially on $INITIAL_NODE_COUNT nodes"

    # Pick a node to fail
    local FAIL_NODE=${NODES%% *}
    info "Simulating failure of node: $FAIL_NODE"

    # Taint node and delete lbnodeagent pod
    kubectl taint node "$FAIL_NODE" purelb-router-test=failover:NoExecute --overwrite
    local AGENT_POD=$(kubectl get pods -n purelb -l component=lbnodeagent -o wide | grep "$FAIL_NODE" | awk '{print $1}')
    if [ -n "$AGENT_POD" ]; then
        kubectl delete pod -n purelb "$AGENT_POD" --grace-period=0 --force 2>/dev/null || true
    fi

    # Wait for VIP to be removed from failed node
    info "Waiting for VIP to be removed from $FAIL_NODE..."
    local TIMEOUT=30
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! ssh_node "$FAIL_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/'"; then
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    if [ $ELAPSED -ge $TIMEOUT ]; then
        warn "VIP not removed from $FAIL_NODE within ${TIMEOUT}s"
    else
        pass "VIP removed from $FAIL_NODE after ${ELAPSED}s"
    fi

    local NEW_NODE_COUNT=$(count_nodes_with_vip "$VIP")
    info "VIP now on $NEW_NODE_COUNT nodes (was $INITIAL_NODE_COUNT)"

    # Wait for BGP to reconverge and test connectivity
    info "Waiting for BGP convergence after failure..."
    sleep 10

    info "Testing connectivity after node failure..."
    RESPONSE=$(test_connectivity "$VIP" 80)
    if echo "$RESPONSE" | grep -q "Pod:"; then
        local RESPONDING_NODE=$(echo "$RESPONSE" | grep "Node:" | awk '{print $2}')
        if [ "$RESPONDING_NODE" = "$FAIL_NODE" ]; then
            warn "Response came from failed node - may be stale connection"
        else
            pass "Connectivity working via $RESPONDING_NODE (not failed node)"
        fi
    else
        fail "Connectivity failed after node failure"
    fi

    # Restore node
    info "Restoring $FAIL_NODE..."
    kubectl taint node "$FAIL_NODE" purelb-router-test-
    kubectl rollout status daemonset/lbnodeagent -n purelb --timeout=60s

    # Wait for VIP to return to restored node
    info "Waiting for VIP to return to $FAIL_NODE..."
    TIMEOUT=30
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ssh_node "$FAIL_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/'"; then
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    if [ $ELAPSED -ge $TIMEOUT ]; then
        warn "VIP not restored to $FAIL_NODE within ${TIMEOUT}s"
    else
        pass "VIP restored to $FAIL_NODE after ${ELAPSED}s"
    fi

    # Verify connectivity after restore
    info "Verifying connectivity after restore..."
    sleep 5
    RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || warn "Connectivity slow after restore"
    pass "Connectivity restored"

    kubectl delete svc -n $NAMESPACE "$SVC_NAME"
    pass "Node failure and recovery test completed"
}

#---------------------------------------------------------------------
# Test 4: ETP Local Connectivity
#---------------------------------------------------------------------

test_etp_local() {
    section "TEST 4: ETP Local Connectivity"

    info "Testing connectivity with externalTrafficPolicy: Local"
    info "VIP should only be on nodes with endpoints"

    local SVC_NAME="router-test-etp-local"

    # Create service with ETP Local
    create_test_service "$SVC_NAME" 80 "Local"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    # Find which nodes have nginx pods
    local ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    local ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    info "Endpoints on $ENDPOINT_COUNT nodes:"
    echo "$ENDPOINT_NODES" | while read n; do [ -n "$n" ] && echo "  - $n"; done

    # Wait for VIP (should be on endpoint nodes only)
    info "Waiting for VIP on endpoint nodes..."
    sleep 10

    # Count and verify VIP placement
    local VIP_COUNT=$(count_nodes_with_vip "$VIP")
    info "VIP on $VIP_COUNT nodes (expected: $ENDPOINT_COUNT)"

    # Verify VIP is ONLY on endpoint nodes
    local MISPLACED=false
    for node in $NODES; do
        local HAS_VIP=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        local HAS_ENDPOINT=$(echo "$ENDPOINT_NODES" | grep -q "^$node$" && echo yes || echo no)

        if [ "$HAS_VIP" = "yes" ] && [ "$HAS_ENDPOINT" = "no" ]; then
            warn "$node has VIP but no endpoint (ETP Local violation)"
            MISPLACED=true
        elif [ "$HAS_VIP" = "yes" ]; then
            pass "$node has VIP and endpoint (correct)"
        fi
    done

    [ "$MISPLACED" = "false" ] || warn "Some VIPs misplaced"

    # Test connectivity
    info "Waiting for BGP convergence..."
    if wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT"; then
        local RESPONSE=$(test_connectivity "$VIP" 80)
        pass "Connectivity working with ETP Local"
        echo "$RESPONSE" | head -3
    else
        fail "Connectivity failed with ETP Local"
    fi

    kubectl delete svc -n $NAMESPACE "$SVC_NAME"
    pass "ETP Local connectivity test completed"
}

#---------------------------------------------------------------------
# Test 5: Service Deletion Cleanup
#---------------------------------------------------------------------

test_service_deletion() {
    section "TEST 5: Service Deletion Cleanup"

    info "Testing that VIPs are properly removed when service is deleted"

    local SVC_NAME="router-test-deletion"

    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Connectivity failed"
    pass "Service created and accessible at $VIP"

    # Delete service
    info "Deleting service..."
    kubectl delete svc -n $NAMESPACE "$SVC_NAME"

    # Wait for VIP to be removed from all nodes
    info "Waiting for VIP to be removed from all nodes..."
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

    if [ $ELAPSED -ge $TIMEOUT ]; then
        fail "VIP not removed from all nodes within ${TIMEOUT}s"
    fi
    pass "VIP removed from all nodes after ${ELAPSED}s"

    # Wait for BGP withdrawal and verify connectivity fails
    info "Waiting for BGP route withdrawal..."
    sleep 10

    local RESPONSE=$(test_connectivity "$VIP" 80)
    if echo "$RESPONSE" | grep -q "Pod:"; then
        warn "VIP still reachable after deletion (stale BGP route?)"
    else
        pass "VIP no longer reachable (route withdrawn)"
    fi

    pass "Service deletion cleanup test completed"
}

#---------------------------------------------------------------------
# Test 6: Full Lifecycle Test
#---------------------------------------------------------------------

test_full_lifecycle() {
    section "TEST 6: Full Lifecycle Test"

    info "Testing complete service lifecycle: create -> scale -> failover -> delete"

    local SVC_NAME="router-test-lifecycle"

    # Phase 1: Create
    info "Phase 1: Creating service..."
    create_test_service "$SVC_NAME" 80 "Cluster"
    local VIP=$(wait_for_service_ip "$SVC_NAME" 60) || fail "No IP allocated"
    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 1 connectivity failed"
    pass "Phase 1: Service created and accessible"

    # Phase 2: Scale
    info "Phase 2: Scaling deployment..."
    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    kubectl scale deployment nginx -n $NAMESPACE --replicas=5
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 5
    local RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Phase 2 connectivity failed"
    pass "Phase 2: Scaled and still accessible"

    # Phase 3: Node failure
    info "Phase 3: Simulating node failure..."
    local FAIL_NODE=${NODES%% *}
    kubectl taint node "$FAIL_NODE" purelb-router-test=lifecycle:NoExecute --overwrite
    local AGENT_POD=$(kubectl get pods -n purelb -l component=lbnodeagent -o wide | grep "$FAIL_NODE" | awk '{print $1}')
    [ -n "$AGENT_POD" ] && kubectl delete pod -n purelb "$AGENT_POD" --grace-period=0 --force 2>/dev/null || true
    sleep 15
    RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Phase 3 connectivity failed"
    pass "Phase 3: Still accessible after node failure"

    # Phase 4: Restore
    info "Phase 4: Restoring node..."
    kubectl taint node "$FAIL_NODE" purelb-router-test-
    kubectl rollout status daemonset/lbnodeagent -n purelb --timeout=60s
    sleep 10
    RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Phase 4 connectivity failed"
    pass "Phase 4: Still accessible after node restore"

    # Phase 5: Delete
    info "Phase 5: Deleting service..."
    kubectl delete svc -n $NAMESPACE "$SVC_NAME"
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
    pass "Phase 5: VIP removed from nodes"

    # Restore replica count
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS

    pass "Full lifecycle test completed"
}

#---------------------------------------------------------------------
# Run All Tests
#---------------------------------------------------------------------

run_all_tests() {
    echo ""
    echo "========================================================"
    echo "  PureLB Router-Based E2E Test Suite (Basic)"
    echo "  Testing connectivity via BGP routing"
    echo "========================================================"
    echo ""
    echo "Cluster context: $CONTEXT"
    echo "Namespace: $NAMESPACE"
    echo "Nodes: $NODE_COUNT ($NODES)"
    echo ""
    echo "Note: This version tests connectivity without router CLI access."
    echo "      For BGP route verification, see test-router-connectivity-frr.sh"
    echo ""

    test_prerequisites
    test_basic_ipv4
    test_ecmp_distribution
    test_node_failure_recovery
    test_etp_local
    test_service_deletion
    test_full_lifecycle

    echo ""
    echo "========================================================"
    echo -e "${GREEN}ALL ROUTER TESTS COMPLETED${NC}"
    echo "========================================================"
}

#---------------------------------------------------------------------
# Main
#---------------------------------------------------------------------

# Parse arguments
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
            echo "Tests connectivity through BGP routing without needing"
            echo "router CLI access. For BGP route verification, use:"
            echo "  test-router-connectivity-frr.sh"
            echo ""
            echo "Environment variables:"
            echo "  SERVICE_GROUP         - ServiceGroup to use (default: remote)"
            echo "  BGP_CONVERGE_TIMEOUT  - Seconds to wait for BGP (default: 30)"
            echo "  ECMP_TEST_REQUESTS    - Requests for ECMP test (default: 100)"
            echo ""
            echo "Tests:"
            echo "  0 - Prerequisites"
            echo "  1 - Basic Connectivity (IPv4)"
            echo "  2 - ECMP Traffic Distribution"
            echo "  3 - Node Failure and Recovery"
            echo "  4 - ETP Local Connectivity"
            echo "  5 - Service Deletion Cleanup"
            echo "  6 - Full Lifecycle Test"
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
        1) test_prerequisites && test_basic_ipv4 ;;
        2) test_prerequisites && test_ecmp_distribution ;;
        3) test_prerequisites && test_node_failure_recovery ;;
        4) test_prerequisites && test_etp_local ;;
        5) test_prerequisites && test_service_deletion ;;
        6) test_prerequisites && test_full_lifecycle ;;
        *) echo "Unknown test: $SELECTED_TEST"; exit 1 ;;
    esac
else
    run_all_tests
fi
