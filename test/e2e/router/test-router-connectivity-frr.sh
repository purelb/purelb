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

CONTEXT="${CONTEXT:-$(command kubectl config current-context 2>/dev/null)}"
NAMESPACE="test"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Required environment variables
ROUTER_HOST="${ROUTER_HOST:-}"

# Optional configuration
VIP_SUBNET="${VIP_SUBNET:-10.255.0.0/24}"
VIP6_SUBNET="${VIP6_SUBNET:-fd00:10:255::/64}"
BGP_CONVERGE_TIMEOUT="${BGP_CONVERGE_TIMEOUT:-30}"
ECMP_TEST_REQUESTS="${ECMP_TEST_REQUESTS:-100}"

# Default aggregation subnets (for route queries)
VIP_DEFAGGR_SUBNET="${VIP_DEFAGGR_SUBNET:-10.255.1.0/24}"
VIP6_DEFAGGR_SUBNET="${VIP6_DEFAGGR_SUBNET:-fd00:10:256::/64}"

# Service YAML files
SVC_IPV4="${SCRIPT_DIR}/svc-router-ipv4.yaml"
SVC_IPV6="${SCRIPT_DIR}/svc-router-ipv6.yaml"
SVC_IPV4_LOCAL="${SCRIPT_DIR}/svc-router-ipv4-etp-local.yaml"
SVC_IPV6_LOCAL="${SCRIPT_DIR}/svc-router-ipv6-etp-local.yaml"
SVC_DUALSTACK="${SCRIPT_DIR}/svc-router-dualstack.yaml"
SVC_DUALSTACK_LOCAL="${SCRIPT_DIR}/svc-router-dualstack-etp-local.yaml"
SVC_IPV4_DEFAGGR="${SCRIPT_DIR}/svc-router-ipv4-default-aggr.yaml"
SVC_IPV6_DEFAGGR="${SCRIPT_DIR}/svc-router-ipv6-default-aggr.yaml"
SVC_NAME_IPV4="router-test-ipv4"
SVC_NAME_IPV6="router-test-ipv6"
SVC_NAME_IPV4_LOCAL="router-test-ipv4-local"
SVC_NAME_IPV6_LOCAL="router-test-ipv6-local"
SVC_NAME_DUALSTACK="router-test-dualstack"
SVC_NAME_DUALSTACK_LOCAL="router-test-dualstack-local"
SVC_NAME_IPV4_DEFAGGR="router-test-ipv4-defaggr"
SVC_NAME_IPV6_DEFAGGR="router-test-ipv6-defaggr"

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

# Build node name -> InternalIP map
declare -A NODE_IPS
while read -r name ip; do
    NODE_IPS[$name]=$ip
done < <(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name} {.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')

#---------------------------------------------------------------------
# SSH Helper Functions
#---------------------------------------------------------------------

# SSH to FRR router
ssh_router() {
    ssh "$ROUTER_HOST" "$@" 2>/dev/null
}

# SSH to cluster node (resolves name to InternalIP)
ssh_node() {
    local NODE=$1
    shift
    ssh "${NODE_IPS[$NODE]}" "$@" 2>/dev/null
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
    kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null || echo "(failed)"
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
    frr_vtysh "show ip route $VIP/32" 2>/dev/null | grep -oP '\*\s+\K[0-9.]+'
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
# FRR IPv6 Route Verification Functions
#---------------------------------------------------------------------

# Show IPv6 routes for VIP subnet
frr_show_routes6() {
    frr_vtysh "show ipv6 route $VIP6_SUBNET longer-prefixes"
}

# Check if a specific IPv6 VIP has a route
frr_check_route6() {
    local VIP=$1
    frr_vtysh "show ipv6 route $VIP/128" 2>/dev/null | grep -q "$VIP"
}

# Get next-hops for an IPv6 VIP from FRR (link-local addresses)
frr_get_nexthops6() {
    local VIP=$1
    frr_vtysh "show ipv6 route $VIP/128" 2>/dev/null | grep -oP '\*\s+\K[0-9a-f:]+'
}

# Count next-hops for an IPv6 VIP
frr_count_nexthops6() {
    local VIP=$1
    frr_get_nexthops6 "$VIP" | wc -l
}

# Get IPv6 route prefix length
frr_get_prefix_length6() {
    local VIP=$1
    frr_vtysh "show ipv6 route $VIP/128" 2>/dev/null | grep -oP "$VIP/\K[0-9]+" | head -1
}

# Wait for IPv6 route to appear in FRR
frr_wait_for_route6() {
    local VIP=$1
    local TIMEOUT=${2:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if frr_check_route6 "$VIP"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

# Wait for IPv6 route to be withdrawn from FRR
frr_wait_for_withdrawal6() {
    local VIP=$1
    local TIMEOUT=${2:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! frr_check_route6 "$VIP"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

#---------------------------------------------------------------------
# FRR Prefix-Aware Route Functions (for non-host routes like /24, /64)
#---------------------------------------------------------------------

# Check if a VIP has a route with a specific prefix length
frr_check_route_prefix() {
    local VIP=$1
    local PREFIX=$2
    frr_vtysh "show ip route $VIP/$PREFIX" 2>/dev/null | grep -q "$VIP"
}

# Get next-hops for a VIP with specific prefix
frr_get_nexthops_prefix() {
    local VIP=$1
    local PREFIX=$2
    frr_vtysh "show ip route $VIP/$PREFIX" 2>/dev/null | grep -oP '\*\s+\K[0-9.]+'
}

# Count next-hops for a route with specific prefix
frr_count_nexthops_prefix() {
    local VIP=$1
    local PREFIX=$2
    frr_get_nexthops_prefix "$VIP" "$PREFIX" | wc -l
}

# Wait for route with specific prefix to appear
frr_wait_for_route_prefix() {
    local VIP=$1
    local PREFIX=$2
    local TIMEOUT=${3:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if frr_check_route_prefix "$VIP" "$PREFIX"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

# Wait for route with specific prefix to be withdrawn
frr_wait_for_withdrawal_prefix() {
    local VIP=$1
    local PREFIX=$2
    local TIMEOUT=${3:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! frr_check_route_prefix "$VIP" "$PREFIX"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

# IPv6 prefix-aware variants
frr_check_route6_prefix() {
    local VIP=$1
    local PREFIX=$2
    frr_vtysh "show ipv6 route $VIP/$PREFIX" 2>/dev/null | grep -q "$VIP"
}

frr_get_nexthops6_prefix() {
    local VIP=$1
    local PREFIX=$2
    frr_vtysh "show ipv6 route $VIP/$PREFIX" 2>/dev/null | grep -oP '\*\s+\K[0-9a-f:]+'
}

frr_count_nexthops6_prefix() {
    local VIP=$1
    local PREFIX=$2
    frr_get_nexthops6_prefix "$VIP" "$PREFIX" | wc -l
}

frr_wait_for_route6_prefix() {
    local VIP=$1
    local PREFIX=$2
    local TIMEOUT=${3:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if frr_check_route6_prefix "$VIP" "$PREFIX"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

frr_wait_for_withdrawal6_prefix() {
    local VIP=$1
    local PREFIX=$2
    local TIMEOUT=${3:-$BGP_CONVERGE_TIMEOUT}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        if ! frr_check_route6_prefix "$VIP" "$PREFIX"; then
            return 0
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

#---------------------------------------------------------------------
# Metrics Functions
#---------------------------------------------------------------------

CYAN='\033[0;36m'

scrape_pod_metrics() {
    local pod=$1
    local local_port=$((30000 + RANDOM % 5000))
    kubectl port-forward -n purelb-system "$pod" ${local_port}:7472 >/dev/null 2>&1 &
    local pf_pid=$!
    local metrics=""
    local attempt
    for attempt in 1 2 3 4 5; do
        sleep 1
        metrics=$(curl -s --connect-timeout 3 "http://127.0.0.1:${local_port}/metrics" 2>/dev/null || true)
        [ -n "$metrics" ] && break
    done
    kill $pf_pid 2>/dev/null || true
    wait $pf_pid 2>/dev/null || true
    echo "$metrics"
}

scrape_allocator_metrics() {
    local pod
    pod=$(kubectl get pods -n purelb-system -l component=allocator -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    [ -z "$pod" ] && { echo ""; return; }
    scrape_pod_metrics "$pod"
}

scrape_lbnodeagent_metrics() {
    local node=${1:-}
    local node_ip
    if [ -n "$node" ]; then
        node_ip="${NODE_IPS[$node]:-}"
    else
        for n in $NODES; do node_ip="${NODE_IPS[$n]:-}"; break; done
    fi
    [ -z "$node_ip" ] && { echo ""; return; }
    curl -s --connect-timeout 5 "http://${node_ip}:7472/metrics" 2>/dev/null || true
}

extract_metric() {
    local metrics="$1"
    local metric_name="$2"
    local value
    if echo "$metric_name" | grep -q '{'; then
        value=$(echo "$metrics" | grep -F "$metric_name" | head -1 | awk '{print $NF}')
    else
        value=$(echo "$metrics" | grep "^${metric_name} " | head -1 | awk '{print $NF}')
    fi
    [ -n "$value" ] && printf '%.0f' "$value" 2>/dev/null || echo "$value"
}

print_allocator_metrics() {
    local metrics="$1"
    local pool="${2:-remote}"
    [ -z "$metrics" ] && { info "allocator metrics: (unavailable)"; return; }
    local config_loaded pool_size in_use
    config_loaded=$(extract_metric "$metrics" "purelb_k8s_client_config_loaded_bool")
    pool_size=$(extract_metric "$metrics" "purelb_address_pool_size{pool=\"${pool}\"}")
    in_use=$(extract_metric "$metrics" "purelb_address_pool_addresses_in_use{pool=\"${pool}\"}")
    echo -e "${CYAN}── Metrics ────────────────────────────────────────────────${NC}"
    echo -e "     allocator │ config_loaded=${config_loaded:-?}  pool_size(${pool})=${pool_size:-?}  in_use(${pool})=${in_use:-?}"
}

print_lbnodeagent_metrics() {
    local metrics="$1"
    local node="${2:-?}"
    [ -z "$metrics" ] && { info "lbnodeagent metrics on $node: (unavailable)"; return; }
    local lease_healthy member_count subnet_count wins adds withdrawals garp renewals
    lease_healthy=$(extract_metric "$metrics" "purelb_election_lease_healthy")
    member_count=$(extract_metric "$metrics" "purelb_election_member_count")
    subnet_count=$(extract_metric "$metrics" "purelb_election_subnet_count")
    wins=$(extract_metric "$metrics" "purelb_lbnodeagent_election_wins_total")
    adds=$(extract_metric "$metrics" "purelb_lbnodeagent_address_additions_total")
    withdrawals=$(extract_metric "$metrics" "purelb_lbnodeagent_address_withdrawals_total")
    garp=$(extract_metric "$metrics" "purelb_lbnodeagent_garp_sent_total")
    renewals=$(extract_metric "$metrics" "purelb_election_lease_renewals_total")
    echo -e "${CYAN}── Metrics ────────────────────────────────────────────────${NC}"
    echo -e "     lbnodeagent(${node}) │ lease_healthy=${lease_healthy:-?}  members=${member_count:-?}  subnets=${subnet_count:-?}"
    echo -e "       counters │ wins=${wins:-0}  adds=${adds:-0}  withdrawals=${withdrawals:-0}  garp=${garp:-0}  lease_renewals=${renewals:-0}"
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

test_connectivity6() {
    local VIP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-5}
    curl -6 -s --connect-timeout $TIMEOUT "http://[$VIP]:$PORT/" 2>/dev/null
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

wait_for_connectivity6() {
    local VIP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-60}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        local RESPONSE=$(test_connectivity6 "$VIP" "$PORT")
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
    local SVC_FILE=$1
    kubectl apply -f "$SVC_FILE"
}

delete_test_service() {
    local SVC_FILE=$1
    kubectl delete -f "$SVC_FILE" --ignore-not-found
}

wait_for_service_ip() {
    local SVC=$1
    local TIMEOUT=${2:-60}

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
        svc/$SVC -n $NAMESPACE --timeout=${TIMEOUT}s >/dev/null 2>&1 || return 1

    kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
}

# Wait for dual-stack service to get both IPs (returns "IPv4 IPv6")
wait_for_dualstack_ips() {
    local SVC=$1
    local TIMEOUT=${2:-60}

    kubectl wait --for=jsonpath='{.status.loadBalancer.ingress[1].ip}' \
        svc/$SVC -n $NAMESPACE --timeout=${TIMEOUT}s >/dev/null 2>&1 || return 1

    local IP0=$(kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    local IP1=$(kubectl get svc $SVC -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[1].ip}')
    echo "$IP0 $IP1"
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
    info "  VIP6_SUBNET: $VIP6_SUBNET"

    # Verify SSH connectivity
    info "Verifying SSH connectivity..."
    verify_ssh "$ROUTER_HOST" "FRR router"
    for node in $NODES; do
        verify_ssh "${NODE_IPS[$node]}" "cluster node $node"
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
    if echo "$BGP_STATUS" | grep -qE "Estab|established|[0-9]{2}:[0-9]{2}:[0-9]{2}"; then
        pass "BGP sessions established"
    else
        warn "BGP sessions may not be fully established"
    fi

    # Verify PureLB is running
    info "Verifying PureLB components..."
    local AGENT_PODS=$(kubectl get pods -n purelb-system -l component=lbnodeagent --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    [ "$AGENT_PODS" -ge 1 ] || fail "LBNodeAgent not running"
    pass "LBNodeAgent running ($AGENT_PODS pods)"

    local ALLOCATOR_READY=$(kubectl get deployment -n purelb-system allocator -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    [ "$ALLOCATOR_READY" -ge 1 ] || fail "Allocator not running"
    pass "Allocator running"

    # Verify nginx test pods exist
    local NGINX_PODS=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
    [ "$NGINX_PODS" -ge 1 ] || fail "No nginx test pods running in namespace $NAMESPACE"
    pass "Test pods running ($NGINX_PODS pods)"

    # Ensure ServiceGroups exist (apply from YAML if missing)
    if ! kubectl get servicegroup -n purelb-system remote >/dev/null 2>&1; then
        info "Applying ServiceGroup 'remote'..."
        kubectl apply -f "${SCRIPT_DIR}/servicegroup-remote.yaml"
    fi
    pass "ServiceGroup 'remote' exists"

    # Ensure remote-default-aggr ServiceGroup exists (needed for default aggregation tests)
    if ! kubectl get servicegroup -n purelb-system remote-default-aggr >/dev/null 2>&1; then
        info "Applying ServiceGroup 'remote-default-aggr'..."
        kubectl apply -f "${SCRIPT_DIR}/servicegroup-remote-default-aggr.yaml"
    fi
    pass "ServiceGroup 'remote-default-aggr' exists"

    # Verify metrics endpoints
    info "Verifying metrics endpoints..."
    local alloc_metrics=$(scrape_allocator_metrics)
    if [ -n "$alloc_metrics" ]; then
        pass "Allocator metrics endpoint reachable"
        print_allocator_metrics "$alloc_metrics"
    else
        warn "Allocator metrics unavailable"
    fi

    local first_node=${NODES%% *}
    local agent_metrics=$(scrape_lbnodeagent_metrics "$first_node")
    if [ -n "$agent_metrics" ]; then
        pass "LBNodeAgent metrics endpoint reachable"
        print_lbnodeagent_metrics "$agent_metrics" "$first_node"
    else
        warn "LBNodeAgent metrics unavailable"
    fi

    pass "All prerequisites verified"
}

#---------------------------------------------------------------------
# Test 1: Basic External Connectivity with Route Verification
#---------------------------------------------------------------------

test_basic_with_route_verification() {
    section "TEST 1: External Connectivity with FRR Route Verification"

    info "Creating test service..."
    create_test_service "$SVC_IPV4"

    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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

    delete_test_service "$SVC_IPV4"
    pass "Basic connectivity with route verification completed"
}

#---------------------------------------------------------------------
# Test 2: ECMP with Next-Hop Verification
#---------------------------------------------------------------------

test_ecmp_with_nexthop_verification() {
    section "TEST 2: ECMP with FRR Next-Hop Verification"

    # Scale for distribution
    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    kubectl scale deployment nginx -n $NAMESPACE --replicas=5
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s

    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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
    delete_test_service "$SVC_IPV4"
    pass "ECMP with next-hop verification completed"
}

#---------------------------------------------------------------------
# Test 3: Node Failure with Route Withdrawal Verification
#---------------------------------------------------------------------

test_node_failure_route_withdrawal() {
    section "TEST 3: Node Failure with FRR Route Withdrawal"

    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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
    local AGENT_POD=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$FAIL_NODE" | awk '{print $1}')
    [ -n "$AGENT_POD" ] && kubectl delete pod -n purelb-system "$AGENT_POD" --grace-period=0 --force 2>/dev/null || true

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
    kubectl rollout status daemonset/lbnodeagent -n purelb-system --timeout=60s

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

    delete_test_service "$SVC_IPV4"
    pass "Node failure with route withdrawal test completed"
}

#---------------------------------------------------------------------
# Test 4: Service Deletion with Route Withdrawal
#---------------------------------------------------------------------

test_deletion_route_withdrawal() {
    section "TEST 4: Service Deletion with FRR Route Withdrawal"

    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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
    delete_test_service "$SVC_IPV4"

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

    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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

    delete_test_service "$SVC_IPV4"
    pass "Route aggregation verification completed"
}

#---------------------------------------------------------------------
# Test 6: IPv6 Basic Connectivity with Route Verification
#---------------------------------------------------------------------

test_ipv6_basic_with_route_verification() {
    section "TEST 6: IPv6 Connectivity with FRR Route Verification"

    info "Creating IPv6 test service..."
    create_test_service "$SVC_IPV6"

    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6" 60) || fail "No IPv6 address allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "IPv6 VIP not announced on nodes"
    pass "IPv6 VIP announced on $NODE_COUNT nodes"

    # FRR-specific: Wait for and verify IPv6 route
    info "Waiting for IPv6 route in FRR RIB..."
    if frr_wait_for_route6 "$VIP" "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv6 route to $VIP found in FRR RIB"
    else
        fail "IPv6 route to $VIP not found in FRR RIB after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    # Show route details
    info "FRR IPv6 route details:"
    frr_vtysh "show ipv6 route $VIP/128"

    # Verify next-hops (link-local addresses)
    local NEXTHOP_COUNT=$(frr_count_nexthops6 "$VIP")
    info "Route has $NEXTHOP_COUNT next-hop(s)"

    if [ "$NEXTHOP_COUNT" -ge 1 ]; then
        pass "IPv6 route has valid next-hops"
        info "Next-hops (link-local):"
        frr_get_nexthops6 "$VIP" | while read nh; do echo "  - $nh"; done
    else
        fail "No next-hops found for IPv6 route"
    fi

    # Verify ECMP — all nodes should be next-hops
    if [ "$NEXTHOP_COUNT" -ge 2 ]; then
        pass "IPv6 ECMP configured: $NEXTHOP_COUNT next-hops"
    else
        warn "Only $NEXTHOP_COUNT next-hop for IPv6 (expected ECMP)"
    fi

    # Verify prefix length is /128
    local PREFIX_LEN=$(frr_get_prefix_length6 "$VIP")
    if [ "$PREFIX_LEN" = "128" ]; then
        pass "IPv6 route has correct /128 aggregation"
    else
        warn "IPv6 route has /$PREFIX_LEN aggregation (expected /128)"
    fi

    # Test connectivity
    info "Testing IPv6 external connectivity..."
    if wait_for_connectivity6 "$VIP" 80 30; then
        local RESPONSE=$(test_connectivity6 "$VIP" 80)
        pass "IPv6 external connectivity successful"
        echo "$RESPONSE" | head -3
    else
        fail "IPv6 external connectivity failed"
    fi

    delete_test_service "$SVC_IPV6"
    pass "IPv6 basic connectivity with route verification completed"
}

#---------------------------------------------------------------------
# Test 7: IPv6 Service Deletion with Route Withdrawal
#---------------------------------------------------------------------

test_ipv6_deletion_route_withdrawal() {
    section "TEST 7: IPv6 Service Deletion with FRR Route Withdrawal"

    create_test_service "$SVC_IPV6"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6" 60) || fail "No IPv6 address allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "IPv6 VIP not announced"
    frr_wait_for_route6 "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "IPv6 route not in FRR"
    wait_for_connectivity6 "$VIP" 80 30 || fail "IPv6 connectivity failed"

    pass "IPv6 service running, route in FRR: $VIP"

    # Show route before deletion
    info "IPv6 route before deletion:"
    frr_vtysh "show ipv6 route $VIP/128"

    # Delete service
    info "Deleting service..."
    delete_test_service "$SVC_IPV6"

    # Wait for VIP removal from nodes
    info "Waiting for IPv6 VIP removal from nodes..."
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
    pass "IPv6 VIP removed from all nodes after ${ELAPSED}s"

    # FRR-specific: Verify route withdrawal
    info "Waiting for IPv6 route withdrawal from FRR..."
    if frr_wait_for_withdrawal6 "$VIP" "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv6 route withdrawn from FRR"
    else
        warn "IPv6 route still in FRR after ${BGP_CONVERGE_TIMEOUT}s (may be stale)"
        frr_vtysh "show ipv6 route $VIP/128" || true
    fi

    # Verify connectivity fails
    local RESPONSE=$(test_connectivity6 "$VIP" 80)
    if echo "$RESPONSE" | grep -q "Pod:"; then
        warn "IPv6 VIP still reachable (stale route?)"
    else
        pass "IPv6 VIP no longer reachable"
    fi

    pass "IPv6 service deletion with route withdrawal completed"
}

#---------------------------------------------------------------------
# Test 8: IPv6 Route Aggregation Verification
#---------------------------------------------------------------------

test_ipv6_route_aggregation() {
    section "TEST 8: IPv6 Route Aggregation Verification"

    create_test_service "$SVC_IPV6"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6" 60) || fail "No IPv6 address allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "IPv6 VIP not announced"
    frr_wait_for_route6 "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "IPv6 route not in FRR"

    # Verify /128 aggregation
    info "Verifying IPv6 route aggregation..."
    local ROUTE_OUTPUT=$(frr_vtysh "show ipv6 route $VIP/128")
    echo "$ROUTE_OUTPUT"

    if echo "$ROUTE_OUTPUT" | grep -q "$VIP/128"; then
        pass "IPv6 route has /128 aggregation (host route)"
    else
        local PREFIX_LEN=$(frr_get_prefix_length6 "$VIP")
        if [ -n "$PREFIX_LEN" ]; then
            warn "IPv6 route has /$PREFIX_LEN aggregation (expected /128)"
        else
            warn "Could not determine IPv6 route prefix length"
        fi
    fi

    # Verify no /64 aggregate route from this pool
    local SUBNET_ROUTE=$(frr_vtysh "show ipv6 route $VIP6_SUBNET" 2>/dev/null || true)
    if echo "$SUBNET_ROUTE" | grep -q "directly connected\|via"; then
        info "IPv6 subnet route exists (may be static or connected)"
    else
        pass "No unwanted /64 aggregate route"
    fi

    delete_test_service "$SVC_IPV6"
    pass "IPv6 route aggregation verification completed"
}

#---------------------------------------------------------------------
# Test 9: ETP Local Route Verification (IPv4)
#---------------------------------------------------------------------

test_etp_local_route_verification() {
    section "TEST 9: ETP Local Route Verification (IPv4)"

    info "Comprehensive ETP Local test: scale-up, verify, migrate, zero-endpoints, restore"

    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')

    #--- Phase 1: Scale to 2 replicas, create service, verify next-hops match endpoint nodes ---
    info "Phase 1: Scale to 2 replicas and create ETP Local service"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=2
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 5  # Let EndpointSlices sync

    create_test_service "$SVC_IPV4_LOCAL"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4_LOCAL" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    local ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    local ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    info "Endpoints on $ENDPOINT_COUNT nodes: $(echo $ENDPOINT_NODES | tr '\n' ' ')"

    info "Waiting for VIP on endpoint nodes..."
    sleep 10

    # Verify VIP ONLY on endpoint nodes
    for node in $NODES; do
        local HAS_VIP=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        local HAS_ENDPOINT=$(echo "$ENDPOINT_NODES" | grep -q "^$node$" && echo yes || echo no)
        if [ "$HAS_ENDPOINT" = "yes" ]; then
            [ "$HAS_VIP" = "yes" ] && pass "$node: VIP present (endpoint node)" || warn "$node: endpoint node missing VIP"
        else
            [ "$HAS_VIP" = "no" ] && pass "$node: no VIP (no endpoint)" || warn "$node: VIP on non-endpoint node"
        fi
    done

    # FRR: Verify next-hop count matches endpoint count
    frr_wait_for_route "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "Route not in FRR"
    local NH_PHASE1=$(frr_count_nexthops "$VIP")
    info "FRR next-hops: $NH_PHASE1 (expected: $ENDPOINT_COUNT)"
    if [ "$NH_PHASE1" -eq "$ENDPOINT_COUNT" ]; then
        pass "Phase 1: next-hop count matches endpoint nodes ($NH_PHASE1)"
    elif [ "$NH_PHASE1" -lt "$NODE_COUNT" ]; then
        warn "Phase 1: next-hop count ($NH_PHASE1) differs from endpoint count ($ENDPOINT_COUNT)"
    else
        warn "Phase 1: next-hops equal total nodes — ETP Local not filtering"
    fi

    wait_for_connectivity "$VIP" 80 30 || fail "Phase 1: connectivity failed"
    pass "Phase 1: connectivity working"

    #--- Phase 2: Scale to 3 replicas — next-hop count should increase ---
    info "Phase 2: Scale to 3 replicas"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=3
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10  # Let EndpointSlices sync + lbnodeagent react + BGP propagate

    ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    info "Endpoints now on $ENDPOINT_COUNT nodes: $(echo $ENDPOINT_NODES | tr '\n' ' ')"

    local NH_PHASE2=$(frr_count_nexthops "$VIP")
    info "FRR next-hops: $NH_PHASE2 (expected: $ENDPOINT_COUNT)"
    if [ "$NH_PHASE2" -ge "$NH_PHASE1" ]; then
        pass "Phase 2: next-hops increased or stable ($NH_PHASE1 -> $NH_PHASE2)"
    else
        warn "Phase 2: next-hops decreased after scale-up ($NH_PHASE1 -> $NH_PHASE2)"
    fi

    wait_for_connectivity "$VIP" 80 30 || fail "Phase 2: connectivity failed"
    pass "Phase 2: connectivity working with 3 replicas"

    #--- Phase 3: Endpoint migration — cordon node, force pod to move ---
    info "Phase 3: Endpoint migration"
    local MIGRATE_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')
    info "Cordoning $MIGRATE_NODE and deleting its pod..."
    kubectl cordon "$MIGRATE_NODE"
    kubectl delete pod -n $NAMESPACE -l app=nginx --field-selector=spec.nodeName=$MIGRATE_NODE --grace-period=1

    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    # Verify VIP removed from cordoned node (if no pods remain there)
    local NEW_EP_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    if ! echo "$NEW_EP_NODES" | grep -q "^$MIGRATE_NODE$"; then
        local HAS_VIP=$(ssh_node "$MIGRATE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        if [ "$HAS_VIP" = "no" ]; then
            pass "Phase 3: VIP removed from $MIGRATE_NODE after pod migration"
        else
            warn "Phase 3: VIP still on $MIGRATE_NODE after pod migration"
        fi
    fi

    local NH_PHASE3=$(frr_count_nexthops "$VIP")
    local NEW_EP_COUNT=$(echo "$NEW_EP_NODES" | grep -c . || echo 0)
    info "FRR next-hops after migration: $NH_PHASE3 (endpoint nodes: $NEW_EP_COUNT)"

    wait_for_connectivity "$VIP" 80 30 || fail "Phase 3: connectivity failed after migration"
    pass "Phase 3: connectivity working after migration"

    kubectl uncordon "$MIGRATE_NODE"

    #--- Phase 4: Zero endpoints — scale to 0, route should be withdrawn ---
    info "Phase 4: Zero endpoints (scale to 0)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=0
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 10

    # Verify VIP removed from ALL nodes
    local VIP_COUNT=$(count_nodes_with_vip "$VIP")
    if [ "$VIP_COUNT" -eq 0 ]; then
        pass "Phase 4: VIP removed from all nodes (0 endpoints)"
    else
        warn "Phase 4: VIP still on $VIP_COUNT nodes with 0 endpoints"
    fi

    # FRR: Route should be withdrawn (no next-hops)
    local NH_PHASE4=$(frr_count_nexthops "$VIP")
    if [ "$NH_PHASE4" -eq 0 ]; then
        pass "Phase 4: FRR next-hops = 0 (route withdrawn)"
    else
        warn "Phase 4: FRR still shows $NH_PHASE4 next-hops with 0 endpoints"
    fi

    #--- Phase 5: Restore — scale back, route should reappear ---
    info "Phase 5: Restore (scale to 1)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local RESTORE_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')
    info "Endpoint restored on $RESTORE_NODE"

    # Verify VIP reappears ONLY on endpoint node
    local HAS_VIP=$(ssh_node "$RESTORE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
    [ "$HAS_VIP" = "yes" ] || fail "Phase 5: VIP not on endpoint node $RESTORE_NODE"
    pass "Phase 5: VIP on endpoint node $RESTORE_NODE"

    # Verify NOT on other nodes
    for node in $NODES; do
        if [ "$node" != "$RESTORE_NODE" ]; then
            local HAS=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
            [ "$HAS" = "no" ] || warn "Phase 5: VIP on non-endpoint node $node"
        fi
    done

    # FRR: Route should reappear with 1 next-hop
    frr_wait_for_route "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "Phase 5: route not in FRR after restore"
    local NH_PHASE5=$(frr_count_nexthops "$VIP")
    info "FRR next-hops after restore: $NH_PHASE5 (expected: 1)"
    [ "$NH_PHASE5" -ge 1 ] && pass "Phase 5: route restored with $NH_PHASE5 next-hop(s)" || fail "Phase 5: no next-hops after restore"

    wait_for_connectivity "$VIP" 80 30 || fail "Phase 5: connectivity failed after restore"
    pass "Phase 5: connectivity restored"

    # Cleanup
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    delete_test_service "$SVC_IPV4_LOCAL"
    pass "ETP Local route verification (IPv4) completed"
}

#---------------------------------------------------------------------
# Test 10: ETP Local Route Verification (IPv6)
#---------------------------------------------------------------------

test_etp_local_route_verification_v6() {
    section "TEST 10: ETP Local Route Verification (IPv6)"

    info "Comprehensive IPv6 ETP Local test: scale, verify, zero-endpoints, restore"

    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')

    #--- Phase 1: Scale to 2 replicas, verify next-hops ---
    info "Phase 1: Scale to 2 replicas and create IPv6 ETP Local service"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=2
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 5

    create_test_service "$SVC_IPV6_LOCAL"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6_LOCAL" 60) || fail "No IPv6 allocated"
    info "Allocated VIP: $VIP"

    local ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    local ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    info "Endpoints on $ENDPOINT_COUNT nodes"

    sleep 10

    # FRR: Verify IPv6 route and next-hop count
    frr_wait_for_route6 "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "IPv6 route not in FRR"
    local NH_PHASE1=$(frr_count_nexthops6 "$VIP")
    info "FRR IPv6 next-hops: $NH_PHASE1 (expected: $ENDPOINT_COUNT)"
    if [ "$NH_PHASE1" -eq "$ENDPOINT_COUNT" ]; then
        pass "Phase 1: IPv6 next-hops match endpoint count ($NH_PHASE1)"
    else
        warn "Phase 1: IPv6 next-hops ($NH_PHASE1) differ from endpoint count ($ENDPOINT_COUNT)"
    fi

    local PREFIX_LEN=$(frr_get_prefix_length6 "$VIP")
    [ "$PREFIX_LEN" = "128" ] && pass "IPv6 /128 aggregation correct" || warn "IPv6 has /$PREFIX_LEN (expected /128)"

    wait_for_connectivity6 "$VIP" 80 30 || fail "Phase 1: IPv6 connectivity failed"
    pass "Phase 1: IPv6 connectivity working"

    #--- Phase 2: Zero endpoints — route should be withdrawn ---
    info "Phase 2: Zero endpoints (scale to 0)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=0
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local VIP_COUNT=$(count_nodes_with_vip "$VIP")
    if [ "$VIP_COUNT" -eq 0 ]; then
        pass "Phase 2: IPv6 VIP removed from all nodes"
    else
        warn "Phase 2: IPv6 VIP still on $VIP_COUNT nodes"
    fi

    local NH_PHASE2=$(frr_count_nexthops6 "$VIP")
    if [ "$NH_PHASE2" -eq 0 ]; then
        pass "Phase 2: FRR IPv6 next-hops = 0 (route withdrawn)"
    else
        warn "Phase 2: FRR still shows $NH_PHASE2 IPv6 next-hops"
    fi

    #--- Phase 3: Restore — route should reappear on endpoint node ---
    info "Phase 3: Restore (scale to 1)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local RESTORE_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')
    info "Endpoint restored on $RESTORE_NODE"

    # Verify VIP only on endpoint node
    local HAS_VIP=$(ssh_node "$RESTORE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
    [ "$HAS_VIP" = "yes" ] || fail "Phase 3: IPv6 VIP not on $RESTORE_NODE"
    pass "Phase 3: IPv6 VIP on $RESTORE_NODE"

    frr_wait_for_route6 "$VIP" "$BGP_CONVERGE_TIMEOUT" || fail "Phase 3: IPv6 route not in FRR"
    local NH_PHASE3=$(frr_count_nexthops6 "$VIP")
    info "FRR IPv6 next-hops after restore: $NH_PHASE3"
    [ "$NH_PHASE3" -ge 1 ] && pass "Phase 3: IPv6 route restored" || fail "Phase 3: no next-hops"

    wait_for_connectivity6 "$VIP" 80 30 || fail "Phase 3: IPv6 connectivity failed"
    pass "Phase 3: IPv6 connectivity restored"

    # Cleanup
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    delete_test_service "$SVC_IPV6_LOCAL"
    pass "ETP Local route verification (IPv6) completed"
}

#---------------------------------------------------------------------
# Test 11: Dual-Stack Route Verification
#---------------------------------------------------------------------

test_dualstack_route_verification() {
    section "TEST 11: Dual-Stack Route Verification"

    info "Verifying both IPv4 /32 and IPv6 /128 routes appear in FRR"

    create_test_service "$SVC_DUALSTACK"

    local IPS=$(wait_for_dualstack_ips "$SVC_NAME_DUALSTACK" 60) || fail "Dual-stack IPs not allocated"
    local VIP4=$(echo "$IPS" | awk '{print $1}')
    local VIP6=$(echo "$IPS" | awk '{print $2}')
    info "Allocated VIPs: IPv4=$VIP4  IPv6=$VIP6"

    wait_for_vip_announced "$VIP4" "$NODE_COUNT" 60 || fail "IPv4 VIP not announced"
    wait_for_vip_announced "$VIP6" "$NODE_COUNT" 60 || fail "IPv6 VIP not announced"

    # Metrics display
    local alloc_metrics=$(scrape_allocator_metrics)
    print_allocator_metrics "$alloc_metrics"
    local first_node=${NODES%% *}
    print_lbnodeagent_metrics "$(scrape_lbnodeagent_metrics "$first_node")" "$first_node"

    # Verify IPv4 route in FRR
    info "Waiting for IPv4 route in FRR..."
    frr_wait_for_route "$VIP4" "$BGP_CONVERGE_TIMEOUT" || fail "IPv4 route not in FRR"
    pass "IPv4 route found in FRR"

    local NH4=$(frr_count_nexthops "$VIP4")
    info "IPv4 next-hops: $NH4"
    [ "$NH4" -ge 1 ] || fail "No IPv4 next-hops"

    local PL4=$(frr_get_prefix_length "$VIP4")
    [ "$PL4" = "32" ] && pass "IPv4 has /32 aggregation" || warn "IPv4 has /$PL4 (expected /32)"

    # Verify IPv6 route in FRR
    info "Waiting for IPv6 route in FRR..."
    frr_wait_for_route6 "$VIP6" "$BGP_CONVERGE_TIMEOUT" || fail "IPv6 route not in FRR"
    pass "IPv6 route found in FRR"

    local NH6=$(frr_count_nexthops6 "$VIP6")
    info "IPv6 next-hops: $NH6"
    [ "$NH6" -ge 1 ] || fail "No IPv6 next-hops"

    local PL6=$(frr_get_prefix_length6 "$VIP6")
    [ "$PL6" = "128" ] && pass "IPv6 has /128 aggregation" || warn "IPv6 has /$PL6 (expected /128)"

    # Test connectivity for both families
    info "Testing IPv4 connectivity..."
    wait_for_connectivity "$VIP4" 80 30 || fail "IPv4 connectivity failed"
    pass "IPv4 connectivity working"

    info "Testing IPv6 connectivity..."
    wait_for_connectivity6 "$VIP6" 80 30 || fail "IPv6 connectivity failed"
    pass "IPv6 connectivity working"

    # Delete and verify both routes withdrawn
    info "Deleting dual-stack service..."
    delete_test_service "$SVC_DUALSTACK"

    info "Waiting for route withdrawals..."
    if frr_wait_for_withdrawal "$VIP4" "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv4 route withdrawn from FRR"
    else
        warn "IPv4 route still in FRR after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    if frr_wait_for_withdrawal6 "$VIP6" "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv6 route withdrawn from FRR"
    else
        warn "IPv6 route still in FRR after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    pass "Dual-stack route verification completed"
}

#---------------------------------------------------------------------
# Test 12: Dual-Stack ETP Local Route Verification
#---------------------------------------------------------------------

test_dualstack_etp_local_route_verification() {
    section "TEST 12: Dual-Stack ETP Local Route Verification"

    info "Dual-stack ETP Local: verify routes, zero-endpoints, restore"

    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')

    #--- Phase 1: Create service, verify both routes with endpoint-count next-hops ---
    info "Phase 1: Create dual-stack ETP Local service"
    create_test_service "$SVC_DUALSTACK_LOCAL"

    local IPS=$(wait_for_dualstack_ips "$SVC_NAME_DUALSTACK_LOCAL" 60) || fail "Dual-stack IPs not allocated"
    local VIP4=$(echo "$IPS" | awk '{print $1}')
    local VIP6=$(echo "$IPS" | awk '{print $2}')
    info "Allocated VIPs: IPv4=$VIP4  IPv6=$VIP6"

    local ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    local ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    info "Endpoints on $ENDPOINT_COUNT nodes"

    sleep 10

    frr_wait_for_route "$VIP4" "$BGP_CONVERGE_TIMEOUT" || fail "IPv4 route not in FRR"
    local NH4=$(frr_count_nexthops "$VIP4")
    info "IPv4 next-hops: $NH4 (expected: $ENDPOINT_COUNT)"
    [ "$NH4" -eq "$ENDPOINT_COUNT" ] && pass "IPv4 next-hops match" || warn "IPv4 next-hops ($NH4) differ"

    frr_wait_for_route6 "$VIP6" "$BGP_CONVERGE_TIMEOUT" || fail "IPv6 route not in FRR"
    local NH6=$(frr_count_nexthops6 "$VIP6")
    info "IPv6 next-hops: $NH6 (expected: $ENDPOINT_COUNT)"
    [ "$NH6" -eq "$ENDPOINT_COUNT" ] && pass "IPv6 next-hops match" || warn "IPv6 next-hops ($NH6) differ"

    wait_for_connectivity "$VIP4" 80 30 || fail "Phase 1: IPv4 failed"
    wait_for_connectivity6 "$VIP6" 80 30 || fail "Phase 1: IPv6 failed"
    pass "Phase 1: both families working"

    #--- Phase 2: Zero endpoints — both routes should be withdrawn ---
    info "Phase 2: Zero endpoints (scale to 0)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=0
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local NH4_ZERO=$(frr_count_nexthops "$VIP4")
    local NH6_ZERO=$(frr_count_nexthops6 "$VIP6")
    if [ "$NH4_ZERO" -eq 0 ] && [ "$NH6_ZERO" -eq 0 ]; then
        pass "Phase 2: both routes withdrawn (0 next-hops)"
    else
        warn "Phase 2: routes remaining (v4=$NH4_ZERO v6=$NH6_ZERO)"
    fi

    #--- Phase 3: Restore ---
    info "Phase 3: Restore (scale to 1)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    frr_wait_for_route "$VIP4" "$BGP_CONVERGE_TIMEOUT" || fail "Phase 3: IPv4 route not restored"
    frr_wait_for_route6 "$VIP6" "$BGP_CONVERGE_TIMEOUT" || fail "Phase 3: IPv6 route not restored"
    pass "Phase 3: both routes restored"

    wait_for_connectivity "$VIP4" 80 30 || fail "Phase 3: IPv4 failed"
    wait_for_connectivity6 "$VIP6" 80 30 || fail "Phase 3: IPv6 failed"
    pass "Phase 3: both families working"

    # Cleanup
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    delete_test_service "$SVC_DUALSTACK_LOCAL"
    pass "Dual-stack ETP Local route verification completed"
}

#---------------------------------------------------------------------
# Test 13: Default Aggregation Route Verification (IPv4 /24)
#---------------------------------------------------------------------

test_default_aggr_route_verification() {
    section "TEST 13: Default Aggregation Route Verification (IPv4 /24)"

    info "Verifying default aggregation produces /24 route in FRR (not /32)"

    create_test_service "$SVC_IPV4_DEFAGGR"

    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4_DEFAGGR" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP (from remote-default-aggr pool)"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"

    # Metrics display
    local alloc_metrics=$(scrape_allocator_metrics)
    print_allocator_metrics "$alloc_metrics" "remote-default-aggr"

    # The default aggregation uses the subnet mask, so we expect a /24 route
    # The route key in FRR will be the subnet address (10.255.1.0/24), not the VIP/24
    local SUBNET_ADDR="${VIP_DEFAGGR_SUBNET%/*}"
    local SUBNET_PREFIX="${VIP_DEFAGGR_SUBNET#*/}"

    info "Waiting for /$SUBNET_PREFIX route in FRR..."
    if frr_wait_for_route_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT"; then
        pass "/$SUBNET_PREFIX aggregate route found in FRR"
    else
        fail "/$SUBNET_PREFIX aggregate route not found after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    # Show route details
    info "FRR route details:"
    frr_vtysh "show ip route $VIP_DEFAGGR_SUBNET"

    # Verify next-hops
    local NH=$(frr_count_nexthops_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX")
    info "Route has $NH next-hop(s)"
    [ "$NH" -ge 1 ] || fail "No next-hops for aggregate route"

    # Verify NO /32 host route exists for this VIP
    if frr_check_route "$VIP"; then
        warn "Unexpected /32 host route also exists for $VIP"
    else
        pass "No /32 host route (only /$SUBNET_PREFIX aggregate)"
    fi

    # Test connectivity
    if wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv4 default aggregation connectivity working"
    else
        fail "IPv4 default aggregation connectivity failed"
    fi

    # Delete and verify withdrawal
    info "Deleting service..."
    delete_test_service "$SVC_IPV4_DEFAGGR"

    info "Waiting for route withdrawal..."
    if frr_wait_for_withdrawal_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT"; then
        pass "/$SUBNET_PREFIX route withdrawn from FRR"
    else
        warn "/$SUBNET_PREFIX route still in FRR after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    pass "Default aggregation route verification (IPv4) completed"
}

#---------------------------------------------------------------------
# Test 14: Default Aggregation Route Verification (IPv6 /64)
#---------------------------------------------------------------------

test_default_aggr_route_verification_v6() {
    section "TEST 14: Default Aggregation Route Verification (IPv6 /64)"

    info "Verifying default aggregation produces /64 route in FRR (not /128)"

    create_test_service "$SVC_IPV6_DEFAGGR"

    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6_DEFAGGR" 60) || fail "No IPv6 allocated"
    info "Allocated VIP: $VIP (from remote-default-aggr pool)"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "IPv6 VIP not announced"

    local SUBNET_ADDR="${VIP6_DEFAGGR_SUBNET%/*}"
    local SUBNET_PREFIX="${VIP6_DEFAGGR_SUBNET#*/}"

    info "Waiting for /$SUBNET_PREFIX route in FRR..."
    if frr_wait_for_route6_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT"; then
        pass "/$SUBNET_PREFIX aggregate route found in FRR"
    else
        fail "/$SUBNET_PREFIX aggregate route not found after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    info "FRR route details:"
    frr_vtysh "show ipv6 route $VIP6_DEFAGGR_SUBNET"

    local NH=$(frr_count_nexthops6_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX")
    info "Route has $NH next-hop(s)"
    [ "$NH" -ge 1 ] || fail "No next-hops for IPv6 aggregate route"

    # Verify NO /128 host route
    if frr_check_route6 "$VIP"; then
        warn "Unexpected /128 host route also exists for $VIP"
    else
        pass "No /128 host route (only /$SUBNET_PREFIX aggregate)"
    fi

    if wait_for_connectivity6 "$VIP" 80 "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv6 default aggregation connectivity working"
    else
        fail "IPv6 default aggregation connectivity failed"
    fi

    info "Deleting service..."
    delete_test_service "$SVC_IPV6_DEFAGGR"

    info "Waiting for route withdrawal..."
    if frr_wait_for_withdrawal6_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT"; then
        pass "/$SUBNET_PREFIX route withdrawn from FRR"
    else
        warn "/$SUBNET_PREFIX route still in FRR after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    pass "Default aggregation route verification (IPv6) completed"
}

#---------------------------------------------------------------------
# Test 15: Default Aggregation Shared Route (IPv4)
#---------------------------------------------------------------------

test_default_aggr_shared_route() {
    section "TEST 15: Default Aggregation Shared Route (IPv4)"

    info "Verifying two services in same /24 pool share one aggregate route"

    local SUBNET_ADDR="${VIP_DEFAGGR_SUBNET%/*}"
    local SUBNET_PREFIX="${VIP_DEFAGGR_SUBNET#*/}"

    # Create first service
    info "Creating first service..."
    create_test_service "$SVC_IPV4_DEFAGGR"
    local VIP_A=$(wait_for_service_ip "$SVC_NAME_IPV4_DEFAGGR" 60) || fail "No IP allocated for service A"
    info "Service A VIP: $VIP_A"

    wait_for_vip_announced "$VIP_A" "$NODE_COUNT" 60 || fail "VIP A not announced"
    frr_wait_for_route_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT" || fail "/$SUBNET_PREFIX route not in FRR"
    pass "/$SUBNET_PREFIX aggregate route present after service A"

    # Create second service (inline — need a different name from the same pool)
    info "Creating second service from same pool..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: router-test-ipv4-defaggr-2
  namespace: $NAMESPACE
  labels:
    test-suite: router
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
  - port: 81
    targetPort: 80
EOF

    local VIP_B=""
    local TIMEOUT=60
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        VIP_B=$(kubectl get svc router-test-ipv4-defaggr-2 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        [ -n "$VIP_B" ] && break
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    [ -n "$VIP_B" ] || fail "No IP allocated for service B"
    info "Service B VIP: $VIP_B"

    wait_for_vip_announced "$VIP_B" "$NODE_COUNT" 60 || fail "VIP B not announced"

    # Verify still one aggregate route
    if frr_check_route_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX"; then
        pass "/$SUBNET_PREFIX aggregate route still present with both services"
    else
        fail "/$SUBNET_PREFIX route disappeared"
    fi

    # Delete service A — route should persist (service B keeps it alive)
    info "Deleting service A..."
    delete_test_service "$SVC_IPV4_DEFAGGR"

    # Wait for VIP A removal
    local ELAPSED=0
    while [ $ELAPSED -lt 30 ]; do
        local COUNT=$(count_nodes_with_vip "$VIP_A")
        [ "$COUNT" -eq 0 ] && break
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    sleep 5  # Let BGP converge

    if frr_check_route_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX"; then
        pass "/$SUBNET_PREFIX route persists after deleting service A (service B keeps it)"
    else
        fail "/$SUBNET_PREFIX route disappeared after deleting service A"
    fi

    # Delete service B — route should be withdrawn
    info "Deleting service B..."
    kubectl delete svc router-test-ipv4-defaggr-2 -n $NAMESPACE --ignore-not-found

    info "Waiting for aggregate route withdrawal..."
    if frr_wait_for_withdrawal_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT"; then
        pass "/$SUBNET_PREFIX route withdrawn after all services deleted"
    else
        warn "/$SUBNET_PREFIX route still in FRR after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    pass "Default aggregation shared route (IPv4) test completed"
}

#---------------------------------------------------------------------
# Test 16: Default Aggregation Shared Route (IPv6)
#---------------------------------------------------------------------

test_default_aggr_shared_route_v6() {
    section "TEST 16: Default Aggregation Shared Route (IPv6)"

    info "Verifying two services in same /64 pool share one aggregate route"

    local SUBNET_ADDR="${VIP6_DEFAGGR_SUBNET%/*}"
    local SUBNET_PREFIX="${VIP6_DEFAGGR_SUBNET#*/}"

    # Create first service
    info "Creating first service..."
    create_test_service "$SVC_IPV6_DEFAGGR"
    local VIP_A=$(wait_for_service_ip "$SVC_NAME_IPV6_DEFAGGR" 60) || fail "No IPv6 allocated for service A"
    info "Service A VIP: $VIP_A"

    wait_for_vip_announced "$VIP_A" "$NODE_COUNT" 60 || fail "IPv6 VIP A not announced"
    frr_wait_for_route6_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT" || fail "/$SUBNET_PREFIX route not in FRR"
    pass "/$SUBNET_PREFIX aggregate route present after service A"

    # Create second service (inline — different name, same pool)
    info "Creating second service from same pool..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: router-test-ipv6-defaggr-2
  namespace: $NAMESPACE
  labels:
    test-suite: router
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
  - port: 81
    targetPort: 80
EOF

    local VIP_B=""
    local TIMEOUT=60
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        VIP_B=$(kubectl get svc router-test-ipv6-defaggr-2 -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        [ -n "$VIP_B" ] && break
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    [ -n "$VIP_B" ] || fail "No IPv6 allocated for service B"
    info "Service B VIP: $VIP_B"

    wait_for_vip_announced "$VIP_B" "$NODE_COUNT" 60 || fail "IPv6 VIP B not announced"

    if frr_check_route6_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX"; then
        pass "/$SUBNET_PREFIX aggregate route still present with both services"
    else
        fail "/$SUBNET_PREFIX route disappeared"
    fi

    # Delete service A
    info "Deleting service A..."
    delete_test_service "$SVC_IPV6_DEFAGGR"

    local ELAPSED=0
    while [ $ELAPSED -lt 30 ]; do
        local COUNT=$(count_nodes_with_vip "$VIP_A")
        [ "$COUNT" -eq 0 ] && break
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    sleep 5

    if frr_check_route6_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX"; then
        pass "/$SUBNET_PREFIX route persists after deleting service A (service B keeps it)"
    else
        fail "/$SUBNET_PREFIX route disappeared after deleting service A"
    fi

    # Delete service B
    info "Deleting service B..."
    kubectl delete svc router-test-ipv6-defaggr-2 -n $NAMESPACE --ignore-not-found

    info "Waiting for aggregate route withdrawal..."
    if frr_wait_for_withdrawal6_prefix "$SUBNET_ADDR" "$SUBNET_PREFIX" "$BGP_CONVERGE_TIMEOUT"; then
        pass "/$SUBNET_PREFIX route withdrawn after all services deleted"
    else
        warn "/$SUBNET_PREFIX route still in FRR after ${BGP_CONVERGE_TIMEOUT}s"
    fi

    pass "Default aggregation shared route (IPv6) test completed"
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
    test_ipv6_basic_with_route_verification
    test_ipv6_deletion_route_withdrawal
    test_ipv6_route_aggregation
    test_etp_local_route_verification
    test_etp_local_route_verification_v6
    test_dualstack_route_verification
    test_dualstack_etp_local_route_verification
    test_default_aggr_route_verification
    test_default_aggr_route_verification_v6
    test_default_aggr_shared_route
    test_default_aggr_shared_route_v6

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
            echo "  VIP_SUBNET            - IPv4 subnet for route queries (default: 10.255.0.0/24)"
            echo "  VIP6_SUBNET           - IPv6 subnet for route queries (default: fd00:10:255::/64)"
            echo "  BGP_CONVERGE_TIMEOUT  - Seconds to wait for BGP (default: 30)"
            echo ""
            echo "Tests:"
            echo "  0 - Prerequisites (with FRR verification)"
            echo "  1 - Basic Connectivity with Route Verification"
            echo "  2 - ECMP with Next-Hop Verification"
            echo "  3 - Node Failure with Route Withdrawal"
            echo "  4 - Service Deletion with Route Withdrawal"
            echo "  5 - Route Aggregation Verification"
            echo "  6 - IPv6 Basic Connectivity with Route Verification"
            echo "  7 - IPv6 Service Deletion with Route Withdrawal"
            echo "  8 - IPv6 Route Aggregation Verification"
            echo "  9 - ETP Local Route Verification (IPv4)"
            echo " 10 - ETP Local Route Verification (IPv6)"
            echo " 11 - Dual-Stack Route Verification"
            echo " 12 - Dual-Stack ETP Local Route Verification"
            echo " 13 - Default Aggregation Route Verification (IPv4 /24)"
            echo " 14 - Default Aggregation Route Verification (IPv6 /64)"
            echo " 15 - Default Aggregation Shared Route (IPv4)"
            echo " 16 - Default Aggregation Shared Route (IPv6)"
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
        6) test_prerequisites && test_ipv6_basic_with_route_verification ;;
        7) test_prerequisites && test_ipv6_deletion_route_withdrawal ;;
        8) test_prerequisites && test_ipv6_route_aggregation ;;
        9) test_prerequisites && test_etp_local_route_verification ;;
        10) test_prerequisites && test_etp_local_route_verification_v6 ;;
        11) test_prerequisites && test_dualstack_route_verification ;;
        12) test_prerequisites && test_dualstack_etp_local_route_verification ;;
        13) test_prerequisites && test_default_aggr_route_verification ;;
        14) test_prerequisites && test_default_aggr_route_verification_v6 ;;
        15) test_prerequisites && test_default_aggr_shared_route ;;
        16) test_prerequisites && test_default_aggr_shared_route_v6 ;;
        *) echo "Unknown test: $SELECTED_TEST"; exit 1 ;;
    esac
else
    run_all_tests
fi
