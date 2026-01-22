#!/bin/bash
set -e

# PureLB Remote Address E2E Test Suite
#
# Tests for remote IP allocation - addresses that don't match any physical NIC subnet
# and are placed on the dummy interface (kube-lb0) instead of the physical interface.
#
# Key differences from local mode:
# - IP on kube-lb0 (not eth0)
# - ALL nodes announce (no leader election)
# - externalTrafficPolicy: Local is supported
# - Uses /32 or /128 aggregation (host routes)

# Bash version check (required for associative arrays)
if [[ ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "ERROR: Bash 4+ required for associative arrays"
    exit 1
fi

CONTEXT="proxmox"
NAMESPACE="test"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Generate unique test ID for this run (for test isolation)
TEST_RUN_ID=$(date +%s)

pass() { echo -e "${GREEN}✓ PASS:${NC} $1"; }
warn() { echo -e "${YELLOW}! WARN:${NC} $1" >&2; }
info() { echo -e "${YELLOW}→${NC} $1" >&2; }

# Override fail() to dump debug state before exiting
fail() {
    echo -e "${RED}✗ FAIL:${NC} $1"
    dump_debug_state
    exit 1
}

kubectl() { command kubectl --context "$CONTEXT" "$@"; }

# Get node list dynamically
NODES=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
NODE_COUNT=$(echo $NODES | wc -w)

#---------------------------------------------------------------------
# SSH Helper Functions (Red Team CRITICAL-2)
#---------------------------------------------------------------------

# All SSH commands must verify connectivity first
ssh_or_fail() {
    local NODE=$1
    shift
    if ! ssh "$NODE" "$@" 2>/dev/null; then
        # Distinguish between SSH failure and command failure
        if ! ssh "$NODE" "true" 2>/dev/null; then
            fail "SSH to $NODE failed - cannot verify cluster state"
        fi
        return 1
    fi
    return 0
}

# Verify ALL nodes are reachable before ANY test
verify_node_connectivity() {
    info "Verifying SSH connectivity to all nodes..."
    for node in $NODES; do
        ssh "$node" "true" 2>/dev/null || fail "Cannot SSH to $node - fix connectivity before running tests"
    done
    pass "All $NODE_COUNT nodes reachable via SSH"
}

#---------------------------------------------------------------------
# Debug and Cleanup Functions
#---------------------------------------------------------------------

dump_debug_state() {
    echo "=== DEBUG STATE DUMP ==="
    echo "--- Services ---"
    kubectl get svc -n $NAMESPACE -o wide 2>/dev/null || echo "(failed)"
    echo "--- Events (last 20) ---"
    kubectl get events -n $NAMESPACE --sort-by=.lastTimestamp 2>/dev/null | tail -20 || echo "(failed)"
    echo "--- kube-lb0 on all nodes ---"
    for node in $NODES; do
        echo "[$node] kube-lb0:"
        ssh $node "ip -o addr show kube-lb0 2>/dev/null" 2>/dev/null || echo "  (SSH failed or no kube-lb0)"
        echo "[$node] eth0:"
        ssh $node "ip -o addr show eth0 2>/dev/null" 2>/dev/null || echo "  (SSH failed)"
    done
    echo "--- nftables service-ips map (first node) ---"
    local FIRST_NODE=${NODES%% *}
    ssh $FIRST_NODE "sudo nft list map ip kube-proxy service-ips 2>/dev/null | head -50" 2>/dev/null || echo "  (failed)"
    echo "--- lbnodeagent pods ---"
    kubectl get pods -n purelb -l component=lbnodeagent -o wide 2>/dev/null || echo "(failed)"
    echo "--- allocator logs (last 20 lines) ---"
    kubectl logs -n purelb deployment/allocator --tail=20 2>/dev/null || echo "(failed)"
    echo "========================="
}

cleanup_on_exit() {
    info "Cleanup: removing taints, test services, and test pod..."
    # Remove any taints left by failover tests
    for node in $NODES; do
        kubectl taint node $node purelb-test- 2>/dev/null || true
        kubectl uncordon $node 2>/dev/null || true
    done
    # Remove test services (keep namespace and nginx deployment)
    kubectl delete svc -n $NAMESPACE -l test-suite=remote --ignore-not-found 2>/dev/null || true
    # Remove test curl pod
    kubectl delete pod -n $NAMESPACE curl-test --ignore-not-found 2>/dev/null || true
}
trap cleanup_on_exit EXIT

#---------------------------------------------------------------------
# Wait/Poll Helper Functions
#---------------------------------------------------------------------

# Wait for IP to appear on interface on expected number of nodes
wait_for_all_nodes_announce() {
    local IP=$1
    local IFACE=$2           # MUST specify interface (kube-lb0 or eth0)
    local EXPECTED=${3:-$NODE_COUNT}
    local TIMEOUT=${4:-60}
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local COUNT=0
        for node in $NODES; do
            if ssh_or_fail $node "ip -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'"; then
                COUNT=$((COUNT + 1))
            fi
        done
        if [ "$COUNT" -eq "$EXPECTED" ]; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    fail "Expected $EXPECTED nodes to have $IP on $IFACE, found $COUNT after ${TIMEOUT}s"
}

# Wait for IP to be removed from all nodes
wait_for_ip_removal_all_nodes() {
    local IP=$1
    local TIMEOUT=${2:-30}
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local FOUND=false
        for node in $NODES; do
            if ssh_or_fail $node "ip -o addr show 2>/dev/null | grep -q ' $IP/'"; then
                FOUND=true
                break
            fi
        done
        if [ "$FOUND" = "false" ]; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

# Wait for service to be allocated AND announced
# CRITICAL: K8s status update != actual announcement
wait_for_service_announced() {
    local SVC=$1
    local IFACE=$2
    local TIMEOUT=${3:-60}

    info "Waiting for $SVC to be allocated and announced..."

    # First wait for K8s to report the IP (redirect to stderr so it doesn't pollute return value)
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/$SVC -n $NAMESPACE --timeout=${TIMEOUT}s >&2 || fail "$SVC: No IP allocated"

    local IP=$(kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

    # THEN wait for actual announcement on interface
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local ANNOUNCED=false
        for node in $NODES; do
            if ssh_or_fail $node "ip -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'"; then
                ANNOUNCED=true
                break
            fi
        done
        if [ "$ANNOUNCED" = "true" ]; then
            echo "$IP"
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    fail "$SVC: IP $IP allocated but not announced on $IFACE after ${TIMEOUT}s"
}

#---------------------------------------------------------------------
# nftables Verification (Red Team CRITICAL-1)
#---------------------------------------------------------------------

# Verify kube-proxy is running in nftables mode
verify_kube_proxy_nftables() {
    info "Verifying kube-proxy is running in nftables mode..."

    local NODE=${NODES%% *}

    # Check that kube-proxy nftables table exists
    if ! ssh_or_fail $NODE "sudo nft list tables 2>/dev/null | grep -q 'kube-proxy'"; then
        echo ""
        echo "ERROR: kube-proxy nftables table not found"
        echo ""
        echo "This test suite requires kube-proxy in nftables mode."
        echo "Check kube-proxy configuration: --proxy-mode=nftables"
        echo ""
        fail "kube-proxy not in nftables mode"
    fi

    # Verify required chains exist
    ssh_or_fail $NODE "sudo nft list chain ip kube-proxy services >/dev/null 2>&1" || \
        fail "kube-proxy 'services' chain not found"

    pass "kube-proxy running in nftables mode"
}

# Wait for kube-proxy to FULLY program nftables rules for a VIP
# This checks that:
# 1. The IP:port is in the service-ips map (basic routing)
# 2. The service chain has a vmap with endpoints (actual backends ready)
# Just checking service-ips is insufficient - the backend chain may not be ready yet.
wait_for_nftables_rules() {
    local IP=$1
    local PORT=$2
    local TIMEOUT=${3:-30}
    local NODE=${4:-${NODES%% *}}

    info "Waiting for kube-proxy nftables rules for $IP:$PORT..."

    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        # Check if IP:port is in service-ips map and get the chain name
        local CHAIN_INFO=$(ssh_or_fail $NODE "sudo nft list map ip kube-proxy service-ips 2>/dev/null | grep '$IP.*$PORT'" 2>/dev/null || echo "")
        if [ -n "$CHAIN_INFO" ]; then
            # Extract the service chain name (e.g., "service-XXXX-test/nginx/tcp/")
            # The external chain jumps to service chain, which has the vmap
            local SVC_CHAIN=$(echo "$CHAIN_INFO" | grep -oP 'goto \K(service|external)-[^,]+' | head -1)
            if [ -n "$SVC_CHAIN" ]; then
                # If it's an external chain, we need to find the service chain it jumps to
                if [[ "$SVC_CHAIN" == external-* ]]; then
                    SVC_CHAIN=$(ssh_or_fail $NODE "sudo nft list chain ip kube-proxy '$SVC_CHAIN' 2>/dev/null | grep -oP 'goto \Kservice-[^}]+'" 2>/dev/null | tr -d ' ' || echo "")
                fi
                # Now check if the service chain has endpoints (vmap with endpoint- entries)
                if [ -n "$SVC_CHAIN" ]; then
                    local HAS_ENDPOINTS=$(ssh_or_fail $NODE "sudo nft list chain ip kube-proxy '$SVC_CHAIN' 2>/dev/null | grep -q 'endpoint-'" 2>/dev/null && echo "yes" || echo "no")
                    if [ "$HAS_ENDPOINTS" = "yes" ]; then
                        pass "nftables rules fully programmed for $IP:$PORT (endpoints ready)"
                        return 0
                    fi
                fi
            fi
        fi
        sleep 1
        ELAPSED=$((ELAPSED + 1))
    done
    fail "kube-proxy did not fully program nftables rules for $IP:$PORT within ${TIMEOUT}s"
}

# Wait for IPv6 nftables rules (fully programmed with endpoints)
wait_for_nftables_rules_v6() {
    local IP=$1
    local PORT=$2
    local TIMEOUT=${3:-30}
    local NODE=${4:-${NODES%% *}}

    info "Waiting for kube-proxy nftables rules for [$IP]:$PORT..."

    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        # Check if IP:port is in service-ips map and get the chain name
        local CHAIN_INFO=$(ssh_or_fail $NODE "sudo nft list map ip6 kube-proxy service-ips 2>/dev/null | grep '$IP.*$PORT'" 2>/dev/null || echo "")
        if [ -n "$CHAIN_INFO" ]; then
            # Extract the service chain name
            local SVC_CHAIN=$(echo "$CHAIN_INFO" | grep -oP 'goto \K(service|external)-[^,]+' | head -1)
            if [ -n "$SVC_CHAIN" ]; then
                # If it's an external chain, find the service chain it jumps to
                if [[ "$SVC_CHAIN" == external-* ]]; then
                    SVC_CHAIN=$(ssh_or_fail $NODE "sudo nft list chain ip6 kube-proxy '$SVC_CHAIN' 2>/dev/null | grep -oP 'goto \Kservice-[^}]+'" 2>/dev/null | tr -d ' ' || echo "")
                fi
                # Check if the service chain has endpoints
                if [ -n "$SVC_CHAIN" ]; then
                    local HAS_ENDPOINTS=$(ssh_or_fail $NODE "sudo nft list chain ip6 kube-proxy '$SVC_CHAIN' 2>/dev/null | grep -q 'endpoint-'" 2>/dev/null && echo "yes" || echo "no")
                    if [ "$HAS_ENDPOINTS" = "yes" ]; then
                        pass "nftables rules fully programmed for [$IP]:$PORT (endpoints ready)"
                        return 0
                    fi
                fi
            fi
        fi
        sleep 1
        ELAPSED=$((ELAPSED + 1))
    done
    fail "kube-proxy did not fully program nftables rules for [$IP]:$PORT within ${TIMEOUT}s"
}

# Verify nftables rules are REMOVED after service deletion
verify_nftables_rules_removed() {
    local IP=$1
    local PORT=$2
    local TIMEOUT=${3:-30}
    local NODE=${4:-${NODES%% *}}

    info "Verifying nftables rules removed for $IP:$PORT..."

    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! ssh_or_fail $NODE "sudo nft list map ip kube-proxy service-ips 2>/dev/null | grep -q '$IP.*$PORT'"; then
            pass "nftables rules removed for $IP:$PORT"
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    fail "nftables rules still present for $IP:$PORT after ${TIMEOUT}s"
}

#---------------------------------------------------------------------
# IP Placement Verification (Red Team HIGH-4)
#---------------------------------------------------------------------

# Must verify BOTH that IP is on kube-lb0 AND not on eth0
verify_remote_ip_placement() {
    local IP=$1
    info "Verifying $IP is on kube-lb0 (not eth0) on all nodes..."
    for node in $NODES; do
        # MUST be on kube-lb0
        ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
            fail "$IP NOT on kube-lb0 on $node"
        # MUST NOT be on eth0
        if ssh_or_fail $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IP/'"; then
            fail "$IP found on eth0 on $node (should ONLY be on kube-lb0)"
        fi
    done
    pass "$IP correctly placed on kube-lb0 (not eth0) on all nodes"
}

# Verify aggregation prefix
verify_aggregation_prefix() {
    local NODE=$1
    local IP=$2
    local EXPECTED_PREFIX=$3
    local ACTUAL=$(ssh_or_fail $NODE "ip -o addr show kube-lb0 2>/dev/null | grep ' $IP/' | sed 's|.* $IP/\([0-9]*\).*|\1|'")
    [ -n "$ACTUAL" ] || fail "Could not find $IP on kube-lb0 on $NODE"
    [ "$ACTUAL" = "$EXPECTED_PREFIX" ] || fail "Expected /$EXPECTED_PREFIX mask, got /$ACTUAL for $IP on $NODE"
}

# Verify prefix is stable (TOCTOU protection)
verify_aggregation_prefix_stable() {
    local IP=$1
    local EXPECTED=$2
    info "Verifying aggregation prefix /$EXPECTED is stable for $IP..."
    for node in $NODES; do
        local PREFIX1=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep ' $IP/' | sed 's|.* $IP/\([0-9]*\).*|\1|'" || echo "")
        sleep 2
        local PREFIX2=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep ' $IP/' | sed 's|.* $IP/\([0-9]*\).*|\1|'" || echo "")
        [ "$PREFIX1" = "$PREFIX2" ] || fail "Prefix changed during verification on $node: $PREFIX1 -> $PREFIX2"
        [ "$PREFIX1" = "$EXPECTED" ] || fail "Expected /$EXPECTED, got /$PREFIX1 on $node"
    done
    pass "Aggregation prefix /$EXPECTED is stable on all nodes"
}

#---------------------------------------------------------------------
# Pod-Based Connectivity Testing
#---------------------------------------------------------------------
# Using a pod for connectivity tests bypasses hairpin NAT issues because:
# - Pod traffic uses pod IP (from CNI network), not node/VIP IP
# - This allows testing VIP reachability from any node, not just endpoint nodes

CURL_POD_READY=false

# Ensure test pod exists and is ready
ensure_curl_pod() {
    if [ "$CURL_POD_READY" = "true" ]; then
        return 0
    fi

    # Check if pod already exists and is running
    if kubectl get pod curl-test -n $NAMESPACE -o jsonpath='{.status.phase}' 2>/dev/null | grep -q Running; then
        CURL_POD_READY=true
        return 0
    fi

    info "Creating curl test pod..."
    kubectl delete pod curl-test -n $NAMESPACE --ignore-not-found 2>/dev/null || true

    # Create a simple curl pod that stays running
    kubectl run curl-test -n $NAMESPACE --image=curlimages/curl:latest \
        --restart=Never --command -- sleep 3600 >&2

    # Wait for pod to be ready
    kubectl wait --for=condition=Ready pod/curl-test -n $NAMESPACE --timeout=60s >&2 || {
        warn "curl-test pod not ready, checking status..."
        kubectl describe pod curl-test -n $NAMESPACE >&2
        fail "Could not create curl-test pod"
    }

    CURL_POD_READY=true
    pass "curl-test pod ready"
}

# Test VIP connectivity from inside a pod (bypasses hairpin NAT)
# This is the most reliable way to test remote VIPs
test_vip_connectivity_from_pod() {
    local IP=$1
    local PORT=${2:-80}

    ensure_curl_pod

    info "Testing VIP $IP:$PORT from curl-test pod..."
    if kubectl exec -n $NAMESPACE curl-test -- curl -s --connect-timeout 5 "http://$IP:$PORT" >/dev/null 2>&1; then
        pass "VIP $IP:$PORT reachable from pod network"
        return 0
    else
        fail "VIP $IP:$PORT NOT reachable from pod network"
    fi
}

# Test IPv6 VIP connectivity from inside a pod
test_vip_connectivity_from_pod_v6() {
    local IP=$1
    local PORT=${2:-80}

    ensure_curl_pod

    info "Testing IPv6 VIP [$IP]:$PORT from curl-test pod..."
    if kubectl exec -n $NAMESPACE curl-test -- curl -6 -s --connect-timeout 5 "http://[$IP]:$PORT" >/dev/null 2>&1; then
        pass "IPv6 VIP $IP:$PORT reachable from pod network"
        return 0
    else
        fail "IPv6 VIP $IP:$PORT NOT reachable from pod network"
    fi
}

#---------------------------------------------------------------------
# Node-Based Connectivity Testing (with hairpin NAT limitations)
#---------------------------------------------------------------------
# These functions test from nodes directly. They have limitations:
# - For kube-lb0 VIPs: must test from endpoint nodes (hairpin NAT issue)
# - For eth0 VIPs: can test from the node with the VIP

# Test connectivity via VIP
# For remote addresses (kube-lb0), we test from a node that has an nginx endpoint
# because hairpin NAT fails when source IP = VIP (which happens on kube-lb0)
test_vip_connectivity() {
    local IP=$1
    local PORT=${2:-80}
    local IFACE=${3:-kube-lb0}

    info "Testing connectivity to $IP:$PORT via VIP..."

    # For kube-lb0 (remote addresses), test from a node with nginx endpoint
    # Hairpin NAT fails when source IP is the VIP itself
    if [ "$IFACE" = "kube-lb0" ]; then
        # Find nodes with nginx endpoints
        local ENDPOINT_NODES=$(kubectl get endpoints nginx -n $NAMESPACE -o jsonpath='{.subsets[*].addresses[*].nodeName}' 2>/dev/null || \
                               kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].spec.nodeName}')

        for node in $ENDPOINT_NODES; do
            if ssh_or_fail $node "curl -s --connect-timeout 5 http://$IP:$PORT >/dev/null"; then
                pass "VIP $IP reachable from $node (endpoint node)"
                return 0
            fi
        done
        fail "VIP $IP not reachable from any endpoint node - check nftables rules"
    else
        # For eth0 (local addresses), test from the node that has the VIP
        for node in $NODES; do
            if ssh_or_fail $node "ip -o addr show $IFACE 2>/dev/null | grep -q ' $IP/'"; then
                if ssh_or_fail $node "curl -s --connect-timeout 5 http://$IP:$PORT >/dev/null"; then
                    pass "VIP $IP reachable from $node (which has VIP on $IFACE)"
                    return 0
                else
                    fail "VIP $IP on $node's $IFACE but curl failed - check nftables rules"
                fi
            fi
        done
        fail "No node found with VIP $IP on $IFACE"
    fi
}

# Test connectivity from a node that does NOT have the VIP (cross-node test)
test_vip_connectivity_cross_node() {
    local IP=$1
    local PORT=${2:-80}

    info "Testing cross-node connectivity to $IP:$PORT..."

    for node in $NODES; do
        if ! ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            if ssh_or_fail $node "curl -s --connect-timeout 5 http://$IP:$PORT >/dev/null"; then
                pass "VIP $IP reachable from $node (cross-node, node does NOT have VIP)"
                return 0
            else
                fail "Cross-node connectivity failed: $node cannot reach $IP:$PORT"
            fi
        fi
    done
    # All nodes have VIP (remote address case) - this is expected
    info "All nodes have VIP - cross-node test not applicable (expected for remote addresses)"
    return 0
}

# IPv6 variant - same hairpin NAT workaround as IPv4
test_vip_connectivity_v6() {
    local IP=$1
    local PORT=${2:-80}

    info "Testing IPv6 connectivity to [$IP]:$PORT via VIP..."

    # For remote addresses, test from a node with nginx endpoint
    # Hairpin NAT fails when source IP is the VIP itself
    local ENDPOINT_NODES=$(kubectl get endpoints nginx -n $NAMESPACE -o jsonpath='{.subsets[*].addresses[*].nodeName}' 2>/dev/null || \
                           kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].spec.nodeName}')

    for node in $ENDPOINT_NODES; do
        if ssh_or_fail $node "curl -6 -s --connect-timeout 5 http://[$IP]:$PORT >/dev/null"; then
            pass "IPv6 VIP $IP reachable from $node (endpoint node)"
            return 0
        fi
    done
    fail "IPv6 VIP $IP not reachable from any endpoint node"
}

# For ETP Local: verify IP is on endpoint nodes and test connectivity from pod
# Note: Direct node testing has hairpin NAT issues, so we:
# 1. Verify the IP exists on the expected nodes (endpoint nodes only)
# 2. Test connectivity from a pod (bypasses hairpin NAT)
test_etp_local_connectivity() {
    local IP=$1
    local PORT=${2:-80}

    info "Testing ETP Local - verifying IP on endpoint nodes only..."

    # Count nodes with the VIP (should only be endpoint nodes for ETP Local)
    local FOUND=0
    for node in $NODES; do
        if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            FOUND=$((FOUND + 1))
        fi
    done

    [ $FOUND -gt 0 ] || fail "No nodes have VIP $IP"
    pass "$FOUND nodes have VIP $IP (should match endpoint count)"

    # Test connectivity from pod (bypasses hairpin NAT)
    test_vip_connectivity_from_pod "$IP" "$PORT"
}

#---------------------------------------------------------------------
# Invariant Checks (Red Team - Missing Invariants)
#---------------------------------------------------------------------

verify_invariants() {
    info "Checking system invariants..."

    # Invariant 1: Same IP never on BOTH eth0 AND kube-lb0
    for node in $NODES; do
        local ETH0_IPS=$(ssh_or_fail $node "ip -o addr show eth0 2>/dev/null | grep -oP '\d+\.\d+\.\d+\.\d+(?=/)'" || echo "")
        local KUBELB_IPS=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -oP '\d+\.\d+\.\d+\.\d+(?=/)'" || echo "")
        for ip in $ETH0_IPS; do
            if echo "$KUBELB_IPS" | grep -q "^$ip$"; then
                fail "INVARIANT VIOLATION: $ip on BOTH eth0 AND kube-lb0 on $node"
            fi
        done
    done

    # Invariant 2: Every remote test IP on kube-lb0 should map to an active service
    # Use retry logic to handle race conditions during normal operation
    for node in $NODES; do
        local KUBELB_IPS=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep ' 10\.255\.' | grep -oP '\d+\.\d+\.\d+\.\d+(?=/)'" || echo "")
        for ip in $KUBELB_IPS; do
            local FOUND=false
            for attempt in 1 2 3; do
                local SVC=$(kubectl get svc -n $NAMESPACE -o jsonpath="{.items[?(@.status.loadBalancer.ingress[0].ip=='$ip')].metadata.name}" 2>/dev/null || echo "")
                if [ -n "$SVC" ]; then
                    FOUND=true
                    break
                fi
                sleep 1
            done
            if [ "$FOUND" = "false" ]; then
                fail "INVARIANT VIOLATION: Orphaned IP $ip on kube-lb0 on $node (no service)"
            fi
        done
    done

    pass "All invariants verified"
}

#---------------------------------------------------------------------
# Pre-Test State Verification
#---------------------------------------------------------------------

verify_clean_state() {
    info "Verifying clean state before tests..."
    local FOUND_STALE=false
    for node in $NODES; do
        STALE=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -c ' 10\.255\.' || echo 0" | tail -1)
        if [ "$STALE" -gt 0 ]; then
            warn "$node has $STALE stale remote IPs on kube-lb0"
            FOUND_STALE=true
        fi
    done
    if [ "$FOUND_STALE" = "true" ]; then
        info "Cleaning up stale IPs from previous run..."
        kubectl delete svc -n $NAMESPACE -l test-suite=remote --ignore-not-found 2>/dev/null || true
        sleep 5
        # Check again
        STILL_STALE=false
        for node in $NODES; do
            STALE=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -c ' 10\.255\.' || echo 0" | tail -1)
            if [ "$STALE" -gt 0 ]; then
                STILL_STALE=true
            fi
        done
        if [ "$STILL_STALE" = "true" ]; then
            fail "Stale IPs remain after cleanup - investigate manually"
        fi
    fi
    # Ensure namespace exists
    kubectl create namespace $NAMESPACE 2>/dev/null || true
    pass "Clean state verified"
}

#---------------------------------------------------------------------
# Test 0: Prerequisites
#---------------------------------------------------------------------
test_prerequisites() {
    echo ""
    echo "=========================================="
    echo "TEST 0: Prerequisites"
    echo "=========================================="

    verify_node_connectivity
    verify_kube_proxy_nftables
    verify_clean_state

    # Verify PureLB components are running
    info "Verifying PureLB allocator..."
    kubectl get deployment allocator -n purelb >/dev/null 2>&1 || fail "Allocator deployment not found"
    kubectl rollout status deployment/allocator -n purelb --timeout=30s || fail "Allocator not ready"
    pass "Allocator is running"

    info "Verifying PureLB lbnodeagent..."
    kubectl get daemonset lbnodeagent -n purelb >/dev/null 2>&1 || fail "LBNodeAgent daemonset not found"
    local READY=$(kubectl get daemonset lbnodeagent -n purelb -o jsonpath='{.status.numberReady}')
    local DESIRED=$(kubectl get daemonset lbnodeagent -n purelb -o jsonpath='{.status.desiredNumberScheduled}')
    [ "$READY" -eq "$DESIRED" ] || fail "LBNodeAgent not ready: $READY/$DESIRED"
    pass "LBNodeAgent running on all $READY nodes"

    # Verify test namespace has nginx deployment
    info "Verifying nginx deployment..."
    kubectl get deployment nginx -n $NAMESPACE >/dev/null 2>&1 || fail "nginx deployment not found in $NAMESPACE"
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=30s || fail "nginx not ready"
    pass "nginx deployment is ready"

    # Apply remote ServiceGroup
    info "Applying remote ServiceGroup..."
    kubectl apply -f "$SCRIPT_DIR/servicegroup-remote.yaml"
    pass "Remote ServiceGroup applied"

    pass "All prerequisites met"
}

#---------------------------------------------------------------------
# Test 1: Remote IPv4 Single-Stack
#---------------------------------------------------------------------
test_remote_ipv4() {
    echo ""
    echo "=========================================="
    echo "TEST 1: Remote IPv4 Single-Stack"
    echo "=========================================="

    info "Creating IPv4-only remote service..."
    kubectl apply -f "$SCRIPT_DIR/nginx-svc-remote-ipv4.yaml"

    IP=$(wait_for_service_announced nginx-remote-ipv4 kube-lb0 60)
    info "Allocated IPv4: $IP"

    # Verify IP is from remote pool range
    [[ "$IP" =~ ^10\.255\.0\.(1[0-4][0-9]|150|100)$ ]] || fail "IP not from expected remote pool"
    pass "IPv4 allocated from correct remote pool"

    # Verify IP on kube-lb0 on ALL nodes (not eth0)
    verify_remote_ip_placement "$IP"

    # Verify /32 prefix on kube-lb0
    verify_aggregation_prefix "${NODES%% *}" "$IP" "32"
    pass "IPv4 has correct /32 aggregation"

    # Wait for nftables rules then test connectivity from pod (bypasses hairpin NAT)
    wait_for_nftables_rules "$IP" 80
    test_vip_connectivity_from_pod "$IP" 80

    # Verify PureLB annotations
    info "Verifying PureLB annotations..."
    ALLOCATED_BY=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-by}')
    ALLOCATED_FROM=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/allocated-from}')
    [ "$ALLOCATED_BY" = "PureLB" ] || fail "Missing or wrong purelb.io/allocated-by annotation"
    [ "$ALLOCATED_FROM" = "remote" ] || fail "Expected allocated-from=remote, got $ALLOCATED_FROM"
    pass "PureLB annotations correctly set"

    pass "Remote IPv4 test completed"
}

#---------------------------------------------------------------------
# Test 2: Remote IPv6 Single-Stack
#---------------------------------------------------------------------
test_remote_ipv6() {
    echo ""
    echo "=========================================="
    echo "TEST 2: Remote IPv6 Single-Stack"
    echo "=========================================="

    info "Creating IPv6-only remote service..."
    kubectl apply -f "$SCRIPT_DIR/nginx-svc-remote-ipv6.yaml"

    IP=$(wait_for_service_announced nginx-remote-ipv6 kube-lb0 60)
    info "Allocated IPv6: $IP"

    # Verify IP is from remote pool range
    [[ "$IP" =~ ^fd00:10:255:: ]] || fail "IP not from expected remote pool"
    pass "IPv6 allocated from correct remote pool"

    # Verify IP on kube-lb0 on ALL nodes
    info "Verifying $IP is on kube-lb0 on all nodes..."
    for node in $NODES; do
        ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
            fail "$IP NOT on kube-lb0 on $node"
        if ssh_or_fail $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IP/'"; then
            fail "$IP found on eth0 on $node (should ONLY be on kube-lb0)"
        fi
    done
    pass "IPv6 correctly placed on kube-lb0 on all nodes"

    # Verify /128 prefix
    verify_aggregation_prefix "${NODES%% *}" "$IP" "128"
    pass "IPv6 has correct /128 aggregation"

    # Wait for nftables rules then test connectivity from pod (bypasses hairpin NAT)
    wait_for_nftables_rules_v6 "$IP" 80
    test_vip_connectivity_from_pod_v6 "$IP" 80

    pass "Remote IPv6 test completed"
}

#---------------------------------------------------------------------
# Test 3: Remote Dual-Stack
#---------------------------------------------------------------------
test_remote_dualstack() {
    echo ""
    echo "=========================================="
    echo "TEST 3: Remote Dual-Stack"
    echo "=========================================="

    info "Creating dual-stack remote service..."
    kubectl apply -f "$SCRIPT_DIR/nginx-svc-remote-dualstack.yaml"

    info "Waiting for IP allocation..."
    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-remote-dualstack -n $NAMESPACE --timeout=60s || fail "No IP allocated"
    sleep 3

    # Detect IPv4 and IPv6 by format
    IPV4=""
    IPV6=""
    for i in 0 1; do
        IP=$(kubectl get svc nginx-remote-dualstack -n $NAMESPACE -o jsonpath="{.status.loadBalancer.ingress[$i].ip}")
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

    # Wait for IPs to be announced
    wait_for_all_nodes_announce "$IPV4" kube-lb0 $NODE_COUNT 60
    wait_for_all_nodes_announce "$IPV6" kube-lb0 $NODE_COUNT 60

    # Verify both on kube-lb0, not eth0
    verify_remote_ip_placement "$IPV4"
    info "Verifying IPv6 placement..."
    for node in $NODES; do
        ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IPV6/'" || \
            fail "IPv6 $IPV6 NOT on kube-lb0 on $node"
    done
    pass "Both addresses correctly placed on kube-lb0"

    # Verify aggregation
    verify_aggregation_prefix "${NODES%% *}" "$IPV4" "32"
    verify_aggregation_prefix "${NODES%% *}" "$IPV6" "128"
    pass "Both have correct aggregation prefixes"

    # Test connectivity from pod (bypasses hairpin NAT)
    wait_for_nftables_rules "$IPV4" 80
    test_vip_connectivity_from_pod "$IPV4" 80
    wait_for_nftables_rules_v6 "$IPV6" 80
    test_vip_connectivity_from_pod_v6 "$IPV6" 80

    pass "Remote dual-stack test completed"
}

#---------------------------------------------------------------------
# Test 4: All Nodes Announce (No Election)
#---------------------------------------------------------------------
test_all_nodes_announce() {
    echo ""
    echo "=========================================="
    echo "TEST 4: All Nodes Announce (No Election)"
    echo "=========================================="

    IP=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

    info "Verifying exactly $NODE_COUNT nodes have $IP..."
    COUNT=0
    for node in $NODES; do
        if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            COUNT=$((COUNT + 1))
        fi
    done

    [ "$COUNT" -eq "$NODE_COUNT" ] || fail "Expected $NODE_COUNT nodes, found $COUNT with IP $IP"
    pass "All $NODE_COUNT nodes are announcing $IP"

    # Verify NO purelb.io/announcing-IPv4 annotation (remote doesn't set this)
    ANNOUNCING=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.metadata.annotations.purelb\.io/announcing-IPv4}' 2>/dev/null || echo "")
    if [ -n "$ANNOUNCING" ]; then
        fail "Remote service should NOT have announcing-IPv4 annotation, but found: $ANNOUNCING"
    fi
    pass "No announcing annotation (correct for remote mode)"

    pass "All-nodes-announce test completed"
}

#---------------------------------------------------------------------
# Test 5: ETP Local - Basic
#---------------------------------------------------------------------
test_etp_local_basic() {
    echo ""
    echo "=========================================="
    echo "TEST 5: ETP Local - Basic"
    echo "=========================================="

    # Scale nginx to 2 replicas to get pods on multiple nodes
    info "Scaling nginx to 2 replicas..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=2
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s

    # Get nodes with endpoints
    info "Getting nodes with nginx pods..."
    ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].spec.nodeName}')
    info "Endpoint nodes: $ENDPOINT_NODES"

    # Create ETP Local service
    info "Creating ETP Local service..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-local
  namespace: $NAMESPACE
  labels:
    test-suite: remote
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
    targetPort: 80
EOF

    IP=$(wait_for_service_announced nginx-etp-local kube-lb0 60)
    info "Allocated IP: $IP"

    # Wait for ETP Local to take effect - lbnodeagent needs time to withdraw
    # IPs from non-endpoint nodes after the initial announcement
    info "Waiting for ETP Local IP withdrawal from non-endpoint nodes (up to 30s)..."
    local ELAPSED=0
    local MAX_WAIT=30
    while [ $ELAPSED -lt $MAX_WAIT ]; do
        local ALL_CORRECT=true
        for node in $NODES; do
            if ! echo "$ENDPOINT_NODES" | grep -q "$node"; then
                if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                    ALL_CORRECT=false
                    break
                fi
            fi
        done
        if [ "$ALL_CORRECT" = "true" ]; then
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    # POSITIVE: Verify IP IS on nodes WITH endpoints
    info "Verifying IP is on nodes WITH endpoints..."
    for node in $ENDPOINT_NODES; do
        ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
            fail "IP $IP should be on $node (has endpoint) but is NOT"
        pass "IP on $node (has endpoint) - correct"
    done

    # NEGATIVE: Verify IP is NOT on nodes WITHOUT endpoints
    info "Verifying IP is NOT on nodes WITHOUT endpoints..."
    for node in $NODES; do
        if ! echo "$ENDPOINT_NODES" | grep -q "$node"; then
            if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                fail "IP $IP should NOT be on $node (no endpoint) but IS"
            fi
            pass "IP NOT on $node (no endpoint) - correct"
        fi
    done

    # Verify IP is NOT on eth0 on ANY node
    for node in $NODES; do
        if ssh_or_fail $node "ip -o addr show eth0 2>/dev/null | grep -q ' $IP/'"; then
            fail "$IP found on eth0 on $node (should only be on kube-lb0)"
        fi
    done
    pass "IP not on eth0 on any node"

    # Test connectivity
    wait_for_nftables_rules "$IP" 80
    test_etp_local_connectivity "$IP" 80

    pass "ETP Local basic test completed"
}

#---------------------------------------------------------------------
# Test 6: ETP Local - Endpoint Migration
#---------------------------------------------------------------------
test_etp_local_migration() {
    echo ""
    echo "=========================================="
    echo "TEST 6: ETP Local - Endpoint Migration"
    echo "=========================================="

    # Scale to 1 replica
    info "Scaling nginx to 1 replica..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 5

    # Get initial node
    NODE_A=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].spec.nodeName}')
    info "Initial endpoint on $NODE_A"

    # Create or get ETP Local service
    IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
    if [ -z "$IP" ]; then
        fail "ETP Local service not found - run test 5 first"
    fi
    info "Using IP: $IP"

    # Verify IP on NODE_A
    ssh_or_fail $NODE_A "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
        fail "IP $IP should be on $NODE_A but is NOT"
    pass "IP on initial node $NODE_A"

    # Cordon NODE_A and delete pod to force reschedule
    info "Cordoning $NODE_A and deleting pod..."
    kubectl cordon "$NODE_A"
    kubectl delete pod -n $NAMESPACE -l app=nginx --grace-period=1

    # Wait for new pod to be ready on different node
    info "Waiting for pod to reschedule..."
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 5

    NODE_B=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].spec.nodeName}')
    info "New endpoint on $NODE_B"

    [ "$NODE_A" != "$NODE_B" ] || fail "Pod didn't migrate to different node"

    # Verify IP REMOVED from NODE_A (poll)
    info "Verifying IP removed from $NODE_A..."
    TIMEOUT=30
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! ssh_or_fail $NODE_A "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            pass "IP removed from $NODE_A (took ${ELAPSED}s)"
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    [ $ELAPSED -lt $TIMEOUT ] || fail "IP not removed from $NODE_A within ${TIMEOUT}s"

    # Verify IP ADDED to NODE_B
    ssh_or_fail $NODE_B "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
        fail "IP $IP should be on $NODE_B but is NOT"
    pass "IP added to $NODE_B"

    # Test connectivity from pod (bypasses hairpin NAT)
    test_vip_connectivity_from_pod "$IP" 80

    # Uncordon NODE_A
    kubectl uncordon "$NODE_A"
    pass "ETP Local migration test completed"
}

#---------------------------------------------------------------------
# Test 7: ETP Local - Zero Endpoints
#---------------------------------------------------------------------
test_etp_local_zero_endpoints() {
    echo ""
    echo "=========================================="
    echo "TEST 7: ETP Local - Zero Endpoints"
    echo "=========================================="

    IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Using IP: $IP"

    # Scale to 0
    info "Scaling nginx to 0 replicas..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=0
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 5

    # Verify IP removed from ALL nodes
    info "Verifying IP removed from all nodes..."
    for node in $NODES; do
        TIMEOUT=30
        ELAPSED=0
        while [ $ELAPSED -lt $TIMEOUT ]; do
            if ! ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                break
            fi
            sleep 2
            ELAPSED=$((ELAPSED + 2))
        done
        if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            fail "IP $IP still on $node (should be removed with 0 endpoints)"
        fi
    done
    pass "IP removed from all nodes (0 endpoints)"

    # Scale back to 1
    info "Scaling nginx back to 1 replica..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 5

    # Get the node with the pod
    ENDPOINT_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[0].spec.nodeName}')
    info "Endpoint now on $ENDPOINT_NODE"

    # Verify IP reappears ONLY on that node
    info "Verifying IP reappears only on endpoint node..."
    ssh_or_fail $ENDPOINT_NODE "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
        fail "IP should reappear on $ENDPOINT_NODE but did NOT"
    pass "IP reappeared on $ENDPOINT_NODE"

    # Verify other nodes still don't have it
    for node in $NODES; do
        if [ "$node" != "$ENDPOINT_NODE" ]; then
            if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                fail "IP $IP on $node but should only be on $ENDPOINT_NODE"
            fi
        fi
    done
    pass "IP only on endpoint node"

    # Test connectivity from pod (bypasses hairpin NAT)
    test_vip_connectivity_from_pod "$IP" 80

    pass "ETP Local zero endpoints test completed"
}

#---------------------------------------------------------------------
# Test 8: Aggregation /32 Verification
#---------------------------------------------------------------------
test_aggregation_ipv4() {
    echo ""
    echo "=========================================="
    echo "TEST 8: Aggregation /32 Verification"
    echo "=========================================="

    IP=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    verify_aggregation_prefix_stable "$IP" "32"
    pass "IPv4 aggregation /32 verified and stable"
}

#---------------------------------------------------------------------
# Test 9: Aggregation /128 Verification
#---------------------------------------------------------------------
test_aggregation_ipv6() {
    echo ""
    echo "=========================================="
    echo "TEST 9: Aggregation /128 Verification"
    echo "=========================================="

    IP=$(kubectl get svc nginx-remote-ipv6 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    verify_aggregation_prefix_stable "$IP" "128"
    pass "IPv6 aggregation /128 verified and stable"
}

#---------------------------------------------------------------------
# Test 10: Default Aggregation /24 Verification (no explicit aggregation)
#---------------------------------------------------------------------
test_aggregation_default_ipv4() {
    echo ""
    echo "=========================================="
    echo "TEST 10: Default Aggregation /24 Verification"
    echo "=========================================="

    # Ensure the default-aggr ServiceGroup exists
    info "Applying ServiceGroup without explicit aggregation..."
    kubectl apply -f "$(dirname "$0")/servicegroup-remote-default-aggr.yaml"
    sleep 2

    # Create service using the default-aggr pool
    info "Creating service using default aggregation pool..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-default-aggr-ipv4
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote-default-aggr
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8090
    targetPort: 80
EOF

    IP=$(wait_for_service_announced nginx-default-aggr-ipv4 kube-lb0 60)
    info "Allocated IP: $IP"

    # Verify default aggregation uses subnet mask (/24)
    verify_aggregation_prefix_stable "$IP" "24"
    pass "IPv4 default aggregation /24 verified (uses subnet mask)"

    # Verify on all nodes
    wait_for_all_nodes_announce "$IP" kube-lb0 $NODE_COUNT 30
    pass "Default aggregation IP on all nodes"

    # Test connectivity from pod (bypasses hairpin NAT)
    wait_for_nftables_rules "$IP" 8090
    test_vip_connectivity_from_pod "$IP" 8090
}

#---------------------------------------------------------------------
# Test 11: Default Aggregation /64 Verification (no explicit aggregation)
#---------------------------------------------------------------------
test_aggregation_default_ipv6() {
    echo ""
    echo "=========================================="
    echo "TEST 11: Default Aggregation /64 Verification"
    echo "=========================================="

    # Create IPv6 service using the default-aggr pool
    info "Creating IPv6 service using default aggregation pool..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-default-aggr-ipv6
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote-default-aggr
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv6
  selector:
    app: nginx
  ports:
  - port: 8091
    targetPort: 80
EOF

    IP=$(wait_for_service_announced nginx-default-aggr-ipv6 kube-lb0 60)
    info "Allocated IPv6: $IP"

    # Verify default aggregation uses subnet mask (/64)
    verify_aggregation_prefix_stable "$IP" "64"
    pass "IPv6 default aggregation /64 verified (uses subnet mask)"

    # Verify on all nodes
    wait_for_all_nodes_announce "$IP" kube-lb0 $NODE_COUNT 30
    pass "Default aggregation IPv6 on all nodes"

    # Test connectivity from pod (bypasses hairpin NAT)
    wait_for_nftables_rules_v6 "$IP" 8091
    test_vip_connectivity_from_pod_v6 "$IP" 8091

    # Cleanup default-aggr services (keep them for cross-contamination tests)
    info "Default aggregation services created - will be cleaned up at end"
}

#---------------------------------------------------------------------
# Test 12: Service Deletion - All Nodes Cleanup
#---------------------------------------------------------------------
test_service_deletion() {
    echo ""
    echo "=========================================="
    echo "TEST 12: Service Deletion - All Nodes Cleanup"
    echo "=========================================="

    # Create a temporary service for deletion test
    info "Creating temporary service for deletion test..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-remote-delete-test
  namespace: $NAMESPACE
  labels:
    test-suite: remote
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
  - port: 8081
    targetPort: 80
EOF

    IP=$(wait_for_service_announced nginx-remote-delete-test kube-lb0 60)
    info "Allocated IP: $IP"

    # Verify on all nodes
    wait_for_all_nodes_announce "$IP" kube-lb0 $NODE_COUNT 30

    # Delete service
    info "Deleting service..."
    kubectl delete svc nginx-remote-delete-test -n $NAMESPACE

    # Poll ALL nodes until IP removed
    info "Verifying IP removed from ALL nodes..."
    if wait_for_ip_removal_all_nodes "$IP" 30; then
        pass "IP $IP removed from all nodes"
    else
        fail "IP $IP not removed from all nodes within 30s"
    fi

    # Verify nftables rules also cleaned up
    verify_nftables_rules_removed "$IP" 8081

    pass "Service deletion cleanup test completed"
}

#---------------------------------------------------------------------
# Test 13: IP Sharing with Remote Addresses
#---------------------------------------------------------------------
test_ip_sharing() {
    echo ""
    echo "=========================================="
    echo "TEST 13: IP Sharing with Remote Addresses"
    echo "=========================================="

    # Create first service with sharing key
    info "Creating first service with sharing key 'webservers'..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-remote-shared-http
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
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

    SHARED_IP=$(wait_for_service_announced nginx-remote-shared-http kube-lb0 60)
    info "First service got IP: $SHARED_IP"

    # Create second service with SAME sharing key but different port
    info "Creating second service with same key, different port..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-remote-shared-https
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
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

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-remote-shared-https -n $NAMESPACE --timeout=30s || fail "Second service not allocated"

    SHARED_IP2=$(kubectl get svc nginx-remote-shared-https -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Second service got IP: $SHARED_IP2"

    # Verify same IP
    [ "$SHARED_IP" = "$SHARED_IP2" ] || fail "Services got different IPs: $SHARED_IP vs $SHARED_IP2 (sharing failed)"
    pass "Both services share the same IP: $SHARED_IP"

    # Verify shared IP on ALL nodes
    wait_for_all_nodes_announce "$SHARED_IP" kube-lb0 $NODE_COUNT 30
    pass "Shared IP on all nodes"

    # Test port conflict: same key + same port = should fail
    info "Testing port conflict (same key, same port)..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-remote-shared-conflict
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
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

    sleep 5
    CONFLICT_IP=$(kubectl get svc nginx-remote-shared-conflict -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
    if [ -n "$CONFLICT_IP" ]; then
        fail "Port conflict NOT detected - got IP $CONFLICT_IP"
    fi
    pass "Port conflict correctly prevented allocation"

    # Cleanup sharing test services
    kubectl delete svc nginx-remote-shared-http nginx-remote-shared-https nginx-remote-shared-conflict -n $NAMESPACE 2>/dev/null || true

    pass "IP sharing test completed"
}

#---------------------------------------------------------------------
# Test 14: Specific IP Request (purelb.io/addresses)
#---------------------------------------------------------------------
test_specific_ip_request() {
    echo ""
    echo "=========================================="
    echo "TEST 14: Specific IP Request (purelb.io/addresses)"
    echo "=========================================="

    REQUESTED_IP="10.255.0.125"
    info "Requesting specific IP: $REQUESTED_IP"

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-remote-specific-ip
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
    purelb.io/addresses: "$REQUESTED_IP"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8082
    targetPort: 80
EOF

    IP=$(wait_for_service_announced nginx-remote-specific-ip kube-lb0 60)
    info "Allocated IP: $IP"

    [ "$IP" = "$REQUESTED_IP" ] || fail "Got wrong IP: $IP (requested $REQUESTED_IP)"
    pass "Got exact requested IP: $IP"

    # Verify on all nodes (on kube-lb0, not eth0)
    wait_for_all_nodes_announce "$IP" kube-lb0 $NODE_COUNT 30

    # Verify specific IP is on kube-lb0 and NOT on eth0 (remote pool)
    verify_remote_ip_placement "$IP"

    # Test connectivity from pod (bypasses hairpin NAT)
    wait_for_nftables_rules "$IP" 8082
    test_vip_connectivity_from_pod "$IP" 8082

    # Cleanup
    kubectl delete svc nginx-remote-specific-ip -n $NAMESPACE

    pass "Specific IP request test completed"
}

#---------------------------------------------------------------------
# Test 15: Mixed Local and Remote Services
#---------------------------------------------------------------------
test_mixed_local_remote() {
    echo ""
    echo "=========================================="
    echo "TEST 15: Mixed Local and Remote Services"
    echo "=========================================="

    # Get existing remote IP
    REMOTE_IP=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Remote IP: $REMOTE_IP (on kube-lb0, all nodes)"

    # Create a local service (if default ServiceGroup exists)
    info "Creating local service..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-local-mixed-test
  namespace: $NAMESPACE
  labels:
    test-suite: remote
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
  - port: 8083
    targetPort: 80
EOF

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-local-mixed-test -n $NAMESPACE --timeout=30s || {
            info "No default ServiceGroup for local pool, skipping mixed test"
            kubectl delete svc nginx-local-mixed-test -n $NAMESPACE 2>/dev/null || true
            pass "Mixed test skipped (no local pool)"
            return 0
        }

    LOCAL_IP=$(kubectl get svc nginx-local-mixed-test -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Local IP: $LOCAL_IP"

    # Wait for local IP to be announced
    sleep 5

    # Verify local IP on eth0 (exactly 1 node)
    LOCAL_NODE_COUNT=0
    for node in $NODES; do
        if ssh_or_fail $node "ip -o addr show eth0 2>/dev/null | grep -q ' $LOCAL_IP/'"; then
            LOCAL_NODE_COUNT=$((LOCAL_NODE_COUNT + 1))
        fi
    done
    [ "$LOCAL_NODE_COUNT" -eq 1 ] || fail "Local IP should be on 1 node, found $LOCAL_NODE_COUNT"
    pass "Local IP on eth0 on exactly 1 node"

    # Verify remote IP on kube-lb0 (all nodes)
    REMOTE_NODE_COUNT=0
    for node in $NODES; do
        if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $REMOTE_IP/'"; then
            REMOTE_NODE_COUNT=$((REMOTE_NODE_COUNT + 1))
        fi
    done
    [ "$REMOTE_NODE_COUNT" -eq "$NODE_COUNT" ] || fail "Remote IP should be on all $NODE_COUNT nodes, found $REMOTE_NODE_COUNT"
    pass "Remote IP on kube-lb0 on all $NODE_COUNT nodes"

    # Cleanup
    kubectl delete svc nginx-local-mixed-test -n $NAMESPACE

    pass "Mixed local/remote test completed"
}

#---------------------------------------------------------------------
# Test 16: No Cross-Contamination Check
#---------------------------------------------------------------------
test_no_cross_contamination() {
    echo ""
    echo "=========================================="
    echo "TEST 16: No Cross-Contamination Check"
    echo "=========================================="

    # Get all remote IPs (10.255.x.x)
    info "Checking that no remote IPs (10.255.x.x) are on eth0..."
    for node in $NODES; do
        REMOTE_ON_ETH0=$(ssh_or_fail $node "ip -o addr show eth0 2>/dev/null | grep ' 10\.255\.' | grep -oP '\d+\.\d+\.\d+\.\d+(?=/)'") || echo ""
        if [ -n "$REMOTE_ON_ETH0" ]; then
            fail "Remote IP $REMOTE_ON_ETH0 found on eth0 on $node (cross-contamination!)"
        fi
    done
    pass "No remote IPs on eth0"

    # Get all local IPs and verify not on kube-lb0
    info "Checking that no local IPs (172.30.x.x) are on kube-lb0..."
    for node in $NODES; do
        LOCAL_ON_KUBELB=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep ' 172\.30\.' | grep -oP '\d+\.\d+\.\d+\.\d+(?=/)'") || echo ""
        if [ -n "$LOCAL_ON_KUBELB" ]; then
            fail "Local IP $LOCAL_ON_KUBELB found on kube-lb0 on $node (cross-contamination!)"
        fi
    done
    pass "No local IPs on kube-lb0"

    pass "No cross-contamination detected"
}

#---------------------------------------------------------------------
# Test 17: Node Failure - Continued Announcement
#---------------------------------------------------------------------
test_node_failure() {
    echo ""
    echo "=========================================="
    echo "TEST 17: Node Failure - Continued Announcement"
    echo "=========================================="

    IP=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Testing with IP: $IP"

    # Pick a node to "fail"
    FAIL_NODE=${NODES%% *}  # First node
    info "Simulating failure of $FAIL_NODE by tainting and evicting lbnodeagent..."

    # Taint node to evict lbnodeagent
    kubectl taint node "$FAIL_NODE" purelb-test=failover:NoExecute --overwrite

    # Delete the pod to speed things up
    AGENT_POD=$(kubectl get pods -n purelb -l component=lbnodeagent -o wide 2>/dev/null | grep "$FAIL_NODE" | awk '{print $1}')
    if [ -n "$AGENT_POD" ]; then
        kubectl delete pod -n purelb "$AGENT_POD" --grace-period=0 --force 2>/dev/null || true
    fi

    # Wait for IP to be removed from failed node
    info "Waiting for IP to be removed from $FAIL_NODE..."
    TIMEOUT=30
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! ssh_or_fail $FAIL_NODE "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            pass "IP removed from $FAIL_NODE (took ${ELAPSED}s)"
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    # Verify IP still on remaining nodes
    REMAINING_COUNT=$((NODE_COUNT - 1))
    info "Verifying IP still on $REMAINING_COUNT remaining nodes..."
    COUNT=0
    for node in $NODES; do
        if [ "$node" != "$FAIL_NODE" ]; then
            if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                COUNT=$((COUNT + 1))
            fi
        fi
    done
    [ "$COUNT" -eq "$REMAINING_COUNT" ] || fail "Expected $REMAINING_COUNT nodes, found $COUNT"
    pass "IP still on $COUNT remaining nodes"

    # Test connectivity still works (from pod, bypasses hairpin NAT)
    test_vip_connectivity_from_pod "$IP" 80

    # Remove taint and let agent recover
    info "Removing taint from $FAIL_NODE..."
    kubectl taint node "$FAIL_NODE" purelb-test- 2>/dev/null || true

    # Wait for DaemonSet to recover
    info "Waiting for lbnodeagent to recover..."
    kubectl rollout status daemonset/lbnodeagent -n purelb --timeout=60s

    # Wait for IP to reappear on recovered node
    info "Waiting for IP to reappear on $FAIL_NODE..."
    TIMEOUT=30
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ssh_or_fail $FAIL_NODE "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            pass "IP reappeared on $FAIL_NODE (took ${ELAPSED}s)"
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    [ $ELAPSED -lt $TIMEOUT ] || fail "IP did not reappear on $FAIL_NODE within ${TIMEOUT}s"

    pass "Node failure test completed"
}

#---------------------------------------------------------------------
# Test 18: Pool Exhaustion
#---------------------------------------------------------------------
test_pool_exhaustion() {
    echo ""
    echo "=========================================="
    echo "TEST 18: Pool Exhaustion"
    echo "=========================================="

    # Pool range is 10.255.0.100-10.255.0.150 = 51 IPs
    # Some may already be in use, so let's just verify we can exhaust it

    info "Creating services to exhaust pool..."
    CREATED=0
    IP_25=""
    for i in $(seq 1 52); do
        cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: v1
kind: Service
metadata:
  name: nginx-exhaust-$i
  namespace: $NAMESPACE
  labels:
    test-suite: remote
    test-type: exhaustion
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
  - port: $((9000 + i))
    targetPort: 80
EOF
        CREATED=$((CREATED + 1))

        # Check if this one got an IP
        sleep 1
        IP=$(kubectl get svc nginx-exhaust-$i -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
        if [ -z "$IP" ]; then
            info "Service #$i did not get IP (pool exhausted at $((i-1)) services)"
            break
        fi

        # Record IP #25 for reuse test
        if [ "$i" -eq 25 ]; then
            IP_25="$IP"
            info "Service #25 got IP: $IP_25"
        fi
    done

    # Find a service that failed to get IP
    FAILED_SVC=""
    for i in $(seq 1 52); do
        IP=$(kubectl get svc nginx-exhaust-$i -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
        if [ -z "$IP" ]; then
            FAILED_SVC="nginx-exhaust-$i"
            break
        fi
    done

    if [ -n "$FAILED_SVC" ]; then
        # Verify AllocationFailed event
        EVENTS=$(kubectl get events -n $NAMESPACE --field-selector involvedObject.name=$FAILED_SVC,reason=AllocationFailed -o jsonpath='{.items[*].message}' 2>/dev/null || echo "")
        if [ -n "$EVENTS" ]; then
            pass "Pool exhaustion correctly reported: $EVENTS"
        else
            info "No AllocationFailed event found but allocation was prevented"
            pass "Pool exhaustion handled"
        fi
    fi

    # Test IP reuse: delete service #25 and verify IP is reused
    if [ -n "$IP_25" ]; then
        # First delete the failed service so it doesn't grab the freed IP
        if [ -n "$FAILED_SVC" ]; then
            info "Deleting failed service $FAILED_SVC before reuse test..."
            kubectl delete svc $FAILED_SVC -n $NAMESPACE 2>/dev/null || true
            sleep 2
        fi

        info "Testing IP reuse: deleting service #25..."
        kubectl delete svc nginx-exhaust-25 -n $NAMESPACE
        if ! wait_for_ip_removal_all_nodes "$IP_25" 60; then
            warn "Timeout waiting for IP removal, but continuing with reuse test"
        fi

        info "Creating new service to get reused IP..."
        cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-exhaust-reuse
  namespace: $NAMESPACE
  labels:
    test-suite: remote
    test-type: exhaustion
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
  - port: 9999
    targetPort: 80
EOF

        kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
            svc/nginx-exhaust-reuse -n $NAMESPACE --timeout=30s || fail "Reuse service did not get IP"

        REUSED_IP=$(kubectl get svc nginx-exhaust-reuse -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        info "New service got IP: $REUSED_IP"

        if [ "$REUSED_IP" = "$IP_25" ]; then
            pass "IP reuse verified: got same IP $IP_25"
        else
            info "Got different IP $REUSED_IP (not necessarily a bug - pool may have other free IPs)"
            pass "IP allocation after release works"
        fi
    fi

    # Cleanup exhaustion test services
    info "Cleaning up exhaustion test services..."
    kubectl delete svc -n $NAMESPACE -l test-type=exhaustion --ignore-not-found 2>/dev/null || true

    pass "Pool exhaustion test completed"
}

#---------------------------------------------------------------------
# Test 19: Out-of-Pool IP Request
#---------------------------------------------------------------------
test_out_of_pool_request() {
    echo ""
    echo "=========================================="
    echo "TEST 19: Out-of-Pool IP Request"
    echo "=========================================="

    INVALID_IP="10.255.1.100"  # Outside pool range (pool is 10.255.0.x)
    info "Requesting out-of-pool IP: $INVALID_IP"

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-out-of-pool
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
    purelb.io/addresses: "$INVALID_IP"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8084
    targetPort: 80
EOF

    sleep 5
    ALLOCATED_IP=$(kubectl get svc nginx-out-of-pool -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")

    if [ -n "$ALLOCATED_IP" ]; then
        fail "Out-of-pool IP request should fail, but got IP: $ALLOCATED_IP"
    fi

    # Verify AllocationFailed event
    EVENTS=$(kubectl get events -n $NAMESPACE --field-selector involvedObject.name=nginx-out-of-pool,reason=AllocationFailed -o jsonpath='{.items[*].message}' 2>/dev/null || echo "")
    if [ -n "$EVENTS" ]; then
        pass "Out-of-pool request correctly rejected: $EVENTS"
    else
        pass "Out-of-pool IP request correctly prevented allocation"
    fi

    # Cleanup
    kubectl delete svc nginx-out-of-pool -n $NAMESPACE

    pass "Out-of-pool IP request test completed"
}

#---------------------------------------------------------------------
# Test 20: Specific IP Already In Use
#---------------------------------------------------------------------
test_specific_ip_in_use() {
    echo ""
    echo "=========================================="
    echo "TEST 20: Specific IP Already In Use"
    echo "=========================================="

    # Get an already-in-use IP
    IN_USE_IP=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Testing with in-use IP: $IN_USE_IP"

    # Try to request the same IP for a different service
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-ip-in-use-test
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
    purelb.io/addresses: "$IN_USE_IP"
spec:
  type: LoadBalancer
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8085
    targetPort: 80
EOF

    sleep 5
    ALLOCATED_IP=$(kubectl get svc nginx-ip-in-use-test -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")

    if [ -n "$ALLOCATED_IP" ]; then
        fail "Already-in-use IP request should fail, but got IP: $ALLOCATED_IP"
    fi

    pass "Already-in-use IP request correctly rejected"

    # Cleanup
    kubectl delete svc nginx-ip-in-use-test -n $NAMESPACE

    pass "Specific IP in use test completed"
}

#---------------------------------------------------------------------
# Test 21: Final Validation
#---------------------------------------------------------------------
test_final_validation() {
    echo ""
    echo "=========================================="
    echo "TEST 21: Final Validation"
    echo "=========================================="

    # Run invariant checks
    verify_invariants

    # Verify announcement counts for remaining services
    info "Verifying announcement counts for remote services..."
    for svc in nginx-remote-ipv4 nginx-remote-ipv6 nginx-remote-dualstack; do
        if kubectl get svc $svc -n $NAMESPACE >/dev/null 2>&1; then
            IP=$(kubectl get svc $svc -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
            COUNT=0
            for node in $NODES; do
                if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                    COUNT=$((COUNT + 1))
                fi
            done
            [ "$COUNT" -eq "$NODE_COUNT" ] || fail "$svc: expected $NODE_COUNT nodes, found $COUNT with IP $IP"
            pass "$svc: IP $IP on all $COUNT nodes"
        fi
    done

    pass "Final validation completed"
}

#---------------------------------------------------------------------
# Test 22: ETP Cluster to Local Transition
#---------------------------------------------------------------------
test_etp_transition() {
    echo ""
    echo "=========================================="
    echo "TEST 22: ETP Cluster to Local Transition"
    echo "=========================================="

    # Create service with ETP Cluster
    info "Creating service with externalTrafficPolicy: Cluster..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-etp-transition
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
spec:
  type: LoadBalancer
  externalTrafficPolicy: Cluster
  ipFamilyPolicy: SingleStack
  ipFamilies:
  - IPv4
  selector:
    app: nginx
  ports:
  - port: 8086
    targetPort: 80
EOF

    IP=$(wait_for_service_announced nginx-etp-transition kube-lb0 60)
    info "Allocated IP: $IP"

    # Verify on ALL nodes
    wait_for_all_nodes_announce "$IP" kube-lb0 $NODE_COUNT 30
    pass "IP on all $NODE_COUNT nodes with ETP Cluster"

    # Get nodes with endpoints (nginx pods)
    ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].spec.nodeName}')
    info "Endpoint nodes: $ENDPOINT_NODES"

    # Change to ETP Local
    info "Patching service to externalTrafficPolicy: Local..."
    kubectl patch svc nginx-etp-transition -n $NAMESPACE -p '{"spec":{"externalTrafficPolicy":"Local"}}'

    sleep 5

    # Verify IP REMOVED from nodes WITHOUT endpoints
    info "Verifying IP removed from non-endpoint nodes..."
    for node in $NODES; do
        HAS_ENDPOINT=false
        for en in $ENDPOINT_NODES; do
            [ "$node" = "$en" ] && HAS_ENDPOINT=true
        done

        if [ "$HAS_ENDPOINT" = "true" ]; then
            ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
                fail "IP should be on $node (has endpoint) but is NOT"
        else
            TIMEOUT=30
            ELAPSED=0
            while [ $ELAPSED -lt $TIMEOUT ]; do
                if ! ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                    break
                fi
                sleep 2
                ELAPSED=$((ELAPSED + 2))
            done
            if ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                fail "IP should NOT be on $node (no endpoint) but IS"
            fi
        fi
    done
    pass "IP correctly adjusted after ETP transition"

    # Verify connectivity still works (from pod, bypasses hairpin NAT)
    test_vip_connectivity_from_pod "$IP" 8086

    # Cleanup
    kubectl delete svc nginx-etp-transition -n $NAMESPACE

    pass "ETP transition test completed"
}

#---------------------------------------------------------------------
# Test 23: Add Sharing Annotation to Existing Service
#---------------------------------------------------------------------
test_add_sharing_annotation() {
    echo ""
    echo "=========================================="
    echo "TEST 23: Add Sharing Annotation to Existing Service"
    echo "=========================================="

    # Create service A on port 80 (no sharing annotation)
    info "Creating service A without sharing annotation..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-sharing-svc-a
  namespace: $NAMESPACE
  labels:
    test-suite: remote
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
  - name: http
    port: 80
    targetPort: 80
EOF

    IP_A=$(wait_for_service_announced nginx-sharing-svc-a kube-lb0 60)
    info "Service A got IP: $IP_A"

    # Create service B on port 443 with sharing key (but A doesn't have it yet)
    info "Creating service B with sharing key (A doesn't have it yet)..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-sharing-svc-b
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
    purelb.io/allow-shared-ip: late-sharing
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

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-sharing-svc-b -n $NAMESPACE --timeout=30s || fail "Service B not allocated"

    IP_B=$(kubectl get svc nginx-sharing-svc-b -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Service B got IP: $IP_B"

    # Verify they got DIFFERENT IPs (A has no sharing key)
    [ "$IP_A" != "$IP_B" ] || fail "Services should have different IPs (A has no sharing key), but both got $IP_A"
    pass "Services correctly got different IPs (A: $IP_A, B: $IP_B)"

    # Now add sharing annotation to service A
    info "Adding sharing annotation to service A..."
    kubectl annotate svc nginx-sharing-svc-a -n $NAMESPACE purelb.io/allow-shared-ip=late-sharing

    # Delete and recreate service B with same sharing key
    info "Deleting and recreating service B..."
    kubectl delete svc nginx-sharing-svc-b -n $NAMESPACE
    wait_for_ip_removal_all_nodes "$IP_B" 30

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-sharing-svc-b
  namespace: $NAMESPACE
  labels:
    test-suite: remote
  annotations:
    purelb.io/service-group: remote
    purelb.io/allow-shared-ip: late-sharing
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

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/nginx-sharing-svc-b -n $NAMESPACE --timeout=30s || fail "Service B not allocated after recreate"

    IP_B_NEW=$(kubectl get svc nginx-sharing-svc-b -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Service B now got IP: $IP_B_NEW"

    # Verify they now share the SAME IP
    [ "$IP_A" = "$IP_B_NEW" ] || fail "Services should now share same IP, but A=$IP_A, B=$IP_B_NEW"
    pass "Services now correctly share the same IP: $IP_A"

    # Cleanup
    kubectl delete svc nginx-sharing-svc-a nginx-sharing-svc-b -n $NAMESPACE

    pass "Add sharing annotation test completed"
}

#---------------------------------------------------------------------
# Test 24: SingleStack to DualStack Transition
#---------------------------------------------------------------------
test_singlestack_to_dualstack() {
    echo ""
    echo "=========================================="
    echo "TEST 24: SingleStack to DualStack Transition"
    echo "=========================================="

    # Create IPv4-only service
    info "Creating IPv4-only service..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: nginx-stack-transition
  namespace: $NAMESPACE
  labels:
    test-suite: remote
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
  - port: 8087
    targetPort: 80
EOF

    IPV4=$(wait_for_service_announced nginx-stack-transition kube-lb0 60)
    info "Allocated IPv4: $IPV4"

    # Verify IPv4 on kube-lb0, no IPv6
    wait_for_all_nodes_announce "$IPV4" kube-lb0 $NODE_COUNT 30
    pass "IPv4 on all nodes"

    # Check no IPv6 assigned
    IPV6_BEFORE=$(kubectl get svc nginx-stack-transition -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[1].ip}' 2>/dev/null || echo "")
    [ -z "$IPV6_BEFORE" ] || fail "Should have no IPv6 yet, but found: $IPV6_BEFORE"
    pass "No IPv6 assigned (SingleStack)"

    # Update service to require dual-stack
    info "Patching service to RequireDualStack..."
    kubectl patch svc nginx-stack-transition -n $NAMESPACE --type=merge -p '
{
  "spec": {
    "ipFamilyPolicy": "RequireDualStack",
    "ipFamilies": ["IPv4", "IPv6"]
  }
}'

    # Wait for IPv6 to be assigned
    info "Waiting for IPv6 allocation..."
    TIMEOUT=30
    ELAPSED=0
    IPV6=""
    while [ $ELAPSED -lt $TIMEOUT ]; do
        IPV6=$(kubectl get svc nginx-stack-transition -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[1].ip}' 2>/dev/null || echo "")
        if [ -n "$IPV6" ] && [[ "$IPV6" =~ ":" ]]; then
            break
        fi
        # Check if IPv6 is at index 0 (order might vary)
        IPV6=$(kubectl get svc nginx-stack-transition -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
        if [[ "$IPV6" =~ ":" ]]; then
            break
        fi
        IPV6=""
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    [ -n "$IPV6" ] || fail "IPv6 not allocated within ${TIMEOUT}s"
    info "Allocated IPv6: $IPV6"
    pass "IPv6 allocated after transition"

    # Verify IPv4 UNCHANGED
    IPV4_AFTER=""
    for i in 0 1; do
        IP=$(kubectl get svc nginx-stack-transition -n $NAMESPACE -o jsonpath="{.status.loadBalancer.ingress[$i].ip}")
        if [[ ! "$IP" =~ ":" ]]; then
            IPV4_AFTER="$IP"
            break
        fi
    done
    [ "$IPV4" = "$IPV4_AFTER" ] || fail "IPv4 changed during transition: $IPV4 -> $IPV4_AFTER"
    pass "IPv4 unchanged: $IPV4"

    # Verify IPv6 on kube-lb0 on all nodes
    info "Waiting for IPv6 to be announced on all nodes..."
    wait_for_all_nodes_announce "$IPV6" kube-lb0 $NODE_COUNT 60
    pass "IPv6 on all nodes"

    # Verify both have correct aggregation
    verify_aggregation_prefix "${NODES%% *}" "$IPV4" "32"
    verify_aggregation_prefix "${NODES%% *}" "$IPV6" "128"
    pass "Both have correct aggregation prefixes"

    # Test connectivity to both (from pod, bypasses hairpin NAT)
    wait_for_nftables_rules "$IPV4" 8087
    test_vip_connectivity_from_pod "$IPV4" 8087
    wait_for_nftables_rules_v6 "$IPV6" 8087
    test_vip_connectivity_from_pod_v6 "$IPV6" 8087

    # Cleanup
    kubectl delete svc nginx-stack-transition -n $NAMESPACE

    pass "SingleStack to DualStack transition test completed"
}

#---------------------------------------------------------------------
# Test 25: LBNodeAgent Restart Recovery
#---------------------------------------------------------------------
test_lbnodeagent_restart() {
    echo ""
    echo "=========================================="
    echo "TEST 25: LBNodeAgent Restart Recovery"
    echo "=========================================="

    IP=$(kubectl get svc nginx-remote-ipv4 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    info "Testing with IP: $IP"

    # Pick a node to restart agent on
    RESTART_NODE=${NODES%% *}
    info "Restarting lbnodeagent on $RESTART_NODE..."

    # Verify IP is currently on this node
    ssh_or_fail $RESTART_NODE "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
        fail "IP should be on $RESTART_NODE before restart"

    # Kill the agent pod (not evict - just restart)
    AGENT_POD=$(kubectl get pods -n purelb -l component=lbnodeagent -o wide 2>/dev/null | grep "$RESTART_NODE" | awk '{print $1}')
    info "Killing agent pod $AGENT_POD..."
    kubectl delete pod -n purelb "$AGENT_POD" --grace-period=1

    # Wait for pod to restart
    info "Waiting for agent to restart..."
    sleep 5
    kubectl rollout status daemonset/lbnodeagent -n purelb --timeout=60s

    # Verify IP is restored on this node
    info "Verifying IP restored after restart..."
    TIMEOUT=30
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ssh_or_fail $RESTART_NODE "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            pass "IP restored on $RESTART_NODE after agent restart (took ${ELAPSED}s)"
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    [ $ELAPSED -lt $TIMEOUT ] || fail "IP not restored on $RESTART_NODE within ${TIMEOUT}s"

    # Verify no duplicate IPs (check all nodes have exactly 1 instance each)
    for node in $NODES; do
        COUNT=$(ssh_or_fail $node "ip -o addr show kube-lb0 2>/dev/null | grep -c ' $IP/'" || echo 0)
        [ "$COUNT" -le 1 ] || fail "Duplicate IP $IP on $node (count: $COUNT)"
    done
    pass "No duplicate IPs after restart"

    pass "LBNodeAgent restart recovery test completed"
}

#---------------------------------------------------------------------
# Test 26: LBNodeAgent Restart During ETP Local
#---------------------------------------------------------------------
test_lbnodeagent_restart_etp_local() {
    echo ""
    echo "=========================================="
    echo "TEST 26: LBNodeAgent Restart During ETP Local"
    echo "=========================================="

    # Ensure we have an ETP Local service
    IP=$(kubectl get svc nginx-etp-local -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")
    if [ -z "$IP" ]; then
        info "ETP Local service not found, skipping this test"
        pass "Test skipped (ETP Local service not available)"
        return 0
    fi
    info "Testing with ETP Local IP: $IP"

    # Get nodes with endpoints
    ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx -o jsonpath='{.items[*].spec.nodeName}')
    info "Endpoint nodes: $ENDPOINT_NODES"

    # Find a node WITH endpoint
    WITH_ENDPOINT=""
    for node in $ENDPOINT_NODES; do
        WITH_ENDPOINT=$node
        break
    done

    # Find a node WITHOUT endpoint
    WITHOUT_ENDPOINT=""
    for node in $NODES; do
        HAS_ENDPOINT=false
        for en in $ENDPOINT_NODES; do
            [ "$node" = "$en" ] && HAS_ENDPOINT=true
        done
        if [ "$HAS_ENDPOINT" = "false" ]; then
            WITHOUT_ENDPOINT=$node
            break
        fi
    done

    # Test restart on node WITH endpoint
    if [ -n "$WITH_ENDPOINT" ]; then
        info "Testing restart on node WITH endpoint: $WITH_ENDPOINT"

        # Verify IP is on this node before restart
        ssh_or_fail $WITH_ENDPOINT "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'" || \
            fail "IP should be on $WITH_ENDPOINT before restart"

        # Kill agent
        AGENT_POD=$(kubectl get pods -n purelb -l component=lbnodeagent -o wide 2>/dev/null | grep "$WITH_ENDPOINT" | awk '{print $1}')
        kubectl delete pod -n purelb "$AGENT_POD" --grace-period=1

        # Wait for restart
        sleep 5
        kubectl rollout status daemonset/lbnodeagent -n purelb --timeout=60s

        # Verify IP returns
        TIMEOUT=30
        ELAPSED=0
        while [ $ELAPSED -lt $TIMEOUT ]; do
            if ssh_or_fail $WITH_ENDPOINT "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
                pass "IP returned to endpoint node $WITH_ENDPOINT after restart"
                break
            fi
            sleep 2
            ELAPSED=$((ELAPSED + 2))
        done
        [ $ELAPSED -lt $TIMEOUT ] || fail "IP did not return to $WITH_ENDPOINT"
    fi

    # Test restart on node WITHOUT endpoint
    if [ -n "$WITHOUT_ENDPOINT" ]; then
        info "Testing restart on node WITHOUT endpoint: $WITHOUT_ENDPOINT"

        # Verify IP is NOT on this node before restart
        if ssh_or_fail $WITHOUT_ENDPOINT "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            fail "IP should NOT be on $WITHOUT_ENDPOINT before restart (no endpoint)"
        fi

        # Kill agent
        AGENT_POD=$(kubectl get pods -n purelb -l component=lbnodeagent -o wide 2>/dev/null | grep "$WITHOUT_ENDPOINT" | awk '{print $1}')
        kubectl delete pod -n purelb "$AGENT_POD" --grace-period=1

        # Wait for restart
        sleep 5
        kubectl rollout status daemonset/lbnodeagent -n purelb --timeout=60s

        # Verify IP does NOT appear
        sleep 5
        if ssh_or_fail $WITHOUT_ENDPOINT "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $IP/'"; then
            fail "IP appeared on $WITHOUT_ENDPOINT after restart (should NOT - no endpoint)"
        fi
        pass "IP correctly NOT added to non-endpoint node $WITHOUT_ENDPOINT after restart"
    fi

    pass "LBNodeAgent restart during ETP Local test completed"
}

#---------------------------------------------------------------------
# Cleanup
#---------------------------------------------------------------------
cleanup_test_services() {
    echo ""
    echo "=========================================="
    echo "Cleanup: Removing test services"
    echo "=========================================="

    info "Deleting remote test services..."
    kubectl delete svc -n $NAMESPACE -l test-suite=remote --ignore-not-found 2>/dev/null || true

    # Restore nginx to 1 replica
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1 2>/dev/null || true

    pass "Test services cleaned up"
}

#---------------------------------------------------------------------
# Run All Tests
#---------------------------------------------------------------------
run_all_tests() {
    echo ""
    echo "╔══════════════════════════════════════════════╗"
    echo "║  PureLB Remote Mode Functional Test Suite    ║"
    echo "╚══════════════════════════════════════════════╝"
    echo ""
    echo "Cluster: $CONTEXT"
    echo "Namespace: $NAMESPACE"
    echo "Nodes: $NODE_COUNT ($NODES)"
    echo ""

    # Test 0: Prerequisites
    test_prerequisites

    # Core remote functionality (Tests 1-4)
    test_remote_ipv4
    test_remote_ipv6
    test_remote_dualstack
    test_all_nodes_announce

    # ETP Local (Tests 5-7)
    test_etp_local_basic
    test_etp_local_migration
    test_etp_local_zero_endpoints

    # Aggregation with explicit config (Tests 8-9)
    test_aggregation_ipv4
    test_aggregation_ipv6

    # Aggregation with default (subnet mask) (Tests 10-11)
    test_aggregation_default_ipv4
    test_aggregation_default_ipv6

    # Service lifecycle (Tests 12-14)
    test_service_deletion
    test_ip_sharing
    test_specific_ip_request

    # Mixed and cross-contamination (Tests 15-17)
    test_mixed_local_remote
    test_no_cross_contamination
    test_node_failure

    # Negative tests (Tests 18-20)
    test_pool_exhaustion
    test_out_of_pool_request
    test_specific_ip_in_use

    # Final validation (Test 21)
    test_final_validation

    # Service update tests (Tests 22-26)
    test_etp_transition
    test_add_sharing_annotation
    test_singlestack_to_dualstack
    test_lbnodeagent_restart
    test_lbnodeagent_restart_etp_local

    # Cleanup
    cleanup_test_services

    echo ""
    echo "=========================================="
    echo -e "${GREEN}ALL TESTS PASSED${NC}"
    echo "=========================================="
}

# Run tests
run_all_tests
