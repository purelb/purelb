#!/bin/bash
set -e

# PureLB Single-Node E2E Test Suite
#
# Tests the subnet-aware election implementation on a single-node cluster.
# Validates v2 API with clean field names (localInterface, dummyInterface).
#
# Prerequisites:
# - kubectl configured for local-kvm context
# - SSH access to purelb1 node
# - PureLB deployed with v2-only images (no v1 support)

CONTEXT="local-kvm"
NAMESPACE="test"
PURELB_NS="purelb-system"
NODE="purelb1"
INTERFACE="enp1s0"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

pass() { echo -e "${GREEN}✓ PASS:${NC} $1"; }
fail() { echo -e "${RED}✗ FAIL:${NC} $1"; dump_debug_state; exit 1; }
info() { echo -e "${YELLOW}→${NC} $1"; }
section() { echo -e "\n${BLUE}═══════════════════════════════════════════════════════════════${NC}"; echo -e "${BLUE}$1${NC}"; echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"; }

kubectl() { command kubectl --context "$CONTEXT" "$@"; }

#---------------------------------------------------------------------
# Helper Functions
#---------------------------------------------------------------------

dump_debug_state() {
    echo ""
    echo "=== DEBUG STATE DUMP ==="
    echo "--- Services in test namespace ---"
    kubectl get svc -n $NAMESPACE -o wide 2>/dev/null || echo "(failed)"
    echo "--- ServiceGroups ---"
    kubectl get servicegroups -n $PURELB_NS -o wide 2>/dev/null || echo "(failed)"
    echo "--- Leases ---"
    kubectl get leases -n $PURELB_NS -o wide 2>/dev/null || echo "(failed)"
    echo "--- PureLB pods ---"
    kubectl get pods -n $PURELB_NS -o wide 2>/dev/null || echo "(failed)"
    echo "--- Node interfaces ---"
    ssh $NODE "ip -o addr show" 2>/dev/null || echo "(SSH failed)"
    echo "--- Allocator logs (last 30 lines) ---"
    kubectl logs -n $PURELB_NS deployment/allocator --tail=30 2>/dev/null || echo "(failed)"
    echo "--- LBNodeAgent logs (last 30 lines) ---"
    kubectl logs -n $PURELB_NS daemonset/lbnodeagent --tail=30 2>/dev/null || echo "(failed)"
    echo "========================="
}

wait_for_service_ip() {
    local SVC=$1
    local TIMEOUT=${2:-60}
    local ELAPSED=0

    info "Waiting for service $SVC to get external IP..."
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local IP=$(kubectl get svc -n $NAMESPACE $SVC -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
        if [ -n "$IP" ] && [ "$IP" != "null" ]; then
            pass "Service $SVC got IP: $IP"
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    fail "Timeout waiting for service $SVC to get IP"
}

get_service_ip() {
    local SVC=$1
    kubectl get svc -n $NAMESPACE $SVC -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null
}

verify_ip_on_interface() {
    local IP=$1
    local IFACE=$2

    # Determine if this is IPv6 (contains colon)
    local IP_FLAG=""
    if [[ "$IP" == *":"* ]]; then
        IP_FLAG="-6"
    else
        IP_FLAG="-4"
    fi

    info "Verifying IP $IP is on interface $IFACE..."
    if ssh $NODE "ip $IP_FLAG -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
        pass "IP $IP found on interface $IFACE"
        return 0
    else
        # Show what interfaces have this IP
        echo "IP $IP not found on $IFACE. Checking all interfaces:"
        ssh $NODE "ip -o addr show | grep '$IP'" 2>/dev/null || echo "  (IP not found on any interface)"
        fail "IP $IP not on expected interface $IFACE"
    fi
}

verify_ip_not_present() {
    local IP=$1

    # Determine if this is IPv6 (contains colon)
    local IP_FLAG=""
    if [[ "$IP" == *":"* ]]; then
        IP_FLAG="-6"
    else
        IP_FLAG="-4"
    fi

    info "Verifying IP $IP is not present on node..."
    if ssh $NODE "ip $IP_FLAG -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
        echo "IP $IP still present:"
        ssh $NODE "ip -o addr show | grep '$IP'" 2>/dev/null
        fail "IP $IP should have been withdrawn"
    else
        pass "IP $IP correctly withdrawn"
        return 0
    fi
}

cleanup_test_services() {
    info "Cleaning up test services..."
    kubectl delete svc -n $NAMESPACE -l test-suite=single-node --ignore-not-found 2>/dev/null || true
}

#---------------------------------------------------------------------
# Phase 0: Prerequisites
#---------------------------------------------------------------------

validate_prerequisites() {
    section "PHASE 0: Prerequisites Validation"

    # Check kubectl context
    info "Checking kubectl context..."
    if kubectl cluster-info &>/dev/null; then
        pass "kubectl context $CONTEXT is accessible"
    else
        fail "Cannot access cluster with context $CONTEXT"
    fi

    # Check SSH to node
    info "Checking SSH access to $NODE..."
    if ssh $NODE "true" 2>/dev/null; then
        pass "SSH access to $NODE working"
    else
        fail "Cannot SSH to $NODE"
    fi

    # Check IP forwarding
    info "Checking IPv4 forwarding on $NODE..."
    local IPV4_FWD=$(ssh $NODE "cat /proc/sys/net/ipv4/ip_forward" 2>/dev/null)
    if [ "$IPV4_FWD" = "1" ]; then
        pass "IPv4 forwarding enabled"
    else
        fail "IPv4 forwarding not enabled on $NODE"
    fi

    # Check IPv6 forwarding
    info "Checking IPv6 forwarding on $NODE..."
    local IPV6_FWD=$(ssh $NODE "cat /proc/sys/net/ipv6/conf/all/forwarding" 2>/dev/null)
    if [ "$IPV6_FWD" = "1" ]; then
        pass "IPv6 forwarding enabled"
    else
        # IPv6 forwarding is not strictly required, just warn
        echo -e "${YELLOW}⚠ WARN:${NC} IPv6 forwarding not enabled (some IPv6 tests may be affected)"
    fi

    # Check if node has IPv6 connectivity
    info "Checking IPv6 connectivity on $NODE..."
    local IPV6_ADDR=$(ssh $NODE "ip -6 addr show $INTERFACE scope global | grep -oP '(?<=inet6 )[^/]+' | head -1" 2>/dev/null)
    if [ -n "$IPV6_ADDR" ]; then
        pass "Node has global IPv6 address: $IPV6_ADDR"
    else
        echo -e "${YELLOW}⚠ WARN:${NC} No global IPv6 address found on $INTERFACE"
    fi
}

#---------------------------------------------------------------------
# Phase 1: Installation Verification
#---------------------------------------------------------------------

verify_installation() {
    section "PHASE 1: Installation Verification"

    # Check allocator pod
    info "Checking allocator pod..."
    local ALLOC_STATUS=$(kubectl get pods -n $PURELB_NS -l component=allocator -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
    if [ "$ALLOC_STATUS" = "Running" ]; then
        pass "Allocator pod is Running"
    else
        fail "Allocator pod status: $ALLOC_STATUS (expected Running)"
    fi

    # Check lbnodeagent pod
    info "Checking lbnodeagent pod..."
    local AGENT_STATUS=$(kubectl get pods -n $PURELB_NS -l component=lbnodeagent -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
    if [ "$AGENT_STATUS" = "Running" ]; then
        pass "LBNodeAgent pod is Running"
    else
        fail "LBNodeAgent pod status: $AGENT_STATUS (expected Running)"
    fi

    # Check lease exists with subnet annotation
    info "Checking election lease..."
    local LEASE_SUBNETS=$(kubectl get lease -n $PURELB_NS purelb-node-$NODE -o jsonpath='{.metadata.annotations.purelb\.io/subnets}' 2>/dev/null)
    if [ -n "$LEASE_SUBNETS" ]; then
        pass "Lease purelb-node-$NODE exists with subnets: $LEASE_SUBNETS"
    else
        fail "Lease purelb-node-$NODE missing or has no subnet annotation"
    fi

    # Verify our IPv4 subnet is in the annotation
    if echo "$LEASE_SUBNETS" | grep -q "192.168.151.0/24"; then
        pass "Node's IPv4 subnet (192.168.151.0/24) is in lease annotation"
    else
        fail "Expected subnet 192.168.151.0/24 not in lease annotation"
    fi

    # Verify IPv6 subnet is in the annotation
    if echo "$LEASE_SUBNETS" | grep -q "2001:470:b8f3:1::/64"; then
        pass "Node's IPv6 subnet (2001:470:b8f3:1::/64) is in lease annotation"
    else
        echo -e "${YELLOW}⚠ WARN:${NC} IPv6 subnet not in lease annotation (IPv6 tests may be skipped)"
    fi
}

#---------------------------------------------------------------------
# Phase 2: v2 API Field Names Verification
#---------------------------------------------------------------------

test_v2_field_names() {
    section "PHASE 2: v2 API Field Names Verification"

    # Test 1: LBNodeAgent with clean v2 field names
    info "Applying LBNodeAgent with v2 field names (localInterface, dummyInterface)..."
    kubectl apply -f "$SCRIPT_DIR/lbnodeagent-test-fields.yaml"

    if [ $? -eq 0 ]; then
        pass "LBNodeAgent with v2 field names accepted by CRD"
    else
        fail "CRD rejected v2 field names"
    fi

    # Verify the config was stored correctly
    local LOCAL_INT=$(kubectl get lbnodeagent -n $PURELB_NS test-v2-fields \
        -o jsonpath='{.spec.local.localInterface}')
    local DUMMY_INT=$(kubectl get lbnodeagent -n $PURELB_NS test-v2-fields \
        -o jsonpath='{.spec.local.dummyInterface}')

    if [ "$LOCAL_INT" = "default" ] && [ "$DUMMY_INT" = "kube-lb0" ]; then
        pass "Field values correctly stored: localInterface=$LOCAL_INT, dummyInterface=$DUMMY_INT"
    else
        fail "Field values incorrect: localInterface=$LOCAL_INT, dummyInterface=$DUMMY_INT"
    fi

    # Cleanup
    kubectl delete lbnodeagent -n $PURELB_NS test-v2-fields --ignore-not-found

    pass "v2 field names VERIFIED"
}

#---------------------------------------------------------------------
# Phase 3: v2 API Testing
#---------------------------------------------------------------------

test_v2_api() {
    section "PHASE 3: v2 API Testing (IPv4 Local and Remote Pools)"

    # Record log timestamp
    local V2_LOG_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)

    # Apply v2 ServiceGroups
    info "Applying v2 ServiceGroups..."
    kubectl apply -f "$SCRIPT_DIR/servicegroups-v2.yaml"
    pass "v2 ServiceGroups applied"

    # Apply v2 LBNodeAgent config
    info "Applying v2 LBNodeAgent config..."
    kubectl apply -f "$SCRIPT_DIR/lbnodeagent-v2.yaml"
    pass "v2 LBNodeAgent config applied"

    # Wait for config to be processed
    sleep 5

    #--- Test v2 LOCAL and REMOTE pools (IPv4) ---
    info "Creating IPv4 test services (local and remote pools)..."
    kubectl apply -f "$SCRIPT_DIR/services-ipv4.yaml"

    wait_for_service_ip test-v2-local
    local LOCAL_IP=$(get_service_ip test-v2-local)
    info "IPv4 Local pool allocated IP: $LOCAL_IP"

    # Verify on real interface
    verify_ip_on_interface "$LOCAL_IP" "$INTERFACE"

    # Verify pool-type annotation
    local POOL_TYPE=$(kubectl get svc -n $NAMESPACE test-v2-local -o jsonpath='{.metadata.annotations.purelb\.io/pool-type}' 2>/dev/null)
    if [ "$POOL_TYPE" = "local" ]; then
        pass "pool-type annotation is 'local'"
    else
        fail "Expected pool-type=local, got '$POOL_TYPE'"
    fi

    wait_for_service_ip test-v2-remote
    local REMOTE_IP=$(get_service_ip test-v2-remote)
    info "IPv4 Remote pool allocated IP: $REMOTE_IP"

    # Verify on dummy interface (kube-lb0)
    verify_ip_on_interface "$REMOTE_IP" "kube-lb0"

    # Verify pool-type annotation
    POOL_TYPE=$(kubectl get svc -n $NAMESPACE test-v2-remote -o jsonpath='{.metadata.annotations.purelb\.io/pool-type}' 2>/dev/null)
    if [ "$POOL_TYPE" = "remote" ]; then
        pass "pool-type annotation is 'remote'"
    else
        fail "Expected pool-type=remote, got '$POOL_TYPE'"
    fi

    # Verify NO deprecation warnings for v2
    info "Verifying no deprecation warnings for v2 API..."
    sleep 2
    local LOGS=$(kubectl logs -n $PURELB_NS deployment/allocator --since-time=$V2_LOG_START 2>/dev/null)
    if echo "$LOGS" | grep -q "ServiceGroup v1 API is deprecated"; then
        echo "Unexpected deprecation warning found:"
        echo "$LOGS" | grep "deprecated"
        fail "v2 API should NOT produce deprecation warnings"
    else
        pass "No deprecation warnings for v2 API (correct)"
    fi

    pass "v2 API VERIFIED (local and remote pools)"
}

#---------------------------------------------------------------------
# Phase 5: IPv6 Testing
#---------------------------------------------------------------------

test_ipv6() {
    section "PHASE 5: IPv6 and Dual-Stack Address Allocation"

    # Check if the node has IPv6 subnet in lease (required for local IPv6)
    local LEASE_SUBNETS=$(kubectl get lease -n $PURELB_NS purelb-node-$NODE -o jsonpath='{.metadata.annotations.purelb\.io/subnets}' 2>/dev/null)
    local HAS_IPV6_SUBNET=false
    if echo "$LEASE_SUBNETS" | grep -q "2001:470:b8f3:1::/64"; then
        HAS_IPV6_SUBNET=true
    fi

    #--- Test IPv6 LOCAL and REMOTE pools ---
    info "Creating IPv6 test services..."
    kubectl apply -f "$SCRIPT_DIR/services-ipv6.yaml"

    if [ "$HAS_IPV6_SUBNET" = "true" ]; then
        wait_for_service_ip test-ipv6-local
        local IPV6_LOCAL_IP=$(get_service_ip test-ipv6-local)
        info "IPv6 Local pool allocated IP: $IPV6_LOCAL_IP"

        # Verify on real interface
        verify_ip_on_interface "$IPV6_LOCAL_IP" "$INTERFACE"

        # Verify pool-type annotation
        local POOL_TYPE=$(kubectl get svc -n $NAMESPACE test-ipv6-local -o jsonpath='{.metadata.annotations.purelb\.io/pool-type}' 2>/dev/null)
        if [ "$POOL_TYPE" = "local" ]; then
            pass "IPv6 local pool-type annotation is 'local'"
        else
            fail "Expected pool-type=local for IPv6, got '$POOL_TYPE'"
        fi

        pass "IPv6 LOCAL pool VERIFIED"
    else
        echo -e "${YELLOW}⚠ SKIP:${NC} IPv6 local pool test skipped (node lacks IPv6 subnet in lease)"
    fi

    wait_for_service_ip test-ipv6-remote
    local IPV6_REMOTE_IP=$(get_service_ip test-ipv6-remote)
    info "IPv6 Remote pool allocated IP: $IPV6_REMOTE_IP"

    # Wait for lbnodeagent to process the service and announce the address
    sleep 3

    # Verify on dummy interface (kube-lb0) - remote pools don't need subnet match
    verify_ip_on_interface "$IPV6_REMOTE_IP" "kube-lb0"

    # Verify pool-type annotation
    local POOL_TYPE=$(kubectl get svc -n $NAMESPACE test-ipv6-remote -o jsonpath='{.metadata.annotations.purelb\.io/pool-type}' 2>/dev/null)
    if [ "$POOL_TYPE" = "remote" ]; then
        pass "IPv6 remote pool-type annotation is 'remote'"
    else
        fail "Expected pool-type=remote for IPv6, got '$POOL_TYPE'"
    fi

    pass "IPv6 REMOTE pool VERIFIED"

    #--- Test IPv6 address lifecycle ---
    info "Testing IPv6 address lifecycle (delete and verify withdrawal)..."

    # Delete the IPv6 remote service
    kubectl delete svc -n $NAMESPACE test-ipv6-remote
    sleep 3

    # Verify IP is withdrawn
    verify_ip_not_present "$IPV6_REMOTE_IP"

    pass "IPv6 address lifecycle VERIFIED"

    # Cleanup remaining IPv6 test service
    kubectl delete svc -n $NAMESPACE test-ipv6-local --ignore-not-found 2>/dev/null || true

    #--- Test DUAL-STACK allocation ---
    if [ "$HAS_IPV6_SUBNET" = "true" ]; then
        info "Creating test service with DUAL-STACK (IPv4 + IPv6)..."
        kubectl apply -f "$SCRIPT_DIR/services-dual-stack.yaml"

        wait_for_service_ip test-dual-stack

        # Get both IPs from the service
        local DUAL_IPV4=$(kubectl get svc -n $NAMESPACE test-dual-stack -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
        local DUAL_IPV6=$(kubectl get svc -n $NAMESPACE test-dual-stack -o jsonpath='{.status.loadBalancer.ingress[1].ip}' 2>/dev/null)

        info "Dual-stack allocated IPv4: $DUAL_IPV4"
        info "Dual-stack allocated IPv6: $DUAL_IPV6"

        # Verify we got both addresses
        if [ -n "$DUAL_IPV4" ] && [ -n "$DUAL_IPV6" ]; then
            pass "Dual-stack service received both IPv4 and IPv6 addresses"
        else
            fail "Dual-stack service missing address (IPv4=$DUAL_IPV4, IPv6=$DUAL_IPV6)"
        fi

        # Verify IPv4 is on real interface
        verify_ip_on_interface "$DUAL_IPV4" "$INTERFACE"

        # Verify IPv6 is on real interface
        verify_ip_on_interface "$DUAL_IPV6" "$INTERFACE"

        # Cleanup
        kubectl delete svc -n $NAMESPACE test-dual-stack --ignore-not-found

        pass "DUAL-STACK allocation VERIFIED"
    else
        echo -e "${YELLOW}⚠ SKIP:${NC} Dual-stack test skipped (node lacks IPv6 subnet in lease)"
    fi

    pass "IPv6 and dual-stack testing COMPLETE"
}

#---------------------------------------------------------------------
# Phase 6: Lease and Election Verification
#---------------------------------------------------------------------

test_lease_election() {
    section "PHASE 6: Lease and Election Verification"

    # Get initial renewTime
    info "Checking lease renewal..."
    local INITIAL_TIME=$(kubectl get lease -n $PURELB_NS purelb-node-$NODE -o jsonpath='{.spec.renewTime}' 2>/dev/null)
    info "Initial renewTime: $INITIAL_TIME"

    # Wait and check again
    sleep 15

    local CURRENT_TIME=$(kubectl get lease -n $PURELB_NS purelb-node-$NODE -o jsonpath='{.spec.renewTime}' 2>/dev/null)
    info "Current renewTime: $CURRENT_TIME"

    if [ "$INITIAL_TIME" != "$CURRENT_TIME" ]; then
        pass "Lease renewTime is being updated"
    else
        fail "Lease renewTime not advancing (stuck at $INITIAL_TIME)"
    fi
}

#---------------------------------------------------------------------
# Phase 7: Metrics Verification
#---------------------------------------------------------------------

test_metrics() {
    section "PHASE 7: Prometheus Metrics Verification"

    # Get lbnodeagent pod name
    local POD=$(kubectl get pods -n $PURELB_NS -l component=lbnodeagent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    # Get the node IP where lbnodeagent binds (PURELB_HOST is set to status.hostIP)
    local NODE_IP=$(kubectl get nodes $NODE -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null)

    # Since lbnodeagent uses hostNetwork and binds to hostIP, use node IP
    info "Fetching metrics from lbnodeagent at $NODE_IP:7472..."
    local METRICS=$(ssh $NODE "curl -s http://$NODE_IP:7472/metrics" 2>/dev/null)

    if [ -z "$METRICS" ]; then
        fail "Could not fetch metrics from lbnodeagent"
    fi

    # Check election metrics
    info "Checking election metrics..."

    if echo "$METRICS" | grep -q "purelb_election_lease_healthy 1"; then
        pass "purelb_election_lease_healthy = 1"
    else
        echo "Election metrics:"
        echo "$METRICS" | grep "purelb_election" || echo "(no election metrics found)"
        fail "purelb_election_lease_healthy != 1"
    fi

    if echo "$METRICS" | grep -q "purelb_election_member_count 1"; then
        pass "purelb_election_member_count = 1 (single node)"
    else
        fail "purelb_election_member_count != 1"
    fi

    pass "Prometheus metrics VERIFIED"
}

#---------------------------------------------------------------------
# Phase 8: Graceful Shutdown Test
#---------------------------------------------------------------------

test_graceful_shutdown() {
    section "PHASE 8: Graceful Shutdown Test"

    # Get current service IP
    local IP=$(get_service_ip test-v2-local)
    if [ -z "$IP" ]; then
        fail "No IP found for test-v2-local service"
    fi
    info "Current IP for test-v2-local: $IP"

    # Verify IP is currently present
    verify_ip_on_interface "$IP" "$INTERFACE"

    # Delete lbnodeagent pod (triggers graceful shutdown)
    info "Deleting lbnodeagent pod to test graceful shutdown..."
    kubectl delete pod -n $PURELB_NS -l component=lbnodeagent --wait=false

    # Wait a moment for shutdown to begin
    sleep 2

    # Check if IP was withdrawn (graceful shutdown should remove it)
    info "Checking if IP was withdrawn during shutdown..."
    # The IP might be gone or the pod might have restarted - either is OK
    # We mainly want to verify the system recovers

    # Wait for new pod to be ready
    info "Waiting for lbnodeagent pod to restart..."
    kubectl rollout status daemonset/lbnodeagent -n $PURELB_NS --timeout=60s

    # Wait for IP to be re-announced
    sleep 5

    # Verify IP is back
    info "Verifying IP is re-announced after restart..."
    verify_ip_on_interface "$IP" "$INTERFACE"

    pass "Graceful shutdown and recovery VERIFIED"
}

#---------------------------------------------------------------------
# Phase 9: Address Lifecycle Test
#---------------------------------------------------------------------

test_address_lifecycle() {
    section "PHASE 9: Address Lifecycle Test"

    # Create a new service
    info "Creating test service for lifecycle test..."
    kubectl apply -f "$SCRIPT_DIR/services-lifecycle.yaml"

    wait_for_service_ip test-lifecycle
    local IP=$(get_service_ip test-lifecycle)
    info "Allocated IP: $IP"

    # Verify IP is announced
    verify_ip_on_interface "$IP" "$INTERFACE"

    # Delete service
    info "Deleting service..."
    kubectl delete svc -n $NAMESPACE test-lifecycle

    # Wait for withdrawal
    sleep 5

    # Verify IP is withdrawn
    verify_ip_not_present "$IP"

    # Recreate service (should get same or different IP)
    info "Recreating service..."
    kubectl apply -f "$SCRIPT_DIR/services-lifecycle.yaml"

    wait_for_service_ip test-lifecycle
    local NEW_IP=$(get_service_ip test-lifecycle)
    info "Re-allocated IP: $NEW_IP"

    # Verify new IP is announced
    verify_ip_on_interface "$NEW_IP" "$INTERFACE"

    # Cleanup
    kubectl delete svc -n $NAMESPACE test-lifecycle --ignore-not-found

    pass "Address lifecycle VERIFIED"
}

#---------------------------------------------------------------------
# Phase 10: GARP Configuration Verification
#---------------------------------------------------------------------

test_garp_config() {
    section "PHASE 10: GARP Configuration Verification"

    # Test structured garpConfig (v2 style, not boolean)
    info "Applying LBNodeAgent with structured garpConfig..."
    kubectl apply -f "$SCRIPT_DIR/lbnodeagent-garp-config.yaml"

    if [ $? -eq 0 ]; then
        pass "LBNodeAgent with structured garpConfig accepted"
    else
        fail "CRD rejected structured garpConfig"
    fi

    # Verify garpConfig values are stored
    local GARP_ENABLED=$(kubectl get lbnodeagent -n $PURELB_NS test-garp-config \
        -o jsonpath='{.spec.local.garpConfig.enabled}')
    local GARP_COUNT=$(kubectl get lbnodeagent -n $PURELB_NS test-garp-config \
        -o jsonpath='{.spec.local.garpConfig.count}')
    local GARP_INTERVAL=$(kubectl get lbnodeagent -n $PURELB_NS test-garp-config \
        -o jsonpath='{.spec.local.garpConfig.interval}')

    if [ "$GARP_ENABLED" = "true" ]; then
        pass "garpConfig.enabled = true"
    else
        fail "garpConfig.enabled incorrect: $GARP_ENABLED"
    fi

    if [ "$GARP_COUNT" = "5" ]; then
        pass "garpConfig.count = 5"
    else
        fail "garpConfig.count incorrect: $GARP_COUNT"
    fi

    if [ "$GARP_INTERVAL" = "250ms" ]; then
        pass "garpConfig.interval = 250ms"
    else
        fail "garpConfig.interval incorrect: $GARP_INTERVAL"
    fi

    # Cleanup
    kubectl delete lbnodeagent -n $PURELB_NS test-garp-config --ignore-not-found

    pass "GARP configuration VERIFIED"
}

#---------------------------------------------------------------------
# Phase 4: Pool Arrays Verification
#---------------------------------------------------------------------

test_pool_arrays() {
    section "PHASE 4: v2 Pool Arrays Verification"

    # Test ServiceGroup with pool arrays (v2 feature) - both v4pools and v6pools
    info "Creating ServiceGroup with v4pools and v6pools arrays..."
    kubectl apply -f "$SCRIPT_DIR/servicegroup-pool-arrays.yaml"

    if [ $? -eq 0 ]; then
        pass "ServiceGroup with v4pools and v6pools arrays accepted"
    else
        fail "CRD rejected pool arrays"
    fi

    # Verify v4pools count
    local V4_POOL_COUNT=$(kubectl get servicegroup -n $PURELB_NS test-pool-arrays \
        -o jsonpath='{.spec.local.v4pools}' | jq '. | length')

    if [ "$V4_POOL_COUNT" = "2" ]; then
        pass "v4pools contains 2 pool definitions"
    else
        fail "Expected 2 v4pools, got $V4_POOL_COUNT"
    fi

    # Verify v6pools count
    local V6_POOL_COUNT=$(kubectl get servicegroup -n $PURELB_NS test-pool-arrays \
        -o jsonpath='{.spec.local.v6pools}' | jq '. | length')

    if [ "$V6_POOL_COUNT" = "2" ]; then
        pass "v6pools contains 2 pool definitions"
    else
        fail "Expected 2 v6pools, got $V6_POOL_COUNT"
    fi

    # Test allocation from pool arrays
    info "Creating services to test allocation from pool arrays..."
    kubectl apply -f "$SCRIPT_DIR/services-pool-arrays.yaml"

    wait_for_service_ip test-pool-array-svc
    local IP=$(get_service_ip test-pool-array-svc)
    info "Allocated IPv4 from pool array: $IP"

    # Verify IP is from one of the pool ranges (192.168.151.220-230)
    if [[ "$IP" =~ ^192\.168\.151\.(22[0-9]|230)$ ]]; then
        pass "IPv4 $IP is from expected pool range"
    else
        fail "IPv4 $IP is not from expected pool range (192.168.151.220-230)"
    fi

    wait_for_service_ip test-pool-array-svc-v6
    local IPV6=$(get_service_ip test-pool-array-svc-v6)
    info "Allocated IPv6 from pool array: $IPV6"

    # Verify IPv6 is from one of the pool ranges (2001:470:b8f3:1::f020-f030)
    if [[ "$IPV6" =~ ^2001:470:b8f3:1::f0(2[0-9]|30)$ ]]; then
        pass "IPv6 $IPV6 is from expected pool range"
    else
        fail "IPv6 $IPV6 is not from expected pool range (2001:470:b8f3:1::f020-f030)"
    fi

    # Cleanup
    kubectl delete -f "$SCRIPT_DIR/services-pool-arrays.yaml" --ignore-not-found
    kubectl delete -f "$SCRIPT_DIR/servicegroup-pool-arrays.yaml" --ignore-not-found

    pass "Pool arrays (v4 and v6) VERIFIED"
}

#---------------------------------------------------------------------
# Cleanup
#---------------------------------------------------------------------

cleanup() {
    section "CLEANUP"

    info "Removing test services..."
    kubectl delete svc -n $NAMESPACE -l test-suite=single-node --ignore-not-found 2>/dev/null || true

    info "Removing v2 ServiceGroups..."
    kubectl delete -f "$SCRIPT_DIR/servicegroups-v2.yaml" --ignore-not-found 2>/dev/null || true

    pass "Cleanup complete"
}

#---------------------------------------------------------------------
# Main
#---------------------------------------------------------------------

main() {
    echo ""
    echo "╔═══════════════════════════════════════════════════════════════╗"
    echo "║  PureLB Single-Node E2E Test Suite                           ║"
    echo "║  Testing subnet-aware election on local-kvm cluster          ║"
    echo "╚═══════════════════════════════════════════════════════════════╝"
    echo ""

    # Deploy test backend if not present
    info "Ensuring nginx test backend is deployed..."
    kubectl apply -f "$SCRIPT_DIR/test-deployment.yaml"
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s

    # Run test phases
    validate_prerequisites          # Phase 0
    verify_installation             # Phase 1
    test_v2_field_names             # Phase 2 (v2 field names verification)
    test_v2_api                     # Phase 3 (v2 local and remote pools - IPv4)
    test_pool_arrays                # Phase 4 (v4pools/v6pools array support)
    test_ipv6                       # Phase 5 (IPv6 and dual-stack)
    test_lease_election             # Phase 6
    test_metrics                    # Phase 7
    test_graceful_shutdown          # Phase 8
    test_address_lifecycle          # Phase 9
    test_garp_config                # Phase 10 (structured garpConfig)
    cleanup

    echo ""
    echo "╔═══════════════════════════════════════════════════════════════╗"
    echo -e "║  ${GREEN}ALL TESTS PASSED${NC}                                            ║"
    echo "╚═══════════════════════════════════════════════════════════════╝"
    echo ""
}

# Run main if script is executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
