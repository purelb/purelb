#!/bin/bash
#
# Common test infrastructure for PureLB E2E tests.
# Source this file from test scripts: source "$(dirname "$0")/../common.sh"
#
# Provides:
#   - Dynamic node discovery (no DNS needed, SSH by IP)
#   - Subnet/interface auto-detection
#   - IPv6 discovery
#   - ServiceGroup generation
#   - Common utility functions (logging, VIP helpers, cleanup)
#
# Requires: bash 4+ (associative arrays), kubectl, ssh, curl
#

# Bash version check (required for associative arrays)
if [[ ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "ERROR: Bash 4+ required for associative arrays"
    exit 1
fi

# =====================================================================
# Colors and Logging
# =====================================================================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

pass() { echo -e "${GREEN}✓ PASS:${NC} $1"; }
fail() { echo -e "${RED}✗ FAIL:${NC} $1"; exit 1; }
info() { echo -e "${YELLOW}→${NC} $1"; }
detail() { echo -e "${CYAN}     ${NC} $1"; }
ts() { date '+%H:%M:%S.%3N'; }
# Return "nodename (IP)" for human-readable display
node_label() { local n=$1; echo "$n (${NODE_IPS[$n]})"; }

# =====================================================================
# Context and CLI Arguments
# =====================================================================

# Default context: use current kubectl context if not set by caller
if [ -z "$CONTEXT" ]; then
    CONTEXT=$(command kubectl config current-context 2>/dev/null)
fi

# Override kubectl to always use the configured context
kubectl() { command kubectl --context "$CONTEXT" "$@"; }

# Parse --context from remaining args (call from your script after sourcing)
# Usage: parse_common_args "$@" ; set -- "${REMAINING_ARGS[@]}"
REMAINING_ARGS=()
parse_common_args() {
    REMAINING_ARGS=()
    while [[ $# -gt 0 ]]; do
        case $1 in
            --context)
                CONTEXT="$2"
                shift 2
                ;;
            *)
                REMAINING_ARGS+=("$1")
                shift
                ;;
        esac
    done
}

# =====================================================================
# Node Discovery (no DNS needed)
# =====================================================================

declare -A NODE_IPS        # node-name -> InternalIP
declare -A NODE_SUBNET     # node-name -> e.g. "172.30.250.0/24"
declare -A NODE_IFACE      # node-name -> e.g. "eth1"
declare -A SUBNET_NODES    # subnet -> "node1 node2 node3"
NODES=""                   # space-separated node names
NODE_COUNT=0
SUBNETS=""                 # space-separated unique subnets
SUBNET_COUNT=0

# IPv6
declare -A SUBNET_V6       # v4-subnet -> v6-subnet (e.g., "172.30.250.0/24" -> "2001:470:b8f3:250::/64")
declare -A SUBNET_V6_PREFIX # v4-subnet -> v6 prefix without mask (e.g., "2001:470:b8f3:250::")
HAS_IPV6=false

# First subnet (convenience for tests that just need "a" subnet)
FIRST_SUBNET=""

discover_nodes() {
    info "Discovering cluster nodes..."

    # Get node names and IPs from kubectl
    local node_data
    node_data=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name} {.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')

    while IFS=' ' read -r name ip; do
        [ -z "$name" ] && continue
        NODE_IPS[$name]="$ip"
        NODES="$NODES $name"
    done <<< "$node_data"

    # Trim leading space
    NODES="${NODES# }"
    NODE_COUNT=$(echo "$NODES" | wc -w)

    if [ "$NODE_COUNT" -eq 0 ]; then
        fail "No nodes found in cluster (context: $CONTEXT)"
    fi

    info "Found $NODE_COUNT nodes: $NODES"
    for node in $NODES; do
        detail "$node -> ${NODE_IPS[$node]}"
    done
}

# =====================================================================
# SSH by IP (works without DNS)
# =====================================================================

node_ssh() {
    local NODE=$1; shift
    local IP="${NODE_IPS[$NODE]}"
    if [ -z "$IP" ]; then
        echo "ERROR: No IP for node $NODE" >&2
        return 1
    fi
    ssh "$IP" "$@"
}

node_ssh_or_fail() {
    local NODE=$1; shift
    local IP="${NODE_IPS[$NODE]}"
    if [ -z "$IP" ]; then
        fail "No IP for node $NODE"
    fi
    if ! ssh "$IP" "$@" 2>/dev/null; then
        if ! ssh "$IP" "true" 2>/dev/null; then
            fail "SSH to $NODE ($IP) failed"
        fi
        return 1
    fi
    return 0
}

# =====================================================================
# Interface Detection
# =====================================================================

detect_interfaces() {
    info "Detecting network interfaces..."
    for node in $NODES; do
        local ip="${NODE_IPS[$node]}"
        local iface
        iface=$(node_ssh "$node" "ip -o addr show 2>/dev/null | grep ' ${ip}/' | awk '{print \$2}'" 2>/dev/null)
        if [ -n "$iface" ]; then
            NODE_IFACE[$node]="$iface"
        else
            # Fallback: try common interface names
            NODE_IFACE[$node]="eth0"
            info "WARNING: Could not detect interface for $node, defaulting to eth0"
        fi
    done

    # Report
    for node in $NODES; do
        detail "$node -> ${NODE_IFACE[$node]}"
    done
}

# =====================================================================
# Subnet Detection
# =====================================================================

detect_subnets() {
    info "Detecting subnets..."

    # Group nodes by /24 initially, then verify via interface
    local -A seen_subnets
    for node in $NODES; do
        local ip="${NODE_IPS[$node]}"
        local iface="${NODE_IFACE[$node]}"

        # Get the actual CIDR from the node's interface
        local cidr
        cidr=$(node_ssh "$node" "ip -o addr show $iface 2>/dev/null | grep ' ${ip}/' | awk '{print \$4}'" 2>/dev/null)

        if [ -n "$cidr" ]; then
            # cidr is like "172.30.250.100/24" - extract mask
            local mask="${cidr#*/}"
            # Compute network address from IP and mask
            local subnet
            subnet=$(compute_network "$ip" "$mask")
            NODE_SUBNET[$node]="$subnet"
        else
            # Fallback: assume /24
            local base
            base=$(echo "$ip" | awk -F. '{print $1"."$2"."$3".0/24"}')
            NODE_SUBNET[$node]="$base"
            info "WARNING: Could not detect subnet for $node ($ip), assuming $base"
        fi

        local s="${NODE_SUBNET[$node]}"
        SUBNET_NODES[$s]="${SUBNET_NODES[$s]} $node"
        seen_subnets[$s]=1
    done

    # Build SUBNETS list
    SUBNETS="${!seen_subnets[*]}"
    SUBNET_COUNT=${#seen_subnets[@]}

    # Set FIRST_SUBNET
    for s in $SUBNETS; do
        FIRST_SUBNET="$s"
        break
    done

    # Trim leading spaces in SUBNET_NODES values
    for s in "${!SUBNET_NODES[@]}"; do
        SUBNET_NODES[$s]="${SUBNET_NODES[$s]# }"
    done

    info "Found $SUBNET_COUNT subnet(s):"
    for s in $SUBNETS; do
        detail "$s -> ${SUBNET_NODES[$s]}"
    done
}

# Compute network address from IP and mask length
# e.g., compute_network "172.30.250.100" "24" -> "172.30.250.0/24"
compute_network() {
    local ip=$1
    local mask=$2

    # Split IP into octets
    IFS='.' read -r o1 o2 o3 o4 <<< "$ip"

    # For common masks, compute directly
    case $mask in
        24) echo "${o1}.${o2}.${o3}.0/24" ;;
        16) echo "${o1}.${o2}.0.0/16" ;;
        8)  echo "${o1}.0.0.0/8" ;;
        *)
            # General case: convert to 32-bit, apply mask, convert back
            local ipnum=$(( (o1 << 24) + (o2 << 16) + (o3 << 8) + o4 ))
            local maskbits=$(( 0xFFFFFFFF << (32 - mask) & 0xFFFFFFFF ))
            local netnum=$(( ipnum & maskbits ))
            local n1=$(( (netnum >> 24) & 0xFF ))
            local n2=$(( (netnum >> 16) & 0xFF ))
            local n3=$(( (netnum >> 8) & 0xFF ))
            local n4=$(( netnum & 0xFF ))
            echo "${n1}.${n2}.${n3}.${n4}/${mask}"
            ;;
    esac
}

# =====================================================================
# IPv6 Discovery
# =====================================================================

discover_ipv6() {
    info "Discovering IPv6..."
    for s in $SUBNETS; do
        # Pick the first node on this subnet
        local first_node
        first_node=$(echo "${SUBNET_NODES[$s]}" | awk '{print $1}')
        local iface="${NODE_IFACE[$first_node]}"

        # Get global-scope IPv6 address on the interface
        local v6addr
        v6addr=$(node_ssh "$first_node" "ip -6 -o addr show $iface scope global 2>/dev/null | head -1 | awk '{print \$4}'" 2>/dev/null)

        if [ -n "$v6addr" ]; then
            # v6addr is like "2001:470:b8f3:250:be24:11ff:fe1b:5642/64"
            local v6mask="${v6addr#*/}"
            local v6ip="${v6addr%/*}"

            # Extract the /64 prefix: take the first 4 groups
            local v6prefix
            v6prefix=$(echo "$v6ip" | awk -F: '{printf "%s:%s:%s:%s::", $1, $2, $3, $4}')
            local v6subnet="${v6prefix}/${v6mask}"

            SUBNET_V6[$s]="$v6subnet"
            SUBNET_V6_PREFIX[$s]="$v6prefix"
            HAS_IPV6=true
            detail "$s -> IPv6: $v6subnet"
        else
            detail "$s -> No IPv6 detected"
        fi
    done

    if [ "$HAS_IPV6" = "true" ]; then
        info "IPv6 is available"
    else
        info "IPv6 not detected on any subnet"
    fi
}

# =====================================================================
# Pool Range Helpers
# =====================================================================

# Generate v4 pool range for a subnet: .200-.220
# Usage: subnet_pool_range "172.30.250.0/24"
subnet_pool_range() {
    local subnet=$1
    local base="${subnet%%.*/*}"   # won't work for all cases
    # Extract first 3 octets from the subnet
    local net="${subnet%/*}"
    local prefix
    prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
    echo "${prefix}.200-${prefix}.220"
}

# Generate v4 pool range for per-test ServiceGroups: .230-.240
# Non-overlapping with the default pool (.200-.220)
# Usage: subnet_test_pool_range "172.30.250.0/24"
subnet_test_pool_range() {
    local subnet=$1
    local net="${subnet%/*}"
    local prefix
    prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
    echo "${prefix}.230-${prefix}.240"
}

# Get a specific test IP from a subnet pool
# Usage: subnet_test_ip "172.30.250.0/24" [offset]
# Default offset 10 -> .210
subnet_test_ip() {
    local subnet=$1
    local offset=${2:-10}
    local net="${subnet%/*}"
    local prefix
    prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
    echo "${prefix}.$((200 + offset))"
}

# Generate v6 pool range for a v6 prefix
# Usage: v6_pool_range "2001:470:b8f3:250::"
# Produces: "2001:470:b8f3:250:a::1-2001:470:b8f3:250:a::20"
v6_pool_range() {
    local v6prefix=$1
    # Strip trailing :: to avoid double :: in result
    local base="${v6prefix%%::}"
    echo "${base}:a::1-${base}:a::20"
}

# Generate non-overlapping v6 test pool range (b:: instead of a::)
# Usage: v6_test_pool_range "2001:470:b8f3:250::"
v6_test_pool_range() {
    local v6prefix=$1
    local base="${v6prefix%%::}"
    echo "${base}:b::1-${base}:b::20"
}

# Generate v4 pool range for timing tests: .241-.250
# Non-overlapping with default (.200-.220) and stress (.230-.240) pools.
# Usage: subnet_timing_pool_range "172.30.250.0/24"
subnet_timing_pool_range() {
    local subnet=$1
    local net="${subnet%/*}"
    local prefix
    prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
    echo "${prefix}.241-${prefix}.250"
}

# Generate v6 pool range for timing tests (c:: instead of a:: or b::)
# Usage: v6_timing_pool_range "2001:470:b8f3:250::"
v6_timing_pool_range() {
    local v6prefix=$1
    local base="${v6prefix%%::}"
    echo "${base}:c::1-${base}:c::20"
}

# Check if an IPv4 address falls within any configured pool range (.200-.220)
# Usage: ip_in_pool_range "172.30.250.205"
ip_in_pool_range() {
    local ip=$1
    local last_octet
    last_octet=$(echo "$ip" | awk -F. '{print $4}')
    local prefix
    prefix=$(echo "$ip" | awk -F. '{print $1"."$2"."$3}')

    # Check if last octet is in 200-220 range
    if [ "$last_octet" -ge 200 ] && [ "$last_octet" -le 220 ]; then
        # Check if the prefix matches any known subnet
        for s in $SUBNETS; do
            local net="${s%/*}"
            local s_prefix
            s_prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
            if [ "$prefix" = "$s_prefix" ]; then
                return 0
            fi
        done
    fi
    return 1
}

# Check if an IPv6 address falls within any configured v6 pool
# Usage: ipv6_in_pool_range "2001:470:b8f3:250:a::5"
ipv6_in_pool_range() {
    local ip=$1
    for s in $SUBNETS; do
        local v6prefix="${SUBNET_V6_PREFIX[$s]}"
        if [ -n "$v6prefix" ]; then
            # Strip trailing :: from prefix, match against prefix:a::
            local base="${v6prefix%%::}"
            if echo "$ip" | grep -qi "^${base}:a::"; then
                return 0
            fi
        fi
    done
    return 1
}

# =====================================================================
# ServiceGroup Generation
# =====================================================================

# Generate and apply the "default" ServiceGroup with pools on all discovered subnets
generate_default_servicegroup() {
    info "Generating default ServiceGroup for $SUBNET_COUNT subnet(s)..."

    local v4pools=""
    local v6pools=""

    for s in $SUBNETS; do
        local pool
        pool=$(subnet_pool_range "$s")
        v4pools="${v4pools}
    - aggregation: default
      pool: ${pool}
      subnet: ${s}"

        # Add v6 pool if IPv6 is available on this subnet
        local v6sub="${SUBNET_V6[$s]}"
        local v6prefix="${SUBNET_V6_PREFIX[$s]}"
        if [ -n "$v6sub" ]; then
            local v6pool
            v6pool=$(v6_pool_range "$v6prefix")
            v6pools="${v6pools}
    - aggregation: default
      pool: ${v6pool}
      subnet: ${v6sub}"
        fi
    done

    local yaml="apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: default
  namespace: purelb-system
spec:
  local:
    v4pools:${v4pools}"

    if [ -n "$v6pools" ]; then
        yaml="${yaml}
    v6pools:${v6pools}"
    fi

    echo "$yaml" | kubectl apply -f - 2>&1
    pass "Default ServiceGroup applied"
}

# Generate and apply a ServiceGroup with a pool on a SINGLE subnet
# Usage: generate_single_subnet_servicegroup NAME SUBNET
generate_single_subnet_servicegroup() {
    local name=$1
    local subnet=$2
    local pool
    pool=$(subnet_test_pool_range "$subnet")

    info "Generating ServiceGroup '$name' for subnet $subnet..."

    local yaml="apiVersion: purelb.io/v2
kind: ServiceGroup
metadata:
  name: ${name}
  namespace: purelb-system
spec:
  local:
    v4pools:
    - aggregation: default
      pool: ${pool}
      subnet: ${subnet}"

    # Add v6 pool if available
    local v6sub="${SUBNET_V6[$subnet]}"
    local v6prefix="${SUBNET_V6_PREFIX[$subnet]}"
    if [ -n "$v6sub" ]; then
        local v6pool
        v6pool=$(v6_test_pool_range "$v6prefix")
        yaml="${yaml}
    v6pools:
    - aggregation: default
      pool: ${v6pool}
      subnet: ${v6sub}"
    fi

    echo "$yaml" | kubectl apply -f - 2>&1
    CLEANUP_SGS+=("$name")
    pass "ServiceGroup '$name' applied"
}

# =====================================================================
# Cleanup
# =====================================================================

CLEANUP_SGS=()

cleanup_servicegroups() {
    for sg in "${CLEANUP_SGS[@]}"; do
        kubectl delete servicegroup "$sg" -n purelb-system --ignore-not-found 2>/dev/null || true
    done
}

# Register cleanup trap (scripts can add their own traps too)
trap cleanup_servicegroups EXIT

# =====================================================================
# CNI Detection
# =====================================================================

detect_cni() {
    if kubectl get daemonset -n kube-flannel kube-flannel-ds &>/dev/null; then
        echo "flannel"; return
    fi
    if kubectl get daemonset -n kube-system kube-flannel-ds &>/dev/null; then
        echo "flannel"; return
    fi
    if kubectl get daemonset -n kube-system calico-node &>/dev/null; then
        echo "calico"; return
    fi
    if kubectl get daemonset -n calico-system calico-node &>/dev/null; then
        echo "calico"; return
    fi
    if kubectl get daemonset -n kube-system cilium &>/dev/null; then
        echo "cilium"; return
    fi
    echo "unknown"
}

# =====================================================================
# VIP and IP Helpers
# =====================================================================

# Wait for an IP to be announced on any node (with timeout)
wait_for_ip_announced() {
    local IP=$1
    local TIMEOUT=${2:-30}
    local INTERVAL=2
    local ELAPSED=0

    while [ $ELAPSED -lt $TIMEOUT ]; do
        for node in $NODES; do
            if node_ssh "$node" "ip -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                return 0
            fi
        done
        sleep $INTERVAL
        ELAPSED=$((ELAPSED + INTERVAL))
    done
    return 1
}

# Wait for IP to NOT be on any node
wait_for_ip_not_on_any_node() {
    local IP=$1
    local TIMEOUT=${2:-30}
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        local FOUND=false
        for node in $NODES; do
            if node_ssh "$node" "ip -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
                FOUND=true
                break
            fi
        done
        [ "$FOUND" = "false" ] && return 0
        sleep 2
        ELAPSED=$((ELAPSED + 2))
    done
    return 1
}

# Find which node has a given VIP
find_vip_node() {
    local IP=$1
    for node in $NODES; do
        if node_ssh "$node" "ip -o addr show 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo "$node"
            return 0
        fi
    done
    return 1
}

# Get the node holding a VIP (on its detected interface)
get_vip_holder() {
    local IP=$1
    for node in $NODES; do
        local iface="${NODE_IFACE[$node]}"
        if node_ssh "$node" "ip -o addr show $iface 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo "$node"
            return 0
        fi
    done
    echo "NONE"
}

# Get the node holding an IPv6 VIP (on its detected interface)
get_v6_vip_holder() {
    local IP=$1
    for node in $NODES; do
        local iface="${NODE_IFACE[$node]}"
        if node_ssh "$node" "ip -6 -o addr show $iface 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo "$node"
            return 0
        fi
    done
    echo "NONE"
}

# Check if any node OTHER than the excluded one has the VIP
vip_on_other_node() {
    local IP=$1
    local EXCLUDE=$2
    for node in $NODES; do
        [ "$node" = "$EXCLUDE" ] && continue
        local iface="${NODE_IFACE[$node]}"
        if node_ssh "$node" "ip -o addr show $iface 2>/dev/null | grep -q ' $IP/'" 2>/dev/null; then
            echo "$node"
            return 0
        fi
    done
    return 1
}

# Test connectivity and return the responding node
test_connectivity_get_node() {
    local IP=$1
    local PORT=${2:-80}
    local RESPONSE
    RESPONSE=$(curl -s --connect-timeout 5 "http://$IP:$PORT/" 2>/dev/null || true)
    if echo "$RESPONSE" | grep -q "Node:"; then
        echo "$RESPONSE" | grep "Node:" | awk '{print $2}'
        return 0
    fi
    return 1
}

# =====================================================================
# Address Detail Helpers
# =====================================================================

# Get detailed address info including flags and lifetime
get_address_details() {
    local NODE=$1
    local IP=$2
    local INTERFACE=$3
    node_ssh "$NODE" "ip -d addr show $INTERFACE 2>/dev/null | grep -A1 ' $IP/'" 2>/dev/null || true
}

# Extract valid_lft value from address details
get_valid_lft() {
    local DETAILS=$1
    if echo "$DETAILS" | grep -q "valid_lft forever"; then
        echo "forever"
    else
        echo "$DETAILS" | grep -oP 'valid_lft \K[0-9]+' || echo "unknown"
    fi
}

# Check if address details contain a specific property
check_address_property() {
    local DETAILS=$1
    local PROPERTY=$2
    echo "$DETAILS" | grep -q "$PROPERTY"
}

# =====================================================================
# Lease/Election Helpers
# =====================================================================

# Get subnet annotations from a node's lease
get_node_lease_subnets() {
    local NODE=$1
    kubectl get lease "purelb-node-$NODE" -n purelb-system \
        -o jsonpath='{.metadata.annotations.purelb\.io/subnets}' 2>/dev/null
}

# Check if a node's lease exists
lease_exists() {
    local NODE=$1
    kubectl get lease "purelb-node-$NODE" -n purelb-system &>/dev/null
}

# Get pool type annotation from a service
get_pool_type() {
    local SVC=$1
    local NS=${2:-$NAMESPACE}
    kubectl get svc "$SVC" -n "$NS" \
        -o jsonpath='{.metadata.annotations.purelb\.io/pool-type}' 2>/dev/null
}

# =====================================================================
# Subnet Helpers for Tests
# =====================================================================

# Get the subnet a given IP belongs to (from our discovered subnets)
ip_to_subnet() {
    local ip=$1
    local prefix
    prefix=$(echo "$ip" | awk -F. '{print $1"."$2"."$3}')
    for s in $SUBNETS; do
        local net="${s%/*}"
        local s_prefix
        s_prefix=$(echo "$net" | awk -F. '{print $1"."$2"."$3}')
        if [ "$prefix" = "$s_prefix" ]; then
            echo "$s"
            return 0
        fi
    done
    return 1
}

# Verify a VIP holder is on the correct subnet for the VIP
verify_vip_subnet_match() {
    local VIP=$1
    local HOLDER_NODE=$2
    if [ "$SUBNET_COUNT" -lt 2 ]; then
        return 0  # Trivially true on single-subnet clusters
    fi
    local vip_subnet
    vip_subnet=$(ip_to_subnet "$VIP")
    local node_subnet="${NODE_SUBNET[$HOLDER_NODE]}"
    if [ "$vip_subnet" = "$node_subnet" ]; then
        return 0
    else
        echo "VIP $VIP (subnet $vip_subnet) is on node $HOLDER_NODE (subnet $node_subnet) - MISMATCH"
        return 1
    fi
}

# Interactive pause - waits for user to press Enter
pause_for_review() {
    if [ "${INTERACTIVE:-false}" = "true" ]; then
        echo ""
        echo -e "${YELLOW}═══════════════════════════════════════════════════════════════${NC}"
        echo -e "${YELLOW}  PAUSED: Review the output above. Press ENTER to continue...${NC}"
        echo -e "${YELLOW}═══════════════════════════════════════════════════════════════${NC}"
        read -r
    fi
}

# =====================================================================
# Auto-Discovery on Source
# =====================================================================

# Run discovery when this file is sourced
discover_nodes
detect_interfaces
detect_subnets
discover_ipv6
