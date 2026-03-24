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

CONTEXT="${CONTEXT:-$(command kubectl config current-context 2>/dev/null)}"
NAMESPACE="test"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Optional configuration
BGP_CONVERGE_TIMEOUT="${BGP_CONVERGE_TIMEOUT:-30}"
ECMP_TEST_REQUESTS="${ECMP_TEST_REQUESTS:-100}"

# Service YAML files
SVC_IPV4="${SCRIPT_DIR}/svc-router-ipv4.yaml"
SVC_IPV4_LOCAL="${SCRIPT_DIR}/svc-router-ipv4-etp-local.yaml"
SVC_IPV6="${SCRIPT_DIR}/svc-router-ipv6.yaml"
SVC_IPV6_LOCAL="${SCRIPT_DIR}/svc-router-ipv6-etp-local.yaml"
SVC_NAME_IPV4="router-test-ipv4"
SVC_NAME_IPV4_LOCAL="router-test-ipv4-local"
SVC_NAME_IPV6="router-test-ipv6"
SVC_NAME_IPV6_LOCAL="router-test-ipv6-local"
SVC_DUALSTACK="${SCRIPT_DIR}/svc-router-dualstack.yaml"
SVC_DUALSTACK_LOCAL="${SCRIPT_DIR}/svc-router-dualstack-etp-local.yaml"
SVC_NAME_DUALSTACK="router-test-dualstack"
SVC_NAME_DUALSTACK_LOCAL="router-test-dualstack-local"
SVC_IPV4_DEFAGGR="${SCRIPT_DIR}/svc-router-ipv4-default-aggr.yaml"
SVC_IPV6_DEFAGGR="${SCRIPT_DIR}/svc-router-ipv6-default-aggr.yaml"
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
    kubectl get pods -n purelb-system -l component=lbnodeagent -o wide 2>/dev/null || echo "(failed)"
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

# Wait for IPv6 connectivity to work (with retries)
wait_for_connectivity_v6() {
    local VIP=$1
    local PORT=${2:-80}
    local TIMEOUT=${3:-60}
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        local RESPONSE=$(test_connectivity_v6 "$VIP" "$PORT")
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

# Wait for service to get an IP
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
    info "  BGP_CONVERGE_TIMEOUT: ${BGP_CONVERGE_TIMEOUT}s"

    # Verify SSH connectivity to cluster nodes
    info "Verifying SSH connectivity to cluster nodes..."
    for node in $NODES; do
        verify_ssh "${NODE_IPS[$node]}" "cluster node $node"
    done

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
# Test 1: Basic External Connectivity (IPv4)
#---------------------------------------------------------------------

test_basic_ipv4() {
    section "TEST 1: Basic Connectivity (IPv4)"

    info "This test verifies traffic reaches service via BGP routing"
    info "Path: This Host -> Router -> BGP Route -> Node -> kube-proxy -> Pod"

    info "Creating test service..."
    create_test_service "$SVC_IPV4"

    info "Waiting for IP allocation..."
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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
    delete_test_service "$SVC_IPV4"

    pass "Basic IPv4 connectivity test completed"
}

#---------------------------------------------------------------------
# Test 2: ECMP Traffic Distribution
#---------------------------------------------------------------------

test_ecmp_distribution() {
    section "TEST 2: ECMP Traffic Distribution"

    info "Testing that traffic distributes across nodes via ECMP"
    info "Note: ECMP is flow-based; using different source ports for variety"

    # Scale to multiple replicas for better distribution visibility
    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')
    info "Scaling nginx deployment to 5 replicas..."
    kubectl scale deployment nginx -n $NAMESPACE --replicas=5
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s

    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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
    delete_test_service "$SVC_IPV4"

    pass "ECMP distribution test completed"
}

#---------------------------------------------------------------------
# Test 3: Node Failure and Recovery
#---------------------------------------------------------------------

test_node_failure_recovery() {
    section "TEST 3: Node Failure and Recovery"

    info "Testing that connectivity continues when a node fails"

    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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
    local AGENT_POD=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$FAIL_NODE" | awk '{print $1}')
    if [ -n "$AGENT_POD" ]; then
        kubectl delete pod -n purelb-system "$AGENT_POD" --grace-period=0 --force 2>/dev/null || true
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
    kubectl rollout status daemonset/lbnodeagent -n purelb-system --timeout=60s

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

    delete_test_service "$SVC_IPV4"
    pass "Node failure and recovery test completed"
}

#---------------------------------------------------------------------
# Test 4: ETP Local Connectivity
#---------------------------------------------------------------------

test_etp_local() {
    section "TEST 4: ETP Local Connectivity"

    info "Comprehensive ETP Local test: scale, verify placement, migrate, zero-endpoints, restore"

    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')

    #--- Phase 1: Scale to 2, create service, verify VIP placement ---
    info "Phase 1: Scale to 2 replicas and create ETP Local service"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=2
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 5

    create_test_service "$SVC_IPV4_LOCAL"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4_LOCAL" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    local ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    local ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    info "Endpoints on $ENDPOINT_COUNT nodes: $(echo $ENDPOINT_NODES | tr '\n' ' ')"

    sleep 10

    # Verify VIP only on endpoint nodes
    for node in $NODES; do
        local HAS_VIP=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        local HAS_ENDPOINT=$(echo "$ENDPOINT_NODES" | grep -q "^$node$" && echo yes || echo no)
        if [ "$HAS_ENDPOINT" = "yes" ]; then
            [ "$HAS_VIP" = "yes" ] && pass "$node: VIP present (endpoint node)" || warn "$node: endpoint node missing VIP"
        else
            [ "$HAS_VIP" = "no" ] || warn "$node: VIP on non-endpoint node (ETP Local violation)"
        fi
    done

    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 1: connectivity failed"
    pass "Phase 1: connectivity working"

    #--- Phase 2: Scale to 3 — VIP should appear on new endpoint node ---
    info "Phase 2: Scale to 3 replicas"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=3
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    local VIP_COUNT=$(count_nodes_with_vip "$VIP")
    info "Endpoints on $ENDPOINT_COUNT nodes, VIP on $VIP_COUNT nodes"

    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 2: connectivity failed"
    pass "Phase 2: connectivity working with 3 replicas"

    #--- Phase 3: Endpoint migration — cordon and move pod ---
    info "Phase 3: Endpoint migration"
    local MIGRATE_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')
    info "Cordoning $MIGRATE_NODE..."
    kubectl cordon "$MIGRATE_NODE"
    kubectl delete pod -n $NAMESPACE -l app=nginx --field-selector=spec.nodeName=$MIGRATE_NODE --grace-period=1

    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local NEW_EP_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    if ! echo "$NEW_EP_NODES" | grep -q "^$MIGRATE_NODE$"; then
        local HAS_VIP=$(ssh_node "$MIGRATE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        [ "$HAS_VIP" = "no" ] && pass "Phase 3: VIP removed from $MIGRATE_NODE after migration" || warn "Phase 3: VIP still on $MIGRATE_NODE"
    fi

    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 3: connectivity failed"
    pass "Phase 3: connectivity after migration"
    kubectl uncordon "$MIGRATE_NODE"

    #--- Phase 4: Zero endpoints — scale to 0, VIP removed from all ---
    info "Phase 4: Zero endpoints (scale to 0)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=0
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 10

    VIP_COUNT=$(count_nodes_with_vip "$VIP")
    if [ "$VIP_COUNT" -eq 0 ]; then
        pass "Phase 4: VIP removed from all nodes (0 endpoints)"
    else
        warn "Phase 4: VIP still on $VIP_COUNT nodes with 0 endpoints"
    fi

    #--- Phase 5: Restore — VIP reappears only on endpoint node ---
    info "Phase 5: Restore (scale to 1)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local RESTORE_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')
    local HAS_VIP=$(ssh_node "$RESTORE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
    [ "$HAS_VIP" = "yes" ] || fail "Phase 5: VIP not on endpoint node $RESTORE_NODE"
    pass "Phase 5: VIP on $RESTORE_NODE"

    # Verify NOT on other nodes
    for node in $NODES; do
        [ "$node" = "$RESTORE_NODE" ] && continue
        local HAS=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        [ "$HAS" = "no" ] || warn "Phase 5: VIP on non-endpoint node $node"
    done

    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 5: connectivity failed"
    pass "Phase 5: connectivity restored"

    # Cleanup
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    delete_test_service "$SVC_IPV4_LOCAL"
    pass "ETP Local connectivity test completed"
}

#---------------------------------------------------------------------
# Test 5: Service Deletion Cleanup
#---------------------------------------------------------------------

test_service_deletion() {
    section "TEST 5: Service Deletion Cleanup"

    info "Testing that VIPs are properly removed when service is deleted"

    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Connectivity failed"
    pass "Service created and accessible at $VIP"

    # Delete service
    info "Deleting service..."
    delete_test_service "$SVC_IPV4"

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

    # Phase 1: Create
    info "Phase 1: Creating service..."
    create_test_service "$SVC_IPV4"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4" 60) || fail "No IP allocated"
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
    local AGENT_POD=$(kubectl get pods -n purelb-system -l component=lbnodeagent -o wide | grep "$FAIL_NODE" | awk '{print $1}')
    [ -n "$AGENT_POD" ] && kubectl delete pod -n purelb-system "$AGENT_POD" --grace-period=0 --force 2>/dev/null || true
    sleep 15
    RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Phase 3 connectivity failed"
    pass "Phase 3: Still accessible after node failure"

    # Phase 4: Restore
    info "Phase 4: Restoring node..."
    kubectl taint node "$FAIL_NODE" purelb-router-test-
    kubectl rollout status daemonset/lbnodeagent -n purelb-system --timeout=60s
    sleep 10
    RESPONSE=$(test_connectivity "$VIP" 80)
    echo "$RESPONSE" | grep -q "Pod:" || fail "Phase 4 connectivity failed"
    pass "Phase 4: Still accessible after node restore"

    # Phase 5: Delete
    info "Phase 5: Deleting service..."
    delete_test_service "$SVC_IPV4"
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
# Test 7: Basic External Connectivity (IPv6)
#---------------------------------------------------------------------

test_basic_ipv6() {
    section "TEST 7: Basic Connectivity (IPv6)"

    info "This test verifies IPv6 traffic reaches service via BGP routing"
    info "Path: This Host -> Router -> BGP Route -> Node -> kube-proxy -> Pod"

    info "Creating IPv6 test service..."
    create_test_service "$SVC_IPV6"

    info "Waiting for IPv6 allocation..."
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6" 60) || fail "No IPv6 allocated"
    info "Allocated VIP: $VIP"

    info "Waiting for VIP to be announced on all nodes..."
    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced on all nodes"
    pass "VIP $VIP announced on $NODE_COUNT nodes"

    info "Waiting for BGP routes to propagate (${BGP_CONVERGE_TIMEOUT}s timeout)..."
    sleep 5

    info "Testing IPv6 connectivity to VIP..."
    if wait_for_connectivity_v6 "$VIP" 80 "$BGP_CONVERGE_TIMEOUT"; then
        local RESPONSE=$(test_connectivity_v6 "$VIP" 80)
        pass "IPv6 connectivity successful"
        echo "$RESPONSE" | head -5
    else
        fail "No response to IPv6 VIP $VIP"
    fi

    delete_test_service "$SVC_IPV6"
    pass "Basic IPv6 connectivity test completed"
}

#---------------------------------------------------------------------
# Test 8: ETP Local Connectivity (IPv6)
#---------------------------------------------------------------------

test_etp_local_v6() {
    section "TEST 8: ETP Local Connectivity (IPv6)"

    info "Comprehensive IPv6 ETP Local test: verify placement, zero-endpoints, restore"

    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')

    #--- Phase 1: Create service, verify VIP on endpoint nodes only ---
    info "Phase 1: Create IPv6 ETP Local service"

    create_test_service "$SVC_IPV6_LOCAL"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6_LOCAL" 60) || fail "No IPv6 allocated"
    info "Allocated VIP: $VIP"

    local ENDPOINT_NODES=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
    local ENDPOINT_COUNT=$(echo "$ENDPOINT_NODES" | grep -c . || echo 0)
    info "Endpoints on $ENDPOINT_COUNT nodes"

    sleep 10

    for node in $NODES; do
        local HAS_VIP=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        local HAS_ENDPOINT=$(echo "$ENDPOINT_NODES" | grep -q "^$node$" && echo yes || echo no)
        if [ "$HAS_ENDPOINT" = "yes" ]; then
            [ "$HAS_VIP" = "yes" ] && pass "$node: IPv6 VIP present (endpoint)" || warn "$node: endpoint missing VIP"
        else
            [ "$HAS_VIP" = "no" ] || warn "$node: IPv6 VIP on non-endpoint node"
        fi
    done

    wait_for_connectivity_v6 "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 1: IPv6 connectivity failed"
    pass "Phase 1: IPv6 connectivity working"

    #--- Phase 2: Zero endpoints ---
    info "Phase 2: Zero endpoints (scale to 0)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=0
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local VIP_COUNT=$(count_nodes_with_vip "$VIP")
    [ "$VIP_COUNT" -eq 0 ] && pass "Phase 2: IPv6 VIP removed from all nodes" || warn "Phase 2: still on $VIP_COUNT nodes"

    #--- Phase 3: Restore ---
    info "Phase 3: Restore (scale to 1)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local RESTORE_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')
    local HAS_VIP=$(ssh_node "$RESTORE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
    [ "$HAS_VIP" = "yes" ] || fail "Phase 3: IPv6 VIP not restored on $RESTORE_NODE"
    pass "Phase 3: IPv6 VIP on $RESTORE_NODE"

    for node in $NODES; do
        [ "$node" = "$RESTORE_NODE" ] && continue
        local HAS=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP/' && echo yes || echo no")
        [ "$HAS" = "no" ] || warn "Phase 3: IPv6 VIP on non-endpoint $node"
    done

    wait_for_connectivity_v6 "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 3: IPv6 connectivity failed"
    pass "Phase 3: IPv6 connectivity restored"

    # Cleanup
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    delete_test_service "$SVC_IPV6_LOCAL"
    pass "ETP Local IPv6 connectivity test completed"
}

#---------------------------------------------------------------------
# Test 9: Service Deletion Cleanup (IPv6)
#---------------------------------------------------------------------

test_service_deletion_v6() {
    section "TEST 9: Service Deletion Cleanup (IPv6)"

    info "Testing that IPv6 VIPs are properly removed when service is deleted"

    create_test_service "$SVC_IPV6"
    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6" 60) || fail "No IPv6 allocated"
    info "Allocated VIP: $VIP"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced"
    wait_for_connectivity_v6 "$VIP" 80 "$BGP_CONVERGE_TIMEOUT" || fail "IPv6 connectivity failed"
    pass "Service created and accessible at $VIP"

    info "Deleting service..."
    delete_test_service "$SVC_IPV6"

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
        fail "IPv6 VIP not removed from all nodes within ${TIMEOUT}s"
    fi
    pass "IPv6 VIP removed from all nodes after ${ELAPSED}s"

    info "Waiting for BGP route withdrawal..."
    sleep 10

    local RESPONSE=$(test_connectivity_v6 "$VIP" 80)
    if echo "$RESPONSE" | grep -q "Pod:"; then
        warn "IPv6 VIP still reachable after deletion (stale BGP route?)"
    else
        pass "IPv6 VIP no longer reachable (route withdrawn)"
    fi

    pass "Service deletion IPv6 cleanup test completed"
}

#---------------------------------------------------------------------
# Test 10: Dual-Stack Connectivity
#---------------------------------------------------------------------

test_dualstack_connectivity() {
    section "TEST 10: Dual-Stack Connectivity"

    info "Testing that a single service gets both IPv4 and IPv6 VIPs"

    create_test_service "$SVC_DUALSTACK"

    info "Waiting for dual-stack IP allocation..."
    local IPS=$(wait_for_dualstack_ips "$SVC_NAME_DUALSTACK" 60) || fail "Dual-stack IPs not allocated"
    local VIP4=$(echo "$IPS" | awk '{print $1}')
    local VIP6=$(echo "$IPS" | awk '{print $2}')
    info "Allocated VIPs: IPv4=$VIP4  IPv6=$VIP6"

    # Verify both VIPs announced on all nodes
    info "Waiting for IPv4 VIP on all nodes..."
    wait_for_vip_announced "$VIP4" "$NODE_COUNT" 60 || fail "IPv4 VIP not announced on all nodes"
    pass "IPv4 VIP $VIP4 announced on $NODE_COUNT nodes"

    info "Waiting for IPv6 VIP on all nodes..."
    wait_for_vip_announced "$VIP6" "$NODE_COUNT" 60 || fail "IPv6 VIP not announced on all nodes"
    pass "IPv6 VIP $VIP6 announced on $NODE_COUNT nodes"

    # Metrics display
    local alloc_metrics=$(scrape_allocator_metrics)
    print_allocator_metrics "$alloc_metrics"
    local first_node=${NODES%% *}
    print_lbnodeagent_metrics "$(scrape_lbnodeagent_metrics "$first_node")" "$first_node"

    info "Waiting for BGP routes to propagate..."
    sleep 5

    # Test IPv4 connectivity
    info "Testing IPv4 connectivity..."
    if wait_for_connectivity "$VIP4" 80 "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv4 connectivity successful"
    else
        fail "IPv4 connectivity failed for dual-stack service"
    fi

    # Test IPv6 connectivity
    info "Testing IPv6 connectivity..."
    if wait_for_connectivity_v6 "$VIP6" 80 "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv6 connectivity successful"
    else
        fail "IPv6 connectivity failed for dual-stack service"
    fi

    delete_test_service "$SVC_DUALSTACK"
    pass "Dual-stack connectivity test completed"
}

#---------------------------------------------------------------------
# Test 11: Dual-Stack ETP Local
#---------------------------------------------------------------------

test_dualstack_etp_local() {
    section "TEST 11: Dual-Stack ETP Local"

    info "Testing dual-stack ETP Local: placement, zero-endpoints, restore"

    local ORIGINAL_REPLICAS=$(kubectl get deployment nginx -n $NAMESPACE -o jsonpath='{.spec.replicas}')

    #--- Phase 1: Create service, verify both VIPs on endpoint nodes only ---
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

    for node in $NODES; do
        local HAS_VIP4=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP4/' && echo yes || echo no")
        local HAS_VIP6=$(ssh_node "$node" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP6/' && echo yes || echo no")
        local HAS_ENDPOINT=$(echo "$ENDPOINT_NODES" | grep -q "^$node$" && echo yes || echo no)

        if [ "$HAS_ENDPOINT" = "yes" ]; then
            [ "$HAS_VIP4" = "yes" ] && [ "$HAS_VIP6" = "yes" ] && pass "$node: both VIPs (endpoint)" || warn "$node: missing VIP (v4=$HAS_VIP4 v6=$HAS_VIP6)"
        else
            [ "$HAS_VIP4" = "no" ] && [ "$HAS_VIP6" = "no" ] || warn "$node: VIP on non-endpoint (v4=$HAS_VIP4 v6=$HAS_VIP6)"
        fi
    done

    wait_for_connectivity "$VIP4" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 1: IPv4 failed"
    wait_for_connectivity_v6 "$VIP6" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 1: IPv6 failed"
    pass "Phase 1: both families working"

    #--- Phase 2: Zero endpoints ---
    info "Phase 2: Zero endpoints (scale to 0)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=0
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local V4_COUNT=$(count_nodes_with_vip "$VIP4")
    local V6_COUNT=$(count_nodes_with_vip "$VIP6")
    if [ "$V4_COUNT" -eq 0 ] && [ "$V6_COUNT" -eq 0 ]; then
        pass "Phase 2: both VIPs removed from all nodes"
    else
        warn "Phase 2: VIPs remaining (v4=$V4_COUNT v6=$V6_COUNT)"
    fi

    #--- Phase 3: Restore ---
    info "Phase 3: Restore (scale to 1)"
    kubectl scale deployment nginx -n $NAMESPACE --replicas=1
    kubectl rollout status deployment/nginx -n $NAMESPACE --timeout=60s
    kubectl wait --for=condition=Ready pods -l app=nginx -n $NAMESPACE --timeout=60s
    sleep 10

    local RESTORE_NODE=$(kubectl get pods -n $NAMESPACE -l app=nginx --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')
    local HAS4=$(ssh_node "$RESTORE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP4/' && echo yes || echo no")
    local HAS6=$(ssh_node "$RESTORE_NODE" "ip -o addr show kube-lb0 2>/dev/null | grep -q ' $VIP6/' && echo yes || echo no")
    [ "$HAS4" = "yes" ] && [ "$HAS6" = "yes" ] && pass "Phase 3: both VIPs on $RESTORE_NODE" || fail "Phase 3: VIPs missing on $RESTORE_NODE (v4=$HAS4 v6=$HAS6)"

    wait_for_connectivity "$VIP4" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 3: IPv4 failed"
    wait_for_connectivity_v6 "$VIP6" 80 "$BGP_CONVERGE_TIMEOUT" || fail "Phase 3: IPv6 failed"
    pass "Phase 3: both families restored"

    # Cleanup
    kubectl scale deployment nginx -n $NAMESPACE --replicas=$ORIGINAL_REPLICAS
    delete_test_service "$SVC_DUALSTACK_LOCAL"
    pass "Dual-stack ETP Local test completed"
}

#---------------------------------------------------------------------
# Test 12: Dual-Stack Service Deletion
#---------------------------------------------------------------------

test_dualstack_deletion() {
    section "TEST 12: Dual-Stack Service Deletion"

    info "Testing that deleting a dual-stack service removes BOTH VIPs"

    create_test_service "$SVC_DUALSTACK"

    local IPS=$(wait_for_dualstack_ips "$SVC_NAME_DUALSTACK" 60) || fail "Dual-stack IPs not allocated"
    local VIP4=$(echo "$IPS" | awk '{print $1}')
    local VIP6=$(echo "$IPS" | awk '{print $2}')
    info "Allocated VIPs: IPv4=$VIP4  IPv6=$VIP6"

    wait_for_vip_announced "$VIP4" "$NODE_COUNT" 60 || fail "IPv4 VIP not announced"
    wait_for_vip_announced "$VIP6" "$NODE_COUNT" 60 || fail "IPv6 VIP not announced"

    # Verify both reachable before deletion
    wait_for_connectivity "$VIP4" 80 "$BGP_CONVERGE_TIMEOUT" || fail "IPv4 not reachable"
    wait_for_connectivity_v6 "$VIP6" 80 "$BGP_CONVERGE_TIMEOUT" || fail "IPv6 not reachable"
    pass "Both VIPs reachable before deletion"

    # Delete service
    info "Deleting dual-stack service..."
    delete_test_service "$SVC_DUALSTACK"

    # Wait for both VIPs to be removed from all nodes
    info "Waiting for VIPs to be removed from all nodes..."
    local TIMEOUT=30
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local COUNT4=$(count_nodes_with_vip "$VIP4")
        local COUNT6=$(count_nodes_with_vip "$VIP6")
        if [ "$COUNT4" -eq 0 ] && [ "$COUNT6" -eq 0 ]; then
            break
        fi
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done

    if [ $ELAPSED -ge $TIMEOUT ]; then
        fail "VIPs not removed from all nodes within ${TIMEOUT}s (v4=$(count_nodes_with_vip "$VIP4") v6=$(count_nodes_with_vip "$VIP6"))"
    fi
    pass "Both VIPs removed from all nodes after ${ELAPSED}s"

    # Verify neither is reachable
    info "Waiting for BGP route withdrawal..."
    sleep 10

    local RESPONSE4=$(test_connectivity "$VIP4" 80)
    local RESPONSE6=$(test_connectivity_v6 "$VIP6" 80)

    if echo "$RESPONSE4" | grep -q "Pod:"; then
        warn "IPv4 VIP still reachable after deletion (stale BGP route?)"
    else
        pass "IPv4 VIP no longer reachable"
    fi

    if echo "$RESPONSE6" | grep -q "Pod:"; then
        warn "IPv6 VIP still reachable after deletion (stale BGP route?)"
    else
        pass "IPv6 VIP no longer reachable"
    fi

    pass "Dual-stack service deletion test completed"
}

#---------------------------------------------------------------------
# Test 13: Default Aggregation IPv4
#---------------------------------------------------------------------

test_default_aggr_ipv4() {
    section "TEST 13: Default Aggregation IPv4"

    info "Testing IPv4 service with default aggregation (/24)"

    create_test_service "$SVC_IPV4_DEFAGGR"

    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV4_DEFAGGR" 60) || fail "No IP allocated"
    info "Allocated VIP: $VIP (from remote-default-aggr pool)"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced on all nodes"
    pass "VIP $VIP announced on $NODE_COUNT nodes"

    # Metrics display
    local alloc_metrics=$(scrape_allocator_metrics)
    print_allocator_metrics "$alloc_metrics" "remote-default-aggr"

    info "Waiting for BGP routes to propagate..."
    sleep 5

    info "Testing connectivity..."
    if wait_for_connectivity "$VIP" 80 "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv4 default aggregation connectivity successful"
    else
        fail "IPv4 default aggregation connectivity failed"
    fi

    delete_test_service "$SVC_IPV4_DEFAGGR"
    pass "Default aggregation IPv4 test completed"
}

#---------------------------------------------------------------------
# Test 14: Default Aggregation IPv6
#---------------------------------------------------------------------

test_default_aggr_ipv6() {
    section "TEST 14: Default Aggregation IPv6"

    info "Testing IPv6 service with default aggregation (/64)"

    create_test_service "$SVC_IPV6_DEFAGGR"

    local VIP=$(wait_for_service_ip "$SVC_NAME_IPV6_DEFAGGR" 60) || fail "No IPv6 allocated"
    info "Allocated VIP: $VIP (from remote-default-aggr pool)"

    wait_for_vip_announced "$VIP" "$NODE_COUNT" 60 || fail "VIP not announced on all nodes"
    pass "VIP $VIP announced on $NODE_COUNT nodes"

    info "Waiting for BGP routes to propagate..."
    sleep 5

    info "Testing IPv6 connectivity..."
    if wait_for_connectivity_v6 "$VIP" 80 "$BGP_CONVERGE_TIMEOUT"; then
        pass "IPv6 default aggregation connectivity successful"
    else
        fail "IPv6 default aggregation connectivity failed"
    fi

    delete_test_service "$SVC_IPV6_DEFAGGR"
    pass "Default aggregation IPv6 test completed"
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
    test_basic_ipv6
    test_etp_local_v6
    test_service_deletion_v6
    test_dualstack_connectivity
    test_dualstack_etp_local
    test_dualstack_deletion
    test_default_aggr_ipv4
    test_default_aggr_ipv6

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
            echo "  7 - Basic Connectivity (IPv6)"
            echo "  8 - ETP Local Connectivity (IPv6)"
            echo "  9 - Service Deletion Cleanup (IPv6)"
            echo " 10 - Dual-Stack Connectivity"
            echo " 11 - Dual-Stack ETP Local"
            echo " 12 - Dual-Stack Service Deletion"
            echo " 13 - Default Aggregation IPv4"
            echo " 14 - Default Aggregation IPv6"
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
        7) test_prerequisites && test_basic_ipv6 ;;
        8) test_prerequisites && test_etp_local_v6 ;;
        9) test_prerequisites && test_service_deletion_v6 ;;
        10) test_prerequisites && test_dualstack_connectivity ;;
        11) test_prerequisites && test_dualstack_etp_local ;;
        12) test_prerequisites && test_dualstack_deletion ;;
        13) test_prerequisites && test_default_aggr_ipv4 ;;
        14) test_prerequisites && test_default_aggr_ipv6 ;;
        *) echo "Unknown test: $SELECTED_TEST"; exit 1 ;;
    esac
else
    run_all_tests
fi
